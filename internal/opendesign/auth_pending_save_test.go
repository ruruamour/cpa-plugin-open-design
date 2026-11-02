package opendesign

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failingSaveHost runs the host's reentrant auth.parse and then fails the save,
// which is exactly CPA's order in saveAuthFile: parse, os.WriteFile, upsert. A
// read-only auth dir (P2 regression) fails after the parse, so the plugin must
// not already have published the new body.
type failingSaveHost struct {
	*fakeHost
	failSave bool
}

func (h *failingSaveHost) call(method string, in, out any) error {
	if method != "host.auth.save" {
		return h.fakeHost.call(method, in, out)
	}
	h.mu.Lock()
	fail := h.failSave
	h.failSave = false
	hook := h.saveHook
	h.mu.Unlock()
	if !fail {
		return h.fakeHost.call(method, in, out)
	}
	if hook != nil {
		hook(stringField(in, "name"), rawField(in, "json"))
	}
	return errString("failed to write auth file: no space left on device")
}

// newFailingHostService wires a Service whose next host.auth.save can fail after
// the reentrant parse already ran.
func newFailingHostService(t *testing.T) (*Service, *failingSaveHost) {
	t.Helper()
	s, host := newTestService(t)
	failing := &failingSaveHost{fakeHost: host}
	s.SetHost(failing.call)
	return s, failing
}

func liveCredential(s *Service, id string) (Credential, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.creds[id]
	return c, ok
}

func billingEpochOf(s *Service, id string) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.billingEpoch[id]
}

// syncDisk materialises what the fake host last accepted as the file on disk, so
// the plugin's disk fallback reads the same content a real host would have.
func syncDisk(t *testing.T, host *fakeHost, filename string) {
	t.Helper()
	body := host.savedAuth(filename)
	if len(body) == 0 {
		t.Fatalf("no stored credential for %s", filename)
	}
	host.writeAuthFile(t, filename, body)
}

// storedCredential decodes the body the host accepted, which is what a real host
// then writes to the auth file.
func storedCredential(t *testing.T, host *fakeHost, filename string) Credential {
	t.Helper()
	b := host.savedAuth(filename)
	if len(b) == 0 {
		t.Fatalf("no accepted credential body for %s", filename)
	}
	var c Credential
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("decode accepted credential: %v", err)
	}
	return c
}

// diskCredential decodes the credential file the plugin and CPA both observe.
func diskCredential(t *testing.T, host *fakeHost, filename string) Credential {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(host.authDir, filename))
	if err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	var c Credential
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("decode credential file: %v", err)
	}
	return c
}

