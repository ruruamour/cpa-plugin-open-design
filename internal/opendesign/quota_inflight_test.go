package opendesign

import (
	"net/http"
	"sort"
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

// gatedReadHost parks the first host.http.stream_read of each upstream stream, so
// a test can hold several streaming requests open at the same time.
type gatedReadHost struct {
	*fakeHost
	mu      sync.Mutex
	seen    map[string]bool
	arrived chan string
	gate    chan struct{}
}

func newGatedReadHost(host *fakeHost) *gatedReadHost {
	return &gatedReadHost{fakeHost: host, seen: map[string]bool{}, arrived: make(chan string, 8), gate: make(chan struct{})}
}

func (h *gatedReadHost) call(method string, in, out any) error {
	if method == "host.http.stream_read" {
		id := stringField(in, "stream_id")
		h.mu.Lock()
		first := !h.seen[id]
		h.seen[id] = true
		h.mu.Unlock()
		if first {
			h.arrived <- id
			<-h.gate
		}
	}
	return h.fakeHost.call(method, in, out)
}

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

// A pause, manual disable, key replacement or deletion that lands between
// credential selection and admission must fail locally: the request may never
// reach the upstream and must leave no in-flight counter behind.
func TestQuotaInFlightAdmissionRechecksLiveCredential(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(t *testing.T, s *Service, id string)
		wantCode string
		wantHTTP int
	}{
		{
			name: "quota pause lands",
			mutate: func(t *testing.T, s *Service, id string) {
				setFixtureQuotaBlock(t, s, id)
			},
			wantCode: "quota_blocked",
			wantHTTP: http.StatusTooManyRequests,
		},
		{
			name: "manual disable lands",
			mutate: func(t *testing.T, s *Service, id string) {
				s.mu.Lock()
				defer s.mu.Unlock()
				c := s.creds[id]
				c.Disabled = true
				s.storeCredentialLocked(c)
			},
			wantCode: "credential_unavailable",
			wantHTTP: http.StatusUnauthorized,
		},
		{
			name: "key replaced",
			mutate: func(t *testing.T, s *Service, id string) {
				s.mu.Lock()
				defer s.mu.Unlock()
				c := s.creds[id]
				c.APIKey = "fixture-replaced-inference-key"
				c.QuotaBlock = nil
				s.storeCredentialLocked(c)
			},
			wantCode: "credential_changed",
			wantHTTP: http.StatusConflict,
		},
		{
			name: "credential deleted",
			mutate: func(t *testing.T, s *Service, id string) {
				s.mu.Lock()
				defer s.mu.Unlock()
				delete(s.creds, id)
				s.revoked[id] = true
			},
			wantCode: "credential_unavailable",
			wantHTTP: http.StatusUnauthorized,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, host := newQuotaPauseTestService(t)
			id := seedModels(t, s, host)
			quotaFixtureAuthFile(t, s, host, id, nil)
			var once sync.Once
			quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeAdmit: func() {
				once.Do(func() { tc.mutate(t, s, id) })
			}})
			_, err := execute(t, s, bufferedQuotaRequest(id))
			if err == nil {
				t.Fatal("request was admitted after the credential changed")
			}
			if code, status := quotaErrorKind(t, err); code != tc.wantCode || status != tc.wantHTTP {
				t.Fatalf("error = %d/%s, want %d/%s", status, code, tc.wantHTTP, tc.wantCode)
			}
			if host.countMethod("host.http.do") != 0 {
				t.Fatal("rejected request still reached the upstream")
			}
			if total := quotaInFlightTotal(s); total != 0 {
				t.Fatalf("rejected request left %d counters", total)
			}
		})
	}

	// The streaming path applies the same fence before opening the stream.
	t.Run("stream pause lands", func(t *testing.T) {
		s, host := newQuotaPauseTestService(t)
		id := seedModels(t, s, host)
		quotaFixtureAuthFile(t, s, host, id, nil)
		var once sync.Once
		quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeAdmit: func() {
			once.Do(func() { setFixtureQuotaBlock(t, s, id) })
		}})
		if _, err := executeStream(t, s, streamQuotaRequest(id, "out-race")); err == nil {
			t.Fatal("paused stream was opened")
		}
		if host.countMethod("host.http.do_stream") != 0 {
			t.Fatal("rejected stream still reached the upstream")
		}
		if total := quotaInFlightTotal(s); total != 0 {
			t.Fatalf("rejected stream left %d counters", total)
		}
	})
}

