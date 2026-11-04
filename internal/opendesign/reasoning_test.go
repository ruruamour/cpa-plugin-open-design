package opendesign

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func reasoningMappingPayload(source, target string) []map[string]string {
	return []map[string]string{{"from": source, "to": target}}
}

// A DeepSeek upstream gets the vendor table pre-filled, anything else starts
// empty, and both start disabled until an operator opts in.
func TestReasoningDefaultsPreFillDeepSeekOnly(t *testing.T) {
	deepseek := defaultReasoningMapping("deepseek-v4.1-flash")
	if deepseek.Enabled {
		t.Fatal("pre-filled mapping must start disabled")
	}
	if len(deepseek.Rules) != len(reasoningSources) {
		t.Fatalf("deepseek rules = %d, want %d", len(deepseek.Rules), len(reasoningSources))
	}
	byFrom := map[string]string{}
	for _, rule := range deepseek.Rules {
		byFrom[rule.From] = rule.To
	}
	for from, want := range map[string]string{
		"unset": "unset", "none": "none", "minimal": "low", "low": "low",
		"medium": "high", "high": "high", "xhigh": "high", "max": "max", "ultra": "max",
	} {
		if byFrom[from] != want {
			t.Errorf("default %s = %q, want %q", from, byFrom[from], want)
		}
	}
	if other := defaultReasoningMapping("vela/gpt-6-luna"); other.Enabled || len(other.Rules) != 0 {
		t.Fatalf("non-deepseek default = %+v, want empty and disabled", other)
	}
}

func TestReasoningMappingValidation(t *testing.T) {
	ok := ReasoningMapping{Enabled: true, Rules: []ReasoningRule{{From: "medium", To: "high"}}}
	if err := validateReasoningMapping(&ok); err != nil {
		t.Fatalf("valid mapping rejected: %v", err)
	}
	if ok.Rules[0].From != "medium" || ok.Rules[0].To != "high" {
		t.Fatalf("normalization changed a valid rule: %+v", ok.Rules[0])
	}
	for name, mapping := range map[string]ReasoningMapping{
		"unknown source": {Rules: []ReasoningRule{{From: "extreme", To: "high"}}},
		"unknown target": {Rules: []ReasoningRule{{From: "medium", To: "ultra"}}},
		"duplicate":      {Rules: []ReasoningRule{{From: "medium", To: "high"}, {From: "medium", To: "low"}}},
		"too many":       {Rules: append(defaultReasoningRules(), ReasoningRule{From: "low", To: "low"})},
	} {
		if err := validateReasoningMapping(&mapping); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// Case and padding are normalized before validation, not rejected.
	trimmed := ReasoningMapping{Rules: []ReasoningRule{{From: " MEDIUM ", To: " High "}}}
	if err := validateReasoningMapping(&trimmed); err != nil {
		t.Fatalf("normalizable rule rejected: %v", err)
	}
	if trimmed.Rules[0].From != "medium" || trimmed.Rules[0].To != "high" {
		t.Fatalf("rule was not normalized: %+v", trimmed.Rules[0])
	}
}

func TestApplyReasoningMapping(t *testing.T) {
	table := ReasoningMapping{Enabled: true, Rules: []ReasoningRule{
		{From: "unset", To: "max"},
		{From: "medium", To: "high"},
		{From: "low", To: "unset"},
	}}
	cases := []struct {
		name        string
		body        map[string]any
		wantEffort  any
		wantPresent bool
		wantFrom    string
		wantApplied bool
	}{
		{
			name: "rewrites a matched strength", body: map[string]any{"reasoning_effort": "medium"},
			wantEffort: "high", wantPresent: true, wantFrom: "medium", wantApplied: true,
		},
		{
			name: "fills an absent strength", body: map[string]any{},
			wantEffort: "max", wantPresent: true, wantFrom: "unset", wantApplied: true,
		},
		{
			name: "blank counts as absent", body: map[string]any{"reasoning_effort": "   "},
			wantEffort: "max", wantPresent: true, wantFrom: "unset", wantApplied: true,
		},
		{
			name: "target unset removes the field", body: map[string]any{"reasoning_effort": "low"},
			wantPresent: false, wantFrom: "low", wantApplied: true,
		},
		{
			name: "unmatched strength passes through", body: map[string]any{"reasoning_effort": "none"},
			wantEffort: "none", wantPresent: true,
		},
		{
			name: "literal unset is not the sentinel", body: map[string]any{"reasoning_effort": "unset"},
			wantEffort: "unset", wantPresent: true,
		},
		{
			name: "non-string value is preserved for upstream validation", body: map[string]any{"reasoning_effort": 3},
			wantEffort: 3, wantPresent: true,
		},
		{
			name: "nested directive suppresses the unset rule", body: map[string]any{"thinking": map[string]any{"type": "enabled"}},
			wantPresent: false,
		},
		{
			name: "nested reasoning suppresses the unset rule", body: map[string]any{"reasoning": map[string]any{"effort": "low"}},
			wantPresent: false,
		},
		{
			// An explicit strength still maps even when a nested directive exists:
			// the nested object is never touched and is not a substitute value.
			name:       "explicit strength maps beside a nested directive",
			body:       map[string]any{"reasoning_effort": "medium", "thinking": map[string]any{"type": "enabled"}},
			wantEffort: "high", wantPresent: true, wantFrom: "medium", wantApplied: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			from, _, applied := applyReasoningMapping(testCase.body, &table)
			value, present := testCase.body["reasoning_effort"]
			if present != testCase.wantPresent {
				t.Fatalf("presence = %v, want %v (value %#v)", present, testCase.wantPresent, value)
			}
			if present && value != testCase.wantEffort {
				t.Fatalf("value = %#v, want %#v", value, testCase.wantEffort)
			}
			if applied != testCase.wantApplied || from != testCase.wantFrom {
				t.Fatalf("applied = %v from = %q, want %v %q", applied, from, testCase.wantApplied, testCase.wantFrom)
			}
		})
	}
}

