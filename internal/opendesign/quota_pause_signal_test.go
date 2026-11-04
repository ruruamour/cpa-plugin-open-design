package opendesign

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestQuotaPauseSignalRequiresStructuredExhaustion(t *testing.T) {
	now := time.Date(2026, time.November, 4, 10, 0, 0, 0, time.UTC)
	reset := "2026-11-04T15:00:00Z"
	cases := []struct {
		name    string
		status  int
		body    string
		blocked bool
	}{
		{"real coding plan exhaustion", 402, `{"error":{"code":"insufficient_balance","details":{"coding_plan_exhausted":true,"reset_at":"` + reset + `"}}}`, true},
		{"explicit balance error", 402, `{"error":{"code":"insufficient_balance"}}`, true},
		{"bare payment required", 402, `{"error":{"message":"payment required"}}`, false},
		{"message marker only", 402, `{"error":{"message":"[code=insufficient_balance]"}}`, false},
		{"unknown balance code", 402, `{"error":{"code":"insufficient_balance_evil"}}`, false},
		{"ordinary rate limit", 429, `{"error":{"code":"rate_limit_exceeded"}}`, false},
		{"unverified insufficient quota", 429, `{"error":{"code":"insufficient_quota"}}`, false},
		{"other status", 403, `{"error":{"code":"insufficient_balance"}}`, false},
		{"success body without plan flag", 200, `{"error":{"code":"insufficient_balance"}}`, false},
		{"success body with plan flag", 200, `{"error":{"code":"insufficient_balance","details":{"coding_plan_exhausted":true}}}`, true},
		{"invalid JSON", 402, `{`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotReset, blocked := quotaPauseSignal(tc.status, nil, []byte(tc.body), now)
			if blocked != tc.blocked {
				t.Fatalf("blocked = %t, want %t", blocked, tc.blocked)
			}
			if tc.name == "real coding plan exhaustion" && !gotReset.Equal(now.Add(5*time.Hour)) {
				t.Fatalf("reset = %s, want %s", gotReset, now.Add(5*time.Hour))
			}
		})
	}
}

func TestUpstreamErrorCarriesOnlyStructuredQuotaPauseSignal(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		exhausted bool
	}{
		{"structured", 402, `{"error":{"code":"insufficient_balance"}}`, true},
		{"bare", 402, `{"error":{"message":"payment required"}}`, false},
		{"message marker", 402, `{"error":{"message":"[code=insufficient_balance]"}}`, false},
		{"rate limit", 429, `{"error":{"code":"rate_limit_exceeded"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := upstreamErrorFromResponse(tc.status, nil, []byte(tc.body))
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.quotaExhausted != tc.exhausted {
				t.Fatalf("quota signal mismatch for status %d", tc.status)
			}
		})
	}
}

func TestQuotaPauseSignalPrefersStructuredResetToRetryAfter(t *testing.T) {
	now := time.Date(2026, time.November, 4, 10, 0, 0, 0, time.UTC)
	headers := http.Header{"Retry-After": []string{"3600"}}
	body := []byte(`{"error":{"code":"insufficient_balance","details":{"reset_at":"2026-11-04T15:00:00Z"}}}`)
	reset, ok := quotaPauseSignal(402, headers, body, now)
	if !ok || !reset.Equal(now.Add(5*time.Hour)) {
		t.Fatalf("structured reset = %s, allowed = %t", reset, ok)
	}
	reset, ok = quotaPauseSignal(402, headers, []byte(`{"error":{"code":"insufficient_balance"}}`), now)
	if !ok || !reset.Equal(now.Add(time.Hour)) {
		t.Fatalf("header fallback = %s, allowed = %t", reset, ok)
	}
}
