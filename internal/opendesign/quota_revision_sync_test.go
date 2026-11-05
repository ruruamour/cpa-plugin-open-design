package opendesign

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Keep worker scheduling out of these file-ordering tests: every sweep below is
// explicit, while registration, pause, resume and parsing use the normal paths.
func quotaRevisionTestService(t *testing.T) (*Service, *fakeHost) {
	t.Helper()
	host := newFakeHost()
	dir := t.TempDir()
	host.authDir = filepath.Join(dir, "auth")
	s := NewService()
	s.SetHost(host.call)
	s.quotaPauseStarted = true
	registrar(t, s, dir)
	enabled := true
	s.mu.Lock()
	s.cfg.Pause.Enabled = &enabled
	s.mu.Unlock()
	t.Cleanup(func() { _, _ = s.quiesce() })
	return s, host
}

func quotaRevisionRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func quotaRevisionDeliver(t *testing.T, s *Service, host *fakeHost, id, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.parseAuth(jsonBytes(map[string]any{
		"FileName": id + ".json", "RawJSON": body, "Host": map[string]any{"AuthDir": host.authDir},
	})); err != nil {
		t.Fatal(err)
	}
}

func quotaRevisionMarker(s *Service, id string) (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.quotaDiskStale[id], s.quotaPauseErrors[id]
}

func quotaRevisionPauseResume(t *testing.T) (*Service, *fakeHost, string, string, []byte, []byte, Credential) {
	t.Helper()
	s, host := quotaRevisionTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, map[string]any{"opaque_extension": "keep"})
	before := quotaRevisionRead(t, path)
	var beforeCred Credential
	if err := json.Unmarshal(before, &beforeCred); err != nil || beforeCred.QuotaRevision != "" || beforeCred.QuotaBlock != nil {
		t.Fatalf("pre-pause fixture is not unversioned and clear: %v", err)
	}
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "model-a", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	paused := quotaRevisionRead(t, path)
	if status, _, body := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/resume", nil,
		map[string]any{"credential_id": id}); status != http.StatusOK {
		t.Fatalf("resume: %d %s", status, body)
	}
	live, _ := liveCredential(s, id)
	if live.quotaBlocked() || live.QuotaRevision == "" {
		t.Fatal("fixture did not publish a versioned cleared state")
	}
	return s, host, id, path, before, paused, live
}

