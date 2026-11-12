package opendesign

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateClock is a manually advanced clock for the concurrency gate.
type gateClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *gateClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *gateClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// gateFixture returns a service with one account, one mapping and a fake gate
// clock. The gate runs on its built-in defaults unless a test changes them.
func gateFixture(t *testing.T) (*Service, *fakeHost, string, *gateClock) {
	t.Helper()
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	clock := &gateClock{now: time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)}
	s.concurrencyClock = clock.Now
	return s, host, id, clock
}

func setConcurrency(t *testing.T, s *Service, policy map[string]any) map[string]any {
	t.Helper()
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/config", nil, map[string]any{"concurrency": policy})
	if status != http.StatusOK {
		t.Fatalf("set concurrency %v: %d %s", policy, status, raw)
	}
	view, _ := body["concurrency"].(map[string]any)
	if view == nil {
		t.Fatalf("config response has no concurrency object: %s", raw)
	}
	return view
}

func gateAdmit(t *testing.T, s *Service, id string) *quotaInFlightLease {
	t.Helper()
	c, ok := liveCredential(s, id)
	if !ok {
		t.Fatalf("credential %s is not live", id)
	}
	lease, err := s.admitQuotaInFlight(c, id)
	if err != nil {
		t.Fatalf("admission refused: %v", err)
	}
	return lease
}

func gateRefused(t *testing.T, s *Service, id string) error {
	t.Helper()
	c, ok := liveCredential(s, id)
	if !ok {
		t.Fatalf("credential %s is not live", id)
	}
	lease, err := s.admitQuotaInFlight(c, id)
	if err == nil {
		lease.release(s)
		t.Fatalf("admission succeeded, want a local refusal")
	}
	return err
}

// upstream429 is what the executor builds for an upstream rate limit.
func upstream429(retryAfter string) error {
	headers := http.Header{}
	if retryAfter != "" {
		headers.Set("Retry-After", retryAfter)
	}
	return upstreamErrorFromResponse(http.StatusTooManyRequests, headers, []byte(`{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`))
}

// gateReject settles a lease with an upstream rate limit and releases it. It
// returns the cooldown the plugin would hand to the host.
func gateReject(s *Service, lease *quotaInFlightLease) int {
	err := upstream429("")
	s.settleConcurrency(lease, err)
	lease.release(s)
	return HostRetryAfterSeconds(err)
}

func gateAccept(s *Service, lease *quotaInFlightLease) {
	s.settleConcurrency(lease, nil)
	lease.release(s)
}

func inFlightTotal(s *Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quotaInFlightTotalLocked()
}

func gateStats(t *testing.T, s *Service) map[string]any {
	t.Helper()
	_, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/config", nil, nil)
	view, _ := body["concurrency"].(map[string]any)
	stats, _ := view["stats"].(map[string]any)
	if stats == nil {
		t.Fatalf("config has no concurrency stats: %s", raw)
	}
	return stats
}

// The default limit is four requests per account. The fifth is refused before
// anything is sent, with the marker the built-in rule turns into "continue".
func TestConcurrencyGateRefusesTheRequestOverTheDefaultLimit(t *testing.T) {
	s, _, id, _ := gateFixture(t)
	leases := make([]*quotaInFlightLease, 0, 4)
	for i := 0; i < concurrencyDefaultMaxInFlight; i++ {
		leases = append(leases, gateAdmit(t, s, id))
	}
	if concurrencyDefaultMaxInFlight != 4 {
		t.Fatalf("default limit = %d, want 4", concurrencyDefaultMaxInFlight)
	}
	err := gateRefused(t, s, id)
	if streamErrorCode(err) != concurrencyLimitKind || statusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("refusal = %v (%d), want %s with 429", err, statusOf(err), concurrencyLimitKind)
	}
	if !strings.Contains(err.Error(), concurrencyBusyMarker) {
		t.Fatalf("refusal %q carries no %s marker", err.Error(), concurrencyBusyMarker)
	}
	if upstreamErrorWasAttempted(err) {
		t.Fatal("a local refusal claims it reached the upstream")
	}
	// A freed slot is usable at once: the limit is a ceiling, not a cooldown.
	gateAccept(s, leases[0])
	next := gateAdmit(t, s, id)
	_ = gateRefused(t, s, id)
	for _, lease := range append(leases[1:], next) {
		gateAccept(s, lease)
	}
	if n := inFlightTotal(s); n != 0 {
		t.Fatalf("%d in-flight counters leaked", n)
	}
	if got := gateStats(t, s)["rejected_at_limit"]; got != float64(2) {
		t.Fatalf("rejected_at_limit = %v, want 2", got)
	}
}

// The limit is a setting, not a constant.
func TestConcurrencyGateLimitFollowsTheSetting(t *testing.T) {
	s, _, id, _ := gateFixture(t)
	for _, limit := range []int{1, 2, 7} {
		setConcurrency(t, s, map[string]any{"max_in_flight": limit})
		leases := make([]*quotaInFlightLease, 0, limit)
		for i := 0; i < limit; i++ {
			leases = append(leases, gateAdmit(t, s, id))
		}
		_ = gateRefused(t, s, id)
		for _, lease := range leases {
			gateAccept(s, lease)
		}
	}
}

// Lowering the limit below what is already running refuses new requests but
// never cancels the ones in flight.
func TestConcurrencyGateLoweredLimitDoesNotTouchRunningRequests(t *testing.T) {
	s, _, id, _ := gateFixture(t)
	setConcurrency(t, s, map[string]any{"max_in_flight": 4})
	leases := []*quotaInFlightLease{gateAdmit(t, s, id), gateAdmit(t, s, id), gateAdmit(t, s, id)}
	setConcurrency(t, s, map[string]any{"max_in_flight": 2})
	_ = gateRefused(t, s, id)
	gateAccept(s, leases[0])
	_ = gateRefused(t, s, id) // still two running
	gateAccept(s, leases[1])
	gateAccept(s, gateAdmit(t, s, id))
	gateAccept(s, leases[2])
}

// Two accounts do not share a budget.
func TestConcurrencyGateIsPerAccount(t *testing.T) {
	s, host, first, _ := gateFixture(t)
	second := addCredential(t, s, host, "secondary", "sk-amr-exec-0002")
	setConcurrency(t, s, map[string]any{"max_in_flight": 1})
	a := gateAdmit(t, s, first)
	_ = gateRefused(t, s, first)
	b := gateAdmit(t, s, second)
	gateAccept(s, a)
	gateAccept(s, b)
}

