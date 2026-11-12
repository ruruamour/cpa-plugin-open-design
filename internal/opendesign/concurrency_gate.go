package opendesign

import (
	"errors"
	"net/http"
	"time"
)

// Per-account concurrency protection.
//
// The host owns account selection: by the time a request reaches the plugin the
// account is already chosen, so the plugin cannot move it elsewhere. What it can
// do is refuse a request locally, before anything is sent upstream. Such a
// refusal carries concurrencyBusyMarker, and the default error rules map that
// marker to the request-scoped action "continue": the host moves to the next
// account and does not cool this one down.
//
// Two measurements from production motivate the two halves of this file
// (11 minutes of the plugin's own request log, 28 active accounts, 969 rows):
//
//   - An account that already had five accepted requests running accepted a
//     sixth 1 time in 39. The plan's concurrency limit is enforced per account.
//   - After a rejection the account stayed rejected for a while: an attempt
//     within 30 seconds was accepted 3% of the time, whether or not the account
//     was idle. The host's own backoff starts at one second, so it returned to
//     the same account after a median of 8 seconds; 68% of all rejections were
//     sent to an account that had been rejected less than 30 seconds earlier.
//
// So the gate (1) keeps the number of requests sent to one account below the
// limit, and (2) after an upstream rejection stops sending to that account for
// a hold period, then lets exactly one request through as a probe. The hold is
// also handed to the host as retry_after_seconds, which the host uses verbatim
// instead of its own exponential backoff.
//
// 0.6.0 shipped only those two and did not restore service: with a concurrency
// limit of four, an account that recovered was refilled to four within five
// seconds and rejected again about ten seconds later. Three captures taken with
// the gate active (limit 4, limit 1, limit 4 again) agree on what the upstream
// actually does: once an accepted request has ended, the account takes nothing
// new for about a minute, counted from the last request that ended.
//
//	seconds since the last accepted request ended     0-60        60-70
//	limit 1                                           7/171       90/90
//	limit 4                                           14/189      42/49
//	limit 4 again                                     9/138       42/44
//
// Requests started together are all taken, because none of them has ended yet:
// with a request already running and none ended in the prior minute, 129 of 136
// were accepted. A rejection does not restart the minute ("time since the last
// rejection" predicts no better than chance). That also explains the hold: it
// is timed from the first rejection, which comes when the first request of a
// burst ends, while the upstream counts from the last one to end, so a 60 s
// probe was regularly a few seconds early and doubled the hold.
//
// Hence (3): after an accepted request ends, nothing is started for that
// account for a rest period.
//
// CORRECTION, measured after (3) went live: the "minute" above is not the
// upstream's rule. It was an artefact of the 60 s hold, which left almost no
// attempts between 10 and 60 s after a request ended. What the upstream meters
// is usage: about 20,000 tokens per account per minute (input plus output),
// charged when a request completes, and an account with usage still owed takes
// nothing. On a capture that was not used for fitting, "accept iff nothing is
// owed" predicts 94.7% of outcomes (nothing owed: 296/322 accepted; 3,000 or
// more owed: 0/203), while 65-90 s after the last request ended only 123/339
// were accepted. So the wait after a batch is proportional to the tokens it
// used (one 14k-token request: about 42 s; four: about 170 s), and a fixed
// rest is only an approximation: roughly
//
//	limit x average tokens per request / 20000 x 60 seconds.
//
// Production runs with rest_seconds = 180 for a limit of four. The default
// below was left as deployed; it is known to be too short for that limit and
// should be replaced by pacing on the usage each request reports.
//
// A refusal whose wait is known (a held or resting account) also tells the host
// how long to stay away, with a second marker and a "continue-and-cooldown"
// rule. Without that the host offered every refusing account to every request:
// one client request became up to 28 plugin calls, several thousand a minute.
//
// Everything here is keyed by the inference-key fingerprint, the same identity
// quotaInflight uses, so several credential records that carry one upstream key
// share one budget. All state is guarded by Service.mu.

