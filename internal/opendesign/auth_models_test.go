package opendesign

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// CPA re-enters auth.parse from buildAuthsFromFileData before writing the auth
// file. If parse trusts disk at that moment it sees the *previous* content, so an
// operator enabling a disabled credential would leave the host record disabled.
func TestReentrantAuthSaveUsesPendingSnapshot(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-reentrant-0001")
	filename := id + ".json"

	// The file on disk still says disabled, as after an earlier disable.
	stale, _ := json.Marshal(Credential{Type: Provider, ID: id, Label: "primary", APIKey: "sk-amr-reentrant-0001", Disabled: true})
	host.writeAuthFile(t, filename, stale)

	var seen []map[string]any
	host.mu.Lock()
	host.saveHook = func(name string, raw []byte) {
		if name != filename {
			return
		}
		// This is the host's parse callback: disk is stale, the payload is new.
		out, err := s.parseAuth(mustJSON(t, map[string]any{
			"Provider": Provider,
			"FileName": name,
			"RawJSON":  stale,
			"Host":     map[string]any{"AuthDir": host.authDir},
		}))
		if err != nil {
			t.Errorf("reentrant auth.parse: %v", err)
			return
		}
		parsed := out.(map[string]any)
		auth, _ := parsed["Auth"].(map[string]any)
		seen = append(seen, auth)
	}
	host.mu.Unlock()

	// Enabling the credential is exactly the flow that used to lose the write.
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "disabled": false})
	if status != http.StatusOK {
		t.Fatalf("enable: %d %s", status, raw)
	}
	if body["disabled"] != false {
		t.Fatalf("enable response = %v", body)
	}
	if len(seen) != 1 {
		t.Fatalf("auth.parse ran %d times, want 1", len(seen))
	}
	if seen[0]["Disabled"] != false {
		t.Fatalf("reentrant parse saw disabled=%v; it used the stale disk copy", seen[0]["Disabled"])
	}
	if seen[0]["ID"] != id {
		t.Fatalf("reentrant parse id = %v", seen[0]["ID"])
	}
	// The live record must agree with what the host was told.
	s.mu.RLock()
	live := s.creds[id]
	s.mu.RUnlock()
	if live.Disabled {
		t.Fatalf("in-memory credential stayed disabled after enable")
	}
	// The save must be complete: no pending snapshot left behind.
	if _, pending := s.pendingFor(filename, id); pending {
		t.Fatalf("pending save leaked after host.auth.save returned")
	}
}

// A save that fails must not leave a pending snapshot that would mask later
// watcher callbacks.
func TestFailedAuthSaveClearsPendingSnapshot(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-failsave-0002")
	host.mu.Lock()
	host.saveAuthErr = errString("auth dir is read-only")
	host.mu.Unlock()
	status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "disabled": true})
	if status == http.StatusOK {
		t.Fatalf("update reported success although host.auth.save failed")
	}
	if _, pending := s.pendingFor(id+".json", id); pending {
		t.Fatalf("failed save left a pending snapshot behind")
	}
}

// The pending snapshot is scoped to the file being saved: another credential's
// parse must still read its own file from disk.
func TestPendingSnapshotDoesNotLeakAcrossFiles(t *testing.T) {
	s, host := newTestService(t)
	a := addCredential(t, s, host, "alpha", "sk-amr-alpha-0003")
	b := addCredential(t, s, host, "beta", "sk-amr-beta-0004")
	otherFile := b + ".json"
	otherBody, _ := json.Marshal(Credential{Type: Provider, ID: b, Label: "beta", APIKey: "sk-amr-beta-0004", Disabled: true})
	host.writeAuthFile(t, otherFile, otherBody)

	s.mirrorPending(a+".json", []byte(`{"type":"open-design","id":"`+a+`"}`), a)
	out, err := s.parseAuth(mustJSON(t, map[string]any{
		"Provider": Provider,
		"FileName": otherFile,
		"RawJSON":  []byte(`{}`),
		"Host":     map[string]any{"AuthDir": host.authDir},
	}))
	if err != nil {
		t.Fatalf("parse other file: %v", err)
	}
	auth, _ := out.(map[string]any)["Auth"].(map[string]any)
	if auth["Disabled"] != true {
		t.Fatalf("unrelated parse did not read its own file: %v", auth)
	}
	s.clearPending(a+".json", a)
}

