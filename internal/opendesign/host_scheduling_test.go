package opendesign

import (
	"net/http"
	"strings"
	"testing"
)

// hostWire is the status and message the host actually schedules on.
func hostWire(s *Service, err error) (string, int) {
	return s.HostErrorDetails(err)
}

// A matched rule must reach the host verbatim: its conductor applies the action
// before its own status classification, so rewriting the status would silently
// drop the operator's continue/stop/cooldown choice.
func TestHostSchedulingForwardsMatchedRuleVerbatim(t *testing.T) {
	s, _ := newTestService(t)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"tier limit", upstreamErrorFromResponse(http.StatusBadRequest, nil,
			[]byte(`{"message":"[code=tier_limit_exceeded] membership concurrency limit exceeded: 11/10","type":"invalid_request_error"}`))},
		{"spent balance", upstreamErrorFromResponse(http.StatusPaymentRequired, nil,
			[]byte(`{"error":{"code":"insufficient_balance","message":"coding plan exhausted"}}`))},
		{"invalid key", upstreamErrorFromResponse(http.StatusForbidden, nil,
			[]byte(`{"error":{"code":"invalid_api_key","message":"api key not found"}}`))},
		{"overloaded", upstreamErrorFromResponse(http.StatusServiceUnavailable, nil,
			[]byte(`{"error":{"code":"server_overloaded","message":"overloaded"}}`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, status := hostWire(s, tc.err)
			if status != statusOf(tc.err) || message != redact(tc.err.Error(), s.allKeys()...) {
				t.Fatalf("wire = (%d, %q), want the original (%d, %q)", status, message, statusOf(tc.err), tc.err.Error())
			}
			if strings.Contains(message, hostTransportHint) {
				t.Fatalf("rule match was rewritten as a transport failure: %q", message)
			}
		})
	}
}

// An operator rule that replaces the built-in policy must win too, including a
// regex match, and a rule for another status must not.
func TestHostSchedulingHonorsOperatorRules(t *testing.T) {
	s, _ := newTestService(t)
	s.mu.Lock()
	s.errorRules = []RequestErrorRule{{
		Status: http.StatusBadGateway, MatchRegexr: []string{`^\[upstream:upstream_protocol_error\]`}, Action: "stop",
	}}
	s.mu.Unlock()

	matched := upstreamProtocolError("upstream returned a non-JSON completion body")
	if message, status := hostWire(s, matched); status != http.StatusBadGateway || strings.Contains(message, hostTransportHint) {
		t.Fatalf("operator rule was not forwarded: status=%d message=%q", status, message)
	}
	other := upstreamTransportError(errString("dial tcp: connection refused"))
	if _, status := hostWire(s, other); status != 0 {
		t.Fatalf("unmatched 502 kept status %d; the host would cool the account for a minute", status)
	}
}

// Empty rules mean the operator opted out, so even a built-in marker must not
// be treated as a rule hit. The credential-owned status still stands on its own.
func TestHostSchedulingEmptyPolicyDropsBuiltinRules(t *testing.T) {
	s, _ := newTestService(t)
	s.mu.Lock()
	s.errorRules = nil
	s.mu.Unlock()

	overloaded := upstreamErrorFromResponse(http.StatusServiceUnavailable, nil,
		[]byte(`{"error":{"code":"server_overloaded","message":"overloaded"}}`))
	if _, status := hostWire(s, overloaded); status != 0 {
		t.Fatalf("empty policy still forwarded a built-in rule, status %d", status)
	}
	banned := upstreamErrorFromResponse(http.StatusUnauthorized, nil, []byte(`{"error":{"message":"nope"}}`))
	if _, status := hostWire(s, banned); status != http.StatusUnauthorized {
		t.Fatalf("401 status = %d, want the credential cooldown preserved", status)
	}
}

