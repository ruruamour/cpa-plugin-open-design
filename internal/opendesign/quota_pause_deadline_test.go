package opendesign

import (
	"net/http"
	"testing"
	"time"
)

// quotaPauseNow is a fixed, sub-second-free instant so table expectations are exact.
var quotaPauseNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func quotaResetJSON(t *testing.T, details map[string]any) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{"error": map[string]any{"details": details}})
}

func TestQuotaPauseDeadlineTable(t *testing.T) {
	on := pauseSettings{enabled: true, defaultWindow: 30 * time.Minute, maxWindow: 24 * time.Hour}
	off := on
	off.enabled = false
	cases := []struct {
		name     string
		resetAt  time.Time
		settings pauseSettings
		want     time.Time
	}{
		{
			name:     "future reset inside window wins",
			resetAt:  quotaPauseNow.Add(90 * time.Minute),
			settings: on,
			want:     quotaPauseNow.Add(90 * time.Minute),
		},
		{
			name:     "missing reset uses default window",
			resetAt:  time.Time{},
			settings: on,
			want:     quotaPauseNow.Add(30 * time.Minute),
		},
		{
			name:     "expired reset uses default window",
			resetAt:  quotaPauseNow.Add(-time.Hour),
			settings: on,
			want:     quotaPauseNow.Add(30 * time.Minute),
		},
		{
			name:     "reset exactly now is not future and uses default window",
			resetAt:  quotaPauseNow,
			settings: on,
			want:     quotaPauseNow.Add(30 * time.Minute),
		},
		{
			name:     "reset closer than minimum is raised to the minimum",
			resetAt:  quotaPauseNow.Add(10 * time.Second),
			settings: on,
			want:     quotaPauseNow.Add(quotaPauseMinWindow),
		},
		{
			name:     "reset beyond configured max is clamped to max",
			resetAt:  quotaPauseNow.Add(48 * time.Hour),
			settings: on,
			want:     quotaPauseNow.Add(24 * time.Hour),
		},
		{
			name:     "configured max takes priority over a larger default",
			resetAt:  time.Time{},
			settings: pauseSettings{enabled: true, defaultWindow: 48 * time.Hour, maxWindow: 2 * time.Hour},
			want:     quotaPauseNow.Add(2 * time.Hour),
		},
		{
			name:     "configured max takes priority over a later reset",
			resetAt:  quotaPauseNow.Add(10 * time.Hour),
			settings: pauseSettings{enabled: true, defaultWindow: 48 * time.Hour, maxWindow: 2 * time.Hour},
			want:     quotaPauseNow.Add(2 * time.Hour),
		},
		{
			name:     "reset exactly at the max is kept",
			resetAt:  quotaPauseNow.Add(24 * time.Hour),
			settings: on,
			want:     quotaPauseNow.Add(24 * time.Hour),
		},
		{
			name:     "disabled pause resolves the same deadline",
			resetAt:  quotaPauseNow.Add(90 * time.Minute),
			settings: off,
			want:     quotaPauseNow.Add(90 * time.Minute),
		},
		{
			name:     "disabled pause still uses the default window",
			resetAt:  time.Time{},
			settings: off,
			want:     quotaPauseNow.Add(30 * time.Minute),
		},
		{
			name:     "default shorter than minimum is raised to the minimum",
			resetAt:  time.Time{},
			settings: pauseSettings{enabled: true, defaultWindow: 10 * time.Second, maxWindow: 24 * time.Hour},
			want:     quotaPauseNow.Add(quotaPauseMinWindow),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := quotaPauseDeadline(quotaPauseNow, tc.resetAt, tc.settings)
			if !got.Equal(tc.want) {
				t.Fatalf("quotaPauseDeadline(%v, %v) = %v, want %v", tc.resetAt, tc.settings, got, tc.want)
			}
			if got.Location() != time.UTC || got.Nanosecond() != 0 {
				t.Fatalf("deadline %v is not UTC second precision", got)
			}
		})
	}
}

func TestQuotaPauseDeadlineDisabledDoesNotChangeResolution(t *testing.T) {
	on := pauseSettings{enabled: true, defaultWindow: 30 * time.Minute, maxWindow: 24 * time.Hour}
	off := on
	off.enabled = false
	for _, resetAt := range []time.Time{
		time.Time{},
		quotaPauseNow.Add(-time.Minute),
		quotaPauseNow,
		quotaPauseNow.Add(10 * time.Second),
		quotaPauseNow.Add(2 * time.Hour),
		quotaPauseNow.Add(30 * 24 * time.Hour),
	} {
		if onDeadline, offDeadline := quotaPauseDeadline(quotaPauseNow, resetAt, on), quotaPauseDeadline(quotaPauseNow, resetAt, off); !onDeadline.Equal(offDeadline) {
			t.Fatalf("enabled/disabled deadlines differ for resetAt %v: %v vs %v", resetAt, onDeadline, offDeadline)
		}
	}
}

