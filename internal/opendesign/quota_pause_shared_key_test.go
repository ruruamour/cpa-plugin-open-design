package opendesign

import (
	"testing"
)

func sharedKeyPauseFixture(t *testing.T) (*Service, *fakeHost, Credential, Credential, string) {
	t.Helper()
	s, host := newQuotaPauseTestService(t)
	const key = "fixture-shared-inference-key"
	idA := addCredential(t, s, host, "A", key)
	idB := addCredential(t, s, host, "B", key)
	pathA := quotaFixtureAuthFile(t, s, host, idA, nil)
	quotaFixtureAuthFile(t, s, host, idB, nil)
	a, _ := liveCredential(s, idA)
	b, _ := liveCredential(s, idB)
	return s, host, a, b, pathA
}

func TestQuotaPauseSharedKeyMergesAnotherCredentialModel(t *testing.T) {
	s, _, sourceA, sourceB, pathA := sharedKeyPauseFixture(t)
	exhausted := &APIError{Status: 402, quotaExhausted: true}
	s.observeQuotaBlock(sourceA, "model-a", exhausted)
	s.observeQuotaBlock(sourceB, "model-b", exhausted)
	a, _ := liveCredential(s, sourceA.ID)
	b, _ := liveCredential(s, sourceB.ID)
	models, complete := quotaBlockModels(a.QuotaBlock)
	if !a.quotaBlocked() || !b.quotaBlocked() || !complete || len(models) != 2 || models[0] != "model-a" || models[1] != "model-b" {
		t.Fatalf("shared key lost another credential's exhausted model: %v complete=%v", models, complete)
	}
	requireQuotaStillBlocked(t, s, sourceA.ID, pathA)
}

func TestQuotaPauseSharedKeyUnknownSourceMergesOnlyExistingBlocks(t *testing.T) {
	s, _, sourceA, sourceB, _ := sharedKeyPauseFixture(t)
	exhausted := &APIError{Status: 402, quotaExhausted: true}
	s.observeQuotaBlock(sourceA, "model-a", exhausted)
	unknown := Credential{APIKey: sourceA.APIKey}
	s.observeQuotaBlock(unknown, "model-b", exhausted)
	a, _ := liveCredential(s, sourceA.ID)
	b, _ := liveCredential(s, sourceB.ID)
	models, complete := quotaBlockModels(a.QuotaBlock)
	if !complete || len(models) != 2 || b.quotaBlocked() {
		t.Fatalf("unknown same-key request should only update an existing block: models=%v complete=%v otherBlocked=%v", models, complete, b.quotaBlocked())
	}
}

func TestQuotaPauseSharedKeyDoesNotReblockExplicitlyResumedRecord(t *testing.T) {
	s, _, sourceA, sourceB, _ := sharedKeyPauseFixture(t)
	exhausted := &APIError{Status: 402, quotaExhausted: true}
	s.observeQuotaBlock(sourceA, "model-a", exhausted)
	a, _ := liveCredential(s, sourceA.ID)
	s.credentialMu.Lock()
	err := s.resumeQuotaBlockLocked(sourceA.ID, a.QuotaRevision, false)
	s.credentialMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	s.observeQuotaBlock(sourceB, "model-b", exhausted)
	a, _ = liveCredential(s, sourceA.ID)
	b, _ := liveCredential(s, sourceB.ID)
	if a.quotaBlocked() || !b.quotaBlocked() {
		t.Fatal("another record's late result re-created an explicitly cleared block")
	}
}
