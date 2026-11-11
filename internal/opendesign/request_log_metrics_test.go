package opendesign

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func int64Pointer(value int64) *int64 { return &value }

func decodeFrame(t *testing.T, raw string) map[string]any {
	t.Helper()
	root, err := decodeObject([]byte(raw))
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return root
}

// The time to first output must not be satisfied by a frame that carries no
// output: the upstream opens every stream with a role-only delta and may send
// usage-only or finish-only frames.
func TestFrameCarriesOutputIgnoresFramesWithoutOutput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string
		want  bool
	}{
		{"role only", `{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`, false},
		{"role with empty content", `{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`, false},
		{"empty delta with finish", `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, false},
		{"usage only", `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`, false},
		{"no choices", `{"id":"c1","model":"m"}`, false},
		{"null content", `{"choices":[{"index":0,"delta":{"content":null}}]}`, false},
		{"empty tool call list", `{"choices":[{"index":0,"delta":{"tool_calls":[]}}]}`, false},
		{"text", `{"choices":[{"index":0,"delta":{"content":"hi"}}]}`, true},
		{"reasoning_content", `{"choices":[{"index":0,"delta":{"reasoning_content":"thinking"}}]}`, true},
		{"reasoning string", `{"choices":[{"index":0,"delta":{"reasoning":"thinking"}}]}`, true},
		{"reasoning details", `{"choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.text","text":"t"}]}}]}`, true},
		{"refusal", `{"choices":[{"index":0,"delta":{"refusal":"no"}}]}`, true},
		{"tool call", `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":""}}]}}]}`, true},
		{"legacy function call", `{"choices":[{"index":0,"delta":{"function_call":{"name":"f"}}}]}`, true},
		{"content parts", `{"choices":[{"index":0,"delta":{"content":[{"type":"text","text":"hi"}]}}]}`, true},
		{"second choice has output", `{"choices":[{"index":0,"delta":{}},{"index":1,"delta":{"content":"x"}}]}`, true},
		{"message shape", `{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`, true},
		{"completions text", `{"choices":[{"index":0,"text":"hi"}]}`, true},
	} {
		if got := frameCarriesOutput(decodeFrame(t, tc.frame)); got != tc.want {
			t.Errorf("%s: frameCarriesOutput = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func onlyLogRow(t *testing.T, s *Service) map[string]any {
	t.Helper()
	_, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-logs", nil, nil)
	rows, _ := body["logs"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want exactly one log row, got %d: %s", len(rows), raw)
	}
	return rows[0].(map[string]any)
}

// A stream records when its first output frame arrived, measured from the
// start of the request, together with the usage details the upstream reported.
func TestStreamLogRecordsFirstTokenAndUsageDetails(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	const gap = 60 * time.Millisecond
	// Read 0 is the role-only frame, read 1 the first output, read 2 more
	// output, read 3 the finish frame with usage, read 4 [DONE].
	host.readHook = func(read int) {
		if read == 1 || read == 3 {
			time.Sleep(gap)
		}
	}
	host.doStreams = append(host.doStreams, scriptedStream{status: http.StatusOK, chunks: [][]byte{
		[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n"),
		[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{"reasoning_content":"plan"}}]}` + "\n\n"),
		[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{"content":"hello"}}]}` + "\n\n"),
		[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":400,"total_tokens":1400,"prompt_tokens_details":{"cached_tokens":750},"completion_tokens_details":{"reasoning_tokens":120}}}` + "\n\n"),
		[]byte("data: [DONE]\n\n"),
	}})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-1", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	row := onlyLogRow(t, s)
	first, ok := row["first_token_ms"].(float64)
	if !ok {
		t.Fatalf("first_token_ms missing or not a number: %#v", row["first_token_ms"])
	}
	duration, _ := row["duration_ms"].(float64)
	// The role-only frame at read 0 must not have counted: the first output
	// arrived after the first gap, and the stream ended after the second.
	if first < float64(gap.Milliseconds()) {
		t.Fatalf("first_token_ms = %v, want at least %v: a frame without output was counted", first, gap.Milliseconds())
	}
	if duration < 2*float64(gap.Milliseconds()) || first > duration-float64(gap.Milliseconds())+5 {
		t.Fatalf("first_token_ms = %v with duration_ms = %v: first output must precede the second gap", first, duration)
	}
	if row["cached_tokens"] != float64(750) || row["reasoning_tokens"] != float64(120) {
		t.Fatalf("usage details = cached %v reasoning %v, want 750 / 120", row["cached_tokens"], row["reasoning_tokens"])
	}
	if row["input_tokens"] != float64(1000) || row["output_tokens"] != float64(400) || row["total_tokens"] != float64(1400) {
		t.Fatalf("usage totals changed: %v / %v / %v", row["input_tokens"], row["output_tokens"], row["total_tokens"])
	}
	// The row's rate is output over the window after the first output.
	rate, ok := row["output_tokens_per_second"].(float64)
	if window := duration - first; window >= requestLogMinRateWindowMS {
		if want := 400 * 1000 / window; !ok || math.Abs(rate-want) > 0.001 {
			t.Fatalf("output_tokens_per_second = %v, want %v", row["output_tokens_per_second"], want)
		}
	} else if row["output_tokens_per_second"] != nil {
		t.Fatalf("a %vms window produced a rate: %v", window, row["output_tokens_per_second"])
	}
}

// A buffered request has no stream, so it has no time to first output. The
// usage details still come from the response body.
func TestBufferedLogHasNoFirstTokenButKeepsUsageDetails(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(
		`{"model":"opendesign-large","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":200,"completion_tokens":20,"total_tokens":220,"prompt_cache_hit_tokens":150,"prompt_cache_miss_tokens":50}}`)})
	if _, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	row := onlyLogRow(t, s)
	if _, present := row["first_token_ms"]; present {
		t.Fatalf("a buffered request recorded first_token_ms: %v", row["first_token_ms"])
	}
	// DeepSeek-style flat field, used when the OpenAI detail object is absent.
	if row["cached_tokens"] != float64(150) {
		t.Fatalf("cached_tokens = %v, want 150 from prompt_cache_hit_tokens", row["cached_tokens"])
	}
	if _, present := row["reasoning_tokens"]; present {
		t.Fatalf("an unreported reasoning count was recorded: %v", row["reasoning_tokens"])
	}
}