func quotaRevisionAssertDiskClear(t *testing.T, s *Service, id, path string) {
	t.Helper()
	body := quotaRevisionRead(t, path)
	var disk Credential
	if err := json.Unmarshal(body, &disk); err != nil {
		t.Fatal(err)
	}
	live, _ := liveCredential(s, id)
	if disk.QuotaBlock != nil || live.QuotaBlock != nil || disk.QuotaRevision == "" || disk.QuotaRevision != live.QuotaRevision {
		t.Fatalf("clear states did not converge: disk block=%v rev=%q live block=%v rev=%q", disk.QuotaBlock, disk.QuotaRevision, live.QuotaBlock, live.QuotaRevision)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || string(fields["opaque_extension"]) != `"keep"` {
		t.Fatal("repair lost an unrelated extension")
	}
	if marker, pending := quotaRevisionMarker(s, id); marker || pending != "" {
		t.Fatalf("clear repair left marker=%v pending=%q", marker, pending)
	}
}

func TestQuotaRevisionPrePauseThenOldPausedCannotResurrect(t *testing.T) {
	s, host, id, path, before, paused, resumed := quotaRevisionPauseResume(t)
	quotaRevisionDeliver(t, s, host, id, path, before)
	live, _ := liveCredential(s, id)
	if live.QuotaRevision != resumed.QuotaRevision || live.quotaBlocked() {
		t.Fatalf("unversioned body erased the cleared revision: got %q want %q", live.QuotaRevision, resumed.QuotaRevision)
	}
	quotaRevisionDeliver(t, s, host, id, path, paused)
	live, _ = liveCredential(s, id)
	if live.quotaBlocked() {
		t.Fatal("old paused body resurrected a cleared pause")
	}
	if marker, _ := quotaRevisionMarker(s, id); !marker {
		t.Fatal("stale paused file was not marked for repair")
	}
	s.sweepQuotaBlocks(make(chan struct{}))
	quotaRevisionAssertDiskClear(t, s, id, path)

	// A fresh plugin instance can only know what has actually reached disk.
	restarted := NewService()
	if _, err := restarted.parseAuth(jsonBytes(map[string]any{"FileName": id + ".json", "RawJSON": quotaRevisionRead(t, path), "Host": map[string]any{"AuthDir": host.authDir}})); err != nil {
		t.Fatal(err)
	}
	if c, ok := liveCredential(restarted, id); !ok || c.quotaBlocked() || c.QuotaRevision == "" {
		t.Fatal("restart lost the repaired cleared state")
	}
}

func TestQuotaRevisionMarkerThenUnversionedClearRepairs(t *testing.T) {
	s, host, id, path, before, paused, resumed := quotaRevisionPauseResume(t)
	quotaRevisionDeliver(t, s, host, id, path, paused)
	if marker, _ := quotaRevisionMarker(s, id); !marker {
		t.Fatal("stale paused body did not arm marker")
	}
	quotaRevisionDeliver(t, s, host, id, path, before)
	live, _ := liveCredential(s, id)
	if live.QuotaRevision != resumed.QuotaRevision {
		t.Fatalf("marker lost its cleared revision: got %q want %q", live.QuotaRevision, resumed.QuotaRevision)
	}
	s.sweepQuotaBlocks(make(chan struct{}))
	quotaRevisionAssertDiskClear(t, s, id, path)
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if status, _, _ := managementCall(t, s, http.MethodGet, apiBase+"/quota-pause", nil, nil); status != http.StatusOK {
			t.Fatal("quota read failed")
		}
	}
	last, err := os.Stat(path)
	if err != nil || !os.SameFile(first, last) {
		t.Fatalf("already repaired file was replaced on read: %v", err)
	}
}

func TestQuotaRevisionNewPauseRetiresStaleClearMarker(t *testing.T) {
	s, host, id, path, _, paused, _ := quotaRevisionPauseResume(t)
	quotaRevisionDeliver(t, s, host, id, path, paused)
	if marker, _ := quotaRevisionMarker(s, id); !marker {
		t.Fatal("fixture did not arm marker")
	}
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "model-b", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	live, _ := liveCredential(s, id)
	if !live.quotaBlocked() || live.QuotaBlock.ModelID != "model-b" {
		t.Fatal("fresh upstream signal did not create a new pause")
	}
	if marker, pending := quotaRevisionMarker(s, id); marker || pending != "" {
		t.Fatalf("successful new pause left marker=%v pending=%q", marker, pending)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		body := quotaRevisionRead(t, path)
		if _, err := s.parseAuth(jsonBytes(map[string]any{"FileName": id + ".json", "RawJSON": body, "Host": map[string]any{"AuthDir": host.authDir}})); err != nil {
			t.Fatal(err)
		}
		if status, _, _ := managementCall(t, s, http.MethodGet, apiBase+"/quota-pause", nil, nil); status != http.StatusOK {
			t.Fatal("quota read failed")
		}
	}
	last, err := os.Stat(path)
	if err != nil || !os.SameFile(first, last) {
		t.Fatalf("settled new pause rewrote auth file on read: %v", err)
	}
}

