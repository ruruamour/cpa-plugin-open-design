package opendesign

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestModelCoverageProofRequiresOfficialAvailableVerdict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		check   any
		present bool
		want    bool
	}{
		{"available", "available", true, true},
		{"insufficient", "insufficient", true, false},
		{"unknown", "unknown", true, false},
		{"omitted", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, host := newTestService(t)
			quotaTestClock(s)
			s.modelCoverageNow = s.quotaNow
			const workspace, model = "ws-proof", "model-a"
			id := addQuotaCredential(t, s, host, workspace)
			body := officialModelCoverage(t, workspace, model)
			if tc.present {
				body["fundingCheck"] = tc.check
			} else {
				delete(body, "fundingCheck")
			}
			// Wallet funding and modelCovered:false are allowed by the official
			// contract; neither is a model-specific balance or a veto by itself.
			body["funding"], body["modelCovered"] = "wallet", false
			body["codingPlan"].(map[string]any)["windows"] = []any{}
			queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, body)})
			view, err := s.readModelCoverage(id, model, "callback", true)
			if err != nil || view.State != "available" || view.evidence == nil {
				t.Fatalf("valid official response was rejected: %+v, %v", view, err)
			}
			block := &QuotaBlock{ModelID: model, ObservedAt: quotaTestTime.Add(-2 * time.Second)}
			if got := quotaRecoveryProven(view, block, id, workspace, quotaTestTime.Add(-time.Second), s.modelCoverageNow()); got != tc.want {
				t.Fatalf("proof = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestModelCoverageProofSuccessReplacesPreviousEvidence(t *testing.T) {
	s, host := newTestService(t)
	advance := quotaTestClock(s)
	s.modelCoverageNow = s.quotaNow
	const workspace, model = "ws-proof", "model-a"
	id := addQuotaCredential(t, s, host, workspace)
	queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, officialModelCoverage(t, workspace, model))})
	old, err := s.readModelCoverage(id, model, "callback", true)
	if err != nil || old.evidence == nil {
		t.Fatalf("first success: %+v, %v", old, err)
	}
	advance(time.Minute)
	block := &QuotaBlock{ModelID: model, ObservedAt: s.modelCoverageNow().Add(-time.Second)}
	recheck := s.modelCoverageNow()
	advance(time.Second)
	body := officialModelCoverage(t, workspace, model)
	body["generatedAt"] = s.modelCoverageNow().Format(time.RFC3339Nano)
	queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, body)})
	fresh, err := s.readModelCoverage(id, model, "callback", true)
	if err != nil || fresh.evidence == nil || fresh.evidence == old.evidence ||
		!fresh.evidence.requestStartedAt.Equal(s.modelCoverageNow()) || !fresh.evidence.generatedAt.Equal(s.modelCoverageNow()) {
		t.Fatalf("later success did not replace evidence: %+v, %v", fresh, err)
	}
	if quotaRecoveryProven(old, block, id, workspace, recheck, s.modelCoverageNow()) ||
		!quotaRecoveryProven(fresh, block, id, workspace, recheck, s.modelCoverageNow()) {
		t.Fatal("old and fresh successes were not distinguished")
	}
}