// A save whose reentrant parse succeeded but whose file write failed must leave
// the live record and disk agreeing on the *previous* content, so the retry does
// not route through a control key that was never stored.
func TestFailedSaveAfterReentrantParseKeepsControlKeyLiveAndOnDisk(t *testing.T) {
	s, host := newFailingHostService(t)
	id := addCredential(t, s, host.fakeHost, "primary", "sk-amr-pendingfail-0001")
	const oldControl = "ctl-old-pendingfail-0001"
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": oldControl}); status != http.StatusOK {
		t.Fatalf("seed control key: %d %s", status, raw)
	}
	filename := id + ".json"
	syncDisk(t, host.fakeHost, filename)
	diskBefore := diskCredential(t, host.fakeHost, filename)
	if diskBefore.ControlKey != oldControl {
		t.Fatalf("seed did not reach disk: %q", diskBefore.ControlKey)
	}
	epochBefore := billingEpochOf(s, id)

	// The host re-enters auth.parse with the new body, then fails the write.
	host.mu.Lock()
	host.saveHook = func(name string, raw []byte) {
		if name != filename {
			return
		}
		if _, err := s.parseAuth(mustJSON(t, map[string]any{
			"Provider": Provider,
			"FileName": name,
			"RawJSON":  raw,
			"Host":     map[string]any{"AuthDir": host.authDir},
		})); err != nil {
			t.Errorf("reentrant auth.parse: %v", err)
		}
	}
	host.failSave = true
	host.mu.Unlock()

	const newControl = "ctl-new-pendingfail-0001"
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": newControl})
	if status != http.StatusInternalServerError {
		t.Fatalf("failed save status = %d, want 500: %s", status, raw)
	}
	if strings.Contains(raw, newControl) {
		t.Fatalf("failure response leaked the control key: %s", raw)
	}

	live, ok := liveCredential(s, id)
	if !ok {
		t.Fatalf("credential disappeared")
	}
	if live.ControlKey != oldControl {
		t.Fatalf("live control key = %q after a failed save; the plugin published an unconfirmed credential", live.ControlKey)
	}
	if got := diskCredential(t, host.fakeHost, filename).ControlKey; got != oldControl {
		t.Fatalf("disk control key = %q after a failed save", got)
	}
	if epoch := billingEpochOf(s, id); epoch != epochBefore {
		t.Fatalf("billing state was invalidated for a save that never landed (%d -> %d)", epochBefore, epoch)
	}
	// A watcher callback for the unchanged file must still resolve from disk and
	// agree with the live record.
	out, err := s.parseAuth(mustJSON(t, map[string]any{
		"Provider": Provider,
		"FileName": filename,
		"RawJSON":  []byte(`{}`),
		"Host":     map[string]any{"AuthDir": host.authDir},
	}))
	if err != nil {
		t.Fatalf("watcher parse: %v", err)
	}
	auth := out.(map[string]any)["Auth"].(map[string]any)
	var reparsed Credential
	if err := json.Unmarshal(auth["StorageJSON"].([]byte), &reparsed); err != nil {
		t.Fatalf("decode parsed credential: %v", err)
	}
	if reparsed.ControlKey != live.ControlKey || reparsed.Disabled != live.Disabled {
		t.Fatalf("live and disk disagree: live=%+v parsed=%+v", live, reparsed)
	}

	// The same update must still work once the host accepts the write.
	status, _, raw = managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": newControl})
	if status != http.StatusOK {
		t.Fatalf("retry status = %d, want 200: %s", status, raw)
	}
	if live, _ := liveCredential(s, id); live.ControlKey != newControl {
		t.Fatalf("retry did not publish the new control key: %q", live.ControlKey)
	}
	if got := storedCredential(t, host.fakeHost, filename).ControlKey; got != newControl {
		t.Fatalf("retry did not reach disk: %q", got)
	}
}

// The enable-after-disable flow from the reviewer report: the reentrant parse
// sees disabled:false, the write then fails, and the live record must stay
// disabled because the host never stored the enabled record.
func TestFailedSaveAfterReentrantParseKeepsDisabledState(t *testing.T) {
	s, host := newFailingHostService(t)
	id := addCredential(t, s, host.fakeHost, "primary", "sk-amr-pendingfail-0002")
	filename := id + ".json"
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "disabled": true}); status != http.StatusOK {
		t.Fatalf("disable: %d %s", status, raw)
	}
	if live, _ := liveCredential(s, id); !live.Disabled {
		t.Fatalf("disable did not publish")
	}

	syncDisk(t, host.fakeHost, filename)
	host.mu.Lock()
	host.saveHook = func(name string, raw []byte) {
		if name != filename {
			return
		}
		if _, err := s.parseAuth(mustJSON(t, map[string]any{
			"Provider": Provider,
			"FileName": name,
			"RawJSON":  raw,
			"Host":     map[string]any{"AuthDir": host.authDir},
		})); err != nil {
			t.Errorf("reentrant auth.parse: %v", err)
		}
	}
	host.failSave = true
	host.mu.Unlock()

	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "disabled": false})
	if status != http.StatusInternalServerError {
		t.Fatalf("failed enable status = %d, want 500: %s", status, raw)
	}
	if live, _ := liveCredential(s, id); !live.Disabled {
		t.Fatalf("failed enable left the live credential enabled while disk still says disabled")
	}
	if !diskCredential(t, host.fakeHost, filename).Disabled {
		t.Fatalf("disk no longer says disabled after a failed write")
	}
	// The credential must stay unusable-for-enable but still consistent: a
	// request routed now sees the disabled record both live and on disk.
	if _, err := s.selectedCredential(ExecutorRequest{AuthID: id}); err == nil {
		t.Fatalf("a credential the host never enabled accepted traffic")
	}
}

