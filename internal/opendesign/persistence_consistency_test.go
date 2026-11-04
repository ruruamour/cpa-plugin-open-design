package opendesign

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func persistenceStore(t *testing.T, dir string, now time.Time) *requestLogStore {
	t.Helper()
	s := newRequestLogStore()
	s.now = func() time.Time { return now }
	// Keep the test in control of the crash boundary instead of racing a timer.
	s.persistRetryDelay = time.Hour
	t.Cleanup(s.cancelScheduledFlush)
	if err := s.configure(dir, defaultRequestLogSettings()); err != nil {
		t.Fatalf("configure persistence store: %v", err)
	}
	return s
}

func persistFixtureLog(t *testing.T, s *requestLogStore, id string, now time.Time) {
	t.Helper()
	s.record(RequestLogEntry{ID: id, StartedAt: now, FinishedAt: now, Status: "success"})
	s.cancelScheduledFlush()
	if err := s.flush(); err != nil {
		t.Fatalf("persist fixture: %v", err)
	}
}

type failedJournalWriter struct {
	*os.File
	stage string
}

func (f failedJournalWriter) Write(data []byte) (int, error) {
	if f.stage == "write" {
		n, _ := f.File.Write(data[:len(data)/2])
		return n, errors.New("injected partial write failure")
	}
	if f.stage == "short-write" {
		return f.File.Write(data[:len(data)/2])
	}
	return f.File.Write(data)
}

func (f failedJournalWriter) Sync() error {
	if err := f.File.Sync(); err != nil {
		return err
	}
	if f.stage == "sync" {
		return errors.New("injected sync failure after complete bytes")
	}
	return nil
}

func (f failedJournalWriter) Close() error {
	err := f.File.Close()
	if f.stage == "close" {
		return errors.Join(err, errors.New("injected close failure after complete bytes"))
	}
	return err
}

func TestRequestLogJournalStagingFailuresPreserveCommittedState(t *testing.T) {
	for _, stage := range []string{"write", "short-write", "sync", "close"} {
		t.Run(stage, func(t *testing.T) {
			dir, now := t.TempDir(), time.Now().UTC()
			s := persistenceStore(t, dir, now)
			persistFixtureLog(t, s, "kept", now)
			path := filepath.Join(dir, requestLogJournalName)
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			s.appendJournal = func(path string, offset int64, event requestLogJournalEvent) requestLogAppendResult {
				return appendRequestLogJournalWithOpen(path, offset, event, func(name string, flags int, mode os.FileMode) (requestLogJournalWriter, error) {
					file, err := os.OpenFile(name, flags, mode)
					return failedJournalWriter{File: file, stage: stage}, err
				})
			}
			if err := s.clear(); err == nil {
				t.Fatal("clear unexpectedly succeeded")
			}
			s.cancelScheduledFlush()
			data, err := os.ReadFile(path)
			if err != nil || len(data) != 0 {
				t.Fatalf("failed staging modified original journal: %q %v", data, err)
			}
			logs, total, _ := s.query(requestLogFilter{limit: 10})
			if total != 1 || logs[0].ID != "kept" {
				t.Fatalf("failed clear changed memory: %#v", logs)
			}
			reloaded := persistenceStore(t, dir, now)
			logs, total, _ = reloaded.query(requestLogFilter{limit: 10})
			if total != 1 || logs[0].ID != "kept" {
				t.Fatalf("failed clear changed recovery: %#v", logs)
			}
			leftovers, _ := filepath.Glob(path + ".tmp-*")
			if len(leftovers) != 0 {
				t.Fatalf("failed stage left temporary files: %v", leftovers)
			}
		})
	}
}

func TestRequestLogPublishedClearSurvivesFinalizationError(t *testing.T) {
	dir, now := t.TempDir(), time.Now().UTC()
	s := persistenceStore(t, dir, now)
	persistFixtureLog(t, s, "old", now)
	s.appendJournal = func(path string, offset int64, event requestLogJournalEvent) requestLogAppendResult {
		result := appendRequestLogJournal(path, offset, event)
		if result.committed {
			result.err = errors.New("injected final directory close error")
		}
		return result
	}
	if err := s.clear(); err != nil {
		t.Fatalf("published clear was reported as uncommitted: %v", err)
	}
	s.cancelScheduledFlush()
	if _, total, _ := s.query(requestLogFilter{limit: 10}); total != 0 {
		t.Fatal("published clear retained memory")
	}
	// No snapshot compaction: recovery must use the already-published tombstone.
	reloaded := persistenceStore(t, dir, now)
	if _, total, _ := reloaded.query(requestLogFilter{limit: 10}); total != 0 {
		t.Fatal("published clear was not replayed")
	}
}

