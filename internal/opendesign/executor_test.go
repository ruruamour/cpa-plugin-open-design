package opendesign

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// seedModels installs a mapping and returns the credential id.
//
// It also switches the per-account concurrency protection off. The tests that
// use it are about the executor, the logs or the quota pause, and send several
// requests to their single account one right after the other, which is exactly
// what the protection refuses. concurrency_gate_test.go uses seedGatedModels and
// tests the protection with its defaults in place.
func seedModels(t *testing.T, s *Service, host *fakeHost) string {
	t.Helper()
	id := seedGatedModels(t, s, host)
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/config", nil, map[string]any{"concurrency": map[string]any{"enabled": false}})
	if status != http.StatusOK {
		t.Fatalf("switch the concurrency protection off: %d %s", status, raw)
	}
	return id
}

// seedGatedModels is seedModels with the concurrency protection untouched.
func seedGatedModels(t *testing.T, s *Service, host *fakeHost) string {
	t.Helper()
	id := addCredential(t, s, host, "primary", "sk-amr-exec-0001")
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{"models": []map[string]string{
		{"id": "od-large", "upstream_id": "opendesign-large"},
	}})
	if status != http.StatusOK {
		t.Fatalf("seed models: %d %s", status, raw)
	}
	return id
}

func execute(t *testing.T, s *Service, req executorPayload) (any, error) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal executor request: %v", err)
	}
	return s.Handle("executor.execute", raw)
}

func executeStream(t *testing.T, s *Service, req executorPayload) (any, error) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal executor request: %v", err)
	}
	return s.Handle("executor.execute_stream", raw)
}

func TestExecutePreservesToolsUsageAndNumberPrecision(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	upstreamBody := `{"id":"cmpl-1","object":"chat.completion","created":1730000000,"model":"opendesign-large",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"completion_tokens_details":{"reasoning_tokens":3}},` +
		`"seed":12345678901234567890}`
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(upstreamBody)})

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"find x"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}],` +
		`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],` +
		`"tool_choice":"auto","reasoning_effort":"high","stream":false,"seed":12345678901234567890}`)
	result, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: payload})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	answer := result.(map[string]any)
	out, ok := answer["Payload"].([]byte)
	if !ok {
		t.Fatalf("Payload type = %T", answer["Payload"])
	}
	text := string(out)

	var decoded map[string]any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, text)
	}
	if decoded["model"] != "od-large" {
		t.Fatalf("model alias not restored: %v", decoded["model"])
	}
	// A large integer must survive as an integer literal, not as 1.2345678901234567e+19.
	if !strings.Contains(text, "12345678901234567890") {
		t.Fatalf("number precision lost: %s", text)
	}
	if !strings.Contains(text, `"tool_calls"`) || !strings.Contains(text, `"usage"`) {
		t.Fatalf("tool calls or usage dropped: %s", text)
	}

	sent := host.lastPayload("host.http.do")
	body, _ := sent["body"].([]byte)
	requestText := string(body)
	for _, want := range []string{`"tools"`, `"tool_choice":"auto"`, `"reasoning_effort":"high"`, `"model":"opendesign-large"`, `"image_url"`, `12345678901234567890`} {
		if !strings.Contains(requestText, want) {
			t.Errorf("upstream request lost %s: %s", want, requestText)
		}
	}
	if strings.Contains(requestText, `"stream":true`) {
		t.Errorf("non-streaming execute sent stream=true")
	}
	if url, _ := sent["url"].(string); url != DefaultBaseURL+"/chat/completions" {
		t.Errorf("upstream url = %q", url)
	}
}

func TestExecuteNeverForwardsClientAuthorization(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)})
	raw, _ := json.Marshal(map[string]any{
		"AuthID":  id,
		"Model":   "od-large",
		"Payload": []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		"Headers": http.Header{
			"Authorization": []string{"Bearer sk-client-side-secret"},
			"Cookie":        []string{"session=abc"},
			"X-Api-Key":     []string{"sk-client-side-secret"},
		},
	})
	if _, err := s.Handle("executor.execute", raw); err != nil {
		t.Fatalf("execute: %v", err)
	}
	sent := host.lastPayload("host.http.do")
	headers, _ := sent["headers"].(http.Header)
	if headers.Get("Authorization") != "Bearer sk-amr-exec-0001" {
		t.Fatalf("authorization = %q, want the stored key", headers.Get("Authorization"))
	}
	if headers.Get("Cookie") != "" || headers.Get("X-Api-Key") != "" {
		t.Fatalf("client headers leaked upstream: %v", headers)
	}
}

func TestExecutePropagatesUpstreamStatuses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid_api_key: api key not found","code":"invalid_api_key"}}`},
		{"rate limited", http.StatusTooManyRequests, `{"error":{"message":"rate limit exceeded","code":"rate_limit_exceeded"}}`},
		{"server error", http.StatusInternalServerError, `{"error":{"message":"internal failure"}}`},
		{"bad gateway", http.StatusBadGateway, `upstream proxy error`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, host := newTestService(t)
			id := seedModels(t, s, host)
			host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: tc.status, Body: []byte(tc.body)})
			_, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
			if err == nil {
				t.Fatalf("status %d was accepted", tc.status)
			}
			var apiErr *APIError
			if !asAPIError(err, &apiErr) {
				t.Fatalf("error is %T, want *APIError", err)
			}
			if apiErr.Status != tc.status {
				t.Fatalf("status = %d, want %d", apiErr.Status, tc.status)
			}
			if strings.Contains(err.Error(), "sk-amr-exec-0001") {
				t.Fatalf("error leaked the key: %v", err)
			}
		})
	}
}

