package opendesign

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// quotaInFlightFixture installs an observation hook on one Service instance. It
// is never global, so parallel or later tests are unaffected.
func quotaInFlightFixture(t *testing.T, s *Service, hook quotaInFlightTestHook) {
	t.Helper()
	s.quotaInFlightTestHook.Store(&hook)
	t.Cleanup(func() { s.quotaInFlightTestHook.Store(nil) })
}

// quotaInFlightState reports the fence for one credential snapshot: whether that
// exact key has a request running, its own count and the total across every key.
func quotaInFlightState(s *Service, c Credential) (bool, int, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.quotaHasInFlightLocked(c), s.quotaInFlightCountLocked(c), s.quotaInFlightTotalLocked()
}

func quotaInFlightTotal(s *Service) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.quotaInFlightTotalLocked()
}

// quotaHasInFlightOf looks the live credential up first, so a test does not have
// to carry a snapshot of its own.
func quotaHasInFlightOf(t *testing.T, s *Service, id string) bool {
	t.Helper()
	c, ok := liveCredential(s, id)
	if !ok {
		t.Fatalf("credential %s not found", id)
	}
	has, _, _ := quotaInFlightState(s, c)
	return has
}

func quotaErrorKind(t *testing.T, err error) (string, int) {
	t.Helper()
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError: %v", err, err)
	}
	return apiErr.Kind, apiErr.Status
}

func bufferedQuotaRequest(id string) executorPayload {
	return executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
}

func streamQuotaRequest(id, streamID string) executorPayload {
	return executorPayload{AuthID: id, Model: "od-large", StreamID: streamID, Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
}

func bufferOK() fakeHTTPResponse {
	return fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)}
}

// gatedQuotaHost parks every host.http.do call on its own gate, so a test can
// hold several same-account requests in flight at once without touching the
// shared fake host. The wrapper is local to this file.
type gatedQuotaHost struct {
	*fakeHost
	mu    sync.Mutex
	gates []chan struct{}
}

func newGatedQuotaHost(host *fakeHost) *gatedQuotaHost {
	return &gatedQuotaHost{fakeHost: host}
}

func (h *gatedQuotaHost) call(method string, in, out any) error {
	if method == "host.http.do" {
		gate := make(chan struct{})
		h.mu.Lock()
		h.gates = append(h.gates, gate)
		h.mu.Unlock()
		<-gate
	}
	return h.fakeHost.call(method, in, out)
}

func (h *gatedQuotaHost) waitGates(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := len(h.gates)
		h.mu.Unlock()
		if n >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only %d of %d requests reached the upstream", func() int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.gates)
	}(), want)
}

func (h *gatedQuotaHost) releaseGate(index int) {
	h.mu.Lock()
	gate := h.gates[index]
	h.mu.Unlock()
	close(gate)
}

// gatedReadHost parks the first host.http.stream_read of each upstream stream on
// its own gate, so a test can open several streams and then finish them in a
// deterministic order instead of racing the pump goroutines.
type gatedReadHost struct {
	*fakeHost
	mu      sync.Mutex
	gates   map[string]chan struct{}
	order   []string
	arrived chan string
}

func newGatedReadHost(host *fakeHost) *gatedReadHost {
	return &gatedReadHost{fakeHost: host, gates: map[string]chan struct{}{}, arrived: make(chan string, 8)}
}

func (h *gatedReadHost) call(method string, in, out any) error {
	if method == "host.http.stream_read" {
		id := stringField(in, "stream_id")
		h.mu.Lock()
		gate, seen := h.gates[id]
		if !seen {
			gate = make(chan struct{})
			h.gates[id] = gate
			h.order = append(h.order, id)
		}
		h.mu.Unlock()
		if !seen {
			h.arrived <- id
			<-gate
		}
	}
	return h.fakeHost.call(method, in, out)
}

// awaitStream waits until one more upstream stream is parked at its first read.
func (h *gatedReadHost) awaitStream(t *testing.T) string {
	t.Helper()
	select {
	case id := <-h.arrived:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("no stream reached its upstream read")
		return ""
	}
}

// releaseStream lets exactly one parked stream continue, so the test controls
// which terminal callback runs first.
func (h *gatedReadHost) releaseStream(t *testing.T, id string) {
	t.Helper()
	h.mu.Lock()
	gate, ok := h.gates[id]
	h.mu.Unlock()
	if !ok {
		t.Fatalf("stream %s was never parked", id)
	}
	close(gate)
}

// The counter must drop only after the quota observation ran: at release time the
// 402 has already published the block.
func TestQuotaInFlightObservesQuotaBlockBeforeRelease(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := seedModels(t, s, host)
	quotaFixtureAuthFile(t, s, host, id, nil)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusPaymentRequired, Body: []byte(quotaPauseFixtureBody)})

	var blockedAtRelease atomic.Bool
	var countedAtRelease atomic.Int64
	var once sync.Once
	quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeRelease: func(key quotaInFlightKey) {
		once.Do(func() {
			c, ok := liveCredential(s, id)
			blockedAtRelease.Store(ok && c.quotaBlocked())
			s.mu.RLock()
			countedAtRelease.Store(int64(s.quotaInflight[key]))
			s.mu.RUnlock()
		})
	}})

	if _, err := execute(t, s, bufferedQuotaRequest(id)); err == nil {
		t.Fatal("402 was returned as a success")
	}
	if !blockedAtRelease.Load() {
		t.Fatal("in-flight counter dropped before the quota block was observed")
	}
	if countedAtRelease.Load() != 1 {
		t.Fatalf("release saw %d counters, want the request itself", countedAtRelease.Load())
	}
	if total := quotaInFlightTotal(s); total != 0 {
		t.Fatalf("counters leaked: total=%d", total)
	}
}

