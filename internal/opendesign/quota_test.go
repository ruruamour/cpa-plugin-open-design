package opendesign

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var quotaTestTime = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

func quotaTestClock(s *Service) func(time.Duration) {
	var stamp atomic.Int64
	stamp.Store(quotaTestTime.UnixNano())
	s.mu.Lock()
	s.quotaNow = func() time.Time { return time.Unix(0, stamp.Load()).UTC() }
	s.mu.Unlock()
	return func(d time.Duration) { stamp.Add(int64(d)) }
}

func addQuotaCredential(t *testing.T, s *Service, host *fakeHost, workspace string) string {
	t.Helper()
	id := addCredential(t, s, host, "quota", "sk-quota-inference-secret")
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
		"id": id, "control_key": "quota-control-secret", "workspace_id": workspace,
	}); status != http.StatusOK {
		t.Fatalf("configure quota: %d %s", status, raw)
	}
	return id
}

func quotaCall(t *testing.T, s *Service, id string, refresh bool) (int, quotaResponse, string) {
	t.Helper()
	query := map[string][]string{"credential_id": {id}}
	if refresh {
		query["refresh"] = []string{"1"}
	}
	status, _, raw := managementCall(t, s, http.MethodGet, apiBase+"/quota", query, nil)
	var view quotaResponse
	if status == http.StatusOK {
		if err := json.Unmarshal([]byte(raw), &view); err != nil {
			t.Fatal(err)
		}
	}
	return status, view, raw
}

func queueQuota(host *fakeHost, responses ...fakeHTTPResponse) {
	host.mu.Lock()
	host.doResponses = append(host.doResponses, responses...)
	host.mu.Unlock()
}

