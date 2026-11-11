package opendesign

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	requestLogSnapshotName    = "request-logs.snapshot.json"
	requestLogJournalName     = "request-logs.journal.jsonl"
	requestLogSnapshotVersion = 1

	defaultRequestLogRetentionDays = 7
	maxRequestLogRetentionDays     = 90
	defaultRequestLogMaxEntries    = 500
	maxRequestLogMaxEntries        = 500
	maxRequestLogTokenValue        = int64(1) << 50

	requestLogQueryDefaultLimit = 50
	requestLogQueryMaxLimit     = 200
	requestLogQueryMaxOffset    = 1_000_000

	requestLogJournalMaxBytes  = 16 << 20
	requestLogSnapshotMaxBytes = 16 << 20
)

// RequestLogEntry is the complete metadata allowlist stored for one request.
// Bodies, headers, query strings, API keys, control keys and workspace ids are
// never part of this shape.
type RequestLogEntry struct {
	ID            string    `json:"id"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	DurationMS    int64     `json:"duration_ms"`
	Model         string    `json:"model"`
	UpstreamModel string    `json:"upstream_model"`
	CredentialID  string    `json:"credential_id"`
	Stream        bool      `json:"stream"`
	Status        string    `json:"status"`
	HTTPStatus    *int      `json:"http_status"`
	ErrorKind     *string   `json:"error_kind"`
	InputTokens   *int64    `json:"input_tokens"`
	OutputTokens  *int64    `json:"output_tokens"`
	TotalTokens   *int64    `json:"total_tokens"`
	RequestID     string    `json:"request_id,omitempty"`
	// UpstreamAttempted distinguishes a locally rejected request from one that
	// reached the provider. A nil value is treated as true for logs written by
	// versions before this field existed.
	UpstreamAttempted *bool `json:"upstream_attempted,omitempty"`
	// ReasoningEffort is the top-level strength the request carried when it was
	// sent upstream, after any mapping rule ran. Empty means no explicit value.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ReasoningMapping records a rule that changed the strength. It is absent
	// when no rule matched, so "unchanged" and "no rule" stay distinguishable.
	ReasoningMapping *RequestLogReasoningMapping `json:"reasoning_mapping,omitempty"`
	// FirstTokenMS is the time from the start of a streaming request to the
	// first frame that carried output (text, reasoning or a tool call). It is
	// absent for buffered requests, for streams that never produced output and
	// for rows written before this field existed.
	FirstTokenMS *int64 `json:"first_token_ms,omitempty"`
	// CachedTokens and ReasoningTokens are the optional usage details an
	// OpenAI-compatible upstream may report. They are subsets of the input and
	// output counts respectively, never added on top of them, and stay absent
	// when the upstream did not report them.
	CachedTokens    *int64 `json:"cached_tokens,omitempty"`
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
}

// requestLogEntryView is one row of the management response. The output rate
// is derived when the row is read rather than stored, so every row, including
// those written by an earlier version, is computed by the same rule.
type requestLogEntryView struct {
	RequestLogEntry
	OutputTokensPerSecond *float64 `json:"output_tokens_per_second"`
}

// requestLogMinRateWindowMS is the shortest generation window a rate is
// computed from. A few tokens divided by a few milliseconds is noise.
const requestLogMinRateWindowMS = 500

// requestLogRateSample returns the output tokens and the window they were
// produced in. The window excludes the time to first output when it is known,
// so a slow start is reported as latency rather than as a slow generation.
func requestLogRateSample(entry RequestLogEntry) (tokens, windowMS int64, ok bool) {
	if entry.OutputTokens == nil || *entry.OutputTokens <= 0 {
		return 0, 0, false
	}
	windowMS = entry.DurationMS
	if entry.FirstTokenMS != nil && *entry.FirstTokenMS > 0 && *entry.FirstTokenMS <= entry.DurationMS {
		windowMS -= *entry.FirstTokenMS
	}
	if windowMS < requestLogMinRateWindowMS {
		return 0, 0, false
	}
	return *entry.OutputTokens, windowMS, true
}

func requestLogEntryViews(entries []RequestLogEntry) []requestLogEntryView {
	views := make([]requestLogEntryView, 0, len(entries))
	for _, entry := range entries {
		view := requestLogEntryView{RequestLogEntry: entry}
		if tokens, windowMS, ok := requestLogRateSample(entry); ok {
			rate := float64(tokens) * 1000 / float64(windowMS)
			view.OutputTokensPerSecond = &rate
		}
		views = append(views, view)
	}
	return views
}

// RequestLogReasoningMapping is the closed from→to pair of one applied rule.
type RequestLogReasoningMapping struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type requestLogSettings struct {
	Enabled       bool
	RetentionDays int
	MaxEntries    int
}

func defaultRequestLogSettings() requestLogSettings {
	return requestLogSettings{
		Enabled:       true,
		RetentionDays: defaultRequestLogRetentionDays,
		MaxEntries:    defaultRequestLogMaxEntries,
	}
}

type requestLogJournalEvent struct {
	Sequence      uint64           `json:"sequence"`
	RetentionDays int              `json:"retention_days"`
	MaxEntries    int              `json:"max_entries"`
	Entry         *RequestLogEntry `json:"entry,omitempty"`
	Clear         bool             `json:"clear,omitempty"`
}

type requestLogSnapshot struct {
	Version  int               `json:"version"`
	Sequence uint64            `json:"sequence"`
	Logs     []RequestLogEntry `json:"logs"`
}

type requestLogStore struct {
	mu                sync.Mutex
	persistMu         sync.Mutex
	persistCommitMu   sync.RWMutex
	persistScheduleMu sync.Mutex
	persistScheduled  bool
	persistDone       chan struct{}
	persistCancel     chan struct{}
	persistGeneration uint64
	persistRetryDelay time.Duration
	appendJournal     func(string, int64, requestLogJournalEvent) requestLogAppendResult
	dir               string
	now               func() time.Time
	keys              func() []string
	loaded            bool
	entries           []RequestLogEntry

	settings requestLogSettings

	sequence      uint64
	journalBytes  int64
	journalEvents int
	snapshotBytes int64
	needsSnapshot bool
	stateRevision uint64
}

func newRequestLogStore() *requestLogStore {
	return &requestLogStore{now: time.Now, settings: defaultRequestLogSettings()}
}

// scheduleFlush coalesces deferred persistence without leaving a worker
// goroutine attached to every service instance. A shutdown flush remains
// synchronous, so the delayed task is only a durability optimization.
func (s *requestLogStore) scheduleFlush() {
	s.persistScheduleMu.Lock()
	if s.persistScheduled {
		s.persistScheduleMu.Unlock()
		return
	}
	s.persistScheduled = true
	generation := s.persistGeneration
	done, cancel := make(chan struct{}), make(chan struct{})
	s.persistDone, s.persistCancel = done, cancel
	s.persistScheduleMu.Unlock()
	go func() {
		finishLocked := func() {
			if s.persistDone == done {
				s.persistScheduled = false
				s.persistDone, s.persistCancel = nil, nil
				close(done)
			}
		}
		defer func() {
			s.persistScheduleMu.Lock()
			finishLocked()
			s.persistScheduleMu.Unlock()
		}()
		for {
			s.persistScheduleMu.Lock()
			delay := s.persistRetryDelay
			current := s.persistGeneration == generation
			s.persistScheduleMu.Unlock()
			if !current {
				return
			}
			if delay <= 0 {
				delay = 25 * time.Millisecond
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-cancel:
				timer.Stop()
				return
			}
			timer.Stop()
			err := s.flush()
			s.persistScheduleMu.Lock()
			if s.persistGeneration != generation {
				s.persistScheduleMu.Unlock()
				return
			}
			if err != nil {
				s.persistRetryDelay = 2 * time.Second
			} else {
				s.persistRetryDelay = 0
			}
			if !s.persistencePending() {
				// Publish idle while holding the scheduling lock: a record after
				// this check can install a fresh task without losing its wakeup.
				finishLocked()
				s.persistScheduleMu.Unlock()
				return
			}
			s.persistScheduleMu.Unlock()
		}
	}()
}

func (s *requestLogStore) persistencePending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded && s.dir != "" && (s.needsSnapshot || s.journalEvents > 0)
}

// cancelScheduledFlush prevents an old delayed task from touching a directory
// after configure/clear/quiesce has moved to a new lifecycle boundary.
func (s *requestLogStore) cancelScheduledFlush() {
	s.persistScheduleMu.Lock()
	s.persistGeneration++
	cancel := s.persistCancel
	done := s.persistDone
	if cancel != nil {
		close(cancel)
		s.persistCancel = nil
	}
	s.persistScheduleMu.Unlock()
	if done != nil {
		<-done
	}
}

// configure loads the store once per data directory and always applies the
// latest policy. Corrupt files fall back to an empty, writable store; the error
// is returned only so callers can observe it, never to fail registration.
func (s *requestLogStore) configure(dir string, settings requestLogSettings) error {
	s.cancelScheduledFlush()
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	s.mu.Lock()
	if dir == "" {
		s.dir = ""
		s.loaded = false
		s.entries = nil
		s.sequence = 0
		s.journalBytes = 0
		s.journalEvents = 0
		s.snapshotBytes = 0
		s.needsSnapshot = false
		s.settings = normalizeRequestLogSettings(settings)
		s.stateRevision++
		s.mu.Unlock()
		return nil
	}
	s.settings = normalizeRequestLogSettings(settings)
	if s.loaded && filepath.Clean(dir) == filepath.Clean(s.dir) {
		changed := s.pruneLocked(s.now())
		if changed {
			s.stateRevision++
			s.needsSnapshot = true
		}
		pending := s.needsSnapshot || s.journalEvents > 0
		s.mu.Unlock()
		if pending {
			s.scheduleFlush()
		}
		return nil
	}
	if s.now == nil {
		s.now = time.Now
	}
	settings = s.settings
	now, keys := s.now, s.keys
	s.mu.Unlock()

	// Read into private state. Slow directory/read/truncate operations never
	// hold a lock used by record(), so inference can finish during recovery.
	loaded := newRequestLogStore()
	loaded.dir, loaded.settings, loaded.now, loaded.keys = dir, settings, now, keys
	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		s.mu.Lock()
		s.dir, s.loaded, s.entries = dir, false, nil
		s.sequence, s.journalBytes, s.journalEvents, s.snapshotBytes = 0, 0, 0, 0
		s.needsSnapshot = false
		s.stateRevision++
		s.mu.Unlock()
		return err
	}
	err = loaded.loadLocked()
	s.mu.Lock()
	s.dir, s.loaded, s.entries = dir, true, loaded.entries
	s.sequence, s.journalBytes = loaded.sequence, loaded.journalBytes
	s.journalEvents, s.snapshotBytes = loaded.journalEvents, loaded.snapshotBytes
	s.needsSnapshot = loaded.needsSnapshot
	s.stateRevision++
	pending := s.needsSnapshot || s.journalEvents > 0
	s.mu.Unlock()
	if pending {
		s.scheduleFlush()
	}
	return err
}

func (s *requestLogStore) setSettings(settings requestLogSettings) {
	s.persistCommitMu.RLock()
	s.mu.Lock()
	s.settings = normalizeRequestLogSettings(settings)
	changed := s.pruneLocked(s.now())
	if changed {
		s.needsSnapshot = true
		s.stateRevision++
	}
	pending := changed && s.loaded && s.dir != ""
	s.mu.Unlock()
	s.persistCommitMu.RUnlock()
	if pending {
		s.scheduleFlush()
	}
}

func normalizeRequestLogSettings(in requestLogSettings) requestLogSettings {
	if in.RetentionDays <= 0 || in.RetentionDays > maxRequestLogRetentionDays {
		in.RetentionDays = defaultRequestLogRetentionDays
	}
	if in.MaxEntries <= 0 || in.MaxEntries > maxRequestLogMaxEntries {
		in.MaxEntries = defaultRequestLogMaxEntries
	}
	return in
}

func (s *requestLogStore) loadLocked() error {
	s.entries = nil
	s.sequence = 0
	s.journalBytes = 0
	s.journalEvents = 0
	s.snapshotBytes = 0
	s.needsSnapshot = false

	snapshotPath := filepath.Join(s.dir, requestLogSnapshotName)
	if data, err := readRequestLogFile(snapshotPath, requestLogSnapshotMaxBytes); err == nil {
		var snapshot requestLogSnapshot
		if json.Unmarshal(data, &snapshot) != nil || snapshot.Version != requestLogSnapshotVersion {
			s.fallbackLocked()
			return errors.New("request log snapshot is corrupt")
		}
		s.entries = sanitizeRequestLogEntries(snapshot.Logs, s.currentKeys()...)
		if !reflect.DeepEqual(snapshot.Logs, s.entries) {
			s.needsSnapshot = true
		}
		s.sequence = snapshot.Sequence
		s.snapshotBytes = int64(len(data))
	} else if !errors.Is(err, os.ErrNotExist) {
		s.fallbackLocked()
		return fmt.Errorf("read request log snapshot: %w", err)
	}

	if err := s.loadJournalLocked(); err != nil {
		s.needsSnapshot = true
		if s.pruneLocked(s.now()) {
			s.needsSnapshot = true
		}
		return err
	}
	s.sortLocked()
	if s.pruneLocked(s.now()) {
		s.needsSnapshot = true
	}
	return nil
}

// fallbackLocked discards a damaged snapshot and recovers the valid journal
// prefix. Its highest sequence must survive compaction so an old clear can never
// erase a new record after a crash between snapshot publication and truncation.
func (s *requestLogStore) fallbackLocked() {
	s.entries = nil
	s.sequence = 0
	s.journalBytes = 0
	s.journalEvents = 0
	s.snapshotBytes = 0
	s.needsSnapshot = true
	_ = s.loadJournalLocked()
	s.pruneRetentionLocked(s.now())
}

func (s *requestLogStore) loadJournalLocked() error {
	path := filepath.Join(s.dir, requestLogJournalName)
	data, err := readRequestLogFile(path, requestLogJournalMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		if info, statErr := os.Stat(path); statErr == nil {
			s.journalBytes = info.Size()
		}
		return err
	}
	s.journalBytes = int64(len(data))
	s.needsSnapshot = len(data) > 0 || s.needsSnapshot
	lastJournalSequence := uint64(0)
	applied := false
	remaining := data
	for len(remaining) > 0 {
		index := bytes.IndexByte(remaining, '\n')
		if index < 0 {
			// A killed writer may leave a partial last line; truncate it away so
			// the next append starts on a clean boundary.
			break
		}
		line := remaining[:index+1]
		var event requestLogJournalEvent
		if err := json.Unmarshal(line, &event); err != nil || !validRequestLogJournalEvent(event) {
			return errors.New("request log journal is corrupt")
		}
		if lastJournalSequence != 0 && event.Sequence < lastJournalSequence {
			return errors.New("request log journal sequence regressed")
		}
		// Clear is an absolute state transition. It must survive a crash even
		// when records reserved later sequence numbers before the next snapshot.
		// Replaying an equal clear is idempotent; older events are already in the
		// snapshot and newer events are applied in file order.
		if event.Sequence > s.sequence || (event.Sequence == s.sequence && event.Clear) {
			s.applyJournalEventLocked(event)
			s.sequence = event.Sequence
			s.needsSnapshot = true
			applied = true
		}
		lastJournalSequence = event.Sequence
		remaining = remaining[index+1:]
	}
	s.journalEvents = 0
	if applied {
		s.needsSnapshot = true
	}
	return nil
}

func validRequestLogJournalEvent(event requestLogJournalEvent) bool {
	return event.Sequence > 0 && (event.Clear || event.Entry != nil)
}

func (s *requestLogStore) applyJournalEventLocked(event requestLogJournalEvent) {
	if event.Clear {
		s.entries = nil
	}
	if event.Entry != nil {
		if entry, ok := sanitizeRequestLogEntry(*event.Entry, s.currentKeys()...); ok {
			s.entries = append(s.entries, entry)
		}
	}
	settings := s.settings
	if event.RetentionDays > 0 && event.RetentionDays <= maxRequestLogRetentionDays {
		settings.RetentionDays = event.RetentionDays
	}
	if event.MaxEntries > 0 && event.MaxEntries <= maxRequestLogMaxEntries {
		settings.MaxEntries = event.MaxEntries
	}
	cutoff := s.now().Add(-time.Duration(settings.RetentionDays) * 24 * time.Hour)
	kept := s.entries[:0]
	for _, entry := range s.entries {
		if !entry.StartedAt.Before(cutoff) {
			kept = append(kept, entry)
		}
	}
	s.entries = kept
	s.sortLocked()
	if len(s.entries) > settings.MaxEntries {
		s.entries = s.entries[len(s.entries)-settings.MaxEntries:]
	}
}

func (s *requestLogStore) sortLocked() {
	sort.SliceStable(s.entries, func(i, j int) bool {
		return s.entries[i].StartedAt.Before(s.entries[j].StartedAt)
	})
}

func (s *requestLogStore) pruneRetentionLocked(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-time.Duration(s.settings.RetentionDays) * 24 * time.Hour)
	kept := s.entries[:0]
	for _, entry := range s.entries {
		if !entry.StartedAt.Before(cutoff) {
			kept = append(kept, entry)
		}
	}
	s.entries = kept
	s.sortLocked()
	if len(s.entries) > s.settings.MaxEntries {
		s.entries = s.entries[len(s.entries)-s.settings.MaxEntries:]
	}
}

func (s *requestLogStore) pruneLocked(now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	before := append([]RequestLogEntry(nil), s.entries...)
	cutoff := now.Add(-time.Duration(s.settings.RetentionDays) * 24 * time.Hour)
	keys := s.currentKeys()
	kept := s.entries[:0]
	for _, entry := range s.entries {
		clean, ok := sanitizeRequestLogEntry(entry, keys...)
		if !ok {
			continue
		}
		if !clean.StartedAt.Before(cutoff) {
			kept = append(kept, clean)
		}
	}
	s.entries = kept
	s.sortLocked()
	if len(s.entries) > s.settings.MaxEntries {
		s.entries = s.entries[len(s.entries)-s.settings.MaxEntries:]
	}
	return !reflect.DeepEqual(before, s.entries)
}

func (s *requestLogStore) currentKeys() []string {
	if s.keys == nil {
		return nil
	}
	return s.keys()
}

func (s *requestLogStore) record(entry RequestLogEntry) {
	s.persistCommitMu.RLock()
	s.mu.Lock()
	if !s.loaded || !s.settings.Enabled || s.dir == "" {
		s.mu.Unlock()
		s.persistCommitMu.RUnlock()
		return
	}
	entry, ok := sanitizeRequestLogEntry(entry, s.currentKeys()...)
	if !ok {
		s.mu.Unlock()
		s.persistCommitMu.RUnlock()
		return
	}
	s.sequence++
	s.entries = append(s.entries, entry)
	s.pruneRetentionLocked(s.now())
	// Disk I/O is deliberately deferred. The bounded in-memory entry is enough
	// for the inference response path; the coalescing worker persists it shortly.
	s.needsSnapshot = true
	s.stateRevision++
	s.mu.Unlock()
	s.persistCommitMu.RUnlock()
	s.scheduleFlush()
}

type requestLogPersistState struct {
	dir           string
	sequence      uint64
	revision      uint64
	journalBytes  int64
	journalEvents int
	entries       []RequestLogEntry
}

func (s *requestLogStore) persistStateLocked() requestLogPersistState {
	return requestLogPersistState{
		dir:           s.dir,
		sequence:      s.sequence,
		revision:      s.stateRevision,
		journalBytes:  s.journalBytes,
		journalEvents: s.journalEvents,
		entries:       append([]RequestLogEntry(nil), s.entries...),
	}
}

func (s *requestLogStore) persistStateMatchesLocked(state requestLogPersistState) bool {
	return s.loaded && s.dir == state.dir && s.sequence == state.sequence && s.stateRevision == state.revision
}

type requestLogAppendResult struct {
	offset    int64
	committed bool
	err       error
}

type requestLogJournalWriter interface {
	io.Writer
	Sync() error
	Close() error
}

// renameFile publishes a staged file over its destination. Every durable write
// in this plugin goes through it so a test can inject the Windows-only failure
// that cannot be reproduced on other platforms: Windows cannot replace a
// destination that still has an open handle and reports "Access is denied".
var renameFile = func(tmp, dst string) error { return os.Rename(tmp, dst) }

// publishRetryPauses is the bounded backoff for a Windows sharing violation. A
// stuck handle belongs to this process or a scanner, so a few short pauses
// resolve the common case without stalling a request.
var publishRetryPauses = []time.Duration{5 * time.Millisecond, 20 * time.Millisecond, 60 * time.Millisecond}

// withShareRetry retries op while it fails. Windows reports "Access is denied"
// when a destination is still open, and the same call succeeds once the handle
// is released; on other platforms the first call already succeeded.
func withShareRetry(op func() error) error {
	err := op()
	for _, pause := range publishRetryPauses {
		if err == nil {
			return nil
		}
		time.Sleep(pause)
		err = op()
	}
	return err
}

// publishStagedFile retries the rename that swaps a staged file into place.
// Callers must have closed any handle on dst: an open destination is the very
// thing Windows refuses to replace, so retrying it alone would not help.
func publishStagedFile(tmp, dst string) error {
	return withShareRetry(func() error { return renameFile(tmp, dst) })
}

func appendRequestLogJournal(path string, offset int64, event requestLogJournalEvent) requestLogAppendResult {
	return appendRequestLogJournalWithOpen(path, offset, event, func(name string, flags int, mode os.FileMode) (requestLogJournalWriter, error) {
		return os.OpenFile(name, flags, mode)
	})
}

func appendRequestLogJournalWithOpen(path string, offset int64, event requestLogJournalEvent, open func(string, int, os.FileMode) (requestLogJournalWriter, error)) requestLogAppendResult {
	result := requestLogAppendResult{offset: offset}
	line, err := json.Marshal(event)
	if err != nil {
		result.err = err
		return result
	}
	line = append(line, '\n')
	if offset < 0 || offset+int64(len(line)) > requestLogJournalMaxBytes {
		result.err = errors.New("request log journal requires compaction")
		return result
	}
	// Stage the complete next journal. A failed write/sync/close leaves the
	// existing file untouched; no fallible in-place rollback can strand a valid
	// clear tombstone on disk while the caller retains the cleared entries.
	var source *os.File
	if source, err = os.Open(path); err != nil && !(errors.Is(err, os.ErrNotExist) && offset == 0) {
		result.err = err
		return result
	}
	if source != nil {
		defer source.Close()
		info, statErr := source.Stat()
		if statErr != nil {
			result.err = statErr
			return result
		}
		if !info.Mode().IsRegular() || info.Size() < offset {
			result.err = errors.New("request log journal unexpectedly shortened or not a regular file")
			return result
		}
	}
	// The source is closed before the swap because Windows cannot rename over a
	// file that is still open; every read of it is finished by then.
	tmp := path + ".tmp-" + newID()
	defer os.Remove(tmp)
	f, err := open(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		result.err = err
		return result
	}
	if source != nil && offset > 0 {
		_, err = io.CopyN(f, source, offset)
	}
	if err == nil {
		var written int
		written, err = f.Write(line)
		if err == nil && written != len(line) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		result.err = err
		return result
	}
	if source != nil {
		_ = source.Close()
		source = nil
	}
	if err = publishStagedFile(tmp, path); err != nil {
		result.err = err
		return result
	}
	// The synced candidate is now published. Any later directory/close error
	// cannot make the in-memory clear revert while replay sees the tombstone.
	result.offset = offset + int64(len(line))
	result.committed = true
	result.err = syncRequestLogDirectory(filepath.Dir(path))
	return result
}

func syncRequestLogDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// truncateRequestLogJournal shortens the journal in place, which keeps the
// inode so a concurrent append sharing the descriptor stays valid. Windows
// cannot truncate a file another handle holds open, so the attempt is retried.
func truncateRequestLogJournal(path string, offsets ...int64) error {
	return withShareRetry(func() error { return truncateRequestLogJournalOnce(path, offsets...) })
}

func truncateRequestLogJournalOnce(path string, offsets ...int64) error {
	flags := os.O_CREATE | os.O_WRONLY
	if len(offsets) == 0 {
		flags |= os.O_TRUNC
	}
	journal, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	if len(offsets) > 0 {
		if err := journal.Truncate(offsets[0]); err != nil {
			_ = journal.Close()
			return err
		}
	}
	return errors.Join(journal.Sync(), journal.Close())
}

func prepareRequestLogSnapshot(dir string, sequence uint64, entries []RequestLogEntry) (string, []byte, error) {
	if entries == nil {
		entries = []RequestLogEntry{}
	}
	data, err := json.Marshal(requestLogSnapshot{Version: requestLogSnapshotVersion, Sequence: sequence, Logs: entries})
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, requestLogSnapshotName)
	tmp := path + ".tmp-" + newID()
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(tmp)
		return "", nil, err
	}
	return tmp, data, nil
}

func publishRequestLogSnapshot(dir, tmp string) (bool, error) {
	if err := publishStagedFile(tmp, filepath.Join(dir, requestLogSnapshotName)); err != nil {
		return false, err
	}
	if runtime.GOOS != "windows" {
		f, err := os.Open(dir)
		if err != nil {
			return true, err
		}
		return true, errors.Join(f.Sync(), f.Close())
	}
	return true, nil
}

func writeRequestLogSnapshot(dir string, sequence uint64, entries []RequestLogEntry) ([]byte, error) {
	tmp, data, err := prepareRequestLogSnapshot(dir, sequence, entries)
	if err != nil {
		return nil, err
	}
	if _, err := publishRequestLogSnapshot(dir, tmp); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	return data, nil
}
func (s *requestLogStore) settingsView() map[string]any {
	s.persistCommitMu.RLock()
	s.mu.Lock()
	view := s.retentionViewLocked()
	s.mu.Unlock()
	s.persistCommitMu.RUnlock()
	return view
}
func (s *requestLogStore) flush() error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	return s.flushLocked()
}

// flushLocked is serialized with clear/configure by persistMu.
func (s *requestLogStore) flushLocked() error {
	s.persistCommitMu.RLock()
	s.mu.Lock()
	if s.dir == "" || !s.loaded || (!s.needsSnapshot && s.journalEvents == 0) {
		s.mu.Unlock()
		s.persistCommitMu.RUnlock()
		return nil
	}
	state := s.persistStateLocked()
	s.mu.Unlock()
	s.persistCommitMu.RUnlock()

	tmp, data, err := prepareRequestLogSnapshot(state.dir, state.sequence, state.entries)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.Remove(tmp)
		}
	}()

	// Only the short state check is synchronized with record/query. Rename and
	// journal truncation are intentionally outside persistCommitMu: metadata I/O
	// can block on slow storage just like fsync, and a concurrent record is safe
	// because the revision check below keeps the store dirty for another pass.
	s.persistCommitMu.RLock()
	s.mu.Lock()
	matched := s.persistStateMatchesLocked(state)
	s.mu.Unlock()
	s.persistCommitMu.RUnlock()
	if !matched {
		return nil
	}
	if err := publishStagedFile(tmp, filepath.Join(state.dir, requestLogSnapshotName)); err != nil {
		return err
	}
	published = true
	if runtime.GOOS != "windows" {
		f, err := os.Open(state.dir)
		if err != nil {
			return err
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return err
		}
	}

	journalNeedsTruncate := state.journalBytes > 0 || state.journalEvents > 0
	if journalNeedsTruncate {
		if err := truncateRequestLogJournal(filepath.Join(state.dir, requestLogJournalName)); err != nil {
			s.persistCommitMu.RLock()
			s.mu.Lock()
			s.needsSnapshot = true
			s.mu.Unlock()
			s.persistCommitMu.RUnlock()
			return err
		}
	}

	s.persistCommitMu.RLock()
	s.mu.Lock()
	matched = s.persistStateMatchesLocked(state)
	if journalNeedsTruncate && s.journalBytes == state.journalBytes && s.journalEvents == state.journalEvents {
		// No new clear can race this operation while persistMu is held; a record
		// may still make the snapshot stale, but it does not append to the journal.
		s.journalBytes = 0
		s.journalEvents = 0
	}
	if matched {
		s.snapshotBytes = int64(len(data))
		s.needsSnapshot = false
	} else {
		s.needsSnapshot = true
	}
	s.mu.Unlock()
	s.persistCommitMu.RUnlock()
	return nil
}

func (s *requestLogStore) clear() error {
	s.cancelScheduledFlush()
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	s.mu.Lock()
	recoveredJournal := s.journalBytes > 0 && s.journalEvents == 0
	s.mu.Unlock()
	if recoveredJournal {
		// A recovered/corrupt/oversized suffix must be compacted before a new
		// tombstone is appended, otherwise replay could stop before that clear.
		if err := s.flushLocked(); err != nil {
			s.scheduleFlush()
			return err
		}
		s.mu.Lock()
		pendingJournal := s.journalBytes > 0 && s.journalEvents == 0
		s.mu.Unlock()
		if pendingJournal {
			s.scheduleFlush()
			return errors.New("request log recovery is still being compacted")
		}
	}
	s.mu.Lock()
	if s.dir == "" || !s.loaded {
		s.mu.Unlock()
		return fail(http.StatusConflict, "plugin is not configured")
	}
	s.stateRevision++
	s.sequence++
	s.needsSnapshot = true
	state := s.persistStateLocked()
	cleared := make(map[string]int, len(state.entries))
	for _, entry := range state.entries {
		cleared[entry.ID]++
	}
	settings := s.settings
	event := requestLogJournalEvent{
		Sequence:      s.sequence,
		RetentionDays: settings.RetentionDays,
		MaxEntries:    settings.MaxEntries,
		Clear:         true,
	}
	s.mu.Unlock()

	journalPath := filepath.Join(state.dir, requestLogJournalName)
	appendJournal := s.appendJournal
	if appendJournal == nil {
		appendJournal = func(path string, offset int64, event requestLogJournalEvent) requestLogAppendResult {
			return appendRequestLogJournal(path, offset, event)
		}
	}
	appendResult := appendJournal(journalPath, state.journalBytes, event)
	if appendResult.err != nil && !appendResult.committed {
		s.scheduleFlush()
		return appendResult.err
	}

	// The durable tombstone is the clear transaction's commit record. The
	// sequence was reserved before the I/O, so concurrent records receive a
	// strictly newer value and cannot collide with this clear during replay.
	s.mu.Lock()
	removeRequestLogEntriesLocked(&s.entries, cleared)
	s.stateRevision++
	s.journalBytes = appendResult.offset
	s.journalEvents++
	s.needsSnapshot = true
	s.mu.Unlock()
	s.scheduleFlush()
	return nil
}

func removeRequestLogEntriesLocked(entries *[]RequestLogEntry, cleared map[string]int) {
	if len(*entries) == 0 || len(cleared) == 0 {
		return
	}
	remaining := make(map[string]int, len(cleared))
	for id, count := range cleared {
		remaining[id] = count
	}
	kept := (*entries)[:0]
	for _, entry := range *entries {
		if remaining[entry.ID] > 0 {
			remaining[entry.ID]--
			continue
		}
		kept = append(kept, entry)
	}
	*entries = kept
}

type requestLogFilter struct {
	limit      int
	offset     int
	status     string
	model      string
	credential string
	attempted  string
	search     string
	since      *time.Time
	until      *time.Time
}

func (f requestLogFilter) matches(entry RequestLogEntry) bool {
	if f.status != "" && entry.Status != f.status {
		return false
	}
	if f.model != "" && entry.Model != f.model && entry.UpstreamModel != f.model {
		return false
	}
	if f.credential != "" && entry.CredentialID != f.credential {
		return false
	}
	if f.attempted == "yes" && entry.UpstreamAttempted != nil && !*entry.UpstreamAttempted {
		return false
	}
	if f.attempted == "no" && (entry.UpstreamAttempted == nil || *entry.UpstreamAttempted) {
		return false
	}
	if f.search != "" && !requestLogMatchesSearch(entry, f.search) {
		return false
	}
	if f.since != nil && entry.StartedAt.Before(*f.since) {
		return false
	}
	if f.until != nil && entry.StartedAt.After(*f.until) {
		return false
	}
	return true
}

// requestLogMatchesSearch implements the console's free-text filter over safe
// metadata fields. The needle is already lowercased and length-checked by the
// query parser; the haystacks are bounded at record time.
func requestLogMatchesSearch(entry RequestLogEntry, needle string) bool {
	fields := []string{entry.Model, entry.UpstreamModel, entry.CredentialID, entry.RequestID}
	if entry.ErrorKind != nil {
		fields = append(fields, *entry.ErrorKind)
	}
	for _, field := range fields {
		if field != "" && strings.Contains(strings.ToLower(field), needle) {
			return true
		}
	}
	return false
}

func (s *requestLogStore) query(filter requestLogFilter) ([]RequestLogEntry, int, map[string]any) {
	s.persistCommitMu.RLock()
	s.mu.Lock()
	changed := s.pruneLocked(s.now())
	if changed {
		s.needsSnapshot = true
		s.stateRevision++
	}
	matched := make([]RequestLogEntry, 0, len(s.entries))
	for i := len(s.entries) - 1; i >= 0; i-- {
		if filter.matches(s.entries[i]) {
			matched = append(matched, s.entries[i])
		}
	}
	total := len(matched)
	start := filter.offset
	if start > total {
		start = total
	}
	end := start + filter.limit
	if end > total {
		end = total
	}
	page := append([]RequestLogEntry(nil), matched[start:end]...)
	if page == nil {
		page = []RequestLogEntry{}
	}
	retention := s.retentionViewLocked()
	s.mu.Unlock()
	s.persistCommitMu.RUnlock()
	if changed {
		s.scheduleFlush()
	}
	return page, total, retention
}

func (s *requestLogStore) retentionViewLocked() map[string]any {
	view := map[string]any{
		"enabled":        s.settings.Enabled,
		"retention_days": s.settings.RetentionDays,
		"max_entries":    s.settings.MaxEntries,
		"entries":        len(s.entries),
		"oldest":         nil,
		"newest":         nil,
	}
	if len(s.entries) > 0 {
		view["oldest"] = s.entries[0].StartedAt
		view["newest"] = s.entries[len(s.entries)-1].StartedAt
	}
	return view
}

type requestLogTokenTotals struct {
	Input  *int64 `json:"input"`
	Output *int64 `json:"output"`
	Total  *int64 `json:"total"`
}

type requestLogModelStats struct {
	Model             string `json:"model"`
	Total             int    `json:"total"`
	Success           int    `json:"success"`
	Failed            int    `json:"failed"`
	UpstreamAttempted int    `json:"upstream_attempted"`
	LocalRejected     int    `json:"local_rejected"`
	InputTokens       *int64 `json:"input_tokens"`
	OutputTokens      *int64 `json:"output_tokens"`
	TotalTokens       *int64 `json:"total_tokens"`
}

func addToken(dst **int64, value *int64) {
	if value == nil {
		return
	}
	if *dst == nil {
		sum := *value
		*dst = &sum
		return
	}
	sum := **dst + *value
	*dst = &sum
}

func (s *requestLogStore) stats() map[string]any {
	return s.statsFor(nil, nil)
}

// statsFor aggregates the retained rows, optionally limited to those that
// started inside [since, until]. The range only narrows the retained set: rows
// already pruned by the retention policy are not part of any answer.
func (s *requestLogStore) statsFor(since, until *time.Time) map[string]any {
	s.persistCommitMu.RLock()
	s.mu.Lock()
	changed := s.pruneLocked(s.now())
	if changed {
		s.needsSnapshot = true
		s.stateRevision++
	}
	success, failed, streams := 0, 0, 0
	upstreamAttempted, localRejected := 0, 0
	var input, output, total, cached, reasoning *int64
	var cacheInput int64
	cacheSamples := 0
	var rateTokens, rateWindowMS int64
	rateSamples := 0
	var firstTokenSum int64
	firstTokenSamples := 0
	withUsage, withoutUsage := 0, 0
	models := map[string]*requestLogModelStats{}
	window := requestLogFilter{since: since, until: until}
	for _, entry := range s.entries {
		if !window.matches(entry) {
			continue
		}
		if entry.Status == "success" {
			success++
		} else {
			failed++
		}
		attempted := entry.UpstreamAttempted == nil || *entry.UpstreamAttempted
		if attempted {
			upstreamAttempted++
		} else {
			localRejected++
		}
		if entry.Stream {
			streams++
		}
		if entry.InputTokens == nil && entry.OutputTokens == nil && entry.TotalTokens == nil {
			withoutUsage++
		} else {
			withUsage++
		}
		addToken(&input, entry.InputTokens)
		addToken(&output, entry.OutputTokens)
		addToken(&total, entry.TotalTokens)
		addToken(&cached, entry.CachedTokens)
		addToken(&reasoning, entry.ReasoningTokens)
		// The hit rate is taken over the rows that actually reported a cached
		// count. A row that says nothing about caching is unknown, not a miss.
		if entry.CachedTokens != nil && entry.InputTokens != nil && *entry.InputTokens > 0 {
			cacheInput += *entry.InputTokens
			cacheSamples++
		}
		if tokens, windowMS, ok := requestLogRateSample(entry); ok {
			rateTokens += tokens
			rateWindowMS += windowMS
			rateSamples++
		}
		if entry.FirstTokenMS != nil {
			firstTokenSum += *entry.FirstTokenMS
			firstTokenSamples++
		}

		model := entry.Model
		if model == "" {
			model = "unknown"
		}
		modelStats := models[model]
		if modelStats == nil {
			modelStats = &requestLogModelStats{Model: model}
			models[model] = modelStats
		}
		modelStats.Total++
		if entry.Status == "success" {
			modelStats.Success++
		} else {
			modelStats.Failed++
		}
		if attempted {
			modelStats.UpstreamAttempted++
		} else {
			modelStats.LocalRejected++
		}
		addToken(&modelStats.InputTokens, entry.InputTokens)
		addToken(&modelStats.OutputTokens, entry.OutputTokens)
		addToken(&modelStats.TotalTokens, entry.TotalTokens)
	}
	perModel := make([]*requestLogModelStats, 0, len(models))
	for _, modelStats := range models {
		perModel = append(perModel, modelStats)
	}
	sort.SliceStable(perModel, func(i, j int) bool {
		if perModel[i].Total != perModel[j].Total {
			return perModel[i].Total > perModel[j].Total
		}
		return perModel[i].Model < perModel[j].Model
	})

	totalRequests := success + failed
	var successRate *float64
	if totalRequests > 0 {
		rate := float64(success) / float64(totalRequests)
		successRate = &rate
	}
	var cacheRate *float64
	if cacheSamples > 0 && cacheInput > 0 && cached != nil {
		rate := float64(*cached) / float64(cacheInput)
		cacheRate = &rate
	}
	// Total output over total generation time, not the mean of per-row rates:
	// a long generation weighs more than a three-token reply.
	var outputRate *float64
	if rateSamples > 0 && rateWindowMS > 0 {
		rate := float64(rateTokens) * 1000 / float64(rateWindowMS)
		outputRate = &rate
	}
	var firstTokenAverage *int64
	if firstTokenSamples > 0 {
		average := firstTokenSum / int64(firstTokenSamples)
		firstTokenAverage = &average
	}
	result := map[string]any{
		"total":              totalRequests,
		"success":            success,
		"failed":             failed,
		"streams":            streams,
		"upstream_attempted": upstreamAttempted,
		"local_rejected":     localRejected,
		"success_rate":       successRate,
		"tokens": map[string]any{
			"input":                  input,
			"output":                 output,
			"total":                  total,
			"cached":                 cached,
			"reasoning":              reasoning,
			"cache_rate":             cacheRate,
			"cache_samples":          cacheSamples,
			"requests_with_usage":    withUsage,
			"requests_without_usage": withoutUsage,
		},
		"output_tokens_per_second": outputRate,
		"output_rate_samples":      rateSamples,
		"first_token_ms":           firstTokenAverage,
		"first_token_samples":      firstTokenSamples,
		"range":                    map[string]any{"since": since, "until": until},
		"models":                   perModel,
		"retention":                s.retentionViewLocked(),
	}
	s.mu.Unlock()
	s.persistCommitMu.RUnlock()
	if changed {
		s.scheduleFlush()
	}
	return result
}

// requestLogDraft is filled while a request runs. Only metadata is copied in;
// the recorder never receives a request or response body.
type requestLogDraft struct {
	started           time.Time
	model             string
	upstreamModel     string
	credentialID      string
	stream            bool
	httpStatus        int
	requestID         string
	upstreamAttempted bool
	secrets           []string
	usage             tokenUsage
	reasoningEffort   string
	reasoningFrom     string
	reasoningTo       string
	reasoningMapped   bool
	// firstTokenMS is set by the stream pump when the first frame carrying
	// output arrives. It stays nil for buffered requests.
	firstTokenMS *int64
}

func (s *Service) recordRequestLog(draft *requestLogDraft, requestErr error) {
	if draft == nil || s.requestLogs == nil {
		return
	}
	defer func() {
		// Logging must never change the inference result, even if storage panics.
		_ = recover()
	}()
	finished := time.Now()
	keys := append(s.metadataSecrets(), draft.secrets...)
	entry := RequestLogEntry{
		ID: newID(), StartedAt: draft.started.UTC(),
		FinishedAt:      finished.UTC(),
		DurationMS:      finished.Sub(draft.started).Milliseconds(),
		Model:           requestLogText(draft.model, 512, keys...),
		UpstreamModel:   requestLogText(draft.upstreamModel, 512, keys...),
		CredentialID:    requestLogText(draft.credentialID, 128, keys...),
		Stream:          draft.stream,
		Status:          "success",
		RequestID:       s.safeRequestIDWithKeys(draft.requestID, keys...),
		InputTokens:     sanitizeToken(draft.usage.Input),
		OutputTokens:    sanitizeToken(draft.usage.Output),
		TotalTokens:     sanitizeToken(draft.usage.Total),
		FirstTokenMS:    draft.firstTokenMS,
		CachedTokens:    draft.usage.Cached,
		ReasoningTokens: draft.usage.Reasoning,
	}
	// Both values come from closed strength sets the plugin itself operates on,
	// never from upstream text, so they are safe to store as-is.
	entry.ReasoningEffort = draft.reasoningEffort
	if draft.reasoningMapped {
		entry.ReasoningMapping = &RequestLogReasoningMapping{From: draft.reasoningFrom, To: draft.reasoningTo}
	}
	if entry.DurationMS < 0 {
		entry.DurationMS = 0
	}
	attempted := draft.upstreamAttempted
	var attemptedFromError interface{ UpstreamAttempted() bool }
	if errors.As(requestErr, &attemptedFromError) && attemptedFromError.UpstreamAttempted() {
		attempted = true
	}
	entry.UpstreamAttempted = &attempted
	if requestErr != nil {
		entry.Status = "error"
		entry.ErrorKind = requestLogErrorKind(requestErr, keys...)
	}
	if draft.httpStatus > 0 {
		status := draft.httpStatus
		entry.HTTPStatus = &status
	}
	s.requestLogs.record(entry)
}

func requestLogErrorKind(err error, keys ...string) *string {
	kind := streamErrorCode(err)
	if !safeDiagnosticKind(kind, keys) {
		kind = "upstream_error"
	}
	return &kind
}

func (s *Service) safeRequestID(value string) string {
	return s.safeRequestIDWithKeys(value, s.allKeys()...)
}

func (s *Service) safeRequestIDWithKeys(value string, keys ...string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	if redactComplete(value, keys...) != value {
		return ""
	}
	return value
}

func requestLogText(value string, limit int, keys ...string) string {
	value = strings.TrimSpace(value)
	value = redactComplete(value, keys...)
	if value == "" {
		return ""
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit])
	}
	return value
}

func sanitizeToken(value *int64) *int64 {
	if value == nil {
		return nil
	}
	n := *value
	if n < 0 || n > maxRequestLogTokenValue {
		return nil
	}
	return &n
}

func sanitizeRequestLogEntry(entry RequestLogEntry, keys ...string) (RequestLogEntry, bool) {
	entry.ID = requestLogText(entry.ID, 64, keys...)
	if entry.ID == "" {
		return RequestLogEntry{}, false
	}
	if entry.Status != "success" && entry.Status != "error" {
		return RequestLogEntry{}, false
	}
	if entry.StartedAt.IsZero() {
		entry.StartedAt = entry.FinishedAt
	}
	if entry.FinishedAt.IsZero() {
		entry.FinishedAt = entry.StartedAt
	}
	if entry.StartedAt.IsZero() {
		return RequestLogEntry{}, false
	}
	entry.StartedAt = entry.StartedAt.UTC()
	entry.FinishedAt = entry.FinishedAt.UTC()
	if entry.DurationMS < 0 {
		entry.DurationMS = 0
	}
	entry.Model = requestLogText(entry.Model, 512, keys...)
	entry.UpstreamModel = requestLogText(entry.UpstreamModel, 512, keys...)
	entry.CredentialID = requestLogText(entry.CredentialID, 128, keys...)
	if redactComplete(entry.RequestID, keys...) != entry.RequestID {
		entry.RequestID = ""
	} else {
		entry.RequestID = requestLogText(entry.RequestID, 128)
	}
	if entry.HTTPStatus != nil && (*entry.HTTPStatus < 100 || *entry.HTTPStatus > 599) {
		entry.HTTPStatus = nil
	}
	if entry.ErrorKind != nil {
		kind := *entry.ErrorKind
		if !safeDiagnosticKind(kind, keys) {
			entry.ErrorKind = nil
		} else {
			entry.ErrorKind = &kind
		}
	}
	// A strength is stored only when it is one the plugin itself recognises. A
	// hand-edited snapshot can therefore not smuggle arbitrary text into the
	// console through the reasoning column.
	if entry.ReasoningEffort != "" && !isReasoningSource(entry.ReasoningEffort) {
		entry.ReasoningEffort = ""
	}
	if entry.ReasoningMapping != nil {
		from, to := entry.ReasoningMapping.From, entry.ReasoningMapping.To
		if !isReasoningSource(from) || !isReasoningTarget(to) {
			entry.ReasoningMapping = nil
		} else {
			entry.ReasoningMapping = &RequestLogReasoningMapping{From: from, To: to}
		}
	}
	entry.InputTokens = sanitizeToken(entry.InputTokens)
	entry.OutputTokens = sanitizeToken(entry.OutputTokens)
	entry.TotalTokens = sanitizeToken(entry.TotalTokens)
	// A detail is a part of its total. One that is missing its total, or is
	// larger than it, cannot be shown as a share and is dropped rather than
	// clamped: an invented number is worse than an unknown one.
	entry.CachedTokens = sanitizeTokenDetail(entry.CachedTokens, entry.InputTokens)
	entry.ReasoningTokens = sanitizeTokenDetail(entry.ReasoningTokens, entry.OutputTokens)
	if entry.FirstTokenMS != nil {
		first := *entry.FirstTokenMS
		if first < 0 || first > entry.DurationMS {
			entry.FirstTokenMS = nil
		} else {
			entry.FirstTokenMS = &first
		}
	}
	return entry, true
}

func sanitizeTokenDetail(detail, total *int64) *int64 {
	detail = sanitizeToken(detail)
	if detail == nil || total == nil || *detail > *total {
		return nil
	}
	return detail
}

func sanitizeRequestLogEntries(entries []RequestLogEntry, keys ...string) []RequestLogEntry {
	out := make([]RequestLogEntry, 0, len(entries))
	for _, entry := range entries {
		if clean, ok := sanitizeRequestLogEntry(entry, keys...); ok {
			out = append(out, clean)
		}
	}
	return out
}

func readRequestLogFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("request log file exceeds size limit")
	}
	return data, nil
}

func writeRequestLogFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp-" + newID()
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := publishStagedFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if runtime.GOOS != "windows" {
		if dir, err := os.Open(filepath.Dir(path)); err == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
	return nil
}

// tokenUsage only carries values that actually appeared in an upstream usage
// object. A missing field stays nil and is serialized as null, never as zero.
type tokenUsage struct {
	Input  *int64
	Output *int64
	Total  *int64
	// Cached and Reasoning are optional details of Input and Output.
	Cached    *int64
	Reasoning *int64
}

func tokenUsageFromMap(root map[string]any) tokenUsage {
	usage, ok := root["usage"].(map[string]any)
	if !ok {
		return tokenUsage{}
	}
	return tokenUsage{
		Input:     tokenFromValue(usage["prompt_tokens"]),
		Output:    tokenFromValue(usage["completion_tokens"]),
		Total:     tokenFromValue(usage["total_tokens"]),
		Cached:    cachedTokensFrom(usage),
		Reasoning: tokenDetailFrom(usage, "completion_tokens_details", "reasoning_tokens"),
	}
}

func (u *tokenUsage) observe(root map[string]any) {
	usage, ok := root["usage"].(map[string]any)
	if !ok {
		return
	}
	if value := tokenFromValue(usage["prompt_tokens"]); value != nil {
		u.Input = value
	}
	if value := tokenFromValue(usage["completion_tokens"]); value != nil {
		u.Output = value
	}
	if value := tokenFromValue(usage["total_tokens"]); value != nil {
		u.Total = value
	}
	if value := cachedTokensFrom(usage); value != nil {
		u.Cached = value
	}
	if value := tokenDetailFrom(usage, "completion_tokens_details", "reasoning_tokens"); value != nil {
		u.Reasoning = value
	}
}

// tokenDetailFrom reads one count from an OpenAI usage detail object, e.g.
// usage.prompt_tokens_details.cached_tokens.
func tokenDetailFrom(usage map[string]any, object, field string) *int64 {
	details, ok := usage[object].(map[string]any)
	if !ok {
		return nil
	}
	return tokenFromValue(details[field])
}

// cachedTokensFrom prefers the OpenAI detail object and falls back to the flat
// prompt_cache_hit_tokens field DeepSeek-style upstreams report.
func cachedTokensFrom(usage map[string]any) *int64 {
	if value := tokenDetailFrom(usage, "prompt_tokens_details", "cached_tokens"); value != nil {
		return value
	}
	return tokenFromValue(usage["prompt_cache_hit_tokens"])
}

func tokenFromValue(value any) *int64 {
	var n int64
	switch v := value.(type) {
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return nil
		}
		n = parsed
	case float64:
		if v < 0 || v > float64(maxRequestLogTokenValue) || v != math.Trunc(v) {
			return nil
		}
		n = int64(v)
	case int64:
		n = v
	case int:
		n = int64(v)
	default:
		return nil
	}
	if n < 0 || n > maxRequestLogTokenValue {
		return nil
	}
	return &n
}

func parseRequestLogQuery(query url.Values) (requestLogFilter, error) {
	filter := requestLogFilter{limit: requestLogQueryDefaultLimit}
	value := func(key string) string {
		if query == nil {
			return ""
		}
		return strings.TrimSpace(query.Get(key))
	}
	if raw := value("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > requestLogQueryMaxLimit {
			return filter, fail(http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(requestLogQueryMaxLimit))
		}
		filter.limit = n
	}
	if raw := value("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > requestLogQueryMaxOffset {
			return filter, fail(http.StatusBadRequest, "offset must be between 0 and "+strconv.Itoa(requestLogQueryMaxOffset))
		}
		filter.offset = n
	}
	switch status := value("status"); status {
	case "", "success", "error":
		filter.status = status
	default:
		return filter, fail(http.StatusBadRequest, "status must be success or error")
	}
	if model := value("model"); model != "" {
		if len(model) > 512 || strings.IndexFunc(model, unicode.IsControl) >= 0 {
			return filter, fail(http.StatusBadRequest, "invalid model filter")
		}
		filter.model = model
	}
	if credential := value("credential_id"); credential != "" {
		if len(credential) > 128 || strings.IndexFunc(credential, unicode.IsControl) >= 0 {
			return filter, fail(http.StatusBadRequest, "invalid credential_id filter")
		}
		filter.credential = credential
	}
	switch attempted := value("attempted"); attempted {
	case "", "all":
	case "yes", "no":
		filter.attempted = attempted
	default:
		return filter, fail(http.StatusBadRequest, "attempted must be yes, no or all")
	}
	if search := value("q"); search != "" {
		if len(search) > 128 || strings.IndexFunc(search, unicode.IsControl) >= 0 {
			return filter, fail(http.StatusBadRequest, "invalid q filter")
		}
		filter.search = strings.ToLower(search)
	}
	if raw := value("since"); raw != "" {
		parsed, err := parseRequestLogTime(raw)
		if err != nil {
			return filter, fail(http.StatusBadRequest, "since must be RFC3339 or unix seconds")
		}
		filter.since = &parsed
	}
	if raw := value("until"); raw != "" {
		parsed, err := parseRequestLogTime(raw)
		if err != nil {
			return filter, fail(http.StatusBadRequest, "until must be RFC3339 or unix seconds")
		}
		filter.until = &parsed
	}
	if filter.since != nil && filter.until != nil && filter.since.After(*filter.until) {
		return filter, fail(http.StatusBadRequest, "since must not be after until")
	}
	return filter, nil
}

func parseRequestLogTime(raw string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed, nil
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed, nil
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
		return time.Unix(seconds, 0), nil
	}
	return time.Time{}, errors.New("invalid time")
}

// parseRequestLogRange reads the optional since/until pair for the statistics
// route. The validation is the same as the list filter's.
func parseRequestLogRange(query url.Values) (since, until *time.Time, err error) {
	read := func(key string) (*time.Time, error) {
		if query == nil {
			return nil, nil
		}
		raw := strings.TrimSpace(query.Get(key))
		if raw == "" {
			return nil, nil
		}
		parsed, parseErr := parseRequestLogTime(raw)
		if parseErr != nil {
			return nil, fail(http.StatusBadRequest, key+" must be RFC3339 or unix seconds")
		}
		parsed = parsed.UTC()
		return &parsed, nil
	}
	if since, err = read("since"); err != nil {
		return nil, nil, err
	}
	if until, err = read("until"); err != nil {
		return nil, nil, err
	}
	if since != nil && until != nil && since.After(*until) {
		return nil, nil, fail(http.StatusBadRequest, "since must not be after until")
	}
	return since, until, nil
}

func (s *Service) requestLogsRequest(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	defer s.done()
	filter, err := parseRequestLogQuery(r.Query)
	if err != nil {
		return managementError(statusOf(err), err.Error())
	}
	logs, total, retention := s.requestLogs.query(filter)
	next := filter.offset + len(logs)
	var nextOffset any
	hasMore := next < total
	if hasMore {
		nextOffset = next
	}
	return managementJSON(http.StatusOK, map[string]any{
		"logs":        requestLogEntryViews(logs),
		"total":       total,
		"limit":       filter.limit,
		"offset":      filter.offset,
		"has_more":    hasMore,
		"next_offset": nextOffset,
		"retention":   retention,
	})
}

func (s *Service) requestLogStatsRequest(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	defer s.done()
	since, until, err := parseRequestLogRange(r.Query)
	if err != nil {
		return managementError(statusOf(err), err.Error())
	}
	return managementJSON(http.StatusOK, s.requestLogs.statsFor(since, until))
}

func (s *Service) requestLogClearRequest(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	defer s.done()
	if err := s.requestLogs.clear(); err != nil {
		return managementError(statusOf(err), "could not clear request logs")
	}
	return managementJSON(http.StatusOK, map[string]any{"cleared": true})
}
