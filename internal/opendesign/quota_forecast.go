package opendesign

import (
	"math/big"
	"time"
)

const maxQuotaForecastSeconds int64 = 366 * 24 * 60 * 60

type quotaWindowIdentity struct {
	policy, start, reset, limit, mode string
	duration                          int64
}

type quotaWindowSample struct {
	used, remaining string
	at              time.Time
}

func quotaIdentity(w quotaWindow) (quotaWindowIdentity, bool) {
	if w.WindowStart == nil || w.ResetsAt == nil {
		return quotaWindowIdentity{}, false
	}
	return quotaWindowIdentity{policy: w.PolicyID, start: *w.WindowStart, reset: *w.ResetsAt,
		limit: w.LimitCredits, mode: w.ResetMode, duration: w.DurationSeconds}, true
}

// applyQuotaForecasts runs while mu is held, only after the credential epoch was
// checked. Samples never outlive that cache generation and are not persisted.
func applyQuotaForecasts(entry *quotaEntry, view *quotaResponse) {
	if view.Funding.Data == nil {
		entry.samples = nil
		return
	}
	data := *view.Funding.Data
	data.CodingPlan.Windows = append([]quotaWindow{}, data.CodingPlan.Windows...)
	view.Funding.Data = &data
	if view.Wallet.State != "available" || view.Funding.State != "available" || view.Funding.FetchedAt == nil {
		entry.samples = nil
		for i := range data.CodingPlan.Windows {
			data.CodingPlan.Windows[i].Forecast = quotaForecast{State: "insufficient_samples"}
		}
		return
	}
	at := *view.Funding.FetchedAt
	next := make(map[quotaWindowIdentity]quotaWindowSample)
	counts := make(map[quotaWindowIdentity]int)
	for _, window := range data.CodingPlan.Windows {
		if key, ok := quotaIdentity(window); ok {
			counts[key]++
		}
	}
	for i := range data.CodingPlan.Windows {
		window := &data.CodingPlan.Windows[i]
		window.Forecast = quotaForecast{State: "insufficient_samples"}
		if window.ResetMode == "activity_triggered" {
			window.Forecast.State = "rolling_window"
			continue
		}
		key, valid := quotaIdentity(*window)
		if !valid || counts[key] != 1 || !quotaWindowContains(key, at) {
			continue
		}
		if previous, exists := entry.samples[key]; exists {
			window.Forecast = forecastQuotaWindow(*window, previous, at)
		}
		next[key] = quotaWindowSample{used: window.UsedCredits, remaining: window.RemainingCredits, at: at}
	}
	// Replace the complete set: a window absent from one successful read cannot
	// resume a prediction against an older, nonadjacent sample.
	entry.samples = next
}

func quotaWindowContains(key quotaWindowIdentity, at time.Time) bool {
	start, startErr := time.Parse(time.RFC3339Nano, key.start)
	reset, resetErr := time.Parse(time.RFC3339Nano, key.reset)
	return startErr == nil && resetErr == nil && start.Before(reset) && !at.Before(start) && at.Before(reset)
}

func forecastQuotaWindow(window quotaWindow, previous quotaWindowSample, at time.Time) quotaForecast {
	unknown := quotaForecast{State: "insufficient_samples"}
	key, valid := quotaIdentity(window)
	if !valid || window.ResetMode != "anchored_recurring" || !quotaWindowContains(key, previous.at) || !quotaWindowContains(key, at) {
		return unknown
	}
	elapsed := quotaElapsedNanos(at, previous.at)
	if elapsed.Cmp(big.NewInt(int64(quotaRefreshInterval))) < 0 {
		return unknown
	}
	used, ok1 := new(big.Int).SetString(window.UsedCredits, 10)
	oldUsed, ok2 := new(big.Int).SetString(previous.used, 10)
	remaining, ok3 := new(big.Int).SetString(window.RemainingCredits, 10)
	oldRemaining, ok4 := new(big.Int).SetString(previous.remaining, 10)
	if !ok1 || !ok2 || !ok3 || !ok4 || remaining.Sign() < 0 || used.Cmp(oldUsed) <= 0 || remaining.Cmp(oldRemaining) >= 0 {
		return unknown
	}
	deltaUsed := new(big.Int).Sub(used, oldUsed)
	// duration (nanoseconds) = remaining * elapsed / deltaUsed. Keeping the
	// numerator/denominator integral avoids loss even above 2^53 or int64.
	numerator := new(big.Int).Mul(remaining, elapsed)
	denominator := new(big.Int).Mul(deltaUsed, big.NewInt(int64(time.Second)))
	seconds, remainder := new(big.Int), new(big.Int)
	seconds.QuoRem(numerator, denominator, remainder)
	if remainder.Sign() > 0 {
		seconds.Add(seconds, big.NewInt(1))
	}
	withinHorizon := seconds.Cmp(big.NewInt(maxQuotaForecastSeconds)) <= 0
	forecast := quotaForecast{Basis: "recent_consumption"}
	if withinHorizon {
		value := seconds.Int64()
		forecast.SecondsToExhaust = &value
	}
	reset, _ := time.Parse(time.RFC3339Nano, key.reset)
	untilReset := quotaElapsedNanos(reset, at)
	if numerator.Cmp(new(big.Int).Mul(deltaUsed, untilReset)) >= 0 {
		forecast.State = "resets_before_exhaustion"
		return forecast
	}
	if !withinHorizon {
		forecast.State = "beyond_horizon"
		return forecast
	}
	exhaustsAt := at.Add(time.Duration(*forecast.SecondsToExhaust) * time.Second)
	// Ceiling to a whole second must not place the displayed estimate past the
	// reset even when the exact rational duration was just below it.
	if !exhaustsAt.Before(reset) {
		forecast.State = "resets_before_exhaustion"
		return forecast
	}
	forecast.State, forecast.ExhaustsAt = "estimated", &exhaustsAt
	return forecast
}

// Compute differences without time.Duration overflow for far-apart timestamps.
func quotaElapsedNanos(later, earlier time.Time) *big.Int {
	seconds := new(big.Int).Sub(big.NewInt(later.Unix()), big.NewInt(earlier.Unix()))
	seconds.Mul(seconds, big.NewInt(int64(time.Second)))
	return seconds.Add(seconds, big.NewInt(int64(later.Nanosecond()-earlier.Nanosecond())))
}

// A cache read cannot assert that an old prediction has actually exhausted a
// window. Hide estimates once their time/reset passed until another explicit
// refresh provides evidence; leave the successful quota snapshot intact.
func quotaViewAt(view quotaResponse, now time.Time) quotaResponse {
	if view.Funding.Data == nil {
		return view
	}
	var windows []quotaWindow
	for i, window := range view.Funding.Data.CodingPlan.Windows {
		forecast := window.Forecast
		if forecast.State == "insufficient_samples" || forecast.State == "rolling_window" {
			continue
		}
		key, valid := quotaIdentity(window)
		if !valid || !quotaWindowContains(key, now) || forecast.ExhaustsAt != nil && !now.Before(*forecast.ExhaustsAt) {
			if windows == nil {
				windows = append([]quotaWindow{}, view.Funding.Data.CodingPlan.Windows...)
			}
			windows[i].Forecast = quotaForecast{State: "insufficient_samples"}
		}
	}
	if windows != nil {
		data := *view.Funding.Data
		data.CodingPlan.Windows = windows
		view.Funding.Data = &data
	}
	return view
}
