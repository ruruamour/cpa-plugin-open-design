package opendesign

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type coverageResult struct {
	view modelCoverageResponse
	err  error
}

func waitCoverageAttempt(t *testing.T, s *Service, key modelCoverageKey) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		entry := s.modelCoverage[key]
		started := entry != nil && entry.inflight != nil && !entry.lastAttempt.IsZero()
		s.mu.RUnlock()
		if started {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("model coverage did not enter the slot queue")
}

func receiveCoverage(t *testing.T, done <-chan coverageResult) modelCoverageResponse {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.view
	case <-time.After(5 * time.Second):
		t.Fatal("model coverage did not finish")
		return modelCoverageResponse{}
	}
}

func TestModelCoverageProofUsesActualHostStartAfterQueueAndOperationOpen(t *testing.T) {
	s, host := newTestService(t)
	advance := quotaTestClock(s)
	s.modelCoverageNow = s.quotaNow
	const workspace, model = "ws-proof", "model/A"
	id := addQuotaCredential(t, s, host, workspace)

	// The attempt is queued before the block. operation_open is also held across
	// the recheck: neither the throttle time nor open is the HTTP start time.
	for range cap(s.modelCoverageSlots) {
		s.modelCoverageSlots <- struct{}{}
	}
	defer func() {
		for range cap(s.modelCoverageSlots) - 1 {
			<-s.modelCoverageSlots
		}
	}()
	opened, releaseOpen := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseOpen) })
	generated := quotaTestTime.Add(3750 * time.Millisecond)
	body := officialModelCoverage(t, workspace, model)
	body["workspaceMemberId"] = " private-member-do-not-expose "
	body["generatedAt"] = generated.In(time.FixedZone("upstream", 2*60*60)).Format(time.RFC3339Nano)
	queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, body)})
	actualHostStart := make(chan time.Time, 1)
	s.SetHost(func(method string, in, out any) error {
		if method == "host.http.operation_open" {
			close(opened)
			<-releaseOpen
		}
		if method == "host.http.do" {
			actualHostStart <- s.modelCoverageNow()
			advance(2 * time.Second)
		}
		return host.call(method, in, out)
	})
	done := make(chan coverageResult, 1)
	go func() {
		view, err := s.readModelCoverage(id, model, "callback", true)
		done <- coverageResult{view, err}
	}()
	waitCoverageAttempt(t, s, modelCoverageKey{CredentialID: id, ModelID: model})
	advance(time.Second)
	block := &QuotaBlock{ModelID: model, ObservedAt: s.modelCoverageNow()}
	advance(time.Second)
	recheck := s.modelCoverageNow()
	<-s.modelCoverageSlots
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("operation_open did not start")
	}
	if host.countMethod("host.http.do") != 0 {
		t.Fatal("HTTP started before operation_open completed")
	}
	// The recheck joins while operation_open is parked. Signal its first clock
	// read under mu so the fetch cannot publish before the join is registered.
	clock := s.modelCoverageNow
	joinedRead := make(chan struct{})
	var joinedOnce sync.Once
	s.modelCoverageNow = func() time.Time {
		joinedOnce.Do(func() { close(joinedRead) })
		return clock()
	}
	joined := make(chan coverageResult, 1)
	go func() {
		view, err := s.readModelCoverage(id, model, "callback", true)
		joined <- coverageResult{view, err}
	}()
	select {
	case <-joinedRead:
	case <-time.After(5 * time.Second):
		t.Fatal("recheck did not join the pending refresh")
	}
	advance(1500 * time.Millisecond)
	expectedStart := s.modelCoverageNow()
	releaseOnce.Do(func() { close(releaseOpen) })
	view := receiveCoverage(t, done)
	joinedView := receiveCoverage(t, joined)
	hostStart := <-actualHostStart
	for _, result := range []modelCoverageResponse{view, joinedView} {
		if !result.refreshedForRead || result.evidence == nil || result.evidence.workspaceMemberID != " private-member-do-not-expose " ||
			!result.evidence.requestStartedAt.Equal(expectedStart) || !hostStart.Equal(expectedStart) ||
			!result.evidence.generatedAt.Equal(generated) || result.FetchedAt == nil || !result.FetchedAt.Equal(expectedStart.Add(2*time.Second)) {
			t.Fatal("wrong member, HTTP start, generatedAt or completion evidence")
		}
		if !quotaRecoveryProven(result, block, id, workspace, recheck, s.modelCoverageNow()) {
			t.Fatal("a request actually started after the block and recheck did not prove recovery")
		}
	}

	// A subsequent refresh is a throttled cache hit, even though its cached
	// available value is very recent. A new recheck must not reuse that success.
	advance(time.Nanosecond)
	cachedRecheck := s.modelCoverageNow()
	for _, refresh := range []bool{false, true} {
		cached, err := s.readModelCoverage(id, model, "callback", refresh)
		if err != nil || cached.refreshedForRead || cached.evidence != view.evidence || host.countMethod("host.http.do") != 1 {
			t.Fatalf("unexpected cached result: %+v, %v", cached, err)
		}
		if quotaRecoveryProven(cached, block, id, workspace, cachedRecheck, s.modelCoverageNow()) ||
			quotaRecoveryProven(cached, block, id, workspace, recheck, s.modelCoverageNow()) {
			t.Fatal("a cache hit proved recovery, even though the HTTP started after the recheck")
		}
	}
	_, _, raw := modelCoverageCall(t, s, id, model, false)
	for _, secret := range []string{"private-member-do-not-expose", "workspaceMemberId", "quota-control-secret", "sk-quota-inference-secret", "generatedAt", "generated_at", "requestStartedAt", "request_started_at", "evidence"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("management exposed internal evidence or secret %q: %s", secret, raw)
		}
	}
}