const (
	// concurrencyBusyMarker is matched by the built-in "continue" rule. Like the
	// other markers it names no provider, because the text can reach a client.
	concurrencyBusyMarker = "[upstream:account_busy]"

	// The two kinds share the marker so a single rule covers both; they stay
	// distinct for the counters and for the explicit model test.
	concurrencyLimitKind = "account_busy"
	concurrencyHeldKind  = "account_held"
	// concurrencyRestingKind is a request refused because an accepted request
	// of the account ended less than the rest period ago.
	concurrencyRestingKind = "account_resting"

	// concurrencyWaitMarker is put on a refusal whose wait is known and long
	// enough to be worth a host cooldown. The built-in rule for it is
	// "continue-and-cooldown", and the refusal carries the wait as
	// retry_after_seconds, so the host skips the account for exactly that long.
	concurrencyWaitMarker = "[upstream:account_wait]"
	// The host never cools for less than this (minQuotaCooldownFloor). A
	// shorter wait is answered with the plain busy marker instead, so the host
	// does not stay away longer than the plugin needs.
	concurrencyHostCooldownFloor = 10 * time.Second

	concurrencyDefaultMaxInFlight = 4
	concurrencyDefaultHold        = 60 * time.Second
	concurrencyDefaultMaxHold     = 5 * time.Minute
	// Chosen when the upstream was believed to want a minute after a request
	// ended. See the correction in the file comment: it suits a single request,
	// not a batch.
	concurrencyDefaultRest = 65 * time.Second

	concurrencyMaxInFlightLimit = 64
	concurrencyHoldLimitSeconds = 3600

	// concurrencyStateRetention bounds how long an account that never recovered
	// keeps its escalated hold once nothing is sent to it any more.
	concurrencyStateRetention = time.Hour
)

// ConcurrencyConfig is the persisted policy. A nil pointer or a zero
// MaxInFlight means "use the default", so a settings.json written before this
// feature existed resolves to the reviewed defaults. HoldSeconds needs a
// pointer because 0 is a real choice: it turns the hold off.
type ConcurrencyConfig struct {
	Enabled        *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	MaxInFlight    int   `json:"max_in_flight,omitempty" yaml:"max_in_flight,omitempty"`
	HoldSeconds    *int  `json:"hold_seconds,omitempty" yaml:"hold_seconds,omitempty"`
	MaxHoldSeconds *int  `json:"max_hold_seconds,omitempty" yaml:"max_hold_seconds,omitempty"`
	// RestSeconds is how long nothing is started for an account after one of
	// its accepted requests ended. It is a pointer because 0 is a real choice:
	// it turns the rest off and leaves the concurrency limit and the hold.
	RestSeconds *int `json:"rest_seconds,omitempty" yaml:"rest_seconds,omitempty"`
}

// concurrencySettings is the resolved, always-valid policy.
type concurrencySettings struct {
	enabled     bool
	maxInFlight int
	hold        time.Duration
	maxHold     time.Duration
	// rest is 0 when resting is off.
	rest time.Duration
}

// concurrencySettings resolves the persisted policy. The result never has a
// maximum hold below the base hold, and a zero base hold disables holding.
func (c Config) concurrencySettings() concurrencySettings {
	settings := concurrencySettings{
		enabled:     true,
		maxInFlight: concurrencyDefaultMaxInFlight,
		hold:        concurrencyDefaultHold,
		maxHold:     concurrencyDefaultMaxHold,
		rest:        concurrencyDefaultRest,
	}
	policy := c.Concurrency
	if policy == nil {
		return settings
	}
	if policy.Enabled != nil {
		settings.enabled = *policy.Enabled
	}
	if policy.MaxInFlight > 0 {
		settings.maxInFlight = policy.MaxInFlight
		if settings.maxInFlight > concurrencyMaxInFlightLimit {
			settings.maxInFlight = concurrencyMaxInFlightLimit
		}
	}
	if policy.HoldSeconds != nil {
		settings.hold = boundedConcurrencySeconds(*policy.HoldSeconds)
	}
	if policy.MaxHoldSeconds != nil {
		settings.maxHold = boundedConcurrencySeconds(*policy.MaxHoldSeconds)
	}
	if settings.maxHold < settings.hold {
		settings.maxHold = settings.hold
	}
	if policy.RestSeconds != nil {
		settings.rest = boundedConcurrencySeconds(*policy.RestSeconds)
	}
	return settings
}

