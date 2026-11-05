package opendesign

import (
	"strings"
	"time"
)

// quotaRecoveryProven checks one validated model preflight, not a balance or a
// timer. Capture recheckStartedAt with modelCoverageNow immediately before the
// recheck's readModelCoverage call, and pass modelCoverageNow() again as now.
// Only the result of this read's refresh (including a joined in-flight refresh)
// can prove recovery; a cache hit or a request started before this recheck cannot.
//
// For multiple model proofs, the caller must also compare each successful
// view.evidence.workspaceMemberID exactly; do not serialize or log it. The
// administrator must configure control and inference keys from the same account:
// a funding preflight cannot verify that these keys belong to the same account.
// The caller must fence the credential, workspace, billing epoch and quota
// revision against concurrent changes before actually clearing a block.
func quotaRecoveryProven(view modelCoverageResponse, block *QuotaBlock, credentialID, workspaceID string, recheckStartedAt, now time.Time) bool {
	if block == nil || block.MultipleModels || strings.TrimSpace(block.ModelID) == "" ||
		block.ObservedAt.IsZero() || credentialID == "" || workspaceID == "" ||
		recheckStartedAt.IsZero() || now.IsZero() || !view.refreshedForRead ||
		view.State != "available" || view.Error != nil || view.FundingCheck == nil || *view.FundingCheck != "available" ||
		view.CredentialID != credentialID || view.WorkspaceID != workspaceID || view.ModelID != block.ModelID {
		return false
	}
	evidence := view.evidence
	if evidence == nil || evidence.credentialID != credentialID || evidence.workspaceID != workspaceID || evidence.modelID != block.ModelID ||
		strings.TrimSpace(evidence.workspaceMemberID) == "" || evidence.fundingCheck != "available" ||
		evidence.requestStartedAt.IsZero() || evidence.generatedAt.IsZero() ||
		!evidence.requestStartedAt.After(block.ObservedAt) || !evidence.requestStartedAt.After(recheckStartedAt) ||
		evidence.requestStartedAt.After(now) || !evidence.generatedAt.After(block.ObservedAt) {
		return false
	}
	age := now.Sub(evidence.generatedAt)
	return age >= -5*time.Second && age <= 30*time.Second
}