func quotaWalletResponse(balance string) fakeHTTPResponse {
	return fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"balanceUsd":"` + balance + `","ignored":"quota-control-secret"}`)}
}

func officialQuotaFunding(t *testing.T, workspace string) map[string]any {
	t.Helper()
	return map[string]any{
		"schemaVersion": 1, "workspaceId": workspace, "workspaceMemberId": "private-member-do-not-expose",
		"modelId": nil, "generatedAt": quotaTestTime.Format(time.RFC3339), "balanceUsd": "-0.125",
		"modelCovered": nil, "funding": "coding_plan",
		"codingPlan": map[string]any{
			"workspaceId": workspace, "generatedAt": quotaTestTime.Format(time.RFC3339), "eligible": true, "tier": "plus",
			"windows": []any{map[string]any{
				"policyId": "p1", "durationSeconds": 3600, "resetMode": "anchored_recurring",
				"usedCredits": "100", "limitCredits": "1000", "remainingCredits": "900",
				"windowStart": quotaTestTime.Format(time.RFC3339), "resetsAt": quotaTestTime.Add(time.Hour).Format(time.RFC3339),
			}},
		},
		"unknown": "quota-control-secret",
	}
}

func TestQuotaCacheOnlyAndNotConfiguredNeverUseNetwork(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "inference-only", "sk-quota-inference-only")
	for _, refresh := range []bool{false, true} {
		status, view, raw := quotaCall(t, s, id, refresh)
		if status != http.StatusOK || view.State != "not_configured" || view.Wallet.Data != nil || view.Funding.Data != nil ||
			view.FetchedAt != nil || view.Wallet.FetchedAt != nil || view.NextRefreshAt != nil {
			t.Fatalf("not configured must stay unknown: %d %s", status, raw)
		}
	}
	configured := addQuotaCredential(t, s, host, "ws-1")
	status, view, raw := quotaCall(t, s, configured, false)
	if status != http.StatusOK || view.State != "needs_refresh" || view.Wallet.Data != nil || view.Funding.Data != nil {
		t.Fatalf("cache miss must stay unknown: %d %s", status, raw)
	}
	if host.countMethod("host.http.do") != 0 {
		t.Fatal("cache-only/not-configured quota performed network I/O")
	}
	if status, _, _ := quotaCall(t, s, "", true); status != http.StatusBadRequest {
		t.Fatalf("missing id = %d", status)
	}
	if status, _, _ := quotaCall(t, s, "unknown", true); status != http.StatusNotFound {
		t.Fatalf("unknown id = %d", status)
	}
	registered := callRPC(t, s, "management.register", nil).(map[string]any)["routes"].([]map[string]string)
	for _, route := range registered {
		if route["Method"] == http.MethodGet && route["Path"] == apiBase+"/quota" {
			return
		}
	}
	t.Fatal("quota GET route was not registered")
}

func TestQuotaOfficialWireContractAllowlistAndIndependentParts(t *testing.T) {
	s, host := newTestService(t)
	advance := quotaTestClock(s)
	workspace := "ws/one +?#"
	id := addQuotaCredential(t, s, host, workspace)
	queueQuota(host, quotaWalletResponse("12.345"), fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, officialQuotaFunding(t, workspace))})
	rawRequest := mustJSON(t, ManagementRequest{Method: http.MethodGet, Path: apiBase + "/quota",
		Query: url.Values{"credential_id": {id}, "refresh": {"1"}}, HostCallbackID: "quota-callback-1"})
	out, err := s.management(rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	response := out.(ManagementResponse)
	var view quotaResponse
	if err := json.Unmarshal(response.Body, &view); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || view.State != "available" || view.Wallet.Data.BalanceUSD != "12.345" ||
		view.Wallet.Data.UpdatedAt != nil || view.Funding.Data.BalanceUSD != "-0.125" || view.Funding.Data.ModelCovered != nil || view.Funding.Data.FundingCheck != nil {
		t.Fatalf("official response: %s", response.Body)
	}
	for _, secret := range []string{"quota-control-secret", "sk-quota-inference-secret", "private-member-do-not-expose", "workspaceMemberId", "unknown", "eligible", "generatedAt"} {
		if strings.Contains(string(response.Body), secret) {
			t.Fatalf("management exposed non-allowlisted field/value %q", secret)
		}
	}
	host.mu.Lock()
	var calls []map[string]any
	for _, call := range host.calls {
		if call.Method == "host.http.do" {
			calls = append(calls, call.Payload.(map[string]any))
		}
	}
	host.mu.Unlock()
	if len(calls) != 2 || calls[0]["url"] != billingAPIURL+"/api/v1/wallet/balance" ||
		calls[1]["url"] != billingAPIURL+"/api/v1/workspaces/"+url.PathEscape(workspace)+"/billing/funding-check" {
		t.Fatalf("quota endpoints = %v", calls)
	}
	for _, call := range calls {
		if call["host_callback_id"] != "quota-callback-1" || call["method"] != http.MethodGet || call["operation_id"] == "" ||
			call["headers"].(http.Header).Get("Authorization") != "Bearer quota-control-secret" {
			t.Fatal("quota request did not use the control-key host operation contract")
		}
		for _, forbidden := range []string{"timeout", "timeout_ms", "deadline", "body"} {
			if _, exists := call[forbidden]; exists {
				t.Fatalf("unsupported quota callback property: %s", forbidden)
			}
		}
	}
	// Wallet and funding do not erase one another when only one endpoint fails.
	advance(time.Minute)
	queueQuota(host, quotaWalletResponse("12.000"), fakeHTTPResponse{Status: http.StatusForbidden, Body: []byte("raw quota-control-secret")})
	_, stale, raw := quotaCall(t, s, id, true)
	if stale.State != "stale" || stale.Wallet.State != "available" || stale.Funding.State != "stale" || stale.Funding.Error.Kind != "forbidden" ||
		!stale.Funding.FetchedAt.Equal(*view.Funding.FetchedAt) || stale.Funding.Data.CodingPlan.Windows[0].UsedCredits != "100" || strings.Contains(raw, "raw quota") {
		t.Fatalf("independent last success: %s", raw)
	}
}

func TestQuotaFirstErrorsAndStaleSnapshotsAreSafe(t *testing.T) {
	cases := []struct {
		name string
		resp fakeHTTPResponse
		kind string
	}{
		{"401", fakeHTTPResponse{Status: 401, Body: []byte("raw quota-control-secret")}, "unauthorized"},
		{"403", fakeHTTPResponse{Status: 403, Body: []byte("raw quota-control-secret")}, "forbidden"},
		{"429", fakeHTTPResponse{Status: 429, Body: []byte("raw quota-control-secret")}, "rate_limited"},
		{"timeout", fakeHTTPResponse{Err: context.DeadlineExceeded}, "timeout"},
		{"host-timeout", fakeHTTPResponse{Err: errString("context deadline exceeded: quota-control-secret")}, "timeout"},
		{"malformed", fakeHTTPResponse{Status: 200, Body: []byte(`{"balanceUsd":0}`)}, "malformed"},
		{"network", fakeHTTPResponse{Err: errString("transport raw quota-control-secret")}, "network"},
		{"upstream", fakeHTTPResponse{Status: 500, Body: []byte("raw quota-control-secret")}, "upstream"},
		{"too-large", fakeHTTPResponse{Status: 200, Body: []byte(strings.Repeat(" ", maxQuotaBody+1))}, "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, previouslySucceeded := range []bool{false, true} {
				s, host := newTestService(t)
				advance := quotaTestClock(s)
				id := addQuotaCredential(t, s, host, "")
				if previouslySucceeded {
					queueQuota(host, quotaWalletResponse("19.875"))
					_, initial, _ := quotaCall(t, s, id, true)
					if initial.State != "available" {
						t.Fatal("seed wallet failed")
					}
					advance(time.Minute)
				}
				queueQuota(host, tc.resp)
				status, view, raw := quotaCall(t, s, id, true)
				if status != http.StatusOK || view.Wallet.Error == nil || view.Wallet.Error.Kind != tc.kind || strings.Contains(raw, "quota-control-secret") || strings.Contains(raw, "raw quota") {
					t.Fatalf("quota error contract: %d %s", status, raw)
				}
				if previouslySucceeded {
					if view.State != "stale" || view.Wallet.State != "stale" || view.Wallet.Data.BalanceUSD != "19.875" || !view.Wallet.FetchedAt.Equal(quotaTestTime) || !view.FetchedAt.Equal(quotaTestTime) {
						t.Fatalf("last success lost: %s", raw)
					}
				} else if view.State != "error" || view.Wallet.Data != nil || view.Wallet.FetchedAt != nil || view.FetchedAt != nil {
					t.Fatalf("first failure fabricated a balance or timestamp: %s", raw)
				}
				if view.Funding.State != "not_requested" || view.Funding.Data != nil {
					t.Fatalf("workspace-less quota: %s", raw)
				}
				before := host.countMethod("host.http.do")
				quotaCall(t, s, id, true)
				quotaCall(t, s, id, false)
				if host.countMethod("host.http.do") != before {
					t.Fatal("failed attempt was not throttled/cached")
				}
			}
		})
	}
}

func TestQuotaPartialSuccessDoesNotInventMissingComponent(t *testing.T) {
	for _, walletOK := range []bool{false, true} {
		s, host := newTestService(t)
		id := addQuotaCredential(t, s, host, "ws-1")
		wallet, funding := quotaWalletResponse("1.25"), fakeHTTPResponse{Status: 200, Body: mustJSON(t, officialQuotaFunding(t, "ws-1"))}
		if walletOK {
			funding = fakeHTTPResponse{Status: 429}
		} else {
			wallet = fakeHTTPResponse{Status: 401}
		}
		queueQuota(host, wallet, funding)
		_, view, raw := quotaCall(t, s, id, true)
		if view.State != "partial" || (view.Wallet.Data != nil) != walletOK || (view.Funding.Data != nil) == walletOK {
			t.Fatalf("partial success: %s", raw)
		}
	}
}

func TestQuotaRefreshIntervalAtLeastSixtySeconds(t *testing.T) {
	s, host := newTestService(t)
	advance := quotaTestClock(s)
	id := addQuotaCredential(t, s, host, "")
	queueQuota(host, quotaWalletResponse("1.00"))
	_, first, _ := quotaCall(t, s, id, true)
	if first.NextRefreshAt == nil || first.NextRefreshAt.Sub(*first.FetchedAt) != time.Minute {
		t.Fatal("refresh lower bound was not advertised")
	}
	advance(60*time.Second - time.Nanosecond)
	quotaCall(t, s, id, true)
	if host.countMethod("host.http.do") != 1 {
		t.Fatal("refresh occurred before 60 seconds")
	}
	advance(time.Nanosecond)
	queueQuota(host, quotaWalletResponse("0.75"))
	_, view, _ := quotaCall(t, s, id, true)
	if host.countMethod("host.http.do") != 2 || view.Wallet.Data.BalanceUSD != "0.75" {
		t.Fatal("refresh at the lower bound failed")
	}
}

func TestQuotaConcurrentRefreshCoalescesAndDefaultReadDoesNotWait(t *testing.T) {
	s, host := newTestService(t)
	id := addQuotaCredential(t, s, host, "")
	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int64
	s.SetHost(func(method string, in, out any) error {
		if method == "host.http.do" {
			if requests.Add(1) == 1 {
				close(started)
			}
			<-release
			assign(out, map[string]any{"StatusCode": 200, "Body": []byte(`{"balanceUsd":"8.125"}`)})
			return nil
		}
		return host.call(method, in, out)
	})
	const workers = 24
	results := make(chan quotaResponse, workers)
	for range workers {
		go func() {
			_, view, _ := quotaCall(t, s, id, true)
			results <- view
		}()
	}
	<-started
	deadline := time.Now().Add(3 * time.Second)
	for s.activeN.Load() < workers && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if s.activeN.Load() != workers || requests.Load() != 1 {
		t.Fatal("concurrent requests did not share one quota operation")
	}
	_, cached, _ := quotaCall(t, s, id, false)
	if cached.State != "needs_refresh" || cached.Wallet.Data != nil {
		t.Fatal("default read waited for or invented a pending snapshot")
	}
	close(release)
	for range workers {
		if view := <-results; view.State != "available" || view.Wallet.Data.BalanceUSD != "8.125" {
			t.Fatal("coalesced refresh did not share the successful result")
		}
	}
	if requests.Load() != 1 {
		t.Fatal("coalesced refresh sent duplicate requests")
	}
	assertIdle(t, s)
}

func TestQuotaRefreshHasThreeCredentialConcurrencyLimit(t *testing.T) {
	s, host := newTestService(t)
	var ids []string
	for range 6 {
		ids = append(ids, addQuotaCredential(t, s, host, ""))
	}
	started, release := make(chan struct{}, 6), make(chan struct{})
	var active, maximum atomic.Int64
	s.SetHost(func(method string, in, out any) error {
		if method == "host.http.do" {
			n := active.Add(1)
			defer active.Add(-1)
			for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
			}
			started <- struct{}{}
			<-release
			assign(out, map[string]any{"StatusCode": 200, "Body": []byte(`{"balanceUsd":"1"}`)})
			return nil
		}
		return host.call(method, in, out)
	})
	var workers sync.WaitGroup
	for _, id := range ids {
		workers.Add(1)
		go func() { defer workers.Done(); quotaCall(t, s, id, true) }()
	}
	for range 3 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("quota concurrency did not reach three")
		}
	}
	close(release)
	workers.Wait()
	if maximum.Load() != 3 {
		t.Fatalf("maximum upstream quota concurrency = %d", maximum.Load())
	}
	assertIdle(t, s)
}

func TestQuotaMutationInvalidatesCacheAndFencesLateResponse(t *testing.T) {
	for _, mutation := range []string{"api_key", "control_key", "workspace_id", "clear_billing", "delete", "key-cycle"} {
		t.Run(mutation, func(t *testing.T) {
			s, host := newTestService(t)
			id := addQuotaCredential(t, s, host, "")
			host.writeAuthFile(t, id+".json", host.savedAuth(id+".json"))
			started, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int64
			s.SetHost(func(method string, in, out any) error {
				if method != "host.http.do" {
					return host.call(method, in, out)
				}
				if calls.Add(1) == 1 {
					close(started)
					<-release
					assign(out, map[string]any{"StatusCode": 200, "Body": []byte(`{"balanceUsd":"999"}`)})
				} else {
					body := []byte(`{"balanceUsd":"11"}`)
					if strings.Contains(stringField(in, "url"), "funding-check") {
						body = mustJSON(t, officialQuotaFunding(t, "new-ws"))
					}
					assign(out, map[string]any{"StatusCode": 200, "Body": body})
				}
				return nil
			})
			type result struct {
				status int
				view   quotaResponse
			}
			late := make(chan result, 1)
			go func() { status, view, _ := quotaCall(t, s, id, true); late <- result{status, view} }()
			<-started
			if mutation == "delete" {
				if status, _, raw := managementCall(t, s, http.MethodDelete, apiBase+"/credentials", map[string][]string{"id": {id}}, nil); status != 200 {
					t.Fatalf("delete: %d %s", status, raw)
				}
			} else {
				value := any("new-key-secret")
				field := mutation
				switch mutation {
				case "clear_billing":
					value = true
				case "workspace_id":
					value = "new-ws"
				case "key-cycle":
					field = "control_key"
				}
				if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, field: value}); status != 200 {
					t.Fatalf("mutation status = %d", status)
				}
				if mutation == "key-cycle" {
					managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": "quota-control-secret"})
				}
			}
			_, cached, _ := quotaCall(t, s, id, false)
			if cached.Wallet.Data != nil {
				t.Fatal("mutation left the old cache visible")
			}
			if mutation != "delete" && mutation != "clear_billing" {
				_, refreshed, _ := quotaCall(t, s, id, true)
				if refreshed.Wallet.Data == nil || refreshed.Wallet.Data.BalanceUSD != "11" {
					t.Fatal("new credential version could not refresh independently")
				}
			}
			close(release)
			gotLate := <-late
			if mutation == "delete" {
				if gotLate.status != 404 {
					t.Fatal("late deleted-credential response was exposed")
				}
			} else if mutation == "clear_billing" {
				if gotLate.view.State != "not_configured" || gotLate.view.Wallet.Data != nil || calls.Load() != 1 {
					t.Fatal("late cleared-credential response was exposed")
				}
			} else if gotLate.view.Wallet.Data == nil || gotLate.view.Wallet.Data.BalanceUSD != "11" {
				t.Fatal("late response overwrote or exposed the old credential snapshot")
			}
			assertIdle(t, s)
		})
	}
}

func TestQuotaParseAuthInvalidatesExternalKeyChange(t *testing.T) {
	s, host := newTestService(t)
	id := addQuotaCredential(t, s, host, "")
	queueQuota(host, quotaWalletResponse("10"))
	quotaCall(t, s, id, true)
	var c Credential
	if err := json.Unmarshal(host.savedAuth(id+".json"), &c); err != nil {
		t.Fatal(err)
	}
	c.ControlKey = "external-control-replacement"
	raw := jsonBytes(c)
	host.writeAuthFile(t, id+".json", raw)
	if _, err := s.parseAuth(mustJSON(t, map[string]any{"FileName": id + ".json", "RawJSON": raw, "Host": map[string]any{"AuthDir": host.authDir}})); err != nil {
		t.Fatal(err)
	}
	_, view, _ := quotaCall(t, s, id, false)
	if view.State != "needs_refresh" || view.Wallet.Data != nil {
		t.Fatal("auth.parse retained a snapshot from the previous control key")
	}
}

func TestQuotaQuiesceCancelsOperationAndWakesJoinedAndQueuedRequests(t *testing.T) {
	for _, queueOnly := range []bool{false, true} {
		s, host := newTestService(t)
		id := addQuotaCredential(t, s, host, "")
		if queueOnly {
			for range 3 {
				s.quotaSlots <- struct{}{}
			}
		} else {
			host.blockCall()
			queueQuota(host, quotaWalletResponse("1"))
		}
		results := make(chan int, 2)
		for range 2 {
			go func() { status, _, _ := quotaCall(t, s, id, true); results <- status }()
		}
		if !queueOnly {
			host.awaitBlocked(t)
		}
		deadline := time.Now().Add(3 * time.Second)
		for s.activeN.Load() < 2 && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		if s.activeN.Load() != 2 {
			t.Fatal("quota requests did not reach the queue/join")
		}
		stopped := make(chan struct{})
		go func() { _, _ = s.quiesce(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Fatal("quota quiesce left a host call/joiner/queue blocked")
		}
		for range 2 {
			if status := <-results; status != http.StatusServiceUnavailable {
				t.Fatalf("quota during quiesce = %d", status)
			}
		}
		if !queueOnly && len(host.cancels()) == 0 {
			t.Fatal("quota did not cancel its host operation")
		}
		if status, _, _ := quotaCall(t, s, id, true); status != http.StatusServiceUnavailable {
			t.Fatal("quota accepted new work after stop")
		}
		assertIdle(t, s)
	}
}
