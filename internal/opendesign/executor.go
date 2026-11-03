package opendesign

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ExecutorRequest is the CPA executor RPC payload. Only the fields this plugin
// uses are decoded; unknown host fields are ignored rather than rejected.
type ExecutorRequest struct {
	AuthID          string            `json:"AuthID"`
	AuthProvider    string            `json:"AuthProvider"`
	Model           string            `json:"Model"`
	Format          string            `json:"Format"`
	SourceFormat    string            `json:"SourceFormat"`
	Stream          bool              `json:"Stream"`
	Alt             string            `json:"Alt"`
	Headers         http.Header       `json:"Headers"`
	Query           url.Values        `json:"Query"`
	OriginalRequest []byte            `json:"OriginalRequest"`
	Payload         []byte            `json:"Payload"`
	Metadata        map[string]any    `json:"Metadata"`
	StorageJSON     []byte            `json:"StorageJSON"`
	AuthMetadata    map[string]any    `json:"AuthMetadata"`
	AuthAttributes  map[string]string `json:"AuthAttributes"`

	UpstreamModelOverride string      `json:"-"`
	CredentialOverride    *Credential `json:"-"`

	StreamID       string `json:"stream_id"`
	HostCallbackID string `json:"host_callback_id"`
}

// The host owns the request deadline: `host.http.do` / `host.http.do_stream`
// receive no timeout field (see CPA's rpcHostHTTPRequest), and the host cancels
// the operation when the client request context ends. The plugin therefore adds
// no deadline of its own and never invents a timeout parameter.

func decodeExecutorRequest(raw json.RawMessage) (ExecutorRequest, error) {
	var r ExecutorRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, localFail(http.StatusBadRequest, "invalid_request", "invalid executor request JSON")
	}
	return r, nil
}

// prepare validates the request and produces the outbound OpenAI payload with the
// upstream model ID substituted. The caller's Authorization header is dropped by
// construction: only headers built here reach the network.
func (s *Service) prepare(r ExecutorRequest) (map[string]any, string, error) {
	upstream := strings.TrimSpace(r.UpstreamModelOverride)
	var err error
	if upstream == "" {
		upstream, err = s.resolveModel(r.Model)
		if err != nil {
			return nil, "", err
		}
	}
	// Payload is the provider-translated body. A host that only forwards the raw
	// client request still works because OriginalRequest is the same shape here.
	raw := r.Payload
	if len(raw) == 0 {
		raw = r.OriginalRequest
	}
	j, err := decodeObject(raw)
	if err != nil {
		return nil, "", localFail(http.StatusBadRequest, "invalid_request", "invalid request JSON payload")
	}
	if messages, ok := j["messages"].([]any); !ok || len(messages) == 0 {
		return nil, "", localFail(http.StatusBadRequest, "invalid_request", "messages must be a nonempty array")
	}
	j["model"] = upstream
	return j, upstream, nil
}

// decodeObject preserves every number as json.Number so an integer the client
// sent cannot come back as a float.
func decodeObject(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty payload")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var j map[string]any
	if err := decoder.Decode(&j); err != nil {
		return nil, err
	}
	if j == nil {
		return nil, errors.New("payload is not a JSON object")
	}
	return j, nil
}

// execute handles executor.execute: one buffered upstream request.
func (s *Service) execute(raw json.RawMessage) (any, error) {
	return s.executePinned(raw, "", nil, true)
}

