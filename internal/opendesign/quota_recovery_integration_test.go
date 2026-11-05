package opendesign

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func quotaRecoveryFixture(t *testing.T, manual bool, models ...string) (*Service, *fakeHost, string, string) {
	t.Helper()
	s, host := newQuotaPauseTestService(t)
	id := addQuotaCredential(t, s, host, "workspace-recovery")
	if manual {
		status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "disabled": true})
		if status != http.StatusOK {
			t.Fatalf("set manual disable: %d", status)
		}
	}
	path := quotaFixtureAuthFile(t, s, host, id, nil)
	now := time.Now().UTC()
	s.mu.Lock()
	c := s.creds[id]
	c.Disabled = manual
	c.QuotaBlock = &QuotaBlock{
		KeyHash: quotaPauseKeyHash(c.APIKey), ManualDisabled: &manual,
		Source: "upstream", Reason: "quota_exhausted",
		BlockedAt: now.Add(-2 * time.Minute), ObservedAt: now.Add(-time.Minute), RetryAt: now.Add(time.Hour),
	}
	for _, model := range models {
		addQuotaBlockModel(c.QuotaBlock, model)
	}
	c.QuotaRevision = newID()
	s.creds[id] = c
	s.mu.Unlock()
	s.credentialMu.Lock()
	err := s.persistQuotaBlockLocked(id, c.QuotaRevision, false)
	s.credentialMu.Unlock()
	if err != nil {
		t.Fatalf("write fixture block: %v", err)
	}
	return s, host, id, path
}

func queueRecoveryEvidence(t *testing.T, host *fakeHost, model, member, verdict string) {
	t.Helper()
	body := officialModelCoverage(t, "workspace-recovery", model)
	body["generatedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
	body["workspaceMemberId"] = member
	body["fundingCheck"] = verdict
	queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, body)})
}

func requireQuotaStillBlocked(t *testing.T, s *Service, id, path string) {
	t.Helper()
	c, ok := liveCredential(s, id)
	if !ok || !c.quotaBlocked() {
		t.Fatal("failed or unproven recheck cleared the in-memory block")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk Credential
	if json.Unmarshal(data, &disk) != nil || !disk.quotaBlocked() {
		t.Fatal("failed or unproven recheck cleared the on-disk block")
	}
}

func TestQuotaRecoveryRequiresFreshEvidenceForAllModels(t *testing.T) {
	for _, tc := range []struct {
		name        string
		manual      bool
		models      []string
		verdicts    []string
		members     []string
		wantSuccess bool
	}{
		{"one model, manually disabled", true, []string{"model-a"}, []string{"available"}, []string{"member"}, true},
		{"all known models", false, []string{"model-a", "model-b"}, []string{"available", "available"}, []string{"member", "member"}, true},
		{"second model insufficient", false, []string{"model-a", "model-b"}, []string{"available", "insufficient"}, []string{"member", "member"}, false},
		{"different workspace members", false, []string{"model-a", "model-b"}, []string{"available", "available"}, []string{"member-a", "member-b"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, host, id, path := quotaRecoveryFixture(t, tc.manual, tc.models...)
			for i, model := range tc.models {
				queueRecoveryEvidence(t, host, model, tc.members[i], tc.verdicts[i])
			}
			err := s.recheckQuotaBlock("callback", id)
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("recheck error %v, want success %v", err, tc.wantSuccess)
			}
			if !tc.wantSuccess {
				requireQuotaStillBlocked(t, s, id, path)
				return
			}
			c, _ := liveCredential(s, id)
			if c.quotaBlocked() || c.Disabled != tc.manual {
				t.Fatalf("wrong recovered or manual state: blocked=%v disabled=%v", c.quotaBlocked(), c.Disabled)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var disk Credential
			if err := json.Unmarshal(raw, &disk); err != nil || disk.quotaBlocked() || disk.Disabled != tc.manual {
				t.Fatalf("on-disk recovered state is wrong: %v", err)
			}
		})
	}
}

func TestQuotaRecoveryRejectsCachedEvidence(t *testing.T) {
	s, host, id, path := quotaRecoveryFixture(t, false, "model-a")
	queueRecoveryEvidence(t, host, "model-a", "member", "available")
	view, err := s.readModelCoverage(id, "model-a", "callback", true)
	if err != nil || view.State != "available" {
		t.Fatalf("initial refresh failed: %v", err)
	}
	// The cache still says available and is inside the refresh interval, but a
	// recovery round must ask the official API again: only a response requested
	// after the round began can prove recovery.
	queueRecoveryEvidence(t, host, "model-a", "member", "insufficient")
	if err := s.recheckQuotaBlock("callback", id); err == nil {
		t.Fatal("cached available response was accepted as a new recheck")
	}
	requireQuotaStillBlocked(t, s, id, path)
	if got := host.countMethod("host.http.do"); got != 2 {
		t.Fatalf("recheck reused the cached view instead of asking again: %d HTTP requests", got)
	}
}

// A proof read started while another refresh of the same model is running must
// wait for it and then perform its own request; joining would reuse evidence that
// began before the round and could never prove anything.
func TestQuotaRecoveryProofDoesNotJoinOlderRefresh(t *testing.T) {
	s, host, id, path := quotaRecoveryFixture(t, false, "model-a")
	for range cap(s.modelCoverageSlots) {
		s.modelCoverageSlots <- struct{}{}
	}
	defer func() {
		for {
			select {
			case <-s.modelCoverageSlots:
			default:
				return
			}
		}
	}()
	// The console's own refresh answers available, the round's own request
	// answers insufficient: only a joined older attempt could "recover" here.
	queueRecoveryEvidence(t, host, "model-a", "member", "available")
	queueRecoveryEvidence(t, host, "model-a", "member", "insufficient")
	done := make(chan coverageResult, 1)
	go func() {
		view, err := s.readModelCoverage(id, "model-a", "callback", true)
		done <- coverageResult{view, err}
	}()
	waitCoverageAttempt(t, s, modelCoverageKey{CredentialID: id, ModelID: "model-a"})
	result := make(chan error, 1)
	go func() { result <- s.recheckQuotaBlock("callback", id) }()
	<-s.modelCoverageSlots
	receiveCoverage(t, done)
	<-s.modelCoverageSlots
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("a joined older refresh proved recovery")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recheck did not finish")
	}
	requireQuotaStillBlocked(t, s, id, path)
}