// After an upstream rate limit the account is left alone for the hold, and the
// same duration (plus a second of slack) is what the host is told to wait.
func TestConcurrencyGateHoldsTheAccountAfterAnUpstreamRateLimit(t *testing.T) {
	s, _, id, clock := gateFixture(t)
	if seconds := gateReject(s, gateAdmit(t, s, id)); seconds != 61 {
		t.Fatalf("host retry-after = %d s, want 61 (60 s hold + 1 s slack)", seconds)
	}
	err := gateRefused(t, s, id)
	if streamErrorCode(err) != concurrencyHeldKind || !strings.Contains(err.Error(), concurrencyBusyMarker) {
		t.Fatalf("refusal while held = %v, want %s with the busy marker", err, concurrencyHeldKind)
	}
	clock.advance(59 * time.Second)
	_ = gateRefused(t, s, id)
	clock.advance(time.Second)
	// The hold is over: exactly one request goes out to find out.
	probe := gateAdmit(t, s, id)
	if probe.gate.probe == 0 {
		t.Fatal("the first request after a hold is not marked as the probe")
	}
	if err := gateRefused(t, s, id); streamErrorCode(err) != concurrencyHeldKind {
		t.Fatalf("second request during the probe = %v, want %s", err, concurrencyHeldKind)
	}
	gateAccept(s, probe)
	// The release itself drops the recovered entry; no management read has
	// happened yet that could have swept it.
	s.mu.Lock()
	afterProbe := len(s.concurrencyStates)
	s.mu.Unlock()
	if afterProbe != 0 {
		t.Fatalf("%d hold entries remain right after the probe was accepted", afterProbe)
	}
	// Accepted: the account is back to its full limit.
	leases := make([]*quotaInFlightLease, 0, 4)
	for i := 0; i < 4; i++ {
		leases = append(leases, gateAdmit(t, s, id))
	}
	if err := gateRefused(t, s, id); streamErrorCode(err) != concurrencyLimitKind {
		t.Fatalf("fifth request after recovery = %v, want %s", err, concurrencyLimitKind)
	}
	for _, lease := range leases {
		gateAccept(s, lease)
	}
	stats := gateStats(t, s)
	if stats["upstream_rate_limited"] != float64(1) || stats["holds_started"] != float64(1) || stats["rejected_while_held"] != float64(3) {
		t.Fatalf("stats = %v, want 1 rate limit, 1 hold, 3 refusals while held", stats)
	}
	s.mu.Lock()
	left := len(s.concurrencyStates)
	s.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d hold entries survive a recovered account", left)
	}
}

// A probe that is rejected again doubles the hold, up to the maximum.
func TestConcurrencyGateDoublesTheHoldUntilTheMaximum(t *testing.T) {
	s, _, id, clock := gateFixture(t)
	want := []int{61, 121, 241, 301, 301}
	for i, seconds := range want {
		got := gateReject(s, gateAdmit(t, s, id))
		if got != seconds {
			t.Fatalf("rejection %d: host retry-after = %d s, want %d", i+1, got, seconds)
		}
		_ = gateRefused(t, s, id)
		clock.advance(time.Duration(seconds) * time.Second)
	}
	// One accepted request resets the escalation.
	gateAccept(s, gateAdmit(t, s, id))
	if got := gateReject(s, gateAdmit(t, s, id)); got != 61 {
		t.Fatalf("after a recovery the hold is %d s, want the base 61", got)
	}
}

// Four requests that were all in flight when the account got rejected are one
// event. They must not escalate the hold four times.
func TestConcurrencyGateCountsABurstOfRejectionsOnce(t *testing.T) {
	s, _, id, clock := gateFixture(t)
	leases := []*quotaInFlightLease{gateAdmit(t, s, id), gateAdmit(t, s, id), gateAdmit(t, s, id), gateAdmit(t, s, id)}
	for i, lease := range leases {
		if got := gateReject(s, lease); got != 61 {
			t.Fatalf("burst rejection %d: host retry-after = %d s, want 61", i+1, got)
		}
	}
	if got := gateStats(t, s); got["holds_started"] != float64(1) || got["upstream_rate_limited"] != float64(4) {
		t.Fatalf("stats = %v, want 1 hold from 4 rate limits", got)
	}
	clock.advance(61 * time.Second)
	if got := gateReject(s, gateAdmit(t, s, id)); got != 121 {
		t.Fatalf("the probe's rejection gives %d s, want 121 (second strike)", got)
	}
}

// A request that was already running when the account got rejected proves
// nothing when it succeeds later: the hold stays.
func TestConcurrencyGateIgnoresASuccessAdmittedBeforeTheRejection(t *testing.T) {
	s, _, id, clock := gateFixture(t)
	old := gateAdmit(t, s, id)
	gateReject(s, gateAdmit(t, s, id))
	gateAccept(s, old)
	if err := gateRefused(t, s, id); streamErrorCode(err) != concurrencyHeldKind {
		t.Fatalf("after an old success the account is %v, want still held", err)
	}
	clock.advance(61 * time.Second)
	// And it still takes a probe, not the full limit.
	probe := gateAdmit(t, s, id)
	_ = gateRefused(t, s, id)
	gateAccept(s, probe)
}

// A probe that ends in something other than a rate limit or a success (a
// timeout, a transport failure, a 5xx) frees the probe slot without changing
// the escalation.
func TestConcurrencyGateFreesTheProbeSlotOnAnUnrelatedFailure(t *testing.T) {
	s, _, id, clock := gateFixture(t)
	gateReject(s, gateAdmit(t, s, id))
	clock.advance(61 * time.Second)
	for _, failure := range []error{
		upstreamTransportError(os.ErrDeadlineExceeded),
		upstreamErrorFromResponse(http.StatusInternalServerError, nil, []byte(`{"error":{"code":"server_error"}}`)),
		upstreamErrorFromResponse(http.StatusBadRequest, nil, []byte(`{"error":{"message":"bad"}}`)),
	} {
		probe := gateAdmit(t, s, id)
		_ = gateRefused(t, s, id)
		s.settleConcurrency(probe, failure)
		probe.release(s)
		if HostRetryAfterSeconds(failure) != 0 {
			t.Fatalf("%v carries a host retry-after", failure)
		}
	}
	// Still one strike: the next rejection is the second, not the fifth.
	if got := gateReject(s, gateAdmit(t, s, id)); got != 121 {
		t.Fatalf("hold after unrelated probe failures = %d s, want 121", got)
	}
}

// The upstream's own Retry-After can lengthen a hold but not past the maximum,
// and a shorter one does not shorten it.
func TestConcurrencyGateHonoursUpstreamRetryAfterWithinTheMaximum(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   int
	}{{"5", 61}, {"90", 91}, {"100000", 301}, {"0", 61}, {"not-a-number", 61}} {
		s, _, id, _ := gateFixture(t)
		lease := gateAdmit(t, s, id)
		err := upstream429(tc.header)
		s.settleConcurrency(lease, err)
		lease.release(s)
		if got := HostRetryAfterSeconds(err); got != tc.want {
			t.Errorf("Retry-After %q: host retry-after = %d s, want %d", tc.header, got, tc.want)
		}
	}
}

