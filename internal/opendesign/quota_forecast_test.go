package opendesign

import (
	"math/big"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func refreshQuotaWindow(t *testing.T, s *Service, host *fakeHost, id, workspace, used, remaining string, change func(map[string]any)) quotaResponse {
	t.Helper()
	funding := officialQuotaFunding(t, workspace)
	window := funding["codingPlan"].(map[string]any)["windows"].([]any)[0].(map[string]any)
	window["usedCredits"], window["remainingCredits"] = used, remaining
	if change != nil {
		change(funding)
	}
	queueQuota(host, quotaWalletResponse("20.00"), fakeHTTPResponse{Status: 200, Body: mustJSON(t, funding)})
	status, view, raw := quotaCall(t, s, id, true)
	if status != 200 || view.Funding.Data == nil {
		t.Fatalf("refresh window: %d %s", status, raw)
	}
	return view
}

func windowForecast(t *testing.T, view quotaResponse) quotaForecast {
	t.Helper()
	if view.Funding.Data == nil || len(view.Funding.Data.CodingPlan.Windows) != 1 {
		t.Fatal("expected one window for forecast")
	}
	return view.Funding.Data.CodingPlan.Windows[0].Forecast
}

func TestQuotaForecastUsesTwoAdjacentSuccessfulWindowReads(t *testing.T) {
	s, host := newTestService(t)
	advance := quotaTestClock(s)
	id := addQuotaCredential(t, s, host, "ws-1")
	first := refreshQuotaWindow(t, s, host, id, "ws-1", "100", "900", nil)
	if f := windowForecast(t, first); f.State != "insufficient_samples" || f.SecondsToExhaust != nil || f.ExhaustsAt != nil {
		t.Fatal("one sample produced an exhaustion estimate")
	}
	advance(time.Minute)
	second := refreshQuotaWindow(t, s, host, id, "ws-1", "200", "800", nil)
	forecast := windowForecast(t, second)
	if forecast.State != "estimated" || forecast.SecondsToExhaust == nil || *forecast.SecondsToExhaust != 480 ||
		forecast.ExhaustsAt == nil || !forecast.ExhaustsAt.Equal(quotaTestTime.Add(9*time.Minute)) || forecast.Basis != "recent_consumption" {
		t.Fatalf("100 credits/60s, 800 left should predict 480s: %+v", forecast)
	}
	before := host.countMethod("host.http.do")
	_, cached, _ := quotaCall(t, s, id, false)
	if f := windowForecast(t, cached); f.ExhaustsAt == nil || !f.ExhaustsAt.Equal(*forecast.ExhaustsAt) || host.countMethod("host.http.do") != before {
		t.Fatal("cache-only read resampled or changed the forecast")
	}
	advance(8 * time.Minute)
	_, cached, _ = quotaCall(t, s, id, false)
	if f := windowForecast(t, cached); f.State != "insufficient_samples" || f.ExhaustsAt != nil || cached.Funding.Data.CodingPlan.Windows[0].RemainingCredits != "800" {
		t.Fatal("old predicted time was treated as proof of current exhaustion")
	}
	if windowForecast(t, second).State != "estimated" {
		t.Fatal("expiring a cached forecast mutated an already published snapshot")
	}
	assertIdle(t, s)
}

func TestQuotaForecastResetBeforeExhaustionAndRollingWindows(t *testing.T) {
	for _, rolling := range []bool{false, true} {
		s, host := newTestService(t)
		advance := quotaTestClock(s)
		id := addQuotaCredential(t, s, host, "ws-1")
		change := func(m map[string]any) {
			if rolling {
				m["codingPlan"].(map[string]any)["windows"].([]any)[0].(map[string]any)["resetMode"] = "activity_triggered"
			}
		}
		refreshQuotaWindow(t, s, host, id, "ws-1", "100", "900", change)
		advance(time.Minute)
		view := refreshQuotaWindow(t, s, host, id, "ws-1", "101", "899", change)
		forecast := windowForecast(t, view)
		want := "resets_before_exhaustion"
		if rolling {
			want = "rolling_window"
		}
		if forecast.State != want || forecast.ExhaustsAt != nil {
			t.Fatalf("reset/rolling forecast = %+v", forecast)
		}
		if rolling && (forecast.SecondsToExhaust != nil || forecast.Basis != "") {
			t.Fatal("activity-triggered window exposed an exhaustion estimate")
		}
		advance(time.Hour)
		_, view, _ = quotaCall(t, s, id, false)
		if !rolling && windowForecast(t, view).State != "insufficient_samples" {
			t.Fatal("a passed reset retained the previous prediction")
		}
	}
}

func TestQuotaForecastWithholdsOnWindowChangeOrNonmonotonicCredits(t *testing.T) {
	changes := []struct {
		name string
		used string
		left string
		edit func(map[string]any)
	}{
		{"flat-used", "100", "800", nil}, {"decreased-used", "99", "800", nil},
		{"flat-remaining", "200", "900", nil}, {"increased-remaining", "200", "901", nil},
		{"new-policy", "200", "800", func(w map[string]any) { w["policyId"] = "p2" }},
		{"new-start", "200", "800", func(w map[string]any) { w["windowStart"] = quotaTestTime.Add(time.Second).Format(time.RFC3339) }},
		{"new-reset", "200", "800", func(w map[string]any) { w["resetsAt"] = quotaTestTime.Add(2 * time.Hour).Format(time.RFC3339) }},
		{"new-limit", "200", "800", func(w map[string]any) { w["limitCredits"] = "2000" }},
		{"new-duration", "200", "800", func(w map[string]any) { w["durationSeconds"] = 7200 }},
		{"null-start", "200", "800", func(w map[string]any) { w["windowStart"] = nil }},
		{"null-reset", "200", "800", func(w map[string]any) { w["resetsAt"] = nil }},
		{"future-start", "200", "800", func(w map[string]any) { w["windowStart"] = quotaTestTime.Add(2 * time.Minute).Format(time.RFC3339) }},
		{"passed-reset", "200", "800", func(w map[string]any) { w["resetsAt"] = quotaTestTime.Add(time.Minute).Format(time.RFC3339) }},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			s, host := newTestService(t)
			advance := quotaTestClock(s)
			id := addQuotaCredential(t, s, host, "ws-1")
			refreshQuotaWindow(t, s, host, id, "ws-1", "100", "900", nil)
			advance(time.Minute)
			view := refreshQuotaWindow(t, s, host, id, "ws-1", tc.used, tc.left, func(m map[string]any) {
				if tc.edit != nil {
					tc.edit(m["codingPlan"].(map[string]any)["windows"].([]any)[0].(map[string]any))
				}
			})
			if f := windowForecast(t, view); f.State != "insufficient_samples" || f.ExhaustsAt != nil || f.SecondsToExhaust != nil {
				t.Fatalf("unsafe prediction after %s: %+v", tc.name, f)
			}
		})
	}
}