// boundedConcurrencySeconds clamps before converting so a malformed settings
// file cannot overflow the multiplication.
func boundedConcurrencySeconds(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	if seconds > concurrencyHoldLimitSeconds {
		seconds = concurrencyHoldLimitSeconds
	}
	return time.Duration(seconds) * time.Second
}

// validateConcurrencyConfig rejects an out-of-range policy before it is saved.
// It is not applied when loading: see Config.validate.
func validateConcurrencyConfig(policy *ConcurrencyConfig) error {
	if policy == nil {
		return nil
	}
	if policy.MaxInFlight < 0 || policy.MaxInFlight > concurrencyMaxInFlightLimit {
		return fail(http.StatusBadRequest, "concurrency.max_in_flight must be between 1 and 64")
	}
	for _, value := range []*int{policy.HoldSeconds, policy.MaxHoldSeconds} {
		if value != nil && (*value < 0 || *value > concurrencyHoldLimitSeconds) {
			return fail(http.StatusBadRequest, "concurrency hold seconds must be between 0 and 3600")
		}
	}
	if policy.HoldSeconds != nil && policy.MaxHoldSeconds != nil && *policy.MaxHoldSeconds < *policy.HoldSeconds {
		return fail(http.StatusBadRequest, "concurrency.max_hold_seconds must not be below concurrency.hold_seconds")
	}
	if policy.RestSeconds != nil && (*policy.RestSeconds < 0 || *policy.RestSeconds > concurrencyHoldLimitSeconds) {
		return fail(http.StatusBadRequest, "concurrency.rest_seconds must be between 0 and 3600")
	}
	return nil
}

// holdFor returns how long an account is held after its n-th consecutive
// rejection: the base hold, doubled per further rejection, capped at the
// maximum. An upstream Retry-After can only lengthen it, never beyond the cap.
func (c concurrencySettings) holdFor(strikes int, hint time.Duration) time.Duration {
	if c.hold <= 0 {
		return 0
	}
	hold := c.hold
	for i := 1; i < strikes && hold < c.maxHold; i++ {
		hold *= 2
	}
	if hint > hold {
		hold = hint
	}
	if hold > c.maxHold {
		hold = c.maxHold
	}
	return hold
}

// concurrencyState is the hold bookkeeping of one inference key. An entry only
// exists while an account is held, probing or still carries strikes.
type concurrencyState struct {
	// holdUntil is when the account may be tried again. Zero means never held.
	holdUntil time.Time
	// strikes counts consecutive rejections without an accepted request between.
	strikes int
	// rejectSeq changes on every rejection; strikeSeq only when strikes change.
	// A lease remembers both from its admission, which is how a burst of
	// rejections is told apart from a second, later rejection: the burst counts
	// as one strike, and a success admitted before the rejection proves nothing.
	rejectSeq uint64
	strikeSeq uint64
	// probe is the token of the single request admitted after a hold expired;
	// zero when no probe is outstanding.
	probe uint64
	// lastEnded is when the most recent accepted request of the account ended.
	// The rest period is counted from it. Zero means none ended yet.
	lastEnded time.Time
}

// concurrencyStats are cumulative since this plugin instance was created.
type concurrencyStats struct {
	rejectedAtLimit     int64
	rejectedWhileHeld   int64
	rejectedAtRest      int64
	upstreamRateLimited int64
	holdsStarted        int64
}

// concurrencyLease is the part of an in-flight lease the gate owns. Every field
// is written only while Service.mu is held.
type concurrencyLease struct {
	rejectSeq uint64
	strikeSeq uint64
	probe     uint64
	accepted  bool
	rejected  bool
}

