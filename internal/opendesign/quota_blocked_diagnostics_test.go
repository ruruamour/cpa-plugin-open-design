package opendesign

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// pausedFixture returns a service whose only credential is quota-paused and has
// one saved model mapping ("od-large" -> "opendesign-large").
func pausedFixture(t *testing.T) (*Service, *fakeHost, string) {
	t.Helper()
	s, host := newQuotaPauseTestService(t)
	id := seedModels(t, s, host)
	quotaFixtureAuthFile(t, s, host, id, nil)
	c, _ := liveCredential(s, id)
	s.observeQuotaBlock(c, "opendesign-large", &APIError{Status: http.StatusPaymentRequired, Kind: "insufficient_balance", quotaExhausted: true, quotaResetAt: time.Now().Add(26 * time.Hour)})
	if paused, _ := liveCredential(s, id); !paused.quotaBlocked() {
		t.Fatal("fixture credential is not paused")
	}
	return s, host, id
}

func upstreamCalls(host *fakeHost) int {
	return host.countMethod("host.http.do") + host.countMethod("host.http.do_stream")
}

// A request refused locally because its account is quota-paused never reaches
// the upstream. The log used to record it as "upstream_error", which told the
// operator the provider had failed. Both executor entry points must keep the
// plugin's own kind.
func TestQuotaBlockedRejectionIsLoggedAsItsOwnKind(t *testing.T) {
	s, host, id := pausedFixture(t)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body}); streamErrorCode(err) != "quota_blocked" {
		t.Fatalf("buffered executor error = %v, want quota_blocked", err)
	}
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: body, Stream: true, StreamID: "client-1"}); streamErrorCode(err) != "quota_blocked" {
		t.Fatalf("stream executor error = %v, want quota_blocked", err)
	}
	s.waitIdle()
	if n := upstreamCalls(host); n != 0 {
		t.Fatalf("a paused account reached the upstream %d time(s)", n)
	}
	_, logs, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-logs", nil, nil)
	entries, _ := logs["logs"].([]any)
	if len(entries) != 2 {
		t.Fatalf("want 2 log rows, got %d: %s", len(entries), raw)
	}
	streams := map[bool]bool{}
	for _, item := range entries {
		entry := item.(map[string]any)
		streams[entry["stream"] == true] = true
		if entry["error_kind"] != "quota_blocked" {
			t.Errorf("logged error_kind = %v, want quota_blocked (stream=%v)", entry["error_kind"], entry["stream"])
		}
		if entry["upstream_attempted"] != false {
			t.Errorf("a local rejection was logged as attempted: %v", entry["upstream_attempted"])
		}
		if entry["http_status"] != nil {
			t.Errorf("a local rejection carries an upstream HTTP status: %v", entry["http_status"])
		}
		if entry["status"] != "error" {
			t.Errorf("status = %v, want error", entry["status"])
		}
	}
	if !streams[true] || !streams[false] {
		t.Fatalf("expected one buffered and one stream row: %s", raw)
	}
	// The kind is searchable, so an operator can list exactly these rejections.
	_, filtered, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-logs", map[string][]string{"q": {"quota_blocked"}}, nil)
	if matched, _ := filtered["logs"].([]any); len(matched) != 2 {
		t.Fatalf("searching quota_blocked matched %d rows: %s", len(matched), raw)
	}
	_, stats, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-stats", nil, nil)
	if stats["local_rejected"] != float64(2) {
		t.Fatalf("local_rejected = %v, want 2: %s", stats["local_rejected"], raw)
	}
}

// The persisted row must survive a restart with its kind intact: the same
// allowlist runs again when the snapshot and journal are loaded.
func TestQuotaBlockedLogKindSurvivesReload(t *testing.T) {
	s, host, id := pausedFixture(t)
	_, _ = execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	s.waitIdle()
	dir := s.config().DataDir
	if _, err := s.quiesce(); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	reloaded := NewService()
	reloaded.SetHost(host.call)
	registrar(t, reloaded, dir)
	t.Cleanup(func() { _, _ = reloaded.quiesce() })
	_, logs, raw := managementCall(t, reloaded, http.MethodGet, apiBase+"/request-logs", nil, nil)
	entries, _ := logs["logs"].([]any)
	if len(entries) != 1 {
		t.Fatalf("want 1 reloaded row, got %d: %s", len(entries), raw)
	}
	if kind := entries[0].(map[string]any)["error_kind"]; kind != "quota_blocked" {
		t.Fatalf("reloaded error_kind = %v, want quota_blocked", kind)
	}
}

// The explicit model test goes through the same executor entry. For a paused
// account it must say so instead of reporting an upstream failure that never
// happened.
func TestModelTestOnPausedAccountReportsThePause(t *testing.T) {
	s, host, id := pausedFixture(t)
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
		"credential_id": id, "model_id": "od-large",
	})
	if status != http.StatusOK {
		t.Fatalf("model-test status = %d body = %s", status, raw)
	}
	if n := upstreamCalls(host); n != 0 {
		t.Fatalf("a model test on a paused account reached the upstream %d time(s)", n)
	}
	if body["status"] != "error" || body["upstream_attempted"] != false || body["real_inference"] != false {
		t.Fatalf("paused model test claimed an upstream attempt: %s", raw)
	}
	failure, _ := body["error"].(map[string]any)
	if failure["kind"] != "quota_blocked" {
		t.Fatalf("model-test error kind = %v, want quota_blocked: %s", failure["kind"], raw)
	}
	message, _ := failure["message"].(string)
	if !strings.Contains(message, "paused") || strings.Contains(message, "upstream request failed") {
		t.Fatalf("model-test message does not describe the pause: %q", message)
	}
	if body["http_status"] != nil {
		t.Fatalf("a local rejection carries an upstream HTTP status: %s", raw)
	}
}