// executePinned is used by explicit model tests to keep the credential and
// alias mapping snapshot used for validation identical to the outbound request.
func (s *Service) executePinned(raw json.RawMessage, upstreamModel string, credential *Credential, manageLifecycle bool) (result any, err error) {
	if manageLifecycle {
		if err := s.begin(); err != nil {
			return nil, err
		}
		defer s.done()
	}
	draft := &requestLogDraft{started: time.Now()}
	defer func() { s.recordRequestLog(draft, err) }()
	r, err := decodeExecutorRequest(raw)
	if err != nil {
		return nil, err
	}
	if upstreamModel != "" {
		r.UpstreamModelOverride = upstreamModel
	}
	if credential != nil {
		r.CredentialOverride = credential
	}
	draft.model = r.Model
	var captured Credential
	_ = json.Unmarshal(r.StorageJSON, &captured)
	draft.secrets = []string{captured.APIKey, captured.ControlKey, captured.WorkspaceID}
	c, err := s.selectedCredential(r)
	draft.secrets = append(draft.secrets, c.APIKey, c.ControlKey, c.WorkspaceID)
	if err != nil {
		return nil, err
	}
	draft.credentialID = c.ID
	j, upstream, err := s.prepare(r)
	if err != nil {
		return nil, err
	}
	draft.upstreamModel = upstream
	delete(j, "stream")
	delete(j, "stream_options")

	body, err := json.Marshal(j)
	if err != nil {
		return nil, localFail(http.StatusBadRequest, "invalid_request", "could not encode upstream request")
	}
	status, headers, respBody, err := s.doJSON(r.HostCallbackID, http.MethodPost, upstreamURL(c.BaseURL, "/chat/completions", nil), upstreamHeaders(c.APIKey), body)
	draft.upstreamAttempted = status >= 100 && status <= 599 || upstreamErrorWasAttempted(err)
	draft.httpStatus = status
	draft.requestID = requestIDFrom(headers)
	if err != nil {
		return nil, err
	}
	keys := s.redactionKeys(c.APIKey, c.ControlKey)
	if status < 200 || status >= 300 {
		return nil, upstreamErrorFromResponse(status, headers, respBody, keys...)
	}
	root, decodeErr := decodeObject(respBody)
	if decodeErr != nil {
		return nil, upstreamProtocolError("upstream returned a non-JSON completion body", status)
	}
	// A 200 carrying a top-level error is an upstream failure, not a completion.
	if errBody := successBodyError(root, status, respBody, keys...); errBody != nil {
		return nil, errBody
	}
	// Only an upstream usage object may populate token counts; a missing or
	// malformed field stays null instead of being guessed.
	draft.usage = tokenUsageFromMap(root)
	// Restore the client-facing alias so the caller sees the model it asked for.
	if _, ok := root["model"]; ok {
		root["model"] = r.Model
		if reencoded, errMarshal := json.Marshal(root); errMarshal == nil {
			respBody = reencoded
		}
	}
	return map[string]any{
		"Payload":  respBody,
		"Headers":  http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		"Metadata": map[string]any{"provider": Provider, "upstream_model": j["model"], "http_status": status},
	}, nil
}

// executeStream handles executor.execute_stream. It returns as soon as the
// upstream stream is open; frames are pushed through host.stream.emit.
func (s *Service) executeStream(raw json.RawMessage) (result any, err error) {
	if err := s.begin(); err != nil {
		return nil, err
	}
	release := sync.OnceFunc(s.done)
	finished := false
	draft := &requestLogDraft{started: time.Now(), stream: true}
	// Every early return still records one metadata row. Once the goroutine takes
	// over, it owns the draft and the release() call.
	defer func() {
		if !finished {
			s.recordRequestLog(draft, err)
			release()
		}
	}()
	r, err := decodeExecutorRequest(raw)
	if err != nil {
		return nil, err
	}
	draft.model = r.Model
	var captured Credential
	_ = json.Unmarshal(r.StorageJSON, &captured)
	draft.secrets = []string{captured.APIKey, captured.ControlKey, captured.WorkspaceID}
	c, err := s.selectedCredential(r)
	draft.secrets = append(draft.secrets, c.APIKey, c.ControlKey, c.WorkspaceID)
	if err != nil {
		return nil, err
	}
	draft.credentialID = c.ID
	j, upstream, err := s.prepare(r)
	if err != nil {
		return nil, err
	}
	draft.upstreamModel = upstream
	if r.StreamID == "" {
		return nil, localFail(http.StatusInternalServerError, "host_contract_error", "host provided no output stream identifier")
	}

	j["stream"] = true
	opts, _ := j["stream_options"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}
	if _, ok := opts["include_usage"]; !ok {
		opts["include_usage"] = true
	}
	j["stream_options"] = opts

	body, err := json.Marshal(j)
	if err != nil {
		return nil, localFail(http.StatusBadRequest, "invalid_request", "could not encode upstream request")
	}
	stream, err := s.openStream(r.HostCallbackID, http.MethodPost, upstreamURL(c.BaseURL, "/chat/completions", nil), upstreamHeaders(c.APIKey), body)
	draft.upstreamAttempted = stream.StatusCode >= 100 && stream.StatusCode <= 599 || upstreamErrorWasAttempted(err)
	draft.httpStatus = stream.StatusCode
	draft.requestID = requestIDFrom(stream.Headers)
	if err != nil {
		return nil, err
	}
	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		// Buffer the error body, then fail the RPC before any output is emitted.
		failure := s.readStreamError(r.HostCallbackID, stream, s.redactionKeys(c.APIKey, c.ControlKey)...)
		return nil, failure
	}
	// CPA owns the downstream envelope: emit raw JSON frames, and only wrap them
	// in `data:` for the Claude route, which expects pre-framed SSE.
	wrapData := strings.TrimSpace(stringValue(r.Metadata["request_path"])) == "/v1/messages"
	emitter := func(frame []byte) error {
		payload := frame
		if wrapData {
			payload = append(append([]byte("data: "), frame...), []byte("\n\n")...)
		}
		if err := s.call("host.stream.emit", map[string]any{"stream_id": r.StreamID, "payload": payload}, nil); err != nil {
			return &APIError{Status: http.StatusBadRequest, Kind: "request_canceled", Message: "client disconnected"}
		}
		return nil
	}
	go func() {
		// The keys are captured now so a credential replaced while the stream is
		// open still redacts the key this request actually used.
		keys := []string{c.APIKey, c.ControlKey}
		usage := &tokenUsage{}
		var streamErr error
		defer release()
		defer func() {
			if recovered := recover(); recovered != nil && streamErr == nil {
				streamErr = upstreamProtocolError("upstream stream processing failed")
			}
			draft.usage = *usage
			func() {
				defer func() {
					if recovered := recover(); recovered != nil && streamErr == nil {
						streamErr = upstreamProtocolError("upstream stream finalization failed")
					}
				}()
				s.finishOutputStream(r.StreamID, streamErr, keys...)
			}()
			s.recordRequestLog(draft, streamErr)
		}()
		streamErr = s.pumpSSE(r.HostCallbackID, stream, r.Model, emitter, requestChoiceCount(j), usage, keys)
	}()
	finished = true
	return map[string]any{
		"Headers": http.Header{"Content-Type": []string{"text/event-stream"}, "Cache-Control": []string{"no-cache"}},
	}, nil
}

