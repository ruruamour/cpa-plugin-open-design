package opendesign

import (
	"errors"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"
)

// Both policy publishers must share the recovery commit boundary: a settings
// POST and a host reconfigure that loads a changed settings file.
func quotaPolicyDisableCall(t *testing.T, s *Service, reconfigure bool) func() error {
	t.Helper()
	if reconfigure {
		cfg := s.config()
		disabled := false
		cfg.Pause.Enabled = &disabled
		if err := s.saveSettings(cfg); err != nil {
			t.Fatal(err)
		}
		body := mustJSON(t, map[string]any{"config_yaml": []byte("data_dir: " + cfg.DataDir + "\n")})
		return func() error {
			_, err := s.Handle("plugin.reconfigure", body)
			return err
		}
	}
	body := mustJSON(t, ManagementRequest{
		Method: http.MethodPost, Path: apiBase + "/config",
		Body: jsonBytes(map[string]any{"pause": map[string]any{"enabled": false}}),
	})
	return func() error {
		out, err := s.management(body)
		if err != nil {
			return err
		}
		if out.(ManagementResponse).StatusCode != http.StatusOK {
			return errors.New("pause policy save failed")
		}
		return nil
	}
}

func TestQuotaPolicyDisableSerializesWithRecoveryCommit(t *testing.T) {
	for _, reconfigure := range []bool{false, true} {
		name := "management save"
		if reconfigure {
			name = "host reconfigure"
		}
		t.Run(name, func(t *testing.T) {
			s, host, id, path, advance := quotaCommitFixture(t, "model-a")
			advance(31 * time.Minute)
			fenced, releaseFenceCh := make(chan struct{}), make(chan struct{})
			releaseFence := sync.OnceFunc(func() { close(releaseFenceCh) })
			defer releaseFence()
			s.quotaInFlightTestHook.Store(&quotaInFlightTestHook{
				afterRecoveryFence: func() { close(fenced); <-releaseFenceCh },
			})
			started, releaseHTTP := parkQuotaHTTP(t, s, host, advance, "model-a", "member", "available")
			defer releaseHTTP()
			result := make(chan error, 1)
			go func() { result <- s.recheckQuotaBlockMode("callback", id, true) }()
			awaitParked(t, started)
			releaseHTTP()
			awaitParked(t, fenced)

			disable := quotaPolicyDisableCall(t, s, reconfigure)
			saving := make(chan struct{})
			saved := make(chan error, 1)
			go func() { close(saving); saved <- disable() }()
			<-saving
			select {
			case err := <-saved:
				t.Fatalf("policy change returned while the previous recovery was still committing: %v", err)
			case <-time.After(40 * time.Millisecond):
			}
			if !s.config().pauseSettings().enabled {
				t.Fatal("policy was published across an in-progress recovery commit")
			}
			requireQuotaStillBlocked(t, s, id, path)
			releaseFence()
			if err := waitRecheck(t, result); err != nil {
				t.Fatalf("already admitted recovery failed: %v", err)
			}
			if err := waitRecheck(t, saved); err != nil {
				t.Fatal(err)
			}
			if s.config().pauseSettings().enabled {
				t.Fatal("policy save was lost after recovery completed")
			}
			current, _ := liveCredential(s, id)
			if current.quotaBlocked() {
				t.Fatal("the recovery ordered before the disable did not finish")
			}
		})
	}
}

func TestQuotaPolicyDisableBeforeCommitCancelsAutomaticRecovery(t *testing.T) {
	for _, reconfigure := range []bool{false, true} {
		name := "management save"
		if reconfigure {
			name = "host reconfigure"
		}
		t.Run(name, func(t *testing.T) {
			s, host, id, path, advance := quotaCommitFixture(t, "model-a")
			advance(31 * time.Minute)
			started, releaseHTTP := parkQuotaHTTP(t, s, host, advance, "model-a", "member", "available")
			defer releaseHTTP()
			result := make(chan error, 1)
			go func() { result <- s.recheckQuotaBlockMode("callback", id, true) }()
			awaitParked(t, started)
			if err := quotaPolicyDisableCall(t, s, reconfigure)(); err != nil {
				t.Fatal(err)
			}
			releaseHTTP()
			if err := waitRecheck(t, result); err == nil {
				t.Fatal("an automatic round committed after its policy was disabled")
			}
			requireQuotaStillBlocked(t, s, id, path)
		})
	}
}

// Respond at the fixture clock so a success can prove recovery. This fixture
// exercises the actual round and worker scan, not a cached coverage response.
func quotaRecheckClockHost(t *testing.T, s *Service, host *fakeHost, verdict string, before func()) {
	t.Helper()
	s.SetHost(func(method string, in, out any) error {
		if method == "host.http.do" {
			if before != nil {
				before()
			}
			body := officialModelCoverage(t, "workspace-commit", "model-a")
			body["generatedAt"] = s.quotaPauseTime().Format(time.RFC3339Nano)
			body["workspaceMemberId"] = "member"
			body["fundingCheck"] = verdict
			queueQuota(host, fakeHTTPResponse{Status: http.StatusOK, Body: mustJSON(t, body)})
		}
		return host.call(method, in, out)
	})
}

