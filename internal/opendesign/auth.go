package opendesign

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// parseAuth lets CPA load an auth file that belongs to this provider. It is the
// only path that can introduce a credential the plugin did not create itself.
func (s *Service) parseAuth(raw json.RawMessage) (any, error) {
	s.authFileMu.Lock()
	defer s.authFileMu.Unlock()
	var r struct {
		Provider, Path, FileName string
		RawJSON                  []byte
		Host                     struct{ AuthDir string }
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	baseName := ""
	if r.FileName != "" {
		baseName = filepath.Base(r.FileName)
		if baseName != r.FileName || strings.ContainsAny(r.FileName, `/\`) {
			baseName = ""
		}
	}
	// 1. A save currently in flight. CPA re-enters auth.parse from
	//    buildAuthsFromFileData *before* it writes the file, so disk still holds
	//    the previous content; the pending body is the only correct source.
	//    The body is used but not published (see below): host.auth.save has not
	//    confirmed the write yet.
	snapshot, pending := s.pendingFor(baseName, r.FileName, r.Path)
	if pending {
		r.RawJSON = snapshot
	}
	// 2. Otherwise re-read the physical file, serialized with plugin writes, so a
	//    watcher callback queued behind a newer edit cannot apply stale content.
	//    This is the stale-callback protection and stays even when pending exists.
	dir := r.Host.AuthDir
	if dir == "" {
		s.mu.RLock()
		dir = s.authDir
		s.mu.RUnlock()
	}
	resolvedFile := false
	if !pending && dir != "" && baseName != "" {
		if latest, err := os.ReadFile(filepath.Join(dir, baseName)); err == nil {
			r.RawJSON = latest
			resolvedFile = true
		}
	}
	var c Credential
	if err := json.Unmarshal(r.RawJSON, &c); err != nil || c.Type != Provider {
		return map[string]any{"Handled": false}, nil
	}
	if c.APIKey == "" {
		return nil, fail(http.StatusUnauthorized, "credential has no api_key")
	}
	c.ID = strings.TrimSpace(c.ID)
	if c.ID == "" {
		c.ID = strings.TrimSuffix(filepath.Base(r.FileName), ".json")
	}
	if c.ID == "" {
		c.ID = PluginID + "-" + newID()
	}
	if strings.TrimSpace(c.Label) == "" {
		c.Label = c.ID
	}
	if c.BaseURL == "" {
		c.BaseURL = s.config().BaseURL
	}
	// Reconcile the plugin-wide retry-scheduling policy before the credential is
	// published: an auth file written by an older plugin version, or by hand,
	// gets the current rules. Caller holds authFileMu and no host call happens
	// here, so the rewrite is safe on this path.
	s.applyErrorRules(&c, dir, baseName, r.RawJSON, pending)
	if pending {
		// The host parses the new body, then writes the file, then upserts its
		// records; any of the last two steps can still fail and leave the POST
		// answering 500. Publishing here would make the live record carry a
		// control key / disabled flag / billing state that disk never received,
		// so "the save failed" and "the live credential is the new one" would
		// disagree. Answer with the AuthData the host needs and let the caller of
		// saveCredential publish the credential only after the host confirmed the
		// save. A failed save therefore keeps the last confirmed live state, and a
		// watcher callback queued behind it still resolves from disk.
		s.mu.RLock()
		revoked := s.revoked[c.ID]
		s.mu.RUnlock()
		if revoked {
			return map[string]any{"Handled": false}, nil
		}
		data := s.authData(c)
		if actual := filepath.Base(r.FileName); actual != "" && actual != "." {
			// Same shape the disk path renders: the file being written, not the
			// fallback "<id>.json" of an authFiles entry that does not exist yet.
			data["FileName"] = actual
		}
		return map[string]any{"Handled": true, "Auth": data}, nil
	}
	now := s.quotaPauseTime()
	s.mu.Lock()
	if s.revoked[c.ID] && !resolvedFile {
		// A watcher may have queued this callback before the delete landed.
		s.mu.Unlock()
		return map[string]any{"Handled": false}, nil
	}
	if resolvedFile {
		delete(s.revoked, c.ID)
	}
	if current, exists := s.creds[c.ID]; exists {
		// A live block is authoritative for both the block itself and the
		// operator's disable choice, because CPA persists a projected `disabled`
		// that may come from a snapshot older than the operator's last change.
		switch {
		case current.APIKey != c.APIKey:
			// The key was replaced, so the old block no longer applies.
			delete(s.quotaPauseErrors, c.ID)
			delete(s.quotaDiskStale, c.ID)
		case current.APIKey == c.APIKey && current.quotaBlocked():
			// Keep the live block even when either revision is missing. Only mark a
			// genuine divergence as unsynchronized: an identical callback after our
			// own write must not create an endless repair/watcher cycle or prevent
			// a fresh quota recheck. Compare the full serialized block as well as
			// its revision; a file editor can alter a block without changing its ID.
			manual := true
			if snapshot := current.QuotaBlock.ManualDisabled; snapshot != nil {
				manual = *snapshot
			}
			if !resolvedFile || !c.quotaBlocked() || c.QuotaRevision != current.QuotaRevision ||
				c.Disabled != manual || !bytes.Equal(jsonBytes(c.QuotaBlock), jsonBytes(current.QuotaBlock)) {
				s.quotaPauseErrors[c.ID] = quotaPauseSyncError
				// A pending clear repair still tells a concurrent new pause that
				// this disk body's manual choice may be stale. Keep the marker
				// until a matching paused body has actually reached disk.
			} else {
				delete(s.quotaDiskStale, c.ID)
				delete(s.quotaPauseErrors, c.ID)
			}
			c.QuotaBlock = cloneQuotaBlock(current.QuotaBlock)
			c.QuotaRevision = current.QuotaRevision
			c.Disabled = manual
		case current.APIKey == c.APIKey && !current.quotaBlocked() && current.QuotaRevision != "" && c.quotaBlocked():
			// The live state is cleared, but the incoming body still carries a
			// paused snapshot (a host rewrite of the pre-resume file). Never
			// resurrect the pause, even if an external writer copied the cleared
			// revision into an inconsistent body. Keep the live cleared state and
			// schedule a repair so a restart does not adopt the stale block.
			s.quotaDiskStale[c.ID] = true
			c.QuotaBlock = nil
			c.QuotaRevision = current.QuotaRevision
			c.Disabled = current.Disabled
		case current.APIKey == c.APIKey && !current.quotaBlocked() && current.QuotaRevision != "":
			// Any other body with no live block cannot advance or erase the
			// published clear revision. A missing or differing revision is an
			// older host body, not a newer decision this process may drop.
			// Keep the live block (nil) rather than adopting an invalid/foreign
			// one, and preserve either side's disable choice: an enable is
			// recoverable from the console, an accidental enable is not safe.
			incomingBlock, incomingRevision, incomingDisabled := c.QuotaBlock, c.QuotaRevision, c.Disabled
			manual := current.Disabled || incomingDisabled
			c.QuotaBlock = nil
			c.QuotaRevision = current.QuotaRevision
			c.Disabled = manual
			if incomingBlock == nil {
				if incomingRevision != current.QuotaRevision || incomingDisabled != manual {
					// Restore the revision and manual choice on disk too, so a
					// restart after repair does not forget the clear.
					s.quotaDiskStale[c.ID] = true
				}
			} else {
				// The disk body carries a block this plugin does not own; it is
				// not repairable by republishing the clear. Drop a marker from an
				// earlier divergence that this body has already replaced, instead
				// of retrying it forever.
				delete(s.quotaDiskStale, c.ID)
			}
		case current.APIKey == c.APIKey && !current.quotaBlocked() && current.QuotaRevision == "" && !c.quotaBlocked():
			// No published clear revision exists yet, so an unversioned body cannot
			// prove it is newer than the console. Keep the union of manual choices
			// (an accidental enable is unsafe, a withheld enable is recoverable),
			// and never adopt an invalid/foreign block. If the disk disagrees with
			// that union, publish a revision and let the repair persist it so a
			// restart does not forget the disable. A foreign/invalid block is not
			// repairable by republishing the clear, so it must not leave a marker
			// that every sweep would retry forever.
			incomingBlock, incomingDisabled := c.QuotaBlock, c.Disabled
			manual := current.Disabled || incomingDisabled
			c.QuotaBlock = nil
			c.Disabled = manual
			if incomingBlock != nil {
				delete(s.quotaDiskStale, c.ID)
			} else if incomingDisabled != manual {
				if c.QuotaRevision == "" {
					c.QuotaRevision = newID()
				}
				s.quotaDiskStale[c.ID] = true
			}
		case current.APIKey == c.APIKey && c.quotaBlocked() && current.QuotaRevision == "":
			// An unversioned live state does not prove that this on-disk block is
			// stale. Adopt it rather than deleting a potentially legitimate pause.
			// Preserve either side's manual disable; unknown snapshots are disabled.
			manual := current.Disabled || c.Disabled || c.QuotaBlock.ManualDisabled == nil
			if snapshot := c.QuotaBlock.ManualDisabled; snapshot != nil {
				manual = manual || *snapshot
			}
			if c.Disabled != manual || c.QuotaBlock.ManualDisabled == nil || *c.QuotaBlock.ManualDisabled != manual {
				c.QuotaBlock = cloneQuotaBlock(c.QuotaBlock)
				c.QuotaBlock.ManualDisabled = &manual
				c.Disabled = manual
				c.QuotaRevision = newID()
				s.quotaPauseErrors[c.ID] = quotaPauseSyncError
			}
			delete(s.quotaDiskStale, c.ID)
		}
	} else {
		// A deleted credential can be re-imported under the same ID. Its old
		// retry markers must not attach to the new identity; normalization below
		// may still create a fresh sync error for the newly imported block.
		delete(s.quotaPauseErrors, c.ID)
		delete(s.quotaDiskStale, c.ID)
	}
	if block, changed := normalizeQuotaBlock(c, now); changed {
		c.QuotaBlock = block
		c.QuotaRevision = newID()
		s.quotaPauseErrors[c.ID] = quotaPauseSyncError
	}
	s.storeCredentialLocked(c)
	if r.FileName != "" {
		s.authFiles[c.ID] = filepath.Base(r.FileName)
	}
	if r.Host.AuthDir != "" {
		s.authDir = r.Host.AuthDir
	}
	s.mu.Unlock()
	s.wakeQuotaPauseWorker()
	return map[string]any{"Handled": true, "Auth": s.authData(c)}, nil
}

// refreshAuth reports the current credential with a far-future refresh time:
// a static API key never rotates on its own.
func (s *Service) refreshAuth(raw json.RawMessage) (any, error) {
	var r struct{ StorageJSON []byte }
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	var c Credential
	if err := json.Unmarshal(r.StorageJSON, &c); err != nil {
		return nil, err
	}
	s.mu.RLock()
	current, known := s.creds[c.ID]
	if known {
		c = current
	}
	revoked := s.revoked[c.ID]
	s.mu.RUnlock()
	if revoked {
		return nil, fail(http.StatusUnauthorized, "upstream credential was removed")
	}
	if !known {
		// Never synthesize an auth record from a stale StorageJSON snapshot: the
		// host would then route traffic through a credential this plugin cannot
		// verify or delete.
		return map[string]any{"Auth": map[string]any{}}, nil
	}
	return map[string]any{"Auth": s.authData(c), "NextRefreshAfter": time.Now().Add(365 * 24 * time.Hour)}, nil
}

// authData renders the CPA AuthData shape. StorageJSON keeps the full credential
// because the host stores it in auth-dir and hands it back on execute.
func (s *Service) authData(c Credential) map[string]any {
	if c.BaseURL == "" {
		c.BaseURL = s.config().BaseURL
	}
	return map[string]any{
		"Provider":    Provider,
		"ID":          c.ID,
		"FileName":    s.fileNameFor(c),
		"Label":       c.Label,
		"Disabled":    c.effectiveDisabled(),
		"StorageJSON": jsonBytes(c),
		// CPA reads request_scoped_errors from the auth metadata to classify an
		// upstream failure as stop/continue and to decide whether the failed
		// credential is cooled down. The plugin owns the policy; the scheduling
		// decision stays with CPA.
		"Metadata":   map[string]any{"type": Provider, "request_scoped_errors": errorRulesForMetadata(c.RequestScopedErrors)},
		"Attributes": map[string]string{"auth_kind": "api_key"},
	}
}

func (s *Service) fileNameFor(c Credential) string {
	s.mu.RLock()
	name := s.authFiles[c.ID]
	s.mu.RUnlock()
	if name != "" {
		return name
	}
	return c.ID + ".json"
}

// selectedCredential resolves the credential for an execution request, preferring
// the live record over a possibly stale StorageJSON snapshot.
func (s *Service) selectedCredential(request ExecutorRequest) (Credential, error) {
	var c Credential
	if request.CredentialOverride != nil {
		c = *request.CredentialOverride
		s.mu.RLock()
		current, ok := s.creds[c.ID]
		epoch := s.credentialEpoch[c.ID]
		revoked := s.revoked[c.ID]
		currentBaseURL := current.BaseURL
		if currentBaseURL == "" {
			currentBaseURL = s.cfg.BaseURL
		}
		snapshotBaseURL := c.BaseURL
		if snapshotBaseURL == "" {
			snapshotBaseURL = s.cfg.BaseURL
		}
		s.mu.RUnlock()
		if !ok || revoked {
			return c, localFail(http.StatusUnauthorized, "credential_unavailable", "upstream credential was replaced or removed")
		}
		if epoch != c.runtimeEpoch || current.APIKey != c.APIKey || current.ControlKey != c.ControlKey || current.WorkspaceID != c.WorkspaceID || current.Disabled != c.Disabled || current.QuotaRevision != c.QuotaRevision || currentBaseURL != snapshotBaseURL {
			return c, localFail(http.StatusConflict, "credential_changed", "upstream credential changed while the model test was starting")
		}
		c.BaseURL = snapshotBaseURL
	} else {
		if len(request.StorageJSON) > 0 {
			_ = json.Unmarshal(request.StorageJSON, &c)
		}
		s.mu.RLock()
		if current, ok := s.creds[c.ID]; ok {
			c = current
		} else if current, ok := s.creds[request.AuthID]; ok {
			c = current
		}
		s.mu.RUnlock()
	}
	s.mu.RLock()
	revoked := s.revoked[c.ID] || s.revoked[request.AuthID]
	s.mu.RUnlock()
	if c.APIKey == "" {
		return c, localFail(http.StatusUnauthorized, "credential_unavailable", "upstream credential is missing")
	}
	if c.Disabled || revoked {
		return c, localFail(http.StatusUnauthorized, "credential_unavailable", "upstream credential is disabled or removed")
	}
	if c.quotaBlocked() {
		return c, localFail(http.StatusTooManyRequests, "quota_blocked", quotaBlockMarker+" upstream account quota is temporarily paused")
	}
	if c.BaseURL == "" {
		c.BaseURL = s.config().BaseURL
	}
	if err := validateBaseURL(c.BaseURL); err != nil {
		return c, err
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	return c, nil
}

// saveCredential writes the credential through the host so it lands in auth-dir
// and the runtime record is upserted in one step.
//
// The in-flight body is published to pendingSaves first because host.auth.save
// re-enters auth.parse before it writes the file (see pending.go). That pending
// snapshot is only used to *answer* the reentrant parse; the live record is
// replaced here, after the host confirmed the write. Publishing inside
// auth.parse instead would leave the plugin's live credential ahead of disk when
// the write or the record upsert fails (POST answers 500): a retried enable would
// then route traffic through a control key / disabled flag disk never stored.
func (s *Service) saveCredential(c Credential, filename string) error {
	body := jsonBytes(c)
	s.mirrorPending(filename, body, c.ID)
	defer s.clearPending(filename, c.ID)
	var saved struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := s.call("host.auth.save", map[string]any{"name": filename, "json": json.RawMessage(body)}, &saved); err != nil {
		return err
	}
	// authFileMu orders this publish against a watcher callback that is reading
	// the file it just wrote: parseAuth publishes what it read while still
	// holding authFileMu, so the confirmed (newest) body always wins the race
	// instead of being overwritten by a stale disk snapshot afterwards.
	s.authFileMu.Lock()
	s.mu.Lock()
	if saved.Path != "" {
		s.authDir = filepath.Dir(saved.Path)
	}
	// The host accepted this body, so it is now what both disk and the host
	// record hold. Publishing here also covers a save whose reentrant
	// auth.parse never ran. Retry markers belong to the previous inference key,
	// not merely the ID, and must not follow a successful key replacement.
	if previous, exists := s.creds[c.ID]; !exists || previous.APIKey != c.APIKey {
		delete(s.quotaPauseErrors, c.ID)
		delete(s.quotaDiskStale, c.ID)
	}
	s.storeCredentialLocked(c)
	// The written file carries exactly this credential's rules, so the policy is
	// known to be in sync for this account.
	s.markErrorRulesSyncedLocked(c.ID, c.RequestScopedErrors, nil)
	if filename != "" {
		s.authFiles[c.ID] = filepath.Base(filename)
	}
	s.mu.Unlock()
	s.authFileMu.Unlock()
	return nil
}

// storeCredentialLocked publishes a complete credential without dropping optional
// billing fields. Callers hold mu, including reentrant auth.parse; no host call
// or additional lock may be taken here.
func (s *Service) storeCredentialLocked(c Credential) {
	previous, exists := s.creds[c.ID]
	identityChanged := !exists || previous.APIKey != c.APIKey || previous.ControlKey != c.ControlKey ||
		previous.WorkspaceID != c.WorkspaceID || previous.Disabled != c.Disabled || previous.BaseURL != c.BaseURL
	if identityChanged {
		delete(s.quotaRecheckStatus, c.ID)
		delete(s.quotaRecheckRetryAt, c.ID)
		delete(s.quotaRecheckFailures, c.ID)
		s.credentialEpoch[c.ID]++
		s.invalidateBillingLocked(c.ID)
		s.clearModelTestsLocked(c.ID)
		// A new random stamp, never derived from a key and never persisted: the
		// console only compares it to know its cached quota belongs to another
		// billing identity.
		c.billingRevision = newID()
	} else {
		c.billingRevision = previous.billingRevision
	}
	c.runtimeEpoch = s.credentialEpoch[c.ID]
	s.creds[c.ID] = c
}

func (s *Service) invalidateBillingLocked(id string) {
	s.billingEpoch[id]++
	if entry := s.quota[id]; entry != nil {
		close(entry.invalidated)
		delete(s.quota, id)
	}
	for key, entry := range s.modelCoverage {
		if key.CredentialID != id {
			continue
		}
		close(entry.invalidated)
		delete(s.modelCoverage, key)
	}
}

func jsonBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