// Every streaming exit path releases exactly once: an SSE terminal quota error,
// a non-2xx stream open, a missing output stream id and a cancelled blocked open.
func TestQuotaInFlightReleasesOnStreamPaths(t *testing.T) {
	t.Run("SSE terminal quota error", func(t *testing.T) {
		s, host := newQuotaPauseTestService(t)
		id := seedModels(t, s, host)
		quotaFixtureAuthFile(t, s, host, id, nil)
		host.doStreams = append(host.doStreams, scriptedStream{status: http.StatusOK, chunks: [][]byte{
			[]byte("data: " + quotaPauseFixtureBody + "\n\n"),
		}})
		var blockedAtRelease atomic.Bool
		quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeRelease: func(quotaInFlightKey) {
			c, ok := liveCredential(s, id)
			blockedAtRelease.Store(ok && c.quotaBlocked())
		}})
		if _, err := executeStream(t, s, streamQuotaRequest(id, "out-sse-quota")); err != nil {
			t.Fatalf("execute_stream: %v", err)
		}
		s.waitIdle()
		if !blockedAtRelease.Load() {
			t.Fatal("SSE terminal error released its counter before the block was observed")
		}
		if total := quotaInFlightTotal(s); total != 0 {
			t.Fatalf("counters leaked: total=%d", total)
		}
	})

	t.Run("stream HTTP 402", func(t *testing.T) {
		s, host := newQuotaPauseTestService(t)
		id := seedModels(t, s, host)
		quotaFixtureAuthFile(t, s, host, id, nil)
		host.doStreams = append(host.doStreams, scriptedStream{status: http.StatusPaymentRequired, chunks: [][]byte{[]byte(quotaPauseFixtureBody)}})
		if _, err := executeStream(t, s, streamQuotaRequest(id, "out-402")); err == nil {
			t.Fatal("HTTP 402 stream was accepted")
		}
		s.waitIdle()
		if total := quotaInFlightTotal(s); total != 0 {
			t.Fatalf("counters leaked: total=%d", total)
		}
	})

	t.Run("early return without stream id", func(t *testing.T) {
		s, host := newQuotaPauseTestService(t)
		id := seedModels(t, s, host)
		quotaFixtureAuthFile(t, s, host, id, nil)
		if _, err := executeStream(t, s, streamQuotaRequest(id, "")); err == nil {
			t.Fatal("stream without an output id was accepted")
		}
		if host.countMethod("host.http.do_stream") != 0 {
			t.Fatal("early return still opened an upstream stream")
		}
		if total := quotaInFlightTotal(s); total != 0 {
			t.Fatalf("counters leaked: total=%d", total)
		}
	})

	t.Run("cancelled blocked open", func(t *testing.T) {
		s, host := newQuotaPauseTestService(t)
		id := seedModels(t, s, host)
		quotaFixtureAuthFile(t, s, host, id, nil)
		host.blockCall()
		host.doStreams = append(host.doStreams, scriptedStream{status: http.StatusOK, chunks: [][]byte{[]byte("data: [DONE]\n\n")}})
		done := make(chan error, 1)
		go func() {
			_, err := executeStream(t, s, streamQuotaRequest(id, "out-cancel"))
			done <- err
		}()
		host.awaitBlocked(t)
		select {
		case err := <-done:
			t.Fatalf("blocked stream returned early: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		if !quotaHasInFlightOf(t, s, id) {
			t.Fatal("blocked stream was not counted as in flight")
		}
		s.cancelOperations()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cancelled stream reported success")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled stream never returned")
		}
		assertIdle(t, s)
		if total := quotaInFlightTotal(s); total != 0 {
			t.Fatalf("counters leaked: total=%d", total)
		}
	})
}