func TestQuotaForecastErrorBreaksSampleContinuity(t *testing.T) {
	for _, failedPart := range []string{"wallet", "funding"} {
		s, host := newTestService(t)
		advance := quotaTestClock(s)
		id := addQuotaCredential(t, s, host, "ws-1")
		refreshQuotaWindow(t, s, host, id, "ws-1", "100", "900", nil)
		advance(time.Minute)
		good := refreshQuotaWindow(t, s, host, id, "ws-1", "200", "800", nil)
		advance(time.Minute)
		wallet := quotaWalletResponse("20")
		funding := fakeHTTPResponse{Status: 200, Body: mustJSON(t, officialQuotaFunding(t, "ws-1"))}
		if failedPart == "wallet" {
			wallet = fakeHTTPResponse{Status: 401}
		} else {
			funding = fakeHTTPResponse{Status: 429}
		}
		queueQuota(host, wallet, funding)
		_, failed, _ := quotaCall(t, s, id, true)
		if failed.State != "stale" || windowForecast(t, failed).State != "insufficient_samples" || windowForecast(t, good).State != "estimated" {
			t.Fatal("failed refresh retained a prediction or mutated the last returned snapshot")
		}
		advance(time.Minute)
		if f := windowForecast(t, refreshQuotaWindow(t, s, host, id, "ws-1", "300", "700", nil)); f.State != "insufficient_samples" {
			t.Fatal("first success after an error reused a pre-error sample")
		}
		advance(time.Minute)
		if f := windowForecast(t, refreshQuotaWindow(t, s, host, id, "ws-1", "400", "600", nil)); f.State != "estimated" {
			t.Fatal("two new successes did not restore estimation")
		}
	}
}

