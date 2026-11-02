package opendesign

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// closeDetail returns the single host.stream.close payload plus its error_detail.
func closeDetail(t *testing.T, host *fakeHost) (map[string]any, map[string]any) {
	t.Helper()
	host.mu.Lock()
	raw := append([]map[string]any(nil), host.streamCloseRaw...)
	host.mu.Unlock()
	if len(raw) != 1 {
		t.Fatalf("host.stream.close called %d times, want 1", len(raw))
	}
	detail, ok := raw[0]["error_detail"].(map[string]any)
	if !ok {
		t.Fatalf("no error_detail in %v", raw[0])
	}
	return raw[0], detail
}

// An SSE error event whose `code` field carries the saved control key must not
// reach error_detail.code: that field is machine-readable and was previously
// neither redacted nor bounded.
func TestStreamErrorCodeNeverEchoesControlKey(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	// A control key has no sk-/Bearer shape, so only the known-key list catches it.
	const control = "ctl-plain-sse-secret-0001"
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": control}); status != http.StatusOK {
		t.Fatalf("save control key: %d %s", status, raw)
	}
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{[]byte(`data: {"error":{"message":"rejected","code":"` + control + `"}}` + "\n\n")},
	})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-c1", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()

	closePayload, detail := closeDetail(t, host)
	code, _ := detail["code"].(string)
	if strings.Contains(code, control) {
		t.Fatalf("error_detail.code leaked the control key: %q", code)
	}
	if code != "upstream_error" {
		t.Fatalf("error_detail.code = %q, want upstream_error", code)
	}
	if message, _ := detail["message"].(string); strings.Contains(message, control) {
		t.Fatalf("error_detail.message leaked the control key: %q", message)
	}
	if text, _ := closePayload["error"].(string); strings.Contains(text, control) {
		t.Fatalf("error fallback leaked the control key: %q", text)
	}
	if encoded := join(host.emitted()); strings.Contains(encoded, control) {
		t.Fatalf("emitted frames leaked the control key")
	}
}

// A useful upstream code must survive unchanged; the rule only rejects secrets,
// wrong characters and oversized values.
func TestStreamErrorCodeKeepsUsefulUpstreamCode(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{[]byte(`data: {"error":{"message":"API key is invalid","code":"invalid_api_key"}}` + "\n\n")},
	})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-c2", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	_, detail := closeDetail(t, host)
	if detail["code"] != "invalid_api_key" {
		t.Fatalf("error_detail.code = %v, want invalid_api_key", detail["code"])
	}
}

// A code carrying prose, punctuation or an unbounded value is not a machine code
// and must be replaced instead of forwarded.
func TestStreamErrorCodeRejectsNonMachineCodes(t *testing.T) {
	cases := map[string]string{
		"punctuation": "bad code! with spaces",
		"too_long":    strings.Repeat("a", 200),
		"newline":     "line\ninjected",
		"empty":       "",
	}
	for name, code := range cases {
		t.Run(name, func(t *testing.T) {
			s, host := newTestService(t)
			id := seedModels(t, s, host)
			host.doStreams = append(host.doStreams, scriptedStream{
				status: http.StatusOK,
				chunks: [][]byte{[]byte(`data: {"error":{"message":"failure","code":` + mustJSONString(t, code) + `}}` + "\n\n")},
			})
			if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-c3", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
				t.Fatalf("execute_stream: %v", err)
			}
			s.waitIdle()
			_, detail := closeDetail(t, host)
			if got, _ := detail["code"].(string); got != "upstream_error" {
				t.Fatalf("error_detail.code = %q, want upstream_error", got)
			}
		})
	}
}

// Replacing the credential while a stream is open must not stop the request's
// own captured key from being redacted in the terminal close, and the new key
// must be redacted too.
func TestStreamErrorCodeRedactsKeysAfterMidStreamReplacement(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	const oldControl = "ctl-captured-stream-0002"
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": oldControl}); status != http.StatusOK {
		t.Fatalf("save old control key: %d %s", status, raw)
	}
	const newControl = "ctl-replacement-stream-0003"

	// The stream opens with the old key, then the operator replaces the control
	// key before the error frame is consumed.
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{[]byte(`data: {"error":{"message":"opaque failure","code":"` + oldControl + `"}}` + "\n\n")},
	})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-c4", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": newControl}); status != http.StatusOK {
		t.Fatalf("replace control key: %d %s", status, raw)
	}
	s.waitIdle()

	closePayload, detail := closeDetail(t, host)
	code, _ := detail["code"].(string)
	if strings.Contains(code, oldControl) || strings.Contains(code, newControl) {
		t.Fatalf("error_detail.code leaked a control key: %q", code)
	}
	if code != "upstream_error" {
		t.Fatalf("error_detail.code = %q, want upstream_error", code)
	}
	if text, _ := closePayload["error"].(string); strings.Contains(text, oldControl) {
		t.Fatalf("error fallback leaked the captured control key: %q", text)
	}
}

// The same guard applies to a non-2xx buffered stream error: the code there also
// comes from the upstream body.
func TestStreamOpenErrorCodeIsBounded(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	const control = "ctl-open-error-0004"
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": control}); status != http.StatusOK {
		t.Fatalf("save control key: %d %s", status, raw)
	}
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusUnauthorized,
		chunks: [][]byte{[]byte(`{"error":{"message":"nope","code":"` + control + `"}}`)},
	})
	_, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-c5", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	if err == nil {
		t.Fatalf("non-2xx stream open was reported as success")
	}
	if strings.Contains(err.Error(), control) {
		t.Fatalf("stream open error leaked the control key: %v", err)
	}
	if code := streamErrorDetailCode(err, s.redactionKeys()); code != "upstream_error" {
		t.Fatalf("stream error code = %q, want upstream_error", code)
	}
}

// The buffered executor path must redact the same way, so both transports agree.
func TestExecuteErrorCodeDoesNotLeakControlKey(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	const control = "ctl-buffered-exec-0005"
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": control}); status != http.StatusOK {
		t.Fatalf("save control key: %d %s", status, raw)
	}
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusUnauthorized,
		Body:   []byte(`{"error":{"message":"rejected","code":"` + control + `"}}`),
	})
	_, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	if err == nil {
		t.Fatalf("upstream error was reported as success")
	}
	if strings.Contains(err.Error(), control) {
		t.Fatalf("buffered error leaked the control key: %v", err)
	}
	if strings.Contains(streamErrorCode(err), control) {
		t.Fatalf("buffered error kind leaked the control key")
	}
}

func mustJSONString(t *testing.T, v string) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal string: %v", err)
	}
	return string(b)
}