// A stream that fails before producing output has no time to first output, and
// a usage detail that contradicts its total is dropped rather than shown.
func TestStreamWithoutOutputAndInconsistentDetailStayUnknown(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doStreams = append(host.doStreams, scriptedStream{status: http.StatusOK, chunks: [][]byte{
		[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"),
		[]byte(`data: {"model":"opendesign-large","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":0,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":999},"completion_tokens_details":{"reasoning_tokens":5}}}` + "\n\n"),
		[]byte("data: [DONE]\n\n"),
	}})
	if _, err := executeStream(t, s, executorPayload{AuthID: id, Model: "od-large", StreamID: "out-1", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	s.waitIdle()
	row := onlyLogRow(t, s)
	for _, field := range []string{"first_token_ms", "cached_tokens", "reasoning_tokens"} {
		if value, present := row[field]; present {
			t.Errorf("%s = %v, want absent", field, value)
		}
	}
	if row["output_tokens_per_second"] != nil {
		t.Errorf("a stream without output produced a rate: %v", row["output_tokens_per_second"])
	}
}

func metricStore(t *testing.T, clock time.Time) *requestLogStore {
	t.Helper()
	store := newRequestLogStore()
	store.now = func() time.Time { return clock }
	if err := store.configure(t.TempDir(), requestLogSettings{Enabled: true, RetentionDays: 7, MaxEntries: 500}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	return store
}

func TestRequestLogStatsAggregateCacheRateAndOutputRate(t *testing.T) {
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	store := metricStore(t, clock)
	at := func(hours int) time.Time { return clock.Add(-time.Duration(hours) * time.Hour) }
	for _, entry := range []RequestLogEntry{
		// 1000 output tokens over a 10s window after a 2s first token.
		{ID: "a", StartedAt: at(1), FinishedAt: at(1), DurationMS: 12000, Status: "success", Stream: true,
			InputTokens: int64Pointer(4000), OutputTokens: int64Pointer(1000), TotalTokens: int64Pointer(5000),
			CachedTokens: int64Pointer(3000), ReasoningTokens: int64Pointer(200), FirstTokenMS: int64Pointer(2000)},
		// 300 output tokens over the whole 6s: buffered, no first token.
		{ID: "b", StartedAt: at(2), FinishedAt: at(2), DurationMS: 6000, Status: "success",
			InputTokens: int64Pointer(1000), OutputTokens: int64Pointer(300), TotalTokens: int64Pointer(1300),
			CachedTokens: int64Pointer(0)},
		// Reports no cache information: unknown, so it is outside the hit rate.
		{ID: "c", StartedAt: at(3), FinishedAt: at(3), DurationMS: 4000, Status: "success", Stream: true,
			InputTokens: int64Pointer(6000), OutputTokens: int64Pointer(100), TotalTokens: int64Pointer(6100), FirstTokenMS: int64Pointer(1000)},
		// A 300ms window is too short to yield a rate.
		{ID: "d", StartedAt: at(4), FinishedAt: at(4), DurationMS: 300, Status: "success",
			InputTokens: int64Pointer(10), OutputTokens: int64Pointer(5), TotalTokens: int64Pointer(15)},
		// A failure without usage.
		{ID: "e", StartedAt: at(5), FinishedAt: at(5), DurationMS: 900, Status: "error"},
	} {
		store.record(entry)
	}
	stats := store.stats()
	tokens := stats["tokens"].(map[string]any)
	if got := tokens["cached"].(*int64); got == nil || *got != 3000 {
		t.Fatalf("cached total = %v, want 3000", got)
	}
	if got := tokens["reasoning"].(*int64); got == nil || *got != 200 {
		t.Fatalf("reasoning total = %v, want 200", got)
	}
	// 3000 cached of the 5000 input tokens that reported cache information.
	if rate := tokens["cache_rate"].(*float64); rate == nil || math.Abs(*rate-0.6) > 1e-9 {
		t.Fatalf("cache_rate = %v, want 0.6", rate)
	}
	if tokens["cache_samples"] != 2 {
		t.Fatalf("cache_samples = %v, want 2", tokens["cache_samples"])
	}
	// (1000 + 300 + 100) tokens over (10 + 6 + 3) seconds.
	want := 1400.0 * 1000 / 19000
	if rate := stats["output_tokens_per_second"].(*float64); rate == nil || math.Abs(*rate-want) > 1e-9 {
		t.Fatalf("output_tokens_per_second = %v, want %v", rate, want)
	}
	if stats["output_rate_samples"] != 3 {
		t.Fatalf("output_rate_samples = %v, want 3", stats["output_rate_samples"])
	}
	if first := stats["first_token_ms"].(*int64); first == nil || *first != 1500 || stats["first_token_samples"] != 2 {
		t.Fatalf("first_token_ms = %v over %v samples, want 1500 over 2", first, stats["first_token_samples"])
	}
	if stats["total"] != 5 || stats["success"] != 4 || stats["failed"] != 1 {
		t.Fatalf("totals changed: %#v", stats)
	}

	// The per-row rate uses the same rule as the aggregate.
	rows := requestLogEntryViews([]RequestLogEntry{
		{DurationMS: 12000, OutputTokens: int64Pointer(1000), FirstTokenMS: int64Pointer(2000)},
		{DurationMS: 300, OutputTokens: int64Pointer(5)},
		{DurationMS: 5000},
		{DurationMS: 5000, OutputTokens: int64Pointer(0)},
	})
	if rate := rows[0].OutputTokensPerSecond; rate == nil || math.Abs(*rate-100) > 1e-9 {
		t.Fatalf("row rate = %v, want 100", rate)
	}
	for i, row := range rows[1:] {
		if row.OutputTokensPerSecond != nil {
			t.Fatalf("row %d produced a rate from unusable data: %v", i+1, *row.OutputTokensPerSecond)
		}
	}
}

func TestRequestLogStatsRangeNarrowsRetainedRows(t *testing.T) {
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	store := metricStore(t, clock)
	for i, hours := range []int{1, 5, 30, 100} {
		started := clock.Add(-time.Duration(hours) * time.Hour)
		status := "success"
		if i == 2 {
			status = "error"
		}
		store.record(RequestLogEntry{ID: string(rune('a' + i)), StartedAt: started, FinishedAt: started, Status: status,
			InputTokens: int64Pointer(100), OutputTokens: int64Pointer(10), TotalTokens: int64Pointer(110)})
	}
	if all := store.stats(); all["total"] != 4 {
		t.Fatalf("unbounded total = %v, want 4", all["total"])
	}
	since := clock.Add(-24 * time.Hour)
	day := store.statsFor(&since, nil)
	if day["total"] != 2 || day["success"] != 2 || day["failed"] != 0 {
		t.Fatalf("last 24h = %#v, want the two newest rows", day)
	}
	if input := day["tokens"].(map[string]any)["input"].(*int64); input == nil || *input != 200 {
		t.Fatalf("last 24h input = %v, want 200", input)
	}
	until := clock.Add(-24 * time.Hour)
	older := store.statsFor(nil, &until)
	if older["total"] != 2 || older["failed"] != 1 {
		t.Fatalf("older than 24h = %#v", older)
	}
	// The range narrows what is retained; it never widens it.
	if retention := day["retention"].(map[string]any); retention["entries"] != 4 {
		t.Fatalf("retention view followed the range: %#v", retention)
	}
	window := day["range"].(map[string]any)
	if got, _ := window["since"].(*time.Time); got == nil || !got.Equal(since) || window["until"].(*time.Time) != nil {
		t.Fatalf("range echo = %#v", window)
	}
}

func TestRequestStatsRouteValidatesRange(t *testing.T) {
	s, _ := newTestService(t)
	now := time.Now().UTC()
	for i, age := range []time.Duration{time.Minute, 3 * time.Hour} {
		started := now.Add(-age)
		s.requestLogs.record(RequestLogEntry{ID: string(rune('a' + i)), StartedAt: started, FinishedAt: started, Status: "success"})
	}
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-stats", nil, nil)
	if status != http.StatusOK || body["total"] != float64(2) {
		t.Fatalf("unbounded stats = %d %s", status, raw)
	}
	window := body["range"].(map[string]any)
	if window["since"] != nil || window["until"] != nil {
		t.Fatalf("an unbounded request echoed a range: %s", raw)
	}
	since := now.Add(-time.Hour).Format(time.RFC3339)
	status, body, raw = managementCall(t, s, http.MethodGet, apiBase+"/request-stats", map[string][]string{"since": {since}}, nil)
	if status != http.StatusOK || body["total"] != float64(1) {
		t.Fatalf("since stats = %d %s", status, raw)
	}
	for name, query := range map[string]map[string][]string{
		"malformed since": {"since": {"yesterday"}},
		"malformed until": {"until": {"2026-13-40"}},
		"inverted range":  {"since": {now.Format(time.RFC3339)}, "until": {now.Add(-time.Hour).Format(time.RFC3339)}},
	} {
		if status, _, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-stats", query, nil); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d body = %s, want 400", name, status, raw)
		}
	}
}

// Rows written before these fields existed, and rows a hand-edited snapshot
// made inconsistent, load with the fields unknown instead of invented.
func TestRequestLogReloadKeepsLegacyRowsAndDropsInconsistentDetails(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	started := clock.Add(-time.Hour).Format(time.RFC3339Nano)
	snapshot := `{"version":1,"sequence":4,"logs":[` +
		`{"id":"legacy","started_at":"` + started + `","finished_at":"` + started + `","duration_ms":4000,"model":"m","upstream_model":"u","credential_id":"c","stream":true,"status":"success","http_status":200,"error_kind":null,"input_tokens":100,"output_tokens":40,"total_tokens":140},` +
		`{"id":"good","started_at":"` + started + `","finished_at":"` + started + `","duration_ms":4000,"model":"m","upstream_model":"u","credential_id":"c","stream":true,"status":"success","http_status":200,"error_kind":null,"input_tokens":100,"output_tokens":40,"total_tokens":140,"first_token_ms":1500,"cached_tokens":80,"reasoning_tokens":10},` +
		`{"id":"cached-over-input","started_at":"` + started + `","finished_at":"` + started + `","duration_ms":4000,"model":"m","upstream_model":"u","credential_id":"c","stream":true,"status":"success","http_status":200,"error_kind":null,"input_tokens":100,"output_tokens":40,"total_tokens":140,"first_token_ms":9000,"cached_tokens":101,"reasoning_tokens":41},` +
		`{"id":"detail-without-total","started_at":"` + started + `","finished_at":"` + started + `","duration_ms":4000,"model":"m","upstream_model":"u","credential_id":"c","stream":true,"status":"error","http_status":502,"error_kind":"upstream_error","input_tokens":null,"output_tokens":null,"total_tokens":null,"first_token_ms":-5,"cached_tokens":5,"reasoning_tokens":5}` +
		`]}`
	if err := os.WriteFile(filepath.Join(dir, requestLogSnapshotName), []byte(snapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newRequestLogStore()
	store.now = func() time.Time { return clock }
	if err := store.configure(dir, requestLogSettings{Enabled: true, RetentionDays: 7, MaxEntries: 500}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	logs, total, _ := store.query(requestLogFilter{limit: 10})
	if total != 4 {
		t.Fatalf("loaded %d rows, want 4", total)
	}
	byID := map[string]RequestLogEntry{}
	for _, entry := range logs {
		byID[entry.ID] = entry
	}
	if legacy := byID["legacy"]; legacy.FirstTokenMS != nil || legacy.CachedTokens != nil || legacy.ReasoningTokens != nil || legacy.InputTokens == nil || *legacy.InputTokens != 100 {
		t.Fatalf("legacy row = %#v", legacy)
	}
	if good := byID["good"]; good.FirstTokenMS == nil || *good.FirstTokenMS != 1500 || good.CachedTokens == nil || *good.CachedTokens != 80 || good.ReasoningTokens == nil || *good.ReasoningTokens != 10 {
		t.Fatalf("consistent row lost a field: %#v", good)
	}
	for _, id := range []string{"cached-over-input", "detail-without-total"} {
		if entry := byID[id]; entry.FirstTokenMS != nil || entry.CachedTokens != nil || entry.ReasoningTokens != nil {
			t.Fatalf("%s kept an inconsistent detail: first=%v cached=%v reasoning=%v", id, entry.FirstTokenMS, entry.CachedTokens, entry.ReasoningTokens)
		}
	}
	// The management shape of a legacy row carries no invented value either.
	encoded, err := json.Marshal(requestLogEntryViews([]RequestLogEntry{byID["legacy"]})[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"first_token_ms", "cached_tokens", "reasoning_tokens"} {
		if strings.Contains(string(encoded), `"`+field+`"`) {
			t.Fatalf("legacy row serialized %s: %s", field, encoded)
		}
	}
	// 40 tokens over the full 4s: no first token is known for a legacy row.
	if !strings.Contains(string(encoded), `"output_tokens_per_second":10`) {
		t.Fatalf("legacy row rate missing: %s", encoded)
	}
}
