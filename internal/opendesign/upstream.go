package opendesign

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// upstreamCall is one buffered request through the host transport.
//
// The host marshals sdk/pluginapi.HTTPResponse directly, and that struct has no
// JSON tags (internal/pluginhost/host_callbacks.go: `marshalRPCResult(resp)`), so
// the wire keys are the Go field names. Streaming uses a different, snake_case
// schema (see upstreamStream).
type upstreamCall struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// UnmarshalJSON reads the host response. v8.0.48 marshals the untagged SDK
// struct, so StatusCode/Headers/Body is authoritative; the snake_case aliases
// are only a compatibility fallback.
func (u *upstreamCall) UnmarshalJSON(raw []byte) error {
	var wire struct {
		StatusCode      int         `json:"StatusCode"`
		Headers         http.Header `json:"Headers"`
		Body            []byte      `json:"Body"`
		StatusCodeSnake int         `json:"status_code"`
		HeadersSnake    http.Header `json:"headers"`
		BodySnake       []byte      `json:"body"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	u.StatusCode, u.Headers, u.Body = wire.StatusCode, wire.Headers, wire.Body
	if u.StatusCode == 0 {
		u.StatusCode = wire.StatusCodeSnake
	}
	if u.Headers == nil {
		u.Headers = wire.HeadersSnake
	}
	if len(u.Body) == 0 {
		u.Body = wire.BodySnake
	}
	return nil
}

// upstreamStream is the handle returned by host.http.do_stream. StreamID must be
// closed on every exit path.
type upstreamStream struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
	Error      string      `json:"error"`
}

// readChunk is one host.http.stream_read result.
type readChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

const (
	// maxBufferedResponse bounds a buffered (non-stream) upstream body.
	maxBufferedResponse = 32 << 20
	// maxSSEFrame bounds a single logical event payload.
	maxSSEFrame = 8 << 20
)

// upstreamProtocolError marks a response we could not interpret. It is a 502
// because the failure is the upstream's, not the caller's. The marker keeps it
// matchable by a request-scoped rule even though the status is synthesized.
func upstreamProtocolError(message string, status ...int) error {
	upstreamStatus := 0
	if len(status) > 0 {
		upstreamStatus = status[0]
	}
	return &APIError{Status: http.StatusBadGateway, Kind: "upstream_protocol_error", Message: upstreamErrorMarkerFor("upstream_protocol_error") + " " + message, upstreamAttempted: true, upstreamStatus: upstreamStatus}
}

// upstreamHeaders is the complete outbound header set. The client's own
// Authorization is never copied: only our bearer token leaves the plugin.
func upstreamHeaders(apiKey string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", "OpenDesign-CPA/"+Version)
	return h
}

func upstreamURL(baseURL, path string, query url.Values) string {
	u := strings.TrimRight(baseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// doJSON performs a buffered host HTTP call and returns the raw body with its
// status. Non-2xx responses are returned intact so the caller can decide.
func (s *Service) doJSON(callbackID, method, url string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	payload := map[string]any{
		"host_callback_id": callbackID,
		"method":           method,
		"url":              url,
		"headers":          headers,
	}
	if len(body) > 0 {
		payload["body"] = body
	}
	var out upstreamCall
	if err := s.doOperation(callbackID, "host.http.do", payload, &out); err != nil {
		return out.StatusCode, out.Headers, nil, upstreamTransportError(err, out.StatusCode)
	}
	raw := out.Body
	if len(raw) > maxBufferedResponse {
		return out.StatusCode, out.Headers, nil, &APIError{
			Status:            http.StatusRequestEntityTooLarge,
			Kind:              "response_limit_exceeded",
			Message:           upstreamErrorMarkerFor("response_limit_exceeded") + " upstream response exceeds plugin limit",
			upstreamAttempted: true,
			upstreamStatus:    out.StatusCode,
		}
	}
	return out.StatusCode, out.Headers, raw, nil
}

// openStream starts a streaming host HTTP call. The caller must close the
// returned stream ID.
func (s *Service) openStream(callbackID, method, url string, headers http.Header, body []byte) (upstreamStream, error) {
	payload := map[string]any{
		"host_callback_id": callbackID,
		"method":           method,
		"url":              url,
		"headers":          headers,
	}
	if len(body) > 0 {
		payload["body"] = body
	}
	var out upstreamStream
	if err := s.doOperation(callbackID, "host.http.do_stream", payload, &out); err != nil {
		s.closeUpstream(out.StreamID)
		return out, upstreamTransportError(err, out.StatusCode)
	}
	if out.Error != "" {
		s.closeUpstream(out.StreamID)
		return out, upstreamTransportError(errors.New(out.Error), out.StatusCode)
	}
	if out.StreamID == "" {
		return out, &APIError{Status: http.StatusBadGateway, Kind: "host_contract_error", Message: upstreamErrorMarkerFor("host_contract_error") + " host returned no upstream stream identifier", upstreamAttempted: true, upstreamStatus: out.StatusCode}
	}
	s.mu.Lock()
	stopped := s.stopped
	if !stopped {
		s.streams[out.StreamID] = struct{}{}
	}
	s.mu.Unlock()
	if stopped {
		s.closeUpstream(out.StreamID)
		return out, &APIError{Status: http.StatusServiceUnavailable, Kind: "plugin_unavailable", Message: upstreamErrorMarkerFor("plugin_unavailable") + " OpenDesign plugin is shutting down", upstreamAttempted: true, upstreamStatus: out.StatusCode}
	}
	return out, nil
}

// readStream drains the upstream stream, invoking emit for every chunk. It always
// closes the stream. A non-2xx status is surfaced as an APIError carrying the
// real upstream status after the body is read for diagnostics.
func (s *Service) readStream(callbackID string, stream upstreamStream, emit func([]byte) error) error {
	defer s.closeUpstream(stream.StreamID)
	for {
		var chunk readChunk
		if err := s.call("host.http.stream_read", map[string]any{"stream_id": stream.StreamID, "host_callback_id": callbackID}, &chunk); err != nil {
			return upstreamTransportError(err)
		}
		if len(chunk.Payload) > 0 && emit != nil {
			if err := emit(chunk.Payload); err != nil {
				return err
			}
		}
		if chunk.Error != "" {
			return upstreamTransportError(errors.New(chunk.Error))
		}
		if chunk.Done {
			return nil
		}
	}
}

func (s *Service) closeUpstream(streamID string) {
	if streamID == "" {
		return
	}
	_ = s.call("host.http.stream_close", map[string]any{"stream_id": streamID}, nil)
	s.mu.Lock()
	delete(s.streams, streamID)
	s.mu.Unlock()
}

// upstreamTransportError exposes a controlled summary and preserves whether the
// host HTTP callback actually ran. Opening an operation alone is not an attempt.
func upstreamTransportError(err error, observedStatus ...int) error {
	var operationErr *hostOperationError
	if errors.As(err, &operationErr) && !operationErr.attempted {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return apiErr
		}
		return localFail(http.StatusBadGateway, "host_contract_error", "host could not start the upstream operation")
	}
	status := 0
	if len(observedStatus) > 0 && observedStatus[0] >= 100 && observedStatus[0] <= 599 {
		status = observedStatus[0]
	}
	return &APIError{Status: http.StatusBadGateway, Kind: "upstream_transport_error", Message: upstreamErrorMarkerFor("upstream_transport_error") + " upstream request failed", upstreamAttempted: true, upstreamStatus: status}
}

func upstreamErrorWasAttempted(err error) bool {
	var attempted interface{ UpstreamAttempted() bool }
	return errors.As(err, &attempted) && attempted.UpstreamAttempted()
}

func upstreamErrorFromResponse(status int, headers http.Header, body []byte, keys ...string) error {
	return upstreamErrorFromResponseObserved(status, status, headers, body, keys...)
}

func upstreamErrorFromResponseObserved(status, observedStatus int, headers http.Header, body []byte, keys ...string) error {
	if observedStatus < 100 || observedStatus > 599 {
		observedStatus = status
	}
	if status < 100 || status > 599 {
		status = http.StatusBadGateway
	}
	kind := "upstream_error"
	// Read only a bounded, structured error code. Upstream message text is
	// untrusted and can contain a prompt, completion, workspace, or another
	// credential, so it never crosses into a management response.
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Error) > 0 {
		var detail struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Error, &detail) == nil {
			candidate := detail.Code
			if candidate == "" {
				if colon := strings.IndexByte(detail.Message, ':'); colon > 0 {
					candidate = strings.TrimSpace(detail.Message[:colon])
				}
			}
			// A code that is known and machine-shaped names the failure. OpenAI's
			// error type field is consulted too: the production tier limit arrives
			// as a bracketed code inside the message with type=invalid_request_error,
			// so the type alone is not specific enough to classify it.
			if isKnownUpstreamCode(candidate) && safeManagementCode(candidate, keys) {
				kind = candidate
			}
		}
	}
	// Some upstreams report the classification in the message rather than a code
	// field. Only a closed allowlist of known codes is recognized, and the text
	// around it never becomes part of the plugin message.
	if kind == "upstream_error" {
		if marker := knownUpstreamCodeFromMessage(body); marker != "" {
			kind = marker
		}
	}
	// The marker is built only from the closed allowlist above. CPA matches its
	// request-scoped error rules against this message, so it must stay stable and
	// must never echo upstream-controlled text.
	message := upstreamErrorMarkerFor(kind) + " upstream request failed"
	if text := http.StatusText(observedStatus); text != "" {
		message += ": " + strings.ToLower(text)
	}
	return &APIError{Status: status, Kind: kind, Message: message, upstreamAttempted: true, upstreamStatus: observedStatus}
}

// isKnownUpstreamCode reports whether an upstream code is on the closed
// allowlist that the plugin is willing to name.
//
// insufficient_balance is OpenDesign's own code for a spent coding plan or an
// uncovered wallet. It is listed separately from insufficient_quota because the
// two upstreams mean different things by them and the plugin's default rules
// key on distinct markers.
func isKnownUpstreamCode(code string) bool {
	switch code {
	case "invalid_api_key", "invalid_request", "rate_limit_exceeded", "insufficient_quota", "insufficient_balance",
		"model_not_found", "context_length_exceeded", "server_error", "server_overloaded", "tier_limit_exceeded":
		return true
	default:
		return false
	}
}

// knownUpstreamCodeFromMessage recognizes an allowlisted error code that an
// upstream embedded in free-form text, e.g. OpenDesign's
// "[code=tier_limit_exceeded] membership concurrency limit exceeded". Only a
// code on the closed allowlist is returned; the surrounding text is discarded,
// so upstream content can never forge a rule marker.
func knownUpstreamCodeFromMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	text := string(body)
	const marker = "[code="
	for start := 0; ; {
		index := strings.Index(text[start:], marker)
		if index < 0 {
			return ""
		}
		value := text[start+index+len(marker):]
		end := strings.IndexByte(value, ']')
		if end < 0 {
			return ""
		}
		candidate := strings.TrimSpace(value[:end])
		if isKnownUpstreamCode(candidate) {
			return candidate
		}
		start += index + len(marker)
	}
}

func truncate(b []byte, limit int) []byte {
	if len(b) <= limit {
		return b
	}
	return b[:limit]
}

// requestIDFrom extracts a correlation header for diagnostics. Never logged with
// request bodies.
func requestIDFrom(headers http.Header) string {
	for _, name := range []string{"X-Request-Id", "X-Request-ID", "Cf-Ray"} {
		if v := headers.Get(name); v != "" {
			return v
		}
	}
	return ""
}

// decodeModels parses the standard OpenAI model list, ignoring unknown fields.
func decodeModels(body []byte) ([]UpstreamModel, error) {
	var list struct {
		Data []UpstreamModel `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, upstreamProtocolError("upstream model list is not valid JSON")
	}
	out := make([]UpstreamModel, 0, len(list.Data))
	seen := map[string]bool{}
	for _, item := range list.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		item.ID = id
		out = append(out, item)
	}
	return out, nil
}

// UpstreamModel is one entry of GET /models.
type UpstreamModel struct {
	ID      string `json:"id"`
	Object  string `json:"object,omitempty"`
	OwnedBy string `json:"owned_by,omitempty"`
	Created int64  `json:"created,omitempty"`
}

// readAllLimited reads at most limit bytes from r.
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(r, limit)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
