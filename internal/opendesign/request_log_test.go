package opendesign

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnconfiguredRequestLogStoreNeverWritesWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())

	s := NewService()
	if _, err := s.quiesce(); err != nil {
		t.Fatalf("unconfigured quiesce: %v", err)
	}
	for _, name := range []string{requestLogSnapshotName, requestLogJournalName} {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unconfigured quiesce created %s: %v", name, err)
		}
	}

	s = NewService()
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/config", nil, map[string]any{
		"logs": map[string]any{"enabled": false},
	})
	if status != http.StatusConflict {
		t.Fatalf("unconfigured config status = %d body = %s", status, raw)
	}
	if _, err := s.quiesce(); err != nil {
		t.Fatalf("unconfigured config quiesce: %v", err)
	}
	for _, name := range []string{requestLogSnapshotName, requestLogJournalName} {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unconfigured config created %s: %v", name, err)
		}
	}
}

func TestRequestLogStorePersistsFiltersStatsAndClear(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	settings := requestLogSettings{Enabled: true, RetentionDays: 7, MaxEntries: 2}
	store := newRequestLogStore()
	store.now = func() time.Time { return clock }
	if err := store.configure(dir, settings); err != nil {
		t.Fatalf("configure: %v", err)
	}
	for _, entry := range []RequestLogEntry{
		{ID: "one", StartedAt: clock.Add(-3 * time.Hour), FinishedAt: clock.Add(-3*time.Hour + time.Second), Model: "alias-one", Status: "success"},
		{ID: "two", StartedAt: clock.Add(-2 * time.Hour), FinishedAt: clock.Add(-2*time.Hour + time.Second), Model: "alias-two", Status: "error"},
		{ID: "three", StartedAt: clock.Add(-time.Hour), FinishedAt: clock.Add(-time.Hour + time.Second), Model: "alias-three", Status: "success"},
	} {
		store.record(entry)
	}
	if err := store.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	logs, total, _ := store.query(requestLogFilter{limit: 10})
	if total != 2 || len(logs) != 2 || logs[0].ID != "three" || logs[1].ID != "two" {
		t.Fatalf("logs = %#v total = %d, want newest two", logs, total)
	}
	filtered, filteredTotal, _ := store.query(requestLogFilter{limit: 10, status: "error"})
	if filteredTotal != 1 || len(filtered) != 1 || filtered[0].ID != "two" {
		t.Fatalf("filtered logs = %#v total = %d", filtered, filteredTotal)
	}
	stats := store.stats()
	if stats["total"] != 2 || stats["success"] != 1 || stats["failed"] != 1 {
		t.Fatalf("stats = %#v", stats)
	}

	reloaded := newRequestLogStore()
	reloaded.now = func() time.Time { return clock }
	if err := reloaded.configure(dir, settings); err != nil {
		t.Fatalf("reload: %v", err)
	}
	logs, total, _ = reloaded.query(requestLogFilter{limit: 10})
	if total != 2 || len(logs) != 2 || logs[0].ID != "three" || logs[1].ID != "two" {
		t.Fatalf("reloaded logs = %#v total = %d", logs, total)
	}
	if err := reloaded.clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cleared := newRequestLogStore()
	cleared.now = func() time.Time { return clock }
	if err := cleared.configure(dir, settings); err != nil {
		t.Fatalf("reload after clear: %v", err)
	}
	logs, total, _ = cleared.query(requestLogFilter{limit: 10})
	if total != 0 || len(logs) != 0 {
		t.Fatalf("logs after clear = %#v total = %d", logs, total)
	}
}

func TestRequestLogRecordRedactsRequestMetadata(t *testing.T) {
	s, host := newTestService(t)
	secret := "sk-log-secret-123"
	_ = addCredential(t, s, host, "primary", secret)
	started := time.Now().Add(-time.Second)
	s.recordRequestLog(&requestLogDraft{
		started:       started,
		model:         "alias",
		upstreamModel: "upstream",
		credentialID:  "credential",
		requestID:     secret,
	}, errors.New("upstream detail: "+secret))
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-logs", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("logs status = %d body = %s", status, raw)
	}
	if strings.Contains(raw, secret) || strings.Contains(raw, "upstream detail") {
		t.Fatalf("request log leaked secret or raw error: %s", raw)
	}
	logs, ok := body["logs"].([]any)
	if !ok || len(logs) != 1 {
		t.Fatalf("logs = %#v", body["logs"])
	}
	entry := logs[0].(map[string]any)
	if entry["request_id"] != nil && entry["request_id"] != "" {
		t.Fatalf("request id was not removed: %#v", entry["request_id"])
	}
	if entry["status"] != "error" || entry["error_kind"] == nil {
		t.Fatalf("error metadata = %#v", entry)
	}
}

