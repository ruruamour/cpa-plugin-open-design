package opendesign

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
	// saveHook runs inside host.auth.save before the file on disk is replaced,
	// which is exactly where CPA re-enters auth.parse.
	saveHook func(name string, raw []byte)

	emitPayloads    [][]byte
	streamCloseErrs []string
	streamCloseRaw  []map[string]any
	authDir         string

	// Operation bridge: every do/do_stream call is registered here so a cancel
	// can release a blocked callback the way the real host does.
	ops            map[string]*fakeOperation
	nextOpID       int
	cancelCalls    []string
	blockNextCall  bool
	blockedStarted chan struct{}
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

// fakeOperation mimics the host's HTTP operation: a cancel aborts the pending
// callback exactly like http_operation_bridge does.
type fakeOperation struct {
	done chan struct{}
	once sync.Once
}

func (o *fakeOperation) cancel() {
	o.once.Do(func() { close(o.done) })
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		savedAuths:     map[string][]byte{},
		streams:        map[string]*scriptedStream{},
		authDir:        "/tmp/auth",
		ops:            map[string]*fakeOperation{},
		blockedStarted: make(chan struct{}, 4),
	}
}

// blockCall makes the next host.http call hang until the host cancels the
// operation, which is what the real host does on a client disconnect.
func (h *fakeHost) blockCall() {
	h.mu.Lock()
	h.blockNextCall = true
	h.mu.Unlock()
}

// awaitBlocked waits until a scripted call is actually parked.
func (h *fakeHost) awaitBlocked(t *testing.T) {
	t.Helper()
	select {
	case <-h.blockedStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("host call never blocked")
	}
}

func (h *fakeHost) cancels() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.cancelCalls...)
}

func (h *fakeHost) call(method string, in, out any) error {
	h.mu.Lock()
	h.calls = append(h.calls, hostCallRecord{Method: method, Payload: in})
	h.mu.Unlock()

	switch method {
	case "host.http.operation_open":
		h.mu.Lock()
		h.nextOpID++
		id := fmt.Sprintf("op-%d", h.nextOpID)
		h.ops[id] = &fakeOperation{done: make(chan struct{})}
		h.mu.Unlock()
		assign(out, map[string]any{"operation_id": id})
		return nil
	case "host.http.cancel":
		opID := stringField(in, "operation_id")
		h.mu.Lock()
		h.cancelCalls = append(h.cancelCalls, opID)
		op, ok := h.ops[opID]
		h.mu.Unlock()
		if ok {
			op.cancel()
		}
		return nil
	case "host.http.do":
		return h.handleDo(in, out)
	case "host.http.do_stream":
		return h.handleDoStream(in, out)
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

func (h *fakeHost) handleDo(in any, out any) error {
	h.mu.Lock()
	if len(h.doResponses) == 0 {
		h.mu.Unlock()
		return errors.New("fakeHost: unexpected host.http.do")
	}
	resp := h.doResponses[0]
	h.doResponses = h.doResponses[1:]
	opID := stringField(in, "operation_id")
	op := h.ops[opID]
	h.mu.Unlock()
	if err := h.waitBlocked(op); err != nil {
		return err
	}
	if resp.Err != nil {
		return resp.Err
	}
	// The host marshals sdk/pluginapi.HTTPResponse, which has no JSON tags.
	assign(out, map[string]any{"StatusCode": resp.Status, "Headers": resp.Headers, "Body": resp.Body})
	return nil
}

func (h *fakeHost) handleDoStream(in any, out any) error {
	h.mu.Lock()
	if len(h.doStreams) == 0 {
		h.mu.Unlock()
		return errors.New("fakeHost: unexpected host.http.do_stream")
	}
	script := h.doStreams[0]
	h.doStreams = h.doStreams[1:]
	opID := stringField(in, "operation_id")
	op := h.ops[opID]
	h.mu.Unlock()
	if err := h.waitBlocked(op); err != nil {
		return err
	}
	h.mu.Lock()
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
	hook := h.saveHook
	dir := h.authDir
	h.mu.Unlock()
	if hook != nil {
		hook(name, raw)
	}
	h.mu.Lock()
	h.savedAuths[name] = raw
	h.mu.Unlock()
	assign(out, map[string]any{"name": name, "path": dir + "/" + name})
	return nil
}

// waitBlocked stands in for a host HTTP call that has not returned yet. It
// returns an error once the operation is cancelled.
func (h *fakeHost) waitBlocked(op *fakeOperation) error {
	h.mu.Lock()
	block := h.blockNextCall
	h.blockNextCall = false
	h.mu.Unlock()
	if !block {
		return nil
	}
	if op == nil {
		return errors.New("fakeHost: blocked call without an operation")
	}
	select {
	case <-op.done:
		return errors.New("host http operation was cancelled")
	default:
	}
	select {
	case h.blockedStarted <- struct{}{}:
	default:
	}
	<-op.done
	return errors.New("host http operation was cancelled")
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

// writeAuthFile puts raw content at <authDir>/<name>, standing in for the file a
// previous save left on disk.
func (h *fakeHost) writeAuthFile(t *testing.T, name string, raw []byte) string {
	t.Helper()
	dir := h.authDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return path
}