// CPA passes FileName as authIDForPath(path): usually the base name, but it can
// also be the credential ID or a full path. Every form must hit the snapshot.
func TestPendingSnapshotMatchesEveryHostKeyForm(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-keys-0009")
	filename := id + ".json"
	full := filepath.Join(host.authDir, filename)
	body, err := json.Marshal(Credential{Type: Provider, ID: id, Label: "primary", APIKey: "sk-amr-keys-0009", Disabled: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	stale, _ := json.Marshal(Credential{Type: Provider, ID: id, Label: "primary", APIKey: "sk-amr-keys-0009", Disabled: true})
	host.writeAuthFile(t, filename, stale)
	s.mirrorPending(filename, body, id)
	defer s.clearPending(filename, id)

	for _, key := range []string{filename, id, full} {
		out, err := s.parseAuth(mustJSON(t, map[string]any{
			"Provider": Provider,
			"FileName": key,
			"RawJSON":  stale,
			"Host":     map[string]any{"AuthDir": host.authDir},
		}))
		if err != nil {
			t.Fatalf("parse with FileName=%q: %v", key, err)
		}
		auth, _ := out.(map[string]any)["Auth"].(map[string]any)
		if auth["Disabled"] != false {
			t.Fatalf("FileName=%q did not use the pending snapshot: %v", key, auth)
		}
	}

	// Once the save finishes, parse must go back to reading disk.
	s.clearPending(filename, id)
	out, err := s.parseAuth(mustJSON(t, map[string]any{
		"Provider": Provider,
		"FileName": filename,
		"RawJSON":  body,
		"Host":     map[string]any{"AuthDir": host.authDir},
	}))
	if err != nil {
		t.Fatalf("parse after clear: %v", err)
	}
	auth, _ := out.(map[string]any)["Auth"].(map[string]any)
	if auth["Disabled"] != true {
		t.Fatalf("clearing the pending snapshot broke the on-disk read: %v", auth)
	}
}

// A watcher callback for a credential with no save in flight must never be
// satisfied by another credential's snapshot.
func TestPendingSnapshotIsKeyedByCredential(t *testing.T) {
	s, host := newTestService(t)
	a := addCredential(t, s, host, "alpha", "sk-amr-keyed-0010")
	b := addCredential(t, s, host, "beta", "sk-amr-keyed-0011")
	body, _ := json.Marshal(Credential{Type: Provider, ID: a, Label: "alpha", APIKey: "sk-amr-keyed-0010", Disabled: false})
	s.mirrorPending(a+".json", body, a)
	defer s.clearPending(a+".json", a)

	if _, ok := s.pendingFor(b + ".json"); ok {
		t.Fatalf("credential b was matched by credential a's snapshot")
	}
	if _, ok := s.pendingFor(b); ok {
		t.Fatalf("credential b id was matched by credential a's snapshot")
	}
}

// An explicit disabled:false has to survive the JSON round trip. omitempty used
// to drop it, so a re-parse would default the field back to false only by luck;
// with the pending snapshot a stale `true` was the dangerous direction.
func TestDisabledFalseIsSerialized(t *testing.T) {
	body, err := json.Marshal(Credential{Type: Provider, ID: "x", APIKey: "sk-amr-x-0005", Disabled: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), `"disabled":false`) {
		t.Fatalf("disabled:false missing from %s", body)
	}
}

// importModels must persist, publish and bump the model revision on every
// credential so CPA re-runs model.for_auth. Otherwise a console flow of
// "add key, then import models" leaves the host with an empty catalog and every
// request fails with model_not_found.
func TestImportModelsRefreshesHostModelRegistration(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-import-0006")

	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]any{{"id": "od-large", "upstream_id": "opendesign-large"}},
	})
	if status != http.StatusOK {
		t.Fatalf("import: %d %s", status, raw)
	}
	if len(body["models"].([]any)) != 1 {
		t.Fatalf("import response = %s", raw)
	}

	// The credential was rewritten, so the host sees changed content and re-runs
	// model.for_auth for it.
	saved := host.savedAuth(id + ".json")
	if len(saved) == 0 {
		t.Fatalf("importModels did not re-save the credential")
	}
	var onDisk Credential
	if err := json.Unmarshal(saved, &onDisk); err != nil {
		t.Fatalf("saved credential: %v", err)
	}
	if onDisk.ModelRevision == 0 {
		t.Fatalf("saved credential carries no model revision: %s", saved)
	}
	if onDisk.APIKey != "sk-amr-import-0006" {
		t.Fatalf("re-saved credential lost its key")
	}
	// model.for_auth now publishes the imported mapping.
	models := s.modelRegistration().(map[string]any)["Models"].([]map[string]any)
	if len(models) != 1 || models[0]["ID"] != "od-large" {
		t.Fatalf("model registration = %v", models)
	}
	// The mapping is on disk so a restart keeps it.
	settings, err := readSettings(filepath.Join(s.config().DataDir, settingsFile))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if len(settings.Models) != 1 || settings.Models[0].UpstreamID != "opendesign-large" {
		t.Fatalf("persisted settings = %+v", settings.Models)
	}
}

