package opendesign

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func quotaDeadlineCall(t *testing.T, s *Service, id string) quotaResponse {
	t.Helper()
	out, err := s.management(mustJSON(t, ManagementRequest{
		Method: http.MethodGet, Path: apiBase + "/quota", HostCallbackID: "quota-deadline-callback",
		Query: url.Values{"credential_id": {id}, "refresh": {"1"}},
	}))
	if err != nil {
		t.Error(err)
		return quotaResponse{}
	}
	response := out.(ManagementResponse)
	var view quotaResponse
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &view) != nil {
		t.Errorf("quota deadline response: %d %s", response.StatusCode, response.Body)
	}
	return view
}

func assertQuotaOperationsReleased(t *testing.T, s *Service) {
	t.Helper()
	assertIdle(t, s)
	if len(s.quotaSlots) != 0 {
		t.Fatal("quota deadline left a concurrency slot occupied")
	}
	s.ops.mu.Lock()
	pending := len(s.ops.pending)
	s.ops.mu.Unlock()
	if pending != 0 {
		t.Fatal("quota deadline left an operation registered")
	}
}

func TestQuotaDeadlineCancelsThreeBlockedRequestsAndReleasesSlots(t *testing.T) {
	s, host := newTestService(t)
	quotaTestClock(s)
	s.quotaBudget = 750 * time.Millisecond
	results := make(chan quotaResponse, 4)
	var firstID string
	for i := range 3 {
		id := addQuotaCredential(t, s, host, "")
		if i == 0 {
			firstID = id
		}
		// These calls really block in the fake transport until host.http.cancel
		// closes the operation. They do not return a scripted timeout error.
		host.blockCall()
		queueQuota(host, quotaWalletResponse("5"))
		go func() { results <- quotaDeadlineCall(t, s, id) }()
		host.awaitBlocked(t)
	}
	go func() { results <- quotaDeadlineCall(t, s, firstID) }()
	for range 4 {
		select {
		case view := <-results:
			if view.State != "error" || view.Wallet.Error == nil || view.Wallet.Error.Kind != "timeout" || view.Wallet.Data != nil {
				t.Fatalf("blocked request did not become an unknown-balance timeout: %+v", view)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("quota total deadline did not release a blocked request/joiner")
		}
	}
	if host.countMethod("host.http.do") != 3 || len(host.cancels()) != 3 {
		t.Fatal("deadline did not coalesce requests and cancel all three host operations")
	}
	host.mu.Lock()
	for _, call := range host.calls {
		if call.Method != "host.http.cancel" {
			continue
		}
		payload := call.Payload.(map[string]any)
		if payload["host_callback_id"] != "quota-deadline-callback" {
			t.Error("deadline cancellation lost the owning host callback id")
		}
		op := host.ops[payload["operation_id"].(string)]
		select {
		case <-op.done:
		default:
			t.Error("deadline cancellation did not actually stop its blocked transport")
		}
	}
	host.mu.Unlock()
	assertQuotaOperationsReleased(t, s)

	// All slots remain usable after the hung operations have been cancelled.
	fourth := addQuotaCredential(t, s, host, "")
	queueQuota(host, quotaWalletResponse("7.50"))
	view := quotaDeadlineCall(t, s, fourth)
	if view.State != "available" || view.Wallet.Data == nil || view.Wallet.Data.BalanceUSD != "7.50" {
		t.Fatal("hung quota refreshes prevented a subsequent credential from refreshing")
	}
	assertQuotaOperationsReleased(t, s)
}

func TestQuotaDeadlineSharesOneBudgetAcrossWalletAndFunding(t *testing.T) {
	s, host := newTestService(t)
	quotaTestClock(s)
	s.quotaBudget = 800 * time.Millisecond
	id := addQuotaCredential(t, s, host, "ws-1")
	walletStarted, releaseWallet := make(chan struct{}), make(chan struct{})
	s.SetHost(func(method string, in, out any) error {
		if method == "host.http.do" {
			if stringField(in, "url") == billingAPIURL+"/api/v1/wallet/balance" {
				close(walletStarted)
				<-releaseWallet
			} else {
				host.blockCall()
			}
		}
		return host.call(method, in, out)
	})
	queueQuota(host, quotaWalletResponse("12.30"), fakeHTTPResponse{Status: 200, Body: mustJSON(t, officialQuotaFunding(t, "ws-1"))})
	result := make(chan quotaResponse, 1)
	go func() { result <- quotaDeadlineCall(t, s, id) }()
	<-walletStarted
	// Spend most of the shared budget on wallet; funding gets only the rest.
	walletDelay := time.NewTimer(3 * s.quotaBudget / 4)
	<-walletDelay.C
	close(releaseWallet)
	host.awaitBlocked(t)
	select {
	case view := <-result:
		if view.State != "partial" || view.Wallet.State != "available" || view.Wallet.Data.BalanceUSD != "12.30" ||
			view.Funding.State != "error" || view.Funding.Error == nil || view.Funding.Error.Kind != "timeout" || view.Funding.Data != nil {
			t.Fatalf("shared-budget funding timeout lost independent wallet success: %+v", view)
		}
	case <-time.After(s.quotaBudget / 2):
		t.Fatal("funding received a new full budget instead of the refresh remainder")
	}
	if len(host.cancels()) != 1 || host.countMethod("host.http.do") != 2 {
		t.Fatal("shared deadline did not cancel the blocked funding operation")
	}
	assertQuotaOperationsReleased(t, s)
}

func TestQuotaDeadlineDoesNotStartFundingAfterWalletTimeout(t *testing.T) {
	s, host := newTestService(t)
	quotaTestClock(s)
	s.quotaBudget = 30 * time.Millisecond
	id := addQuotaCredential(t, s, host, "ws-1")
	host.blockCall()
	queueQuota(host, quotaWalletResponse("5"))
	view := quotaDeadlineCall(t, s, id)
	if view.Wallet.Error == nil || view.Wallet.Error.Kind != "timeout" || view.Funding.Error == nil || view.Funding.Error.Kind != "timeout" ||
		view.Wallet.Data != nil || view.Funding.Data != nil || host.countMethod("host.http.do") != 1 || host.countMethod("host.http.operation_open") != 1 || len(host.cancels()) != 1 {
		t.Fatal("expired total budget still started funding or failed to cancel wallet")
	}
	assertQuotaOperationsReleased(t, s)
}

func TestQuotaDeadlineBoundsSlotQueueWithoutHTTP(t *testing.T) {
	s, host := newTestService(t)
	quotaTestClock(s)
	s.quotaBudget = 25 * time.Millisecond
	id := addQuotaCredential(t, s, host, "ws-1")
	for range 3 {
		s.quotaSlots <- struct{}{}
	}
	result := make(chan quotaResponse, 1)
	go func() { result <- quotaDeadlineCall(t, s, id) }()
	select {
	case view := <-result:
		if view.Wallet.Error == nil || view.Wallet.Error.Kind != "timeout" || view.Funding.Error == nil || view.Funding.Error.Kind != "timeout" || view.NextRefreshAt == nil {
			t.Fatal("queue deadline did not preserve the failed-attempt quota contract")
		}
	case <-time.After(time.Second):
		t.Fatal("quota queue ignored the total deadline")
	}
	for range 3 {
		<-s.quotaSlots
	}
	quotaDeadlineCall(t, s, id) // Immediate retry is throttled even after a queue timeout.
	if host.countMethod("host.http.do") != 0 || host.countMethod("host.http.operation_open") != 0 {
		t.Fatal("queued deadline/retry issued an HTTP operation")
	}
	assertQuotaOperationsReleased(t, s)
}

func TestBoundedQuotaRejectsMissingCancellationBridge(t *testing.T) {
	for _, emptyID := range []bool{false, true} {
		s, host := newTestService(t)
		id := addQuotaCredential(t, s, host, "")
		s.SetHost(func(method string, in, out any) error {
			if method == "host.http.operation_open" {
				if emptyID {
					assign(out, map[string]any{"operation_id": ""})
					return nil
				}
				return errors.New("unsupported operation bridge")
			}
			return host.call(method, in, out)
		})
		view := quotaDeadlineCall(t, s, id)
		if view.State != "error" || view.Wallet.Error == nil || view.Wallet.Data != nil || host.countMethod("host.http.do") != 0 {
			t.Fatal("bounded quota fell back to an uncancellable host request")
		}
		assertQuotaOperationsReleased(t, s)
	}
}

func TestBoundedOperationCancelsLateOpenWithoutStartingHTTP(t *testing.T) {
	s, host := newTestService(t)
	opened, release := make(chan struct{}), make(chan struct{})
	s.SetHost(func(method string, in, out any) error {
		err := host.call(method, in, out)
		if method == "host.http.operation_open" {
			close(opened)
			<-release
		}
		return err
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		var response upstreamCall
		result <- s.doOperationContext(ctx, "late-open-owner", "host.http.do", map[string]any{"host_callback_id": "late-open-owner"}, &response)
	}()
	<-opened
	cancel()
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("late operation result = %v", err)
	}
	if host.countMethod("host.http.do") != 0 || len(host.cancels()) != 1 || stringField(host.lastPayload("host.http.cancel"), "host_callback_id") != "late-open-owner" {
		t.Fatal("late opened operation escaped its request cancellation")
	}
	assertQuotaOperationsReleased(t, s)
}