// Replacing the key must not let an old request block, or release, the counter of
// the new key.
func TestQuotaInFlightKeyReplacementIsolation(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := seedModels(t, s, host)
	quotaFixtureAuthFile(t, s, host, id, nil)
	old, _ := liveCredential(s, id)
	gated := newGatedQuotaHost(host)
	s.SetHost(gated.call)
	host.doResponses = append(host.doResponses, bufferOK(), bufferOK())

	oldDone := make(chan error, 1)
	go func() {
		_, err := execute(t, s, bufferedQuotaRequest(id))
		oldDone <- err
	}()
	gated.waitGates(t, 1)

	// Replace the inference key while the old request is still running.
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
		"id": id, "api_key": "sk-amr-exec-replaced-0002",
	}); status != http.StatusOK {
		t.Fatalf("key replacement: %d %s", status, raw)
	}
	current, _ := liveCredential(s, id)
	if current.APIKey == old.APIKey {
		t.Fatal("key was not replaced")
	}
	has, count, total := quotaInFlightState(s, current)
	if has || count != 0 {
		t.Fatalf("old request blocked the new key: has=%t count=%d", has, count)
	}
	if total != 1 {
		t.Fatalf("old request lost its counter: total=%d", total)
	}

	// A new-key request is admitted independently.
	newDone := make(chan error, 1)
	go func() {
		_, err := execute(t, s, bufferedQuotaRequest(id))
		newDone <- err
	}()
	gated.waitGates(t, 2)
	has, count, total = quotaInFlightState(s, current)
	if !has || count != 1 || total != 2 {
		t.Fatalf("new key tracking = has:%t count:%d total:%d, want 1 of 2", has, count, total)
	}

	// Releasing the old request must not decrement the new key's counter.
	gated.releaseGate(0)
	select {
	case err := <-oldDone:
		if err != nil {
			t.Fatalf("old request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old request never returned")
	}
	has, count, total = quotaInFlightState(s, current)
	if !has || count != 1 || total != 1 {
		t.Fatalf("old release touched the new key: has:%t count:%d total:%d", has, count, total)
	}

	gated.releaseGate(1)
	select {
	case err := <-newDone:
		if err != nil {
			t.Fatalf("new request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("new request never returned")
	}
	if total := quotaInFlightTotal(s); total != 0 {
		t.Fatalf("counters leaked: total=%d", total)
	}
}

// A manual enable/disable bumps the runtime epoch but keeps the same key, so the
// account must still wait for that key's in-flight request.
func TestQuotaInFlightEpochChangeKeepsSameFence(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := seedModels(t, s, host)
	quotaFixtureAuthFile(t, s, host, id, nil)
	host.blockCall()
	host.doResponses = append(host.doResponses, bufferOK())
	done := make(chan error, 1)
	go func() {
		_, err := execute(t, s, bufferedQuotaRequest(id))
		done <- err
	}()
	host.awaitBlocked(t)

	c, _ := liveCredential(s, id)
	before := c.runtimeEpoch
	for _, disabled := range []bool{true, false} {
		if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
			"id": id, "disabled": disabled,
		}); status != http.StatusOK {
			t.Fatalf("manual toggle: %d %s", status, raw)
		}
	}
	toggled, _ := liveCredential(s, id)
	if toggled.runtimeEpoch == before {
		t.Fatal("manual toggles did not change the runtime epoch")
	}
	has, count, _ := quotaInFlightState(s, toggled)
	if !has || count != 1 {
		t.Fatalf("epoch change dropped the same-key fence: has=%t count=%d", has, count)
	}

	s.cancelOperations()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request never returned")
	}
	if _, _, total := quotaInFlightState(s, toggled); total != 0 {
		t.Fatalf("counters leaked: total=%d", total)
	}
}

// Without a live record (host restart passing only StorageJSON) inference keeps
// working and leaves no counter behind; a disabled or paused snapshot stays
// rejected.
func TestQuotaInFlightWithoutLiveCredential(t *testing.T) {
	const key = "sk-amr-storage-only-0001"
	cases := []struct {
		name    string
		build   func() Credential
		wantErr bool
	}{
		{
			name:  "enabled snapshot still runs",
			build: func() Credential { return Credential{Type: Provider, ID: "open-design-storage-only", APIKey: key} },
		},
		{
			name: "disabled snapshot",
			build: func() Credential {
				return Credential{Type: Provider, ID: "open-design-storage-only", APIKey: key, Disabled: true}
			},
			wantErr: true,
		},
		{
			name: "blocked snapshot",
			build: func() Credential {
				manual := false
				now := time.Now()
				return Credential{
					Type: Provider, ID: "open-design-storage-only", APIKey: key,
					QuotaBlock: &QuotaBlock{
						KeyHash: quotaPauseKeyHash(key), ManualDisabled: &manual,
						Source: "upstream", Reason: "quota_exhausted",
						BlockedAt: now.Add(-time.Minute), RetryAt: now.Add(time.Hour),
					},
				}
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, host := newQuotaPauseTestService(t)
			live := seedModels(t, s, host)
			// Drop the live record to imitate a plugin that has not parsed the auth
			// file yet while the host still hands the snapshot back.
			s.mu.Lock()
			delete(s.creds, live)
			s.mu.Unlock()
			snapshot := tc.build()
			host.doResponses = append(host.doResponses, bufferOK())
			req := executorPayload{
				AuthID:      snapshot.ID,
				Model:       "od-large",
				Payload:     []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
				StorageJSON: jsonBytes(snapshot),
			}
			_, err := execute(t, s, req)
			if tc.wantErr {
				if err == nil {
					t.Fatal("unsafe snapshot was accepted")
				}
				if host.countMethod("host.http.do") != 0 {
					t.Fatal("rejected snapshot reached the upstream")
				}
			} else if err != nil {
				t.Fatalf("enabled snapshot failed: %v", err)
			}
			if total := quotaInFlightTotal(s); total != 0 {
				t.Fatalf("snapshot request leaked %d counters", total)
			}
		})
	}
}

