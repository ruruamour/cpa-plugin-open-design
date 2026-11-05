package opendesign

import (
	"net/http"
	"sync"
)

// Non-throttling inference in-flight tracking.
//
// The plugin never limits concurrency: this bookkeeping exists only so a
// recovery decision (implemented by the parent session) can prove that no
// inference admitted for a credential's current key is still running before it
// clears a quota block. Every inference therefore adds one counter after the
// live credential was re-verified, and drops it exactly once when the request
// (or its terminal stream pump) has finished.
//
// The fence is keyed by credential id plus the inference key fingerprint: the id
// keeps releases isolated per record, while a recovery asks about the inference
// key, merging every record (including a StorageJSON-only request with no id)
// that carries it. runtimeEpoch is deliberately absent. A manual enable/disable
// bumps the epoch but not the key, and the account still has requests running
// against the same upstream key, so a recovery must keep waiting. Conversely,
// replacing the key must not let old requests block — or release — the counter
// of the newly installed key.

// quotaInFlightKey identifies the admitted inferences of one credential key.
// It is immutable once a lease exists, so reading it needs no lock.
//
// The credential id is kept for release isolation (an old key's request must
// never decrement the counter of the newly installed key) but it is *not* part
// of the fence a recovery reads: the same inference key can be reachable through
// several records, including the empty id of a StorageJSON-only request.
type quotaInFlightKey struct {
	credentialID string
	keyHash      string
}

// quotaInFlightKeyOf derives the fence key from a credential snapshot.
func quotaInFlightKeyOf(c Credential) quotaInFlightKey {
	return quotaInFlightKey{credentialID: c.ID, keyHash: quotaPauseKeyHash(c.APIKey)}
}

// quotaInFlightLease is the proof that one inference was admitted. release is
// idempotent: the buffered path uses it from its cleanup defers, and the
// streaming path may reach both an early return and the terminal pump defer, so
// the once value guarantees exactly one decrement.
//
// admittedEpoch is the credential generation that was live when the request was
// admitted, or 0 when no live record existed (the StorageJSON-only fallback).
type quotaInFlightLease struct {
	key           quotaInFlightKey
	admittedEpoch uint64
	once          sync.Once
}

// quotaInFlightTestHook lets tests observe the exact admission/release
// interleaving. It is stored per Service (never globally) and is nil in
// production. Both callbacks run outside Service.mu, so a callback may take any
// plugin lock; the hook pointer itself is read atomically, and a test installs
// it before starting the request it wants to observe.
type quotaInFlightTestHook struct {
	beforeAdmit   func()
	beforeObserve func()
	beforeRelease func(quotaInFlightKey)
}