// These are the failures production misclassified. Each must rotate without a
// cooldown instead of pinning the request or parking the account.
func TestHostSchedulingNeutralizesUnrecognizedFailures(t *testing.T) {
	s, _ := newTestService(t)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"banned key", upstreamErrorFromResponse(http.StatusForbidden, nil,
			[]byte(`{"error":{"code":"forbidden_api_key","message":"[code=forbidden_api_key] api key owner banned"}}`))},
		{"bare 402", upstreamErrorFromResponse(http.StatusPaymentRequired, nil,
			[]byte(`{"error":{"message":"payment required"}}`))},
		{"cloudflare timeout", upstreamErrorFromResponse(524, nil, []byte(`<html>error code: 524</html>`))},
		{"bare 500", upstreamErrorFromResponse(http.StatusInternalServerError, nil,
			[]byte(`{"error":{"message":"internal failure"}}`))},
		{"transport", upstreamTransportError(errString("dial tcp: connection refused"))},
		{"non-json body", upstreamProtocolError("upstream returned a non-JSON completion body")},
		{"malformed frame", upstreamProtocolError("upstream returned an invalid JSON frame")},
		{"local request", localFail(http.StatusBadRequest, "invalid_request", "messages must be a nonempty array")},
		{"client disconnect", &APIError{Status: http.StatusBadRequest, Kind: "request_canceled", Message: "client disconnected"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, status := hostWire(s, tc.err)
			if status != 0 {
				t.Fatalf("status = %d, want none so the host neither cools nor pins the account", status)
			}
			if !strings.Contains(message, hostTransportHint) {
				t.Fatalf("message %q lacks the transient transport hint", message)
			}
			if !strings.Contains(message, redact(tc.err.Error(), s.allKeys()...)) {
				t.Fatalf("message %q dropped the original diagnostic", message)
			}
		})
	}
}

// A credential-owned failure keeps its status when no rule claims it, so the
// host's own cooldown and model rotation still apply.
func TestHostSchedulingPreservesCredentialFaults(t *testing.T) {
	s, _ := newTestService(t)
	s.mu.Lock()
	s.errorRules = nil
	s.mu.Unlock()
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"unknown 401", upstreamErrorFromResponse(http.StatusUnauthorized, nil, []byte(`{"error":{"message":"nope"}}`)), http.StatusUnauthorized},
		{"unknown 429", upstreamErrorFromResponse(http.StatusTooManyRequests, nil, []byte(`{"error":{"message":"slow down"}}`)), http.StatusTooManyRequests},
		{"missing model", upstreamErrorFromResponse(http.StatusBadRequest, nil,
			[]byte(`{"error":{"code":"model_not_found","message":"no such model"}}`)), http.StatusBadRequest},
		{"context length", upstreamErrorFromResponse(http.StatusBadRequest, nil,
			[]byte(`{"error":{"code":"context_length_exceeded","message":"too long"}}`)), http.StatusBadRequest},
		{"quota block", localFail(http.StatusTooManyRequests, "quota_blocked", quotaBlockMarker+" upstream account quota is temporarily paused"), http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, status := hostWire(s, tc.err)
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			if strings.Contains(message, hostTransportHint) {
				t.Fatalf("credential fault was neutralized: %q", message)
			}
		})
	}
}

// The transport hint is added once, even when the upstream text already says it.
func TestHostSchedulingTransportHintIsIdempotent(t *testing.T) {
	s, _ := newTestService(t)
	err := upstreamErrorFromResponse(http.StatusBadGateway, nil, []byte("server misbehaving"))
	message, status := hostWire(s, err)
	if status != 0 {
		t.Fatalf("status = %d, want none", status)
	}
	if strings.Count(strings.ToLower(message), hostTransportHint) != 1 {
		t.Fatalf("hint repeated: %q", message)
	}
}

// Rewriting what the host sees must not change what the plugin records: the
// request log keeps the real upstream status and the original error kind.
func TestHostSchedulingDoesNotChangeRequestLog(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusForbidden,
		Body:   []byte(`{"error":{"code":"forbidden_api_key","message":"[code=forbidden_api_key] api key owner banned"}}`),
	})
	_, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	if err == nil {
		t.Fatal("banned key was accepted")
	}
	if _, status := hostWire(s, err); status != 0 {
		t.Fatalf("host status = %d, want none", status)
	}
	logs, total, _ := s.requestLogs.query(requestLogFilter{limit: 10, status: "error"})
	if total != 1 || len(logs) != 1 || logs[0].HTTPStatus == nil || *logs[0].HTTPStatus != http.StatusForbidden {
		t.Fatalf("log = %#v total=%d, want the real upstream 403", logs, total)
	}
	if logs[0].ErrorKind == nil || *logs[0].ErrorKind != "upstream_error" {
		t.Fatalf("error kind = %v, want the original classification", logs[0].ErrorKind)
	}
}
