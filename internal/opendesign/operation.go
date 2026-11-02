package opendesign

import (
	"net/http"
	"sync"
)

// operationTracker maps a plugin-side in-flight request to the host operation id
// returned by host.http.operation_open. Quiesce uses it to cancel work that is
// blocked inside a host callback, which no amount of stream bookkeeping can stop.
//
// Only documented CPA callbacks are used:
//
//	host.http.operation_open {host_callback_id} -> {operation_id}
//	host.http.cancel         {host_callback_id, operation_id}
//
// Both are accepted by internal/pluginhost/host_callbacks.go (v8.0.48).
type operationTracker struct {
	mu      sync.Mutex
	next    int
	pending map[int]pendingOperation
	closed  bool
}

// pendingOperation is one in-flight host call. The callback id is kept because
// host.http.cancel resolves the owning plugin instance from it.
type pendingOperation struct {
	callbackID  string
	operationID string
}

func newOperationTracker() *operationTracker {
	return &operationTracker{pending: map[int]pendingOperation{}}
}

// register reserves a local slot before the host call starts so a concurrent
// quiesce can cancel it even if operation_open has not returned yet.
func (o *operationTracker) register(callbackID string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0
	}
	o.next++
	o.pending[o.next] = pendingOperation{callbackID: callbackID}
	return o.next
}

// bind records the host operation id for a local slot. It reports false when the
// slot was already cancelled, so the caller can abandon the host call.
func (o *operationTracker) bind(local int, operationID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, ok := o.pending[local]
	if !ok {
		return false
	}
	entry.operationID = operationID
	o.pending[local] = entry
	return !o.closed
}

// isClosed reports whether quiesce has already been requested.
func (o *operationTracker) isClosed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed
}

// release forgets a finished operation.
func (o *operationTracker) release(local int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.pending, local)
}

// closeAll marks the tracker closed and returns every host operation id that is
// still outstanding.
func (o *operationTracker) closeAll() []pendingOperation {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	ids := make([]pendingOperation, 0, len(o.pending))
	for _, entry := range o.pending {
		if entry.operationID != "" {
			ids = append(ids, entry)
		}
	}
	o.pending = map[int]pendingOperation{}
	return ids
}

// reopen clears the closed flag so a re-registered instance accepts work again.
func (o *operationTracker) reopen() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = false
	if o.pending == nil {
		o.pending = map[int]pendingOperation{}
	}
}

// doOperation runs one host HTTP callback under a host operation so quiesce can
// cancel it. The host binds the operation to the request's host_callback_id, so
// the same callback id must be used for open, do, and cancel.
func (s *Service) doOperation(callbackID, method string, payload map[string]any, out any) error {
	local := s.ops.register(callbackID)
	if local == 0 {
		return fail(http.StatusServiceUnavailable, "OpenDesign plugin is shutting down")
	}
	defer s.ops.release(local)

	var opened struct {
		OperationID string `json:"operation_id"`
	}
	openPayload := map[string]any{}
	if callbackID != "" {
		openPayload["host_callback_id"] = callbackID
	}
	if err := s.call("host.http.operation_open", openPayload, &opened); err != nil {
		// Older hosts may not expose the operation bridge. The request still runs;
		// it just cannot be cancelled mid-flight. Do not start it at all if
		// quiesce already ran, so shutdown stays bounded.
		if s.ops.isClosed() {
			return fail(http.StatusServiceUnavailable, "OpenDesign plugin is shutting down")
		}
		return s.call(method, payload, out)
	}
	if opened.OperationID != "" {
		if !s.ops.bind(local, opened.OperationID) {
			// Quiesce already ran; do not start new work.
			_ = s.call("host.http.cancel", map[string]any{"host_callback_id": callbackID, "operation_id": opened.OperationID}, nil)
			return fail(http.StatusServiceUnavailable, "OpenDesign plugin is shutting down")
		}
		payload["operation_id"] = opened.OperationID
		// The host finishes the operation itself when the callback returns
		// (callHostHTTPDo defers operation.finish), and for a successful stream it
		// keeps it alive until host.http.stream_close. Cancelling here would abort
		// the stream context that carries the response body, so the tracker only
		// needs to forget the id.
	} else {
		s.ops.release(local)
	}
	return s.call(method, payload, out)
}

// cancelOperations asks the host to abort every operation still in flight.
func (s *Service) cancelOperations() {
	for _, op := range s.ops.closeAll() {
		payload := map[string]any{"operation_id": op.operationID}
		if op.callbackID != "" {
			payload["host_callback_id"] = op.callbackID
		}
		_ = s.call("host.http.cancel", payload, nil)
	}
}
