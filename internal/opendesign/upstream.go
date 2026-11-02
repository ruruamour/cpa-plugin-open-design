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

// upstreamCall is one buffered request through the host transport. Streaming
// responses go through upstreamStream instead.
type upstreamCall struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
	Error      string      `json:"error"`
	Body       []byte      `json:"body,omitempty"`
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
// because the failure is the upstream's, not the caller's.
func upstreamProtocolError(message string) error {
	return &APIError{Status: http.StatusBadGateway, Kind: "upstream_protocol_error", Message: message}
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
	if err := s.call("host.http.do", payload, &out); err != nil {
		return 0, nil, nil, upstreamTransportError(err)
	}
	if out.Error != "" {
		return 0, nil, nil, upstreamTransportError(errors.New(out.Error))
	}
	raw := out.Body
	if len(raw) > maxBufferedResponse {
		return out.StatusCode, out.Headers, nil, &APIError{Status: http.StatusRequestEntityTooLarge, Kind: "response_limit_exceeded", Message: "upstream response exceeds plugin limit"}
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
	if err := s.call("host.http.do_stream", payload, &out); err != nil {
		return out, upstreamTransportError(err)
	}
	if out.Error != "" {
		s.closeUpstream(out.StreamID)
		return out, upstreamTransportError(errors.New(out.Error))
	}
	if out.StreamID == "" {
		return out, upstreamProtocolError("host returned no upstream stream identifier")
	}
	s.mu.Lock()
	stopped := s.stopped
	if !stopped {
		s.streams[out.StreamID] = struct{}{}
	}
	s.mu.Unlock()
	if stopped {
		s.closeUpstream(out.StreamID)
		return out, fail(http.StatusServiceUnavailable, "OpenDesign plugin is shutting down")
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

// upstreamTransportError wraps a transport failure. The message is already
// redacted by the host error path; never include request headers here.
func upstreamTransportError(err error) error {
	return &APIError{Status: http.StatusBadGateway, Kind: "upstream_transport_error", Message: "upstream request failed: " + transportMessage(err)}
}

func transportMessage(err error) string {
	if err == nil {
		return "unknown transport error"
	}
	return err.Error()
}

// upstreamErrorFromResponse decodes an OpenAI-shaped error body and preserves the
// upstream status. The body is summarized and scrubbed, never echoed verbatim.
func upstreamErrorFromResponse(status int, headers http.Header, body []byte, keys ...string) error {
	if status == 0 {
		status = http.StatusBadGateway
	}
	kind := "upstream_error"
	message := ""
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		if len(envelope.Error) > 0 {
			var detail struct {
				Message string `json:"message"`
				Code    any    `json:"code"`
				Type    string `json:"type"`
			}
			if json.Unmarshal(envelope.Error, &detail) == nil {
				message = strings.TrimSpace(detail.Message)
				switch code := detail.Code.(type) {
				case string:
					if code != "" {
						kind = code
					}
				}
			} else {
				var text string
				if json.Unmarshal(envelope.Error, &text) == nil {
					message = strings.TrimSpace(text)
				}
			}
		}
		if message == "" {
			message = strings.TrimSpace(envelope.Message)
		}
	}
	if message == "" {
		message = strings.TrimSpace(string(truncate(body, 300)))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	// Keep the upstream code visible: operators grep for it, and the host only
	// forwards the message text to the client.
	if kind != "upstream_error" && !strings.Contains(message, kind) {
		message = kind + ": " + message
	}
	return &APIError{Status: status, Kind: kind, Message: redact(message, keys...)}
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
