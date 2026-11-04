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

func setQuotaPauseFixtureClock(s *Service, now time.Time) func(time.Time) {
	var stamp atomic.Int64
	stamp.Store(now.UnixNano())
	s.mu.Lock()
	s.quotaNow = func() time.Time { return time.Unix(0, stamp.Load()).UTC() }
	s.mu.Unlock()
	return func(value time.Time) { stamp.Store(value.UnixNano()) }
}

func quotaFixtureAuthFile(t *testing.T, s *Service, host *fakeHost, id string, extra map[string]any) string {
	t.Helper()
	c, ok := liveCredential(s, id)
	if !ok {
		t.Fatal("fixture credential not found")
	}
	if err := os.MkdirAll(host.authDir, 0700); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(jsonBytes(c), &fields); err != nil {
		t.Fatal(err)
	}
	for key, value := range extra {
		fields[key] = value
	}
	path := filepath.Join(host.authDir, id+".json")
	if err := os.WriteFile(path, jsonBytes(fields), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestObserveQuotaBlockPersistsOnlyQuotaFields(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, map[string]any{"opaque_extension": json.Number("12345678901234567890")})
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	setNow := setQuotaPauseFixtureClock(s, now)
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true, quotaResetAt: now.Add(5 * time.Hour)})
	current, _ := liveCredential(s, id)
	if !current.quotaBlocked() || current.Disabled || !current.QuotaBlock.RetryAt.Equal(now.Add(5*time.Hour)) {
		t.Fatal("quota block was not published with its bounded deadline")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `12345678901234567890`) {
		t.Fatal("quota write discarded an unknown credential extension")
	}
	var stored Credential
	if err := json.Unmarshal(raw, &stored); err != nil || !stored.quotaBlocked() || stored.Disabled {
		t.Fatal("quota block or manual state did not survive the auth-file round trip")
	}
	s.mu.RLock()
	syncError := s.quotaPauseErrors[id]
	s.mu.RUnlock()
	if syncError != "" {
		t.Fatal("successful quota write reported a synchronization error")
	}

	// A delayed error for another model may add ambiguity, but cannot push the
	// first deadline forward indefinitely.
	now = now.Add(10 * time.Minute)
	setNow(now)
	s.observeQuotaBlock(source, "fixture-other-model", &APIError{Status: 402, quotaExhausted: true, quotaResetAt: now.Add(24 * time.Hour)})
	current, _ = liveCredential(s, id)
	if !current.QuotaBlock.MultipleModels || !current.QuotaBlock.RetryAt.Equal(time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)) {
		t.Fatal("duplicate error extended the deadline or lost model ambiguity")
	}
}

func TestObserveQuotaBlockFailureKeepsLocalGuard(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	source, _ := liveCredential(s, id)
	// The fake host reports a path but does not create a physical auth file.
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: 402, quotaExhausted: true})
	current, _ := liveCredential(s, id)
	if !current.quotaBlocked() {
		t.Fatal("failed persistence discarded the in-memory block")
	}
	s.mu.RLock()
	syncError := s.quotaPauseErrors[id]
	s.mu.RUnlock()
	if syncError != quotaPauseSyncError {
		t.Fatal("failed persistence was not surfaced")
	}
	quotaFixtureAuthFile(t, s, host, id, nil)
	s.credentialMu.Lock()
	err := s.persistQuotaBlockLocked(id, current.QuotaRevision)
	s.credentialMu.Unlock()
	if err != nil {
		t.Fatalf("retry of quota persistence failed: %v", err)
	}
}