// pumpSSE decodes the upstream event stream and emits one JSON frame per event.
// A `[DONE]` marker terminates the stream without being forwarded: the host
// writes its own terminal marker.
func (s *Service) pumpSSE(callbackID string, stream upstreamStream, alias string, emit func([]byte) error, expectedChoices int64, usage *tokenUsage, keys []string) error {
	decoder := &sseDecoder{maxBytes: maxSSEFrame}
	var sawDone bool
	// Per-choice completion: with n>1, one finished choice does not make the
	// response complete, so an EOF must not be reported as success.
	tracker := newChoiceTracker(expectedChoices)
	err := s.readStream(callbackID, stream, func(chunk []byte) error {
		return decoder.feed(chunk, func(event sseEvent) error {
			data := strings.TrimSpace(string(event.Data))
			if data == "" {
				return nil
			}
			if data == "[DONE]" {
				sawDone = true
				return errStreamDone
			}
			root, err := decodeObject(event.Data)
			if err != nil {
				return upstreamProtocolError("upstream returned an invalid JSON frame")
			}
			if root["error"] != nil || strings.EqualFold(event.Event, "error") {
				// The transport status is 200 here; the payload carries the real
				// failure, so classify it as a gateway error rather than success.
				status := stream.StatusCode
				if status < 400 {
					status = http.StatusBadGateway
				}
				// The event body is upstream-controlled: parse it with both the keys
				// this request was sent with and every key known right now, so a code
				// that carries a secret cannot become the error kind or message.
				return upstreamErrorFromResponseObserved(status, stream.StatusCode, nil, event.Data, s.redactionKeys(keys...)...)
			}
			tracker.observe(root)
			if usage != nil {
				// Only values the upstream actually reported are kept; an absent
				// usage object leaves every token count null.
				usage.observe(root)
			}
			if model, ok := root["model"].(string); ok && model != "" {
				root["model"] = alias
			}
			encoded, err := json.Marshal(root)
			if err != nil {
				return upstreamProtocolError("could not re-encode upstream frame")
			}
			return emit(encoded)
		})
	})
	if err != nil && !errors.Is(err, errStreamDone) {
		return err
	}
	if !sawDone {
		// An EOF without [DONE] and without a finish_reason is a truncated
		// stream. Reporting success here would hand the client a partial answer.
		if endErr := decoder.end(); endErr != nil {
			return endErr
		}
		if !tracker.complete() {
			return upstreamProtocolError("upstream stream closed before [DONE] and before every choice finished")
		}
	}
	return nil
}

// choiceTracker follows completion per stream choice index. A stream without
// [DONE] is only accepted when every choice that appeared — and every choice the
// caller asked for via `n` — has a finish_reason. Tracking a single boolean
// would let choice 0 finishing mask a truncated choice 1.
type choiceTracker struct {
	finished        map[int64]bool
	seen            map[int64]bool
	expectedChoices int64
}

func newChoiceTracker(expected ...int64) *choiceTracker {
	t := &choiceTracker{finished: map[int64]bool{}, seen: map[int64]bool{}}
	if len(expected) > 0 && expected[0] > 0 {
		t.expectedChoices = expected[0]
	}
	return t
}