// clear_billing on a save that fails must not silently keep the plugin reporting
// "billing not configured" while the file still holds the key.
func TestFailedSaveAfterReentrantParseKeepsBilling(t *testing.T) {
	s, host := newFailingHostService(t)
	id := addCredential(t, s, host.fakeHost, "primary", "sk-amr-pendingfail-0003")
	const control = "ctl-clear-pendingfail-0003"
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": control, "workspace_id": "ws-1"}); status != http.StatusOK {
		t.Fatalf("seed billing: %d %s", status, raw)
	}
	filename := id + ".json"
	syncDisk(t, host.fakeHost, filename)
	epochBefore := billingEpochOf(s, id)

	host.mu.Lock()
	host.saveHook = func(name string, raw []byte) {
		if name != filename {
			return
		}
		if _, err := s.parseAuth(mustJSON(t, map[string]any{
			"Provider": Provider,
			"FileName": name,
			"RawJSON":  raw,
			"Host":     map[string]any{"AuthDir": host.authDir},
		})); err != nil {
			t.Errorf("reentrant auth.parse: %v", err)
		}
	}
	host.failSave = true
	host.mu.Unlock()

	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "clear_billing": true})
	if status != http.StatusInternalServerError {
		t.Fatalf("failed clear_billing status = %d, want 500: %s", status, raw)
	}
	live, _ := liveCredential(s, id)
	if live.ControlKey != control || live.WorkspaceID != "ws-1" {
		t.Fatalf("failed clear_billing dropped live billing state: %+v", live)
	}
	disk := diskCredential(t, host.fakeHost, filename)
	if disk.ControlKey != control || disk.WorkspaceID != "ws-1" {
		t.Fatalf("disk lost billing state: %+v", disk)
	}
	if epoch := billingEpochOf(s, id); epoch != epochBefore {
		t.Fatalf("billing cache was dropped for a clear that never landed")
	}
	// The console view is derived from the live record, so it must still report
	// the configured billing.
	_, body, response := managementCall(t, s, http.MethodGet, apiBase+"/credentials", nil, nil)
	view := body["credentials"].([]any)[0].(map[string]any)
	if view["billing_configured"] != true || view["workspace_id"] != "ws-1" {
		t.Fatalf("view disagrees with disk after a failed clear: %s", response)
	}
}

// A create whose save fails after the reentrant parse must not leave a live
// credential the host and disk do not know about.
func TestFailedSaveAfterReentrantParseCreatesNothing(t *testing.T) {
	s, host := newFailingHostService(t)
	id := addCredential(t, s, host.fakeHost, "seed", "sk-amr-pendingfail-0004")
	filename := id + ".json"
	host.mu.Lock()
	host.saveHook = func(name string, raw []byte) {
		if _, err := s.parseAuth(mustJSON(t, map[string]any{
			"Provider": Provider,
			"FileName": name,
			"RawJSON":  raw,
			"Host":     map[string]any{"AuthDir": host.authDir},
		})); err != nil {
			t.Errorf("reentrant auth.parse: %v", err)
		}
	}
	host.failSave = true
	host.mu.Unlock()

	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"label": "second", "api_key": "sk-amr-pendingfail-new-0005"})
	if status != http.StatusInternalServerError {
		t.Fatalf("failed create status = %d, want 500: %s", status, raw)
	}
	s.mu.RLock()
	live := len(s.creds)
	_, seeded := s.creds[id]
	s.mu.RUnlock()
	if live != 1 || !seeded {
		t.Fatalf("failed create left %d live credentials, want only the seeded one", live)
	}
	host.mu.Lock()
	stored := make([]string, 0, len(host.savedAuths))
	for name := range host.savedAuths {
		stored = append(stored, name)
	}
	host.mu.Unlock()
	if len(stored) != 1 || stored[0] != filename {
		t.Fatalf("failed create stored %v, want only the seeded file", stored)
	}
}