func TestQuotaForecastKeyWorkspaceAndClearChangesBreakSamples(t *testing.T) {
	for _, field := range []string{"control_key", "api_key", "workspace_id", "clear_billing"} {
		s, host := newTestService(t)
		advance := quotaTestClock(s)
		id := addQuotaCredential(t, s, host, "ws-1")
		refreshQuotaWindow(t, s, host, id, "ws-1", "100", "900", nil)
		advance(time.Minute)
		refreshQuotaWindow(t, s, host, id, "ws-1", "200", "800", nil)
		value, workspace := any("replacement-key"), "ws-1"
		if field == "workspace_id" {
			value, workspace = "ws-2", "ws-2"
		}
		if field == "clear_billing" {
			value = true
		}
		if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, field: value}); status != http.StatusOK {
			t.Fatalf("mutation %s = %d", field, status)
		}
		if field == "clear_billing" || field == "control_key" {
			managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": "quota-control-secret", "workspace_id": "ws-1"})
		}
		advance(time.Minute)
		view := refreshQuotaWindow(t, s, host, id, workspace, "300", "700", nil)
		if windowForecast(t, view).State != "insufficient_samples" {
			t.Fatalf("prediction crossed credential version change: %s", field)
		}
	}
}

func TestQuotaForecastAbsentOrDuplicateWindowCannotReuseSample(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		s, host := newTestService(t)
		advance := quotaTestClock(s)
		id := addQuotaCredential(t, s, host, "ws-1")
		refreshQuotaWindow(t, s, host, id, "ws-1", "100", "900", nil)
		advance(time.Minute)
		view := refreshQuotaWindow(t, s, host, id, "ws-1", "200", "800", func(m map[string]any) {
			plan := m["codingPlan"].(map[string]any)
			if duplicate {
				window := plan["windows"].([]any)[0]
				plan["windows"] = []any{window, window}
			} else {
				m["funding"] = "wallet"
				plan["windows"] = []any{}
			}
		})
		for _, w := range view.Funding.Data.CodingPlan.Windows {
			if w.Forecast.State != "insufficient_samples" {
				t.Fatal("ambiguous duplicate window produced a prediction")
			}
		}
		advance(time.Minute)
		if f := windowForecast(t, refreshQuotaWindow(t, s, host, id, "ws-1", "300", "700", nil)); f.State != "insufficient_samples" {
			t.Fatal("nonadjacent or ambiguous sample was reused")
		}
	}
}

