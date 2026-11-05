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
// The fence is keyed by credential id plus the inference key fingerprint, not by
// runtimeEpoch. A manual enable/disable bumps the epoch but not the key, and the
// account still has requests running against the same upstream key, so a
// recovery must keep waiting. Conversely, replacing the key must not let old
// requests block — or release — the counter of the newly installed key.

// quotaInFlightKey identifies the admitted inferences of one credential key.
// It is immutable once a lease exists, so reading it needs no lock.
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
type quotaInFlightLease struct {
	key  quotaInFlightKey
	once sync.Once
}

// quotaInFlightTestHook lets tests observe the exact admission/release
// interleaving. It is stored per Service (never globally) and is nil in
// production. Both callbacks run outside Service.mu, so a callback may take any
// plugin lock; the hook pointer itself is read atomically, and a test installs
// it before starting the request it wants to observe.
type quotaInFlightTestHook struct {
	beforeAdmit   func()
	beforeRelease func(quotaInFlightKey)
}

// admitQuotaInFlight re-verifies the live credential and reserves one in-flight
// counter inside the same Service.mu critical section. A pause, key replacement,
// manual disable or deletion that already happened can therefore not slip
// between the check and the registration: the request fails locally and never
// reaches the upstream.
//
// The source is the value selectedCredential returned. For a live credential it
// must still match the published record (key and runtime epoch) and that record
// must be enabled and not quota-blocked. A credential the plugin has no live
// record for (a host restart passing only StorageJSON) keeps working exactly as
// before: there is no pause state to enforce, so only the snapshot's own flags
// apply.
//
// No concurrency cap, queue or deadline is added here; admission is only a
// counter.
func (s *Service) admitQuotaInFlight(source Credential) (*quotaInFlightLease, error) {
	if hook := s.quotaInFlightTestHook.Load(); hook != nil && hook.beforeAdmit != nil {
		hook.beforeAdmit()
	}
	if source.ID == "" || source.APIKey == "" {
		return nil, localFail(http.StatusUnauthorized, "credential_unavailable", "OpenDesign credential is missing")
	}
	key := quotaInFlightKeyOf(source)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked[source.ID] {
		return nil, localFail(http.StatusUnauthorized, "credential_unavailable", "OpenDesign credential is disabled or removed")
	}
	if current, exists := s.creds[source.ID]; exists {
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
		if current.runtimeEpoch != source.runtimeEpoch {
			// Same key, but the identity changed and changed back (for example a
			// manual disable followed by an enable). The snapshot is stale.
			return nil, localFail(http.StatusConflict, "credential_changed", "OpenDesign credential changed before the request was admitted")
		}
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
	return &quotaInFlightLease{key: key}, nil
}

// release drops the in-flight counter. Callers run it only after the request's
// quota observation (observeQuotaBlock) completed, so an exhausted account is
// published before its fence disappears.
//
// When the last request for this key finishes, the existing quota-pause worker is
// woken so the parent session's recovery can re-evaluate the now-ungated account.
// The wake is a non-blocking channel send performed after the lock is dropped, so
// it can neither deadlock nor change the request's result.
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
		idle := s.quotaInflight[l.key] == 0
		s.mu.Unlock()
		if idle {
			s.wakeQuotaPauseWorker()
		}
	})
}

// quotaHasInFlightLocked reports whether an inference admitted for this exact
// credential key is still running. It is read-only and takes no lock of its own,
// so a caller already holding Service.mu can use it both before deciding to
// recover a paused account and again when committing that recovery. An epoch
// change from a manual enable/disable does not reset the answer: the same key
// still has the request in flight.
func (s *Service) quotaHasInFlightLocked(c Credential) bool {
	return s.quotaInflight[quotaInFlightKeyOf(c)] > 0
}

// quotaInFlightCountLocked returns how many inferences are admitted for the
// credential's current key. Counters of a replaced key are deliberately not
// included. Caller holds Service.mu.
func (s *Service) quotaInFlightCountLocked(c Credential) int {
	return s.quotaInflight[quotaInFlightKeyOf(c)]
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