func TestModelCoverageProofJoinIsFencedAgainstNewCompletion(t *testing.T) {
	s, host := newTestService(t)
	advance := quotaTestClock(s)
	s.modelCoverageNow = s.quotaNow
	const workspace, model = "ws-proof", "model-a"
	id := addQuotaCredential(t, s, host, workspace)
	key := modelCoverageKey{CredentialID: id, ModelID: model}
	queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, officialModelCoverage(t, workspace, model))})
	if _, err := s.readModelCoverage(id, model, "callback", true); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	entry := s.modelCoverage[key]
	firstDone := entry.completed
	s.mu.RUnlock()
	if firstDone == nil {
		t.Fatal("first completion was not recorded")
	}
	advance(time.Minute)
	body := officialModelCoverage(t, workspace, model)
	body["generatedAt"] = s.modelCoverageNow().Format(time.RFC3339Nano)
	queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, body)})
	if _, err := s.readModelCoverage(id, model, "callback", true); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	secondDone := entry.completed
	s.mu.RUnlock()
	if secondDone == nil || firstDone == secondDone {
		t.Fatal("second completion was not recorded separately")
	}
	view, err := s.joinedModelCoverage(key, entry, firstDone)
	if err != nil || view.refreshedForRead || view.evidence == nil {
		t.Fatalf("old join adopted a different completion: %+v, %v", view, err)
	}
	block := &QuotaBlock{ModelID: model, ObservedAt: quotaTestTime.Add(-time.Second)}
	if quotaRecoveryProven(view, block, id, workspace, quotaTestTime.Add(-time.Second), s.modelCoverageNow()) {
		t.Fatal("another refresh's cached result proved the old join")
	}
	view, err = s.joinedModelCoverage(key, entry, secondDone)
	if err != nil || !view.refreshedForRead {
		t.Fatalf("matching completed join was not recognized: %+v, %v", view, err)
	}
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
		"id": id, "control_key": "new-control-secret",
	}); status != http.StatusOK {
		t.Fatalf("control key mutation failed: %d %s", status, raw)
	}
	view, err = s.joinedModelCoverage(key, entry, secondDone)
	if err != nil || view.refreshedForRead || view.evidence != nil {
		t.Fatalf("invalidated join retained evidence: %+v, %v", view, err)
	}
}

func TestModelCoverageProofInvalidationFencesLateSuccess(t *testing.T) {
	for _, mutation := range []struct {
		name   string
		fields map[string]any
	}{
		{"workspace", map[string]any{"workspace_id": "ws-new"}},
		{"control key", map[string]any{"control_key": "new-control-secret"}},
		{"clear billing", map[string]any{"clear_billing": true}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			s, host := newTestService(t)
			quotaTestClock(s)
			s.modelCoverageNow = s.quotaNow
			const workspace, model = "ws-proof", "model-a"
			id := addQuotaCredential(t, s, host, workspace)
			queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, officialModelCoverage(t, workspace, model))})
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			s.SetHost(func(method string, in, out any) error {
				if method == "host.http.do" {
					close(started)
					<-release
				}
				return host.call(method, in, out)
			})
			done := make(chan coverageResult, 1)
			go func() {
				view, err := s.readModelCoverage(id, model, "callback", true)
				done <- coverageResult{view, err}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("HTTP did not start")
			}
			fields := map[string]any{"id": id}
			for key, value := range mutation.fields {
				fields[key] = value
			}
			if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, fields); status != http.StatusOK {
				t.Fatalf("mutation failed: %d %s", status, raw)
			}
			releaseOnce.Do(func() { close(release) })
			view := receiveCoverage(t, done)
			if view.evidence != nil || view.FundingCheck != nil || view.State == "available" {
				t.Fatalf("obsolete result retained recovery evidence: %+v", view)
			}
			block := &QuotaBlock{ModelID: model, ObservedAt: quotaTestTime.Add(-2 * time.Second)}
			if quotaRecoveryProven(view, block, id, workspace, quotaTestTime.Add(-time.Second), s.modelCoverageNow()) {
				t.Fatal("obsolete result proved recovery")
			}
		})
	}
}

func TestModelCoverageProofOperationOpenFailureHasNoHTTPStart(t *testing.T) {
	s, host := newTestService(t)
	id := addQuotaCredential(t, s, host, "ws-proof")
	s.SetHost(func(method string, in, out any) error {
		if method == "host.http.operation_open" {
			return errors.New("open failed")
		}
		return host.call(method, in, out)
	})
	view, err := s.readModelCoverage(id, "model-a", "callback", true)
	if err != nil || view.Error == nil || view.State != "error" || view.evidence != nil || host.countMethod("host.http.do") != 0 {
		t.Fatalf("failed operation open fabricated evidence: %+v, %v", view, err)
	}
}