// admitQuotaInFlight re-verifies the live credential and reserves one in-flight
// counter inside the same Service.mu critical section. A pause, key replacement,
// manual disable or deletion that already happened can therefore not slip
// between the check and the registration: the request fails locally and never
// reaches the upstream.
//
// requestAuthID is the id the host routed the request by. selectedCredential
// treats it as a second identity for revocation, so admission re-checks it too
// rather than trusting the selection made a moment earlier.
//
// The source is the value selectedCredential returned. A request the plugin has
// no live record for keeps working exactly as before: that is both the
// host-restart case (StorageJSON only) and the historical fallback where
// StorageJSON carried an inference key but no id. Such a request is still
// counted under its key, so a credential record for the same key that appears
// later is fenced too.
//
// No concurrency cap, queue or deadline is added here; admission is only a
// counter.
func (s *Service) admitQuotaInFlight(source Credential, requestAuthID string) (*quotaInFlightLease, error) {
	if hook := s.quotaInFlightTestHook.Load(); hook != nil && hook.beforeAdmit != nil {
		hook.beforeAdmit()
	}
	if source.APIKey == "" {
		return nil, localFail(http.StatusUnauthorized, "credential_unavailable", "OpenDesign credential is missing")
	}
	key := quotaInFlightKeyOf(source)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Mirror selectedCredential's revocation check: the resolved record and the
	// routed id are both identities, and either may have been revoked since.
	if (source.ID != "" && s.revoked[source.ID]) || (requestAuthID != "" && s.revoked[requestAuthID]) {
		return nil, localFail(http.StatusUnauthorized, "credential_unavailable", "OpenDesign credential is disabled or removed")
	}
	// The live record to verify against. selectedCredential prefers the resolved
	// id and only falls back to the routed id when that id has no record, so the
	// same order is used here.
	liveID := source.ID
	lease := &quotaInFlightLease{key: key}
	current, hasLive := Credential{}, false
	if liveID != "" {
		current, hasLive = s.creds[liveID]
	}
	if !hasLive && source.ID == "" && requestAuthID != "" {
		current, hasLive = s.creds[requestAuthID]
	}
	if hasLive {
		if current.APIKey != source.APIKey {
			// The key was replaced between selection and admission. Failing closed
			// keeps a stale model-test snapshot from reaching a key this request
			// never validated.
			return nil, localFail(http.StatusConflict, "credential_changed", "OpenDesign credential changed before the request was admitted")
		}
		if current.Disabled {
			return nil, localFail(http.StatusUnauthorized, "credential_unavailable", "OpenDesign credential is disabled")
		}
		if current.quotaBlocked() {
			return nil, localFail(http.StatusTooManyRequests, "quota_blocked", quotaBlockMarker+" OpenDesign account quota is temporarily paused")
		}
		// source.runtimeEpoch is 0 for a snapshot that came from StorageJSON or an
		// id-less request: those never carried an epoch, so a record published
		// between selection and here is a first publication, not an identity
		// round trip. The epoch fence only applies to live-derived snapshots.
		if source.runtimeEpoch != 0 && current.runtimeEpoch != source.runtimeEpoch {
			// Same key, but the identity changed and changed back (for example a
			// manual disable followed by an enable). The snapshot is stale.
			return nil, localFail(http.StatusConflict, "credential_changed", "OpenDesign credential changed before the request was admitted")
		}
		lease.admittedEpoch = current.runtimeEpoch
	} else {
		if source.Disabled {
			return nil, localFail(http.StatusUnauthorized, "credential_unavailable", "OpenDesign credential is disabled")
		}
		if source.quotaBlocked() {
			return nil, localFail(http.StatusTooManyRequests, "quota_blocked", quotaBlockMarker+" OpenDesign account quota is temporarily paused")
		}
	}
	if s.quotaInflight == nil {
		s.quotaInflight = map[quotaInFlightKey]int{}
	}
	s.quotaInflight[key]++
	return lease, nil
}

// release drops the in-flight counter. Callers run it only after the request's
// quota observation (observeQuotaBlock) completed, so an exhausted account is
// published before its fence disappears.
//
// The wake is deliberately narrow: it fires only when the released inference key
// has no request left *and* a currently blocked record carries that same key, so
// a parent recovery can re-evaluate the now-ungated account. A normal request
// therefore never wakes the sweep, which keeps the per-request cost at one map
// update and avoids pointlessly re-running the full-table sweep and its failed
// persistence retries. The wake itself is a non-blocking channel send performed
// after the lock is dropped, so it can neither deadlock nor change the result.
func (l *quotaInFlightLease) release(s *Service) {
	if l == nil || s == nil {
		return
	}
	l.once.Do(func() {
		if hook := s.quotaInFlightTestHook.Load(); hook != nil && hook.beforeRelease != nil {
			hook.beforeRelease(l.key)
		}
		s.mu.Lock()
		if count := s.quotaInflight[l.key]; count > 1 {
			s.quotaInflight[l.key] = count - 1
		} else {
			delete(s.quotaInflight, l.key)
		}
		idle := true
		for key, count := range s.quotaInflight {
			// The fence is key-wide: another record reusing the same inference key
			// may still be running.
			if key.keyHash == l.key.keyHash && count > 0 {
				idle = false
				break
			}
		}
		wake := false
		if idle {
			for id, c := range s.creds {
				if s.revoked[id] || !c.quotaBlocked() {
					continue
				}
				if quotaPauseKeyHash(c.APIKey) == l.key.keyHash {
					wake = true
					break
				}
			}
		}
		s.mu.Unlock()
		if wake {
			s.wakeQuotaPauseWorker()
		}
	})
}

