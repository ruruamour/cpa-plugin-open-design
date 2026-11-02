package opendesign

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func modelCoverageCall(t *testing.T, s *Service, id, model string, refresh bool) (int, modelCoverageResponse, string) {
	t.Helper()
	query := map[string][]string{"credential_id": {id}, "model_id": {model}}
	if refresh {
		query["refresh"] = []string{"1"}
	}
	status, _, raw := managementCall(t, s, http.MethodGet, apiBase+"/model-coverage", query, nil)
	var view modelCoverageResponse
	if status == http.StatusOK {
		if err := json.Unmarshal([]byte(raw), &view); err != nil {
			t.Fatal(err)
		}
	}
	return status, view, raw
}

func officialModelCoverage(t *testing.T, workspace, model string) map[string]any {
	t.Helper()
	return map[string]any{
		"schemaVersion": 1, "workspaceId": workspace, "workspaceMemberId": "private-member-do-not-expose",
		"modelId": model, "generatedAt": quotaTestTime.Format("2006-01-02T15:04:05Z"), "balanceUsd": "-0.125",
		"modelCovered": true, "funding": "coding_plan", "fundingCheck": "available",
		"codingPlan": map[string]any{
			"workspaceId": workspace, "generatedAt": quotaTestTime.Format("2006-01-02T15:04:05Z"), "eligible": true, "tier": "plus",
			"windows": []any{map[string]any{
				"policyId": "p1", "durationSeconds": 3600, "resetMode": "anchored_recurring",
				"usedCredits": "100", "limitCredits": "1000", "remainingCredits": "900",
				"windowStart": quotaTestTime.Format("2006-01-02T15:04:05Z"), "resetsAt": quotaTestTime.Add(3600 * 1e9).Format("2006-01-02T15:04:05Z"),
			}},
		},
	}
}

func TestModelCoverageRouteAndOfficialModelScopedRequest(t *testing.T) {
	s, host := newTestService(t)
	id := addQuotaCredential(t, s, host, "ws-coverage")
	queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, officialModelCoverage(t, "ws-coverage", "model-a"))})
	status, view, raw := modelCoverageCall(t, s, id, "model-a", true)
	if status != http.StatusOK || view.State != "available" || view.ModelID != "model-a" || view.ModelCovered == nil || !*view.ModelCovered || view.FundingCheck == nil || *view.FundingCheck != "available" {
		t.Fatalf("coverage response: %d %s", status, raw)
	}
	if view.Funding == nil || *view.Funding != "coding_plan" || view.FetchedAt == nil || view.NextRefreshAt == nil {
		t.Fatalf("coverage metadata: %s", raw)
	}
	if strings.Contains(raw, "private-member-do-not-expose") || strings.Contains(raw, "quota-control-secret") || strings.Contains(raw, "sk-quota-inference-secret") {
		t.Fatalf("coverage response leaked an internal value: %s", raw)
	}
	payload := host.lastPayload("host.http.do")
	address, _ := payload["url"].(string)
	parsed, err := url.Parse(address)
	if err != nil || parsed.Path != "/api/v1/workspaces/ws-coverage/billing/funding-check" || parsed.Query().Get("modelId") != "model-a" {
		t.Fatalf("coverage URL = %q", address)
	}
	if strings.Contains(address, "wallet") || payload["method"] != http.MethodGet {
		t.Fatalf("coverage used the wrong upstream request: %v", payload)
	}
	if got := payload["headers"].(http.Header).Get("Authorization"); got != "Bearer quota-control-secret" {
		t.Fatalf("coverage authorization = %q", got)
	}
	if host.countMethod("host.http.do") != 1 {
		t.Fatalf("coverage request count = %d", host.countMethod("host.http.do"))
	}
	// A coverage refresh does not manufacture a shared quota snapshot or issue a
	// wallet/funding model-less read.
	if status, quota, _ := quotaCall(t, s, id, false); status != http.StatusOK || quota.State != "needs_refresh" || quota.Wallet.Data != nil || quota.Funding.Data != nil {
		t.Fatalf("coverage polluted quota state: %+v", quota)
	}
	registered := callRPC(t, s, "management.register", nil).(map[string]any)["routes"].([]map[string]string)
	found := false
	for _, route := range registered {
		if route["Method"] == http.MethodGet && route["Path"] == apiBase+"/model-coverage" {
			found = true
		}
	}
	if !found {
		t.Fatal("model coverage route was not registered")
	}
}

