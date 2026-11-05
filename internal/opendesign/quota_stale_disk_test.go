package opendesign

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// A stale host rewrite of a pre-pause body must not drop the plugin console's
// disable choice, and the worker must repair the file from live state.
func TestStaleHostWriteKeepsPluginManualDisable(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, map[string]any{"disabled": false})
	staleBody, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "model-a", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	if c, _ := liveCredential(s, id); !c.quotaBlocked() {
		t.Fatal("fixture did not enter quota pause")
	}

	// The operator disables the account in the plugin console.
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
		map[string]any{"id": id, "disabled": true}); status != http.StatusOK {
		t.Fatal("plugin manual disable failed")
	}
	// CPA then rewrites the pre-pause body, as it does when it persists its own
	// runtime snapshot.
	s.credentialMu.Lock()
	if err := os.WriteFile(path, staleBody, 0o600); err != nil {
		s.credentialMu.Unlock()
		t.Fatal(err)
	}
	_, err = s.parseAuth(jsonBytes(map[string]any{
		"FileName": id + ".json", "RawBody": staleBody, "RawJSON": staleBody,
		"Host": map[string]any{"AuthDir": host.authDir},
	}))
	s.credentialMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	current, _ := liveCredential(s, id)
	if !current.Disabled || !current.quotaBlocked() || current.QuotaBlock.ManualDisabled == nil || !*current.QuotaBlock.ManualDisabled {
		t.Fatalf("stale host write dropped the plugin manual disable: disabled=%v block=%v", current.Disabled, current.QuotaBlock)
	}
	s.sweepQuotaBlocks(make(chan struct{}))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored Credential
	if err := json.Unmarshal(raw, &stored); err != nil || !stored.Disabled || !stored.quotaBlocked() {
		t.Fatalf("repair did not restore the paused+disabled file: %v disabled=%v", err, stored.Disabled)
	}
	// The explicit resume keeps the plugin-owned disable choice.
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/resume", nil,
		map[string]any{"credential_id": id}); status != http.StatusOK {
		t.Fatal("explicit resume failed")
	}
	final, _ := liveCredential(s, id)
	if final.quotaBlocked() || !final.Disabled {
		t.Fatalf("resume lost the plugin manual disable: blocked=%v disabled=%v", final.quotaBlocked(), final.Disabled)
	}
}