func TestQuotaRevisionRestartUnversionedAdoptsAndResumesPause(t *testing.T) {
	for _, tc := range []struct {
		name       string
		revision   string
		manual     *bool
		disabled   bool
		wantManual bool
	}{
		{"legacy enabled", "", boolPointer(false), false, false},
		{"versioned enabled", "incoming", boolPointer(false), false, false},
		{"unknown manual", "", nil, false, true},
		{"previously disabled", "incoming", boolPointer(false), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, host := quotaRevisionTestService(t)
			id := addCredential(t, s, host, "fixture", "fixture-inference-key")
			path := quotaFixtureAuthFile(t, s, host, id, map[string]any{"disabled": tc.disabled})
			// Start a new plugin instance: its first disk body contains neither a
			// pause nor a revision, so it has no evidence of a prior resume.
			s = NewService()
			s.SetHost(host.call)
			s.quotaPauseStarted = true
			registrar(t, s, filepath.Dir(host.authDir))
			t.Cleanup(func() { _, _ = s.quiesce() })
			body := quotaRevisionRead(t, path)
			if _, err := s.parseAuth(jsonBytes(map[string]any{"FileName": id + ".json", "RawJSON": body, "Host": map[string]any{"AuthDir": host.authDir}})); err != nil {
				t.Fatal(err)
			}
			current, _ := liveCredential(s, id)
			if current.QuotaRevision != "" || current.quotaBlocked() {
				t.Fatal("restart fixture must remain truly unversioned")
			}
			now := time.Now().UTC()
			incoming := current
			incoming.Disabled = false
			incoming.QuotaRevision = tc.revision
			incoming.QuotaBlock = &QuotaBlock{KeyHash: quotaPauseKeyHash(current.APIKey), ManualDisabled: tc.manual,
				Source: "upstream", Reason: "quota_exhausted", BlockedAt: now, ObservedAt: now,
				RetryAt: now.Add(time.Hour), ModelID: "model-a", ModelIDs: []string{"model-a"}}
			quotaRevisionDeliver(t, s, host, id, path, jsonBytes(incoming))
			live, _ := liveCredential(s, id)
			if !live.quotaBlocked() || live.Disabled != tc.wantManual || live.QuotaBlock.ManualDisabled == nil || *live.QuotaBlock.ManualDisabled != tc.wantManual {
				t.Fatal("unversioned live did not conservatively adopt the valid pause")
			}
			s.sweepQuotaBlocks(make(chan struct{}))
			if status, _, body := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/resume", nil, map[string]any{"credential_id": id}); status != http.StatusOK {
				t.Fatalf("adopted pause could not be explicitly resumed: %d %s", status, body)
			}
			live, _ = liveCredential(s, id)
			if live.quotaBlocked() || live.Disabled != tc.wantManual {
				t.Fatal("resume failed to retain the conservative manual state")
			}
		})
	}
}

func TestQuotaRevisionNewKeyDoesNotInheritClearFence(t *testing.T) {
	s, host, id, path, _, paused, _ := quotaRevisionPauseResume(t)
	quotaRevisionDeliver(t, s, host, id, path, paused)
	old, _ := liveCredential(s, id)
	newKey := old
	newKey.APIKey = "another-inference-key"
	newKey.QuotaBlock = nil
	newKey.QuotaRevision = ""
	quotaRevisionDeliver(t, s, host, id, path, jsonBytes(newKey))
	live, _ := liveCredential(s, id)
	if live.APIKey != newKey.APIKey || live.QuotaRevision != "" || live.quotaBlocked() {
		t.Fatal("new key inherited old key's quota state")
	}
	if marker, pending := quotaRevisionMarker(s, id); marker || pending != "" {
		t.Fatalf("new key inherited marker=%v pending=%q", marker, pending)
	}
	var oldPaused Credential
	if err := json.Unmarshal(paused, &oldPaused); err != nil {
		t.Fatal(err)
	}
	foreign := newKey
	foreign.QuotaBlock = oldPaused.QuotaBlock // fingerprint belongs to the previous key
	quotaRevisionDeliver(t, s, host, id, path, jsonBytes(foreign))
	live, _ = liveCredential(s, id)
	if live.quotaBlocked() {
		t.Fatal("a block belonging to the old key paused the new key")
	}
	now := time.Now().UTC()
	newKey.QuotaBlock = &QuotaBlock{KeyHash: quotaPauseKeyHash(newKey.APIKey), ManualDisabled: boolPointer(false),
		Source: "upstream", Reason: "quota_exhausted", BlockedAt: now, ObservedAt: now,
		RetryAt: now.Add(time.Hour), ModelID: "model-b", ModelIDs: []string{"model-b"}}
	quotaRevisionDeliver(t, s, host, id, path, jsonBytes(newKey))
	live, _ = liveCredential(s, id)
	if !live.quotaBlocked() || live.QuotaBlock.KeyHash != quotaPauseKeyHash(newKey.APIKey) {
		t.Fatal("valid pause for new unversioned key was discarded")
	}
}