// concurrencyAdmitLocked decides whether one more request may be sent with this
// inference key. inFlight is the number already running for the key. It returns
// a local rejection, or the lease bookkeeping for an admitted request. Caller
// holds Service.mu.
func (s *Service) concurrencyAdmitLocked(keyHash string, inFlight int, now time.Time) (concurrencyLease, error) {
	settings := s.cfg.concurrencySettings()
	state := s.concurrencyStates[keyHash]
	lease := concurrencyLease{}
	if state != nil {
		lease.rejectSeq, lease.strikeSeq = state.rejectSeq, state.strikeSeq
	}
	if !settings.enabled || keyHash == "" {
		return lease, nil
	}
	if state != nil && settings.hold > 0 && state.holdUntil.After(now) {
		s.concurrencyStats.rejectedWhileHeld++
		return lease, concurrencyRefusal(concurrencyHeldKind,
			"upstream account is temporarily held after a rate limit", state.holdUntil.Sub(now))
	}
	// Before the probe: a resting account rejects everything, so probing it
	// would only be rejected and escalate the hold.
	// A rest of 0 (off) can never leave a positive wait, so it needs no branch.
	if state != nil && !state.lastEnded.IsZero() {
		if wait := state.lastEnded.Add(settings.rest).Sub(now); wait > 0 {
			s.concurrencyStats.rejectedAtRest++
			return lease, concurrencyRefusal(concurrencyRestingKind,
				"upstream account is resting after a finished request", wait)
		}
	}
	if state != nil && settings.hold > 0 {
		if state.strikes > 0 {
			// The hold expired but nothing has been accepted since. Send one
			// request to find out instead of the whole limit at once.
			if state.probe != 0 && inFlight == 0 {
				// A probe never outlives its request; this only repairs the
				// bookkeeping if that invariant were ever broken.
				state.probe = 0
			}
			if state.probe != 0 {
				s.concurrencyStats.rejectedWhileHeld++
				// How long the probe will run is not known, so no cooldown.
				return lease, concurrencyRefusal(concurrencyHeldKind,
					"upstream account is being probed after a rate limit", 0)
			}
			s.concurrencySeq++
			state.probe = s.concurrencySeq
			lease.probe = state.probe
			return lease, nil
		}
	}
	if inFlight >= settings.maxInFlight {
		s.concurrencyStats.rejectedAtLimit++
		// A slot can free at any moment, so the host must not stay away.
		return lease, concurrencyRefusal(concurrencyLimitKind,
			"upstream account is at its concurrency limit", 0)
	}
	return lease, nil
}

// concurrencyRefusal builds a local refusal. A wait the host can honour (at
// least its own minimum cooldown) is attached as the cooldown and announced
// with the wait marker; anything else carries the plain busy marker, which the
// built-in rule answers with "continue" and no cooldown.
func concurrencyRefusal(kind, text string, wait time.Duration) error {
	if wait < concurrencyHostCooldownFloor {
		return localFail(http.StatusTooManyRequests, kind, concurrencyBusyMarker+" "+text)
	}
	return &APIError{
		Status: http.StatusTooManyRequests, Kind: kind,
		Message:        concurrencyWaitMarker + " " + text,
		hostRetryAfter: wait,
	}
}

// concurrencyAccepted records that the upstream took the request. It clears the
// strikes only when no rejection happened after this request was admitted:
// a request that was already running when the account got rejected says nothing
// about whether the account is usable again.
func (s *Service) concurrencyAccepted(lease *quotaInFlightLease) {
	if s == nil || lease == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	gate := &lease.gate
	if gate.accepted || gate.rejected {
		return
	}
	gate.accepted = true
	state := s.concurrencyStates[lease.key.keyHash]
	if state == nil {
		return
	}
	if gate.probe != 0 && state.probe == gate.probe {
		state.probe = 0
	}
	if state.rejectSeq != gate.rejectSeq || state.strikes == 0 {
		return
	}
	state.strikes = 0
	s.concurrencySeq++
	state.strikeSeq = s.concurrencySeq
}