// observeQuotaBlockAfterAdmission observes a request's terminal error with the
// identity fence that belongs to the admission, not to the snapshot that was
// selected a moment earlier.
//
// A StorageJSON-only snapshot carries no runtime epoch. If a live record for the
// same key exists (or appears) before the upstream error arrives, handing the
// epoch-less snapshot to observeQuotaBlock would drop a real 402, because the
// live record carries a different epoch. The fence is therefore widened, but
// only across generations that prove nothing moved on:
//
//   - a record existed at admission: its generation must be unchanged since, so
//     a key replacement, manual toggle or explicit resume (each of which bumps
//     the generation) still suppresses the late error;
//   - no record existed at admission: only the first publication since
//     (generation 1) is accepted; a publish-then-resume reaches generation 2 and
//     is dropped, preserving the manual-resume guarantee.
func (s *Service) observeQuotaBlockAfterAdmission(source Credential, lease *quotaInFlightLease, upstreamModel string, requestErr error) {
	if hook := s.quotaInFlightTestHook.Load(); hook != nil && hook.beforeObserve != nil {
		hook.beforeObserve()
	}
	if lease != nil && source.ID != "" && source.runtimeEpoch == 0 {
		if epoch, ok := s.quotaInFlightObservationEpoch(source, lease.admittedEpoch); ok {
			source.runtimeEpoch = epoch
		}
	}
	s.observeQuotaBlock(source, upstreamModel, requestErr)
}

// quotaInFlightObservationEpoch reports the epoch an epoch-less snapshot may be
// observed under, or ok=false when the live generation has moved past the
// admitted one. It is read-only and takes no other lock.
func (s *Service) quotaInFlightObservationEpoch(source Credential, admittedEpoch uint64) (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.revoked[source.ID] {
		return 0, false
	}
	current, exists := s.creds[source.ID]
	if !exists || current.APIKey != source.APIKey || current.runtimeEpoch == 0 {
		return 0, false
	}
	if admittedEpoch == 0 {
		// No live record at admission: accept exactly the first publication. A
		// resume or a manual toggle would have advanced the generation further.
		if current.runtimeEpoch != 1 {
			return 0, false
		}
		return current.runtimeEpoch, true
	}
	if current.runtimeEpoch != admittedEpoch {
		return 0, false
	}
	return current.runtimeEpoch, true
}

// quotaHasInFlightLocked reports whether an inference admitted with this
// credential's inference key is still running. The answer merges every record
// that reuses the same key, including the empty id of a StorageJSON-only
// request: a request that arrives late through another record must not be able
// to slip past a recovery committed for this one. It is read-only and takes no
// lock of its own, so a caller already holding Service.mu can use it both before
// deciding to recover a paused account and again when committing that recovery.
// An epoch change from a manual enable/disable does not reset the answer: the
// same key still has the request in flight.
func (s *Service) quotaHasInFlightLocked(c Credential) bool {
	return s.quotaInFlightCountLocked(c) > 0
}

// quotaInFlightCountLocked returns how many inferences are admitted with the
// credential's inference key, summed across every record that carries it.
// Counters of a replaced key are deliberately not included. Caller holds
// Service.mu.
func (s *Service) quotaInFlightCountLocked(c Credential) int {
	if c.APIKey == "" {
		return 0
	}
	hash := quotaPauseKeyHash(c.APIKey)
	total := 0
	for key, count := range s.quotaInflight {
		if key.keyHash == hash {
			total += count
		}
	}
	return total
}

// quotaInFlightTotalLocked returns the number of admitted inferences across every
// key. Caller holds Service.mu; it is read-only and tests use it to prove that no
// path leaked a counter.
func (s *Service) quotaInFlightTotalLocked() int {
	total := 0
	for _, count := range s.quotaInflight {
		total += count
	}
	return total
}