func TestModelCoverageIsModelKeyedAndRefreshIsThrottled(t *testing.T) {
	s, host := newTestService(t)
	advance := quotaTestClock(s)
	s.modelCoverageNow = s.quotaNow
	id := addQuotaCredential(t, s, host, "ws-coverage")
	queueQuota(host,
		fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, officialModelCoverage(t, "ws-coverage", "model-a"))},
		fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, officialModelCoverage(t, "ws-coverage", "model-b"))},
	)
	if _, first, _ := modelCoverageCall(t, s, id, "model-a", true); first.State != "available" {
		t.Fatal("model-a coverage did not succeed")
	}
	if _, cached, _ := modelCoverageCall(t, s, id, "model-a", false); cached.State != "available" || cached.ModelID != "model-a" {
		t.Fatal("model-a cache was not returned")
	}
	if _, second, _ := modelCoverageCall(t, s, id, "model-b", true); second.State != "available" || second.ModelID != "model-b" {
		t.Fatal("model-b coverage did not use its own cache key")
	}
	if host.countMethod("host.http.do") != 2 {
		t.Fatalf("model-scoped request count = %d", host.countMethod("host.http.do"))
	}
	if _, _, _ = modelCoverageCall(t, s, id, "model-b", true); host.countMethod("host.http.do") != 2 {
		t.Fatal("model coverage ignored the one-minute refresh throttle")
	}
	advance(modelCoverageRefreshInterval)
	queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, officialModelCoverage(t, "ws-coverage", "model-b"))})
	if _, refreshed, _ := modelCoverageCall(t, s, id, "model-b", true); refreshed.State != "available" || host.countMethod("host.http.do") != 3 {
		t.Fatal("model coverage did not refresh at the interval")
	}
}

func TestModelCoverageRejectsMismatchedOfficialContext(t *testing.T) {
	s, host := newTestService(t)
	advance := quotaTestClock(s)
	s.modelCoverageNow = s.quotaNow
	id := addQuotaCredential(t, s, host, "ws-coverage")
	queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, officialModelCoverage(t, "ws-other", "model-a"))})
	status, view, raw := modelCoverageCall(t, s, id, "model-a", true)
	if status != http.StatusOK || view.State != "error" || view.Error == nil || view.Error.Kind != "malformed" || view.ModelCovered != nil || view.FundingCheck != nil {
		t.Fatalf("mismatched context was accepted: %d %s", status, raw)
	}
	queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, officialModelCoverage(t, "ws-coverage", "model-other"))})
	advance(modelCoverageRefreshInterval)
	status, view, raw = modelCoverageCall(t, s, id, "model-a", true)
	if status != http.StatusOK || view.State != "error" || view.Error == nil || view.Error.Kind != "malformed" || view.ModelCovered != nil {
		t.Fatalf("mismatched model was accepted: %d %s", status, raw)
	}
	if host.countMethod("host.http.do") != 2 {
		t.Fatalf("mismatched context did not perform two official requests: %d", host.countMethod("host.http.do"))
	}
}