// A second import must bump the revision again, otherwise the host would treat
// the file as unchanged and keep the previous catalog.
func TestImportModelsBumpsRevisionMonotonically(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-revision-0007")
	importOne := func(upstream string) int64 {
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []map[string]any{{"id": "od-large", "upstream_id": upstream}},
		})
		if status != http.StatusOK {
			t.Fatalf("import %s: %d %s", upstream, status, raw)
		}
		var c Credential
		if err := json.Unmarshal(host.savedAuth(id+".json"), &c); err != nil {
			t.Fatalf("decode saved credential: %v", err)
		}
		return c.ModelRevision
	}
	first := importOne("opendesign-large")
	second := importOne("opendesign-large-v2")
	if first == 0 || second <= first {
		t.Fatalf("revisions did not increase: %d then %d", first, second)
	}
}

// Two concurrent imports must not leave disk and memory disagreeing. The first
// writer is parked right after its rename, which is the exact window where an
// unlocked publish would let the loser's snapshot win in memory.
func TestConcurrentImportsKeepDiskAndMemoryConsistent(t *testing.T) {
	s, host := newTestService(t)
	addCredential(t, s, host, "primary", "sk-amr-concurrent-0008")

	parked := make(chan struct{})
	release := make(chan struct{})
	var first atomic.Bool
	// Only the first writer parks. sync.Once would be wrong here: it serializes
	// the hook calls themselves, which would hide a missing configMu.
	hook := func() {
		if first.CompareAndSwap(false, true) {
			close(parked)
			<-release
		}
	}
	testSettingsWriteHook.Store(&hook)
	defer testSettingsWriteHook.Store(nil)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []map[string]any{{"id": "od-large", "upstream_id": "upstream-a"}},
		})
	}()
	<-parked

	secondDone := make(chan struct{})
	var secondStatus int
	go func() {
		defer close(secondDone)
		secondStatus, _, _ = managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []map[string]any{{"id": "od-large", "upstream_id": "upstream-b"}},
		})
	}()
	// The second import must block on configMu instead of racing the publish.
	select {
	case <-secondDone:
		t.Fatalf("second import did not serialize behind the first")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-firstDone
	<-secondDone
	if secondStatus != http.StatusOK {
		t.Fatalf("second import status = %d", secondStatus)
	}

	memory := s.config().Models
	onDisk, err := readSettings(filepath.Join(s.config().DataDir, settingsFile))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if len(memory) != 1 || len(onDisk.Models) != 1 {
		t.Fatalf("unexpected model counts: memory=%v disk=%v", memory, onDisk.Models)
	}
	if memory[0].UpstreamID != onDisk.Models[0].UpstreamID {
		t.Fatalf("disk and memory diverged: disk=%q memory=%q", onDisk.Models[0].UpstreamID, memory[0].UpstreamID)
	}
}

