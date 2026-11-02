package opendesign

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The buffered host callback marshals sdk/pluginapi.HTTPResponse, which has no
// JSON tags, so the plugin must read StatusCode/Headers/Body. A snake_case
// decoder silently yields status 0 and would turn every 401/429 into a success.
func TestBufferedHostResponseUsesPascalCaseSchema(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)

	// 200 success
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusOK,
		Body:   []byte(`{"model":"opendesign-large","choices":[]}`),
	})
	result, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	if err != nil {
		t.Fatalf("200: %v", err)
	}
	if payload, ok := result.(map[string]any)["Payload"].([]byte); !ok || len(payload) == 0 {
		t.Fatalf("200 payload = %v", result)
	}

	// 401 must not be reported as success
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusUnauthorized,
		Body:   []byte(`{"error":{"message":"invalid_api_key","code":"invalid_api_key"}}`),
	})
	if _, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err == nil {
		t.Fatalf("401 was accepted as a completion")
	} else if status := statusOf(err); status != http.StatusUnauthorized {
		t.Fatalf("401 status = %d", status)
	}

	// 429 must keep its status
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusTooManyRequests,
		Body:   []byte(`{"error":{"message":"rate limited","code":"rate_limit_exceeded"}}`),
	})
	if _, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err == nil {
		t.Fatalf("429 was accepted as a completion")
	} else if status := statusOf(err); status != http.StatusTooManyRequests {
		t.Fatalf("429 status = %d", status)
	}
}

// Model discovery uses the same buffered callback, so a 401 from GET /models has
// to reach the console as 401 rather than as an empty model list.
func TestModelDiscoveryUsesPascalCaseSchema(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-schema-0002")
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"data":[{"id":"m1"}]}`)})
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/upstream-models", map[string][]string{"credential_id": {id}}, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body %s", status, raw)
	}
	if len(body["data"].([]any)) != 1 {
		t.Fatalf("models = %v", body["data"])
	}

	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusUnauthorized, Body: []byte(`{"error":{"message":"api key not found"}}`)})
	status, _, _ = managementCall(t, s, http.MethodGet, apiBase+"/upstream-models", map[string][]string{"credential_id": {id}}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("401 discovery status = %d", status)
	}
}

// A host callback blocked waiting for upstream headers must still be released by
// quiesce. Only host.http.cancel can do this; closing stream ids cannot.
func TestQuiesceCancelsBlockedBufferedCall(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.blockCall()
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"choices":[]}`)})

	done := make(chan struct{})
	go func() {
		_, _ = execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
		close(done)
	}()
	host.awaitBlocked(t)

	finished := make(chan struct{})
	go func() {
		_, _ = s.Handle("plugin.quiesce", nil)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatalf("quiesce did not finish; blocked host call was never cancelled")
	}
	if len(host.cancels()) == 0 {
		t.Fatalf("quiesce did not call host.http.cancel")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("blocked execute did not return after cancel")
	}
	assertIdle(t, s)
}

// assertIdle proves the active counter is released so a later quiesce or
// shutdown cannot hang on work that already ended.
func assertIdle(t *testing.T, s *Service) {
	t.Helper()
	released := make(chan struct{})
	go func() {
		s.waitIdle()
		close(released)
	}()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatalf("active counter was never released")
	}
}

// The same applies to a stream that has not returned its headers yet.
func TestQuiesceCancelsBlockedStreamOpen(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.blockCall()
	host.doStreams = append(host.doStreams, scriptedStream{status: http.StatusOK, chunks: [][]byte{[]byte("data: [DONE]\n\n")}})

	done := make(chan struct{})
	go func() {
		_, _ = executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-q1", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
		close(done)
	}()
	host.awaitBlocked(t)

	finished := make(chan struct{})
	go func() {
		_, _ = s.Handle("plugin.quiesce", nil)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatalf("quiesce did not finish for a blocked stream open")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("blocked execute_stream did not return after cancel")
	}
	assertIdle(t, s)
}

// Management model discovery is a long-running host call too; it must not keep
// the plugin alive forever once quiesce starts.
func TestQuiesceCancelsBlockedManagementDiscovery(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-quiesce-0003")
	host.blockCall()
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"data":[]}`)})

	done := make(chan struct{})
	go func() {
		managementCall(t, s, http.MethodGet, apiBase+"/upstream-models", map[string][]string{"credential_id": {id}}, nil)
		close(done)
	}()
	host.awaitBlocked(t)

	finished := make(chan struct{})
	go func() {
		_, _ = s.Handle("plugin.quiesce", nil)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatalf("quiesce did not finish while discovery was blocked")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("blocked discovery did not return after cancel")
	}
	assertIdle(t, s)
	// A quiesced plugin must not accept new discovery work.
	if status, _, _ := managementCall(t, s, http.MethodGet, apiBase+"/upstream-models", map[string][]string{"credential_id": {id}}, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("discovery after quiesce status = %d, want 503", status)
	}
}

// With n=2, choice 0 finishing must not make a truncated stream look complete.
func TestStreamTruncationTracksEveryChoiceIndex(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{
			[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"stop"},{"index":1,"delta":{"content":"b"}}]}` + "\n\n"),
		},
	})
	if _, err := executeStream(t, s, executorPayload{
		AuthID:   id,
		Model:    "od-large",
		StreamID: "out-c2",
		Payload:  []byte(`{"messages":[{"role":"user","content":"hi"}],"n":2}`),
	}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	if len(host.streamCloseErrs) != 1 {
		t.Fatalf("truncated second choice was accepted: %v", host.streamCloseErrs)
	}
	if !strings.Contains(host.streamCloseErrs[0], "choice") {
		t.Fatalf("error does not mention completion tracking: %q", host.streamCloseErrs[0])
	}
}

