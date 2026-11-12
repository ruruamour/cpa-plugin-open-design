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
	// ReasoningPinned/ReasoningOverride carry an explicit policy snapshot for the
	// explicit model test. Pinning is independent of the upstream override, so a
	// pinned nil stays nil even when the latest published configuration has rules.
	ReasoningPinned   bool              `json:"-"`
	ReasoningOverride *ReasoningMapping `json:"-"`

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

// modelSnapshot is one consistent read of the published model list. The alias
// resolution and the reasoning policy both come from the same slice, so a
// concurrent save can never pair a new upstream ID with an old rule table (or
// vice versa). The policy is a deep copy, so the caller never touches the
// published configuration.
type modelSnapshot struct {
	upstream  string
	reasoning *ReasoningMapping
}

// modelSnapshotFor reads one model row by alias or exact upstream ID under a
// single lock acquisition. It returns ok=false for an unknown model so the
// caller can produce the same model_not_enabled error resolveModel did.
func (s *Service) modelSnapshotFor(model string) (modelSnapshot, bool) {
	model = strings.TrimSpace(model)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.cfg.Models {
		if s.cfg.Models[i].ID == model {
			return modelSnapshot{upstream: s.cfg.Models[i].UpstreamID, reasoning: cloneReasoningMapping(s.cfg.Models[i].Reasoning)}, true
		}
	}
	for i := range s.cfg.Models {
		if s.cfg.Models[i].UpstreamID == model {
			return modelSnapshot{upstream: s.cfg.Models[i].UpstreamID, reasoning: cloneReasoningMapping(s.cfg.Models[i].Reasoning)}, true
		}
	}
	return modelSnapshot{}, false
}

// prepare validates the request and produces the outbound OpenAI payload with the
// upstream model ID substituted. The caller's Authorization header is dropped by
// construction: only headers built here reach the network. The model snapshot is
// taken once and returned so the reasoning policy is applied from the exact same
// configuration the upstream ID came from.
func (s *Service) prepare(r ExecutorRequest) (map[string]any, string, modelSnapshot, error) {
	snapshot := modelSnapshot{}
	upstream := strings.TrimSpace(r.UpstreamModelOverride)
	if upstream == "" {
		var ok bool
		snapshot, ok = s.modelSnapshotFor(r.Model)
		if !ok {
			return nil, "", modelSnapshot{}, &APIError{Status: http.StatusBadRequest, Kind: "model_not_enabled", Message: "model is not enabled"}
		}
		upstream = snapshot.upstream
	} else if !r.ReasoningPinned {
		if resolved, ok := s.modelSnapshotFor(r.Model); ok && resolved.upstream == upstream {
			// An explicit override pins the upstream ID. The policy still has to come
			// from the same row, so it is read in the same snapshot as the alias
			// rather than re-resolved later.
			snapshot = resolved
		}
	}
	if r.ReasoningPinned {
		// The pin is explicit and independent of how the upstream ID was selected.
		// A nil override means "no mapping", so it must not fall back to reading
		// the current configuration.
		snapshot.upstream = upstream
		snapshot.reasoning = cloneReasoningMapping(r.ReasoningOverride)
	}
	// Payload is the provider-translated body. A host that only forwards the raw
	// client request still works because OriginalRequest is the same shape here.
	raw := r.Payload
	if len(raw) == 0 {
		raw = r.OriginalRequest
	}
	j, err := decodeObject(raw)
	if err != nil {
		return nil, "", modelSnapshot{}, localFail(http.StatusBadRequest, "invalid_request", "invalid request JSON payload")
	}
	if messages, ok := j["messages"].([]any); !ok || len(messages) == 0 {
		return nil, "", modelSnapshot{}, localFail(http.StatusBadRequest, "invalid_request", "messages must be a nonempty array")
	}
	j["model"] = upstream
	return j, upstream, snapshot, nil
}

