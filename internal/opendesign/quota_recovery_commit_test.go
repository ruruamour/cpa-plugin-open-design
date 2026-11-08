package opendesign

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// quotaCommitFixture prepares a paused credential with a fake clock shared by the
// recovery proof and the quota policy code. These tests drive rounds explicitly,
// so registration does not start a competing background worker.
func quotaCommitFixture(t *testing.T, models ...string) (*Service, *fakeHost, string, string, func(time.Duration)) {
	t.Helper()
	s, host := quotaRevisionTestService(t)
	advance := quotaTestClock(s)
	s.mu.Lock()
	s.modelCoverageNow = s.quotaNow
	s.mu.Unlock()
	id := addQuotaCredential(t, s, host, "workspace-commit")
	path := quotaFixtureAuthFile(t, s, host, id, nil)
	now := s.quotaPauseTime()
	s.mu.Lock()
	c := s.creds[id]
	manual := false
	c.QuotaBlock = &QuotaBlock{
		KeyHash: quotaPauseKeyHash(c.APIKey), ManualDisabled: &manual,
		Source: "upstream", Reason: "quota_exhausted",
		BlockedAt: now.Add(-2 * time.Minute), ObservedAt: now.Add(-time.Minute), RetryAt: now.Add(time.Hour),
	}
	for _, model := range models {
		addQuotaBlockModel(c.QuotaBlock, model)
	}
	c.QuotaRevision = newID()
	s.creds[id] = c
	s.mu.Unlock()
	s.credentialMu.Lock()
	err := s.persistQuotaBlockLocked(id, c.QuotaRevision, false)
	s.credentialMu.Unlock()
	if err != nil {
		t.Fatalf("write fixture block: %v", err)
	}
	return s, host, id, path, advance
}

// parkQuotaHTTP installs a host that answers each billing GET with an official
// available body generated at the fake clock value of that call, then parks the
// call until the returned release function runs. Keeping generatedAt tied to the
// call makes the proof fresh at check time, so a test can age it deliberately
// only in the window it wants to exercise.
func parkQuotaHTTP(t *testing.T, s *Service, host *fakeHost, advance func(time.Duration), model, member, verdict string) (started <-chan struct{}, release func()) {
	t.Helper()
	begin := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	s.SetHost(func(method string, in, out any) error {
		if method == "host.http.operation_open" {
			// A real request starts strictly after the recheck recorded its
			// window; the frozen fake clock needs an explicit tick for that. The
			// tick must happen before the HTTP start is recorded.
			advance(time.Millisecond)
		}
		if method != "host.http.do" {
			return host.call(method, in, out)
		}
		body := officialModelCoverage(t, "workspace-commit", model)
		s.mu.RLock()
		stamp := s.modelCoverageNow()
		s.mu.RUnlock()
		body["generatedAt"] = stamp.Format(time.RFC3339Nano)
		body["workspaceMemberId"] = member
		body["fundingCheck"] = verdict
		encoded := mustJSON(t, body)
		once.Do(func() { close(begin) })
		<-gate
		host.mu.Lock()
		host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: encoded})
		host.mu.Unlock()
		return host.call(method, in, out)
	})
	release = func() { releaseOnce.Do(func() { close(gate) }) }
	return begin, release
}

func awaitParked(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("official preflight never started")
	}
}

func waitRecheck(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("recheck did not finish")
		return nil
	}
}

func TestQuotaRecoveryRejectsEvidenceThatAgedWhileWaitingForTheCommitLock(t *testing.T) {
	s, host, id, path, advance := quotaCommitFixture(t, "model-a")
	started, release := parkQuotaHTTP(t, s, host, advance, "model-a", "member", "available")
	result := make(chan error, 1)
	go func() { result <- s.recheckQuotaBlock("callback", id) }()
	awaitParked(t, started)

	// Hold the commit path's file lock so the proof cannot be published. The
	// per-model check still passes; only aging before the write must stop it.
	s.authFileMu.Lock()
	unlock := sync.OnceFunc(s.authFileMu.Unlock)
	defer unlock()
	release()
	// Give the round time to finish its per-model proof and reach the commit.
	time.Sleep(100 * time.Millisecond)
	advance(31 * time.Second)
	unlock()
	err := waitRecheck(t, result)
	if err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("stale proof was not rejected at commit time: %v", err)
	}
	requireQuotaStillBlocked(t, s, id, path)
}

