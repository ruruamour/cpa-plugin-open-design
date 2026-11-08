package opendesign

import (
	"strings"
	"testing"
	"time"
)

func TestQuotaPauseReviewAtBoundsFirstReviewWithoutChangingStoredDeadline(t *testing.T) {
	start := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		retry time.Time
		want  time.Time
	}{
		{"long upstream hint", start.Add(24 * time.Hour), start.Add(30 * time.Minute)},
		{"earlier hint or configured cap", start.Add(5 * time.Minute), start.Add(5 * time.Minute)},
		{"exact boundary", start.Add(30 * time.Minute), start.Add(30 * time.Minute)},
		{"already due", start.Add(-time.Minute), start.Add(-time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := &QuotaBlock{BlockedAt: start, RetryAt: tc.retry}
			if got := quotaPauseReviewAt(block); !got.Equal(tc.want) {
				t.Fatalf("review = %v, want %v", got, tc.want)
			}
			if !block.RetryAt.Equal(tc.retry) {
				t.Fatal("calculating a review changed the persisted deadline")
			}
		})
	}
}

func TestQuotaPauseReviewAcceptsNormalizedLegacySingleModel(t *testing.T) {
	now := time.Now().UTC()
	manual := false
	c := Credential{APIKey: "fixture-key", QuotaBlock: &QuotaBlock{
		KeyHash: quotaPauseKeyHash("fixture-key"), ManualDisabled: &manual,
		Source: "upstream", Reason: "quota_exhausted",
		BlockedAt: now.Add(-time.Hour), RetryAt: now.Add(12 * time.Hour), ModelID: "model-a",
	}}
	block, changed := normalizeQuotaBlock(c, now)
	if !changed || !block.RetryAt.Equal(c.QuotaBlock.RetryAt) || !quotaPauseReviewAt(block).Before(now) {
		t.Fatal("normalized legacy pause was not due without changing its stored deadline")
	}
	models, complete := quotaBlockModels(block)
	if !complete || len(models) != 1 || models[0] != "model-a" {
		t.Fatal("normalization did not retain the legacy model for proof")
	}
}

// prepareLongHintReview makes an old single-model pause due under the new
// schedule while its original stored deadline is still in the future. Keep the
// worker backoff armed until the caller is ready, including during the write.
func prepareLongHintReview(t *testing.T, s *Service, id string) {
	t.Helper()
	now := time.Now().UTC()
	s.mu.Lock()
	c := s.creds[id]
	c.QuotaBlock = cloneQuotaBlock(c.QuotaBlock)
	c.QuotaBlock.BlockedAt = now.Add(-31 * time.Minute)
	c.QuotaBlock.ObservedAt = now.Add(-time.Minute)
	c.QuotaBlock.RetryAt = now.Add(23 * time.Hour)
	// Older single-model auth files did not include the model list.
	c.QuotaBlock.ModelIDs = nil
	c.QuotaRevision = newID()
	s.creds[id] = c
	s.quotaRecheckRetryAt[id] = now.Add(time.Hour)
	s.mu.Unlock()
	s.credentialMu.Lock()
	err := s.persistQuotaBlockLocked(id, c.QuotaRevision, false)
	s.credentialMu.Unlock()
	if err != nil {
		t.Fatalf("persist old pause: %v", err)
	}
}

func enableLongHintReview(s *Service, id string) {
	s.mu.Lock()
	delete(s.quotaRecheckRetryAt, id)
	s.mu.Unlock()
	s.wakeQuotaPauseWorker()
}

func TestQuotaWorkerReviewsExistingLongHintAndPreservesManualDisable(t *testing.T) {
	s, host, id, path := quotaRecoveryFixture(t, true, "model-a")
	prepareLongHintReview(t, s, id)
	current, _ := liveCredential(s, id)
	if !current.QuotaBlock.RetryAt.After(time.Now().Add(20 * time.Hour)) {
		t.Fatal("test must keep the historical deadline in the future")
	}
	view := quotaPauseView(current, time.Now(), "", "")
	if view["awaiting_recheck"] != true || !view["resume_at"].(time.Time).Equal(current.QuotaBlock.BlockedAt.Add(30*time.Minute)) {
		t.Fatalf("status did not report the effective review deadline: %#v", view)
	}
	requireQuotaStillBlocked(t, s, id, path)
	queueRecoveryEvidence(t, host, "model-a", "member", "available")
	enableLongHintReview(s, id)
	waitForRecoveryCondition(t, func() bool {
		c, ok := liveCredential(s, id)
		return ok && !c.quotaBlocked()
	})
	current, _ = liveCredential(s, id)
	if !current.Disabled {
		t.Fatal("automatic recovery enabled a manually disabled credential")
	}
	if got := host.countMethod("host.http.do"); got != 1 {
		t.Fatalf("model-level recovery issued %d upstream requests, want one", got)
	}
	if url := stringField(host.lastPayload("host.http.do"), "url"); !strings.Contains(url, "modelId=model-a") {
		t.Fatalf("recovery was not a model-specific funding check: %s", url)
	}
}

