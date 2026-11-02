package opendesign

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A 2xx that is not JSON must not be reported as a completion.
func TestExecuteRejectsNonJSONSuccess(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"text/html"}},
		Body:    []byte("<html><body>gateway</body></html>"),
	})
	_, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	if err == nil {
		t.Fatalf("non-JSON 200 was treated as a completion")
	}
	if status := statusOf(err); status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
}

// A 200 whose body carries a top-level error is a failure, not a completion.
func TestExecuteTreatsSuccessBodyErrorAsFailure(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusOK,
		Body:   []byte(`{"error":{"message":"upstream rejected the request","code":"invalid_request"}}`),
	})
	_, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	if err == nil {
		t.Fatalf("200-with-error was returned as success")
	}
	if status := statusOf(err); status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if !strings.Contains(err.Error(), "invalid_request") {
		t.Fatalf("upstream code lost: %v", err)
	}
}

// An SSE stream that ends without [DONE] and without finish_reason is truncated.
func TestExecuteStreamTruncatedWithoutDoneIsFailure(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{
			[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n"),
		},
	})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-t1", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	if len(host.streamCloseErrs) != 1 {
		t.Fatalf("truncated stream reported %d errors: %v", len(host.streamCloseErrs), host.streamCloseErrs)
	}
	if !strings.Contains(host.streamCloseErrs[0], "finish_reason") && !strings.Contains(host.streamCloseErrs[0], "[DONE]") {
		t.Fatalf("truncation error is unclear: %q", host.streamCloseErrs[0])
	}
}

// finish_reason without [DONE] is still a complete stream on the wire.
func TestExecuteStreamAcceptsFinishReasonWithoutDone(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{
			[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"),
		},
	})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-t2", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	if len(host.streamCloseErrs) != 0 {
		t.Fatalf("complete stream was reported as failed: %v", host.streamCloseErrs)
	}
	if len(host.emitted()) != 1 {
		t.Fatalf("emitted = %d", len(host.emitted()))
	}
}

// An in-stream error event arrives inside an HTTP 200, so it must be classified
// as an upstream failure rather than as a successful frame.
func TestExecuteStreamErrorEventIsFailureNotSuccess(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{
			[]byte(`data: {"error":{"message":"overloaded","code":"server_overloaded"}}` + "\n\n"),
		},
	})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-t3", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	if len(host.emitted()) != 0 {
		t.Fatalf("error payload was emitted as a normal frame")
	}
	if len(host.streamCloseErrs) != 1 || !strings.Contains(host.streamCloseErrs[0], "server_overloaded") {
		t.Fatalf("stream close errors = %v", host.streamCloseErrs)
	}
}

// A malformed frame mid-stream must abort rather than be skipped.
func TestExecuteStreamMalformedFrameAborts(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{
			[]byte(`data: {"model":"opendesign-large","choices":[]}` + "\n\n"),
			[]byte("data: not-json\n\n"),
		},
	})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-t4", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	if len(host.streamCloseErrs) != 1 {
		t.Fatalf("malformed frame was not reported")
	}
	if len(host.emitted()) != 1 {
		t.Fatalf("frames emitted before the malformed one = %d, want 1", len(host.emitted()))
	}
}

// Upstream transport failures surface as 502 and keep the key out of the message.
func TestExecuteTransportFailureIsRedacted(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Err: errString("dial tcp: connection refused for Bearer sk-amr-exec-0001")})
	_, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	if err == nil {
		t.Fatalf("transport failure was ignored")
	}
	if status := statusOf(err); status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	message, _ := s.HostErrorDetails(err)
	if strings.Contains(message, "sk-amr-exec-0001") {
		t.Fatalf("error leaked the key: %s", message)
	}
}

// The plugin must not send a timeout field the host does not understand.
func TestHostHTTPPayloadUsesOnlyDocumentedFields(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)})
	if _, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	payload := host.lastPayload("host.http.do")
	allowed := map[string]bool{"host_callback_id": true, "method": true, "url": true, "headers": true, "body": true}
	for key := range payload {
		if !allowed[key] {
			t.Fatalf("undocumented host.http.do field %q", key)
		}
	}
}

func TestStreamFramesPreserveNumericPrecision(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{
			[]byte(`data: {"model":"opendesign-large","choices":[],"usage":{"prompt_tokens":3},"x_big":12345678901234567890}` + "\n\n"),
			[]byte("data: [DONE]\n\n"),
		},
	})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-n1", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	emitted := host.emitted()
	if len(emitted) != 1 {
		t.Fatalf("emitted = %d", len(emitted))
	}
	if !strings.Contains(string(emitted[0]), "12345678901234567890") {
		t.Fatalf("numeric precision lost: %s", emitted[0])
	}
}

type errString string

func (e errString) Error() string { return string(e) }

var _ = json.Marshal

// The structured stream error uses the singular `error_detail` field CPA reads,
// with the plain `error` string retained as a fallback.
func TestStreamCloseCarriesSingularErrorDetail(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{[]byte(`data: {"error":{"message":"overloaded","code":"server_overloaded"}}` + "\n\n")},
	})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-d1", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	host.mu.Lock()
	raw := append([]map[string]any(nil), host.streamCloseRaw...)
	host.mu.Unlock()
	if len(raw) != 1 {
		t.Fatalf("host.stream.close called %d times", len(raw))
	}
	detail, ok := raw[0]["error_detail"].(map[string]any)
	if !ok {
		t.Fatalf("no singular error_detail in %v", raw[0])
	}
	if _, plural := raw[0]["error_details"]; plural {
		t.Fatalf("plural error_details must not be sent: %v", raw[0])
	}
	if detail["code"] != "server_overloaded" {
		t.Fatalf("code = %v", detail["code"])
	}
	if detail["http_status"] != http.StatusBadGateway {
		t.Fatalf("http_status = %v, want 502", detail["http_status"])
	}
	if text, _ := detail["message"].(string); !strings.Contains(text, "overloaded") {
		t.Fatalf("detail message = %q", text)
	}
	if text, _ := raw[0]["error"].(string); text == "" {
		t.Fatalf("plain error fallback missing")
	}
}

// A clean stream close must not invent an error detail.
func TestStreamCloseIsBareOnSuccess(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{
			[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"),
			[]byte("data: [DONE]\n\n"),
		},
	})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-d2", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	host.mu.Lock()
	raw := append([]map[string]any(nil), host.streamCloseRaw...)
	host.mu.Unlock()
	if len(raw) != 1 {
		t.Fatalf("host.stream.close called %d times", len(raw))
	}
	if _, exists := raw[0]["error"]; exists {
		t.Fatalf("successful close carried an error: %v", raw[0])
	}
	if _, exists := raw[0]["error_detail"]; exists {
		t.Fatalf("successful close carried an error_detail: %v", raw[0])
	}
}