// The explicit model test pins a credential snapshot. A replace-before-admit must
// be rejected instead of reaching the newly installed key.
func TestQuotaInFlightAdmissionRejectsStaleModelTestSnapshot(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "primary", "sk-model-test-stale-key")
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "od-large", "upstream_id": "opendesign-large"}},
	}); status != http.StatusOK {
		t.Fatalf("seed mapping: %d %s", status, raw)
	}
	var once sync.Once
	quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeAdmit: func() {
		once.Do(func() {
			// Replace the key exactly between snapshot validation and admission.
			if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
				"id": id, "api_key": "sk-model-test-newer-key",
			}); status != http.StatusOK {
				t.Fatalf("key replacement: %d %s", status, raw)
			}
		})
	}})
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
		"credential_id": id, "model_id": "od-large",
	})
	if status != http.StatusOK {
		t.Fatalf("model test envelope: %d %s", status, raw)
	}
	if host.countMethod("host.http.do") != 0 {
		t.Fatal("stale model-test snapshot reached the upstream")
	}
	if total := quotaInFlightTotal(s); total != 0 {
		t.Fatalf("stale model test left %d counters", total)
	}
}

// Inference is not throttled: two requests for the same account are admitted at
// the same time and both are counted.
func TestQuotaInFlightIsNotAConcurrencyLimit(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := seedModels(t, s, host)
	quotaFixtureAuthFile(t, s, host, id, nil)
	gated := newGatedQuotaHost(host)
	s.SetHost(gated.call)
	host.doResponses = append(host.doResponses, bufferOK(), bufferOK())

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := func(index int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[index] = execute(t, s, bufferedQuotaRequest(id))
		}()
	}
	start(0)
	gated.waitGates(t, 1)
	start(1)
	gated.waitGates(t, 2)

	c, _ := liveCredential(s, id)
	has, count, total := quotaInFlightState(s, c)
	if !has || count != 2 || total != 2 {
		t.Fatalf("in-flight tracking = has:%t count:%d total:%d, want both admitted", has, count, total)
	}

	// Both requests were already at the upstream host call, so no concurrency cap
	// rejected the second one.
	gated.releaseGate(0)
	gated.releaseGate(1)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
	}
	if got := host.countMethod("host.http.do"); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if _, _, total := quotaInFlightState(s, c); total != 0 {
		t.Fatalf("counters leaked: total=%d", total)
	}
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
// ones that observed the quota block.
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

	var mu sync.Mutex
	var seenCounts []int
	var seenBlocked []bool
	quotaInFlightFixture(t, s, quotaInFlightTestHook{beforeRelease: func(key quotaInFlightKey) {
		c, ok := liveCredential(s, id)
		s.mu.RLock()
		count := s.quotaInflight[key]
		s.mu.RUnlock()
		mu.Lock()
		seenCounts = append(seenCounts, count)
		seenBlocked = append(seenBlocked, ok && c.quotaBlocked())
		mu.Unlock()
	}})

	if _, err := executeStream(t, s, streamQuotaRequest(id, "out-multi-a")); err != nil {
		t.Fatalf("first stream: %v", err)
	}
	gated.awaitStream(t)
	if _, err := executeStream(t, s, streamQuotaRequest(id, "out-multi-b")); err != nil {
		t.Fatalf("second stream: %v", err)
	}
	gated.awaitStream(t)

	c, _ := liveCredential(s, id)
	if has, count, total := quotaInFlightState(s, c); !has || count != 2 || total != 2 {
		t.Fatalf("two streams in flight = has:%t count:%d total:%d", has, count, total)
	}

	close(gated.gate)
	s.waitIdle()

	mu.Lock()
	counts := append([]int(nil), seenCounts...)
	blocked := append([]bool(nil), seenBlocked...)
	mu.Unlock()
	if len(counts) != 2 {
		t.Fatalf("terminal callbacks = %d, want 2", len(counts))
	}
	sort.Ints(counts)
	if counts[0] != 1 || counts[1] != 2 {
		t.Fatalf("counts at release = %v, want the two streams to unwind one by one", counts)
	}
	for i, ok := range blocked {
		if !ok {
			t.Fatalf("release %d ran before the quota block was observed", i)
		}
	}
	if _, _, total := quotaInFlightState(s, c); total != 0 {
		t.Fatalf("counters leaked: total=%d", total)
	}
}

