package opendesign

import (
	"net/http"
	"strings"
)

// The reasoning mapping rewrites the top-level reasoning_effort of a
// chat-completions body before it is sent upstream. It never adds or removes a
// thinking directive of its own, never touches a nested thinking/reasoning
// object, and never changes a token budget: a strength with no rule is passed
// through unchanged.
//
// reasoningSources is the closed set of values a client may send, in the order
// the console renders them. reasoningTargets is the closed set a rule may name;
// it has no ultra entry because the upstream side is what the vendor actually
// accepts.
var reasoningSources = []string{"unset", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

var reasoningTargets = []string{"unset", "none", "minimal", "low", "medium", "high", "xhigh", "max"}

// reasoningUnset is the config sentinel for "the client sent no explicit
// strength". It is not an accepted input value: a request that literally sends
// reasoning_effort="unset" keeps whatever the upstream makes of it.
const reasoningUnset = "unset"

// defaultReasoningRules is the DeepSeek table the console pre-fills. Vendor
// documentation maps its accepted efforts onto two practical levels, so the
// pre-filled table folds minimal/low and medium/high/xhigh.
func defaultReasoningRules() []ReasoningRule {
	return []ReasoningRule{
		{From: "unset", To: "unset"},
		{From: "none", To: "none"},
		{From: "minimal", To: "low"},
		{From: "low", To: "low"},
		{From: "medium", To: "high"},
		{From: "high", To: "high"},
		{From: "xhigh", To: "high"},
		{From: "max", To: "max"},
		{From: "ultra", To: "max"},
	}
}

// defaultReasoningMapping pre-fills the DeepSeek table for a DeepSeek upstream
// and an empty table otherwise. The mapping still starts disabled: an operator
// opts in per model.
func defaultReasoningMapping(upstreamID string) ReasoningMapping {
	if strings.Contains(strings.ToLower(upstreamID), "deepseek") {
		return ReasoningMapping{Rules: defaultReasoningRules()}
	}
	return ReasoningMapping{}
}

func isReasoningSource(value string) bool {
	for _, candidate := range reasoningSources {
		if candidate == value {
			return true
		}
	}
	return false
}

func isReasoningTarget(value string) bool {
	for _, candidate := range reasoningTargets {
		if candidate == value {
			return true
		}
	}
	return false
}

// validateReasoningMapping bounds one model's policy. Rules are addressable by
// source, so a duplicate source is rejected instead of silently winning.
func validateReasoningMapping(mapping *ReasoningMapping) error {
	if mapping == nil {
		return nil
	}
	if len(mapping.Rules) > len(reasoningSources) {
		return localFail(http.StatusBadRequest, "invalid_request", "reasoning mapping has more rules than there are strengths")
	}
	seen := make(map[string]bool, len(mapping.Rules))
	for i := range mapping.Rules {
		rule := &mapping.Rules[i]
		rule.From = strings.ToLower(strings.TrimSpace(rule.From))
		rule.To = strings.ToLower(strings.TrimSpace(rule.To))
		if !isReasoningSource(rule.From) {
			return localFail(http.StatusBadRequest, "invalid_request", "reasoning mapping source is not a known strength")
		}
		if !isReasoningTarget(rule.To) {
			return localFail(http.StatusBadRequest, "invalid_request", "reasoning mapping target is not a known strength")
		}
		if seen[rule.From] {
			return localFail(http.StatusBadRequest, "invalid_request", "reasoning mapping sources must be unique")
		}
		seen[rule.From] = true
	}
	return nil
}

// reasoningMappingFor returns the policy bound to a client alias or an exact
// upstream ID. The returned value is a copy, so a caller may not mutate config.
func (s *Service) reasoningMappingFor(model string) *ReasoningMapping {
	model = strings.TrimSpace(model)
	models := s.config().Models
	for i := range models {
		if models[i].ID == model {
			return cloneReasoningMapping(models[i].Reasoning)
		}
	}
	for i := range models {
		if models[i].UpstreamID == model {
			return cloneReasoningMapping(models[i].Reasoning)
		}
	}
	return nil
}

func cloneReasoningMapping(mapping *ReasoningMapping) *ReasoningMapping {
	if mapping == nil {
		return nil
	}
	copied := &ReasoningMapping{Enabled: mapping.Enabled}
	if mapping.Rules != nil {
		copied.Rules = append([]ReasoningRule(nil), mapping.Rules...)
	}
	return copied
}

// applyReasoningMapping rewrites the top-level reasoning_effort in place and
// reports the conversion that was applied.
//
// An unsupported value (a number, an object, an unknown strength) is preserved
// for the upstream to validate: the mapping only ever acts on a value it has a
// rule for. When the client sent no strength but a nested thinking/reasoning
// directive is present, the nested policy is left alone rather than being
// overridden by a "unset" rule.
func applyReasoningMapping(j map[string]any, mapping *ReasoningMapping) (from, to string, applied bool) {
	if j == nil || mapping == nil || !mapping.Enabled || len(mapping.Rules) == 0 {
		return "", "", false
	}
	from = reasoningUnset
	if raw, exists := j["reasoning_effort"]; exists && raw != nil {
		value, ok := raw.(string)
		if !ok {
			return "", "", false
		}
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			if value == reasoningUnset {
				return "", "", false
			}
			from = value
		}
	}
	if from == reasoningUnset && (j["reasoning"] != nil || j["thinking"] != nil) {
		return "", "", false
	}
	for _, rule := range mapping.Rules {
		if rule.From != from {
			continue
		}
		if rule.To == reasoningUnset {
			delete(j, "reasoning_effort")
		} else {
			j["reasoning_effort"] = rule.To
		}
		return from, rule.To, true
	}
	return "", "", false
}

// reasoningEffortOf reads the strength a body actually carries. An absent or
// blank value is reported as none so the log can distinguish "not sent" from a
// value the client named.
func reasoningEffortOf(j map[string]any) string {
	raw, exists := j["reasoning_effort"]
	if !exists || raw == nil {
		return ""
	}
	value, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(value))
}