func TestQuotaQueuedAutomaticRecheckRespectsNewBackoff(t *testing.T) {
	s, host, id1, _, advance := quotaCommitFixture(t, "model-a")
	id2 := addQuotaCredential(t, s, host, "workspace-commit")
	quotaFixtureAuthFile(t, s, host, id2, nil)
	s.mu.Lock()
	c := s.creds[id2]
	c.QuotaBlock = cloneQuotaBlock(s.creds[id1].QuotaBlock)
	c.QuotaRevision = newID()
	s.creds[id2] = c
	s.mu.Unlock()
	s.credentialMu.Lock()
	err := s.persistQuotaBlockLocked(id2, c.QuotaRevision, false)
	s.credentialMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	advance(31 * time.Minute)
	ids := []string{id1, id2}
	sort.Strings(ids)
	second, _ := liveCredential(s, ids[1])
	var once sync.Once
	var next time.Time
	quotaRecheckClockHost(t, s, host, "insufficient", func() {
		once.Do(func() {
			// The scan has already queued both IDs. A prior manual round's
			// result is published before the second account gets its turn.
			s.setQuotaRecheckStatus(ids[1], second.QuotaRevision,
				localFail(http.StatusConflict, "quota_recheck_unproven", "unproven"))
			s.mu.RLock()
			next = s.quotaRecheckRetryAt[ids[1]]
			s.mu.RUnlock()
		})
	})
	s.recheckDueQuotaBlocks(s.stopCh)
	if got := host.countMethod("host.http.do"); got != 1 {
		t.Fatalf("queued account ignored its new backoff: got %d upstream requests, want 1", got)
	}
	s.mu.RLock()
	retry, failures, status := s.quotaRecheckRetryAt[ids[1]], s.quotaRecheckFailures[ids[1]], s.quotaRecheckStatus[ids[1]]
	s.mu.RUnlock()
	if !retry.Equal(next) || failures != 1 || status != "unproven" {
		t.Fatalf("skipping a queued account changed its prior backoff: retry=%v want=%v failures=%d status=%s", retry, next, failures, status)
	}
}

func TestQuotaManualRecheckPublishesFailureBeforeNextAutomaticRound(t *testing.T) {
	s, host, id, path, advance := quotaCommitFixture(t, "model-a")
	advance(31 * time.Minute)
	quotaRecheckClockHost(t, s, host, "insufficient", nil)
	if err := s.recheckQuotaBlock("callback", id); err == nil {
		t.Fatal("insufficient evidence proved recovery")
	}
	// The caller has not published any status: it must already be committed
	// when the round releases the shared recovery gate.
	s.quotaRecoveryGate <- struct{}{}
	s.mu.RLock()
	retry, failures, status := s.quotaRecheckRetryAt[id], s.quotaRecheckFailures[id], s.quotaRecheckStatus[id]
	s.mu.RUnlock()
	<-s.quotaRecoveryGate
	if !retry.Equal(s.quotaPauseTime().Add(5*time.Minute)) || failures != 1 || status != "unproven" {
		t.Fatalf("round released the gate without its failure/backoff: retry=%v failures=%d status=%s", retry, failures, status)
	}
	if err := s.recheckQuotaBlockMode("callback", id, true); err == nil {
		t.Fatal("automatic round ignored the new backoff")
	}
	if got := host.countMethod("host.http.do"); got != 1 {
		t.Fatalf("backoff allowed another request: %d", got)
	}
	requireQuotaStillBlocked(t, s, id, path)
	// A manual check is still allowed during backoff and may prove recovery.
	quotaRecheckClockHost(t, s, host, "available", nil)
	if err := s.recheckQuotaBlock("callback", id); err != nil {
		t.Fatalf("backoff blocked a manual recheck: %v", err)
	}
	if current, _ := liveCredential(s, id); current.quotaBlocked() {
		t.Fatal("manual proof did not clear the pause")
	}
}

func TestQuotaAutomaticFailuresUseBoundedBackoff(t *testing.T) {
	s, host, id, path, advance := quotaCommitFixture(t, "model-a")
	advance(31 * time.Minute)
	quotaRecheckClockHost(t, s, host, "insufficient", nil)
	for i, delay := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		s.recheckDueQuotaBlocks(s.stopCh)
		s.mu.RLock()
		retry, failures := s.quotaRecheckRetryAt[id], s.quotaRecheckFailures[id]
		s.mu.RUnlock()
		if want := s.quotaPauseTime().Add(delay); !retry.Equal(want) || failures != i+1 {
			t.Fatalf("round %d: retry=%v want=%v failures=%d", i+1, retry, want, failures)
		}
		// Both repeated sweeps and direct queued rounds must respect the same
		// deadline without changing it or counting a second failed attempt.
		s.recheckDueQuotaBlocks(s.stopCh)
		_ = s.recheckQuotaBlockMode("callback", id, true)
		if got := host.countMethod("host.http.do"); got != i+1 {
			t.Fatalf("round %d repeated during backoff: %d requests", i+1, got)
		}
		s.mu.RLock()
		unchanged := s.quotaRecheckRetryAt[id].Equal(retry) && s.quotaRecheckFailures[id] == i+1
		s.mu.RUnlock()
		if !unchanged {
			t.Fatal("skipping a backoff changed the retry state")
		}
		requireQuotaStillBlocked(t, s, id, path)
		advance(delay)
	}
}

func TestQuotaManualRecheckDoesNotDoubleCountFailure(t *testing.T) {
	s, host, id, _, advance := quotaCommitFixture(t, "model-a")
	advance(31 * time.Minute)
	quotaRecheckClockHost(t, s, host, "insufficient", nil)
	status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/recheck", nil, map[string]any{"credential_id": id})
	if status != http.StatusConflict {
		t.Fatalf("unproven manual recheck returned %d", status)
	}
	s.mu.RLock()
	failures := s.quotaRecheckFailures[id]
	s.mu.RUnlock()
	if failures != 1 {
		t.Fatalf("one failed management round counted as %d failures", failures)
	}
	_, payload, _ := managementCall(t, s, http.MethodGet, apiBase+"/status", nil, nil)
	if pause := pauseOf(t, credentialByID(t, payload, id)); pause["recheck_status"] != "unproven" {
		t.Fatalf("failed manual round was not reflected in account status: %#v", pause)
	}
}