// hold_seconds = 0 turns the hold off and keeps the limit.
func TestConcurrencyGateHoldCanBeTurnedOffSeparately(t *testing.T) {
	s, _, id, _ := gateFixture(t)
	setConcurrency(t, s, map[string]any{"hold_seconds": 0, "max_hold_seconds": 0, "max_in_flight": 2})
	if got := gateReject(s, gateAdmit(t, s, id)); got != 0 {
		t.Fatalf("host retry-after = %d s with the hold off, want none", got)
	}
	a, b := gateAdmit(t, s, id), gateAdmit(t, s, id)
	if err := gateRefused(t, s, id); streamErrorCode(err) != concurrencyLimitKind {
		t.Fatalf("third request = %v, want the limit to still apply", err)
	}
	gateAccept(s, a)
	gateAccept(s, b)
	if got := gateStats(t, s); got["upstream_rate_limited"] != float64(1) || got["holds_started"] != float64(0) {
		t.Fatalf("stats = %v, want the rate limit counted and no hold", got)
	}
}

// enabled = false is the 0.5.0 behaviour: nothing is refused, nothing is held,
// and the host gets no retry hint.
func TestConcurrencyGateDisabledChangesNothing(t *testing.T) {
	s, _, id, _ := gateFixture(t)
	setConcurrency(t, s, map[string]any{"enabled": false})
	leases := make([]*quotaInFlightLease, 0, 12)
	for i := 0; i < 12; i++ {
		leases = append(leases, gateAdmit(t, s, id))
	}
	if got := gateReject(s, leases[0]); got != 0 {
		t.Fatalf("host retry-after = %d s with the protection off", got)
	}
	leases = append(leases[1:], gateAdmit(t, s, id))
	for _, lease := range leases {
		gateAccept(s, lease)
	}
	if n := inFlightTotal(s); n != 0 {
		t.Fatalf("%d in-flight counters leaked", n)
	}
}

// Turning the protection off releases a held account at once, and turning it
// back on does not resurrect the old hold.
func TestConcurrencyGateDisablingReleasesHeldAccounts(t *testing.T) {
	s, _, id, _ := gateFixture(t)
	gateReject(s, gateAdmit(t, s, id))
	_ = gateRefused(t, s, id)
	setConcurrency(t, s, map[string]any{"enabled": false})
	gateAccept(s, gateAdmit(t, s, id))
	setConcurrency(t, s, map[string]any{"enabled": true})
	gateAccept(s, gateAdmit(t, s, id))
}

// A quota exhaustion belongs to the quota pause, not to the gate.
func TestConcurrencyGateLeavesQuotaExhaustionToThePause(t *testing.T) {
	for name, failure := range map[string]error{
		"402 insufficient_balance": &APIError{Status: http.StatusPaymentRequired, Kind: "insufficient_balance", upstreamAttempted: true, upstreamStatus: http.StatusPaymentRequired, quotaExhausted: true},
		"429 insufficient_quota":   &APIError{Status: http.StatusTooManyRequests, Kind: "insufficient_quota", upstreamAttempted: true, upstreamStatus: http.StatusTooManyRequests},
		"429 exhausted, rate kind": &APIError{Status: http.StatusTooManyRequests, Kind: "rate_limit_exceeded", upstreamAttempted: true, upstreamStatus: http.StatusTooManyRequests, quotaExhausted: true},
		"local 429":                localFail(http.StatusTooManyRequests, "quota_blocked", quotaBlockMarker+" paused"),
		"local rate_limit kind":    localFail(http.StatusTooManyRequests, "rate_limit_exceeded", "never sent"),
		"transport":                upstreamTransportError(os.ErrDeadlineExceeded),
		"500 server_error":         upstreamErrorFromResponse(http.StatusInternalServerError, nil, []byte(`{"error":{"code":"server_error"}}`)),
		"400 unclassified":         upstreamErrorFromResponse(http.StatusBadRequest, nil, []byte(`{"error":{"message":"bad request"}}`)),
	} {
		if _, rejected := concurrencyRejection(failure); rejected {
			t.Errorf("%s is treated as an upstream rate limit", name)
		}
	}
	for name, failure := range map[string]error{
		"429 rate_limit_exceeded": upstream429(""),
		"200 rate_limit_exceeded": upstreamErrorFromResponseObserved(http.StatusBadGateway, http.StatusOK, nil, []byte(`{"error":{"code":"rate_limit_exceeded"}}`)),
		"429 unclassified":        upstreamErrorFromResponse(http.StatusTooManyRequests, nil, []byte(`not json`)),
		"400 tier_limit_exceeded": upstreamErrorFromResponse(http.StatusBadRequest, nil, []byte(`{"error":{"message":"[code=tier_limit_exceeded] membership concurrency limit exceeded","type":"invalid_request_error"}}`)),
	} {
		if _, rejected := concurrencyRejection(failure); !rejected {
			t.Errorf("%s is not treated as an upstream rate limit", name)
		}
	}
}

// The host is only given a retry hint next to a 429: a tier limit arrives as a
// 400, holds the account locally, and must not carry one.
func TestConcurrencyGateRetryHintOnlyTravelsWithA429(t *testing.T) {
	s, _, id, _ := gateFixture(t)
	lease := gateAdmit(t, s, id)
	failure := upstreamErrorFromResponse(http.StatusBadRequest, nil, []byte(`{"error":{"message":"[code=tier_limit_exceeded] membership concurrency limit exceeded"}}`))
	s.settleConcurrency(lease, failure)
	lease.release(s)
	if got := HostRetryAfterSeconds(failure); got != 0 {
		t.Fatalf("a 400 carries a host retry-after of %d s", got)
	}
	if err := gateRefused(t, s, id); streamErrorCode(err) != concurrencyHeldKind {
		t.Fatalf("after a tier limit the account is %v, want held", err)
	}
}

// End to end through the buffered executor: an upstream 429 holds the account,
// the error handed to the host keeps its status and marker and gains the
// cooldown, and the next request never leaves the plugin.
func TestBufferedExecutorHoldsTheAccountAndTellsTheHostHowLong(t *testing.T) {
	s, host, id, _ := gateFixture(t)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusTooManyRequests,
		Body:   []byte(`{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`),
	})
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	_, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body})
	if streamErrorCode(err) != "rate_limit_exceeded" {
		t.Fatalf("executor error = %v, want rate_limit_exceeded", err)
	}
	message, status := s.HostErrorDetails(err)
	if status != http.StatusTooManyRequests || !strings.Contains(message, upstreamErrorMarkerFor("rate_limit_exceeded")) {
		t.Fatalf("host sees %d %q, want the 429 with its marker", status, message)
	}
	if got := HostRetryAfterSeconds(err); got != 61 {
		t.Fatalf("host retry-after = %d s, want 61", got)
	}
	_, err = execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body})
	if streamErrorCode(err) != concurrencyHeldKind {
		t.Fatalf("second request = %v, want %s", err, concurrencyHeldKind)
	}
	message, status = s.HostErrorDetails(err)
	if status != http.StatusTooManyRequests || !strings.Contains(message, concurrencyBusyMarker) {
		t.Fatalf("host sees %d %q for a held account, want a 429 with the busy marker", status, message)
	}
	if HostRetryAfterSeconds(err) != 0 {
		t.Fatal("a local refusal carries a host retry-after")
	}
	if n := host.countMethod("host.http.do"); n != 1 {
		t.Fatalf("the upstream was called %d times, want 1", n)
	}
	if n := inFlightTotal(s); n != 0 {
		t.Fatalf("%d in-flight counters leaked", n)
	}
}

