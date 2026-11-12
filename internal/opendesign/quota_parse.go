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

// creditsPerUSDValue validates an operator-recorded plan rate. The value is a
// positive decimal string; anything else (empty, zero, malformed, oversized)
// stays unknown so the console never divides by a guessed or malformed number.
// It is presentation-only: the plugin still blocks and recovers purely on the
// upstream's own windows.
func creditsPerUSDValue(value string) string {
	raw := strings.TrimSpace(value)
	if len(raw) > 0 && len(raw) <= 32 && quotaUnsignedDecimal.MatchString(raw) {
		if positive, ok := new(big.Rat).SetString(raw); ok && positive.Sign() > 0 {
			return raw
		}
	}
	return ""
}

// These validators follow the official vela-wallet.ts reader and
// vela-billing.ts parseBillingPreflight at 5a0c477. Unknown fields are ignored;
// only explicitly validated fields are copied into management responses.
func parseQuotaWallet(body []byte) (*quotaWalletData, bool) {
	data, reason := parseQuotaWalletReason(body)
	return data, reason == ""
}

// parseQuotaWalletReason also names the check that failed. The balance is the
// only field the official reader requires:
//
//	isValidUsdBalance(body.balanceUsd)                      -> reject otherwise
//	updatedAt: typeof body.updatedAt === 'string' ? ... : null
//
// so updatedAt is display metadata. A value that is not a usable timestamp is
// reported as unknown; it must not discard a balance the upstream did return.
//
// One deliberate difference from the official reader: a negative balance is
// accepted. The official rule (/^\d+(?:\.\d+)?$/) rejects it, which made the
// console report "malformed" for an account that was simply overdrawn (the
// funding check of the same account returned balanceUsd "-N.NNNN" in
// production). The wallet is display-only here, so showing the amount the
// upstream returned is more useful than hiding it; the funding parser, which
// feeds the quota pause, already accepted a sign and is unchanged.
func parseQuotaWalletReason(body []byte) (*quotaWalletData, string) {
	if len(body) > maxQuotaBody {
		return nil, "body_too_large"
	}
	m, ok := quotaObject(body)
	if !ok {
		return nil, "not_json_object"
	}
	raw, present := m["balanceUsd"]
	if !present {
		return nil, "balance_missing"
	}
	balance, ok := quotaString(raw)
	if !ok {
		return nil, "balance_not_string"
	}
	if len(balance) > 4096 || !quotaSignedDecimal.MatchString(balance) {
		return nil, "balance_format"
	}
	updatedAt, _ := quotaNullableTime(m["updatedAt"], true)
	return &quotaWalletData{BalanceUSD: balance, UpdatedAt: updatedAt}, ""
}

func parseQuotaFunding(body []byte, workspaceID string) (*quotaFundingData, bool) {
	return parseQuotaFundingForModel(body, workspaceID, nil)
}

func parseModelCoverageFunding(body []byte, workspaceID, modelID string) (*quotaFundingData, bool) {
	return parseQuotaFundingForModel(body, workspaceID, &modelID)
}

func parseQuotaFundingForModel(body []byte, workspaceID string, modelID *string) (*quotaFundingData, bool) {
	m, ok := quotaObject(body)
	if !ok || !quotaNumberEquals(m["schemaVersion"], 1) || !quotaStringEquals(m["workspaceId"], workspaceID) {
		return nil, false
	}
	if modelID == nil {
		if !bytes.Equal(bytes.TrimSpace(m["modelId"]), []byte("null")) {
			return nil, false
		}
	} else if !quotaStringEquals(m["modelId"], *modelID) {
		return nil, false
	}
	member, ok := quotaString(m["workspaceMemberId"])
	if !ok || strings.TrimSpace(member) == "" {
		return nil, false
	}
	generatedAt, ok := quotaTimestamp(m["generatedAt"])
	if !ok {
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
		FundingCheck: fundingCheck, CodingPlan: quotaCodingPlan{Tier: tier, Windows: windows},
		generatedAt: generatedAt, workspaceMemberID: member}, true
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
	_, ok := quotaTimestamp(raw)
	return ok
}

func quotaTimestamp(raw json.RawMessage) (time.Time, bool) {
	value, ok := quotaString(raw)
	if !ok {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
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