func TestRequestLogCorruptSnapshotPreservesJournalSequence(t *testing.T) {
	dir, now := t.TempDir(), time.Now().UTC()
	if err := os.WriteFile(filepath.Join(dir, requestLogSnapshotName), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, requestLogJournalName)
	result := appendRequestLogJournal(path, 0, requestLogJournalEvent{Sequence: 900, Clear: true})
	if result.err != nil || !result.committed {
		t.Fatalf("fixture tombstone: %+v", result)
	}
	s := newRequestLogStore()
	s.now = func() time.Time { return now }
	s.persistRetryDelay = time.Hour
	t.Cleanup(s.cancelScheduledFlush)
	if err := s.configure(dir, defaultRequestLogSettings()); err == nil {
		t.Fatal("corrupt snapshot was not reported")
	}
	s.record(RequestLogEntry{ID: "new", StartedAt: now, Status: "success"})
	s.cancelScheduledFlush()
	s.mu.Lock()
	sequence, entries := s.sequence, append([]RequestLogEntry(nil), s.entries...)
	s.mu.Unlock()
	if sequence <= 900 {
		t.Fatalf("recovery reused an old sequence: %d", sequence)
	}
	// Reproduce a crash after snapshot rename and before journal truncation.
	if _, err := writeRequestLogSnapshot(dir, sequence, entries); err != nil {
		t.Fatal(err)
	}
	reloaded := persistenceStore(t, dir, now)
	logs, total, _ := reloaded.query(requestLogFilter{limit: 10})
	if total != 1 || logs[0].ID != "new" {
		t.Fatalf("old tombstone erased a new record: %#v", logs)
	}
}