// The same through the streaming executor, for a rejection before any output.
func TestStreamExecutorHoldsTheAccountOnARejectedOpen(t *testing.T) {
	s, host, id, _ := gateFixture(t)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusTooManyRequests,
		chunks: [][]byte{[]byte(`{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`)},
	})
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	_, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body, Stream: true, StreamID: "client-1"})
	if streamErrorCode(err) != "rate_limit_exceeded" || HostRetryAfterSeconds(err) != 61 {
		t.Fatalf("stream open error = %v with retry-after %d, want rate_limit_exceeded with 61", err, HostRetryAfterSeconds(err))
	}
	_, err = executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body, Stream: true, StreamID: "client-2"})
	if streamErrorCode(err) != concurrencyHeldKind {
		t.Fatalf("second stream = %v, want %s", err, concurrencyHeldKind)
	}
	s.waitIdle()
	if n := host.countMethod("host.http.do_stream"); n != 1 {
		t.Fatalf("the upstream was called %d times, want 1", n)
	}
	if n := inFlightTotal(s); n != 0 {
		t.Fatalf("%d in-flight counters leaked", n)
	}
}

// A stream that opened is an accepted request even while it is still running,
// so a probe reopens the account without waiting for the stream to finish.
func TestStreamProbeReopensTheAccountWhenTheStreamOpens(t *testing.T) {
	s, host, id, clock := gateFixture(t)
	gateReject(s, gateAdmit(t, s, id))
	clock.advance(61 * time.Second)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	// Registered after the fixture, so it runs before the fixture's quiesce: a
	// failed assertion below must fail the test, not leave the stream parked.
	t.Cleanup(unblock)
	host.readHook = func(int) { <-release }
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{[]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")},
	})
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body, Stream: true, StreamID: "client-1"}); err != nil {
		t.Fatalf("probe stream: %v", err)
	}
	// The probe is still streaming; three more fit under the limit of four.
	leases := []*quotaInFlightLease{gateAdmit(t, s, id), gateAdmit(t, s, id), gateAdmit(t, s, id)}
	if err := gateRefused(t, s, id); streamErrorCode(err) != concurrencyLimitKind {
		t.Fatalf("fifth request = %v, want the limit", err)
	}
	for _, lease := range leases {
		gateAccept(s, lease)
	}
	unblock()
	s.waitIdle()
	if n := inFlightTotal(s); n != 0 {
		t.Fatalf("%d in-flight counters leaked", n)
	}
}

// A rate limit that arrives as an event inside an open stream still holds the
// account, and the cooldown reaches the host through error_detail.
func TestStreamExecutorHoldsTheAccountOnARateLimitEvent(t *testing.T) {
	s, host, id, _ := gateFixture(t)
	host.doStreams = append(host.doStreams, scriptedStream{
		status: http.StatusOK,
		chunks: [][]byte{[]byte(`data: {"error":{"message":"slow down","code":"rate_limit_exceeded"}}` + "\n\n")},
	})
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body, Stream: true, StreamID: "client-1"}); err != nil {
		t.Fatalf("stream open: %v", err)
	}
	s.waitIdle()
	_, detail := closeDetail(t, host)
	if detail["code"] != "rate_limit_exceeded" {
		t.Fatalf("error_detail code = %v, want rate_limit_exceeded", detail["code"])
	}
	// The stream opened with 200, so the host is told 502, not 429. The host
	// reads a retry hint only next to a 429, so none may be sent here.
	if detail["http_status"] != http.StatusBadGateway {
		t.Fatalf("error_detail status = %v, want 502", detail["http_status"])
	}
	if _, present := detail["retry_after_seconds"]; present {
		t.Fatalf("error_detail carries a retry hint next to a 502: %v", detail)
	}
	// The account is held all the same: the plugin stops sending to it.
	if err := gateRefused(t, s, id); streamErrorCode(err) != concurrencyHeldKind {
		t.Fatalf("after a rate-limit event the account is %v, want held", err)
	}
	if got := gateStats(t, s); got["upstream_rate_limited"] != float64(1) || got["holds_started"] != float64(1) {
		t.Fatalf("stats = %v, want one rate limit and one hold", got)
	}
}

// The same signal inside a 200 body on the buffered path.
func TestBufferedExecutorHoldsTheAccountOnARateLimitInA200Body(t *testing.T) {
	s, host, id, _ := gateFixture(t)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusOK,
		Body:   []byte(`{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`),
	})
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	_, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body})
	if streamErrorCode(err) != "rate_limit_exceeded" || statusOf(err) != http.StatusBadGateway {
		t.Fatalf("executor error = %v (%d), want rate_limit_exceeded as 502", err, statusOf(err))
	}
	if got := HostRetryAfterSeconds(err); got != 0 {
		t.Fatalf("a 502 carries a host retry-after of %d s", got)
	}
	if err := gateRefused(t, s, id); streamErrorCode(err) != concurrencyHeldKind {
		t.Fatalf("after a rate limit in a 200 body the account is %v, want held", err)
	}
}

// A local refusal is counted, never listed: one client request can be refused by
// many accounts in a row, which would bury the requests that were really sent.
func TestConcurrencyRefusalIsCountedNotLogged(t *testing.T) {
	s, host, id, _ := gateFixture(t)
	setConcurrency(t, s, map[string]any{"max_in_flight": 1})
	parked := gateAdmit(t, s, id)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	for i := 0; i < 3; i++ {
		if _, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body}); streamErrorCode(err) != concurrencyLimitKind {
			t.Fatalf("buffered refusal = %v", err)
		}
		if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body, Stream: true, StreamID: "c"}); streamErrorCode(err) != concurrencyLimitKind {
			t.Fatalf("stream refusal = %v", err)
		}
	}
	gateAccept(s, parked)
	// The same holds for an account refused because it is held.
	gateReject(s, gateAdmit(t, s, id))
	for i := 0; i < 2; i++ {
		if _, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body}); streamErrorCode(err) != concurrencyHeldKind {
			t.Fatalf("buffered refusal while held = %v", err)
		}
		if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body, Stream: true, StreamID: "c"}); streamErrorCode(err) != concurrencyHeldKind {
			t.Fatalf("stream refusal while held = %v", err)
		}
	}
	s.waitIdle()
	if n := upstreamCalls(host); n != 0 {
		t.Fatalf("a refused request reached the upstream %d time(s)", n)
	}
	_, logs, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-logs", nil, nil)
	if entries, _ := logs["logs"].([]any); len(entries) != 0 {
		t.Fatalf("local refusals were written to the request log: %s", raw)
	}
	stats := gateStats(t, s)
	if stats["rejected_at_limit"] != float64(6) || stats["rejected_while_held"] != float64(4) {
		t.Fatalf("stats = %v, want 6 refusals at the limit and 4 while held", stats)
	}
	if n := inFlightTotal(s); n != 0 {
		t.Fatalf("%d in-flight counters leaked", n)
	}
}

