package opendesign

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// quotaPauseMaxWindow is an absolute upper bound even for untrusted upstream
// values and malformed local settings. It is not a host cooldown setting.
const (
	quotaBlockMarker        = "[upstream:quota_block]"
	quotaPauseMinWindow     = time.Minute
	quotaPauseDefaultWindow = 30 * time.Minute
	quotaPauseDefaultMax    = 24 * time.Hour
	quotaPauseMaxWindow     = 7 * 24 * time.Hour
	quotaPauseMaxModels     = 32
)

// QuotaBlock mirrors the ClinePass separation between an operator's Disabled
// choice and a temporary plugin-owned quota block. The fingerprint identifies
// the inference key without storing that key in the block or exposing it in the
// management response. A nil ManualDisabled is a legacy/unknown state and must
// not be interpreted as permission to enable a credential.
type QuotaBlock struct {
	KeyHash        string    `json:"key_hash"`
	ManualDisabled *bool     `json:"manual_disabled,omitempty"`
	Source         string    `json:"source"`
	Reason         string    `json:"reason"`
	BlockedAt      time.Time `json:"blocked_at"`
	ObservedAt     time.Time `json:"observed_at"`
	RetryAt        time.Time `json:"retry_at"`
	// ModelID/MultipleModels remain readable for existing auth files and UIs.
	// ModelIDs is the complete bounded set of models that reported exhaustion;
	// ModelsAmbiguous means it cannot safely be treated as a complete set.
	ModelID         string   `json:"model_id,omitempty"`
	MultipleModels  bool     `json:"multiple_models,omitempty"`
	ModelIDs        []string `json:"model_ids,omitempty"`
	ModelsAmbiguous bool     `json:"models_ambiguous,omitempty"`
}

func quotaPauseKeyHash(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("open-design-quota-block-v1\x00" + apiKey))
	return hex.EncodeToString(sum[:])
}

// quotaBlockBelongsToKey validates the fields that are required before trusting
// a stored manual-state snapshot. Invalid/legacy data must not be used as a
// reason to turn a credential on.
func quotaBlockBelongsToKey(block *QuotaBlock, apiKey string) bool {
	return block != nil && apiKey != "" && block.KeyHash == quotaPauseKeyHash(apiKey) &&
		block.Source == "upstream" && block.Reason == "quota_exhausted" &&
		!block.BlockedAt.IsZero() && !block.RetryAt.IsZero()
}

// normalizeQuotaBlock bounds persisted timestamps as well as newly received
// hints. A missing manual snapshot is unknown and therefore filled as disabled;
// it must never be used as permission to enable an account.
func normalizeQuotaBlock(c Credential, now time.Time) (*QuotaBlock, bool) {
	if !quotaBlockBelongsToKey(c.QuotaBlock, c.APIKey) {
		return c.QuotaBlock, false
	}
	block := cloneQuotaBlock(c.QuotaBlock)
	changed := false
	if block.ManualDisabled == nil {
		manual := true
		block.ManualDisabled = &manual
		changed = true
	}
	// Older blocks recorded only the first model and a multiple-model flag.
	// A multi-model legacy block cannot reconstruct its complete set, so keep
	// it ambiguous instead of treating one model as sufficient proof.
	if len(block.ModelIDs) == 0 && block.MultipleModels && !block.ModelsAmbiguous {
		block.ModelsAmbiguous = true
		changed = true
	} else if len(block.ModelIDs) == 0 && !block.MultipleModels && validQuotaModelID(block.ModelID) {
		block.ModelIDs = []string{block.ModelID}
		changed = true
	}
	if block.BlockedAt.After(now) {
		block.BlockedAt = now.UTC()
		changed = true
	}
	if block.ObservedAt.IsZero() || block.ObservedAt.Before(block.BlockedAt) {
		block.ObservedAt = block.BlockedAt
		changed = true
	} else if block.ObservedAt.After(now) {
		block.ObservedAt = now.UTC()
		changed = true
	}
	if !block.RetryAt.After(block.BlockedAt) {
		block.RetryAt = block.BlockedAt.Add(quotaPauseDefaultWindow)
		changed = true
	}
	if latest := block.BlockedAt.Add(quotaPauseMaxWindow); block.RetryAt.After(latest) {
		block.RetryAt = latest
		changed = true
	}
	return block, changed
}

func (c Credential) quotaBlocked() bool {
	return quotaBlockBelongsToKey(c.QuotaBlock, c.APIKey)
}

func (c Credential) effectiveDisabled() bool {
	return c.Disabled || c.quotaBlocked()
}