// concurrencyRejected records an upstream rate-limit rejection and returns how
// long the account is now held, or 0 when holding is off. hint is the
// upstream's own Retry-After, if any.
func (s *Service) concurrencyRejected(lease *quotaInFlightLease, hint time.Duration) time.Duration {
	if s == nil || lease == nil {
		return 0
	}
	now := s.concurrencyNow()
	s.mu.Lock()
	defer s.mu.Unlock()
	gate := &lease.gate
	if gate.rejected {
		return 0
	}
	gate.rejected = true
	keyHash := lease.key.keyHash
	if keyHash == "" {
		return 0
	}
	s.concurrencyStats.upstreamRateLimited++
	settings := s.cfg.concurrencySettings()
	state := s.concurrencyStates[keyHash]
	if state != nil && gate.probe != 0 && state.probe == gate.probe {
		state.probe = 0
	}
	if !settings.enabled || settings.hold <= 0 {
		return 0
	}
	if state == nil {
		state = &concurrencyState{}
		if s.concurrencyStates == nil {
			s.concurrencyStates = map[string]*concurrencyState{}
		}
		s.concurrencyStates[keyHash] = state
	}
	s.concurrencySeq++
	state.rejectSeq = s.concurrencySeq
	if gate.strikeSeq == state.strikeSeq {
		// First rejection since this request was admitted: a new strike. Later
		// rejections of the same burst only refresh the hold.
		state.strikes++
		state.strikeSeq = s.concurrencySeq
		s.concurrencyStats.holdsStarted++
	}
	strikes := state.strikes
	if strikes < 1 {
		strikes = 1
	}
	until := now.Add(settings.holdFor(strikes, hint))
	if until.After(state.holdUntil) {
		state.holdUntil = until
	}
	return state.holdUntil.Sub(now)
}

// concurrencyReleasedLocked runs after the in-flight counter of a lease was
// dropped. A probe that ended neither accepted nor rejected (a timeout, a
// transport failure) frees the probe slot so the next request can try, and an
// entry that carries no information any more is removed. Caller holds mu.
func (s *Service) concurrencyReleasedLocked(lease *quotaInFlightLease, now time.Time) {
	keyHash := lease.key.keyHash
	state := s.concurrencyStates[keyHash]
	// The upstream counts its minute from the end of a request it had taken.
	// However the request ended (completed, failed mid-stream, cancelled by the
	// client), the upstream served it. A request it rejected does not count.
	//
	// It is recorded whatever the policy says right now: the upstream counts
	// from it regardless, so switching the rest back on must still know about
	// a request that ended while it was off. Only admission reads the policy.
	if lease.gate.accepted && keyHash != "" {
		if state == nil {
			state = &concurrencyState{}
			if s.concurrencyStates == nil {
				s.concurrencyStates = map[string]*concurrencyState{}
			}
			s.concurrencyStates[keyHash] = state
		}
		if now.After(state.lastEnded) {
			state.lastEnded = now
		}
	}
	if state == nil {
		return
	}
	if lease.gate.probe != 0 && state.probe == lease.gate.probe {
		state.probe = 0
	}
	s.concurrencyPruneLocked(keyHash, now)
}

// concurrencyPruneLocked drops an entry once no request for the key is running
// and it either recovered or has been left alone for the retention period.
func (s *Service) concurrencyPruneLocked(keyHash string, now time.Time) {
	state := s.concurrencyStates[keyHash]
	if state == nil || state.probe != 0 || s.quotaInFlightHashCountLocked(keyHash) > 0 {
		return
	}
	// When the last request ended stays known for as long as any rest setting
	// could still need it, whatever the setting is right now.
	rested := state.lastEnded.IsZero() || now.Sub(state.lastEnded) > concurrencyHoldLimitSeconds*time.Second
	recovered := state.strikes == 0 && !state.holdUntil.After(now)
	abandoned := now.Sub(state.holdUntil) > concurrencyStateRetention
	if rested && (recovered || abandoned) {
		delete(s.concurrencyStates, keyHash)
	}
}

// quotaInFlightHashCountLocked returns the admitted inferences for one
// inference-key fingerprint across every record that carries it.
func (s *Service) quotaInFlightHashCountLocked(keyHash string) int {
	total := 0
	for key, count := range s.quotaInflight {
		if key.keyHash == keyHash {
			total += count
		}
	}
	return total
}