func TestRequestLogReplayRedactsCurrentSecrets(t *testing.T) {
	dir := t.TempDir()
	secret := "sk-replay-secret"
	kind := secret
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	data, err := json.Marshal(requestLogSnapshot{
		Version:  requestLogSnapshotVersion,
		Sequence: 1,
		Logs: []RequestLogEntry{{
			ID: "replayed", StartedAt: now, FinishedAt: now, Status: "error",
			ErrorKind: &kind, RequestID: secret,
		}},
	})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	dirPath := filepath.Join(dir, requestLogSnapshotName)
	if err := os.WriteFile(dirPath, data, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	store := newRequestLogStore()
	store.now = func() time.Time { return now }
	store.keys = func() []string { return []string{secret} }
	if err := store.configure(dir, defaultRequestLogSettings()); err != nil {
		t.Fatalf("configure: %v", err)
	}
	logs, total, _ := store.query(requestLogFilter{limit: 10})
	if total != 1 || len(logs) != 1 {
		t.Fatalf("replayed logs = %#v total=%d", logs, total)
	}
	if logs[0].ErrorKind != nil || logs[0].RequestID != "" {
		t.Fatalf("replayed secret metadata survived: %#v", logs[0])
	}
	if err := store.flush(); err != nil {
		t.Fatalf("flush scrubbed snapshot: %v", err)
	}
	persisted, err := os.ReadFile(dirPath)
	if err != nil {
		t.Fatalf("read scrubbed snapshot: %v", err)
	}
	if strings.Contains(string(persisted), secret) {
		t.Fatalf("scrubbed snapshot still contains secret: %s", persisted)
	}
}

func TestRequestLogPruneKeepsNewestByStartedAtWhenCompletionsArriveOutOfOrder(t *testing.T) {
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	store := newRequestLogStore()
	store.now = func() time.Time { return clock }
	if err := store.configure(t.TempDir(), requestLogSettings{Enabled: true, RetentionDays: 7, MaxEntries: 2}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	for _, entry := range []RequestLogEntry{
		{ID: "newest", StartedAt: clock.Add(-time.Hour), Status: "success"},
		{ID: "oldest", StartedAt: clock.Add(-3 * time.Hour), Status: "success"},
		{ID: "middle", StartedAt: clock.Add(-2 * time.Hour), Status: "success"},
	} {
		store.record(entry)
	}
	logs, total, _ := store.query(requestLogFilter{limit: 10})
	if total != 2 || len(logs) != 2 || logs[0].ID != "newest" || logs[1].ID != "middle" {
		t.Fatalf("logs = %#v total = %d, want newest and middle entries", logs, total)
	}
}

func TestRequestLogClearFailurePreservesMemoryAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	store := newRequestLogStore()
	store.now = func() time.Time { return clock }
	if err := store.configure(dir, defaultRequestLogSettings()); err != nil {
		t.Fatalf("configure: %v", err)
	}
	store.record(RequestLogEntry{ID: "kept", StartedAt: clock, FinishedAt: clock, Status: "success"})
	store.cancelScheduledFlush()
	if err := store.flush(); err != nil {
		t.Fatalf("initial flush: %v", err)
	}
	journalPath := filepath.Join(dir, requestLogJournalName)
	if err := os.Mkdir(journalPath, 0o700); err != nil {
		t.Fatalf("make journal path unwritable: %v", err)
	}
	if err := store.clear(); err == nil {
		t.Fatal("clear unexpectedly succeeded with a directory at the journal path")
	}
	logs, total, _ := store.query(requestLogFilter{limit: 10})
	if total != 1 || len(logs) != 1 || logs[0].ID != "kept" {
		t.Fatalf("failed clear changed memory: %#v total=%d", logs, total)
	}
	if err := os.Remove(journalPath); err != nil {
		t.Fatalf("remove journal directory: %v", err)
	}
	reloaded := newRequestLogStore()
	reloaded.now = func() time.Time { return clock }
	if err := reloaded.configure(dir, defaultRequestLogSettings()); err != nil {
		t.Fatalf("reload after failed clear: %v", err)
	}
	logs, total, _ = reloaded.query(requestLogFilter{limit: 10})
	if total != 1 || len(logs) != 1 || logs[0].ID != "kept" {
		t.Fatalf("failed clear changed durable state: %#v total=%d", logs, total)
	}
}

func TestRequestLogClearTombstoneRecoversAfterSnapshotFailure(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	store := newRequestLogStore()
	store.now = func() time.Time { return clock }
	if err := store.configure(dir, defaultRequestLogSettings()); err != nil {
		t.Fatalf("configure: %v", err)
	}
	store.record(RequestLogEntry{ID: "kept", StartedAt: clock, FinishedAt: clock, Status: "success"})
	store.cancelScheduledFlush()
	if err := store.flush(); err != nil {
		t.Fatalf("initial flush: %v", err)
	}
	snapshotPath := filepath.Join(dir, requestLogSnapshotName)
	backupPath := snapshotPath + ".backup"
	if err := os.Rename(snapshotPath, backupPath); err != nil {
		t.Fatalf("move snapshot aside: %v", err)
	}
	if err := os.Mkdir(snapshotPath, 0o700); err != nil {
		t.Fatalf("make snapshot target unwritable: %v", err)
	}
	if err := store.clear(); err != nil {
		t.Fatalf("clear with unavailable snapshot target: %v", err)
	}
	logs, total, _ := store.query(requestLogFilter{limit: 10})
	if total != 0 || len(logs) != 0 {
		t.Fatalf("clear did not commit in memory: %#v total=%d", logs, total)
	}
	store.cancelScheduledFlush()
	if err := os.Remove(snapshotPath); err != nil {
		t.Fatalf("remove snapshot directory: %v", err)
	}
	if err := os.Rename(backupPath, snapshotPath); err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}
	if err := store.flush(); err != nil {
		t.Fatalf("flush recovered clear: %v", err)
	}
	reloaded := newRequestLogStore()
	reloaded.now = func() time.Time { return clock }
	if err := reloaded.configure(dir, defaultRequestLogSettings()); err != nil {
		t.Fatalf("reload after clear: %v", err)
	}
	logs, total, _ = reloaded.query(requestLogFilter{limit: 10})
	if total != 0 || len(logs) != 0 {
		t.Fatalf("clear tombstone was not recovered: %#v total=%d", logs, total)
	}
}