// The built-in policy maps the busy marker to "continue" (use another account,
// do not cool this one), and it wins over the generic 429 handling.
func TestDefaultRulesContinueWithoutCooldownOnABusyAccount(t *testing.T) {
	var busy *RequestErrorRule
	rules := defaultErrorRules()
	for i := range rules {
		for _, match := range rules[i].Match {
			if match == concurrencyBusyMarker {
				busy = &rules[i]
			}
		}
	}
	if busy == nil {
		t.Fatalf("no default rule matches %s", concurrencyBusyMarker)
	}
	if busy.Status != http.StatusTooManyRequests || busy.Action != "continue" {
		t.Fatalf("busy rule = %+v, want 429 continue", *busy)
	}
	for _, kind := range []string{concurrencyLimitKind, concurrencyHeldKind} {
		message := concurrencyBusyMarker + " upstream account is busy"
		if class := hostSchedulingClass(rules, http.StatusTooManyRequests, message, kind); class != hostScheduleRule {
			t.Errorf("%s with the default rules is class %d, want the rule class", kind, class)
		}
		// An operator who deleted the rule must not get the account cooled down:
		// a bare 429 is what the host answers with a quota cooldown.
		if class := hostSchedulingClass(nil, http.StatusTooManyRequests, message, kind); class != hostScheduleNeutral {
			t.Errorf("%s without its rule is class %d, want neutral", kind, class)
		}
	}
	s, _, id, _ := gateFixture(t)
	setConcurrency(t, s, map[string]any{"max_in_flight": 1})
	parked := gateAdmit(t, s, id)
	refusal := gateRefused(t, s, id)
	gateAccept(s, parked)
	s.mu.Lock()
	s.errorRules = nil
	s.mu.Unlock()
	message, status := s.HostErrorDetails(refusal)
	if status != 0 || !strings.Contains(message, hostTransportHint) || !strings.Contains(message, concurrencyBusyMarker) {
		t.Fatalf("without the rule the host sees %d %q, want the neutral transport form", status, message)
	}
}

// The policy is read, changed, validated and persisted like the other
// plugin-owned settings.
func TestConcurrencyConfigRoundTripAndValidation(t *testing.T) {
	s, _ := newTestService(t)
	_, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/config", nil, nil)
	view, _ := body["concurrency"].(map[string]any)
	if view == nil {
		t.Fatalf("GET /config has no concurrency object: %s", raw)
	}
	for key, want := range map[string]any{"enabled": true, "max_in_flight": float64(4), "hold_seconds": float64(60), "max_hold_seconds": float64(300), "owner": "plugin"} {
		if view[key] != want {
			t.Errorf("default %s = %v, want %v", key, view[key], want)
		}
	}
	// A settings.json from before this feature is not rewritten by a read.
	settingsPath := filepath.Join(s.config().DataDir, settingsFile)
	if before, err := os.ReadFile(settingsPath); err == nil && strings.Contains(string(before), "concurrency") {
		t.Fatalf("defaults were persisted without a save: %s", before)
	}
	updated := setConcurrency(t, s, map[string]any{"enabled": true, "max_in_flight": 6, "hold_seconds": 30, "max_hold_seconds": 120})
	for key, want := range map[string]any{"max_in_flight": float64(6), "hold_seconds": float64(30), "max_hold_seconds": float64(120)} {
		if updated[key] != want {
			t.Errorf("after update %s = %v, want %v", key, updated[key], want)
		}
	}
	// A partial update keeps the other fields.
	partial := setConcurrency(t, s, map[string]any{"max_in_flight": 3})
	if partial["max_in_flight"] != float64(3) || partial["hold_seconds"] != float64(30) || partial["max_hold_seconds"] != float64(120) {
		t.Fatalf("partial update changed other fields: %v", partial)
	}
	persisted, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var onDisk struct {
		Concurrency *ConcurrencyConfig `json:"concurrency"`
	}
	if err := json.Unmarshal(persisted, &onDisk); err != nil || onDisk.Concurrency == nil {
		t.Fatalf("settings.json has no concurrency policy: %s", persisted)
	}
	if onDisk.Concurrency.MaxInFlight != 3 || onDisk.Concurrency.HoldSeconds == nil || *onDisk.Concurrency.HoldSeconds != 30 {
		t.Fatalf("persisted policy = %s", persisted)
	}
	// A new instance on the same data directory resolves the saved policy.
	reloaded := NewService()
	reloaded.SetHost(newFakeHost().call)
	registrar(t, reloaded, s.config().DataDir)
	t.Cleanup(func() { _, _ = reloaded.quiesce() })
	if got := reloaded.config().concurrencySettings(); got.maxInFlight != 3 || got.hold != 30*time.Second || got.maxHold != 120*time.Second || !got.enabled {
		t.Fatalf("reloaded policy = %+v", got)
	}
	for name, policy := range map[string]map[string]any{
		"zero limit":       {"max_in_flight": 0},
		"negative limit":   {"max_in_flight": -1},
		"limit too large":  {"max_in_flight": 65},
		"negative hold":    {"hold_seconds": -1},
		"hold too large":   {"hold_seconds": 3601},
		"max too large":    {"max_hold_seconds": 3601},
		"max below hold":   {"hold_seconds": 200, "max_hold_seconds": 100},
		"hold above saved": {"hold_seconds": 121},
		"unknown field":    {"queue": 5},
	} {
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/config", nil, map[string]any{"concurrency": policy})
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", name, status, raw)
		}
	}
	// A refused update left the saved policy alone.
	if got := s.config().concurrencySettings(); got.maxInFlight != 3 || got.hold != 30*time.Second || got.maxHold != 120*time.Second {
		t.Fatalf("a refused update changed the policy: %+v", got)
	}
}

