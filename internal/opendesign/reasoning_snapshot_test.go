package opendesign

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// config() must hand back a deep copy of the reasoning policy. Otherwise a
// caller (e.g. cfg.validate() normalizing rules) would rewrite the slice backing
// the published configuration while another goroutine reads it.
func TestReasoningConfigDeepCopyIsolation(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "high"}}},
	}); status != http.StatusOK {
		t.Fatalf("save reasoning: %d %s", status, raw)
	}

	first := s.config()
	first.Models[0].Reasoning.Rules[0].From = "tampered"
	first.Models[0].Reasoning.Enabled = false

	second := s.config()
	mapping := second.Models[0].Reasoning
	if mapping == nil || !mapping.Enabled || mapping.Rules[0].From != "medium" || mapping.Rules[0].To != "high" {
		t.Fatalf("config() shared the published reasoning policy: %+v", mapping)
	}
	if got := s.reasoningMappingFor("od-large"); got == nil || got.Rules[0].From != "medium" {
		t.Fatalf("reasoningMappingFor observed a mutated policy: %+v", got)
	}
}

// The revision token is content-addressed: stable for an unchanged policy and
// different once the policy or the upstream identity changes. The model list and
// the save response both expose it so a console tab can echo it back.
func TestReasoningRevisionStableAndPolicySensitive(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)

	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/models", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list models: %d %s", status, raw)
	}
	view := body["models"].([]any)[0].(map[string]any)
	rev, _ := view["reasoning_revision"].(string)
	if rev == "" {
		t.Fatalf("model view has no reasoning_revision: %s", raw)
	}
	// Stable across reads.
	_, body, _ = managementCall(t, s, http.MethodGet, apiBase+"/models", nil, nil)
	if again, _ := body["models"].([]any)[0].(map[string]any)["reasoning_revision"].(string); again != rev {
		t.Fatalf("reasoning_revision changed without a policy change: %q then %q", rev, again)
	}

	status, saved, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "high"}}},
	})
	if status != http.StatusOK {
		t.Fatalf("save reasoning: %d %s", status, raw)
	}
	newRev, _ := saved["reasoning_revision"].(string)
	if newRev == "" || newRev == rev {
		t.Fatalf("save did not advance the reasoning revision: old=%q new=%q", rev, newRev)
	}
}

// A stale tab that echoes an old expected_revision is rejected with 409, while a
// caller that omits the field keeps the previous last-writer-wins behavior.
func TestReasoningMappingStaleRevisionRejected(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)

	_, body, _ := managementCall(t, s, http.MethodGet, apiBase+"/models", nil, nil)
	rev, _ := body["models"].([]any)[0].(map[string]any)["reasoning_revision"].(string)

	// First save with the current revision succeeds.
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"expected_revision": rev,
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "high"}}},
	}); status != http.StatusOK {
		t.Fatalf("first save with expected_revision: %d %s", status, raw)
	}

	// A second save still carrying the old revision must be rejected.
	status, conflict, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"expected_revision": rev,
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "low"}}},
	})
	if status != http.StatusConflict {
		t.Fatalf("stale save status = %d, want 409: %s", status, raw)
	}
	if conflict["error"] == nil {
		t.Fatalf("stale save did not return an error envelope: %s", raw)
	}
	// The rejected save must not have changed the stored policy.
	if got := s.reasoningMappingFor("od-large"); got == nil || got.Rules[0].To != "high" {
		t.Fatalf("stale save mutated the policy: %+v", got)
	}

	// Omitting expected_revision stays compatible (legacy callers).
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "low"}}},
	}); status != http.StatusOK {
		t.Fatalf("legacy save without expected_revision: %d %s", status, raw)
	}
}

// A whitespace-only edit to an alias/upstream ID must not make importModels lose
// the stored reasoning policy. The keep-existing lookup has to trim before it
// compares.
func TestImportModelsWhitespacePreservesReasoningPolicy(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "high"}}},
	}); status != http.StatusOK {
		t.Fatalf("save reasoning: %d %s", status, raw)
	}
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "  od-large  ", "upstream_id": "  opendesign-large  "}},
	}); status != http.StatusOK {
		t.Fatalf("whitespace re-import: %d %s", status, raw)
	}
	mapping := s.reasoningMappingFor("od-large")
	if mapping == nil || !mapping.Enabled || len(mapping.Rules) != 1 || mapping.Rules[0].To != "high" {
		t.Fatalf("whitespace-only import dropped the policy: %+v", mapping)
	}
}