func TestRequestLogClearTombstoneReplaysAcrossSequenceGap(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	old := RequestLogEntry{ID: "old", StartedAt: clock, FinishedAt: clock, Status: "success"}
	data, err := json.Marshal(requestLogSnapshot{Version: requestLogSnapshotVersion, Sequence: 4, Logs: []RequestLogEntry{old}})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, requestLogSnapshotName), data, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	journal, err := os.OpenFile(filepath.Join(dir, requestLogJournalName), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	for _, event := range []requestLogJournalEvent{
		{Sequence: 5, RetentionDays: 7, MaxEntries: 500, Clear: true},
		{Sequence: 7, RetentionDays: 7, MaxEntries: 500, Entry: &RequestLogEntry{ID: "new", StartedAt: clock, FinishedAt: clock, Status: "success"}},
	} {
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal journal event: %v", err)
		}
		if _, err := journal.Write(append(line, '\n')); err != nil {
			t.Fatalf("write journal event: %v", err)
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	store := newRequestLogStore()
	store.now = func() time.Time { return clock }
	if err := store.configure(dir, defaultRequestLogSettings()); err != nil {
		t.Fatalf("replay clear tombstone: %v", err)
	}
	logs, total, _ := store.query(requestLogFilter{limit: 10})
	if total != 1 || len(logs) != 1 || logs[0].ID != "new" {
		t.Fatalf("replayed logs = %#v total=%d", logs, total)
	}
}

func TestRequestLogClearReservesSequenceBeforeConcurrentRecord(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	store := newRequestLogStore()
	store.now = func() time.Time { return clock }
	if err := store.configure(dir, defaultRequestLogSettings()); err != nil {
		t.Fatalf("configure: %v", err)
	}
	store.record(RequestLogEntry{ID: "old", StartedAt: clock, FinishedAt: clock, Status: "success"})
	store.cancelScheduledFlush()
	if err := store.flush(); err != nil {
		t.Fatalf("initial flush: %v", err)
	}

	entered := make(chan requestLogJournalEvent, 1)
	continueAppend := make(chan struct{})
	store.appendJournal = func(path string, offset int64, event requestLogJournalEvent) requestLogAppendResult {
		entered <- event
		<-continueAppend
		return appendRequestLogJournal(path, offset, event)
	}
	clearDone := make(chan error, 1)
	go func() { clearDone <- store.clear() }()
	event := <-entered
	store.mu.Lock()
	reserved := store.sequence
	store.mu.Unlock()
	if event.Sequence != reserved {
		t.Fatalf("clear sequence = %d, reserved sequence = %d", event.Sequence, reserved)
	}
	store.record(RequestLogEntry{ID: "new", StartedAt: clock.Add(time.Second), FinishedAt: clock.Add(time.Second), Status: "success"})
	close(continueAppend)
	if err := <-clearDone; err != nil {
		t.Fatalf("clear: %v", err)
	}
	store.appendJournal = nil
	store.cancelScheduledFlush()

	store.mu.Lock()
	sequence := store.sequence
	entries := append([]RequestLogEntry(nil), store.entries...)
	store.mu.Unlock()
	if sequence <= event.Sequence || len(entries) != 1 || entries[0].ID != "new" {
		t.Fatalf("post-clear state sequence=%d entries=%#v, clear event=%d", sequence, entries, event.Sequence)
	}
	if _, err := writeRequestLogSnapshot(dir, sequence, entries); err != nil {
		t.Fatalf("publish snapshot before journal truncate: %v", err)
	}

	reloaded := newRequestLogStore()
	reloaded.now = func() time.Time { return clock }
	if err := reloaded.configure(dir, defaultRequestLogSettings()); err != nil {
		t.Fatalf("replay concurrent clear: %v", err)
	}
	logs, total, _ := reloaded.query(requestLogFilter{limit: 10})
	if total != 1 || len(logs) != 1 || logs[0].ID != "new" {
		t.Fatalf("concurrent post-clear request lost during replay: %#v total=%d", logs, total)
	}
}

func TestRequestLogAttemptedStatsAndFilteringPreserveLegacyNil(t *testing.T) {
	s, _ := newTestService(t)
	now := time.Now().UTC()
	local := false
	attempted := true
	for _, entry := range []RequestLogEntry{
		{ID: "success", StartedAt: now, FinishedAt: now, Status: "success", UpstreamAttempted: &attempted},
		{ID: "remote-failure", StartedAt: now, FinishedAt: now, Status: "error", UpstreamAttempted: nil},
		{ID: "local-failure", StartedAt: now, FinishedAt: now, Status: "error", UpstreamAttempted: &local},
	} {
		s.requestLogs.record(entry)
	}
	stats := s.requestLogs.stats()
	if stats["total"] != 3 || stats["upstream_attempted"] != 2 || stats["local_rejected"] != 1 {
		t.Fatalf("attempted stats = %#v", stats)
	}
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-logs", map[string][]string{"attempted": {"no"}}, nil)
	if status != http.StatusOK {
		t.Fatalf("local filter status = %d body = %s", status, raw)
	}
	logs, ok := body["logs"].([]any)
	if !ok || len(logs) != 1 || logs[0].(map[string]any)["id"] != "local-failure" {
		t.Fatalf("local-filter logs = %#v", body["logs"])
	}
	if status, _, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-logs", map[string][]string{"attempted": {"maybe"}}, nil); status != http.StatusBadRequest {
		t.Fatalf("invalid attempted filter status = %d body = %s", status, raw)
	}
}