// A disabled mapping and an empty rule table both leave the body alone, so
// turning the switch off is a guaranteed passthrough.
func TestApplyReasoningMappingDisabledIsPassthrough(t *testing.T) {
	for name, mapping := range map[string]ReasoningMapping{
		"disabled": {Enabled: false, Rules: []ReasoningRule{{From: "unset", To: "max"}}},
		"empty":    {Enabled: true},
	} {
		body := map[string]any{}
		if _, _, applied := applyReasoningMapping(body, &mapping); applied {
			t.Fatalf("%s mapping applied", name)
		}
		if _, present := body["reasoning_effort"]; present {
			t.Fatalf("%s mapping wrote a value", name)
		}
	}
	if _, _, applied := applyReasoningMapping(map[string]any{}, nil); applied {
		t.Fatal("nil mapping applied")
	}
}

// The mapping runs exactly once per request. A table where applying it twice
// would land on a different value proves the executor does not re-enter.
func TestExecuteAppliesReasoningMappingExactlyOnce(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{
			{"from": "medium", "to": "high"},
			{"from": "high", "to": "low"},
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("save reasoning: %d %s", status, raw)
	}
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)})
	if _, err := execute(t, s, executorPayload{
		AuthID: id, Model: "od-large",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`),
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	sent := host.lastPayload("host.http.do")
	body, _ := sent["body"].([]byte)
	text := string(body)
	if !strings.Contains(text, `"reasoning_effort":"high"`) {
		t.Fatalf("mapping did not run once: %s", text)
	}
	if strings.Contains(text, `"reasoning_effort":"low"`) {
		t.Fatalf("mapping ran twice: %s", text)
	}
}

// A request that carries no strength and no rule for "unset" is sent unchanged,
// so an unconfigured model keeps its previous behavior.
func TestExecuteWithoutMappingLeavesReasoningUntouched(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)})
	if _, err := execute(t, s, executorPayload{
		AuthID: id, Model: "od-large",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"max"}`),
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	sent := host.lastPayload("host.http.do")
	body, _ := sent["body"].([]byte)
	if !strings.Contains(string(body), `"reasoning_effort":"max"`) {
		t.Fatalf("unmapped request lost its strength: %s", body)
	}
}

