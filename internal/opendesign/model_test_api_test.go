package opendesign

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestModelTestUsesOneBufferedRequestAndReturnsMetadataOnly(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-model-test-secret")
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "open-design-test", "upstream_id": "od/test"}},
	})
	if status != http.StatusOK {
		t.Fatalf("save models status = %d body = %s", status, raw)
	}
	host.mu.Lock()
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"X-Request-Id": []string{"req-model-test"}},
		Body:    []byte(`{"id":"completion-1","model":"od/test","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5},"secret":"must-not-return"}`),
	})
	host.mu.Unlock()

	status, body, raw = managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
		"credential_id": id,
		"model_id":      "open-design-test",
		"prompt":        "Reply with OK",
	})
	if status != http.StatusOK {
		t.Fatalf("model-test status = %d body = %s", status, raw)
	}
	if body["status"] != "success" || body["real_inference"] != true || body["streaming"] != false {
		t.Fatalf("unexpected model-test response: %#v", body)
	}
	if body["upstream_model"] != "od/test" || body["request_kind"] != "chat.completions" {
		t.Fatalf("model-test routing metadata = %#v", body)
	}
	if got, ok := body["http_status"].(float64); !ok || got != 200 {
		t.Fatalf("http_status = %#v", body["http_status"])
	}
	usage, ok := body["usage"].(map[string]any)
	if !ok || usage["prompt_tokens"] != float64(3) || usage["completion_tokens"] != float64(2) || usage["total_tokens"] != float64(5) {
		t.Fatalf("usage = %#v", body["usage"])
	}
	for _, forbidden := range []string{"secret", "completion-1", "sk-model-test-secret", "choices", "headers", "workspace"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("model-test response leaked %q: %s", forbidden, raw)
		}
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	var doCalls []hostCallRecord
	for _, call := range host.calls {
		if call.Method == "host.http.do" {
			doCalls = append(doCalls, call)
		}
	}
	if len(doCalls) != 1 {
		t.Fatalf("host.http.do calls = %d, want 1", len(doCalls))
	}
	payload, ok := doCalls[0].Payload.(map[string]any)
	if !ok {
		t.Fatalf("host.http.do payload = %T", doCalls[0].Payload)
	}
	var outbound map[string]any
	if err := json.Unmarshal(payload["body"].([]byte), &outbound); err != nil {
		t.Fatalf("decode outbound body: %v", err)
	}
	if outbound["model"] != "od/test" || outbound["max_tokens"] != float64(16) {
		t.Fatalf("outbound body = %#v", outbound)
	}
	if _, exists := outbound["prompt"]; exists {
		t.Fatal("model-test sent a non-chat prompt field")
	}
}

func TestModelTestReportsActualUpstreamStatusWithoutEchoingErrorBody(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-model-test-status-secret")
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "open-design-test", "upstream_id": "od/test"}},
	})
	if status != http.StatusOK {
		t.Fatalf("save models status = %d body = %s", status, raw)
	}
	host.mu.Lock()
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusTooManyRequests,
		Body:   []byte(`{"error":{"message":"rate_limit_exceeded: completion secret must not escape","code":"rate_limit_exceeded"}}`),
	})
	host.mu.Unlock()

	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
		"credential_id": id,
		"model_id":      "open-design-test",
	})
	if status != http.StatusOK {
		t.Fatalf("model-test status = %d body = %s", status, raw)
	}
	if body["status"] != "error" || body["real_inference"] != true || body["upstream_attempted"] != true {
		t.Fatalf("unexpected error response: %#v", body)
	}
	if got, ok := body["http_status"].(float64); !ok || got != http.StatusTooManyRequests {
		t.Fatalf("http_status = %#v", body["http_status"])
	}
	if !strings.Contains(raw, "rate_limit_exceeded") {
		t.Fatalf("safe upstream code missing: %s", raw)
	}
	for _, forbidden := range []string{"completion secret must not escape", "sk-model-test-status-secret"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("model-test error leaked %q: %s", forbidden, raw)
		}
	}
}

func TestModelTestTreatsNonJSONSuccessAsAttemptedFailure(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-model-test-protocol-secret")
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "open-design-test", "upstream_id": "od/test"}},
	})
	if status != http.StatusOK {
		t.Fatalf("save models status = %d body = %s", status, raw)
	}
	host.mu.Lock()
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusOK,
		Body:   []byte("<completion body must not be returned>"),
	})
	host.mu.Unlock()

	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
		"credential_id": id,
		"model_id":      "open-design-test",
	})
	if status != http.StatusOK {
		t.Fatalf("model-test status = %d body = %s", status, raw)
	}
	if body["status"] != "error" || body["real_inference"] != true || body["upstream_attempted"] != true {
		t.Fatalf("unexpected protocol response: %#v", body)
	}
	if got, ok := body["http_status"].(float64); !ok || got != http.StatusOK {
		t.Fatalf("http_status = %#v", body["http_status"])
	}
	if strings.Contains(raw, "completion body must not be returned") || strings.Contains(raw, "sk-model-test-protocol-secret") {
		t.Fatalf("protocol error leaked response data: %s", raw)
	}
}