// readSettings decodes the persisted settings file.
func readSettings(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Delete must first hand the host a disabled record so CPA re-runs model.for_auth
// and withdraws the catalog, then unlink the file, then stay revoked.
// CPA 0.48 exposes no host.auth.delete (only list/get/get_runtime/save/models).
func TestDeleteCredentialWithdrawsHostModelsBeforeUnlink(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-delete-0012")
	filename := id + ".json"
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{"models": []map[string]string{
		{"id": "od-large", "upstream_id": "opendesign-large"},
	}}); status != http.StatusOK {
		t.Fatalf("seed models: %d %s", status, raw)
	}
	if models := s.modelRegistration().(map[string]any)["Models"].([]map[string]any); len(models) == 0 {
		t.Fatalf("models were not registered before the delete")
	}

	// The credential file exists on disk, as after a successful create.
	body, _ := json.Marshal(Credential{Type: Provider, ID: id, Label: "primary", APIKey: "sk-amr-delete-0012"})
	path := host.writeAuthFile(t, filename, body)

	var saves []Credential
	host.mu.Lock()
	host.saveHook = func(name string, raw []byte) {
		if name != filename {
			return
		}
		var c Credential
		if err := json.Unmarshal(raw, &c); err == nil {
			saves = append(saves, c)
		}
	}
	host.mu.Unlock()

	status, resp, raw := managementCall(t, s, http.MethodDelete, apiBase+"/credentials", map[string][]string{"id": {id}}, nil)
	if status != http.StatusOK || resp["deleted"] != true {
		t.Fatalf("delete: %d %s", status, raw)
	}
	// The host saw a disabled record before the unlink, which is what withdraws
	// the models (registerModelsForAuthWithConfigAndCache -> unregister).
	if len(saves) != 1 {
		t.Fatalf("host.auth.save called %d times, want 1", len(saves))
	}
	if !saves[0].Disabled {
		t.Fatalf("delete did not disable the record the host was given")
	}
	if saves[0].APIKey != "sk-amr-delete-0012" {
		t.Fatalf("disable-before-delete lost the key")
	}
	// The file is gone and the id stays revoked so a queued watcher callback
	// cannot resurrect it.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("credential file still present: %v", err)
	}
	s.mu.RLock()
	_, live := s.creds[id]
	revoked := s.revoked[id]
	s.mu.RUnlock()
	if live {
		t.Fatalf("deleted credential stayed live")
	}
	if !revoked {
		t.Fatalf("deleted credential was not revoked")
	}
	// Nothing may be advertised once the last credential is gone.
	if models := s.modelRegistration().(map[string]any)["Models"].([]map[string]any); len(models) != 0 {
		t.Fatalf("models still registered after delete: %v", models)
	}
}

// A disable that the host rejects must abort the delete and leave the key intact.
func TestDeleteAbortsWhenHostDisableFails(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-abort-0013")
	body, _ := json.Marshal(Credential{Type: Provider, ID: id, Label: "primary", APIKey: "sk-amr-abort-0013"})
	path := host.writeAuthFile(t, id+".json", body)
	host.mu.Lock()
	host.saveAuthErr = errString("auth dir is read-only")
	host.mu.Unlock()

	status, _, raw := managementCall(t, s, http.MethodDelete, apiBase+"/credentials", map[string][]string{"id": {id}}, nil)
	if status != http.StatusBadGateway {
		t.Fatalf("delete status = %d, want 502: %s", status, raw)
	}
	if strings.Contains(raw, "sk-amr-abort-0013") {
		t.Fatalf("delete error leaked the key: %s", raw)
	}
	// The credential must still be usable and the file untouched.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("failed delete removed the file: %v", err)
	}
	s.mu.RLock()
	c, live := s.creds[id]
	revoked := s.revoked[id]
	s.mu.RUnlock()
	if !live || c.APIKey != "sk-amr-abort-0013" || revoked {
		t.Fatalf("failed delete mutated state: live=%v revoked=%v key=%q", live, revoked, c.APIKey)
	}
}

