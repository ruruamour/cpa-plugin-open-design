package opendesign

import (
	"testing"
)

func TestExistingQuotaBlockCollectsLateErrorsWhenPolicyDisabled(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := seedModels(t, s, host)
	quotaFixtureAuthFile(t, s, host, id, nil)
	source, _ := liveCredential(s, id)
	exhausted := &APIError{Status: 402, quotaExhausted: true}
	s.observeQuotaBlock(source, "model-a", exhausted)
	before, _ := liveCredential(s, id)
	if !before.quotaBlocked() {
		t.Fatal("initial quota block was not created")
	}
	off := false
	s.mu.Lock()
	s.cfg.Pause.Enabled = &off
	s.mu.Unlock()
	s.observeQuotaBlock(source, "model-b", exhausted)
	after, _ := liveCredential(s, id)
	models, complete := quotaBlockModels(after.QuotaBlock)
	if !after.quotaBlocked() || !complete || len(models) != 2 || models[0] != "model-a" || models[1] != "model-b" || after.QuotaRevision == before.QuotaRevision {
		t.Fatalf("late error was lost after policy toggle: models=%v complete=%v", models, complete)
	}
}

func TestDisabledPolicyDoesNotCreateNewQuotaBlock(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := seedModels(t, s, host)
	quotaFixtureAuthFile(t, s, host, id, nil)
	source, _ := liveCredential(s, id)
	off := false
	s.mu.Lock()
	s.cfg.Pause.Enabled = &off
	s.mu.Unlock()
	s.observeQuotaBlock(source, "model-a", &APIError{Status: 402, quotaExhausted: true})
	current, _ := liveCredential(s, id)
	if current.quotaBlocked() {
		t.Fatal("disabled policy created a new quota block")
	}
}