func TestRequestLogCorruptAndOversizedJournalsCompactBeforeClear(t *testing.T) {
	for _, suffix := range []string{"partial", "corrupt", "oversized"} {
		t.Run(suffix, func(t *testing.T) {
			dir, now := t.TempDir(), time.Now().UTC()
			seed := persistenceStore(t, dir, now)
			persistFixtureLog(t, seed, "old", now)
			path := filepath.Join(dir, requestLogJournalName)
			data := []byte("{partial")
			if suffix == "corrupt" {
				data = []byte("{corrupt}\n")
			} else if suffix == "oversized" {
				data = bytes.Repeat([]byte("x"), requestLogJournalMaxBytes+1)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			s := newRequestLogStore()
			s.now = func() time.Time { return now }
			s.persistRetryDelay = time.Hour
			t.Cleanup(s.cancelScheduledFlush)
			_ = s.configure(dir, defaultRequestLogSettings())
			s.cancelScheduledFlush()
			if err := s.clear(); err != nil {
				t.Fatalf("clear could not repair journal suffix: %v", err)
			}
			s.cancelScheduledFlush()
			reloaded := persistenceStore(t, dir, now)
			if _, total, _ := reloaded.query(requestLogFilter{limit: 10}); total != 0 {
				t.Fatal("recovery skipped clear after corrupt suffix")
			}
			info, err := os.Stat(path)
			if err != nil || info.Size() > 4096 {
				t.Fatalf("journal was not compacted: %v %v", info, err)
			}
		})
	}
}

func TestRequestLogConfigureFailureAndEmptyDirectoryDoNotOverwriteData(t *testing.T) {
	now, dir := time.Now().UTC(), t.TempDir()
	s := persistenceStore(t, dir, now)
	persistFixtureLog(t, s, "retained", now)
	if err := s.configure("", defaultRequestLogSettings()); err != nil {
		t.Fatal(err)
	}
	s.record(RequestLogEntry{ID: "ignored", StartedAt: now, Status: "success"})
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.configure(filepath.Join(blocker, "logs"), defaultRequestLogSettings()); err == nil {
		t.Fatal("configure unexpectedly created a child of a file")
	}
	s.mu.Lock()
	loaded, sequence, journalBytes := s.loaded, s.sequence, s.journalBytes
	s.mu.Unlock()
	if loaded || sequence != 0 || journalBytes != 0 {
		t.Fatalf("failed configure retained stale state: %v %d %d", loaded, sequence, journalBytes)
	}
	if err := s.configure(dir, defaultRequestLogSettings()); err != nil {
		t.Fatal(err)
	}
	logs, total, _ := s.query(requestLogFilter{limit: 10})
	if total != 1 || logs[0].ID != "retained" {
		t.Fatalf("reconfigure overwrote existing snapshot: %#v", logs)
	}
}

func TestRequestLogCancelJoinsRetryWorker(t *testing.T) {
	dir, now := t.TempDir(), time.Now().UTC()
	s := persistenceStore(t, dir, now)
	persistFixtureLog(t, s, "old", now)
	if err := os.Remove(filepath.Join(dir, requestLogSnapshotName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, requestLogSnapshotName), 0o700); err != nil {
		t.Fatal(err)
	}
	s.persistScheduleMu.Lock()
	s.persistRetryDelay = time.Millisecond
	s.persistScheduleMu.Unlock()
	s.record(RequestLogEntry{ID: "pending", StartedAt: now, Status: "success"})
	deadline := time.Now().Add(time.Second)
	for {
		s.persistScheduleMu.Lock()
		retrying := s.persistRetryDelay == 2*time.Second
		s.persistScheduleMu.Unlock()
		if retrying {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker never entered retry delay")
		}
		time.Sleep(time.Millisecond)
	}
	s.cancelScheduledFlush()
	s.persistScheduleMu.Lock()
	pending, done, cancel := s.persistScheduled, s.persistDone, s.persistCancel
	s.persistScheduleMu.Unlock()
	if pending || done != nil || cancel != nil {
		t.Fatal("canceled worker remained installed")
	}
	// An explicit new record can install its own worker after the boundary.
	s.record(RequestLogEntry{ID: "new", StartedAt: now, Status: "success"})
	s.cancelScheduledFlush()
}

func TestRequestLogQueryRejectsUnsafeRanges(t *testing.T) {
	for _, raw := range []string{
		"limit=0", "limit=201", "offset=-1", "offset=1000001", "status=other",
		"attempted=maybe", "model=" + strings.Repeat("a", 513), "credential_id=a%0Ab",
		"since=oops", "until=-1", "since=200&until=100",
	} {
		query, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseRequestLogQuery(query); err == nil {
			t.Errorf("accepted unsafe query: %s", raw)
		}
	}
	filter, err := parseRequestLogQuery(url.Values{"model": {strings.Repeat("a", 512)}, "since": {"100"}, "until": {"200"}})
	if err != nil || filter.since == nil || filter.until == nil {
		t.Fatalf("rejected bounded query: %#v %v", filter, err)
	}
}

// Windows cannot replace a destination that still has an open handle, so the
// journal append must close the file it read from before the swap.
func TestRequestLogJournalAppendClosesSourceBeforeSwap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, requestLogJournalName)
	first := appendRequestLogJournal(path, 0, requestLogJournalEvent{Sequence: 1, Clear: true})
	if first.err != nil || !first.committed {
		t.Fatalf("seed journal: %+v", first)
	}

	openAtSwap := true
	original := swapJournalFile
	swapJournalFile = func(tmp, dst string) error {
		if handle, err := os.Open(dst); err == nil {
			handle.Close()
		} else {
			openAtSwap = false
		}
		return original(tmp, dst)
	}
	t.Cleanup(func() { swapJournalFile = original })

	second := appendRequestLogJournal(path, first.offset, requestLogJournalEvent{Sequence: 2, Clear: true})
	if second.err != nil || !second.committed {
		t.Fatalf("append: %+v", second)
	}
	if !openAtSwap {
		t.Fatal("destination was not present at swap time")
	}
}

// A successful append must contain exactly a complete event and newline.
func TestRequestLogJournalAtomicAppendShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), requestLogJournalName)
	first := appendRequestLogJournal(path, 0, requestLogJournalEvent{Sequence: 1, Clear: true})
	second := appendRequestLogJournal(path, first.offset, requestLogJournalEvent{Sequence: 3, Clear: true})
	if first.err != nil || second.err != nil || !second.committed {
		t.Fatalf("append results: %+v %+v", first, second)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	for _, sequence := range []uint64{1, 3} {
		var event requestLogJournalEvent
		if err := decoder.Decode(&event); err != nil || event.Sequence != sequence || !event.Clear {
			t.Fatalf("journal event = %+v, %v", event, err)
		}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("journal trailing bytes: %v", err)
	}
}