// The last release for a key wakes the existing worker exactly once per idle
// transition, and the wake is consumed without any lock the release held. No
// worker is started here, so the buffered wake stays observable.
func TestQuotaInFlightReleaseWakesWorkerWhenIdle(t *testing.T) {
	s := NewService()
	c := Credential{Type: Provider, ID: "fixture-inflight", Label: "fixture", APIKey: "fixture-inflight-key"}
	s.mu.Lock()
	s.storeCredentialLocked(c)
	s.mu.Unlock()
	live, _ := liveCredential(s, c.ID)

	first, err := s.admitQuotaInFlight(live)
	if err != nil {
		t.Fatalf("admit first: %v", err)
	}
	second, err := s.admitQuotaInFlight(live)
	if err != nil {
		t.Fatalf("admit second: %v", err)
	}
	if _, count, _ := quotaInFlightState(s, live); count != 2 {
		t.Fatalf("same key was not counted twice: %d", count)
	}

	// The first release does not reach zero, so the worker is not woken yet and
	// the fence still reports the same key as busy.
	first.release(s)
	if has, count, _ := quotaInFlightState(s, live); !has || count != 1 {
		t.Fatalf("partial release = has:%t count:%d", has, count)
	}
	if len(s.quotaPauseWake) != 0 {
		t.Fatal("a non-final release woke the quota-pause worker")
	}

	// The last release reaches zero and wakes the worker. release is idempotent,
	// so the cleanup path and the terminal defer cannot double-count or double-wake.
	second.release(s)
	second.release(s)
	if _, _, total := quotaInFlightState(s, live); total != 0 {
		t.Fatalf("counters leaked: total=%d", total)
	}
	if len(s.quotaPauseWake) != 1 {
		t.Fatal("the idle transition did not wake the quota-pause worker")
	}
	// Consuming the wake needs no lock the release held.
	select {
	case <-s.quotaPauseWake:
	default:
		t.Fatal("wake signal was not readable")
	}

	// A fresh idle transition on a blocked account still wakes the worker; the
	// wake does not depend on the block state, so a parent recovery is never
	// skipped.
	third, err := s.admitQuotaInFlight(live)
	if err != nil {
		t.Fatalf("admit third: %v", err)
	}
	manual := false
	s.mu.Lock()
	blocked := s.creds[c.ID]
	blocked.QuotaBlock = &QuotaBlock{
		KeyHash: quotaPauseKeyHash(blocked.APIKey), ManualDisabled: &manual,
		Source: "upstream", Reason: "quota_exhausted",
		BlockedAt: time.Now().Add(-time.Minute), RetryAt: time.Now().Add(time.Hour),
	}
	blocked.QuotaRevision = "fixture-revision"
	s.storeCredentialLocked(blocked)
	s.mu.Unlock()
	third.release(s)
	if len(s.quotaPauseWake) != 1 {
		t.Fatal("the last release for a blocked key did not wake the quota-pause worker")
	}
}
