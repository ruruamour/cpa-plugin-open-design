package opendesign

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// fakeHost is a scripted CPA host. It records every callback so tests assert on
// the real wire contract instead of on internal state.
type fakeHost struct {
	mu    sync.Mutex
	calls []hostCallRecord

	doResponses  []fakeHTTPResponse
	doStreams    []scriptedStream
	streams      map[string]*scriptedStream
	nextStreamID int

	savedAuths  map[string][]byte
	saveAuthErr error

	emitPayloads    [][]byte
	streamCloseErrs []string
	streamCloseRaw  []map[string]any
	authDir         string
}

type hostCallRecord struct {
	Method  string
	Payload any
}

type fakeHTTPResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
	Err     error
}

// scriptedStream is one upstream stream: the chunks host.http.stream_read will
// hand back, in order.
type scriptedStream struct {
	status  int
	headers http.Header
	chunks  [][]byte
	errText string
	closed  bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{savedAuths: map[string][]byte{}, streams: map[string]*scriptedStream{}, authDir: "/tmp/auth"}
}

func (h *fakeHost) call(method string, in, out any) error {
	h.mu.Lock()
	h.calls = append(h.calls, hostCallRecord{Method: method, Payload: in})
	h.mu.Unlock()

	switch method {
	case "host.http.do":
		return h.handleDo(out)
	case "host.http.do_stream":
		return h.handleDoStream(out)
	case "host.http.stream_read":
		return h.handleStreamRead(in, out)
	case "host.http.stream_close":
		h.mu.Lock()
		if id := stringField(in, "stream_id"); id != "" {
			if st, ok := h.streams[id]; ok {
				st.closed = true
			}
		}
		h.mu.Unlock()
		return nil
	case "host.auth.save":
		return h.handleAuthSave(in, out)
	case "host.stream.emit":
		h.mu.Lock()
		h.emitPayloads = append(h.emitPayloads, rawField(in, "payload"))
		h.mu.Unlock()
		return nil
	case "host.stream.close":
		h.mu.Lock()
		if msg, ok := in.(map[string]any); ok {
			h.streamCloseRaw = append(h.streamCloseRaw, msg)
			if text, _ := msg["error"].(string); text != "" {
				h.streamCloseErrs = append(h.streamCloseErrs, text)
			}
		}
		h.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("fakeHost: unsupported method %s", method)
	}
}

func (h *fakeHost) handleDo(out any) error {
	h.mu.Lock()
	if len(h.doResponses) == 0 {
		h.mu.Unlock()
		return errors.New("fakeHost: unexpected host.http.do")
	}
	resp := h.doResponses[0]
	h.doResponses = h.doResponses[1:]
	h.mu.Unlock()
	if resp.Err != nil {
		return resp.Err
	}
	assign(out, map[string]any{"status_code": resp.Status, "headers": resp.Headers, "body": resp.Body})
	return nil
}

func (h *fakeHost) handleDoStream(out any) error {
	h.mu.Lock()
	if len(h.doStreams) == 0 {
		h.mu.Unlock()
		return errors.New("fakeHost: unexpected host.http.do_stream")
	}
	script := h.doStreams[0]
	h.doStreams = h.doStreams[1:]
	h.nextStreamID++
	id := fmt.Sprintf("stream-%d", h.nextStreamID)
	h.streams[id] = &script
	h.mu.Unlock()
	assign(out, map[string]any{"status_code": script.status, "headers": script.headers, "stream_id": id})
	return nil
}

func (h *fakeHost) handleStreamRead(in, out any) error {
	id := stringField(in, "stream_id")
	h.mu.Lock()
	st, ok := h.streams[id]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("fakeHost: unknown stream %s", id)
	}
	if len(st.chunks) == 0 {
		errText := st.errText
		h.mu.Unlock()
		payload := map[string]any{"done": true}
		if errText != "" {
			payload["error"] = errText
		}
		assign(out, payload)
		return nil
	}
	chunk := st.chunks[0]
	st.chunks = st.chunks[1:]
	done := len(st.chunks) == 0
	h.mu.Unlock()
	payload := map[string]any{"payload": chunk, "done": done}
	if done && st.errText != "" {
		payload["error"] = st.errText
	}
	assign(out, payload)
	return nil
}

func (h *fakeHost) handleAuthSave(in, out any) error {
	name := stringField(in, "name")
	raw := rawField(in, "json")
	h.mu.Lock()
	if h.saveAuthErr != nil {
		err := h.saveAuthErr
		h.mu.Unlock()
		return err
	}
	h.savedAuths[name] = raw
	dir := h.authDir
	h.mu.Unlock()
	assign(out, map[string]any{"name": name, "path": dir + "/" + name})
	return nil
}

func (h *fakeHost) methods() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.calls))
	for _, c := range h.calls {
		out = append(out, c.Method)
	}
	return out
}

func (h *fakeHost) countMethod(method string) int {
	n := 0
	for _, m := range h.methods() {
		if m == method {
			n++
		}
	}
	return n
}

// lastPayload returns the most recent payload sent for method.
func (h *fakeHost) lastPayload(method string) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.calls) - 1; i >= 0; i-- {
		if h.calls[i].Method != method {
			continue
		}
		if m, ok := h.calls[i].Payload.(map[string]any); ok {
			return m
		}
	}
	return nil
}

func (h *fakeHost) emitted() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([][]byte, len(h.emitPayloads))
	copy(out, h.emitPayloads)
	return out
}

// waitIdle blocks until the plugin reports no in-flight work. The fake host does
// not own that wait group, so tests call Service.waitIdle directly.
func (h *fakeHost) savedAuth(name string) []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.savedAuths[name]
}

func assign(out any, value map[string]any) {
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, out)
}

func stringField(in any, key string) string {
	m, _ := in.(map[string]any)
	value, _ := m[key].(string)
	return value
}

func rawField(in any, key string) []byte {
	m, _ := in.(map[string]any)
	switch v := m[key].(type) {
	case json.RawMessage:
		return v
	case []byte:
		return v
	case string:
		return []byte(v)
	}
	return nil
}