func TestModelCoverageProofRejectsRequestStartedBeforePauseAndRecheck(t *testing.T) {
	s, host := newTestService(t)
	advance := quotaTestClock(s)
	s.modelCoverageNow = s.quotaNow
	const workspace, model = "ws-proof", "model-a"
	id := addQuotaCredential(t, s, host, workspace)
	body := officialModelCoverage(t, workspace, model)
	body["generatedAt"] = quotaTestTime.Add(3 * time.Second).Format(time.RFC3339Nano)
	queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, body)})
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
	first := make(chan coverageResult, 1)
	go func() {
		view, err := s.readModelCoverage(id, model, "callback", true)
		first <- coverageResult{view, err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP did not start")
	}
	advance(time.Second)
	block := &QuotaBlock{ModelID: model, ObservedAt: s.modelCoverageNow()}
	advance(time.Second)
	recheck := s.modelCoverageNow()
	second := make(chan coverageResult, 1)
	go func() {
		view, err := s.readModelCoverage(id, model, "callback", true)
		second <- coverageResult{view, err}
	}()
	advance(2 * time.Second)
	releaseOnce.Do(func() { close(release) })
	for _, result := range []modelCoverageResponse{receiveCoverage(t, first), receiveCoverage(t, second)} {
		if result.evidence == nil || !result.evidence.requestStartedAt.Equal(quotaTestTime) ||
			result.FetchedAt == nil || !result.FetchedAt.After(block.ObservedAt) ||
			!result.evidence.generatedAt.After(block.ObservedAt) {
			t.Fatalf("wrong request evidence: %+v", result)
		}
		if quotaRecoveryProven(result, block, id, workspace, recheck, s.modelCoverageNow()) {
			t.Fatal("a request started before the pause/recheck proved recovery")
		}
	}
	if host.countMethod("host.http.do") != 1 {
		t.Fatal("concurrent/cached reads issued an extra HTTP request")
	}
}

