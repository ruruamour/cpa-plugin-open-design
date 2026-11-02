package opendesign

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func assertExhaustedQuotaWindow(t *testing.T, view quotaResponse) {
	t.Helper()
	forecast := windowForecast(t, view)
	if view.State != "available" || forecast.State != "exhausted" || forecast.SecondsToExhaust != nil || forecast.ExhaustsAt != nil || forecast.Basis != "" {
		t.Fatalf("official zero was not preserved as exhausted without a projected time: %+v", forecast)
	}
}

func TestQuotaRoutePreservesFreshExhaustedWindowAcrossCacheReads(t *testing.T) {
	s, host := newTestService(t)
	advance := quotaTestClock(s)
	id := addQuotaCredential(t, s, host, "ws-1")
	refreshQuotaWindow(t, s, host, id, "ws-1", "100", "900", nil)
	advance(time.Minute)
	fresh := refreshQuotaWindow(t, s, host, id, "ws-1", "1000", "0", nil)
	assertExhaustedQuotaWindow(t, fresh)
	before := host.countMethod("host.http.do")
	// The previous estimated-at-now zero was erased on this same return path.
	// An actual zero remains known even when time advances beyond fetched_at.
	advance(time.Nanosecond)
	for _, refresh := range []bool{false, true} {
		status, cached, raw := quotaCall(t, s, id, refresh)
		if status != http.StatusOK {
			t.Fatalf("cached exhausted quota status = %d", status)
		}
		assertExhaustedQuotaWindow(t, cached)
		if cached.Funding.Data.CodingPlan.Windows[0].RemainingCredits != "0" || !cached.Funding.FetchedAt.Equal(*fresh.Funding.FetchedAt) ||
			strings.Contains(raw, "seconds_to_exhaust") || strings.Contains(raw, "exhausts_at") || strings.Contains(raw, "recent_consumption") {
			t.Fatalf("cached official zero was lost or treated as a rate estimate: %s", raw)
		}
	}
	if host.countMethod("host.http.do") != before {
		t.Fatal("cache/throttled exhausted read issued another upstream request")
	}
	advance(time.Hour)
	_, afterReset, _ := quotaCall(t, s, id, false)
	if f := windowForecast(t, afterReset); f.State != "insufficient_samples" || afterReset.Funding.Data.CodingPlan.Windows[0].RemainingCredits != "0" {
		t.Fatal("cached zero claimed the new window was exhausted after a known reset")
	}
	assertExhaustedQuotaWindow(t, fresh) // Cache expiry never mutates a published snapshot.
}

func TestQuotaRouteOfficialZeroNeedsNoConsumptionSamples(t *testing.T) {
	for _, mode := range []string{"anchored_recurring", "activity_triggered", "nullable_bounds"} {
		t.Run(mode, func(t *testing.T) {
			s, host := newTestService(t)
			quotaTestClock(s)
			id := addQuotaCredential(t, s, host, "ws-1")
			view := refreshQuotaWindow(t, s, host, id, "ws-1", "1000", "000", func(m map[string]any) {
				w := m["codingPlan"].(map[string]any)["windows"].([]any)[0].(map[string]any)
				if mode == "nullable_bounds" {
					w["windowStart"], w["resetsAt"] = nil, nil
				} else {
					w["resetMode"] = mode
				}
			})
			assertExhaustedQuotaWindow(t, view)
			_, cached, _ := quotaCall(t, s, id, false)
			assertExhaustedQuotaWindow(t, cached)
		})
	}
}

func TestQuotaRouteFailedRefreshWithdrawsExhaustedStatus(t *testing.T) {
	s, host := newTestService(t)
	advance := quotaTestClock(s)
	id := addQuotaCredential(t, s, host, "ws-1")
	fresh := refreshQuotaWindow(t, s, host, id, "ws-1", "1000", "0", nil)
	assertExhaustedQuotaWindow(t, fresh)
	advance(time.Minute)
	queueQuota(host, quotaWalletResponse("20"), fakeHTTPResponse{Status: http.StatusForbidden})
	status, stale, raw := quotaCall(t, s, id, true)
	if status != http.StatusOK || stale.State != "stale" || stale.Funding.State != "stale" || stale.Funding.Error == nil ||
		stale.Funding.Error.Kind != "forbidden" || windowForecast(t, stale).State != "insufficient_samples" ||
		stale.Funding.Data.CodingPlan.Windows[0].RemainingCredits != "0" {
		t.Fatalf("failed refresh asserted a fresh exhausted state or discarded the last success: %s", raw)
	}
}