// A stale paused body delivered after an explicit resume must not be persisted,
// otherwise the pause comes back on the next restart.
func TestStalePausedWriteAfterResumeDoesNotResurrect(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, nil)
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "model-a", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	pausedBody, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(pausedBody) {
		t.Fatal("fixture block was not persisted")
	}
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/resume", nil,
		map[string]any{"credential_id": id}); status != http.StatusOK {
		t.Fatal("explicit resume failed")
	}
	if c, _ := liveCredential(s, id); c.quotaBlocked() {
		t.Fatal("resume did not clear the live block")
	}

	// Host rewrites the old paused body.
	s.credentialMu.Lock()
	if err := os.WriteFile(path, pausedBody, 0o600); err != nil {
		s.credentialMu.Unlock()
		t.Fatal(err)
	}
	_, err = s.parseAuth(jsonBytes(map[string]any{
		"FileName": id + ".json", "RawBody": pausedBody, "RawJSON": pausedBody,
		"Host": map[string]any{"AuthDir": host.authDir},
	}))
	s.credentialMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := liveCredential(s, id); c.quotaBlocked() {
		t.Fatal("a stale paused body re-blocked the live credential")
	}
	s.sweepQuotaBlocks(make(chan struct{}))
	s.mu.RLock()
	pendingRepair := s.quotaDiskStale[id]
	s.mu.RUnlock()
	if pendingRepair {
		t.Fatal("successful repair left a stale marker and would be retried forever")
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s.sweepQuotaBlocks(make(chan struct{}))
	second, err := os.Stat(path)
	if err != nil || !os.SameFile(first, second) {
		t.Fatalf("a subsequent sweep rewrote the already repaired file: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored Credential
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.quotaBlocked() {
		t.Fatal("the stale paused body was left on disk and would return after a restart")
	}
	// A reload from disk must stay resumed.
	reloaded := NewService()
	reloaded.SetHost(host.call)
	if _, err := reloaded.parseAuth(jsonBytes(map[string]any{
		"FileName": id + ".json", "RawBody": raw, "RawJSON": raw,
		"Host": map[string]any{"AuthDir": host.authDir},
	})); err != nil {
		t.Fatal(err)
	}
	if c, ok := liveCredential(reloaded, id); ok && c.quotaBlocked() {
		t.Fatal("restart resurrected the cleared pause")
	}
}

// A newer host write may already have repaired the stale body before the worker
// reaches it. A matching cleared file must retire the marker without a rewrite.
func TestAlreadyRepairedDiskRetiresStaleMarker(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, nil)
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "model-a", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	paused, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/resume", nil,
		map[string]any{"credential_id": id}); status != http.StatusOK {
		t.Fatal("explicit resume failed")
	}
	cleared, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s.credentialMu.Lock()
	if err := os.WriteFile(path, paused, 0600); err != nil {
		s.credentialMu.Unlock()
		t.Fatal(err)
	}
	_, err = s.parseAuth(jsonBytes(map[string]any{
		"FileName": id + ".json", "RawJSON": paused,
		"Host": map[string]any{"AuthDir": host.authDir},
	}))
	if err == nil {
		err = os.WriteFile(path, cleared, 0600)
	}
	s.credentialMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s.sweepQuotaBlocks(make(chan struct{}))
	s.mu.RLock()
	pending := s.quotaDiskStale[id]
	s.mu.RUnlock()
	after, err := os.Stat(path)
	if err != nil || pending || !os.SameFile(before, after) {
		t.Fatalf("already repaired file left stale marker=%v or was rewritten: %v", pending, err)
	}
}

// A live block that never reached disk (persistence failed, or the file was
// removed) must still be resumable from the plugin console.
func TestManualResumeWorksWhenDiskBlockIsMissing(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, nil)
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "model-a", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	if c, _ := liveCredential(s, id); !c.quotaBlocked() {
		t.Fatal("fixture did not enter quota pause")
	}
	// Drop the block from disk without telling the plugin, as a host rewrite of an
	// older body does.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "quota_block")
	delete(fields, "quota_revision")
	trimmed, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, trimmed, 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _ := liveCredential(s, id); !c.quotaBlocked() {
		t.Fatal("divergence should not clear the live block by itself")
	}
	status, body, _ := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/resume", nil,
		map[string]any{"credential_id": id})
	if status != http.StatusOK {
		t.Fatalf("resume could not clear a block that exists only in memory: %d %v", status, body)
	}
	if c, _ := liveCredential(s, id); c.quotaBlocked() {
		t.Fatal("resume left the live block in place")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored Credential
	if err := json.Unmarshal(after, &stored); err != nil || stored.quotaBlocked() {
		t.Fatal("resume did not repair the disk state")
	}
}

// A legacy block without a revision string must survive a stale host rewrite.
func TestLegacyBlockWithoutRevisionSurvivesStaleWrite(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, nil)
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "model-a", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	current, _ := liveCredential(s, id)
	// Simulate a legacy in-memory record: block present, no revision string.
	s.mu.Lock()
	legacy := s.creds[id]
	legacy.QuotaRevision = ""
	s.creds[id] = legacy
	s.mu.Unlock()
	stale := jsonBytes(Credential{Type: Provider, ID: id, Label: "fixture", APIKey: current.APIKey})
	if err := os.WriteFile(path, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.parseAuth(jsonBytes(map[string]any{
		"FileName": id + ".json", "RawBody": stale, "RawJSON": stale,
		"Host": map[string]any{"AuthDir": host.authDir},
	})); err != nil {
		t.Fatal(err)
	}
	after, _ := liveCredential(s, id)
	if !after.quotaBlocked() {
		t.Fatal("a legacy block without a revision was dropped by a stale write")
	}
	s.mu.RLock()
	pending := s.quotaPauseErrors[id]
	s.mu.RUnlock()
	if pending == "" {
		t.Fatal("a legacy block dropped from disk was not scheduled for repair")
	}
}