// The explicit model test uses a pinned credential snapshot; it must be counted
// while it runs and released afterwards.
func TestQuotaInFlightModelTestIsCountedAndReleased(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "primary", "sk-model-test-inflight")
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "od-large", "upstream_id": "opendesign-large"}},
	}); status != http.StatusOK {
		t.Fatalf("seed mapping: %d %s", status, raw)
	}
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[],"usage":{"total_tokens":1}}`)})
	var counted atomic.Int64
	quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeRelease: func(key quotaInFlightKey) {
		s.mu.RLock()
		counted.Store(int64(s.quotaInflight[key]))
		s.mu.RUnlock()
	}})
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
		"credential_id": id, "model_id": "od-large",
	})
	if status != http.StatusOK {
		t.Fatalf("model test: %d %s", status, raw)
	}
	if counted.Load() != 1 {
		t.Fatalf("model test was counted %d times during its run, want 1", counted.Load())
	}
	if total := quotaInFlightTotal(s); total != 0 {
		t.Fatalf("model test leaked a counter: total=%d", total)
	}
}

// Several streaming requests can be in flight at once, and the key's counter only
// reaches zero after every terminal callback recorded its error — including the
// ones that observed the quota block. Each terminal callback is released one at a
// time, so the assertion does not depend on goroutine scheduling.
func TestQuotaInFlightStreamsReleaseAfterEveryTerminalCallback(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := seedModels(t, s, host)
	quotaFixtureAuthFile(t, s, host, id, nil)
	for i := 0; i < 2; i++ {
		host.doStreams = append(host.doStreams, scriptedStream{status: http.StatusOK, chunks: [][]byte{
			[]byte("data: " + quotaPauseFixtureBody + "\n\n"),
		}})
	}
	gated := newGatedReadHost(host)
	s.SetHost(gated.call)

	type observation struct {
		count   int
		blocked bool
	}
	observed := make(chan observation, 2)
	quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeRelease: func(key quotaInFlightKey) {
		c, ok := liveCredential(s, id)
		s.mu.RLock()
		count := s.quotaInflight[key]
		s.mu.RUnlock()
		observed <- observation{count: count, blocked: ok && c.quotaBlocked()}
	}})

	if _, err := executeStream(t, s, streamQuotaRequest(id, "out-multi-a")); err != nil {
		t.Fatalf("first stream: %v", err)
	}
	first := gated.awaitStream(t)
	if _, err := executeStream(t, s, streamQuotaRequest(id, "out-multi-b")); err != nil {
		t.Fatalf("second stream: %v", err)
	}
	second := gated.awaitStream(t)

	c, _ := liveCredential(s, id)
	if has, count, total := quotaInFlightState(s, c); !has || count != 2 || total != 2 {
		t.Fatalf("two streams in flight = has:%t count:%d total:%d", has, count, total)
	}

	// Finish the first stream: at its release both counters are still present.
	gated.releaseStream(t, first)
	got := <-observed
	if got.count != 2 || !got.blocked {
		t.Fatalf("first release = count:%d blocked:%t, want count 2 with the block observed", got.count, got.blocked)
	}
	if has, count, _ := quotaInFlightState(s, c); !has || count != 1 {
		t.Fatalf("after the first stream = has:%t count:%d, want the second still fenced", has, count)
	}

	// Finish the second stream: its release sees only its own counter.
	gated.releaseStream(t, second)
	got = <-observed
	if got.count != 1 || !got.blocked {
		t.Fatalf("second release = count:%d blocked:%t, want count 1 with the block observed", got.count, got.blocked)
	}
	s.waitIdle()
	if _, _, total := quotaInFlightState(s, c); total != 0 {
		t.Fatalf("counters leaked: total=%d", total)
	}
}

// The wake is narrow: a normal request never wakes the sweep (no full-table
// scan, no failed-persistence retries), while the last release for a key that a
// currently blocked record carries does wake it. The wake is empty-channel
// buffered and is consumed without any lock the release held.
func TestQuotaInFlightReleaseWakeIsScopedToBlockedKey(t *testing.T) {
	s := NewService()
	c := Credential{Type: Provider, ID: "fixture-inflight", Label: "fixture", APIKey: "fixture-inflight-key"}
	// This test is about the recovery wake, and admits several requests for one
	// key at the same instant. That is exactly what the concurrency protection
	// refuses, so it is switched off here; concurrency_gate_test.go covers it.
	off := false
	s.mu.Lock()
	s.cfg.Concurrency = &ConcurrencyConfig{Enabled: &off}
	s.storeCredentialLocked(c)
	s.mu.Unlock()
	live, _ := liveCredential(s, c.ID)

	first, err := s.admitQuotaInFlight(live, "")
	if err != nil {
		t.Fatalf("admit first: %v", err)
	}
	second, err := s.admitQuotaInFlight(live, "")
	if err != nil {
		t.Fatalf("admit second: %v", err)
	}
	if _, count, _ := quotaInFlightState(s, live); count != 2 {
		t.Fatalf("same key was not counted twice: %d", count)
	}

	// The first release does not reach zero.
	first.release(s)
	if has, count, _ := quotaInFlightState(s, live); !has || count != 1 {
		t.Fatalf("partial release = has:%t count:%d", has, count)
	}
	if len(s.quotaPauseWake) != 0 {
		t.Fatal("a non-final release woke the quota-pause worker")
	}

	// The last release reaches zero on a healthy (not blocked) account, so no
	// sweep is scheduled. release stays idempotent.
	second.release(s)
	second.release(s)
	if _, _, total := quotaInFlightState(s, live); total != 0 {
		t.Fatalf("counters leaked: total=%d", total)
	}
	if len(s.quotaPauseWake) != 0 {
		t.Fatal("a normal request woke the quota-pause worker")
	}

	// A blocked record carrying a different key must not wake either.
	manual := false
	blockedSnapshot := func(id, key string) Credential {
		return Credential{
			Type: Provider, ID: id, Label: id, APIKey: key,
			QuotaBlock: &QuotaBlock{
				KeyHash: quotaPauseKeyHash(key), ManualDisabled: &manual,
				Source: "upstream", Reason: "quota_exhausted",
				BlockedAt: time.Now().Add(-time.Minute), RetryAt: time.Now().Add(time.Hour),
			},
			QuotaRevision: "fixture-revision",
		}
	}
	s.mu.Lock()
	other := blockedSnapshot("fixture-other", "fixture-other-inference-key")
	s.storeCredentialLocked(other)
	s.mu.Unlock()
	unrelated, err := s.admitQuotaInFlight(live, "")
	if err != nil {
		t.Fatalf("admit unrelated: %v", err)
	}
	unrelated.release(s)
	if len(s.quotaPauseWake) != 0 {
		t.Fatal("a released key woke the sweep for a different blocked key")
	}

	// The last release for the blocked key does wake it exactly once.
	third, err := s.admitQuotaInFlight(live, "")
	if err != nil {
		t.Fatalf("admit third: %v", err)
	}
	s.mu.Lock()
	s.storeCredentialLocked(blockedSnapshot(c.ID, c.APIKey))
	s.mu.Unlock()
	third.release(s)
	if len(s.quotaPauseWake) != 1 {
		t.Fatal("the last release for a blocked key did not wake the quota-pause worker")
	}
	select {
	case <-s.quotaPauseWake:
	default:
		t.Fatal("wake signal was not readable")
	}
}

// selectedCredential has always allowed a StorageJSON snapshot that carries an
// inference key but no id, with no live record. The admission fence must keep
// that fallback for both the buffered and the streaming path, must still count
// the request under its key, and must reject a snapshot that is paused or
// disabled on its own.
func TestQuotaInFlightEmptyIDStorageOnlyFallback(t *testing.T) {
	now := time.Now()
	blockedSnapshot := func() Credential {
		manual := false
		return Credential{
			Type: Provider, APIKey: "sk-amr-emptytoken-0001",
			QuotaBlock: &QuotaBlock{
				KeyHash: quotaPauseKeyHash("sk-amr-emptytoken-0001"), ManualDisabled: &manual,
				Source: "upstream", Reason: "quota_exhausted",
				BlockedAt: now.Add(-time.Minute), RetryAt: now.Add(time.Hour),
			},
		}
	}
	cases := []struct {
		name     string
		snapshot func() Credential
		stream   bool
		wantErr  bool
	}{
		{
			name:     "buffered empty id runs",
			snapshot: func() Credential { return Credential{Type: Provider, APIKey: "sk-amr-emptytoken-0001"} },
		},
		{
			name:     "stream empty id runs",
			snapshot: func() Credential { return Credential{Type: Provider, APIKey: "sk-amr-emptytoken-0001"} },
			stream:   true,
		},
		{
			name: "buffered empty id disabled snapshot",
			snapshot: func() Credential {
				return Credential{Type: Provider, APIKey: "sk-amr-emptytoken-0001", Disabled: true}
			},
			wantErr: true,
		},
		{
			name:     "buffered empty id blocked snapshot",
			snapshot: blockedSnapshot,
			wantErr:  true,
		},
		{
			name:     "stream empty id blocked snapshot",
			snapshot: blockedSnapshot,
			stream:   true,
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, host := newQuotaPauseTestService(t)
			live := seedModels(t, s, host)
			// The id-less request must not resolve to the seeded live credential.
			s.mu.Lock()
			delete(s.creds, live)
			s.mu.Unlock()
			snapshot := tc.snapshot()
			var seen atomic.Int64
			var seenKey atomic.Value
			quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeRelease: func(key quotaInFlightKey) {
				s.mu.RLock()
				seen.Store(int64(s.quotaInflight[key]))
				s.mu.RUnlock()
				seenKey.Store(key)
			}})

			host.doResponses = append(host.doResponses, bufferOK())
			host.doStreams = append(host.doStreams, scriptedStream{status: http.StatusOK, chunks: [][]byte{
				[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"),
				[]byte("data: [DONE]\n\n"),
			}})

			req := executorPayload{
				Model:       "od-large",
				Payload:     []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
				StorageJSON: jsonBytes(snapshot),
			}
			if tc.stream {
				req.StreamID = "out-empty-id"
				_, err := executeStream(t, s, req)
				if tc.wantErr {
					if err == nil {
						t.Fatal("unsafe id-less stream snapshot was accepted")
					}
					if host.countMethod("host.http.do_stream") != 0 {
						t.Fatal("rejected id-less stream reached the upstream")
					}
					return
				}
				if err != nil {
					t.Fatalf("id-less stream failed: %v", err)
				}
				s.waitIdle()
			} else {
				if _, err := execute(t, s, req); tc.wantErr {
					if err == nil {
						t.Fatal("unsafe id-less snapshot was accepted")
					}
					if host.countMethod("host.http.do") != 0 {
						t.Fatal("rejected id-less snapshot reached the upstream")
					}
					return
				} else if err != nil {
					t.Fatalf("id-less buffered request failed: %v", err)
				}
			}

			if seen.Load() != 1 {
				t.Fatalf("id-less request was counted %d times during its run, want 1", seen.Load())
			}
			key, _ := seenKey.Load().(quotaInFlightKey)
			if key.credentialID != "" || key.keyHash != quotaPauseKeyHash(snapshot.APIKey) {
				t.Fatalf("id-less request was tracked as %+v, want an empty id under its own key", key)
			}
			if total := quotaInFlightTotal(s); total != 0 {
				t.Fatalf("id-less request leaked %d counters", total)
			}
		})
	}

	// The admission fence itself also guards the id-less form: a request that
	// reaches it with a paused or disabled snapshot must fail there too, so the
	// fallback cannot be used as a bypass.
	t.Run("empty id admission guards", func(t *testing.T) {
		s := NewService()
		enabled := Credential{Type: Provider, APIKey: "sk-amr-emptytoken-0002"}
		lease, err := s.admitQuotaInFlight(enabled, "")
		if err != nil {
			t.Fatalf("enabled id-less snapshot rejected: %v", err)
		}
		lease.release(s)
		for _, tc := range []struct {
			name     string
			snapshot Credential
			wantCode string
		}{
			{"disabled", Credential{Type: Provider, APIKey: enabled.APIKey, Disabled: true}, "credential_unavailable"},
			{"blocked", blockedSnapshot(), "quota_blocked"},
			{"no key", Credential{Type: Provider}, "credential_unavailable"},
		} {
			_, err := s.admitQuotaInFlight(tc.snapshot, "")
			if err == nil {
				t.Fatalf("%s id-less snapshot was admitted", tc.name)
			}
			if code, _ := quotaErrorKind(t, err); code != tc.wantCode {
				t.Fatalf("%s id-less snapshot error = %s, want %s", tc.name, code, tc.wantCode)
			}
		}
		if total := quotaInFlightTotal(s); total != 0 {
			t.Fatalf("guarded id-less admission left %d counters", total)
		}
	})
}

// Several records can carry the same inference key. The recovery fence is keyed
// on that key, so a request admitted through one record keeps every other record
// — including the empty id of a StorageJSON-only request — from being treated as
// idle. Inference concurrency itself is never limited.
func TestQuotaInFlightMergesSameKeyAcrossRecords(t *testing.T) {
	const key = "sk-amr-shared-inference-key"
	s, host := newQuotaPauseTestService(t)
	first := addCredential(t, s, host, "first", key)
	other := addCredential(t, s, host, "other", "sk-amr-other-inference-key")
	seedModels(t, s, host)
	quotaFixtureAuthFile(t, s, host, first, nil)

	// A second live record reusing the same inference key, which is what a host
	// restart or a hand-edited auth file can produce.
	s.mu.Lock()
	shared := s.creds[first]
	shared.ID = other
	shared.Label = "second"
	shared.QuotaBlock = nil
	shared.QuotaRevision = newID()
	s.storeCredentialLocked(shared)
	s.mu.Unlock()
	sharedLive, ok := liveCredential(s, other)
	if !ok || sharedLive.APIKey != key {
		t.Fatal("shared-key record was not published")
	}
	emptyID := Credential{APIKey: key}

	gated := newGatedQuotaHost(host)
	s.SetHost(gated.call)
	host.doResponses = append(host.doResponses, bufferOK(), bufferOK())

	// Two requests arrive through two different records with the same key; both
	// must reach the upstream at the same time.
	done := make(chan error, 2)
	for _, authID := range []string{first, other} {
		go func(authID string) {
			_, err := execute(t, s, bufferedQuotaRequest(authID))
			done <- err
		}(authID)
	}
	gated.waitGates(t, 2)

	if has, count, total := quotaInFlightState(s, emptyID); !has || count != 2 || total != 2 {
		t.Fatalf("cross-record fence = has:%t count:%d total:%d, want both merged", has, count, total)
	}
	// A record carrying a different key is unaffected.
	if has, count, _ := quotaInFlightState(s, Credential{ID: other, APIKey: "sk-amr-other-inference-key"}); has || count != 0 {
		t.Fatalf("unrelated key was merged: has=%t count=%d", has, count)
	}

	// Releasing one record's request keeps the key fenced for the other.
	gated.releaseGate(0)
	if err := <-done; err != nil {
		t.Fatalf("first request failed: %v", err)
	}
	if has, count, total := quotaInFlightState(s, emptyID); !has || count != 1 || total != 1 {
		t.Fatalf("partial cross-record release = has:%t count:%d total:%d", has, count, total)
	}

	gated.releaseGate(1)
	if err := <-done; err != nil {
		t.Fatalf("second request failed: %v", err)
	}
	if has, count, total := quotaInFlightState(s, emptyID); has || count != 0 || total != 0 {
		t.Fatalf("fence did not clear after both records finished: has:%t count:%d total:%d", has, count, total)
	}
	if got := host.countMethod("host.http.do"); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (no concurrency cap)", got)
	}
}

// A StorageJSON-only request can have a live record published for the same key
// and id before admission runs. That is a first publication, not an identity
// round trip, so it must be admitted (and counted) instead of failing with 409.
func TestQuotaInFlightStorageOnlySnapshotAdmittedAfterFirstPublication(t *testing.T) {
	const key = "sk-amr-latepublication-0001"
	s, host := newQuotaPauseTestService(t)
	seedModels(t, s, host)
	host.doResponses = append(host.doResponses, bufferOK())
	snapshot := Credential{Type: Provider, ID: "open-design-late", APIKey: key}

	var published atomic.Bool
	quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeAdmit: func() {
		if published.Swap(true) {
			return
		}
		// The host watcher (or auth.parse) publishes the record right here.
		s.mu.Lock()
		s.storeCredentialLocked(snapshot)
		s.mu.Unlock()
	}})

	lease, err := s.admitQuotaInFlight(snapshot, snapshot.ID)
	if err != nil {
		t.Fatalf("first publication was treated as a stale snapshot: %v", err)
	}
	if lease.admittedEpoch == 0 {
		t.Fatal("admission did not record the published generation")
	}
	if total := quotaInFlightTotal(s); total != 1 {
		t.Fatalf("admitted request was not counted: total=%d", total)
	}
	lease.release(s)
	if total := quotaInFlightTotal(s); total != 0 {
		t.Fatalf("counters leaked: total=%d", total)
	}

	// The same window with a blocked record must still be rejected: the widened
	// epoch rule must not weaken the pause check.
	s.mu.Lock()
	manual := false
	blocked := s.creds[snapshot.ID]
	blocked.QuotaBlock = &QuotaBlock{
		KeyHash: quotaPauseKeyHash(key), ManualDisabled: &manual,
		Source: "upstream", Reason: "quota_exhausted",
		BlockedAt: time.Now().Add(-time.Minute), RetryAt: time.Now().Add(time.Hour),
	}
	blocked.QuotaRevision = "fixture-revision"
	s.storeCredentialLocked(blocked)
	s.mu.Unlock()
	if _, err := s.admitQuotaInFlight(snapshot, snapshot.ID); err == nil {
		t.Fatal("a record blocked during the window was admitted")
	}
}

// A request admitted with no live record and then published must still observe
// its own 402: the epoch fence is widened exactly for that first publication.
func TestQuotaInFlightObservesBlockAfterFirstPublication(t *testing.T) {
	const key = "sk-amr-latepublication-0002"
	s, host := newQuotaPauseTestService(t)
	seedModels(t, s, host)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusPaymentRequired, Body: []byte(quotaPauseFixtureBody)})
	snapshot := Credential{Type: Provider, ID: "open-design-late402", APIKey: key}

	var once sync.Once
	quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeObserve: func() {
		once.Do(func() {
			// The record is published while the request is still running, so the
			// 402 arrives afterwards under a fresh runtime epoch.
			s.mu.Lock()
			s.storeCredentialLocked(snapshot)
			s.mu.Unlock()
		})
	}})

	_, err := execute(t, s, executorPayload{
		AuthID:      snapshot.ID,
		Model:       "od-large",
		Payload:     []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		StorageJSON: jsonBytes(snapshot),
	})
	if err == nil {
		t.Fatal("402 was returned as a success")
	}
	c, ok := liveCredential(s, snapshot.ID)
	if !ok || !c.quotaBlocked() {
		t.Fatal("a real 402 was dropped because the record was published after admission")
	}
	if total := quotaInFlightTotal(s); total != 0 {
		t.Fatalf("counters leaked: total=%d", total)
	}
}

// The widened epoch rule must not undo an explicit resume: once the record
// carries a quota revision (the resume stamps one), a late error from a request
// admitted before it is dropped instead of re-pausing the account.
func TestQuotaInFlightLateErrorCannotUndoExplicitResume(t *testing.T) {
	const key = "sk-amr-latepublication-0003"
	for _, tc := range []struct {
		name string
		// afterPublish models what really happens after the record is published:
		// every one of these paths bumps the credential generation.
		afterPublish func(s *Service, c Credential) Credential
	}{
		{
			name: "explicit resume before the late error",
			afterPublish: func(s *Service, c Credential) Credential {
				// resumeQuotaBlockLocked's in-memory effect: new revision plus a
				// generation bump, so the block is proven cleared.
				c.QuotaRevision = "fixture-resume-revision"
				s.credentialEpoch[c.ID]++
				c.runtimeEpoch = s.credentialEpoch[c.ID]
				return c
			},
		},
		{
			name: "identity replaced before the late error",
			afterPublish: func(s *Service, c Credential) Credential {
				c.APIKey = "sk-amr-latepublication-replaced"
				return c
			},
		},
		{
			name: "manual toggles before the late error",
			afterPublish: func(s *Service, c Credential) Credential {
				c.Disabled = true
				return c
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, host := newQuotaPauseTestService(t)
			seedModels(t, s, host)
			host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusPaymentRequired, Body: []byte(quotaPauseFixtureBody)})
			snapshot := Credential{Type: Provider, ID: "open-design-late402b", APIKey: key}
			var once sync.Once
			quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeObserve: func() {
				once.Do(func() {
					s.mu.Lock()
					s.storeCredentialLocked(snapshot)
					s.storeCredentialLocked(tc.afterPublish(s, s.creds[snapshot.ID]))
					s.mu.Unlock()
				})
			}})

			if _, err := execute(t, s, executorPayload{
				AuthID:      snapshot.ID,
				Model:       "od-large",
				Payload:     []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
				StorageJSON: jsonBytes(snapshot),
			}); err == nil {
				t.Fatal("402 was returned as a success")
			}
			c, ok := liveCredential(s, snapshot.ID)
			if !ok || c.quotaBlocked() {
				t.Fatal("a late error re-paused an account that had already moved on")
			}
		})
	}
}

// Revocation is checked for both identities selectedCredential accepts, so a
// delete that lands between selection and admission cannot slip through.
func TestQuotaInFlightAdmissionRechecksRoutedAuthID(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := seedModels(t, s, host)
	quotaFixtureAuthFile(t, s, host, id, nil)
	var once sync.Once
	quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeAdmit: func() {
		once.Do(func() {
			s.mu.Lock()
			s.revoked[id] = true
			s.mu.Unlock()
		})
	}})
	// StorageJSON carries a different id, so only the routed AuthID is revoked.
	snapshot := Credential{Type: Provider, ID: "open-design-other-record", APIKey: "sk-amr-routed-check"}
	_, err := execute(t, s, executorPayload{
		AuthID:      id,
		Model:       "od-large",
		Payload:     []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		StorageJSON: jsonBytes(snapshot),
	})
	if err == nil {
		t.Fatal("request routed through a revoked id was admitted")
	}
	if code, _ := quotaErrorKind(t, err); code != "credential_unavailable" {
		t.Fatalf("error = %s, want credential_unavailable", code)
	}
	if host.countMethod("host.http.do") != 0 {
		t.Fatal("revoked request reached the upstream")
	}
	if total := quotaInFlightTotal(s); total != 0 {
		t.Fatalf("rejected request left %d counters", total)
	}
}

// A StorageJSON snapshot carries no runtime epoch even when a live record for the
// same key and id already exists (the host hands back its own JSON). The late 402
// must still be observed while that generation is unchanged, and must be dropped
// as soon as the generation moves.
func TestQuotaInFlightObservesLateErrorWithSnapshotEpochZero(t *testing.T) {
	const key = "sk-amr-latepublication-0004"
	for _, tc := range []struct {
		name        string
		afterAdmit  func(s *Service, c Credential) Credential
		wantBlocked bool
	}{
		{
			name:        "generation unchanged",
			afterAdmit:  func(s *Service, c Credential) Credential { return c },
			wantBlocked: true,
		},
		{
			name: "explicit resume after admission",
			afterAdmit: func(s *Service, c Credential) Credential {
				c.QuotaRevision = "fixture-resume-revision"
				s.credentialEpoch[c.ID]++
				c.runtimeEpoch = s.credentialEpoch[c.ID]
				return c
			},
		},
		{
			name: "manual toggle after admission",
			afterAdmit: func(s *Service, c Credential) Credential {
				c.Disabled = true
				return c
			},
		},
		{
			name: "key replaced after admission",
			afterAdmit: func(s *Service, c Credential) Credential {
				c.APIKey = "sk-amr-latepublication-0004b"
				return c
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, host := newQuotaPauseTestService(t)
			live := seedModels(t, s, host)
			quotaFixtureAuthFile(t, s, host, live, nil)
			host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusPaymentRequired, Body: []byte(quotaPauseFixtureBody)})
			current, _ := liveCredential(s, live)
			// The host hands back a plain snapshot: no runtime epoch, no revision.
			snapshot := Credential{Type: Provider, ID: live, Label: current.Label, APIKey: current.APIKey}

			var once sync.Once
			quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeObserve: func() {
				once.Do(func() {
					s.mu.Lock()
					defer s.mu.Unlock()
					s.storeCredentialLocked(tc.afterAdmit(s, s.creds[live]))
				})
			}})

			if _, err := execute(t, s, executorPayload{
				AuthID:      live,
				Model:       "od-large",
				Payload:     []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
				StorageJSON: jsonBytes(snapshot),
			}); err == nil {
				t.Fatal("402 was returned as a success")
			}
			got, ok := liveCredential(s, live)
			if !ok {
				t.Fatal("credential disappeared")
			}
			if got.quotaBlocked() != tc.wantBlocked {
				t.Fatalf("blocked = %t, want %t", got.quotaBlocked(), tc.wantBlocked)
			}
			if total := quotaInFlightTotal(s); total != 0 {
				t.Fatalf("counters leaked: total=%d", total)
			}
		})
	}
}