// Rewriting the model list from the console must not silently drop a policy the
// operator saved: the mapping survives an unrelated edit but not an upstream
// change, which would apply one vendor's table to another model.
func TestReasoningMappingSurvivesModelListSave(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": reasoningMappingPayload("medium", "high")},
	}); status != http.StatusOK {
		t.Fatalf("save reasoning: %d %s", status, raw)
	}
	// An unrelated re-save of the list keeps the policy.
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{"models": []map[string]string{
		{"id": "od-large", "upstream_id": "opendesign-large"},
		{"id": "od-small", "upstream_id": "opendesign-small"},
	}}); status != http.StatusOK {
		t.Fatalf("resave models: %d %s", status, raw)
	}
	kept := s.reasoningMappingFor("od-large")
	if kept == nil || !kept.Enabled || len(kept.Rules) != 1 {
		t.Fatalf("policy lost on list save: %+v", kept)
	}
	// The same alias pointed at a different upstream drops it.
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{"models": []map[string]string{
		{"id": "od-large", "upstream_id": "opendesign-other"},
	}}); status != http.StatusOK {
		t.Fatalf("repoint models: %d %s", status, raw)
	}
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"choices":[]}`)})
	if _, err := execute(t, s, executorPayload{
		AuthID: id, Model: "od-large",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`),
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	sent := host.lastPayload("host.http.do")
	body, _ := sent["body"].([]byte)
	if !strings.Contains(string(body), `"reasoning_effort":"medium"`) {
		t.Fatalf("policy followed the alias onto a new upstream: %s", body)
	}
}

func TestSaveReasoningMappingRouteFailures(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	for name, input := range map[string]map[string]any{
		"missing mapping": {"model": "od-large", "upstream_id": "opendesign-large"},
		"unknown model":   {"model": "nope", "upstream_id": "opendesign-large", "reasoning_mapping": map[string]any{"enabled": true}},
		"stale upstream":  {"model": "od-large", "upstream_id": "opendesign-other", "reasoning_mapping": map[string]any{"enabled": true}},
		"bad source":      {"model": "od-large", "upstream_id": "opendesign-large", "reasoning_mapping": map[string]any{"enabled": true, "rules": reasoningMappingPayload("extreme", "high")}},
		"bad target":      {"model": "od-large", "upstream_id": "opendesign-large", "reasoning_mapping": map[string]any{"enabled": true, "rules": reasoningMappingPayload("medium", "ultra")}},
	} {
		want := http.StatusBadRequest
		switch name {
		case "unknown model":
			want = http.StatusNotFound
		case "stale upstream":
			want = http.StatusConflict
		}
		if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, input); status != want {
			t.Errorf("%s: status = %d want %d (%s)", name, status, want, raw)
		}
	}
	// A rejected save leaves the stored policy untouched.
	if mapping := s.reasoningMappingFor("od-large"); mapping != nil && mapping.Enabled {
		t.Fatalf("rejected save published a policy: %+v", mapping)
	}
}

// The policy is plugin settings, so it has to survive a reload the same way a
// model list does. A hand-edited file with an unknown strength is rejected at
// load instead of being applied.
func TestReasoningMappingPersistsAndReloads(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": reasoningMappingPayload("unset", "max")},
	}); status != http.StatusOK {
		t.Fatalf("save reasoning: %d %s", status, raw)
	}
	// Re-registering on the same data_dir reloads settings.json from disk.
	registrar(t, s, s.config().DataDir)
	mapping := s.reasoningMappingFor("od-large")
	if mapping == nil || !mapping.Enabled || len(mapping.Rules) != 1 || mapping.Rules[0].To != "max" {
		t.Fatalf("policy did not survive reload: %+v", mapping)
	}
}