// A watcher callback for the exact body just written by the plugin must not
// invent a sync failure and repeatedly rewrite the file it is watching.
func TestIdenticalPausedAuthReloadDoesNotTriggerRepair(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, nil)
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "model-a", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Block the background worker so the error, if introduced by parseAuth,
	// cannot be hidden by a successful repair before the assertion.
	s.credentialMu.Lock()
	for i := 0; i < 3; i++ {
		_, err = s.parseAuth(jsonBytes(map[string]any{
			"FileName": id + ".json", "RawJSON": body,
			"Host": map[string]any{"AuthDir": host.authDir},
		}))
		if err != nil {
			s.credentialMu.Unlock()
			t.Fatal(err)
		}
		s.mu.RLock()
		pending := s.quotaPauseErrors[id]
		s.mu.RUnlock()
		if pending != "" {
			s.credentialMu.Unlock()
			t.Fatalf("identical watcher body falsely marked pending: %q", pending)
		}
	}
	s.credentialMu.Unlock()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s.sweepQuotaBlocks(make(chan struct{}))
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("identical body caused the file to be rewritten: %v", err)
	}
}

// Matching a revision is not sufficient if a file editor changed the actual
// block under the same ID: restore the live model set and report divergence.
func TestChangedBlockWithSameRevisionTriggersRepair(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, nil)
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "model-a", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk Credential
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	disk.QuotaBlock.ModelID = "model-b"
	disk.QuotaBlock.ModelIDs = []string{"model-b"}
	s.credentialMu.Lock()
	if err := os.WriteFile(path, jsonBytes(disk), 0600); err != nil {
		s.credentialMu.Unlock()
		t.Fatal(err)
	}
	_, err = s.parseAuth(jsonBytes(map[string]any{
		"FileName": id + ".json", "RawJSON": jsonBytes(disk),
		"Host": map[string]any{"AuthDir": host.authDir},
	}))
	s.mu.RLock()
	pending := s.quotaPauseErrors[id]
	live := s.creds[id]
	s.mu.RUnlock()
	s.credentialMu.Unlock()
	if err != nil || pending == "" || live.QuotaBlock == nil || live.QuotaBlock.ModelID != "model-a" {
		t.Fatalf("changed block was not flagged and retained: err=%v pending=%q", err, pending)
	}
	s.sweepQuotaBlocks(make(chan struct{}))
	raw, err = os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &disk) != nil || disk.QuotaBlock == nil || disk.QuotaBlock.ModelID != "model-a" {
		t.Fatal("repair failed to restore the live block")
	}
}

// Without a cleared-state revision, an incoming valid block may be a legitimate
// persisted pause rather than an old snapshot. It is unsafe to erase it merely
// because the in-memory credential came from an older file without revisions.
func TestUnknownClearedRevisionDoesNotEraseIncomingPause(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, nil)
	current, _ := liveCredential(s, id)
	if current.QuotaRevision != "" || current.quotaBlocked() {
		t.Fatal("fixture must start with an unversioned, unblocked credential")
	}
	now := time.Now().UTC()
	manual := false
	paused := current
	paused.QuotaRevision = "incoming-revision"
	paused.QuotaBlock = &QuotaBlock{
		KeyHash: quotaPauseKeyHash(current.APIKey), ManualDisabled: &manual,
		Source: "upstream", Reason: "quota_exhausted", ModelID: "model-a",
		BlockedAt: now, ObservedAt: now, RetryAt: now.Add(time.Hour),
	}
	if err := os.WriteFile(path, jsonBytes(paused), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.parseAuth(jsonBytes(map[string]any{
		"FileName": id + ".json", "RawJSON": jsonBytes(paused),
		"Host": map[string]any{"AuthDir": host.authDir},
	})); err != nil {
		t.Fatal(err)
	}
	if c, _ := liveCredential(s, id); !c.quotaBlocked() {
		t.Fatal("valid incoming block was discarded without evidence of an explicit resume")
	}
	s.sweepQuotaBlocks(make(chan struct{}))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored Credential
	if err := json.Unmarshal(raw, &stored); err != nil || !stored.quotaBlocked() {
		t.Fatal("an unversioned live state must not erase a valid persisted pause")
	}
}