// When every choice finishes, EOF without [DONE] is acceptable.
func TestStreamAllChoicesFinishedAcceptedWithoutDone(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{
			[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"stop"}]}` + "\n\n"),
			[]byte(`data: {"model":"opendesign-large","choices":[{"index":1,"delta":{"content":"b"},"finish_reason":"stop"}]}` + "\n\n"),
		},
	})
	if _, err := executeStream(t, s, executorPayload{
		AuthID:   id,
		Model:    "od-large",
		StreamID: "out-c3",
		Payload:  []byte(`{"messages":[{"role":"user","content":"hi"}],"n":2}`),
	}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	if len(host.streamCloseErrs) != 0 {
		t.Fatalf("complete two-choice stream reported as failed: %v", host.streamCloseErrs)
	}
	if len(host.emitted()) != 2 {
		t.Fatalf("emitted = %d", len(host.emitted()))
	}
}

// `n` in the request raises the expected choice count even if a choice never
// appears in the stream.
func TestStreamMissingExpectedChoiceIsFailure(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{
			[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"stop"}]}` + "\n\n"),
		},
	})
	if _, err := executeStream(t, s, executorPayload{
		AuthID:   id,
		Model:    "od-large",
		StreamID: "out-c4",
		Payload:  []byte(`{"messages":[{"role":"user","content":"hi"}],"n":3}`),
	}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	if len(host.streamCloseErrs) != 1 {
		t.Fatalf("missing expected choices were accepted: %v", host.streamCloseErrs)
	}
}

// The PascalCase schema is authoritative, but a snake_case payload must not
// silently decode to status 0 either.
func TestUpstreamCallDecodesBothSchemas(t *testing.T) {
	var pascal upstreamCall
	if err := json.Unmarshal([]byte(`{"StatusCode":429,"Headers":{"X-A":["b"]},"Body":"aGk="}`), &pascal); err != nil {
		t.Fatalf("pascal: %v", err)
	}
	if pascal.StatusCode != 429 || pascal.Headers.Get("X-A") != "b" || string(pascal.Body) != "hi" {
		t.Fatalf("pascal decode = %+v", pascal)
	}
	var snake upstreamCall
	if err := json.Unmarshal([]byte(`{"status_code":401,"headers":{"X-A":["b"]},"body":"aGk="}`), &snake); err != nil {
		t.Fatalf("snake: %v", err)
	}
	if snake.StatusCode != 401 || string(snake.Body) != "hi" {
		t.Fatalf("snake decode = %+v", snake)
	}
}

// Characterize the per-choice rule directly: a finished choice never covers an
// unfinished sibling.
func TestChoiceTrackerRequiresEveryChoice(t *testing.T) {
	mustDecode := func(raw string) map[string]any {
		decoded, err := decodeObject([]byte(raw))
		if err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return decoded
	}
	partial := newChoiceTracker(2)
	partial.observe(mustDecode(`{"choices":[{"index":0,"finish_reason":"stop"},{"index":1}]}`))
	if partial.complete() {
		t.Fatalf("one finished choice out of two was treated as complete")
	}
	partial.observe(mustDecode(`{"choices":[{"index":1,"finish_reason":"length"}]}`))
	if !partial.complete() {
		t.Fatalf("both choices finished but the stream was not complete")
	}
	// An expected choice that never appears keeps the stream incomplete.
	if newChoiceTracker(3).complete() {
		t.Fatalf("empty stream with n=3 was treated as complete")
	}
	missing := newChoiceTracker(3)
	missing.observe(mustDecode(`{"choices":[{"index":0,"finish_reason":"stop"}]}`))
	if missing.complete() {
		t.Fatalf("n=3 with only choice 0 finished was treated as complete")
	}
	// Out-of-order completion still counts per index.
	outOfOrder := newChoiceTracker(2)
	outOfOrder.observe(mustDecode(`{"choices":[{"index":1,"finish_reason":"stop"}]}`))
	outOfOrder.observe(mustDecode(`{"choices":[{"index":0,"finish_reason":"stop"}]}`))
	if !outOfOrder.complete() {
		t.Fatalf("out-of-order completion was not recognized")
	}
}
