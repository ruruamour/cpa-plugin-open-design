package opendesign

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQuotaWalletValidation(t *testing.T) {
	for _, balance := range []string{"0", "00012.3400", "999999999999999999999999999999.123456789"} {
		for _, updated := range []any{nil, "2026-11-03T00:00:00.123Z"} {
			data, valid := parseQuotaWallet(mustJSON(t, map[string]any{"balanceUsd": balance, "updatedAt": updated, "extra": "ignored"}))
			if !valid || data.BalanceUSD != balance {
				t.Fatalf("valid wallet balance rejected: %q", balance)
			}
		}
	}
	if data, valid := parseQuotaWallet([]byte(`{"balanceUsd":"1.20"}`)); !valid || data.UpdatedAt != nil {
		t.Fatal("optional updatedAt must remain null")
	}
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"balanceUsd":0}`, `{"balanceUsd":null}`, `{"balanceUsd":"-1"}`, `{"balanceUsd":"+1"}`,
		`{"balanceUsd":"1e2"}`, `{"balanceUsd":" 1"}`, `{"balanceUsd":".1"}`, `{"balanceUsd":"1."}`,
		`{"balanceUsd":"1"} {}`,
		`{"BalanceUsd":"1"}`,
	} {
		if _, valid := parseQuotaWallet([]byte(raw)); valid {
			t.Fatalf("invalid wallet accepted: %s", raw)
		}
	}
}

// The official reader (vela-wallet.ts at 5a0c477) requires only a valid
// balanceUsd and reads updatedAt as `typeof v === 'string' ? v : null`. A
// timestamp the plugin cannot use is therefore unknown metadata, not a reason
// to discard a balance the upstream returned.
func TestQuotaWalletKeepsBalanceWhenUpdatedAtIsUnusable(t *testing.T) {
	for name, raw := range map[string]string{
		"not a timestamp": `{"balanceUsd":"12.34","updatedAt":"invalid"}`,
		"number":          `{"balanceUsd":"12.34","updatedAt":1762765500}`,
		"empty string":    `{"balanceUsd":"12.34","updatedAt":""}`,
		"space separated": `{"balanceUsd":"12.34","updatedAt":"2026-11-10 09:05:00"}`,
		"no zone":         `{"balanceUsd":"12.34","updatedAt":"2026-11-10T09:05:00"}`,
		"date only":       `{"balanceUsd":"12.34","updatedAt":"2026-11-10"}`,
		"object":          `{"balanceUsd":"12.34","updatedAt":{"seconds":1}}`,
		"boolean":         `{"balanceUsd":"12.34","updatedAt":false}`,
	} {
		data, reason := parseQuotaWalletReason([]byte(raw))
		if reason != "" || data == nil {
			t.Errorf("%s: balance rejected with reason %q: %s", name, reason, raw)
			continue
		}
		if data.BalanceUSD != "12.34" {
			t.Errorf("%s: balance = %q", name, data.BalanceUSD)
		}
		// Unusable metadata is unknown; it is never passed through as text.
		if data.UpdatedAt != nil {
			t.Errorf("%s: unusable updatedAt was kept as %q", name, *data.UpdatedAt)
		}
	}
	// A usable timestamp is still reported.
	data, reason := parseQuotaWalletReason([]byte(`{"balanceUsd":"1","updatedAt":"2026-11-03T00:00:00.123Z"}`))
	if reason != "" || data.UpdatedAt == nil || *data.UpdatedAt != "2026-11-03T00:00:00.123Z" {
		t.Fatalf("valid updatedAt lost: %#v reason %q", data, reason)
	}
}

// Each rejection names the check that failed with a fixed code, so a
// "malformed" wallet is diagnosable without ever exposing the upstream body.
func TestQuotaWalletRejectionReasons(t *testing.T) {
	for raw, want := range map[string]string{
		`null`:                        "not_json_object",
		`[]`:                          "not_json_object",
		`"12.34"`:                     "not_json_object",
		`<html>502</html>`:            "not_json_object",
		``:                            "not_json_object",
		`{"balanceUsd":"1"} {}`:       "not_json_object",
		`{}`:                          "balance_missing",
		`{"BalanceUsd":"1"}`:          "balance_missing",
		`{"data":{"balanceUsd":"1"}}`: "balance_missing",
		`{"balance":"1"}`:             "balance_missing",
		`{"balanceUsd":0}`:            "balance_not_string",
		`{"balanceUsd":12.34}`:        "balance_not_string",
		`{"balanceUsd":null}`:         "balance_not_string",
		`{"balanceUsd":"-1"}`:         "balance_format",
		`{"balanceUsd":"+1"}`:         "balance_format",
		`{"balanceUsd":"1e2"}`:        "balance_format",
		`{"balanceUsd":" 1"}`:         "balance_format",
		`{"balanceUsd":".1"}`:         "balance_format",
		`{"balanceUsd":"1."}`:         "balance_format",
		`{"balanceUsd":""}`:           "balance_format",
		`{"balanceUsd":"$12.34"}`:     "balance_format",
		`{"balanceUsd":"1,234.50"}`:   "balance_format",
	} {
		data, reason := parseQuotaWalletReason([]byte(raw))
		if reason != want || data != nil {
			t.Errorf("%q: reason = %q data = %v, want %q and no data", raw, reason, data, want)
		}
		if _, valid := parseQuotaWallet([]byte(raw)); valid {
			t.Errorf("%q: parseQuotaWallet accepted a rejected body", raw)
		}
	}
	oversized := []byte(`{"balanceUsd":"1","pad":"` + strings.Repeat("x", maxQuotaBody) + `"}`)
	if _, reason := parseQuotaWalletReason(oversized); reason != "body_too_large" {
		t.Fatalf("oversized body reason = %q", reason)
	}
	huge := []byte(`{"balanceUsd":"` + strings.Repeat("9", 4097) + `"}`)
	if _, reason := parseQuotaWalletReason(huge); reason != "balance_format" {
		t.Fatalf("overlong balance reason = %q", reason)
	}
}

func TestQuotaFundingOfficialOptionalAndNullableFields(t *testing.T) {
	for _, covered := range []any{true, false, nil} {
		for _, tier := range []any{"go", "plus", "pro", "max", nil} {
			m := officialQuotaFunding(t, "ws-1")
			m["modelCovered"] = covered
			plan := m["codingPlan"].(map[string]any)
			plan["tier"] = tier
			window := plan["windows"].([]any)[0].(map[string]any)
			window["windowStart"], window["resetsAt"] = nil, nil
			data, valid := parseQuotaFunding(mustJSON(t, m), "ws-1")
			if !valid || data.FundingCheck != nil || data.CodingPlan.Windows[0].WindowStart != nil || data.CodingPlan.Windows[0].ResetsAt != nil {
				t.Fatal("official optional/nullable field was treated as required/nonnullable")
			}
		}
	}
	for _, check := range []string{"available", "insufficient", "unknown"} {
		m := officialQuotaFunding(t, "ws-1")
		m["fundingCheck"] = check
		data, valid := parseQuotaFunding(mustJSON(t, m), "ws-1")
		if !valid || data.FundingCheck == nil || *data.FundingCheck != check {
			t.Fatal("valid optional fundingCheck rejected")
		}
	}
	for _, funding := range []string{"wallet", "gateway"} {
		m := officialQuotaFunding(t, "ws-1")
		m["funding"] = funding
		m["codingPlan"].(map[string]any)["windows"] = []any{}
		data, valid := parseQuotaFunding(mustJSON(t, m), "ws-1")
		if !valid || data.CodingPlan.Windows == nil || len(data.CodingPlan.Windows) != 0 {
			t.Fatal("non-plan funding must allow an explicitly empty window list")
		}
	}
}

func TestQuotaFundingRejectsMissingRequiredFields(t *testing.T) {
	fields := []struct {
		level string
		names []string
	}{
		{"root", []string{"schemaVersion", "workspaceId", "workspaceMemberId", "modelId", "generatedAt", "balanceUsd", "modelCovered", "funding", "codingPlan"}},
		{"plan", []string{"workspaceId", "generatedAt", "eligible", "tier", "windows"}},
		{"window", []string{"policyId", "durationSeconds", "resetMode", "usedCredits", "limitCredits", "remainingCredits", "windowStart", "resetsAt"}},
	}
	for _, group := range fields {
		for _, field := range group.names {
			t.Run(group.level+"/"+field, func(t *testing.T) {
				m := officialQuotaFunding(t, "ws-1")
				object := m
				if group.level != "root" {
					object = m["codingPlan"].(map[string]any)
				}
				if group.level == "window" {
					object = object["windows"].([]any)[0].(map[string]any)
				}
				delete(object, field)
				if _, valid := parseQuotaFunding(mustJSON(t, m), "ws-1"); valid {
					t.Fatal("missing official required field was accepted")
				}
			})
		}
	}
}

func TestQuotaFundingRejectsMalformedFields(t *testing.T) {
	cases := []struct {
		level string
		field string
		value any
	}{
		{"root", "schemaVersion", 2}, {"root", "schemaVersion", "1"}, {"root", "workspaceId", "different"},
		{"root", "workspaceMemberId", "  "}, {"root", "modelId", "model-was-not-requested"}, {"root", "generatedAt", "bad-date"},
		{"root", "balanceUsd", "1e3"}, {"root", "balanceUsd", 0}, {"root", "modelCovered", "false"},
		{"root", "funding", "other"}, {"root", "fundingCheck", "other"}, {"root", "fundingCheck", nil},
		{"root", "codingPlan", nil}, {"plan", "workspaceId", "different"}, {"plan", "generatedAt", nil},
		{"plan", "eligible", nil}, {"plan", "eligible", "true"}, {"plan", "tier", "unverified-tier"},
		{"plan", "windows", nil}, {"plan", "windows", []any{}},
		{"window", "policyId", ""}, {"window", "policyId", 2}, {"window", "durationSeconds", 0},
		{"window", "durationSeconds", -1}, {"window", "durationSeconds", 1.5}, {"window", "durationSeconds", "60"},
		{"window", "durationSeconds", json.Number("9007199254740992")}, {"window", "resetMode", "rolling"},
		{"window", "usedCredits", "-1"}, {"window", "usedCredits", 100}, {"window", "usedCredits", "1.5"},
		{"window", "limitCredits", "0"}, {"window", "remainingCredits", "1001"}, {"window", "remainingCredits", nil},
		{"window", "windowStart", "bad-date"}, {"window", "resetsAt", 0},
	}
	for _, tc := range cases {
		t.Run(tc.level+"/"+tc.field, func(t *testing.T) {
			m := officialQuotaFunding(t, "ws-1")
			object := m
			if tc.level != "root" {
				object = m["codingPlan"].(map[string]any)
			}
			if tc.level == "window" {
				object = object["windows"].([]any)[0].(map[string]any)
			}
			object[tc.field] = tc.value
			if _, valid := parseQuotaFunding(mustJSON(t, m), "ws-1"); valid {
				t.Fatalf("malformed %s was accepted", tc.field)
			}
		})
	}
}

func TestQuotaFundingPreservesBigCreditStringsAndOfficialOverage(t *testing.T) {
	m := officialQuotaFunding(t, "ws-1")
	window := m["codingPlan"].(map[string]any)["windows"].([]any)[0].(map[string]any)
	limit := "1" + strings.Repeat("0", 120)
	used := "9" + strings.Repeat("0", 120) // The official validator allows usage above the limit.
	remaining := strings.Repeat("9", 119)
	window["limitCredits"], window["usedCredits"], window["remainingCredits"] = limit, used, remaining
	data, valid := parseQuotaFunding(mustJSON(t, m), "ws-1")
	if !valid || data.CodingPlan.Windows[0].LimitCredits != limit || data.CodingPlan.Windows[0].UsedCredits != used || data.CodingPlan.Windows[0].RemainingCredits != remaining {
		t.Fatal("large official credit values were rounded or an unsupported sum constraint was applied")
	}
	window["remainingCredits"] = "2" + strings.Repeat("0", 120)
	if _, valid := parseQuotaFunding(mustJSON(t, m), "ws-1"); valid {
		t.Fatal("large remainingCredits above the limit was accepted")
	}
}