func TestQuotaRevisionMarkerDoesNotOutliveCredentialIdentity(t *testing.T) {
	t.Run("management key replacement", func(t *testing.T) {
		s, host, id, path, _, paused, _ := quotaRevisionPauseResume(t)
		quotaRevisionDeliver(t, s, host, id, path, paused)
		// Unlike the default fake, make this host save actually replace the file
		// before reporting success, as the real host does.
		host.mu.Lock()
		host.saveHook = func(_ string, raw []byte) {
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
		host.mu.Unlock()
		if status, _, body := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
			map[string]any{"id": id, "api_key": "another-inference-key"}); status != http.StatusOK {
			t.Fatalf("key replacement: %d %s", status, body)
		}
		live, _ := liveCredential(s, id)
		if live.APIKey != "another-inference-key" || live.quotaBlocked() {
			t.Fatal("key replacement did not publish the new identity")
		}
		if marker, pending := quotaRevisionMarker(s, id); marker || pending != "" {
			t.Fatalf("new key inherited old retry state: marker=%v pending=%q", marker, pending)
		}
	})

	t.Run("failed key replacement keeps old identity", func(t *testing.T) {
		s, host, id, path, _, paused, _ := quotaRevisionPauseResume(t)
		quotaRevisionDeliver(t, s, host, id, path, paused)
		old, _ := liveCredential(s, id)
		host.mu.Lock()
		host.saveAuthErr = errors.New("fixture host failure")
		host.mu.Unlock()
		if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
			map[string]any{"id": id, "api_key": "another-inference-key"}); status != http.StatusInternalServerError {
			t.Fatalf("failed key replacement returned %d", status)
		}
		live, _ := liveCredential(s, id)
		marker, _ := quotaRevisionMarker(s, id)
		if live.APIKey != old.APIKey || live.QuotaRevision != old.QuotaRevision || !marker {
			t.Fatal("failed save changed the old key, revision or repair marker")
		}
	})

	for _, unknownPause := range []bool{false, true} {
		name := "delete and reimport unversioned credential"
		if unknownPause {
			name = "delete and reimport pause with unknown manual state"
		}
		t.Run(name, func(t *testing.T) {
			s, host, id, path, _, paused, _ := quotaRevisionPauseResume(t)
			quotaRevisionDeliver(t, s, host, id, path, paused)
			if status, _, body := managementCall(t, s, http.MethodDelete, apiBase+"/credentials", map[string][]string{"id": {id}}, nil); status != http.StatusOK {
				t.Fatalf("delete: %d %s", status, body)
			}
			fresh := Credential{Type: Provider, ID: id, APIKey: "another-inference-key"}
			if unknownPause {
				now := time.Now().UTC()
				fresh.QuotaBlock = &QuotaBlock{KeyHash: quotaPauseKeyHash(fresh.APIKey), Source: "upstream", Reason: "quota_exhausted",
					BlockedAt: now, ObservedAt: now, RetryAt: now.Add(time.Hour), ModelID: "model-a", ModelIDs: []string{"model-a"}}
			}
			quotaRevisionDeliver(t, s, host, id, path, jsonBytes(fresh))
			live, _ := liveCredential(s, id)
			marker, pending := quotaRevisionMarker(s, id)
			if live.APIKey != fresh.APIKey || marker {
				t.Fatal("reimport inherited the deleted credential's identity or marker")
			}
			if unknownPause {
				if !live.quotaBlocked() || !live.Disabled || pending == "" {
					t.Fatal("new unknown pause lost its own conservative sync error")
				}
				s.sweepQuotaBlocks(make(chan struct{}))
				if _, pending = quotaRevisionMarker(s, id); pending != "" {
					t.Fatal("new unknown pause could not be persisted")
				}
			} else if live.QuotaRevision != "" || live.quotaBlocked() || pending != "" {
				t.Fatal("new unversioned credential inherited quota state")
			}
		})
	}
}

