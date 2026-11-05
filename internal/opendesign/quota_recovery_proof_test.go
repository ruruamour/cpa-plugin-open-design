package opendesign

import (
	"encoding/json"
	"testing"
	"time"
)

type recoveryProofInput struct {
	view             modelCoverageResponse
	block            *QuotaBlock
	credentialID     string
	workspaceID      string
	recheckStartedAt time.Time
	now              time.Time
}

func recoveryProofFixture() recoveryProofInput {
	now := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
	check := "available"
	return recoveryProofInput{
		view: modelCoverageResponse{
			CredentialID: "credential", WorkspaceID: "workspace", ModelID: "model-A",
			State: "available", FundingCheck: &check, refreshedForRead: true,
			evidence: &modelCoverageEvidence{
				credentialID: "credential", workspaceID: "workspace", workspaceMemberID: "member", modelID: "model-A", fundingCheck: "available",
				requestStartedAt: now.Add(-10 * time.Second), generatedAt: now.Add(-5 * time.Second),
			},
		},
		block:        &QuotaBlock{ModelID: "model-A", ObservedAt: now.Add(-40 * time.Second)},
		credentialID: "credential", workspaceID: "workspace",
		recheckStartedAt: now.Add(-20 * time.Second), now: now,
	}
}

func (p recoveryProofInput) proven() bool {
	return quotaRecoveryProven(p.view, p.block, p.credentialID, p.workspaceID, p.recheckStartedAt, p.now)
}