func TestQuotaRecoveryRejectsUnknownModelsAndInFlight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Service, Credential)
	}{
		{"legacy incomplete multiple-model list", func(s *Service, c Credential) {
			c.QuotaBlock = cloneQuotaBlock(c.QuotaBlock)
			c.QuotaBlock.MultipleModels, c.QuotaBlock.ModelIDs = true, nil
			s.creds[c.ID] = c
		}},
		{"same key request is still in flight", func(s *Service, c Credential) {
			s.quotaInflight[quotaInFlightKeyOf(c)]++
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, host, id, path := quotaRecoveryFixture(t, false, "model-a")
			s.mu.Lock()
			tc.mutate(s, s.creds[id])
			s.mu.Unlock()
			if err := s.recheckQuotaBlock("callback", id); err == nil {
				t.Fatal("unprovable block was cleared")
			}
			requireQuotaStillBlocked(t, s, id, path)
			if n := host.countMethod("host.http.do"); n != 0 {
				t.Fatalf("unprovable block unexpectedly made %d upstream calls", n)
			}
		})
	}
}

func makeQuotaPauseDue(t *testing.T, s *Service, id string) {
	t.Helper()
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	s.mu.Lock()
	c := s.creds[id]
	c.QuotaBlock = cloneQuotaBlock(c.QuotaBlock)
	c.QuotaBlock.RetryAt = time.Now().Add(-time.Second)
	c.QuotaRevision = newID()
	s.creds[id] = c
	s.mu.Unlock()
	if err := s.persistQuotaBlockLocked(id, c.QuotaRevision, false); err != nil {
		t.Fatalf("make block due: %v", err)
	}
}

func waitForRecoveryCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for automatic quota recheck")
}

func TestQuotaRecoveryWorkerRechecksDueAndPreservesManualDisable(t *testing.T) {
	s, host, id, path := quotaRecoveryFixture(t, true, "model-a")
	queueRecoveryEvidence(t, host, "model-a", "member", "available")
	makeQuotaPauseDue(t, s, id)
	s.wakeQuotaPauseWorker()
	waitForRecoveryCondition(t, func() bool {
		c, ok := liveCredential(s, id)
		return ok && !c.quotaBlocked()
	})
	c, _ := liveCredential(s, id)
	if !c.Disabled || host.countMethod("host.http.do") != 1 {
		t.Fatal("automatic recheck did not preserve the manual disable or was not serial")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk Credential
	if json.Unmarshal(raw, &disk) != nil || disk.quotaBlocked() || !disk.Disabled {
		t.Fatal("automatic recovery did not persist the manual disable")
	}
}

func TestQuotaRecoveryWorkerKeepsBlockWhenOfficialCheckFails(t *testing.T) {
	s, host, id, path := quotaRecoveryFixture(t, false, "model-a")
	queueQuota(host, fakeHTTPResponse{Status: http.StatusUnauthorized, Body: []byte(`{"error":"unauthorized"}`)})
	makeQuotaPauseDue(t, s, id)
	s.wakeQuotaPauseWorker()
	waitForRecoveryCondition(t, func() bool { return host.countMethod("host.http.do") >= 1 })
	// Synchronize with the serial recheck so the assertion runs after it returns.
	s.quotaRecoveryGate <- struct{}{}
	<-s.quotaRecoveryGate
	waitForRecoveryCondition(t, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.quotaRecheckStatus[id] == "unauthorized"
	})
	requireQuotaStillBlocked(t, s, id, path)
	if host.countMethod("host.http.do") != 1 {
		t.Fatal("failed checks bypassed the per-model retry interval")
	}
	status, _, raw := managementCall(t, s, http.MethodGet, apiBase+"/status", nil, nil)
	if status != http.StatusOK || !strings.Contains(raw, `"recheck_status":"unauthorized"`) {
		t.Fatalf("safe authorization failure was not shown in status: %d %s", status, raw)
	}
	for _, secret := range []string{"quota-control-secret", "sk-quota-inference-secret", "private-member-do-not-expose"} {
		if strings.Contains(raw, secret) {
			t.Fatal("status exposed an internal recovery credential or member identity")
		}
	}
}