// prepareWithReasoning decodes the outbound body, resolves the upstream model,
// and applies the model's thinking-strength policy. The policy runs once, before
// any attempt, and the same decision feeds both the outbound value and the log
// row, so a recorded conversion always describes the request that was sent.
func (s *Service) prepareWithReasoning(r ExecutorRequest, draft *requestLogDraft) (map[string]any, string, error) {
	j, upstream, snapshot, err := s.prepare(r)
	if err != nil {
		return nil, "", err
	}
	if from, to, applied := applyReasoningMapping(j, snapshot.reasoning); applied && draft != nil {
		draft.reasoningFrom, draft.reasoningTo, draft.reasoningMapped = from, to, true
	}
	if draft != nil {
		draft.reasoningEffort = reasoningEffortOf(j)
	}
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

// reasoningDecision is the executor's own account of the thinking-strength policy
// it applied: the effort actually sent, the rule that produced it and whether an
// upstream call was attempted. The explicit model test echoes this rather than
// re-deriving it, so it can never claim a value the request did not carry.
type reasoningDecision struct {
	effort    string
	mapping   *RequestLogReasoningMapping
	attempted bool
}

// executePinned is used by explicit model tests to keep the credential and
// alias mapping snapshot used for validation identical to the outbound request.
func (s *Service) executePinned(raw json.RawMessage, upstreamModel string, credential *Credential, manageLifecycle bool) (result any, err error) {
	// Legacy helper semantics: an explicit upstream override does not pin the
	// policy, so the executor still resolves the current row's mapping.
	result, _, err = s.executePinnedWithReasoning(raw, upstreamModel, nil, false, credential, manageLifecycle)
	return result, err
}

// executePinnedWithReasoning runs one pinned buffered request and also returns
// the reasoning decision the executor reached. reasoningPinned is explicit and
// separate from upstreamModel: the explicit model test sets it true so a nil
// policy snapshot stays nil, while the legacy executePinned helper sets it false
// and keeps resolving the current policy.
func (s *Service) executePinnedWithReasoning(raw json.RawMessage, upstreamModel string, reasoning *ReasoningMapping, reasoningPinned bool, credential *Credential, manageLifecycle bool) (result any, decision reasoningDecision, err error) {
	if manageLifecycle {
		if err := s.begin(); err != nil {
			return nil, reasoningDecision{}, err
		}
		defer s.done()
	}
	draft := &requestLogDraft{started: time.Now()}
	var c Credential
	var lease *quotaInFlightLease
	// The safety net runs last (LIFO) so a panic in the metadata bookkeeping
	// cannot leave an in-flight counter that would fence recovery forever.
	defer func() { lease.release(s) }()
	defer func() {
		// First, because a new hold is attached to err as the cooldown the host
		// should apply, and err is what this function is about to return.
		s.settleConcurrency(lease, err)
		s.observeQuotaBlockAfterAdmission(c, lease, draft.upstreamModel, err)
		s.recordRequestLog(draft, err)
		// The counter drops only after the quota observation above completed, so
		// an exhausted account is published before the fence disappears.
		lease.release(s)
	}()
	r, err := decodeExecutorRequest(raw)
	if err != nil {
		return nil, reasoningDecision{}, err
	}
	if upstreamModel != "" {
		r.UpstreamModelOverride = upstreamModel
	}
	if reasoningPinned {
		// Pinning true with a nil reasoning is meaningful: the snapshot said this
		// model had no mapping, and a later save must not change that.
		r.ReasoningPinned = true
		r.ReasoningOverride = reasoning
	}
	if credential != nil {
		r.CredentialOverride = credential
	}
	draft.model = r.Model
	var captured Credential
	_ = json.Unmarshal(r.StorageJSON, &captured)
	draft.secrets = []string{captured.APIKey, captured.ControlKey, captured.WorkspaceID}
	c, err = s.selectedCredential(r)
	draft.secrets = append(draft.secrets, c.APIKey, c.ControlKey, c.WorkspaceID)
	if err != nil {
		return nil, reasoningDecision{}, err
	}
	// Admission re-verifies the live credential and counts this inference under
	// the same lock, so a pause, key replacement or removal between selection
	// and here fails locally instead of reaching the upstream.
	lease, err = s.admitQuotaInFlight(c, r.AuthID)
	if err != nil {
		return nil, reasoningDecision{}, err
	}
	draft.credentialID = c.ID
	j, upstream, err := s.prepareWithReasoning(r, draft)
	if err != nil {
		return nil, reasoningDecision{}, err
	}
	draft.upstreamModel = upstream
	decision = reasoningDecision{effort: draft.reasoningEffort}
	if draft.reasoningMapped {
		decision.mapping = &RequestLogReasoningMapping{From: draft.reasoningFrom, To: draft.reasoningTo}
	}
	delete(j, "stream")
	delete(j, "stream_options")

	body, err := json.Marshal(j)
	if err != nil {
		return nil, decision, localFail(http.StatusBadRequest, "invalid_request", "could not encode upstream request")
	}
	status, headers, respBody, err := s.doJSON(r.HostCallbackID, http.MethodPost, upstreamURL(c.BaseURL, "/chat/completions", nil), upstreamHeaders(c.APIKey), body)
	draft.upstreamAttempted = status >= 100 && status <= 599 || upstreamErrorWasAttempted(err)
	decision.attempted = draft.upstreamAttempted
	draft.httpStatus = status
	draft.requestID = requestIDFrom(headers)
	if err != nil {
		return nil, decision, err
	}
	keys := s.redactionKeys(c.APIKey, c.ControlKey)
	if status < 200 || status >= 300 {
		return nil, decision, upstreamErrorFromResponse(status, headers, respBody, keys...)
	}
	root, decodeErr := decodeObject(respBody)
	if decodeErr != nil {
		return nil, decision, upstreamProtocolError("upstream returned a non-JSON completion body", status)
	}
	// A 200 carrying a top-level error is an upstream failure, not a completion.
	if errBody := successBodyError(root, status, headers, respBody, keys...); errBody != nil {
		return nil, decision, errBody
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
	}, decision, nil
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
	var c Credential
	var lease *quotaInFlightLease
	// Every early return records its quota observation and metadata row. Once
	// the goroutine takes over, it owns both observations and the release call.
	// The safety net is declared first so it runs last: a panic cannot leave an
	// in-flight counter behind (release is idempotent).
	defer func() {
		if !finished {
			lease.release(s)
		}
	}()
	defer func() {
		if !finished {
			// Before the error is returned: see the buffered path.
			if err != nil {
				s.settleConcurrency(lease, err)
			}
			s.observeQuotaBlockAfterAdmission(c, lease, draft.upstreamModel, err)
			s.recordRequestLog(draft, err)
			lease.release(s)
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
	c, err = s.selectedCredential(r)
	draft.secrets = append(draft.secrets, c.APIKey, c.ControlKey, c.WorkspaceID)
	if err != nil {
		return nil, err
	}
	// Same admission fence as the buffered path: it runs before any upstream
	// byte is requested, so a pause that landed during selection is respected.
	lease, err = s.admitQuotaInFlight(c, r.AuthID)
	if err != nil {
		return nil, err
	}
	draft.credentialID = c.ID
	j, upstream, err := s.prepareWithReasoning(r, draft)
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
	// The upstream took the request. A stream can run for minutes, so the gate
	// learns this now rather than at the end: a probe that was accepted reopens
	// the account while it is still streaming.
	s.concurrencyAccepted(lease)
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
		// Declared before the metadata defer so it runs after it; the metadata
		// defer drops the counter itself, this is only panic insurance.
		defer lease.release(s)
		defer func() {
			if recovered := recover(); recovered != nil && streamErr == nil {
				streamErr = upstreamProtocolError("upstream stream processing failed")
			}
			draft.usage = *usage
			// A rate limit that arrived as a stream event still holds the
			// account; this runs before finishOutputStream reports the error.
			if streamErr != nil {
				s.settleConcurrency(lease, streamErr)
			}
			// Publish the local guard before notifying the host of a terminal
			// quota error, so an immediate retry cannot reach the exhausted key.
			s.observeQuotaBlockAfterAdmission(c, lease, draft.upstreamModel, streamErr)
			func() {
				defer func() {
					if recovered := recover(); recovered != nil && streamErr == nil {
						streamErr = upstreamProtocolError("upstream stream finalization failed")
					}
				}()
				s.finishOutputStream(r.StreamID, streamErr, keys...)
			}()
			s.recordRequestLog(draft, streamErr)
			// Only after the terminal error was observed, so a pause reported by
			// the last SSE frame is published before the fence disappears.
			lease.release(s)
		}()
		streamErr = s.pumpSSE(r.HostCallbackID, stream, r.Model, emitter, requestChoiceCount(j), usage, func() {
			// Measured from the same origin as the row's duration, when the frame
			// arrived here: downstream send time is not part of it.
			elapsed := time.Since(draft.started).Milliseconds()
			if elapsed < 0 {
				elapsed = 0
			}
			draft.firstTokenMS = &elapsed
		}, keys)
	}()
	finished = true
	return map[string]any{
		"Headers": http.Header{"Content-Type": []string{"text/event-stream"}, "Cache-Control": []string{"no-cache"}},
	}, nil
}

// pumpSSE decodes the upstream event stream and emits one JSON frame per event.
// A `[DONE]` marker terminates the stream without being forwarded: the host
// writes its own terminal marker. onOutput, when set, is called once, before
// the first frame that carries output is forwarded.
func (s *Service) pumpSSE(callbackID string, stream upstreamStream, alias string, emit func([]byte) error, expectedChoices int64, usage *tokenUsage, onOutput func(), keys []string) error {
	decoder := &sseDecoder{maxBytes: maxSSEFrame}
	var sawDone bool
	outputSeen := onOutput == nil
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
				return upstreamErrorFromResponseObserved(status, stream.StatusCode, stream.Headers, event.Data, s.redactionKeys(keys...)...)
			}
			tracker.observe(root)
			if !outputSeen && frameCarriesOutput(root) {
				outputSeen = true
				onOutput()
			}
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

// frameCarriesOutput reports whether a stream frame delivers something the
// model produced: text, reasoning, a refusal or a tool call. A role-only
// delta, an empty delta, a bare finish_reason and a usage-only frame do not
// count, so the time to first output is not satisfied by a heartbeat.
func frameCarriesOutput(root map[string]any) bool {
	choices, ok := root["choices"].([]any)
	if !ok {
		return false
	}
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if outputValue(choice["text"]) {
			return true
		}
		for _, key := range []string{"delta", "message"} {
			part, ok := choice[key].(map[string]any)
			if !ok {
				continue
			}
			for _, field := range []string{"content", "reasoning_content", "reasoning", "reasoning_details", "refusal", "tool_calls", "function_call"} {
				if outputValue(part[field]) {
					return true
				}
			}
		}
	}
	return false
}

// outputValue is true for a non-empty string, array or object.
func outputValue(value any) bool {
	switch v := value.(type) {
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return false
	}
}

// successBodyError converts a 2xx body that actually carries an error into the
// matching APIError, so a 200-with-error cannot be returned as a completion.
func successBodyError(root map[string]any, status int, headers http.Header, raw []byte, keys ...string) error {
	if root["error"] == nil && root["success"] != false {
		return nil
	}
	observedStatus := status
	if status < 400 {
		status = http.StatusBadGateway
	}
	return upstreamErrorFromResponseObserved(status, observedStatus, headers, raw, keys...)
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
		if seconds := HostRetryAfterFor(err, status); seconds > 0 {
			detail["retry_after_seconds"] = seconds
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