func TestQuotaRevisionRestartDirectlyLoadsLegacyPausedFile(t *testing.T) {
	for _, manual := range []*bool{boolPointer(false), nil} {
		name := "known enabled"
		if manual == nil {
			name = "unknown manual state"
		}
		t.Run(name, func(t *testing.T) {
			original, host := quotaRevisionTestService(t)
			id := addCredential(t, original, host, "fixture", "fixture-inference-key")
			now := time.Now().UTC()
			block := &QuotaBlock{KeyHash: quotaPauseKeyHash("fixture-inference-key"), ManualDisabled: manual,
				Source: "upstream", Reason: "quota_exhausted", BlockedAt: now, ObservedAt: now,
				RetryAt: now.Add(time.Hour), ModelID: "model-a", ModelIDs: []string{"model-a"}}
			path := quotaFixtureAuthFile(t, original, host, id, map[string]any{"quota_block": block})
			raw := quotaRevisionRead(t, path)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil || fields["quota_revision"] != nil {
				t.Fatal("legacy disk fixture unexpectedly has a revision")
			}
			s := NewService()
			s.SetHost(host.call)
			s.quotaPauseStarted = true
			registrar(t, s, filepath.Dir(host.authDir))
			t.Cleanup(func() { _, _ = s.quiesce() })
			if _, err := s.parseAuth(jsonBytes(map[string]any{"FileName": id + ".json", "RawJSON": raw, "Host": map[string]any{"AuthDir": host.authDir}})); err != nil {
				t.Fatal(err)
			}
			live, _ := liveCredential(s, id)
			wantDisabled := manual == nil
			if !live.quotaBlocked() || live.Disabled != wantDisabled || live.QuotaBlock.ManualDisabled == nil || *live.QuotaBlock.ManualDisabled != wantDisabled {
				t.Fatal("legacy on-disk pause or conservative manual state was lost")
			}
			s.sweepQuotaBlocks(make(chan struct{}))
			if status, _, body := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/resume", nil,
				map[string]any{"credential_id": id}); status != http.StatusOK {
				t.Fatalf("resume legacy disk pause: %d %s", status, body)
			}
			live, _ = liveCredential(s, id)
			if live.quotaBlocked() || live.Disabled != wantDisabled || live.QuotaRevision == "" {
				t.Fatal("legacy pause did not resume with the conservative manual state")
			}
		})
	}
}

func TestQuotaRevisionUnversionedClearPreservesManualDisable(t *testing.T) {
	t.Run("live disable survives old enable", func(t *testing.T) {
		s, host, id, path, before, _, _ := quotaRevisionPauseResume(t)
		if status, _, body := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
			map[string]any{"id": id, "disabled": true}); status != http.StatusOK {
			t.Fatalf("disable: %d %s", status, body)
		}
		mirrorAuthFile(t, s, host, id)
		quotaRevisionDeliver(t, s, host, id, path, before)
		if live, _ := liveCredential(s, id); !live.Disabled {
			t.Fatal("unversioned old enable undid a manual disable")
		}
		s.sweepQuotaBlocks(make(chan struct{}))
		quotaRevisionAssertDiskClear(t, s, id, path)
		var disk Credential
		if err := json.Unmarshal(quotaRevisionRead(t, path), &disk); err != nil || !disk.Disabled {
			t.Fatal("repair did not preserve the manual disable")
		}
	})

	t.Run("disk disable discovered at repair", func(t *testing.T) {
		s, host, id, path, before, _, _ := quotaRevisionPauseResume(t)
		quotaRevisionDeliver(t, s, host, id, path, before)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(before, &fields); err != nil {
			t.Fatal(err)
		}
		fields["disabled"] = jsonBytes(true)
		if err := os.WriteFile(path, jsonBytes(fields), 0600); err != nil {
			t.Fatal(err)
		}
		old, _ := liveCredential(s, id)
		s.sweepQuotaBlocks(make(chan struct{}))
		live, _ := liveCredential(s, id)
		if !live.Disabled || live.runtimeEpoch == old.runtimeEpoch {
			t.Fatal("repair did not publish and fence the disk-only manual disable")
		}
		quotaRevisionAssertDiskClear(t, s, id, path)
		var disk Credential
		if err := json.Unmarshal(quotaRevisionRead(t, path), &disk); err != nil || !disk.Disabled {
			t.Fatal("repair overwrote the disk-only manual disable")
		}
	})
}