// concurrencyRejection reports whether a terminal error is the upstream telling
// the account to slow down, and the upstream's own Retry-After when it sent one.
// A quota exhaustion is not one: the quota pause owns that signal.
//
// The upstream's own code decides first, because a rate limit can arrive as an
// error object inside a 200 response or as an event in an already open stream;
// there the observed HTTP status is 200. Both codes come from the closed
// allowlist in upstream.go, never from free upstream text. A bare 429 with no
// recognizable code counts too.
func concurrencyRejection(err error) (time.Duration, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil || !apiErr.upstreamAttempted || apiErr.quotaExhausted {
		return 0, false
	}
	switch apiErr.Kind {
	case "rate_limit_exceeded", "tier_limit_exceeded":
		return apiErr.upstreamRetryAfter, true
	case "insufficient_quota", "insufficient_balance":
		return 0, false
	}
	if apiErr.upstreamStatus == http.StatusTooManyRequests {
		return apiErr.upstreamRetryAfter, true
	}
	return 0, false
}

// settleConcurrency feeds one request's terminal outcome to the gate. It runs
// before the error is handed to the host, because a new hold is attached to the
// error as the cooldown the host should apply. It is idempotent per lease.
func (s *Service) settleConcurrency(lease *quotaInFlightLease, requestErr error) {
	if s == nil || lease == nil {
		return
	}
	if requestErr == nil {
		s.concurrencyAccepted(lease)
		return
	}
	hint, rejected := concurrencyRejection(requestErr)
	if !rejected {
		return
	}
	hold := s.concurrencyRejected(lease, hint)
	if hold <= 0 {
		return
	}
	var apiErr *APIError
	if errors.As(requestErr, &apiErr) && apiErr != nil && apiErr.Status == http.StatusTooManyRequests {
		// One second of slack: the host's cooldown should end after the hold, so
		// its next attempt is the probe rather than one more local refusal.
		apiErr.hostRetryAfter = hold + time.Second
	}
}

// isConcurrencyRefusal reports whether an error is the gate's own local refusal.
func isConcurrencyRefusal(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil || apiErr.upstreamAttempted {
		return false
	}
	return isConcurrencyKind(apiErr.Kind)
}

// isConcurrencyKind reports the kinds of the gate's own local refusals.
func isConcurrencyKind(kind string) bool {
	return kind == concurrencyLimitKind || kind == concurrencyHeldKind || kind == concurrencyRestingKind
}

// HostRetryAfterFor returns the cooldown to send to the host next to
// hostStatus, the status the host is actually given. The host reads a retry
// hint only for a 429, and HostErrorDetails may have neutralized the status, so
// anything else gets none: a cooldown must never travel with a status the
// plugin decided the host should not schedule on.
func HostRetryAfterFor(err error, hostStatus int) int {
	if hostStatus != http.StatusTooManyRequests {
		return 0
	}
	return HostRetryAfterSeconds(err)
}

// HostRetryAfterSeconds returns the cooldown the concurrency gate attached to
// this error, or 0 when there is none. Callers that talk to the host use
// HostRetryAfterFor.
func HostRetryAfterSeconds(err error) int {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil || apiErr.hostRetryAfter <= 0 {
		return 0
	}
	seconds := int((apiErr.hostRetryAfter + time.Second - 1) / time.Second)
	if seconds > concurrencyHoldLimitSeconds+1 {
		seconds = concurrencyHoldLimitSeconds + 1
	}
	return seconds
}

// concurrencyConfigView is the plugin-owned policy plus cumulative counters.
func (s *Service) concurrencyConfigView() map[string]any {
	now := s.concurrencyNow()
	s.mu.Lock()
	defer s.mu.Unlock()
	settings := s.cfg.concurrencySettings()
	held := 0
	for keyHash, state := range s.concurrencyStates {
		s.concurrencyPruneLocked(keyHash, now)
		if state.holdUntil.After(now) {
			held++
		}
	}
	return map[string]any{
		"enabled":          settings.enabled,
		"max_in_flight":    settings.maxInFlight,
		"hold_seconds":     int64(settings.hold / time.Second),
		"max_hold_seconds": int64(settings.maxHold / time.Second),
		"rest_seconds":     int64(settings.rest / time.Second),
		// Like the quota pause, this only decides what the plugin sends for its
		// own accounts. Selection, rotation and cooldown stay with CPA.
		"owner": "plugin",
		"limits": map[string]any{
			"max_in_flight":    []int{1, concurrencyMaxInFlightLimit},
			"hold_seconds":     []int{0, concurrencyHoldLimitSeconds},
			"max_hold_seconds": []int{0, concurrencyHoldLimitSeconds},
			"rest_seconds":     []int{0, concurrencyHoldLimitSeconds},
		},
		"stats": map[string]any{
			"since":                 s.concurrencySince.UTC(),
			"held_now":              held,
			"rejected_at_limit":     s.concurrencyStats.rejectedAtLimit,
			"rejected_while_held":   s.concurrencyStats.rejectedWhileHeld,
			"rejected_at_rest":      s.concurrencyStats.rejectedAtRest,
			"upstream_rate_limited": s.concurrencyStats.upstreamRateLimited,
			"holds_started":         s.concurrencyStats.holdsStarted,
		},
	}
}