// The delete flow disables the record through host.auth.save first. If that save
// fails after the reentrant parse, the credential must stay enabled so the
// retried delete does not claim a state the host never stored.
func TestFailedDisableSaveDuringDeleteKeepsCredential(t *testing.T) {
	s, host := newFailingHostService(t)
	id := addCredential(t, s, host.fakeHost, "primary", "sk-amr-pendingfail-0006")
	filename := id + ".json"
	body, _ := json.Marshal(Credential{Type: Provider, ID: id, Label: "primary", APIKey: "sk-amr-pendingfail-0006"})
	path := host.writeAuthFile(t, filename, body)

	host.mu.Lock()
	host.saveHook = func(name string, raw []byte) {
		if name != filename {
			return
		}
		if _, err := s.parseAuth(mustJSON(t, map[string]any{
			"Provider": Provider,
			"FileName": name,
			"RawJSON":  raw,
			"Host":     map[string]any{"AuthDir": host.authDir},
		})); err != nil {
			t.Errorf("reentrant auth.parse: %v", err)
		}
	}
	host.failSave = true
	host.mu.Unlock()

	status, _, raw := managementCall(t, s, http.MethodDelete, apiBase+"/credentials", map[string][]string{"id": {id}}, nil)
	if status != http.StatusBadGateway {
		t.Fatalf("delete status = %d, want 502: %s", status, raw)
	}
	if strings.Contains(raw, "sk-amr-pendingfail-0006") {
		t.Fatalf("delete error leaked the key: %s", raw)
	}
	live, ok := liveCredential(s, id)
	if !ok || live.Disabled {
		t.Fatalf("failed disable-save left the live record %+v (want enabled and present)", live)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("failed delete removed the file: %v", err)
	}
	s.mu.RLock()
	revoked := s.revoked[id]
	s.mu.RUnlock()
	if revoked {
		t.Fatalf("failed delete revoked the credential")
	}
}

// A successful reentrant save must not be rolled back by a watcher callback that
// read the previous file content before the write landed.
func TestSuccessfulReentrantSaveSurvivesLateStaleWatcher(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-pendingfail-0007")
	filename := id + ".json"
	stale, _ := json.Marshal(Credential{Type: Provider, ID: id, Label: "primary", APIKey: "sk-amr-pendingfail-0007", ControlKey: "ctl-stale-0007", Disabled: true})
	host.writeAuthFile(t, filename, stale)

	// CPA's order: parse the new body, then write it to disk.
	host.mu.Lock()
	host.saveHook = func(name string, raw []byte) {
		if name != filename {
			return
		}
		if _, err := s.parseAuth(mustJSON(t, map[string]any{
			"Provider": Provider,
			"FileName": name,
			"RawJSON":  stale,
			"Host":     map[string]any{"AuthDir": host.authDir},
		})); err != nil {
			t.Errorf("reentrant auth.parse: %v", err)
		}
		host.writeAuthFile(t, name, raw)
	}
	host.mu.Unlock()

	const newControl = "ctl-fresh-0007"
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": newControl, "disabled": false})
	if status != http.StatusOK {
		t.Fatalf("update: %d %s", status, raw)
	}
	live, _ := liveCredential(s, id)
	if live.ControlKey != newControl || live.Disabled {
		t.Fatalf("successful save did not publish: %+v", live)
	}

	// A watcher callback that carries the pre-save snapshot arrives afterwards.
	out, err := s.parseAuth(mustJSON(t, map[string]any{
		"Provider": Provider,
		"FileName": filename,
		"RawJSON":  stale,
		"Host":     map[string]any{"AuthDir": host.authDir},
	}))
	if err != nil {
		t.Fatalf("late watcher parse: %v", err)
	}
	if handled, _ := out.(map[string]any)["Handled"].(bool); !handled {
		t.Fatalf("late watcher callback was rejected")
	}
	live, _ = liveCredential(s, id)
	if live.ControlKey != newControl || live.Disabled {
		t.Fatalf("late watcher rolled the live record back: %+v", live)
	}
	if _, pending := s.pendingFor(filename, id); pending {
		t.Fatalf("pending snapshot leaked after a successful save")
	}
}