func TestQuotaResetTimeFromBodyTable(t *testing.T) {
	future := quotaPauseNow.Add(2 * time.Hour).Format(time.RFC3339)
	past := quotaPauseNow.Add(-time.Hour).Format(time.RFC3339)
	atSevenDays := quotaPauseNow.Add(quotaPauseMaxWindow).Format(time.RFC3339)
	atSevenDaysPlusOne := quotaPauseNow.Add(quotaPauseMaxWindow + time.Second).Format(time.RFC3339)
	atThirtyDays := quotaPauseNow.Add(30 * 24 * time.Hour).Format(time.RFC3339)
	cases := []struct {
		name string
		body []byte
		want time.Time
	}{
		{
			name: "reset_at RFC3339",
			body: quotaResetJSON(t, map[string]any{"reset_at": future}),
			want: quotaPauseNow.Add(2 * time.Hour),
		},
		{
			name: "resets_at RFC3339",
			body: quotaResetJSON(t, map[string]any{"resets_at": future}),
			want: quotaPauseNow.Add(2 * time.Hour),
		},
		{
			name: "resetAt camelCase",
			body: quotaResetJSON(t, map[string]any{"resetAt": future}),
			want: quotaPauseNow.Add(2 * time.Hour),
		},
		{
			name: "resetsAt camelCase",
			body: quotaResetJSON(t, map[string]any{"resetsAt": future}),
			want: quotaPauseNow.Add(2 * time.Hour),
		},
		{
			name: "resets at textual shape",
			body: quotaResetJSON(t, map[string]any{"reset_at": "resets at " + future}),
			want: quotaPauseNow.Add(2 * time.Hour),
		},
		{
			name: "resets at textual shape with trailing period",
			body: quotaResetJSON(t, map[string]any{"resets_at": "Resets at " + future + "."}),
			want: quotaPauseNow.Add(2 * time.Hour),
		},
		{
			name: "expired reset is ignored",
			body: quotaResetJSON(t, map[string]any{"reset_at": past}),
			want: time.Time{},
		},
		{
			name: "reset exactly now is not future",
			body: quotaResetJSON(t, map[string]any{"reset_at": quotaPauseNow.Format(time.RFC3339)}),
			want: time.Time{},
		},
		{
			name: "missing reset",
			body: quotaResetJSON(t, map[string]any{"something_else": future}),
			want: time.Time{},
		},
		{
			name: "missing details",
			body: mustJSON(t, map[string]any{"error": map[string]any{}}),
			want: time.Time{},
		},
		{
			name: "missing error envelope",
			body: mustJSON(t, map[string]any{"details": map[string]any{"reset_at": future}}),
			want: time.Time{},
		},
		{
			name: "empty body",
			body: nil,
			want: time.Time{},
		},
		{
			name: "invalid timestamp",
			body: quotaResetJSON(t, map[string]any{"reset_at": "not-a-time"}),
			want: time.Time{},
		},
		{
			name: "non RFC3339 timestamp",
			body: quotaResetJSON(t, map[string]any{"reset_at": "2026-01-02 05:04:05"}),
			want: time.Time{},
		},
		{
			name: "numeric timestamp is rejected",
			body: quotaResetJSON(t, map[string]any{"reset_at": 12345}),
			want: time.Time{},
		},
		{
			name: "invalid first field falls through to next field",
			body: quotaResetJSON(t, map[string]any{"reset_at": "bad", "resets_at": future}),
			want: quotaPauseNow.Add(2 * time.Hour),
		},
		{
			name: "reset_at wins over resets_at",
			body: quotaResetJSON(t, map[string]any{"reset_at": future, "resets_at": quotaPauseNow.Add(3 * time.Hour).Format(time.RFC3339)}),
			want: quotaPauseNow.Add(2 * time.Hour),
		},
		{
			name: "exactly seven days is kept",
			body: quotaResetJSON(t, map[string]any{"reset_at": atSevenDays}),
			want: quotaPauseNow.Add(quotaPauseMaxWindow),
		},
		{
			name: "beyond seven days is clamped to seven days",
			body: quotaResetJSON(t, map[string]any{"reset_at": atSevenDaysPlusOne}),
			want: quotaPauseNow.Add(quotaPauseMaxWindow),
		},
		{
			name: "far future is clamped to seven days not zero",
			body: quotaResetJSON(t, map[string]any{"reset_at": atThirtyDays}),
			want: quotaPauseNow.Add(quotaPauseMaxWindow),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := quotaResetTimeFromBody(tc.body, quotaPauseNow)
			if !got.Equal(tc.want) {
				t.Fatalf("quotaResetTimeFromBody(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestQuotaResetTimeFromHeadersTable(t *testing.T) {
	futureDate := quotaPauseNow.Add(2 * time.Hour).Format(http.TimeFormat)
	pastDate := quotaPauseNow.Add(-time.Hour).Format(http.TimeFormat)
	sevenDayDate := quotaPauseNow.Add(quotaPauseMaxWindow).Format(http.TimeFormat)
	thirtyDayDate := quotaPauseNow.Add(30 * 24 * time.Hour).Format(http.TimeFormat)
	cases := []struct {
		name    string
		headers http.Header
		want    time.Time
	}{
		{
			name:    "retry-after seconds",
			headers: http.Header{"Retry-After": []string{"120"}},
			want:    quotaPauseNow.Add(120 * time.Second),
		},
		{
			name:    "retry-after seconds with surrounding space",
			headers: http.Header{"Retry-After": []string{"  120  "}},
			want:    quotaPauseNow.Add(120 * time.Second),
		},
		{
			name:    "retry-after zero",
			headers: http.Header{"Retry-After": []string{"0"}},
			want:    time.Time{},
		},
		{
			name:    "retry-after negative",
			headers: http.Header{"Retry-After": []string{"-30"}},
			want:    time.Time{},
		},
		{
			name:    "retry-after invalid text",
			headers: http.Header{"Retry-After": []string{"soon"}},
			want:    time.Time{},
		},
		{
			name:    "retry-after fractional seconds is invalid",
			headers: http.Header{"Retry-After": []string{"60.5"}},
			want:    time.Time{},
		},
		{
			name:    "retry-after HTTP-date",
			headers: http.Header{"Retry-After": []string{futureDate}},
			want:    quotaPauseNow.Add(2 * time.Hour),
		},
		{
			name:    "retry-after expired HTTP-date",
			headers: http.Header{"Retry-After": []string{pastDate}},
			want:    time.Time{},
		},
		{
			name:    "retry-after HTTP-date exactly seven days is kept",
			headers: http.Header{"Retry-After": []string{sevenDayDate}},
			want:    quotaPauseNow.Add(quotaPauseMaxWindow),
		},
		{
			name:    "retry-after HTTP-date beyond seven days is clamped",
			headers: http.Header{"Retry-After": []string{thirtyDayDate}},
			want:    quotaPauseNow.Add(quotaPauseMaxWindow),
		},
		{
			name:    "retry-after seconds beyond seven days is clamped",
			headers: http.Header{"Retry-After": []string{"31536000"}},
			want:    quotaPauseNow.Add(quotaPauseMaxWindow),
		},
		{
			name:    "retry-after huge seconds overflowing duration is clamped",
			headers: http.Header{"Retry-After": []string{"9223372036854775807"}},
			want:    quotaPauseNow.Add(quotaPauseMaxWindow),
		},
		{
			name:    "retry-after seconds above uint64 are clamped without overflow",
			headers: http.Header{"Retry-After": []string{"99999999999999999999999999"}},
			want:    quotaPauseNow.Add(quotaPauseMaxWindow),
		},
		{
			name:    "missing header",
			headers: http.Header{},
			want:    time.Time{},
		},
		{
			name:    "nil headers",
			headers: nil,
			want:    time.Time{},
		},
		{
			name:    "empty header value",
			headers: http.Header{"Retry-After": []string{"   "}},
			want:    time.Time{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := quotaResetTimeFromHeaders(tc.headers, quotaPauseNow)
			if !got.Equal(tc.want) {
				t.Fatalf("quotaResetTimeFromHeaders(%v) = %v, want %v", tc.headers, got, tc.want)
			}
		})
	}
}

func TestBoundedQuotaResetTable(t *testing.T) {
	fixed := time.FixedZone("UTC+8", 8*60*60)
	cases := []struct {
		name  string
		value time.Time
		want  time.Time
	}{
		{
			name:  "future inside window is kept",
			value: quotaPauseNow.Add(time.Hour),
			want:  quotaPauseNow.Add(time.Hour),
		},
		{
			name:  "future exactly at max is kept",
			value: quotaPauseNow.Add(quotaPauseMaxWindow),
			want:  quotaPauseNow.Add(quotaPauseMaxWindow),
		},
		{
			name:  "future beyond max is clamped to max not zero",
			value: quotaPauseNow.Add(quotaPauseMaxWindow + time.Second),
			want:  quotaPauseNow.Add(quotaPauseMaxWindow),
		},
		{
			name:  "far future is clamped to max not zero",
			value: quotaPauseNow.Add(10 * 365 * 24 * time.Hour),
			want:  quotaPauseNow.Add(quotaPauseMaxWindow),
		},
		{
			name:  "expired value becomes zero",
			value: quotaPauseNow.Add(-time.Second),
			want:  time.Time{},
		},
		{
			name:  "value exactly now becomes zero",
			value: quotaPauseNow,
			want:  time.Time{},
		},
		{
			name:  "zero value stays zero",
			value: time.Time{},
			want:  time.Time{},
		},
		{
			name:  "non UTC value is returned in UTC",
			value: quotaPauseNow.Add(time.Hour).In(fixed),
			want:  quotaPauseNow.Add(time.Hour),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := boundedQuotaReset(tc.value, quotaPauseNow)
			if !got.Equal(tc.want) {
				t.Fatalf("boundedQuotaReset(%v, now) = %v, want %v", tc.value, got, tc.want)
			}
			if !got.IsZero() && got.Location() != time.UTC {
				t.Fatalf("boundedQuotaReset returned non-UTC %v", got)
			}
		})
	}
}