func cloneQuotaBlock(block *QuotaBlock) *QuotaBlock {
	if block == nil {
		return nil
	}
	copy := *block
	copy.ModelIDs = append([]string(nil), block.ModelIDs...)
	if block.ManualDisabled != nil {
		manual := *block.ManualDisabled
		copy.ManualDisabled = &manual
	}
	return &copy
}

func validQuotaModelID(model string) bool {
	return model != "" && len(model) <= 512 && strings.TrimSpace(model) == model && strings.IndexFunc(model, unicode.IsControl) < 0
}

// addQuotaBlockModel only removes ambiguity when every observed model can be
// enumerated. A missing or oversized set must not become a single-model proof.
func addQuotaBlockModel(block *QuotaBlock, model string) {
	model = strings.TrimSpace(model)
	if !validQuotaModelID(model) {
		block.ModelsAmbiguous = true
		return
	}
	if len(block.ModelIDs) == 0 && block.MultipleModels {
		// Legacy metadata records that more than one model failed, but not all
		// their IDs; adding one now cannot reconstruct the old missing set.
		block.ModelsAmbiguous = true
	}
	if len(block.ModelIDs) == 0 && validQuotaModelID(block.ModelID) {
		block.ModelIDs = append(block.ModelIDs, block.ModelID)
	}
	for _, existing := range block.ModelIDs {
		if existing == model {
			return
		}
	}
	if len(block.ModelIDs) >= quotaPauseMaxModels {
		block.ModelsAmbiguous = true
		block.MultipleModels = true
		return
	}
	block.ModelIDs = append(block.ModelIDs, model)
	sort.Strings(block.ModelIDs)
	if block.ModelID == "" {
		block.ModelID = model
	}
	block.MultipleModels = block.MultipleModels || len(block.ModelIDs) > 1
}

// quotaBlockModels returns only a complete, internally consistent set. An old
// block with a multiple-model flag but no list can be resumed manually only.
func quotaBlockModels(block *QuotaBlock) ([]string, bool) {
	if block == nil || block.ModelsAmbiguous || len(block.ModelIDs) > quotaPauseMaxModels {
		return nil, false
	}
	models := append([]string(nil), block.ModelIDs...)
	if len(models) == 0 && !block.MultipleModels && validQuotaModelID(block.ModelID) {
		models = append(models, block.ModelID)
	}
	if len(models) == 0 || block.MultipleModels && len(models) < 2 {
		return nil, false
	}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if !validQuotaModelID(model) || seen[model] {
			return nil, false
		}
		seen[model] = true
	}
	if block.ModelID != "" && !seen[block.ModelID] {
		return nil, false
	}
	sort.Strings(models)
	return models, true
}

type pauseSettings struct {
	enabled       bool
	defaultWindow time.Duration
	maxWindow     time.Duration
}

// pauseSettings resolves the persisted policy. New and older settings without
// an explicit choice run the reviewed default: automatic quota pauses are on.
// An operator can still persist enabled=false; the CPA-native manual-disable
// attribution limitation remains documented. Zero MaxSeconds keeps the cap.
func (c Config) pauseSettings() pauseSettings {
	settings := pauseSettings{enabled: true, defaultWindow: quotaPauseDefaultWindow, maxWindow: quotaPauseDefaultMax}
	if c.Pause.Enabled != nil {
		settings.enabled = *c.Pause.Enabled
	}
	if c.Pause.MaxSeconds > 0 {
		// Compare before converting to Duration so a malformed settings file
		// cannot overflow the multiplication.
		if c.Pause.MaxSeconds >= int(quotaPauseMaxWindow/time.Second) {
			settings.maxWindow = quotaPauseMaxWindow
		} else {
			settings.maxWindow = time.Duration(c.Pause.MaxSeconds) * time.Second
		}
	}
	if settings.maxWindow > quotaPauseMaxWindow {
		settings.maxWindow = quotaPauseMaxWindow
	}
	if settings.maxWindow < quotaPauseMinWindow {
		settings.maxWindow = quotaPauseMinWindow
	}
	if settings.defaultWindow > settings.maxWindow {
		settings.defaultWindow = settings.maxWindow
	}
	return settings
}