// observe records the completion state carried by one stream frame.
func (t *choiceTracker) observe(root map[string]any) {
	choices, ok := root["choices"].([]any)
	if !ok {
		return
	}
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		index := choiceIndex(choice)
		t.seen[index] = true
		if reason, ok := choice["finish_reason"].(string); ok && strings.TrimSpace(reason) != "" {
			t.finished[index] = true
		}
	}
}

// complete reports whether every observed and expected choice has finished.
func (t *choiceTracker) complete() bool {
	if len(t.seen) == 0 {
		return false
	}
	for index := range t.seen {
		if !t.finished[index] {
			return false
		}
	}
	for index := int64(0); index < t.expectedChoices; index++ {
		if !t.finished[index] {
			return false
		}
	}
	return true
}

func choiceIndex(choice map[string]any) int64 {
	switch v := choice["index"].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
	case float64:
		return int64(v)
	}
	return 0
}

// successBodyError converts a 2xx body that actually carries an error into the
// matching APIError, so a 200-with-error cannot be returned as a completion.
func successBodyError(root map[string]any, status int, raw []byte, keys ...string) error {
	if root["error"] == nil && root["success"] != false {
		return nil
	}
	observedStatus := status
	if status < 400 {
		status = http.StatusBadGateway
	}
	return upstreamErrorFromResponseObserved(status, observedStatus, nil, raw, keys...)
}

// readStreamError reads a non-2xx upstream body and converts it into the matching
// APIError so the client sees the real status.
func (s *Service) readStreamError(callbackID string, stream upstreamStream, keys ...string) error {
	var body []byte
	_ = s.readStream(callbackID, stream, func(chunk []byte) error {
		if len(body) < maxBufferedResponse {
			body = append(body, chunk...)
		}
		return nil
	})
	return upstreamErrorFromResponse(stream.StatusCode, stream.Headers, body, keys...)
}

// finishOutputStream closes the downstream stream exactly once, reporting the
// terminal error when there is one.
//
// CPA's public stream bridge (stream_bridge.go, v8.0.51) reads a singular
// `error_detail` object carrying code/message/retryable/http_status and gives it
// precedence over the plain `error` string. Both are sent: the structured detail
// preserves the real upstream status, and the string stays as a fallback for
// hosts that only read the message.
func (s *Service) finishOutputStream(streamID string, err error, keys ...string) {
	payload := map[string]any{"stream_id": streamID}
	if err != nil {
		// The stream can outlive the credential snapshot it started with, so the
		// current keys are added at close time as well.
		keys = s.redactionKeys(keys...)
		message, status := s.HostErrorDetails(err)
		message = redact(message, keys...)
		payload["error"] = message
		detail := map[string]any{
			"code":        streamErrorDetailCode(err, keys),
			"message":     message,
			"http_status": status,
		}
		if retryable, ok := err.(interface{ Retryable() bool }); ok && retryable.Retryable() {
			detail["retryable"] = true
		}
		payload["error_detail"] = detail
	}
	_ = s.call("host.stream.close", payload, nil)
}

// streamErrorCode reports the machine-readable kind for the structured detail.
func streamErrorCode(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		if code := strings.TrimSpace(coded.Code()); code != "" {
			return code
		}
	}
	return "upstream_error"
}

// streamErrorDetailCode is the code that actually reaches host.stream.close.
// The kind can originate in an upstream error body, where the "code" field is
// attacker- or misconfiguration-controlled, so it is held to the same rule as
// the message: a known key disqualifies it and anything that is not a short
// machine identifier is replaced. Useful upstream codes such as invalid_api_key
// pass through unchanged; the human-readable text stays in the message.
func streamErrorDetailCode(err error, keys []string) string {
	code := streamErrorCode(err)
	if !safeManagementCode(code, keys) {
		return "upstream_error"
	}
	return code
}

// redactionKeys combines the keys captured when a request started with every key
// the plugin knows now, so an error produced after a credential update cannot
// echo either the old or the new secret.
func (s *Service) redactionKeys(captured ...string) []string {
	keys := make([]string, 0, len(captured)+2)
	for _, key := range captured {
		if key != "" {
			keys = append(keys, key)
		}
	}
	return append(keys, s.allKeys()...)
}

// errStreamDone is an internal sentinel; it is never returned to the host.
var errStreamDone = errors.New("upstream stream finished")

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

// requestChoiceCount reads the caller's `n`. Absent or invalid means one choice,
// which is the OpenAI default.
func requestChoiceCount(j map[string]any) int64 {
	switch v := j["n"].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return n
		}
	case float64:
		if v > 0 {
			return int64(v)
		}
	}
	return 1
}