// The streaming executor must apply the same one-snapshot reasoning policy as the
// buffered path. Because the stream call returns as soon as the upstream stream
// is open, the policy is applied before that point and is pinned with the alias.
func TestExecutorStreamPinsReasoningSnapshot(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "high"}}},
	}); status != http.StatusOK {
		t.Fatalf("save reasoning: %d %s", status, raw)
	}
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{[]byte("data: {\"model\":\"opendesign-large\",\"choices\":[]}\n\n"), []byte("data: [DONE]\n\n")},
	})
	if _, err := executeStream(t, s, executorPayload{
		AuthID: id, Model: "od-large", StreamID: "out-reasoning",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`),
	}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	// The streaming path does not emit into host.http.do, so inspect the
	// host.http.do_stream body, which is the outbound request that was sent.
	sent := host.lastPayload("host.http.do_stream")
	if sent == nil {
		t.Fatal("stream never reached host.http.do_stream")
	}
	body, _ := sent["body"].([]byte)
	if !strings.Contains(string(body), `"reasoning_effort":"high"`) {
		t.Fatalf("streaming path did not apply the mapped strength: %s", body)
	}
	s.waitIdle()
}

// reasoningEffortOf must report the bytes the body actually carries. A passthrough
// uppercase/padded/unknown value must not be reported as a normalized strength.
func TestReasoningEffortOfDoesNotNormalizePassthrough(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"uppercase": {"reasoning_effort": "HIGH"},
		"padded":    {"reasoning_effort": " medium "},
		"unknown":   {"reasoning_effort": "extreme"},
		"absent":    {},
	} {
		got := reasoningEffortOf(body)
		switch name {
		case "uppercase":
			if got != "HIGH" {
				t.Errorf("uppercase passthrough = %q, want HIGH", got)
			}
		case "padded":
			if got != " medium " {
				t.Errorf("padded passthrough = %q, want verbatim", got)
			}
		case "unknown":
			if got != "extreme" {
				t.Errorf("unknown passthrough = %q, want extreme", got)
			}
		case "absent":
			if got != "" {
				t.Errorf("absent strength = %q, want empty", got)
			}
		}
	}

	// End to end: an unmapped uppercase value passes through untouched upstream,
	// and the request log does not falsely claim it was normalized to "high".
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)})
	if _, err := execute(t, s, executorPayload{
		AuthID: id, Model: "od-large",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"HIGH"}`),
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	sent := host.lastPayload("host.http.do")
	if body, _ := sent["body"].([]byte); !strings.Contains(string(body), `"reasoning_effort":"HIGH"`) {
		t.Fatalf("passthrough value was rewritten upstream: %s", body)
	}
	logs, total, _ := s.requestLogs.query(requestLogFilter{limit: 10})
	if total != 1 || len(logs) != 1 {
		t.Fatalf("logs = %#v total=%d", logs, total)
	}
	if logs[0].ReasoningEffort == "high" {
		t.Fatalf("log falsely normalized an unmapped passthrough to %q", logs[0].ReasoningEffort)
	}
}

// The model test must echo the executor's own decision, not a prediction. The
// reported effort has to match the strength in the outbound body byte for byte.
func TestModelTestEchoesExecutorReasoningDecision(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "high"}}},
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
		t.Fatalf("model test reported effort %#v, want high", body["reasoning_effort"])
	}
	mapping, ok := body["reasoning_mapping"].(map[string]any)
	if !ok || mapping["from"] != "medium" || mapping["to"] != "high" {
		t.Fatalf("model test mapping = %#v", body["reasoning_mapping"])
	}
	sent := host.lastPayload("host.http.do")
	upstreamBody, _ := sent["body"].([]byte)
	if !strings.Contains(string(upstreamBody), `"reasoning_effort":"high"`) {
		t.Fatalf("echoed conversion disagrees with the request: %s", upstreamBody)
	}
}

// A model test that never reached the upstream must not claim it sent a
// strength, even though the executor had already selected one.
func TestModelTestDoesNotClaimReasoningWhenUpstreamNotAttempted(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "high"}}},
	}); status != http.StatusOK {
		t.Fatalf("save reasoning: %d %s", status, raw)
	}

	// Park operation_open, then cancel the operation the way quiesce does. The
	// upstream HTTP call never starts, so nothing was sent.
	late := &lateOpenHost{fakeHost: host, opened: make(chan struct{}), release: make(chan struct{})}
	s.SetHost(late.call)
	request := mustJSON(t, ManagementRequest{
		Method: http.MethodPost,
		Path:   apiBase + "/model-test",
		Body:   mustJSON(t, map[string]any{"credential_id": id, "model_id": "od-large", "reasoning_effort": "medium"}),
	})
	resultCh := make(chan ManagementResponse, 1)
	go func() {
		result, err := s.Handle("management.handle", request)
		if err != nil {
			resultCh <- ManagementResponse{StatusCode: http.StatusInternalServerError, Body: []byte(err.Error())}
			return
		}
		resultCh <- result.(ManagementResponse)
	}()
	select {
	case <-late.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("operation_open was never entered")
	}
	s.cancelOperations()
	close(late.release)
	var response ManagementResponse
	select {
	case response = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("model test did not return after cancellation")
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatalf("decode model-test body: %v (%s)", err, response.Body)
	}
	if body["upstream_attempted"] != false {
		t.Fatalf("cancelled operation_open claimed an attempt: %s", response.Body)
	}
	if _, present := body["reasoning_effort"]; present {
		t.Fatalf("model test claimed a sent strength without attempting upstream: %s", response.Body)
	}
	if _, present := body["reasoning_mapping"]; present {
		t.Fatalf("model test reported a conversion without attempting upstream: %s", response.Body)
	}
}