func TestQuotaRecoveryWorkerDoesNotCheckWhenPolicyIsDisabled(t *testing.T) {
	s, host, id, path := quotaRecoveryFixture(t, false, "model-a")
	disabled := false
	s.mu.Lock()
	s.cfg.Pause.Enabled = &disabled
	s.mu.Unlock()
	queueRecoveryEvidence(t, host, "model-a", "member", "available")
	makeQuotaPauseDue(t, s, id)
	s.recheckDueQuotaBlocks(s.stopCh)
	requireQuotaStillBlocked(t, s, id, path)
	if host.countMethod("host.http.do") != 0 {
		t.Fatal("disabled automatic policy queried the official API")
	}
}

func TestQuotaRecoveryFencesControlKeyChangeDuringCheck(t *testing.T) {
	s, host, id, path := quotaRecoveryFixture(t, false, "model-a")
	queueRecoveryEvidence(t, host, "model-a", "member", "available")
	started, release := make(chan struct{}), make(chan struct{})
	s.SetHost(func(method string, in, out any) error {
		if method == "host.http.do" {
			close(started)
			<-release
		}
		return host.call(method, in, out)
	})
	result := make(chan error, 1)
	go func() { result <- s.recheckQuotaBlock("callback", id) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("official preflight did not start")
	}
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
		"id": id, "control_key": "new-control-secret",
	})
	close(release)
	if status != http.StatusOK {
		t.Fatalf("update failed: %d %s", status, raw)
	}
	select {
	case err := <-result:
		if err == nil || strings.Contains(err.Error(), "new-control-secret") {
			t.Fatalf("old proof must fail without leaking the changed secret: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recheck did not finish after identity update")
	}
	requireQuotaStillBlocked(t, s, id, path)
}

func TestQuotaRecoveryWorkerBacksOffAfterRepeatedFailures(t *testing.T) {
	s, host, id, path := quotaRecoveryFixture(t, false, "model-a")
	// A coding-plan window can be days away, so consecutive failures must
	// lengthen the automatic retry delay instead of polling every ticker.
	revision, _ := liveCredential(s, id)
	failure := &APIError{Status: http.StatusConflict, Kind: "quota_recheck_unproven", Message: "unproven"}
	var previous time.Duration
	for round := 1; round <= 4; round++ {
		s.setQuotaRecheckStatus(id, revision.QuotaRevision, failure)
		s.mu.RLock()
		failures := s.quotaRecheckFailures[id]
		delay := time.Until(s.quotaRecheckRetryAt[id])
		s.mu.RUnlock()
		if failures != round {
			t.Fatalf("round %d: failures = %d", round, failures)
		}
		if delay < quotaRecheckBackoffMin {
			t.Fatalf("round %d: retry delay %s is below the minimum backoff", round, delay)
		}
		if delay > quotaRecheckBackoffMax+time.Second {
			t.Fatalf("round %d: retry delay %s exceeds the cap", round, delay)
		}
		if round > 1 && delay+2*time.Second < previous {
			t.Fatalf("round %d: retry delay %s did not grow from %s", round, delay, previous)
		}
		previous = delay
	}
	// While the backoff is armed the automatic sweep must not contact the API.
	makeQuotaPauseDue(t, s, id)
	before := host.countMethod("host.http.do")
	s.recheckDueQuotaBlocks(s.stopCh)
	if host.countMethod("host.http.do") != before {
		t.Fatal("automatic recheck ignored the failure backoff")
	}
	requireQuotaStillBlocked(t, s, id, path)

	// Success clears the status, the counter and the backoff, so a later pause
	// starts its automatic schedule from scratch.
	current, _ := liveCredential(s, id)
	s.setQuotaRecheckStatus(id, current.QuotaRevision, nil)
	s.mu.RLock()
	_, armed := s.quotaRecheckRetryAt[id]
	_, failures := s.quotaRecheckFailures[id]
	_, status := s.quotaRecheckStatus[id]
	s.mu.RUnlock()
	if armed || failures || status {
		t.Fatal("a cleared recovery left stale backoff state")
	}
}