func TestModelCoverageMutationFencesLateResponse(t *testing.T) {
	s, host := newTestService(t)
	id := addQuotaCredential(t, s, host, "ws-old")
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	s.SetHost(func(method string, in, out any) error {
		if method != "host.http.do" {
			return host.call(method, in, out)
		}
		if calls.Add(1) == 1 {
			close(started)
			<-release
			assign(out, map[string]any{"StatusCode": http.StatusOK, "Body": mustJSON(t, officialModelCoverage(t, "ws-old", "model-a"))})
			return nil
		}
		assign(out, map[string]any{"StatusCode": http.StatusOK, "Body": mustJSON(t, officialModelCoverage(t, "ws-new", "model-a"))})
		return nil
	})
	late := make(chan modelCoverageResponse, 1)
	go func() {
		_, view, _ := modelCoverageCall(t, s, id, "model-a", true)
		late <- view
	}()
	<-started
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "workspace_id": "ws-new"}); status != http.StatusOK {
		t.Fatalf("workspace mutation: %d %s", status, raw)
	}
	if _, view, _ := modelCoverageCall(t, s, id, "model-a", false); view.State != "needs_refresh" || view.ModelCovered != nil {
		t.Fatal("workspace mutation retained the old coverage cache")
	}
	close(release)
	got := <-late
	if got.State != "needs_refresh" || got.ModelCovered != nil || got.WorkspaceID != "ws-new" {
		t.Fatalf("late coverage response was exposed: %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("unexpected post-mutation upstream calls: %d", calls.Load())
	}
	if status, view, raw := modelCoverageCall(t, s, id, "model-a", true); status != http.StatusOK || view.State != "available" || view.WorkspaceID != "ws-new" || view.ModelCovered == nil || !*view.ModelCovered || strings.Contains(raw, "ws-old") {
		t.Fatalf("new coverage refresh failed: %d %s", status, raw)
	}
}

func TestModelCoverageRequiresBothControlAndWorkspaceWithoutNetwork(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "inference-only", "sk-model-coverage-only")
	for _, model := range []string{"model-a", "model-b"} {
		status, view, raw := modelCoverageCall(t, s, id, model, true)
		if status != http.StatusOK || view.State != "not_configured" || view.ModelCovered != nil || view.Error != nil {
			t.Fatalf("unconfigured coverage: %d %s", status, raw)
		}
	}
	if host.countMethod("host.http.do") != 0 {
		t.Fatal("unconfigured model coverage performed network I/O")
	}
}

// A cache-only read and a control-only account must never allocate a cache
// entry: misses are stateless so ordinary GETs cannot grow the map.
func TestModelCoverageMissAndControlOnlyNeverCreateEntries(t *testing.T) {
	s, host := newTestService(t)
	id := addQuotaCredential(t, s, host, "ws-coverage")
	if status, view, raw := modelCoverageCall(t, s, id, "model-a", false); status != http.StatusOK || view.State != "needs_refresh" || view.ModelCovered != nil {
		t.Fatalf("cache-only miss: %d %s", status, raw)
	}
	controlOnly := addCredential(t, s, host, "control-only", "sk-control-only-secret")
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
		"id": controlOnly, "control_key": "quota-control-secret",
	}); status != http.StatusOK {
		t.Fatalf("configure control key: %d %s", status, raw)
	}
	for _, refresh := range []bool{false, true} {
		if status, view, raw := modelCoverageCall(t, s, controlOnly, "model-a", refresh); status != http.StatusOK || view.State != "not_configured" || view.ModelCovered != nil || view.Error != nil {
			t.Fatalf("control-only coverage: %d %s", status, raw)
		}
	}
	if host.countMethod("host.http.do") != 0 {
		t.Fatal("miss/control-only coverage performed network I/O")
	}
	s.mu.Lock()
	entries := len(s.modelCoverage)
	s.mu.Unlock()
	if entries != 0 {
		t.Fatalf("stateless reads allocated %d cache entries", entries)
	}
}

