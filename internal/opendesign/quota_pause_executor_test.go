package opendesign

import (
	"net/http"
	"testing"
	"time"
)

const quotaPauseFixtureBody = `{"error":{"code":"insufficient_balance","details":{"coding_plan_exhausted":true,"reset_at":"2099-01-01T00:00:00Z"}}}`

func TestQuotaPauseExecutorBufferedPaths(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantPause bool
	}{
		{"structured 402", 402, quotaPauseFixtureBody, true},
		{"structured 200 error", 200, quotaPauseFixtureBody, true},
		{"bare 402", 402, `{"error":{"message":"payment required"}}`, false},
		{"text marker only", 402, `{"error":{"message":"[code=insufficient_balance]"}}`, false},
		{"ordinary 429", 429, `{"error":{"code":"rate_limit_exceeded"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, host := newQuotaPauseTestService(t)
			id := seedModels(t, s, host)
			quotaFixtureAuthFile(t, s, host, id, nil)
			host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: tc.status, Body: []byte(tc.body)})
			req := executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
			if _, err := execute(t, s, req); err == nil {
				t.Fatal("fixture error was returned as a success")
			}
			c, _ := liveCredential(s, id)
			if c.quotaBlocked() != tc.wantPause {
				t.Fatalf("quota blocked = %t, want %t", c.quotaBlocked(), tc.wantPause)
			}
			if tc.wantPause {
				before := host.countMethod("host.http.do")
				if _, err := execute(t, s, req); statusOf(err) != http.StatusTooManyRequests {
					t.Fatalf("paused credential returned status %d", statusOf(err))
				}
				if host.countMethod("host.http.do") != before {
					t.Fatal("paused credential reached the upstream")
				}
			}
		})
	}
}

func TestQuotaPauseSuccessBodyHonorsRetryAfter(t *testing.T) {
	const body = `{"error":{"code":"insufficient_balance","details":{"coding_plan_exhausted":true}}}`
	for _, streaming := range []bool{false, true} {
		name := "buffered"
		if streaming {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			s, host := newQuotaPauseTestService(t)
			id := seedModels(t, s, host)
			quotaFixtureAuthFile(t, s, host, id, nil)
			headers := http.Header{"Retry-After": []string{"120"}}
			req := executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
			if streaming {
				req.StreamID = "quota-hint-stream"
				host.doStreams = append(host.doStreams, scriptedStream{status: 200, headers: headers, chunks: [][]byte{[]byte("data: " + body + "\n\n")}})
				if _, err := executeStream(t, s, req); err != nil {
					t.Fatalf("stream did not open: %v", err)
				}
				s.waitIdle()
			} else {
				host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: 200, Headers: headers, Body: []byte(body)})
				if _, err := execute(t, s, req); err == nil {
					t.Fatal("quota error was returned as success")
				}
			}
			current, _ := liveCredential(s, id)
			if !current.quotaBlocked() {
				t.Fatal("structured error did not create a quota block")
			}
			delay := current.QuotaBlock.RetryAt.Sub(current.QuotaBlock.BlockedAt)
			if delay < 115*time.Second || delay > 121*time.Second {
				t.Fatalf("Retry-After was lost: delay = %s", delay)
			}
		})
	}
}

func TestQuotaPauseExecutorStreamErrorPaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		chunks [][]byte
	}{
		{"HTTP 402", 402, [][]byte{[]byte(quotaPauseFixtureBody)}},
		{"SSE error", 200, [][]byte{[]byte("data: " + quotaPauseFixtureBody + "\n\n")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, host := newQuotaPauseTestService(t)
			id := seedModels(t, s, host)
			quotaFixtureAuthFile(t, s, host, id, nil)
			host.doStreams = append(host.doStreams, scriptedStream{status: tc.status, chunks: tc.chunks})
			_, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "fixture-stream", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
			if tc.status == 402 && err == nil {
				t.Fatal("HTTP 402 stream was accepted")
			}
			if tc.status == 200 && err != nil {
				t.Fatalf("SSE stream did not open: %v", err)
			}
			s.waitIdle()
			c, _ := liveCredential(s, id)
			if !c.quotaBlocked() {
				t.Fatal("stream quota error did not block the credential")
			}
		})
	}
}
