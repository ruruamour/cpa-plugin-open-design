package opendesign

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// installSwapFailure makes the first failures journal/snapshot renames fail the
// way Windows does when the destination still has an open handle: "Access is
// denied" instead of a silent replace. It returns a counter of attempts.
func installSwapFailure(t *testing.T, failures int) *int32 {
	t.Helper()
	var attempts int32
	original := swapJournalFile
	swapJournalFile = func(tmp, dst string) error {
		if int(atomic.AddInt32(&attempts, 1)) <= failures {
			return errors.New("Access is denied.")
		}
		return original(tmp, dst)
	}
	t.Cleanup(func() { swapJournalFile = original })
	return &attempts
}

// A transient Windows share violation on the rename must be retried instead of
// surfacing as a failed persistence step. This is the class of failure that
// broke the Windows release job on GitHub Actions.
func TestJournalSwapRetriesTransientWindowsShareViolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, requestLogJournalName)
	first := appendRequestLogJournal(path, 0, requestLogJournalEvent{Sequence: 1, Clear: true})
	if first.err != nil || !first.committed {
		t.Fatalf("seed journal: %+v", first)
	}

	attempts := installSwapFailure(t, 2)
	second := appendRequestLogJournal(path, first.offset, requestLogJournalEvent{Sequence: 2, Clear: true})
	if second.err != nil || !second.committed {
		t.Fatalf("append under share violation: %+v", second)
	}
	if got := atomic.LoadInt32(attempts); got != 3 {
		t.Fatalf("rename attempts = %d, want 3", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if len(data) <= int(first.offset) {
		t.Fatalf("journal was not replaced with the staged copy: %d bytes", len(data))
	}
}

// A persistent share violation must still fail loudly and leave the previous
// journal in place, so the caller keeps its committed state.
func TestJournalSwapPersistentWindowsShareViolationKeepsJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, requestLogJournalName)
	first := appendRequestLogJournal(path, 0, requestLogJournalEvent{Sequence: 1, Clear: true})
	if first.err != nil || !first.committed {
		t.Fatalf("seed journal: %+v", first)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}

	installSwapFailure(t, 1<<30)
	second := appendRequestLogJournal(path, first.offset, requestLogJournalEvent{Sequence: 2, Clear: true})
	if second.err == nil || second.committed {
		t.Fatalf("append unexpectedly committed: %+v", second)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("failed swap modified the previous journal")
	}
	if leftovers, _ := filepath.Glob(path + ".tmp-*"); len(leftovers) != 0 {
		t.Fatalf("failed swap left staged files: %v", leftovers)
	}
}

// clear() publishes the durable tombstone through the journal rename, so the
// retry must also cover the transaction the operator triggers from the console.
func TestClearRetriesTransientWindowsShareViolation(t *testing.T) {
	dir, now := t.TempDir(), time.Now().UTC()
	s := persistenceStore(t, dir, now)
	persistFixtureLog(t, s, "old", now)

	attempts := installSwapFailure(t, 2)
	if err := s.clear(); err != nil {
		t.Fatalf("clear under share violation: %v", err)
	}
	s.cancelScheduledFlush()
	if _, total, _ := s.query(requestLogFilter{limit: 10}); total != 0 {
		t.Fatal("clear did not take effect")
	}
	if got := atomic.LoadInt32(attempts); got != 3 {
		t.Fatalf("rename attempts = %d, want 3", got)
	}
}

// clear() must stay transactional when every rename keeps failing: the operator
// sees an error and the entries stay queryable.
func TestClearKeepsStateOnPersistentWindowsShareViolation(t *testing.T) {
	dir, now := t.TempDir(), time.Now().UTC()
	s := persistenceStore(t, dir, now)
	persistFixtureLog(t, s, "kept", now)

	installSwapFailure(t, 1<<30)
	if err := s.clear(); err == nil {
		t.Fatal("clear unexpectedly succeeded")
	}
	s.cancelScheduledFlush()
	logs, total, _ := s.query(requestLogFilter{limit: 10})
	if total != 1 || logs[0].ID != "kept" {
		t.Fatalf("failed clear changed memory: %#v", logs)
	}
	reloaded := persistenceStore(t, dir, now)
	logs, total, _ = reloaded.query(requestLogFilter{limit: 10})
	if total != 1 || logs[0].ID != "kept" {
		t.Fatalf("failed clear changed recovery: %#v", logs)
	}
}

// The snapshot publish has the same Windows constraint, so a share violation
// there is retried as well.
func TestSnapshotPublishRetriesTransientWindowsShareViolation(t *testing.T) {
	dir, now := t.TempDir(), time.Now().UTC()
	s := persistenceStore(t, dir, now)
	persistFixtureLog(t, s, "old", now)
	s.needsSnapshot = true

	attempts := installSwapFailure(t, 2)
	if err := s.flush(); err != nil {
		t.Fatalf("flush under share violation: %v", err)
	}
	if got := atomic.LoadInt32(attempts); got != 3 {
		t.Fatalf("rename attempts = %d, want 3", got)
	}
	if _, err := os.Stat(filepath.Join(dir, requestLogSnapshotName)); err != nil {
		t.Fatalf("snapshot was not published: %v", err)
	}
}