func TestQuotaWorkerLongHintRequiresFreshProofAndBacksOff(t *testing.T) {
	s, host, id, path := quotaRecoveryFixture(t, false, "model-a")
	prepareLongHintReview(t, s, id)
	queueRecoveryEvidence(t, host, "model-a", "member", "insufficient")
	enableLongHintReview(s, id)
	waitForRecoveryCondition(t, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.quotaRecheckStatus[id] == "unproven"
	})
	requireQuotaStillBlocked(t, s, id, path)
	s.mu.RLock()
	retryAt, failures := s.quotaRecheckRetryAt[id], s.quotaRecheckFailures[id]
	s.mu.RUnlock()
	if delay := time.Until(retryAt); failures != 1 || delay < 4*time.Minute+50*time.Second || delay > 5*time.Minute {
		t.Fatalf("first failed review backoff = %v, failures=%d, want about 5m", delay, failures)
	}
	s.recheckDueQuotaBlocks(s.stopCh)
	if got := host.countMethod("host.http.do"); got != 1 {
		t.Fatalf("backoff allowed an early repeated request: %d", got)
	}
	requireQuotaStillBlocked(t, s, id, path)

	// When the backoff expires, a fresh available response may prove recovery.
	queueRecoveryEvidence(t, host, "model-a", "member", "available")
	s.mu.Lock()
	s.quotaRecheckRetryAt[id] = time.Now().Add(-time.Second)
	s.mu.Unlock()
	s.wakeQuotaPauseWorker()
	waitForRecoveryCondition(t, func() bool {
		c, ok := liveCredential(s, id)
		return ok && !c.quotaBlocked()
	})
	if got := host.countMethod("host.http.do"); got != 2 {
		t.Fatalf("recovery did not require a new official request: %d", got)
	}
}

func TestQuotaWorkerLongHintRespectsDisabledPolicyAndPendingSync(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Service, string)
	}{
		{"policy disabled", func(s *Service, id string) {
			disabled := false
			s.cfg.Pause.Enabled = &disabled
		}},
		{"pause not synced", func(s *Service, id string) {
			s.quotaPauseErrors[id] = quotaPauseSyncError
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, host, id, path := quotaRecoveryFixture(t, false, "model-a")
			prepareLongHintReview(t, s, id)
			// Prevent the background file-repair sweep from completing before
			// this direct recheck scan has observed the pending sync flag.
			s.credentialMu.Lock()
			defer s.credentialMu.Unlock()
			s.mu.Lock()
			tc.mutate(s, id)
			delete(s.quotaRecheckRetryAt, id)
			s.mu.Unlock()
			// Run the recheck sweep directly: the separate file-repair worker is
			// allowed to repair a pending sync before a later worker iteration.
			s.recheckDueQuotaBlocks(s.stopCh)
			requireQuotaStillBlocked(t, s, id, path)
			if got := host.countMethod("host.http.do"); got != 0 {
				t.Fatalf("unready pause issued %d official requests", got)
			}
		})
	}
}

func TestQuotaLongHintDoesNotAdvanceBlindResume(t *testing.T) {
	s, _, id, path := quotaRecoveryFixture(t, false, "model-a")
	prepareLongHintReview(t, s, id)
	c, _ := liveCredential(s, id)
	s.credentialMu.Lock()
	err := s.resumeQuotaBlockLocked(id, c.QuotaRevision, true)
	s.credentialMu.Unlock()
	if err == nil {
		t.Fatal("earlier proof review made the legacy blind resume due")
	}
	requireQuotaStillBlocked(t, s, id, path)
}

func TestQuotaLongHintDueOnlyRejectsEarlyReview(t *testing.T) {
	s, host, id, path := quotaRecoveryFixture(t, false, "model-a")
	// The fixture is only two minutes old and has a long stored RetryAt.
	queueRecoveryEvidence(t, host, "model-a", "member", "available")
	if err := s.recheckQuotaBlockMode("callback", id, true); err == nil {
		t.Fatal("automatic round started before the effective deadline")
	}
	requireQuotaStillBlocked(t, s, id, path)
	if got := host.countMethod("host.http.do"); got != 0 {
		t.Fatalf("early automatic round issued %d upstream requests", got)
	}
}