func TestQuotaForecastExactBigIntegerArithmeticAndBounds(t *testing.T) {
	start, reset := quotaTestTime.Format(time.RFC3339Nano), quotaTestTime.Add(time.Hour).Format(time.RFC3339Nano)
	base := quotaWindow{PolicyID: "p", DurationSeconds: 3600, ResetMode: "anchored_recurring", WindowStart: &start, ResetsAt: &reset,
		LimitCredits: "100000000000000000000000000000000000", UsedCredits: "9007199254740993", RemainingCredits: "15"}
	previous := quotaWindowSample{used: "9007199254740992", remaining: "16", at: quotaTestTime}
	f := forecastQuotaWindow(base, previous, quotaTestTime.Add(time.Minute))
	if f.State != "estimated" || f.SecondsToExhaust == nil || *f.SecondsToExhaust != 900 {
		t.Fatalf("one-credit delta above 2^53 was rounded: %+v", f)
	}
	// Scale all credit values by 10^100; the 480-second result must be identical.
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(100), nil)
	credit := func(n int64) string { return new(big.Int).Mul(scale, big.NewInt(n)).String() }
	base.UsedCredits, base.RemainingCredits, base.LimitCredits = credit(200), credit(800), credit(1000)
	previous.used, previous.remaining = credit(100), credit(900)
	f = forecastQuotaWindow(base, previous, quotaTestTime.Add(time.Minute))
	if f.State != "estimated" || f.SecondsToExhaust == nil || *f.SecondsToExhaust != 480 {
		t.Fatalf("huge credits overflowed: %+v", f)
	}
	base.UsedCredits, base.RemainingCredits, base.LimitCredits = "4", "1", "1000"
	previous.used, previous.remaining = "1", "2"
	f = forecastQuotaWindow(base, previous, quotaTestTime.Add(60100*time.Millisecond))
	if f.SecondsToExhaust == nil || *f.SecondsToExhaust != 21 {
		t.Fatalf("fractional elapsed seconds must round exhaustion up: %+v", f)
	}
	f = forecastQuotaWindow(base, previous, quotaTestTime.Add(time.Minute-time.Nanosecond))
	if f.State != "insufficient_samples" || f.SecondsToExhaust != nil {
		t.Fatal("sub-60-second samples produced a prediction")
	}
	// A huge implied duration never converts to int64/time.Duration.
	base.UsedCredits, base.RemainingCredits, base.LimitCredits = "2", credit(800), credit(1000)
	previous.used, previous.remaining = "1", credit(900)
	f = forecastQuotaWindow(base, previous, quotaTestTime.Add(time.Minute))
	if f.State != "resets_before_exhaustion" || f.SecondsToExhaust != nil || f.ExhaustsAt != nil {
		t.Fatalf("overflow-sized estimate escaped bounds: %+v", f)
	}
	longReset := quotaTestTime.Add(2 * 366 * 24 * time.Hour).Format(time.RFC3339)
	base.ResetsAt, base.RemainingCredits = &longReset, "600000"
	previous.remaining = "600001"
	f = forecastQuotaWindow(base, previous, quotaTestTime.Add(time.Minute))
	if f.State != "beyond_horizon" || f.SecondsToExhaust != nil || f.ExhaustsAt != nil {
		t.Fatalf("forecast beyond %s seconds escaped bounds: %+v", strconv.FormatInt(maxQuotaForecastSeconds, 10), f)
	}
}

func TestQuotaForecastResetBoundaryAndZeroRemaining(t *testing.T) {
	start, reset := quotaTestTime.Format(time.RFC3339), quotaTestTime.Add(2*time.Minute).Format(time.RFC3339)
	window := quotaWindow{PolicyID: "p", DurationSeconds: 120, ResetMode: "anchored_recurring", WindowStart: &start, ResetsAt: &reset,
		UsedCredits: "2", RemainingCredits: "1", LimitCredits: "10"}
	previous := quotaWindowSample{used: "1", remaining: "2", at: quotaTestTime}
	f := forecastQuotaWindow(window, previous, quotaTestTime.Add(time.Minute))
	if f.State != "resets_before_exhaustion" || f.ExhaustsAt != nil {
		t.Fatal("estimate at the reset boundary was presented as exhaustion")
	}
	window.RemainingCredits = "0"
	f = forecastQuotaWindow(window, previous, quotaTestTime.Add(time.Minute))
	if f.State != "estimated" || f.SecondsToExhaust == nil || *f.SecondsToExhaust != 0 {
		t.Fatal("a validated actual zero remaining was confused with unknown data")
	}
}
