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
	s.mu.Lock()
	if s.revoked[c.ID] && !resolvedFile {
		// A watcher may have queued this callback before the delete landed.
		s.mu.Unlock()
		return map[string]any{"Handled": false}, nil
	}
	if resolvedFile {
		delete(s.revoked, c.ID)
	}
	s.storeCredentialLocked(c)
	if r.FileName != "" {
		s.authFiles[c.ID] = filepath.Base(r.FileName)
	}
	if r.Host.AuthDir != "" {
		s.authDir = r.Host.AuthDir
	}
	s.mu.Unlock()
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
		"Disabled":    c.Disabled,
		"StorageJSON": jsonBytes(c),
		"Metadata":    map[string]any{"type": Provider},
		"Attributes":  map[string]string{"auth_kind": "api_key"},
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
	if len(request.StorageJSON) > 0 {
		_ = json.Unmarshal(request.StorageJSON, &c)
	}
	s.mu.RLock()
	if current, ok := s.creds[c.ID]; ok {
		c = current
	} else if current, ok := s.creds[request.AuthID]; ok {
		c = current
	}
	revoked := s.revoked[c.ID] || s.revoked[request.AuthID]
	s.mu.RUnlock()
	if c.APIKey == "" {
		return c, fail(http.StatusUnauthorized, "OpenDesign credential is missing")
	}
	if c.Disabled || revoked {
		return c, fail(http.StatusUnauthorized, "OpenDesign credential is disabled or removed")
	}
	if c.BaseURL == "" {
		c.BaseURL = s.config().BaseURL
	}
	if err := validateBaseURL(c.BaseURL); err != nil {
		return c, err
	}
	return c, nil
}

// saveCredential writes the credential through the host so it lands in auth-dir
// and the runtime record is upserted in one step.
//
// The in-flight body is published to pendingSaves first because host.auth.save
// re-enters auth.parse before it writes the file (see pending.go).
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
	if saved.Path != "" {
		s.mu.Lock()
		s.authDir = filepath.Dir(saved.Path)
		s.mu.Unlock()
	}
	return nil
}

// storeCredentialLocked publishes a complete credential without dropping optional
// billing fields. Callers hold mu, including reentrant auth.parse; no host call
// or additional lock may be taken here.
func (s *Service) storeCredentialLocked(c Credential) {
	previous, exists := s.creds[c.ID]
	if !exists || previous.APIKey != c.APIKey || previous.ControlKey != c.ControlKey || previous.WorkspaceID != c.WorkspaceID {
		s.invalidateBillingLocked(c.ID)
	}
	s.creds[c.ID] = c
}

func (s *Service) invalidateBillingLocked(id string) {
	s.billingEpoch[id]++
	if entry := s.quota[id]; entry != nil {
		close(entry.invalidated)
		delete(s.quota, id)
	}
}

func jsonBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