// quotaPauseDeadline bounds a plugin pause independently of the CPA host's
// cooldown. A missing or expired hint uses a bounded fallback; long hints are
// clamped, rather than being mistaken for a missing hint and retried early.
func quotaPauseDeadline(now, resetAt time.Time, settings pauseSettings) time.Time {
	maxWindow := settings.maxWindow
	if maxWindow <= 0 {
		maxWindow = quotaPauseDefaultMax
	}
	if maxWindow > quotaPauseMaxWindow {
		maxWindow = quotaPauseMaxWindow
	}
	if maxWindow < quotaPauseMinWindow {
		maxWindow = quotaPauseMinWindow
	}
	fallback := settings.defaultWindow
	if fallback <= 0 {
		fallback = quotaPauseDefaultWindow
	}
	if fallback > maxWindow {
		fallback = maxWindow
	}
	deadline := now.Add(fallback)
	if resetAt.After(now) {
		deadline = resetAt
	}
	if earliest := now.Add(quotaPauseMinWindow); deadline.Before(earliest) {
		deadline = earliest
	}
	if latest := now.Add(maxWindow); deadline.After(latest) {
		deadline = latest
	}
	return deadline.UTC()
}

func isQuotaExhaustionCode(kind string) bool {
	return kind == "insufficient_quota" || kind == "insufficient_balance"
}

// quotaPauseSignal is intentionally narrower than normal error classification:
// a quota pause affects the whole account, so free text, a bare 402 and ordinary
// 429s must not pause it. A 2xx error body or SSE event is accepted only when
// the structured response explicitly declares the coding plan exhausted.
func quotaPauseSignal(observedStatus int, headers http.Header, body []byte, now time.Time) (time.Time, bool) {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				CodingPlanExhausted bool `json:"coding_plan_exhausted"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Error.Code != "insufficient_balance" {
		return time.Time{}, false
	}
	if observedStatus != http.StatusPaymentRequired &&
		!(observedStatus >= 200 && observedStatus < 300 && envelope.Error.Details.CodingPlanExhausted) {
		return time.Time{}, false
	}
	resetAt := quotaResetTimeFromBody(body, now)
	if resetAt.IsZero() {
		resetAt = quotaResetTimeFromHeaders(headers, now)
	}
	return resetAt, true
}

// quotaResetTimeFromBody reads only structured reset fields. Callers must first
// classify the error by a recognized machine code; this function must never be
// used to infer quota exhaustion from text or a bare HTTP status.
func quotaResetTimeFromBody(body []byte, now time.Time) time.Time {
	var envelope struct {
		Error struct {
			Details struct {
				ResetAt   string `json:"reset_at"`
				ResetsAt  string `json:"resets_at"`
				ResetAtC  string `json:"resetAt"`
				ResetsAtC string `json:"resetsAt"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return time.Time{}
	}
	fields := envelope.Error.Details
	for _, value := range []string{fields.ResetAt, fields.ResetsAt, fields.ResetAtC, fields.ResetsAtC} {
		if result, ok := parseQuotaResetTimestamp(value, now); ok {
			return result
		}
	}
	return time.Time{}
}

func parseQuotaResetTimestamp(value string, now time.Time) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		// Some reset fields contain the literal prefix "resets at". This
		// interprets only the timestamp within an already-classified error;
		// it never turns free-text payment messages into quota signals.
		const prefix = "resets at "
		if !strings.HasPrefix(strings.ToLower(value), prefix) {
			return time.Time{}, false
		}
		value = strings.TrimRight(strings.TrimSpace(value[len(prefix):]), ".;")
		parsed, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, false
		}
	}
	result := boundedQuotaReset(parsed, now)
	return result, !result.IsZero()
}

// quotaResetTimeFromHeaders accepts Retry-After in HTTP-date or delay-seconds
// form. Numeric comparison precedes the Duration conversion to avoid overflow.
func quotaResetTimeFromHeaders(headers http.Header, now time.Time) time.Time {
	value := strings.TrimSpace(headers.Get("Retry-After"))
	if value == "" {
		return time.Time{}
	}
	if strings.Trim(value, "0123456789") == "" {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			// An all-digit overflow is a very long delay, not a short fallback.
			return now.Add(quotaPauseMaxWindow).UTC()
		}
		if seconds == 0 {
			return time.Time{}
		}
		if limit := uint64(quotaPauseMaxWindow / time.Second); seconds > limit {
			seconds = limit
		}
		return now.Add(time.Duration(seconds) * time.Second).UTC()
	}
	if parsed, err := http.ParseTime(value); err == nil {
		return boundedQuotaReset(parsed, now)
	}
	return time.Time{}
}

func boundedQuotaReset(value, now time.Time) time.Time {
	if !value.After(now) {
		return time.Time{}
	}
	if limit := now.Add(quotaPauseMaxWindow); value.After(limit) {
		return limit.UTC()
	}
	return value.UTC()
}
