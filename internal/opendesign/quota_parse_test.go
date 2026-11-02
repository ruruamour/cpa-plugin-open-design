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
		`{"balanceUsd":"1","updatedAt":"invalid"}`, `{"balanceUsd":"1","updatedAt":2}`, `{"balanceUsd":"1"} {}`,
		`{"BalanceUsd":"1"}`,
	} {
		if _, valid := parseQuotaWallet([]byte(raw)); valid {
			t.Fatalf("invalid wallet accepted: %s", raw)
		}
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