// concurrencyAccountView is one account's live gate state for /runtime.
type concurrencyAccountView struct {
	InFlight     int `json:"in_flight"`
	PeakInFlight int `json:"peak_in_flight"`
	// RestingUntil is when the account may be used again after its last
	// accepted request ended; null when it is not resting.
	RestingUntil          *time.Time `json:"resting_until"`
	HeldUntil             *time.Time `json:"held_until"`
	Probing               bool       `json:"probing"`
	ConsecutiveRejections int        `json:"consecutive_rejections"`
}

// concurrencyRuntimeView returns the resolved limit and each credential's gate
// state. Records sharing one inference key report the same shared numbers.
func (s *Service) concurrencyRuntimeView(creds []Credential) (map[string]any, map[string]concurrencyAccountView) {
	hashes := make(map[string]string, len(creds))
	for _, credential := range creds {
		if credential.APIKey != "" {
			hashes[credential.ID] = quotaPauseKeyHash(credential.APIKey)
		}
	}
	now := s.concurrencyNow()
	s.mu.Lock()
	defer s.mu.Unlock()
	settings := s.cfg.concurrencySettings()
	live := make(map[string]bool, len(s.creds))
	for _, credential := range s.creds {
		if credential.APIKey != "" {
			live[quotaPauseKeyHash(credential.APIKey)] = true
		}
	}
	// A replaced or removed key has nothing left to report under its old
	// fingerprint once its last request finished. Dropping it here keeps both
	// maps bounded by the live accounts instead of by every key ever seen.
	for keyHash := range s.concurrencyPeak {
		if !live[keyHash] && s.quotaInFlightHashCountLocked(keyHash) == 0 {
			delete(s.concurrencyPeak, keyHash)
		}
	}
	for keyHash := range s.concurrencyStates {
		if !live[keyHash] && s.quotaInFlightHashCountLocked(keyHash) == 0 {
			delete(s.concurrencyStates, keyHash)
		}
	}

	rows := make(map[string]concurrencyAccountView, len(hashes))
	for id, keyHash := range hashes {
		row := concurrencyAccountView{
			InFlight:     s.quotaInFlightHashCountLocked(keyHash),
			PeakInFlight: s.concurrencyPeak[keyHash],
		}

		if state := s.concurrencyStates[keyHash]; state != nil {
			row.ConsecutiveRejections = state.strikes
			if settings.enabled && !state.lastEnded.IsZero() {
				if until := state.lastEnded.Add(settings.rest); until.After(now) {
					until = until.UTC()
					row.RestingUntil = &until
				}
			}
			if state.holdUntil.After(now) {
				until := state.holdUntil.UTC()
				row.HeldUntil = &until
			} else if state.strikes > 0 && settings.enabled && settings.hold > 0 {
				row.Probing = true
			}
		}
		rows[id] = row
	}
	return map[string]any{
		"enabled": settings.enabled, "max_in_flight": settings.maxInFlight,
		"rest_seconds": int64(settings.rest / time.Second),
	}, rows
}

// concurrencyNow is the gate's clock; a test replaces Service.concurrencyClock.
func (s *Service) concurrencyNow() time.Time {
	if s.concurrencyClock != nil {
		return s.concurrencyClock()
	}
	return time.Now()
}