func TestModelTestLateResultIsDiscardedAfterMappingChange(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-model-test-late")
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "open-design-test", "upstream_id": "od/old"}},
	}); status != http.StatusOK {
		t.Fatalf("seed mapping status = %d body = %s", status, raw)
	}
	host.mu.Lock()
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusOK,
		Body:   []byte(`{"id":"late","choices":[],"usage":{"total_tokens":1}}`),
	})
	host.mu.Unlock()
	host.blockCall()
	request := mustJSON(t, ManagementRequest{
		Method: http.MethodPost,
		Path:   apiBase + "/model-test",
		Body:   mustJSON(t, map[string]any{"credential_id": id, "model_id": "open-design-test"}),
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

	outboundCall := host.lastPayload("host.http.do")
	if outboundCall == nil {
		t.Fatal("model-test did not reach host.http.do")
	}
	var outbound map[string]any
	if err := json.Unmarshal(outboundCall["body"].([]byte), &outbound); err != nil {
		t.Fatalf("decode blocked outbound body: %v", err)
	}
	if outbound["model"] != "od/old" {
		t.Fatalf("model-test outbound model changed after snapshot: %#v", outbound)
	}

	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "open-design-test", "upstream_id": "od/new"}},
	}); status != http.StatusOK {
		t.Fatalf("mapping replacement status = %d body = %s", status, raw)
	}
	s.cancelOperations()
	select {
	case <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("late model-test request did not finish")
	}
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/runtime", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("runtime status = %d body = %s", status, raw)
	}
	account := body["credentials"].([]any)[0].(map[string]any)
	if account["testing"] != false || account["last_test"] != nil {
		t.Fatalf("stale model-test result survived mapping change: %s", raw)
	}
}

func TestModelTestLateInvalidResultDoesNotDeleteNewRecord(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-model-test-new")
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "open-design-test", "upstream_id": "od/test"}},
	})
	if status != http.StatusOK {
		t.Fatalf("save models status = %d body = %s", status, raw)
	}
	oldCredential, err := s.credentialByID(id)
	if err != nil {
		t.Fatalf("old credential: %v", err)
	}
	oldIdentity := s.modelTestIdentity(oldCredential, "open-design-test", "od/test")
	s.mu.Lock()
	updated := oldCredential
	updated.APIKey = "sk-model-test-replaced"
	s.creds[id] = updated
	s.mu.Unlock()
	newIdentity := s.modelTestIdentity(updated, "open-design-test", "od/test")
	key := id + "\x00open-design-test"
	s.modelTestStarted(key)
	s.modelTestStarted(key)
	newRecord := modelTestRecord{Status: "success", TestedAt: time.Now().UTC()}
	oldRecord := modelTestRecord{Status: "error", TestedAt: newRecord.TestedAt.Add(time.Second)}
	s.modelTestFinished(key, newRecord, newIdentity)
	s.modelTestFinished(key, oldRecord, oldIdentity)

	s.mu.RLock()
	stored, ok := s.modelTests[key]
	s.mu.RUnlock()
	if !ok || stored.Status != "success" {
		t.Fatalf("late invalid result removed current record: ok=%v record=%#v", ok, stored)
	}
}

func TestModelTestShutdownPreservesUpstreamAttempt(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-model-test-shutdown")
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "open-design-test", "upstream_id": "od/test"}},
	})
	if status != http.StatusOK {
		t.Fatalf("save models status = %d body = %s", status, raw)
	}
	host.blockCall()
	host.mu.Lock()
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusOK,
		Body:   []byte(`{"choices":[]}`),
	})
	host.mu.Unlock()
	request := mustJSON(t, ManagementRequest{
		Method: http.MethodPost,
		Path:   apiBase + "/model-test",
		Body:   mustJSON(t, map[string]any{"credential_id": id, "model_id": "open-design-test"}),
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
	if _, err := s.quiesce(); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	response := <-resultCh
	var body map[string]any
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatalf("decode shutdown model-test response: %v", err)
	}
	if body["real_inference"] != true || body["upstream_attempted"] != true {
		t.Fatalf("shutdown lost the already-started upstream attempt: %s", response.Body)
	}
}

func TestModelTestDoesNotClaimInferenceForLocalFailure(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-local-failure")
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "open-design-test", "upstream_id": "od/test"}},
	})
	if status != http.StatusOK {
		t.Fatalf("save models status = %d body = %s", status, raw)
	}
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
		"credential_id": id,
		"model_id":      "open-design-test",
		"messages":      []map[string]any{{"role": "user", "content": "bad\u0001input"}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid message status = %d body = %s", status, raw)
	}
	if body != nil && body["real_inference"] == true {
		t.Fatalf("local validation unexpectedly claimed inference: %#v", body)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	for _, call := range host.calls {
		if call.Method == "host.http.do" {
			t.Fatal("local model-test validation made an upstream request")
		}
	}
}