// The request log records the strength that was actually sent and the rule that
// produced it, so an operator can tell a mapped request from a passthrough one.
func TestRequestLogRecordsReasoningConversion(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": reasoningMappingPayload("medium", "high")},
	}); status != http.StatusOK {
		t.Fatalf("save reasoning: %d %s", status, raw)
	}
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)})
	if _, err := execute(t, s, executorPayload{
		AuthID: id, Model: "od-large",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`),
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-logs", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("logs status = %d body = %s", status, raw)
	}
	logs, _ := body["logs"].([]any)
	if len(logs) != 1 {
		t.Fatalf("logs = %#v", body["logs"])
	}
	entry := logs[0].(map[string]any)
	if entry["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want high", entry["reasoning_effort"])
	}
	mapping, ok := entry["reasoning_mapping"].(map[string]any)
	if !ok || mapping["from"] != "medium" || mapping["to"] != "high" {
		t.Fatalf("reasoning_mapping = %#v", entry["reasoning_mapping"])
	}
	// A request with no rule applied records the strength but no conversion.
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)})
	if _, err := execute(t, s, executorPayload{
		AuthID: id, Model: "od-large",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`),
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	_, body, _ = managementCall(t, s, http.MethodGet, apiBase+"/request-logs", nil, nil)
	logs, _ = body["logs"].([]any)
	// The route returns newest first, so the passthrough row is at the head.
	passthrough := logs[0].(map[string]any)
	if passthrough["reasoning_effort"] != "none" || passthrough["reasoning_mapping"] != nil {
		t.Fatalf("passthrough entry = %#v", passthrough)
	}
}

// A journal or snapshot edited by hand must not be able to smuggle arbitrary
// text into the console through the reasoning column.
func TestRequestLogSanitizesReasoningFields(t *testing.T) {
	injected := "‹img src=x onerror=alert(1)›"
	entry, ok := sanitizeRequestLogEntry(RequestLogEntry{
		ID: "id", Status: "success", StartedAt: time.Now(),
		ReasoningEffort:  injected,
		ReasoningMapping: &RequestLogReasoningMapping{From: injected, To: injected},
	})
	if !ok {
		t.Fatal("entry was dropped")
	}
	if entry.ReasoningEffort != "" || entry.ReasoningMapping != nil {
		t.Fatalf("sanitizer kept an unknown strength: %#v", entry)
	}
	entry, ok = sanitizeRequestLogEntry(RequestLogEntry{
		ID: "id", Status: "success", StartedAt: time.Now(),
		ReasoningEffort:  "high",
		ReasoningMapping: &RequestLogReasoningMapping{From: "medium", To: "high"},
	})
	if !ok || entry.ReasoningEffort != "high" || entry.ReasoningMapping == nil {
		t.Fatalf("sanitizer dropped a valid strength: %#v", entry)
	}
}

// The model test reports the conversion the executor will apply. Both call the
// same pure function on the same input, so the reported pair cannot drift.
func TestModelTestReportsAppliedReasoning(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": reasoningMappingPayload("medium", "high")},
	}); status != http.StatusOK {
		t.Fatalf("save reasoning: %d %s", status, raw)
	}
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusOK,
		Body:   []byte(`{"model":"opendesign-large","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
	})
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
		"credential_id": id, "model_id": "od-large", "reasoning_effort": "medium",
	})
	if status != http.StatusOK {
		t.Fatalf("model test: %d %s", status, raw)
	}
	if body["reasoning_effort"] != "high" {
		t.Fatalf("test reported effort %#v, want high", body["reasoning_effort"])
	}
	mapping, ok := body["reasoning_mapping"].(map[string]any)
	if !ok || mapping["from"] != "medium" || mapping["to"] != "high" {
		t.Fatalf("test mapping = %#v", body["reasoning_mapping"])
	}
	sent := host.lastPayload("host.http.do")
	upstreamBody, _ := sent["body"].([]byte)
	if !strings.Contains(string(upstreamBody), `"reasoning_effort":"high"`) {
		t.Fatalf("reported conversion disagrees with the request: %s", upstreamBody)
	}
	// An unknown strength is rejected before any upstream call.
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
		"credential_id": id, "model_id": "od-large", "reasoning_effort": "extreme",
	}); status != http.StatusBadRequest {
		t.Fatalf("unknown test strength status = %d", status)
	}
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
		"credential_id": id, "model_id": "od-large", "reasoning_effort": "unset",
	}); status != http.StatusBadRequest {
		t.Fatalf("config sentinel accepted as a test strength: %d", status)
	}
}