// A hand-edited settings.json that is out of range is clamped when resolved and
// refused when validated, and can never overflow a duration.
func TestConcurrencySettingsResolveSafely(t *testing.T) {
	huge, negative, zero := 1<<40, -5, 0
	on := true
	cases := map[string]struct {
		policy *ConcurrencyConfig
		want   concurrencySettings
	}{
		"absent":         {nil, concurrencySettings{true, 4, 60 * time.Second, 300 * time.Second}},
		"empty":          {&ConcurrencyConfig{}, concurrencySettings{true, 4, 60 * time.Second, 300 * time.Second}},
		"huge":           {&ConcurrencyConfig{Enabled: &on, MaxInFlight: 1 << 40, HoldSeconds: &huge, MaxHoldSeconds: &huge}, concurrencySettings{true, 64, time.Hour, time.Hour}},
		"negative":       {&ConcurrencyConfig{MaxInFlight: -3, HoldSeconds: &negative, MaxHoldSeconds: &negative}, concurrencySettings{true, 4, 0, 0}},
		"hold off":       {&ConcurrencyConfig{HoldSeconds: &zero}, concurrencySettings{true, 4, 0, 300 * time.Second}},
		"max below hold": {&ConcurrencyConfig{MaxHoldSeconds: &zero}, concurrencySettings{true, 4, 60 * time.Second, 60 * time.Second}},
	}
	for name, tc := range cases {
		if got := (Config{Concurrency: tc.policy}).concurrencySettings(); got != tc.want {
			t.Errorf("%s: resolved %+v, want %+v", name, got, tc.want)
		}
	}
	small, large := 100, 200
	for name, policy := range map[string]*ConcurrencyConfig{
		"huge limit": {MaxInFlight: 65}, "negative limit": {MaxInFlight: -1},
		"huge hold": {HoldSeconds: &huge}, "negative max": {MaxHoldSeconds: &negative},
		"max below hold": {HoldSeconds: &large, MaxHoldSeconds: &small},
	} {
		if validateConcurrencyConfig(policy) == nil {
			t.Errorf("%s passed validation", name)
		}
	}
	for name, policy := range map[string]*ConcurrencyConfig{
		"absent": nil, "empty": {}, "edge": {MaxInFlight: 64, HoldSeconds: &zero, MaxHoldSeconds: &zero},
		"max equals hold": {HoldSeconds: &small, MaxHoldSeconds: &small},
	} {
		if err := validateConcurrencyConfig(policy); err != nil {
			t.Errorf("%s failed validation: %v", name, err)
		}
	}
	settings := concurrencySettings{true, 4, time.Hour, time.Hour}
	if got := settings.holdFor(1<<30, 1<<62); got != time.Hour {
		t.Fatalf("holdFor with absurd inputs = %v, want the maximum", got)
	}
}

// /runtime shows what the gate is doing to each account.
func TestRuntimeReportsConcurrencyPerAccount(t *testing.T) {
	s, host, id, clock := gateFixture(t)
	other := addCredential(t, s, host, "secondary", "sk-amr-exec-0002")
	running := []*quotaInFlightLease{gateAdmit(t, s, id), gateAdmit(t, s, id)}
	gateReject(s, gateAdmit(t, s, other))
	read := func() (map[string]any, map[string]map[string]any) {
		_, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/runtime", nil, nil)
		top, _ := body["concurrency"].(map[string]any)
		rows := map[string]map[string]any{}
		for _, item := range body["credentials"].([]any) {
			row := item.(map[string]any)
			view, _ := row["concurrency"].(map[string]any)
			if view == nil {
				t.Fatalf("runtime row has no concurrency object: %s", raw)
			}
			rows[row["id"].(string)] = view
		}
		return top, rows
	}
	top, rows := read()
	if top["enabled"] != true || top["max_in_flight"] != float64(4) {
		t.Fatalf("runtime concurrency = %v", top)
	}
	if rows[id]["in_flight"] != float64(2) || rows[id]["peak_in_flight"] != float64(2) || rows[id]["held_until"] != nil || rows[id]["probing"] != false {
		t.Fatalf("running account = %v", rows[id])
	}
	until, _ := rows[other]["held_until"].(string)
	if want := clock.Now().Add(60 * time.Second).Format(time.RFC3339); until != want {
		t.Fatalf("held_until = %q, want %q", until, want)
	}
	if rows[other]["consecutive_rejections"] != float64(1) || rows[other]["in_flight"] != float64(0) || rows[other]["peak_in_flight"] != float64(1) {
		t.Fatalf("held account = %v", rows[other])
	}
	if got := gateStats(t, s)["held_now"]; got != float64(1) {
		t.Fatalf("held_now = %v, want 1", got)
	}
	clock.advance(61 * time.Second)
	_, rows = read()
	if rows[other]["held_until"] != nil || rows[other]["probing"] != true {
		t.Fatalf("after the hold the account reads %v, want probing", rows[other])
	}
	for _, lease := range running {
		gateAccept(s, lease)
	}
	_, rows = read()
	if rows[id]["in_flight"] != float64(0) || rows[id]["peak_in_flight"] != float64(2) {
		t.Fatalf("idle account = %v, want in_flight 0 and the peak kept", rows[id])
	}
}

// Replacing an account's key starts it clean, and the old key's bookkeeping
// does not accumulate.
func TestConcurrencyStateDoesNotOutliveAReplacedKey(t *testing.T) {
	s, _, id, _ := gateFixture(t)
	gateReject(s, gateAdmit(t, s, id))
	_ = gateRefused(t, s, id)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "api_key": "sk-amr-exec-9999"}); status != http.StatusOK {
		t.Fatalf("replace key: %d %s", status, raw)
	}
	// The new key is a different upstream identity: it does not inherit the hold.
	gateAccept(s, gateAdmit(t, s, id))
	s.mu.Lock()
	before := len(s.concurrencyStates)
	s.mu.Unlock()
	if before != 1 {
		t.Fatalf("%d hold entries before the sweep, want the old key's one", before)
	}
	if status, _, raw := managementCall(t, s, http.MethodGet, apiBase+"/runtime", nil, nil); status != http.StatusOK {
		t.Fatalf("runtime: %d %s", status, raw)
	}
	s.mu.Lock()
	peaks, states := len(s.concurrencyPeak), len(s.concurrencyStates)
	_, oldPeak := s.concurrencyPeak[quotaPauseKeyHash("sk-amr-exec-0001")]
	s.mu.Unlock()
	if peaks != 1 || oldPeak {
		t.Fatalf("peak entries = %d (old key present: %v), want only the live key", peaks, oldPeak)
	}
	if states != 0 {
		t.Fatalf("%d hold entries survive for a key no account uses", states)
	}
}

