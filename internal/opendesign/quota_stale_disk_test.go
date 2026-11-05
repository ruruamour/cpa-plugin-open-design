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

// The automatic recheck backoff must not overflow into an immediate retry.
func TestQuotaRecheckBackoffSaturates(t *testing.T) {
	s, host, id, _ := quotaRecoveryFixture(t, false, "model-a")
	s.mu.RLock()
	revision := s.creds[id].QuotaRevision
	s.mu.RUnlock()
	for i := 0; i < 64; i++ {
		now := s.quotaPauseTime()
		s.setQuotaRecheckStatus(id, revision,
			localFail(http.StatusConflict, "quota_recheck_unproven", "unproven"))
		s.mu.RLock()
		retry, ok := s.quotaRecheckRetryAt[id]
		s.quotaRecheckRetryAt[id] = time.Time{}
		s.mu.RUnlock()
		if !ok {
			t.Fatalf("round %d recorded no retry delay", i+1)
		}
		if delay := retry.Sub(now); delay <= 0 || delay > quotaRecheckBackoffMax+time.Second {
			t.Fatalf("round %d scheduled a %v retry delay", i+1, delay)
		}
	}
	_ = host
}