func TestQuotaRecoveryCommitFencesNewAdmissionsForSharedKey(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		name := "successful commit"
		if failWrite {
			name = "failed commit"
		}
		t.Run(name, func(t *testing.T) {
			s, host, id, path, advance := quotaCommitFixture(t, "model-a")
			paused, _ := liveCredential(s, id)
			siblingID := addCredential(t, s, host, "same-key", paused.APIKey)
			sibling, _ := liveCredential(s, siblingID)
			fenced, releaseFenceCh := make(chan struct{}), make(chan struct{})
			releaseFence := sync.OnceFunc(func() { close(releaseFenceCh) })
			defer releaseFence()
			s.quotaInFlightTestHook.Store(&quotaInFlightTestHook{
				afterRecoveryFence: func() { close(fenced); <-releaseFenceCh },
			})
			started, releaseHTTP := parkQuotaHTTP(t, s, host, advance, "model-a", "member", "available")
			defer releaseHTTP()
			result := make(chan error, 1)
			go func() { result <- s.recheckQuotaBlock("callback", id) }()
			awaitParked(t, started)
			releaseHTTP()
			awaitParked(t, fenced)

			// Both a live sibling and an id-less StorageJSON fallback share the
			// paused key; neither may slip past the no-in-flight check.
			for _, source := range []Credential{sibling, {APIKey: paused.APIKey}} {
				lease, err := s.admitQuotaInFlight(source, source.ID)
				if lease != nil {
					lease.release(s)
				}
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Kind != "quota_blocked" {
					t.Fatalf("same-key admission was not fenced: %v", err)
				}
			}
			// An unrelated inference key remains usable during the file commit.
			other, err := s.admitQuotaInFlight(Credential{APIKey: "other-inference-key"}, "")
			if err != nil {
				t.Fatalf("unrelated key was blocked: %v", err)
			}
			other.release(s)
			if failWrite {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			releaseFence()
			if err := waitRecheck(t, result); (err != nil) != failWrite {
				t.Fatalf("commit error = %v, failWrite=%v", err, failWrite)
			}
			// The temporary fence must be released on success and every failure.
			lease, err := s.admitQuotaInFlight(sibling, sibling.ID)
			if err != nil {
				t.Fatalf("commit left its shared-key admission fence behind: %v", err)
			}
			lease.release(s)
		})
	}
}

func TestQuotaRecoveryRejectsControlKeyChangedOnDiskWithoutWatcher(t *testing.T) {
	s, host, id, path, advance := quotaCommitFixture(t, "model-a")
	started, release := parkQuotaHTTP(t, s, host, advance, "model-a", "member", "available")
	result := make(chan error, 1)
	go func() { result <- s.recheckQuotaBlock("callback", id) }()
	awaitParked(t, started)

	// The operator edits the file directly; no watcher callback publishes it. The
	// in-memory identity still matches the proof, so only the on-disk check can
	// catch it.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["control_key"] = "changed-on-disk-control-key"
	if err := writeJSONAtomic(path, fields); err != nil {
		t.Fatal(err)
	}
	release()
	err = waitRecheck(t, result)
	if err == nil {
		t.Fatal("a recheck proved recovery for a control key the disk no longer holds")
	}
	requireQuotaStillBlocked(t, s, id, path)
	if strings.Contains(err.Error(), "changed-on-disk-control-key") {
		t.Fatal("recheck error leaked the changed control key")
	}
}

func TestQuotaRecoveryAutomaticRoundAbortsWhenPolicyGenerationChanges(t *testing.T) {
	s, host, id, path, advance := quotaCommitFixture(t, "model-a", "model-b")
	now := s.quotaPauseTime()
	s.mu.Lock()
	c := s.creds[id]
	c.QuotaBlock = cloneQuotaBlock(c.QuotaBlock)
	c.QuotaBlock.RetryAt = now.Add(-time.Second)
	revision := newID()
	c.QuotaRevision = revision
	s.creds[id] = c
	s.mu.Unlock()
	s.credentialMu.Lock()
	err := s.persistQuotaBlockLocked(id, revision, false)
	s.credentialMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	started, release := parkQuotaHTTP(t, s, host, advance, "model-a", "member", "available")
	result := make(chan error, 1)
	go func() { result <- s.recheckQuotaBlockMode("callback", id, true) }()
	awaitParked(t, started)

	// Change the policy generation (as a config save does) while the first model
	// is still being checked; the round must not ask for the second model.
	s.mu.Lock()
	s.pauseGeneration++
	s.mu.Unlock()
	release()
	err = waitRecheck(t, result)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != "quota_recheck_not_due" || !strings.Contains(apiErr.Message, "策略已变化") {
		t.Fatalf("the old policy round was not aborted for the policy change: %v", err)
	}
	requireQuotaStillBlocked(t, s, id, path)
	if got := host.countMethod("host.http.do"); got != 1 {
		t.Fatalf("the aborted round asked for %d models, want 1", got)
	}
}

func TestQuotaRecoveryCommitDoesNotWriteAfterQuiesce(t *testing.T) {
	s, host, id, path, advance := quotaCommitFixture(t, "model-a")
	started, release := parkQuotaHTTP(t, s, host, advance, "model-a", "member", "available")
	result := make(chan error, 1)
	go func() { result <- s.recheckQuotaBlock("callback", id) }()
	awaitParked(t, started)

	// Quiesce publishes stopped while the official check is still parked, so the
	// recheck reaches its commit after the plugin already reported itself stopped.
	quiesced := make(chan struct{})
	go func() {
		defer close(quiesced)
		_, _ = s.quiesce()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.RLock()
		stopped := s.stopped
		s.mu.RUnlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("quiesce never published the stopped flag")
		}
		time.Sleep(time.Millisecond)
	}
	release()
	if err := waitRecheck(t, result); err == nil {
		t.Fatal("a recheck committed after the plugin reported itself stopped")
	}
	select {
	case <-quiesced:
	case <-time.After(15 * time.Second):
		t.Fatal("quiesce did not finish after the commit lock was released")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk Credential
	if json.Unmarshal(data, &disk) != nil || disk.QuotaBlock == nil {
		t.Fatal("a stopped plugin cleared the pause on disk")
	}
}
