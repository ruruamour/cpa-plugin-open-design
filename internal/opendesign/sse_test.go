package opendesign

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSSEDecoderReassemblesAcrossChunks(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   []string
	}{
		{
			name:   "single event",
			chunks: []string{"data: {\"a\":1}\n\n"},
			want:   []string{`{"a":1}`},
		},
		{
			name:   "split mid json",
			chunks: []string{"data: {\"a\":", "1}\n\n"},
			want:   []string{`{"a":1}`},
		},
		{
			name:   "split between crlf and blank line",
			chunks: []string{"data: {\"a\":1}\r", "\n\r\n"},
			want:   []string{`{"a":1}`},
		},
		{
			name:   "two events in one chunk plus trailing partial",
			chunks: []string{"data: one\n\ndata: two\n\ndata: thr", "ee\n\n"},
			want:   []string{"one", "two", "three"},
		},
		{
			name:   "comments and keepalives are ignored",
			chunks: []string{": keepalive\n\n", "data: x\n\n"},
			want:   []string{"x"},
		},
		{
			name:   "multi-line data is joined with newlines",
			chunks: []string{"data: line1\ndata: line2\n\n"},
			want:   []string{"line1\nline2"},
		},
		{
			name:   "done marker",
			chunks: []string{"data: [DONE]\n\n"},
			want:   []string{"[DONE]"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &sseDecoder{}
			var got []string
			for _, chunk := range tc.chunks {
				if err := d.feed([]byte(chunk), func(e sseEvent) error {
					got = append(got, string(e.Data))
					return nil
				}); err != nil {
					t.Fatalf("feed(%q): %v", chunk, err)
				}
			}
			if err := d.end(); err != nil {
				t.Fatalf("end: %v", err)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("events = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSSEDecoderReportsTruncatedFrame(t *testing.T) {
	d := &sseDecoder{}
	if err := d.feed([]byte("data: {\"partial\":true"), func(sseEvent) error { return nil }); err != nil {
		t.Fatalf("feed: %v", err)
	}
	if err := d.end(); err == nil {
		t.Fatalf("truncated frame was accepted")
	}
}

func TestSSEDecoderEnforcesFrameLimit(t *testing.T) {
	d := &sseDecoder{maxBytes: 16}
	err := d.feed([]byte("data: 0123456789012345678901234567890"), func(sseEvent) error { return nil })
	if err == nil {
		t.Fatalf("oversized buffer was accepted")
	}
	if status := statusOf(err); status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
}

func TestBaseURLIsPinnedToOpenDesign(t *testing.T) {
	bad := []string{
		"https://api.openai.com/v1",
		"https://amr-link.open-design.ai/v2",
		"http://amr-link.open-design.ai/v1",
		"https://amr-link.open-design.ai/v1?x=1",
		"https://user:pass@amr-link.open-design.ai/v1",
	}
	for _, raw := range bad {
		if err := validateBaseURL(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if err := validateBaseURL(DefaultBaseURL); err != nil {
		t.Fatalf("rejected the documented base url: %v", err)
	}
	if err := validateBaseURL(DefaultBaseURL + "/"); err != nil {
		t.Fatalf("rejected the documented base url with a trailing slash: %v", err)
	}
}

func TestRedactRemovesKeysAndBearerTokens(t *testing.T) {
	const key = "sk-amr-super-secret-value"
	got := redact("Authorization: Bearer "+key+" failed; retry with "+key, key)
	if strings.Contains(got, key) {
		t.Fatalf("key survived redaction: %s", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("redaction marker missing: %s", got)
	}
	// A generic bearer token is scrubbed even when the key is unknown.
	if got := redact("failed with Bearer abc123def"); strings.Contains(got, "abc123def") {
		t.Fatalf("unknown bearer token survived: %s", got)
	}
}

func TestConfigValidationRejectsCollisionsAndEmptyIDs(t *testing.T) {
	cfg := defaultConfig()
	cfg.Models = []Model{{ID: "a", UpstreamID: "x"}, {ID: "a", UpstreamID: "y"}}
	if err := cfg.validate(); err == nil {
		t.Fatalf("duplicate alias accepted")
	}
	cfg = defaultConfig()
	cfg.Models = []Model{{ID: "", UpstreamID: "x"}}
	if err := cfg.validate(); err == nil {
		t.Fatalf("empty alias accepted")
	}
	cfg = defaultConfig()
	cfg.DataDir = "  "
	if err := cfg.validate(); err == nil {
		t.Fatalf("empty data dir accepted")
	}
	cfg = defaultConfig()
	cfg.BaseURL = "https://evil.example/v1"
	if err := cfg.validate(); err == nil {
		t.Fatalf("unexpected base url accepted")
	}
}

func TestCredentialJSONKeepsUnknownFields(t *testing.T) {
	raw := []byte(`{"type":"open-design","id":"x","label":"l","api_key":"k","priority":3,"proxy_url":"http://p"}`)
	var c Credential
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"priority":3`, `"proxy_url":"http://p"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("lost field %s in %s", want, encoded)
		}
	}
}

func TestRedactTruncatesWithoutBreakingUTF8(t *testing.T) {
	got := redact(strings.Repeat("错", 600))
	if !utf8.ValidString(got) {
		t.Fatalf("redaction produced invalid UTF-8")
	}
	if len([]rune(got)) != 500 {
		t.Fatalf("rune length = %d, want 500", len([]rune(got)))
	}
}