// Explicit refreshes are cached, but the cache is bounded and evicts the least
// recently used idle entry rather than growing without limit.
func TestModelCoverageEvictionPreservesRecentThrottle(t *testing.T) {
	s, host := newTestService(t)
	id := addQuotaCredential(t, s, host, "ws-coverage")
	now := time.Unix(1_800_000_000, 0)
	s.modelCoverageNow = func() time.Time { return now }
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < modelCoverageCacheCapacity; i++ {
		key := modelCoverageKey{CredentialID: id, ModelID: "idle-" + strconv.Itoa(i)}
		s.modelCoverage[key] = &modelCoverageEntry{
			epoch:       s.billingEpoch[id],
			lastAccess:  now.Add(-time.Duration(i+1) * time.Hour),
			invalidated: make(chan struct{}),
		}
	}
	recentKey := modelCoverageKey{CredentialID: id, ModelID: "idle-" + strconv.Itoa(modelCoverageCacheCapacity-1)}
	s.modelCoverage[recentKey].lastAttempt = now.Add(-time.Second)
	if _, err := s.modelCoverageEntryLocked(modelCoverageKey{CredentialID: id, ModelID: "new"}); err != nil {
		t.Fatalf("recently attempted entry made the cache unavailable: %v", err)
	}
	if _, ok := s.modelCoverage[recentKey]; !ok {
		t.Fatal("eviction removed an entry still inside its refresh throttle window")
	}
	if len(s.modelCoverage) != modelCoverageCacheCapacity {
		t.Fatalf("cache size after eviction = %d", len(s.modelCoverage))
	}
}
func TestModelCoverageCacheIsBoundedAndKeepsInflightEntries(t *testing.T) {
	s, host := newTestService(t)
	advance := quotaTestClock(s)
	s.modelCoverageNow = s.quotaNow
	id := addQuotaCredential(t, s, host, "ws-coverage")
	model := func(i int) string { return "model-" + strconv.Itoa(i) }
	for i := 0; i < modelCoverageCacheCapacity+4; i++ {
		queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, officialModelCoverage(t, "ws-coverage", model(i)))})
		if status, view, raw := modelCoverageCall(t, s, id, model(i), true); status != http.StatusOK || view.State != "available" {
			t.Fatalf("refresh %d: %d %s", i, status, raw)
		}
		advance(time.Second)
	}
	s.mu.Lock()
	size := len(s.modelCoverage)
	s.mu.Unlock()
	if size > modelCoverageCacheCapacity {
		t.Fatalf("cache grew to %d entries", size)
	}
	last := modelCoverageKey{CredentialID: id, ModelID: model(modelCoverageCacheCapacity + 3)}
	s.mu.Lock()
	_, newest := s.modelCoverage[last]
	s.mu.Unlock()
	if !newest {
		t.Fatal("bounded cache evicted the newest entry")
	}

	// An entry with a request in flight is never evicted; a new key evicts an
	// idle entry instead, so the waiter still resolves from its own cache slot.
	started, release := make(chan struct{}), make(chan struct{})
	inflightModel := "model-inflight"
	s.SetHost(func(method string, in, out any) error {
		if method != "host.http.do" {
			return host.call(method, in, out)
		}
		close(started)
		<-release
		assign(out, map[string]any{"StatusCode": http.StatusOK, "Body": mustJSON(t, officialModelCoverage(t, "ws-coverage", inflightModel))})
		return nil
	})
	done := make(chan modelCoverageResponse, 1)
	go func() {
		_, view, _ := modelCoverageCall(t, s, id, inflightModel, true)
		done <- view
	}()
	<-started
	s.mu.Lock()
	inflightKey := modelCoverageKey{CredentialID: id, ModelID: inflightModel}
	_, tracked := s.modelCoverage[inflightKey]
	s.mu.Unlock()
	if !tracked {
		close(release)
		<-done
		t.Fatal("inflight refresh was not tracked")
	}
	s.SetHost(host.call)
	queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, officialModelCoverage(t, "ws-coverage", "model-fresh"))})
	if status, view, raw := modelCoverageCall(t, s, id, "model-fresh", true); status != http.StatusOK || view.State != "available" {
		close(release)
		<-done
		t.Fatalf("refresh during inflight fetch: %d %s", status, raw)
	}
	close(release)
	if view := <-done; view.State != "available" || view.ModelID != inflightModel {
		t.Fatalf("inflight entry was evicted: %+v", view)
	}
	s.mu.Lock()
	size = len(s.modelCoverage)
	s.mu.Unlock()
	if size > modelCoverageCacheCapacity {
		t.Fatalf("cache grew to %d entries with inflight work", size)
	}
}
