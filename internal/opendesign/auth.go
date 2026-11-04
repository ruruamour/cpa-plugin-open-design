package opendesign

import (
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
		return nil, fail(http.StatusUnauthorized, "OpenDesign credential has no api_key")
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
		if current.APIKey == c.APIKey && current.QuotaRevision != "" && current.QuotaRevision != c.QuotaRevision {
			// A watcher can have read an old snapshot just before a quota write.
			// Keep the live plugin-owned block while still accepting other fields.
			c.QuotaBlock = cloneQuotaBlock(current.QuotaBlock)
			c.QuotaRevision = current.QuotaRevision
		} else if current.APIKey != c.APIKey {
			delete(s.quotaPauseErrors, c.ID)
		}
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
		return nil, fail(http.StatusUnauthorized, "OpenDesign credential was removed")
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
			return c, localFail(http.StatusUnauthorized, "credential_unavailable", "OpenDesign credential was replaced or removed")
		}
		if epoch != c.runtimeEpoch || current.APIKey != c.APIKey || current.ControlKey != c.ControlKey || current.WorkspaceID != c.WorkspaceID || current.Disabled != c.Disabled || current.QuotaRevision != c.QuotaRevision || currentBaseURL != snapshotBaseURL {
			return c, localFail(http.StatusConflict, "credential_changed", "OpenDesign credential changed while the model test was starting")
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
		return c, localFail(http.StatusUnauthorized, "credential_unavailable", "OpenDesign credential is missing")
	}
	if c.Disabled || revoked {
		return c, localFail(http.StatusUnauthorized, "credential_unavailable", "OpenDesign credential is disabled or removed")
	}
	if c.quotaBlocked() {
		return c, localFail(http.StatusTooManyRequests, "quota_blocked", quotaBlockMarker+" OpenDesign account quota is temporarily paused")
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
	// auth.parse never ran.
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