func TestUnversionedManualDisableSurvivesIncomingPause(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, nil)
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
		map[string]any{"id": id, "disabled": true}); status != http.StatusOK {
		t.Fatal("plugin manual disable failed")
	}
	current, _ := liveCredential(s, id)
	if !current.Disabled || current.QuotaRevision != "" {
		t.Fatal("fixture must have a manual disable but no quota revision")
	}
	now := time.Now().UTC()
	manual := false
	paused := current
	paused.Disabled = false // a stale body unaware of the plugin-console disable
	paused.QuotaRevision = "incoming-revision"
	paused.QuotaBlock = &QuotaBlock{
		KeyHash: quotaPauseKeyHash(current.APIKey), ManualDisabled: &manual,
		Source: "upstream", Reason: "quota_exhausted", ModelID: "model-a",
		ModelIDs: []string{"model-a"}, BlockedAt: now, ObservedAt: now, RetryAt: now.Add(time.Hour),
	}
	s.credentialMu.Lock()
	if err := os.WriteFile(path, jsonBytes(paused), 0600); err != nil {
		s.credentialMu.Unlock()
		t.Fatal(err)
	}
	_, err := s.parseAuth(jsonBytes(map[string]any{
		"FileName": id + ".json", "RawJSON": jsonBytes(paused),
		"Host": map[string]any{"AuthDir": host.authDir},
	}))
	live, _ := liveCredential(s, id)
	s.credentialMu.Unlock()
	if err != nil || !live.quotaBlocked() || !live.Disabled || live.QuotaBlock.ManualDisabled == nil || !*live.QuotaBlock.ManualDisabled {
		t.Fatal("adopting an unversioned pause lost the manual disable")
	}
	s.sweepQuotaBlocks(make(chan struct{}))
	raw, err := os.ReadFile(path)
	var stored Credential
	if err != nil || json.Unmarshal(raw, &stored) != nil || !stored.Disabled || !stored.quotaBlocked() ||
		stored.QuotaBlock.ManualDisabled == nil || !*stored.QuotaBlock.ManualDisabled {
		t.Fatal("manual disable and incoming pause were not repaired together")
	}
}

// The automatic recheck backoff must not overflow into an immediate retry.
func TestQuotaRecheckBackoffSaturates(t *testing.T) {
	s, _, id, _ := quotaRecoveryFixture(t, false, "model-a")
	now := time.Now().UTC()
	s.mu.Lock()
	// Prevent the worker from rechecking concurrently, and make the block due so
	// the reset hint cannot mask a broken exponential delay with the 30m cap.
	disabled := false
	s.cfg.Pause.Enabled = &disabled
	s.quotaNow = func() time.Time { return now }
	current := s.creds[id]
	current.QuotaBlock = cloneQuotaBlock(current.QuotaBlock)
	current.QuotaBlock.RetryAt = now.Add(-time.Minute)
	s.creds[id] = current
	revision := current.QuotaRevision
	s.mu.Unlock()
	for i := 0; i < 64; i++ {
		s.setQuotaRecheckStatus(id, revision,
			localFail(http.StatusConflict, "quota_recheck_unproven", "unproven"))
		s.mu.RLock()
		retry, ok := s.quotaRecheckRetryAt[id]
		s.mu.RUnlock()
		if !ok {
			t.Fatalf("round %d recorded no retry delay", i+1)
		}
		want := quotaRecheckBackoffMax
		if i == 0 {
			want = 5 * time.Minute
		} else if i == 1 {
			want = 10 * time.Minute
		} else if i == 2 {
			want = 20 * time.Minute
		}
		if delay := retry.Sub(now); delay != want {
			t.Fatalf("round %d scheduled a %v retry delay, want %v", i+1, delay, want)
		}
	}
}