func TestRequestLogSearchMatchesSafeMetadataCaseInsensitively(t *testing.T) {
	s, _ := newTestService(t)
	now := time.Now().UTC()
	attempted := true
	quotaKind := "insufficient_balance"
	for _, entry := range []RequestLogEntry{
		{ID: "req-alpha", StartedAt: now, FinishedAt: now, Status: "success", Model: "od-flash", UpstreamModel: "deepseek-v4.1-flash", CredentialID: "cred-main", RequestID: "chatcmpl-ABC", UpstreamAttempted: &attempted},
		{ID: "req-beta", StartedAt: now, FinishedAt: now, Status: "error", Model: "od-pro", ErrorKind: &quotaKind, UpstreamAttempted: &attempted},
		{ID: "req-gamma", StartedAt: now, FinishedAt: now, Status: "success", Model: "other", UpstreamAttempted: &attempted},
	} {
		s.requestLogs.record(entry)
	}
	for needle, want := range map[string][]string{
		"deepseek":         {"req-alpha"},
		"CHATCMPl":         {"req-alpha"},
		"cred-main":        {"req-alpha"},
		"INSUFFICIENT":     {"req-beta"},
		"od-":              {"req-alpha", "req-beta"},
		"no-such-metadata": nil,
	} {
		status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-logs", map[string][]string{"q": {needle}}, nil)
		if status != http.StatusOK {
			t.Fatalf("search %q status = %d body = %s", needle, status, raw)
		}
		logs, _ := body["logs"].([]any)
		got := make([]string, 0, len(logs))
		for _, item := range logs {
			got = append(got, item.(map[string]any)["id"].(string))
		}
		if len(got) != len(want) {
			t.Fatalf("search %q ids = %#v, want %#v", needle, got, want)
		}
		found := map[string]bool{}
		for _, id := range got {
			found[id] = true
		}
		for _, id := range want {
			if !found[id] {
				t.Fatalf("search %q ids = %#v, want %#v", needle, got, want)
			}
		}
	}
	if status, _, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-logs", map[string][]string{"q": {"bad\nneedle"}}, nil); status != http.StatusBadRequest {
		t.Fatalf("control-character search status = %d body = %s", status, raw)
	}
	if status, _, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-logs", map[string][]string{"q": {strings.Repeat("x", 129)}}, nil); status != http.StatusBadRequest {
		t.Fatalf("over-length search status = %d body = %s", status, raw)
	}
}