// An unlink failure must be reported as such, not as success.
func TestDeleteReportsUnlinkFailure(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-unlink-0014")
	body, _ := json.Marshal(Credential{Type: Provider, ID: id, Label: "primary", APIKey: "sk-amr-unlink-0014"})
	host.writeAuthFile(t, id+".json", body)
	// Read still succeeds (ownership check passes) but unlink cannot: removing an
	// entry needs write permission on the directory.
	dir := host.authDir
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("setup chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	status, _, raw := managementCall(t, s, http.MethodDelete, apiBase+"/credentials", map[string][]string{"id": {id}}, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("unlink failure status = %d, want 500, body %s", status, raw)
	}
	if !strings.Contains(raw, "credential removal failed") {
		t.Fatalf("unlink failure did not report the real error: %s", raw)
	}
	s.mu.RLock()
	_, revoked := s.revoked[id]
	s.mu.RUnlock()
	if revoked {
		t.Fatalf("failed unlink revoked the credential")
	}
}

// A watcher callback queued before the unlink must not bring the credential back.
func TestDeletedCredentialIsNotResurrectedByWatcher(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-revive-0015")
	body, _ := json.Marshal(Credential{Type: Provider, ID: id, Label: "primary", APIKey: "sk-amr-revive-0015"})
	host.writeAuthFile(t, id+".json", body)
	if status, _, raw := managementCall(t, s, http.MethodDelete, apiBase+"/credentials", map[string][]string{"id": {id}}, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %s", status, raw)
	}

	// The stale payload arrives after the delete landed; disk no longer has the file.
	out, err := s.parseAuth(mustJSON(t, map[string]any{
		"Provider": Provider,
		"FileName": id + ".json",
		"RawJSON":  body,
		"Host":     map[string]any{"AuthDir": host.authDir},
	}))
	if err != nil {
		t.Fatalf("stale parse: %v", err)
	}
	if handled, _ := out.(map[string]any)["Handled"].(bool); handled {
		t.Fatalf("stale watcher callback resurrected the deleted credential")
	}
	s.mu.RLock()
	_, live := s.creds[id]
	s.mu.RUnlock()
	if live {
		t.Fatalf("deleted credential is live again")
	}
	// Re-creating the same file is an operator action and must still work.
	host.writeAuthFile(t, id+".json", body)
	out, err = s.parseAuth(mustJSON(t, map[string]any{
		"Provider": Provider,
		"FileName": id + ".json",
		"RawJSON":  body,
		"Host":     map[string]any{"AuthDir": host.authDir},
	}))
	if err != nil {
		t.Fatalf("parse after re-create: %v", err)
	}
	if handled, _ := out.(map[string]any)["Handled"].(bool); !handled {
		t.Fatalf("re-created credential was not accepted")
	}
}

// Models are only advertised while at least one live credential can serve them.
func TestModelRegistrationIsEmptyWithoutCredentials(t *testing.T) {
	s, host := newTestService(t)
	if models := s.modelRegistration().(map[string]any)["Models"].([]map[string]any); len(models) != 0 {
		t.Fatalf("fresh plugin advertised models: %v", models)
	}
	addCredential(t, s, host, "primary", "sk-amr-reg-0016")
	seedModels(t, s, host)
	if models := s.modelRegistration().(map[string]any)["Models"].([]map[string]any); len(models) == 0 {
		t.Fatalf("models missing although a credential exists")
	}
}