func TestExecuteStreamEmitsRawFramesAndStopsAtDone(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	frames := []string{
		`data: {"id":"c1","model":"opendesign-large","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n",
		// Split one frame across two transport chunks on purpose.
		`data: {"id":"c1","model":"opendesign-large","choices":[{"index":0,"delta":{"content":"hel`,
		`lo"}}]}` + "\n\n",
		`data: {"id":"c1","model":"opendesign-large","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	chunks := make([][]byte, 0, len(frames))
	for _, f := range frames {
		chunks = append(chunks, []byte(f))
	}
	host.doStreams = append(host.doStreams, scriptedStream{status: http.StatusOK, headers: http.Header{"Content-Type": []string{"text/event-stream"}}, chunks: chunks})

	result, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-1", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	headers := result.(map[string]any)["Headers"].(http.Header)
	if headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("downstream content type = %q", headers.Get("Content-Type"))
	}
	s.waitIdle()

	emitted := host.emitted()
	if len(emitted) != 3 {
		t.Fatalf("emitted %d frames, want 3 (DONE is host-owned): %s", len(emitted), join(emitted))
	}
	for _, frame := range emitted {
		text := string(frame)
		if strings.HasPrefix(text, "data:") {
			t.Fatalf("frame was double-framed: %q", text)
		}
		if !strings.Contains(text, `"model":"od-large"`) {
			t.Fatalf("alias not restored in %q", text)
		}
	}
	if !strings.Contains(string(emitted[1]), `"content":"hello"`) {
		t.Fatalf("split frame was not reassembled: %q", string(emitted[1]))
	}
	if !strings.Contains(string(emitted[2]), `"usage"`) {
		t.Fatalf("usage frame dropped: %q", string(emitted[2]))
	}
	if host.countMethod("host.stream.close") != 1 {
		t.Fatalf("downstream stream was not closed exactly once")
	}
	if host.countMethod("host.http.stream_close") != 1 {
		t.Fatalf("upstream stream was not closed exactly once")
	}
}

func TestExecuteStreamWrapsFramesForMessagesRoute(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{[]byte("data: {\"model\":\"opendesign-large\",\"choices\":[]}\n\n"), []byte("data: [DONE]\n\n")},
	})
	_, err := executeStream(t, s, executorPayload{
		AuthID:   id,
		Model:    "od-large",
		StreamID: "out-2",
		Payload:  []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		Metadata: map[string]any{"request_path": "/v1/messages"},
	})
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	emitted := host.emitted()
	if len(emitted) != 1 {
		t.Fatalf("emitted = %d", len(emitted))
	}
	text := string(emitted[0])
	if !strings.HasPrefix(text, "data: ") || !strings.HasSuffix(text, "\n\n") {
		t.Fatalf("messages route expects pre-framed SSE, got %q", text)
	}
	if strings.Count(text, "data:") != 1 {
		t.Fatalf("double framing: %q", text)
	}
}

func TestExecuteStreamErrorsBeforeAnyOutput(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusTooManyRequests,
		chunks: [][]byte{[]byte(`{"error":{"message":"quota exhausted","code":"insufficient_quota"}}`)},
	})
	_, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-3", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	if err == nil {
		t.Fatalf("429 stream was accepted")
	}
	if status := statusOf(err); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	if strings.Contains(err.Error(), "sk-amr-exec-0001") {
		t.Fatalf("error leaked the key: %v", err)
	}
	if len(host.emitted()) != 0 {
		t.Fatalf("frames were emitted for a failed stream")
	}
	if host.countMethod("host.http.stream_close") != 1 {
		t.Fatalf("upstream stream was not closed")
	}
}

func TestExecuteStreamSplitsManyFramesInOneChunk(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	bundle := []byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{"content":"a"}}]}` + "\n\n" +
		`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{"content":"b"}}]}` + "\n\n" +
		`data: [DONE]` + "\n\n")
	host.doStreams = append(host.doStreams, scriptedStream{status: http.StatusOK, chunks: [][]byte{bundle}})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-4", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	emitted := host.emitted()
	if len(emitted) != 2 {
		t.Fatalf("emitted %d frames, want 2: %s", len(emitted), join(emitted))
	}
	if !strings.Contains(string(emitted[0]), `"content":"a"`) || !strings.Contains(string(emitted[1]), `"content":"b"`) {
		t.Fatalf("frames = %s", join(emitted))
	}
}

func TestExecuteStreamReportsIncompleteStream(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{[]byte("data: {\"model\":\"opendesign-large\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x")},
	})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-5", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	if len(host.streamCloseErrs) != 1 {
		t.Fatalf("incomplete stream was not reported: %v", host.streamCloseErrs)
	}
	if strings.Contains(host.streamCloseErrs[0], "sk-amr-exec-0001") {
		t.Fatalf("stream close error leaked the key")
	}
}

func TestExecuteStreamCancellationClosesBothStreams(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{status: http.StatusOK, chunks: [][]byte{[]byte("data: {\"model\":\"opendesign-large\",\"choices\":[]}\n\n")}})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-6", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	// Quiesce while the pump is still running: it must not block or leak.
	s.waitIdle()
	if _, err := s.Handle("plugin.quiesce", nil); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	if _, err := s.Handle("executor.execute_stream", mustJSON(t, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-7", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})); err == nil {
		t.Fatalf("execute_stream accepted work after quiesce")
	}
}

func TestCountTokensIsAvailableNeutral(t *testing.T) {
	s, _ := newTestService(t)
	_, err := s.Handle("executor.count_tokens", mustJSON(t, map[string]any{}))
	if err == nil {
		t.Fatalf("count_tokens unexpectedly succeeded")
	}
	if status := statusOf(err); status != http.StatusNotFound {
		t.Fatalf("count_tokens status = %d, want 404 so the host does not cool the credential", status)
	}
}

func TestUnsupportedMethodIsRejected(t *testing.T) {
	s, _ := newTestService(t)
	if _, err := s.Handle("quota.fetch", nil); err == nil {
		t.Fatalf("unsupported method was accepted")
	}
	if _, err := s.Handle("model.route", nil); err == nil {
		t.Fatalf("unsupported router method was accepted")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func join(frames [][]byte) string {
	parts := make([]string, 0, len(frames))
	for _, f := range frames {
		parts = append(parts, string(f))
	}
	return strings.Join(parts, " | ")
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if apiErr, ok := err.(*APIError); ok {
			*target = apiErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
