package opendesign

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"regexp"
	"strings"
	"time"
)

var (
	quotaUnsignedDecimal = regexp.MustCompile(`^\d+(?:\.\d+)?$`)
	quotaSignedDecimal   = regexp.MustCompile(`^-?\d+(?:\.\d+)?$`)
	quotaCredits         = regexp.MustCompile(`^\d+$`)
)

// These validators follow the official vela-wallet.ts reader and
// vela-billing.ts parseBillingPreflight at 5a0c477. Unknown fields are ignored;
// only explicitly validated fields are copied into management responses.
func parseQuotaWallet(body []byte) (*quotaWalletData, bool) {
	m, ok := quotaObject(body)
	if !ok {
		return nil, false
	}
	balance, ok := quotaString(m["balanceUsd"])
	if !ok || len(balance) > 4096 || !quotaUnsignedDecimal.MatchString(balance) {
		return nil, false
	}
	updatedAt, ok := quotaNullableTime(m["updatedAt"], true)
	if !ok {
		return nil, false
	}
	return &quotaWalletData{BalanceUSD: balance, UpdatedAt: updatedAt}, true
}

func parseQuotaFunding(body []byte, workspaceID string) (*quotaFundingData, bool) {
	m, ok := quotaObject(body)
	if !ok || !quotaNumberEquals(m["schemaVersion"], 1) || !quotaStringEquals(m["workspaceId"], workspaceID) ||
		!bytes.Equal(bytes.TrimSpace(m["modelId"]), []byte("null")) {
		return nil, false
	}
	member, ok := quotaString(m["workspaceMemberId"])
	if !ok || strings.TrimSpace(member) == "" || !quotaTimeField(m["generatedAt"]) {
		return nil, false
	}
	balance, ok := quotaString(m["balanceUsd"])
	if !ok || len(balance) > 4096 || !quotaSignedDecimal.MatchString(balance) {
		return nil, false
	}
	funding, ok := quotaEnum(m["funding"], "coding_plan", "wallet", "gateway")
	if !ok {
		return nil, false
	}
	var covered *bool
	if !bytes.Equal(bytes.TrimSpace(m["modelCovered"]), []byte("null")) {
		value, valid := quotaBool(m["modelCovered"])
		if !valid {
			return nil, false
		}
		covered = &value
	}
	var fundingCheck *string
	if raw, exists := m["fundingCheck"]; exists {
		value, valid := quotaEnum(raw, "available", "insufficient", "unknown")
		if !valid {
			return nil, false
		}
		fundingCheck = &value
	}
	plan, ok := quotaObject(m["codingPlan"])
	if !ok || !quotaStringEquals(plan["workspaceId"], workspaceID) || !quotaTimeField(plan["generatedAt"]) {
		return nil, false
	}
	if _, ok := quotaBool(plan["eligible"]); !ok {
		return nil, false
	}
	var tier *string
	if !bytes.Equal(bytes.TrimSpace(plan["tier"]), []byte("null")) {
		value, valid := quotaEnum(plan["tier"], "go", "plus", "pro", "max")
		if !valid {
			return nil, false
		}
		tier = &value
	}
	var rawWindows []json.RawMessage
	raw := bytes.TrimSpace(plan["windows"])
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &rawWindows) != nil || (funding == "coding_plan" && len(rawWindows) == 0) {
		return nil, false
	}
	windows := make([]quotaWindow, 0, len(rawWindows))
	for _, raw := range rawWindows {
		window, valid := parseQuotaWindow(raw)
		if !valid {
			return nil, false
		}
		windows = append(windows, window)
	}
	return &quotaFundingData{BalanceUSD: balance, Funding: funding, ModelCovered: covered,
		FundingCheck: fundingCheck, CodingPlan: quotaCodingPlan{Tier: tier, Windows: windows}}, true
}

func parseQuotaWindow(raw []byte) (quotaWindow, bool) {
	var w quotaWindow
	m, ok := quotaObject(raw)
	if !ok {
		return w, false
	}
	if w.PolicyID, ok = quotaString(m["policyId"]); !ok || w.PolicyID == "" {
		return w, false
	}
	var duration float64
	if json.Unmarshal(m["durationSeconds"], &duration) != nil || duration <= 0 || duration > 9007199254740991 || math.Trunc(duration) != duration {
		return w, false
	}
	w.DurationSeconds = int64(duration)
	if w.ResetMode, ok = quotaEnum(m["resetMode"], "activity_triggered", "anchored_recurring"); !ok {
		return w, false
	}
	for name, dest := range map[string]*string{"usedCredits": &w.UsedCredits, "limitCredits": &w.LimitCredits, "remainingCredits": &w.RemainingCredits} {
		if *dest, ok = quotaString(m[name]); !ok || len(*dest) > 4096 || !quotaCredits.MatchString(*dest) {
			return w, false
		}
	}
	limit, _ := new(big.Int).SetString(w.LimitCredits, 10)
	remaining, _ := new(big.Int).SetString(w.RemainingCredits, 10)
	if limit.Sign() <= 0 || remaining.Cmp(limit) > 0 {
		return w, false
	}
	if w.WindowStart, ok = quotaNullableTime(m["windowStart"], false); !ok {
		return w, false
	}
	if w.ResetsAt, ok = quotaNullableTime(m["resetsAt"], false); !ok {
		return w, false
	}
	w.Forecast = quotaForecast{State: "insufficient_samples"}
	if w.ResetMode == "activity_triggered" {
		w.Forecast.State = "rolling_window"
	}
	return w, true
}

func quotaObject(raw []byte) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if len(raw) > maxQuotaBody || json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

func quotaString(raw json.RawMessage) (string, bool) {
	var value string
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func quotaStringEquals(raw json.RawMessage, want string) bool {
	got, ok := quotaString(raw)
	return ok && got == want
}

func quotaEnum(raw json.RawMessage, values ...string) (string, bool) {
	got, ok := quotaString(raw)
	if ok {
		for _, value := range values {
			if got == value {
				return got, true
			}
		}
	}
	return "", false
}

func quotaNumberEquals(raw json.RawMessage, want float64) bool {
	var value float64
	return json.Unmarshal(raw, &value) == nil && value == want
}

func quotaBool(raw json.RawMessage) (bool, bool) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

func quotaTimeField(raw json.RawMessage) bool {
	_, ok := quotaNullableTime(raw, false)
	return ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func quotaNullableTime(raw json.RawMessage, allowMissing bool) (*string, bool) {
	raw = bytes.TrimSpace(raw)
	if (allowMissing && len(raw) == 0) || bytes.Equal(raw, []byte("null")) {
		return nil, true
	}
	value, ok := quotaString(raw)
	if !ok {
		return nil, false
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return nil, false
	}
	return &value, true
}