func TestQuotaRevisionNewPauseOnStaleDiskKeepsManualDisable(t *testing.T) {
	s, host, id, path, _, paused, _ := quotaRevisionPauseResume(t)
	if status, _, body := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
		map[string]any{"id": id, "disabled": true}); status != http.StatusOK {
		t.Fatalf("disable: %d %s", status, body)
	}
	mirrorAuthFile(t, s, host, id)
	quotaRevisionDeliver(t, s, host, id, path, paused) // old manual choice was enabled
	if marker, _ := quotaRevisionMarker(s, id); !marker {
		t.Fatal("fixture did not mark the old pause stale")
	}
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "model-b", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	live, _ := liveCredential(s, id)
	if !live.quotaBlocked() || !live.Disabled || live.QuotaBlock.ManualDisabled == nil || !*live.QuotaBlock.ManualDisabled {
		t.Fatal("new pause trusted the stale file and undid a manual disable")
	}
	if marker, pending := quotaRevisionMarker(s, id); marker || pending != "" {
		t.Fatalf("new pause did not settle: marker=%v pending=%q", marker, pending)
	}
	if status, _, body := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/resume", nil,
		map[string]any{"credential_id": id}); status != http.StatusOK {
		t.Fatalf("resume: %d %s", status, body)
	}
	live, _ = liveCredential(s, id)
	if live.quotaBlocked() || !live.Disabled {
		t.Fatal("resuming the new pause lost the manual disable")
	}
}