// Many goroutines hammer one account through the real executor path. However
// they interleave, the upstream never sees more than the limit at once, every
// request either succeeds or is refused locally, and no counter leaks.
func TestConcurrencyGateNeverExceedsTheLimitUnderLoad(t *testing.T) {
	s, host, id, _ := gateFixture(t)
	const limit = 3
	setConcurrency(t, s, map[string]any{"max_in_flight": limit})
	var running, peak, succeeded, refused, other atomic.Int64
	host.doDefault = &fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"id":"c","object":"chat.completion","model":"opendesign-large","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)}
	host.doHook = func() {
		now := running.Add(1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(200 * time.Microsecond)
		running.Add(-1)
	}
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	var wg sync.WaitGroup
	for worker := 0; worker < 24; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				raw, _ := json.Marshal(executorPayload{AuthID: id, Model: "od-large", Payload: body})
				_, err := s.Handle("executor.execute", raw)
				switch {
				case err == nil:
					succeeded.Add(1)
				case streamErrorCode(err) == concurrencyLimitKind:
					refused.Add(1)
				default:
					other.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	s.waitIdle()
	if got := peak.Load(); got > limit {
		t.Fatalf("the upstream saw %d requests at once, limit is %d", got, limit)
	}
	if other.Load() != 0 {
		t.Fatalf("%d requests ended in something other than success or a local refusal", other.Load())
	}
	if succeeded.Load() == 0 || refused.Load() == 0 {
		t.Fatalf("succeeded=%d refused=%d: the test did not exercise both outcomes", succeeded.Load(), refused.Load())
	}
	if succeeded.Load()+refused.Load() != 24*40 {
		t.Fatalf("accounted for %d of %d requests", succeeded.Load()+refused.Load(), 24*40)
	}
	if n := inFlightTotal(s); n != 0 {
		t.Fatalf("%d in-flight counters leaked", n)
	}
	if got := gateStats(t, s)["rejected_at_limit"]; got != float64(refused.Load()) {
		t.Fatalf("rejected_at_limit = %v, executor saw %d refusals", got, refused.Load())
	}
	s.mu.Lock()
	recorded := s.concurrencyPeak[quotaPauseKeyHash("sk-amr-exec-0001")]
	s.mu.Unlock()
	if recorded > limit || int64(recorded) < peak.Load() {
		t.Fatalf("recorded peak %d, upstream saw %d, limit %d", recorded, peak.Load(), limit)
	}
}

// The explicit model test goes through the same gate and names the reason
// instead of reporting an upstream failure.
func TestModelTestNamesAConcurrencyRefusal(t *testing.T) {
	for kind, want := range map[string]string{
		concurrencyLimitKind: "at its concurrency limit",
		concurrencyHeldKind:  "held after an upstream rate limit",
	} {
		if got := modelTestErrorMessage(kind); !strings.Contains(got, want) || !strings.Contains(got, "nothing was sent upstream") {
			t.Errorf("model test message for %s = %q", kind, got)
		}
		if !safeDiagnosticKind(kind, nil) {
			t.Errorf("%s is not a reportable diagnostic kind", kind)
		}
	}
}

// Settling one request twice must count it once. Both executor paths have more
// than one place that can reach the terminal outcome.
func TestConcurrencySettlingTwiceCountsOnce(t *testing.T) {
	s, _, id, _ := gateFixture(t)
	lease := gateAdmit(t, s, id)
	first, second := upstream429(""), upstream429("")
	s.settleConcurrency(lease, first)
	s.settleConcurrency(lease, second)
	s.settleConcurrency(lease, nil)
	lease.release(s)
	lease.release(s)
	if HostRetryAfterSeconds(first) != 61 || HostRetryAfterSeconds(second) != 0 {
		t.Fatalf("retry hints = %d and %d, want 61 and none", HostRetryAfterSeconds(first), HostRetryAfterSeconds(second))
	}
	stats := gateStats(t, s)
	if stats["upstream_rate_limited"] != float64(1) || stats["holds_started"] != float64(1) {
		t.Fatalf("stats = %v, want exactly one rate limit and one hold", stats)
	}
	if n := inFlightTotal(s); n != 0 {
		t.Fatalf("%d in-flight counters after a double release", n)
	}
}

// A probe that fails for an unrelated reason frees its slot when it is
// released, even while an older request of the same account is still running.
// Without that the account would stay shut until the old request finished.
func TestConcurrencyProbeSlotIsFreedWhileAnOlderRequestRuns(t *testing.T) {
	s, _, id, clock := gateFixture(t)
	old := gateAdmit(t, s, id)
	gateReject(s, gateAdmit(t, s, id))
	clock.advance(61 * time.Second)
	probe := gateAdmit(t, s, id)
	if probe.gate.probe == 0 {
		t.Fatal("the request after the hold is not the probe")
	}
	_ = gateRefused(t, s, id)
	s.settleConcurrency(probe, upstreamTransportError(os.ErrDeadlineExceeded))
	probe.release(s)
	// The old request is still in flight, so nothing but the release could
	// have freed the probe slot.
	next := gateAdmit(t, s, id)
	if next.gate.probe == 0 {
		t.Fatal("the request after a failed probe is not the next probe")
	}
	gateAccept(s, next)
	gateAccept(s, old)
	if n := inFlightTotal(s); n != 0 {
		t.Fatalf("%d in-flight counters leaked", n)
	}
}

// With holding turned off a leftover entry must not refuse anything.
func TestConcurrencyLeftoverHoldIsIgnoredWhenHoldingIsOff(t *testing.T) {
	s, _, id, clock := gateFixture(t)
	setConcurrency(t, s, map[string]any{"hold_seconds": 0, "max_hold_seconds": 0})
	s.mu.Lock()
	s.concurrencyStates[quotaPauseKeyHash("sk-amr-exec-0001")] = &concurrencyState{holdUntil: clock.Now().Add(time.Hour), strikes: 3, probe: 7}
	s.mu.Unlock()
	leases := []*quotaInFlightLease{gateAdmit(t, s, id), gateAdmit(t, s, id), gateAdmit(t, s, id), gateAdmit(t, s, id)}
	if err := gateRefused(t, s, id); streamErrorCode(err) != concurrencyLimitKind {
		t.Fatalf("fifth request = %v, want only the limit to apply", err)
	}
	for _, lease := range leases {
		gateAccept(s, lease)
	}
}

// A hand-edited settings.json with an out-of-range policy must not keep the
// plugin from loading: that would take every account offline over one number.
// The values are clamped, and a save through the API is still refused.
func TestOutOfRangeConcurrencyInSettingsDoesNotBlockLoading(t *testing.T) {
	dir := t.TempDir()
	settings := `{"data_dir":"` + dir + `","base_url":"` + DefaultBaseURL + `","models":[],` +
		`"concurrency":{"enabled":true,"max_in_flight":5000,"hold_seconds":-9,"max_hold_seconds":999999}}`
	if err := os.WriteFile(filepath.Join(dir, settingsFile), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewService()
	s.SetHost(newFakeHost().call)
	registrar(t, s, dir)
	t.Cleanup(func() { _, _ = s.quiesce() })
	got := s.config().concurrencySettings()
	if want := (concurrencySettings{true, concurrencyMaxInFlightLimit, 0, time.Hour}); got != want {
		t.Fatalf("resolved policy = %+v, want %+v", got, want)
	}
	// Saving without correcting the stored value is refused, and names the field.
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/config", nil, map[string]any{"concurrency": map[string]any{"enabled": true}})
	if status != http.StatusBadRequest || !strings.Contains(raw, "max_in_flight") {
		t.Fatalf("re-saving an out-of-range stored policy: %d %s", status, raw)
	}
	// Supplying valid values replaces it.
	view := setConcurrency(t, s, map[string]any{"max_in_flight": 4, "hold_seconds": 60, "max_hold_seconds": 300})
	if view["max_in_flight"] != float64(4) || view["hold_seconds"] != float64(60) {
		t.Fatalf("after correcting the policy: %v", view)
	}
}

// The cooldown is sent to the host only next to the 429 it belongs to. The
// status passed in is the one the host is given, which may have been
// neutralized to 0 after the hold was attached.
func TestHostRetryAfterOnlyAccompaniesA429(t *testing.T) {
	s, _, id, _ := gateFixture(t)
	lease := gateAdmit(t, s, id)
	held := upstream429("")
	s.settleConcurrency(lease, held)
	lease.release(s)
	for status, want := range map[int]int{
		http.StatusTooManyRequests:     61,
		0:                              0,
		http.StatusBadGateway:          0,
		http.StatusBadRequest:          0,
		http.StatusServiceUnavailable:  0,
		http.StatusInternalServerError: 0,
	} {
		if got := HostRetryAfterFor(held, status); got != want {
			t.Errorf("status %d: retry-after %d, want %d", status, got, want)
		}
	}
	if got := HostRetryAfterFor(upstream429(""), http.StatusTooManyRequests); got != 0 {
		t.Fatalf("an error the gate never held carries a retry-after of %d", got)
	}
	if got := HostRetryAfterFor(nil, http.StatusTooManyRequests); got != 0 {
		t.Fatalf("a nil error carries a retry-after of %d", got)
	}
}

// A probe that was accepted is no longer the probe, even while its request is
// still running. If the account is rejected again later, the next probe must
// not be refused because of the first one's stale marker.
func TestConcurrencyAcceptedProbeDoesNotBlockTheNextProbe(t *testing.T) {
	s, _, id, clock := gateFixture(t)
	gateReject(s, gateAdmit(t, s, id))
	clock.advance(61 * time.Second)
	longRunning := gateAdmit(t, s, id)
	if longRunning.gate.probe == 0 {
		t.Fatal("the request after the hold is not the probe")
	}
	// Accepted (a stream opened) but not finished: it keeps running below.
	s.concurrencyAccepted(longRunning)
	// The account is open again, and then gets rejected a second time.
	gateReject(s, gateAdmit(t, s, id))
	clock.advance(61 * time.Second)
	next := gateAdmit(t, s, id)
	if next.gate.probe == 0 {
		t.Fatal("the request after the second hold is not a probe")
	}
	gateAccept(s, next)
	longRunning.release(s)
	if n := inFlightTotal(s); n != 0 {
		t.Fatalf("%d in-flight counters leaked", n)
	}
}

// held_now counts accounts whose hold is still running, not every account that
// still carries an entry: one waiting for its probe is not held any more.
func TestConcurrencyHeldNowCountsOnlyRunningHolds(t *testing.T) {
	s, host, id, clock := gateFixture(t)
	other := addCredential(t, s, host, "secondary", "sk-amr-exec-0002")
	gateReject(s, gateAdmit(t, s, id))
	if got := gateStats(t, s)["held_now"]; got != float64(1) {
		t.Fatalf("held_now = %v right after a rejection, want 1", got)
	}
	clock.advance(30 * time.Second)
	gateReject(s, gateAdmit(t, s, other))
	if got := gateStats(t, s)["held_now"]; got != float64(2) {
		t.Fatalf("held_now = %v with two running holds, want 2", got)
	}
	// The first hold (60 s) is over, the second (started 30 s later) is not.
	clock.advance(31 * time.Second)
	if got := gateStats(t, s)["held_now"]; got != float64(1) {
		t.Fatalf("held_now = %v after the first hold expired, want 1", got)
	}
	s.mu.Lock()
	entries := len(s.concurrencyStates)
	s.mu.Unlock()
	if entries != 2 {
		t.Fatalf("%d entries, want both kept: the expired one still needs its probe", entries)
	}
}

// A partial update is judged against what it resolves to, including the
// defaults of the fields it does not mention and that were never saved.
func TestConcurrencyPartialUpdateIsCheckedAgainstTheDefaults(t *testing.T) {
	s, _ := newTestService(t)
	for name, policy := range map[string]map[string]any{
		"hold above the default maximum": {"hold_seconds": 400},
		"maximum below the default hold": {"max_hold_seconds": 30},
	} {
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/config", nil, map[string]any{"concurrency": policy})
		if status != http.StatusBadRequest || !strings.Contains(raw, "max_hold_seconds") {
			t.Errorf("%s: status %d, want 400 naming max_hold_seconds: %s", name, status, raw)
		}
	}
	if got := s.config().concurrencySettings(); got.hold != 60*time.Second || got.maxHold != 300*time.Second {
		t.Fatalf("a refused partial update changed the policy: %+v", got)
	}
	// Both together are fine, and so is a hold at exactly the default maximum.
	setConcurrency(t, s, map[string]any{"hold_seconds": 400, "max_hold_seconds": 400})
	s2, _ := newTestService(t)
	setConcurrency(t, s2, map[string]any{"hold_seconds": 300})
}

// A probe marker with nothing in flight cannot belong to a running request.
// It must not keep the account shut; the next request becomes the probe.
func TestConcurrencyStaleProbeMarkerDoesNotShutTheAccount(t *testing.T) {
	s, _, id, clock := gateFixture(t)
	s.mu.Lock()
	s.concurrencyStates[quotaPauseKeyHash("sk-amr-exec-0001")] = &concurrencyState{
		holdUntil: clock.Now().Add(-time.Second), strikes: 2, probe: 99,
	}
	s.mu.Unlock()
	probe := gateAdmit(t, s, id)
	if probe.gate.probe == 0 || probe.gate.probe == 99 {
		t.Fatalf("probe token = %d, want a fresh one", probe.gate.probe)
	}
	gateAccept(s, probe)
}

// host.stream.close carries the cooldown only when the status it reports is the
// 429 the cooldown belongs to. A stream that opened with 200 and then reported a
// rate limit is closed as 502, and that must not carry one.
func TestStreamCloseCarriesTheCooldownOnlyNextToA429(t *testing.T) {
	detailFor := func(failure error) map[string]any {
		t.Helper()
		s, host := newTestService(t)
		s.finishOutputStream("client-1", failure)
		_, detail := closeDetail(t, host)
		return detail
	}
	held := upstream429("")
	var apiErr *APIError
	if !errorsAs(held, &apiErr) {
		t.Fatal("fixture error is not an APIError")
	}
	apiErr.hostRetryAfter = 61 * time.Second
	detail := detailFor(held)
	if detail["http_status"] != http.StatusTooManyRequests || detail["retry_after_seconds"] != 61 {
		t.Fatalf("429 close detail = %v, want http_status 429 with retry_after_seconds 61", detail)
	}
	if got := detailFor(upstream429("")); got["http_status"] != http.StatusTooManyRequests {
		t.Fatalf("429 close detail = %v", got)
	} else if _, present := got["retry_after_seconds"]; present {
		t.Fatalf("a 429 the gate never held carries a cooldown: %v", got)
	}
	inBody := upstreamErrorFromResponseObserved(http.StatusBadGateway, http.StatusOK, nil, []byte(`{"error":{"code":"rate_limit_exceeded"}}`))
	if !errorsAs(inBody, &apiErr) {
		t.Fatal("fixture error is not an APIError")
	}
	apiErr.hostRetryAfter = 61 * time.Second
	detail = detailFor(inBody)
	if detail["http_status"] != http.StatusBadGateway {
		t.Fatalf("close detail status = %v, want 502", detail["http_status"])
	}
	if _, present := detail["retry_after_seconds"]; present {
		t.Fatalf("a 502 close carries a cooldown: %v", detail)
	}
}

func errorsAs(err error, target **APIError) bool {
	typed, ok := err.(*APIError)
	if ok {
		*target = typed
	}
	return ok
}