func TestModelCoverageProofFailureKeepsOnlyLastSuccessfulEvidence(t *testing.T) {
	failures := []struct {
		name string
		resp fakeHTTPResponse
		kind string
	}{
		{"unauthorized", fakeHTTPResponse{Status: http.StatusUnauthorized}, "unauthorized"},
		{"server error", fakeHTTPResponse{Status: http.StatusInternalServerError}, "upstream"},
		{"network", fakeHTTPResponse{Err: context.DeadlineExceeded}, "timeout"},
		{"invalid body", fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{}`)}, "malformed"},
		{"oversized body", fakeHTTPResponse{Status: http.StatusOK, Body: []byte(strings.Repeat(" ", maxQuotaBody+1))}, "malformed"},
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			for _, previouslySucceeded := range []bool{false, true} {
				s, host := newTestService(t)
				advance := quotaTestClock(s)
				s.modelCoverageNow = s.quotaNow
				const workspace, model = "ws-proof", "model-a"
				id := addQuotaCredential(t, s, host, workspace)
				var previous modelCoverageResponse
				if previouslySucceeded {
					body := officialModelCoverage(t, workspace, model)
					// Within the freshness window at the later failure, to ensure
					// the stale status itself cannot be mistaken for proof.
					body["generatedAt"] = quotaTestTime.Add(time.Minute).Format(time.RFC3339Nano)
					queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, body)})
					var err error
					previous, err = s.readModelCoverage(id, model, "callback", true)
					if err != nil || previous.evidence == nil {
						t.Fatalf("initial success: %+v, %v", previous, err)
					}
					advance(time.Minute)
				}
				queueQuota(host, failure.resp)
				recheck := s.modelCoverageNow().Add(-time.Nanosecond)
				view, err := s.readModelCoverage(id, model, "callback", true)
				if err != nil || !view.refreshedForRead || view.Error == nil || view.Error.Kind != failure.kind {
					t.Fatalf("unexpected failure: %+v, %v", view, err)
				}
				block := &QuotaBlock{ModelID: model, ObservedAt: quotaTestTime.Add(-2 * time.Second)}
				if quotaRecoveryProven(view, block, id, workspace, recheck, s.modelCoverageNow()) {
					t.Fatal("failed request proved recovery")
				}
				if previouslySucceeded {
					if view.State != "stale" || view.evidence != previous.evidence || !view.FetchedAt.Equal(*previous.FetchedAt) {
						t.Fatalf("failure changed last successful evidence: %+v", view)
					}
					// Even if the old generatedAt is fresh and the old successful
					// HTTP started after an earlier recheck, stale is never proof.
					if quotaRecoveryProven(view, block, id, workspace, quotaTestTime.Add(-time.Second), s.modelCoverageNow()) {
						t.Fatal("stale success was accepted as proof")
					}
					// Verify this case really would satisfy the other constraints.
					optimistic := view
					optimistic.State, optimistic.Error = "available", nil
					if !quotaRecoveryProven(optimistic, block, id, workspace, quotaTestTime.Add(-time.Second), s.modelCoverageNow()) {
						t.Fatal("fixture did not isolate stale status and error")
					}
				} else if view.State != "error" || view.evidence != nil || view.FetchedAt != nil {
					t.Fatalf("first failure fabricated evidence: %+v", view)
				}
			}
		})
	}
}

func TestModelCoverageProofQueueTimeoutNeverCreatesEvidence(t *testing.T) {
	s, host := newTestService(t)
	s.modelCoverageBudget = 20 * time.Millisecond
	id := addQuotaCredential(t, s, host, "ws-proof")
	for range cap(s.modelCoverageSlots) {
		s.modelCoverageSlots <- struct{}{}
	}
	defer func() {
		for range cap(s.modelCoverageSlots) {
			<-s.modelCoverageSlots
		}
	}()
	view, err := s.readModelCoverage(id, "model-a", "callback", true)
	if err != nil || view.State != "error" || view.Error == nil || view.Error.Kind != "timeout" || view.evidence != nil ||
		host.countMethod("host.http.operation_open") != 0 || host.countMethod("host.http.do") != 0 {
		t.Fatalf("queue timeout fabricated evidence or sent HTTP: %+v, %v", view, err)
	}
}

func TestModelCoverageProofRequiresWholeOfficialContract(t *testing.T) {
	tests := []struct {
		name   string
		change func(map[string]any)
	}{
		{"wrong workspace", func(m map[string]any) { m["workspaceId"] = "other" }},
		{"wrong model", func(m map[string]any) { m["modelId"] = "other" }},
		{"missing model", func(m map[string]any) { delete(m, "modelId") }},
		{"missing member", func(m map[string]any) { delete(m, "workspaceMemberId") }},
		{"blank member", func(m map[string]any) { m["workspaceMemberId"] = " \t" }},
		{"missing generatedAt", func(m map[string]any) { delete(m, "generatedAt") }},
		{"invalid generatedAt", func(m map[string]any) { m["generatedAt"] = "bad" }},
		{"null generatedAt", func(m map[string]any) { m["generatedAt"] = nil }},
		{"plan workspace", func(m map[string]any) { m["codingPlan"].(map[string]any)["workspaceId"] = "other" }},
		{"invalid plan", func(m map[string]any) { m["codingPlan"].(map[string]any)["eligible"] = "true" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, host := newTestService(t)
			id := addQuotaCredential(t, s, host, "ws-proof")
			body := officialModelCoverage(t, "ws-proof", "model-a")
			tt.change(body)
			queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, body)})
			view, err := s.readModelCoverage(id, "model-a", "callback", true)
			if err != nil || view.State != "error" || view.Error == nil || view.Error.Kind != "malformed" || view.evidence != nil {
				t.Fatalf("invalid response accepted: %+v, %v", view, err)
			}
		})
	}
}

func TestQuotaFundingGeneratedAtIsInternalAndValidated(t *testing.T) {
	const timestamp = "2026-11-03T03:00:00.123456789+03:00"
	for _, modelScoped := range []bool{false, true} {
		body := officialQuotaFunding(t, "ws-proof")
		if modelScoped {
			body["modelId"] = "model-a"
		}
		body["generatedAt"] = timestamp
		raw := mustJSON(t, body)
		var data *quotaFundingData
		var ok bool
		if modelScoped {
			data, ok = parseModelCoverageFunding(raw, "ws-proof", "model-a")
		} else {
			data, ok = parseQuotaFunding(raw, "ws-proof")
		}
		want, _ := time.Parse(time.RFC3339Nano, timestamp)
		if !ok || !data.generatedAt.Equal(want) || data.workspaceMemberID != "private-member-do-not-expose" {
			t.Fatal("official generatedAt/member were not retained exactly")
		}
		public, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{timestamp, "generatedAt", "generated_at", "workspaceMemberId", "private-member-do-not-expose", "quota-control-secret"} {
			if strings.Contains(string(public), secret) {
				t.Fatalf("funding JSON leaked %q: %s", secret, public)
			}
		}
	}
}