// Concurrent saves and inference must stay race-free, and every request must be
// internally consistent: the upstream ID and the reasoning rule both come from a
// single published snapshot, never a mix of two saves.
func TestConcurrentReasoningSaveAndExecute(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	seedPolicy := func(to string) {
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
			"model": "od-large", "upstream_id": "opendesign-large",
			"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": to}}},
		})
		if status != http.StatusOK {
			t.Fatalf("seed policy %s: %d %s", to, status, raw)
		}
	}
	seedPolicy("high")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		targets := []string{"low", "high", "low", "high"}
		for _, to := range targets {
			select {
			case <-stop:
				return
			default:
			}
			seedPolicy(to)
		}
	}()

	host.doResponses = make([]fakeHTTPResponse, 0, 32)
	for i := 0; i < 32; i++ {
		host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)})
	}
	var inner sync.WaitGroup
	for i := 0; i < 8; i++ {
		inner.Add(1)
		go func() {
			defer inner.Done()
			_, _ = execute(t, s, executorPayload{
				AuthID: id, Model: "od-large",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`),
			})
		}()
	}
	inner.Wait()
	close(stop)
	wg.Wait()

	// Every outbound body must carry exactly one of the two configured targets,
	// never a mixture, which would mean the alias and rule came from different
	// snapshots.
	for _, call := range host.calls {
		if call.Method != "host.http.do" {
			continue
		}
		payload, _ := call.Payload.(map[string]any)
		body, _ := payload["body"].([]byte)
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("decode outbound: %v (%s)", err, body)
		}
		if decoded["model"] != "opendesign-large" {
			t.Fatalf("outbound model = %#v", decoded["model"])
		}
		if effort := decoded["reasoning_effort"]; effort != "high" && effort != "low" {
			t.Fatalf("outbound effort = %#v, want high or low", effort)
		}
	}
}

// A model test that starts under one policy and finishes after a concurrent save
// must report the decision it actually sent, not the newer stored policy. The
// save is parked inside host.http.do, so the newer policy is already published
// when the test reads its decision back.
func TestModelTestConcurrentReasoningSaveEchoesSentDecision(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "high"}}},
	}); status != http.StatusOK {
		t.Fatalf("save reasoning: %d %s", status, raw)
	}

	// Park the model-test upstream call so the concurrent save lands while the
	// test is still in flight.
	host.blockCall()
	host.mu.Lock()
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusOK,
		Body:   []byte(`{"model":"opendesign-large","choices":[]}`),
	})
	host.mu.Unlock()

	request := mustJSON(t, ManagementRequest{
		Method: http.MethodPost,
		Path:   apiBase + "/model-test",
		Body:   mustJSON(t, map[string]any{"credential_id": id, "model_id": "od-large", "reasoning_effort": "medium"}),
	})
	resultCh := make(chan ManagementResponse, 1)
	go func() {
		result, err := s.Handle("management.handle", request)
		if err != nil {
			resultCh <- ManagementResponse{StatusCode: http.StatusInternalServerError, Body: []byte(err.Error())}
			return
		}
		resultCh <- result.(ManagementResponse)
	}()
	host.awaitBlocked(t)

	// The model test already decided "high". Replace the stored policy with "low"
	// before releasing the call.
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "low"}}},
	}); status != http.StatusOK {
		t.Fatalf("concurrent save: %d %s", status, raw)
	}

	// Release the parked call by cancelling the operation the way quiesce does;
	// the response then reports the observed failure with the decision intact.
	s.cancelOperations()
	var response ManagementResponse
	select {
	case response = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("model test did not finish after the concurrent save")
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatalf("decode model-test body: %v (%s)", err, response.Body)
	}
	if body["reasoning_effort"] != "high" {
		t.Fatalf("model test reported %#v after a concurrent save, want the sent value high: %s", body["reasoning_effort"], response.Body)
	}
	mapping, ok := body["reasoning_mapping"].(map[string]any)
	if !ok || mapping["from"] != "medium" || mapping["to"] != "high" {
		t.Fatalf("model test mapping = %#v, want medium->high", body["reasoning_mapping"])
	}
	// The outbound body proves the echoed value was really sent.
	sent := host.lastPayload("host.http.do")
	upstreamBody, _ := sent["body"].([]byte)
	if !strings.Contains(string(upstreamBody), `"reasoning_effort":"high"`) {
		t.Fatalf("model test claimed high but sent: %s", upstreamBody)
	}
}
