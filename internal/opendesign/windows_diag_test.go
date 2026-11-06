package opendesign

import (
	"context"
	"testing"
	"time"
)

func TestWindowsDiagRecoveryProof(t *testing.T) {
	s, host, id, _ := quotaRecoveryFixture(t, false, "model-a")
	queueRecoveryEvidence(t, host, "model-a", "member", "available")
	s.mu.RLock()
	current := s.creds[id]
	block := cloneQuotaBlock(current.QuotaBlock)
	workspace := current.WorkspaceID
	s.mu.RUnlock()
	started := s.modelCoverageNow()
	view, err := s.readModelCoverageForProof(context.Background(), id, "model-a", "callback")
	if err != nil {
		t.Fatalf("proof read: %v", err)
	}
	t.Logf("refreshed=%v state=%q funding=%v viewErr=%v credMatch=%v wsMatch=%v modelMatch=%v",
		view.refreshedForRead, view.State, view.FundingCheck, view.Error,
		view.CredentialID == id, view.WorkspaceID == workspace, view.ModelID == block.ModelID)
	if view.evidence == nil {
		t.Fatalf("no evidence")
	}
	e := view.evidence
	now := s.modelCoverageNow()
	t.Logf("block observed=%s", block.ObservedAt.Format(timeLayoutDiag))
	t.Logf("started=%s evidence.started=%s generated=%s now=%s",
		started.Format(timeLayoutDiag), e.requestStartedAt.Format(timeLayoutDiag),
		e.generatedAt.Format(timeLayoutDiag), now.Format(timeLayoutDiag))
	t.Logf("afterObs=%v afterStart=%v afterNow=%v afterObs5s=%v age=%s",
		e.requestStartedAt.After(block.ObservedAt), e.requestStartedAt.After(started),
		e.requestStartedAt.After(now), e.generatedAt.After(block.ObservedAt.Add(5*time.Second)), now.Sub(e.generatedAt))
	t.Logf("memberSet=%v credE=%v wsE=%v modelE=%v check=%q",
		e.workspaceMemberID != "", e.credentialID == id, e.workspaceID == workspace, e.modelID == block.ModelID, e.fundingCheck)
	t.Logf("proven=%v", quotaRecoveryProven(view, block, id, workspace, started, now))
}

const timeLayoutDiag = "2006-01-02T15:04:05.000000000Z07:00"