func TestResumeQuotaBlockPreservesLatestManualState(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, nil)
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	setNow := setQuotaPauseFixtureClock(s, now)
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: 402, quotaExhausted: true, quotaResetAt: now.Add(time.Hour)})
	current, _ := liveCredential(s, id)
	firstRevision := current.QuotaRevision

	// A newer error invalidates any recovery decision based on an old revision.
	now = now.Add(time.Minute)
	setNow(now)
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: 402, quotaExhausted: true})
	current, _ = liveCredential(s, id)
	s.credentialMu.Lock()
	err := s.resumeQuotaBlockLocked(id, firstRevision, false)
	s.credentialMu.Unlock()
	if err == nil {
		t.Fatal("old revision cleared a newer quota block")
	}

	// Simulate an operator using the plugin's manual switch while paused, then
	// CPA projecting effective Disabled=true into the stored file.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	block := cloneQuotaBlock(current.QuotaBlock)
	manual := true
	block.ManualDisabled = &manual
	fields["disabled"] = jsonBytes(true)
	fields["quota_block"] = jsonBytes(block)
	if err := os.WriteFile(path, jsonBytes(fields), 0600); err != nil {
		t.Fatal(err)
	}
	s.credentialMu.Lock()
	err = s.resumeQuotaBlockLocked(id, current.QuotaRevision, true)
	s.credentialMu.Unlock()
	if err == nil {
		t.Fatal("future deadline was resumed by the timer")
	}
	now = now.Add(time.Hour)
	setNow(now)
	s.credentialMu.Lock()
	err = s.resumeQuotaBlockLocked(id, current.QuotaRevision, true)
	s.credentialMu.Unlock()
	if err != nil {
		t.Fatalf("due recovery failed: %v", err)
	}
	recovered, _ := liveCredential(s, id)
	if !recovered.Disabled || recovered.QuotaBlock != nil {
		t.Fatal("recovery did not keep the latest manual disable")
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored Credential
	if json.Unmarshal(raw, &stored) != nil || !stored.Disabled || stored.QuotaBlock != nil {
		t.Fatal("manual disable or quota recovery did not survive persistence")
	}
}

func TestQuotaBlockSurvivesStaleWatcherAfterFailedWrite(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	source, _ := liveCredential(s, id)
	stale := jsonBytes(source)
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: 402, quotaExhausted: true})
	// The old callback cannot resolve the nonexistent file, so it reuses the
	// bytes it captured before the quota block was published.
	result, err := s.parseAuth(jsonBytes(map[string]any{
		"FileName": id + ".json", "RawJSON": stale,
	}))
	if err != nil || result.(map[string]any)["Handled"] != true {
		t.Fatal("stale watcher callback failed")
	}
	current, _ := liveCredential(s, id)
	if !current.quotaBlocked() {
		t.Fatal("stale watcher callback removed the in-memory quota block")
	}
}

func TestObserveQuotaBlockUsesLatestManualState(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	path := quotaFixtureAuthFile(t, s, host, id, map[string]any{"disabled": true})
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: 402, quotaExhausted: true})
	current, _ := liveCredential(s, id)
	if !current.Disabled || current.QuotaBlock == nil || current.QuotaBlock.ManualDisabled == nil || !*current.QuotaBlock.ManualDisabled {
		t.Fatal("latest manual disable on disk was overwritten")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored Credential
	if err := json.Unmarshal(raw, &stored); err != nil || !stored.Disabled {
		t.Fatal("manual disable was not preserved on disk")
	}
}

func TestQuotaBlockSweepExpiresOnlyDueBlocks(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	quotaFixtureAuthFile(t, s, host, id, nil)
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	setNow := setQuotaPauseFixtureClock(s, now)
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: 402, quotaExhausted: true, quotaResetAt: now.Add(time.Hour)})

	stop := make(chan struct{})
	s.sweepQuotaBlocks(stop)
	if c, _ := liveCredential(s, id); !c.quotaBlocked() {
		t.Fatal("a block before its deadline was resumed")
	}
	now = now.Add(time.Hour)
	setNow(now)
	s.sweepQuotaBlocks(stop)
	c, _ := liveCredential(s, id)
	if c.quotaBlocked() {
		t.Fatal("an expired block was not resumed by the sweep")
	}
	// The sweep must be a no-op once the block is gone.
	s.sweepQuotaBlocks(stop)
	if c, _ := liveCredential(s, id); c.quotaBlocked() {
		t.Fatal("sweep resurrected a cleared block")
	}
}

func TestQuotaBlockSweepRetriesFailedPersistence(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	source, _ := liveCredential(s, id)
	// No physical file yet: the first write fails and the local guard stays.
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: 402, quotaExhausted: true})
	s.mu.RLock()
	before := s.quotaPauseErrors[id]
	s.mu.RUnlock()
	if before == "" {
		t.Fatal("fixture did not fail its first persistence")
	}
	quotaFixtureAuthFile(t, s, host, id, nil)
	s.sweepQuotaBlocks(make(chan struct{}))
	s.mu.RLock()
	after := s.quotaPauseErrors[id]
	s.mu.RUnlock()
	if after != "" {
		t.Fatalf("sweep did not clear the persistence error: %q", after)
	}
	if c, _ := liveCredential(s, id); !c.quotaBlocked() {
		t.Fatal("sweep discarded a not-yet-due block")
	}
}