func TestQuotaRecoveryProofStrictConditions(t *testing.T) {
	setFundingCheck := func(value string) func(*recoveryProofInput) {
		return func(p *recoveryProofInput) { p.view.FundingCheck = &value }
	}
	tests := []struct {
		name   string
		change func(*recoveryProofInput)
		want   bool
	}{
		{"valid", func(*recoveryProofInput) {}, true},
		{"missing block", func(p *recoveryProofInput) { p.block = nil }, false},
		{"multiple blocked models", func(p *recoveryProofInput) { p.block.MultipleModels = true }, false},
		{"empty model", func(p *recoveryProofInput) { p.block.ModelID = "" }, false},
		{"blank model", func(p *recoveryProofInput) { p.block.ModelID = " \t" }, false},
		{"model is case sensitive", func(p *recoveryProofInput) { p.view.ModelID = "model-a" }, false},
		{"model is not trimmed", func(p *recoveryProofInput) { p.view.ModelID = " model-A" }, false},
		{"evidence from another model", func(p *recoveryProofInput) { p.view.evidence.modelID = "model-B" }, false},
		{"missing credential", func(p *recoveryProofInput) { p.credentialID = "" }, false},
		{"wrong credential view", func(p *recoveryProofInput) { p.view.CredentialID = "other" }, false},
		{"wrong credential evidence", func(p *recoveryProofInput) { p.view.evidence.credentialID = "other" }, false},
		{"missing workspace", func(p *recoveryProofInput) { p.workspaceID = "" }, false},
		{"wrong workspace view", func(p *recoveryProofInput) { p.view.WorkspaceID = "other" }, false},
		{"wrong workspace evidence", func(p *recoveryProofInput) { p.view.evidence.workspaceID = "other" }, false},
		{"missing member evidence", func(p *recoveryProofInput) { p.view.evidence.workspaceMemberID = "" }, false},
		{"blank member evidence", func(p *recoveryProofInput) { p.view.evidence.workspaceMemberID = " \t" }, false},
		{"stale state", func(p *recoveryProofInput) { p.view.State = "stale" }, false},
		{"error state", func(p *recoveryProofInput) { p.view.State = "error" }, false},
		{"needs refresh", func(p *recoveryProofInput) { p.view.State = "needs_refresh" }, false},
		{"cache hit", func(p *recoveryProofInput) { p.view.refreshedForRead = false }, false},
		{"error even if available", func(p *recoveryProofInput) { p.view.Error = &quotaError{Kind: "timeout"} }, false},
		{"missing funding check", func(p *recoveryProofInput) { p.view.FundingCheck = nil }, false},
		{"insufficient funding", setFundingCheck("insufficient"), false},
		{"unknown funding", setFundingCheck("unknown"), false},
		{"original funding was insufficient", func(p *recoveryProofInput) { p.view.evidence.fundingCheck = "insufficient" }, false},
		{"original funding was absent", func(p *recoveryProofInput) { p.view.evidence.fundingCheck = "" }, false},
		{"modelCovered false is not a balance", func(p *recoveryProofInput) { value := false; p.view.ModelCovered = &value }, true},
		{"no fetchedAt required as proof", func(p *recoveryProofInput) { p.view.FetchedAt = nil }, true},
		{"missing evidence", func(p *recoveryProofInput) { p.view.evidence = nil }, false},
		{"missing block observation", func(p *recoveryProofInput) { p.block.ObservedAt = time.Time{} }, false},
		{"missing recheck start", func(p *recoveryProofInput) { p.recheckStartedAt = time.Time{} }, false},
		{"missing now", func(p *recoveryProofInput) { p.now = time.Time{} }, false},
		{"missing request start", func(p *recoveryProofInput) { p.view.evidence.requestStartedAt = time.Time{} }, false},
		{"request before block", func(p *recoveryProofInput) {
			p.view.evidence.requestStartedAt = p.block.ObservedAt.Add(-time.Nanosecond)
		}, false},
		{"request at block", func(p *recoveryProofInput) { p.view.evidence.requestStartedAt = p.block.ObservedAt }, false},
		{"request before recheck", func(p *recoveryProofInput) {
			p.view.evidence.requestStartedAt = p.recheckStartedAt.Add(-time.Nanosecond)
		}, false},
		{"request at recheck", func(p *recoveryProofInput) { p.view.evidence.requestStartedAt = p.recheckStartedAt }, false},
		{"request after now", func(p *recoveryProofInput) { p.view.evidence.requestStartedAt = p.now.Add(time.Nanosecond) }, false},
		{"missing generatedAt", func(p *recoveryProofInput) { p.view.evidence.generatedAt = time.Time{} }, false},
		{"generated before observation", func(p *recoveryProofInput) { p.view.evidence.generatedAt = p.block.ObservedAt.Add(-time.Nanosecond) }, false},
		{"generated at observation", func(p *recoveryProofInput) { p.view.evidence.generatedAt = p.block.ObservedAt }, false},
		{"generated just after observation", func(p *recoveryProofInput) {
			p.block.ObservedAt = p.now.Add(-20 * time.Second)
			p.view.evidence.generatedAt = p.block.ObservedAt.Add(time.Nanosecond)
		}, true},
		{"30 seconds old inclusive", func(p *recoveryProofInput) { p.view.evidence.generatedAt = p.now.Add(-30 * time.Second) }, true},
		{"too old by one nanosecond", func(p *recoveryProofInput) {
			p.view.evidence.generatedAt = p.now.Add(-30*time.Second - time.Nanosecond)
		}, false},
		{"5 seconds future inclusive", func(p *recoveryProofInput) { p.view.evidence.generatedAt = p.now.Add(5 * time.Second) }, true},
		{"too future by one nanosecond", func(p *recoveryProofInput) { p.view.evidence.generatedAt = p.now.Add(5*time.Second + time.Nanosecond) }, false},
		{"generated before request can still be recent", func(p *recoveryProofInput) {
			p.view.evidence.generatedAt = p.view.evidence.requestStartedAt.Add(-time.Nanosecond)
		}, true},
		{"request at now", func(p *recoveryProofInput) { p.view.evidence.requestStartedAt = p.now }, true},
		{"request just after both starts", func(p *recoveryProofInput) {
			p.view.evidence.requestStartedAt = p.recheckStartedAt.Add(time.Nanosecond)
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := recoveryProofFixture()
			tt.change(&p)
			if got := p.proven(); got != tt.want {
				t.Fatalf("proof = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestQuotaRecoveryProofIsNotSerialized(t *testing.T) {
	p := recoveryProofFixture()
	if !p.proven() {
		t.Fatal("fixture was not proven")
	}
	raw, err := json.Marshal(p.view)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 11 {
		t.Fatalf("management fields unexpectedly changed: %s", raw)
	}
	// Unmarshal into a fresh value, as an existing object retains private fields.
	var decoded modelCoverageResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	p.view = decoded
	if p.view.evidence != nil || p.proven() {
		t.Fatal("a management JSON round trip created recovery evidence")
	}
}