func TestQuotaRevisionNewPauseFailedWriteRetries(t *testing.T) {
	s, host, id, path, _, paused, _ := quotaRevisionPauseResume(t)
	quotaRevisionDeliver(t, s, host, id, path, paused)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "model-b", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	if marker, pending := quotaRevisionMarker(s, id); !marker || pending == "" {
		t.Fatalf("failed write lost retry state: marker=%v pending=%q", marker, pending)
	}
	if err := os.WriteFile(path, paused, 0600); err != nil {
		t.Fatal(err)
	}
	// A watcher can see the old body while the new block is waiting for its
	// retry; it must not retire the old-clear marker before the file converges.
	if _, err := s.parseAuth(jsonBytes(map[string]any{"FileName": id + ".json", "RawJSON": paused, "Host": map[string]any{"AuthDir": host.authDir}})); err != nil {
		t.Fatal(err)
	}
	if marker, pending := quotaRevisionMarker(s, id); !marker || pending == "" {
		t.Fatalf("divergent watcher lost retry state: marker=%v pending=%q", marker, pending)
	}
	s.sweepQuotaBlocks(make(chan struct{}))
	live, _ := liveCredential(s, id)
	var disk Credential
	if err := json.Unmarshal(quotaRevisionRead(t, path), &disk); err != nil || !disk.quotaBlocked() || disk.QuotaRevision != live.QuotaRevision || disk.QuotaBlock.ModelID != "model-b" {
		t.Fatal("retry did not persist the new pause")
	}
	if marker, pending := quotaRevisionMarker(s, id); marker || pending != "" {
		t.Fatalf("successful retry left marker=%v pending=%q", marker, pending)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s.sweepQuotaBlocks(make(chan struct{}))
	last, err := os.Stat(path)
	if err != nil || !os.SameFile(first, last) {
		t.Fatalf("successful retry was repeated: %v", err)
	}
}

// A delayed host write can land after the proof starts but before its commit,
// without a watcher callback. The old paused block is valid and the optional
// revision is absent, so the on-disk identity and sync fences both allow it.
func TestQuotaRecoveryStaleUnversionedPauseKeepsManualDisable(t *testing.T) {
	s, host, id, path, advance := quotaCommitFixture(t, "model-a")
	var stale map[string]json.RawMessage
	if err := json.Unmarshal(quotaRevisionRead(t, path), &stale); err != nil {
		t.Fatal(err)
	}
	delete(stale, "quota_revision")
	if status, _, body := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
		map[string]any{"id": id, "disabled": true}); status != http.StatusOK {
		t.Fatalf("manual disable: %d %s", status, body)
	}
	mirrorAuthFile(t, s, host, id)
	live, _ := liveCredential(s, id)
	if !live.quotaBlocked() || !live.Disabled || live.QuotaBlock.ManualDisabled == nil || !*live.QuotaBlock.ManualDisabled {
		t.Fatal("fixture did not publish a paused manual disable")
	}
	// Make the block due for the real automatic path, while preventing the
	// fixture's background worker from racing this explicitly driven round.
	s.mu.Lock()
	s.quotaRecheckRetryAt[id] = s.quotaNow().Add(3 * time.Hour)
	s.mu.Unlock()
	advance(2 * time.Hour)
	started, release := parkQuotaHTTP(t, s, host, advance, "model-a", "member", "available")
	defer release()
	result := make(chan error, 1)
	go func() { result <- s.recheckQuotaBlockMode("callback", id, true) }()
	awaitParked(t, started)
	// This write intentionally bypasses parseAuth: the watcher can be delayed.
	if err := os.WriteFile(path, jsonBytes(stale), 0600); err != nil {
		t.Fatal(err)
	}
	release()
	if err := waitRecheck(t, result); err != nil {
		t.Fatalf("valid proof with an optional missing disk revision was rejected: %v", err)
	}
	live, _ = liveCredential(s, id)
	var disk Credential
	if err := json.Unmarshal(quotaRevisionRead(t, path), &disk); err != nil {
		t.Fatal(err)
	}
	if live.quotaBlocked() || disk.quotaBlocked() || !live.Disabled || !disk.Disabled {
		t.Fatalf("recovery lost manual disable: live disabled=%v blocked=%v disk disabled=%v blocked=%v", live.Disabled, live.quotaBlocked(), disk.Disabled, disk.quotaBlocked())
	}
}

func TestQuotaRevisionClearRepairRefusesUnownedDisk(t *testing.T) {
	for _, tc := range []string{"different key", "foreign block", "conflicting clear revision"} {
		t.Run(tc, func(t *testing.T) {
			s, host, id, path, _, paused, _ := quotaRevisionPauseResume(t)
			quotaRevisionDeliver(t, s, host, id, path, paused)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(paused, &fields); err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "different key":
				fields["api_key"] = jsonBytes("another-inference-key")
			case "foreign block":
				var block QuotaBlock
				if err := json.Unmarshal(fields["quota_block"], &block); err != nil {
					t.Fatal(err)
				}
				block.KeyHash = quotaPauseKeyHash("another-inference-key")
				fields["quota_block"] = jsonBytes(block)
			case "conflicting clear revision":
				delete(fields, "quota_block")
				fields["quota_revision"] = jsonBytes("other-revision")
			}
			body := jsonBytes(fields)
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			first, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			s.sweepQuotaBlocks(make(chan struct{}))
			last, err := os.Stat(path)
			if err != nil || !os.SameFile(first, last) || string(quotaRevisionRead(t, path)) != string(body) {
				t.Fatal("repair overwrote an unowned or conflicting disk state")
			}
			if marker, _ := quotaRevisionMarker(s, id); !marker {
				t.Fatal("failed repair falsely retired the marker")
			}
			// A later valid stale body can still be repaired; failures do not
			// erase evidence or silently change an unrelated credential.
			if err := os.WriteFile(path, paused, 0600); err != nil {
				t.Fatal(err)
			}
			s.sweepQuotaBlocks(make(chan struct{}))
			quotaRevisionAssertDiskClear(t, s, id, path)
		})
	}
}
