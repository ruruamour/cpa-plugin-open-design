package opendesign

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const (
	modelCoverageRefreshInterval = time.Minute
	modelCoverageBudget          = 30 * time.Second
	modelCoverageCacheCapacity   = 256
	// quotaRecoveryBudget bounds one multi-model recovery round. It is shorter
	// than the 30-second evidence freshness window, so a round cannot accept
	// evidence that expires while the remaining models are checked.
	quotaRecoveryBudget = 25 * time.Second
)

// modelCoverageResponse is deliberately smaller than quotaResponse. A model
// preflight reports official coverage signals only; it never presents shared
// window credits as a model-specific balance.
type modelCoverageResponse struct {
	CredentialID      string      `json:"credential_id"`
	ModelID           string      `json:"model_id"`
	BillingConfigured bool        `json:"billing_configured"`
	WorkspaceID       string      `json:"workspace_id"`
	State             string      `json:"state"`
	FetchedAt         *time.Time  `json:"fetched_at"`
	NextRefreshAt     *time.Time  `json:"next_refresh_at"`
	ModelCovered      *bool       `json:"model_covered"`
	FundingCheck      *string     `json:"funding_check"`
	Funding           *string     `json:"funding"`
	Error             *quotaError `json:"error"`

	// Evidence is immutable and belongs to the last validated successful HTTP
	// response. It is never serialized, including when the view becomes stale.
	evidence *modelCoverageEvidence
	// True only on the result of the read that started or joined this refresh;
	// it is never stored in the cache or serialized.
	refreshedForRead bool
}

type modelCoverageEvidence struct {
	credentialID      string
	workspaceID       string
	workspaceMemberID string // exact validated value; never serialize or log
	modelID           string
	fundingCheck      string
	requestStartedAt  time.Time
	generatedAt       time.Time
}

type modelCoverageKey struct {
	CredentialID string
	ModelID      string
}

type modelCoverageEntry struct {
	epoch       uint64
	view        modelCoverageResponse
	lastAttempt time.Time
	lastAccess  time.Time
	inflight    chan struct{}
	completed   chan struct{} // last published attempt, for fencing joined reads
	invalidated chan struct{}
}

func (s *Service) modelCoverageRequest(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	defer s.done()
	credentialID := strings.TrimSpace(r.Query.Get("credential_id"))
	if credentialID == "" {
		return managementError(http.StatusBadRequest, "credential_id is required")
	}
	modelID := strings.TrimSpace(r.Query.Get("model_id"))
	if modelID == "" {
		return managementError(http.StatusBadRequest, "model_id is required")
	}
	if len(modelID) > 512 || strings.IndexFunc(modelID, unicode.IsControl) >= 0 {
		return managementError(http.StatusBadRequest, "invalid model_id")
	}
	view, err := s.readModelCoverage(credentialID, modelID, r.HostCallbackID, r.Query.Get("refresh") == "1")
	if err != nil {
		return managementError(statusOf(err), err.Error())
	}
	return managementJSON(http.StatusOK, view)
}

func (s *Service) readModelCoverage(credentialID, modelID, callbackID string, refresh bool) (modelCoverageResponse, error) {
	return s.readModelCoverageContext(context.Background(), credentialID, modelID, callbackID, refresh)
}

// readModelCoverageContext bounds one refresh read (including waiting for a
// shared in-flight refresh) by the caller's deadline. Joining a shared refresh
// stays cancellable: expiry falls back to the cached view, which never counts as
// fresh evidence for a recovery decision.
func (s *Service) readModelCoverageContext(ctx context.Context, credentialID, modelID, callbackID string, refresh bool) (modelCoverageResponse, error) {
	return s.readModelCoverageMode(ctx, credentialID, modelID, callbackID, refresh, false)
}

// readModelCoverageForProof starts a brand-new official request instead of
// reusing a cached view, the refresh throttle, or an attempt that was already in
// flight. quotaRecoveryProven requires evidence requested after the recheck
// began; a throttled cache hit or a joined older attempt can never satisfy that,
// so a recovery read must not silently inherit either.
func (s *Service) readModelCoverageForProof(ctx context.Context, credentialID, modelID, callbackID string) (modelCoverageResponse, error) {
	return s.readModelCoverageMode(ctx, credentialID, modelID, callbackID, true, true)
}

func (s *Service) readModelCoverageMode(ctx context.Context, credentialID, modelID, callbackID string, refresh, forceFetch bool) (modelCoverageResponse, error) {
	key := modelCoverageKey{CredentialID: credentialID, ModelID: modelID}
	s.mu.Lock()
	entry := s.modelCoverage[key]
	if entry == nil || entry.epoch != s.billingEpoch[credentialID] {
		view, err := s.modelCoverageInitialViewLocked(key)
		if err != nil {
			s.mu.Unlock()
			return modelCoverageResponse{}, err
		}
		if !refresh || !view.BillingConfigured || view.WorkspaceID == "" {
			s.mu.Unlock()
			return modelCoverageViewAt(view, s.modelCoverageNow()), nil
		}
		entry, err = s.modelCoverageEntryLocked(key)
		if err != nil {
			s.mu.Unlock()
			return modelCoverageResponse{}, err
		}
	} else {
		entry.lastAccess = s.modelCoverageNow()
	}
	if !refresh || !entry.view.BillingConfigured || entry.view.WorkspaceID == "" {
		view := modelCoverageViewAt(entry.view, s.modelCoverageNow())
		s.mu.Unlock()
		return view, nil
	}
	if done := entry.inflight; done != nil {
		if forceFetch {
			return s.readModelCoverageAfterInflight(ctx, key, entry, done, callbackID, modelID)
		}
		invalidated := entry.invalidated
		stop := s.stopCh
		s.mu.Unlock()
		select {
		case <-done:
			return s.joinedModelCoverage(key, entry, done)
		case <-invalidated:
		case <-stop:
		case <-ctx.Done():
		}
		return s.cachedModelCoverage(key)
	}
	if !forceFetch && !entry.lastAttempt.IsZero() && s.modelCoverageNow().Before(entry.lastAttempt.Add(modelCoverageRefreshInterval)) {
		view := modelCoverageViewAt(entry.view, s.modelCoverageNow())
		s.mu.Unlock()
		return view, nil
	}
	done := make(chan struct{})
	entry.inflight = done
	c := s.creds[credentialID]
	stop := s.stopCh
	s.mu.Unlock()

	view := s.fetchModelCoverage(ctx, callbackID, c, modelID, entry, key, stop)
	s.mu.Lock()
	published := s.modelCoverage[key] == entry && entry.epoch == s.billingEpoch[credentialID] && view != nil
	if published {
		entry.view = *view
		entry.completed = done
	}
	entry.inflight = nil
	close(done)
	result, resultErr := s.modelCoverageViewLocked(key)
	result.refreshedForRead = resultErr == nil && published
	s.mu.Unlock()
	return result, resultErr
}

func (s *Service) cachedModelCoverage(key modelCoverageKey) (modelCoverageResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelCoverageViewLocked(key)
}

// readModelCoverageAfterInflight waits for the attempt that is already running
// to finish and then starts its own, because a recovery proof cannot use evidence
// whose request began before the recheck did. The wait is bounded by the caller's
// deadline and the plugin lifecycle; any of those aborts the read instead of
// falling back to the cached view.
func (s *Service) readModelCoverageAfterInflight(ctx context.Context, key modelCoverageKey, entry *modelCoverageEntry, done chan struct{}, callbackID, modelID string) (modelCoverageResponse, error) {
	invalidated := entry.invalidated
	stop := s.stopCh
	s.mu.Unlock()
	select {
	case <-done:
	case <-invalidated:
		return s.cachedModelCoverage(key)
	case <-stop:
		return s.cachedModelCoverage(key)
	case <-ctx.Done():
		return modelCoverageResponse{}, localFail(http.StatusConflict, "quota_recheck_timeout", "官方复查超时，请稍后重试")
	}
	return s.readModelCoverageMode(ctx, key.CredentialID, key.ModelID, callbackID, true, true)
}

// A waiter may wake after invalidation or after a different refresh completed.
// Only the result actually published by the joined attempt is non-cached.
func (s *Service) joinedModelCoverage(key modelCoverageKey, entry *modelCoverageEntry, done chan struct{}) (modelCoverageResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	view, err := s.modelCoverageViewLocked(key)
	view.refreshedForRead = err == nil && s.modelCoverage[key] == entry &&
		entry.epoch == s.billingEpoch[key.CredentialID] && entry.completed == done
	return view, err
}

func (s *Service) modelCoverageViewLocked(key modelCoverageKey) (modelCoverageResponse, error) {
	if s.stopped {
		return modelCoverageResponse{}, fail(http.StatusServiceUnavailable, "plugin is shutting down")
	}
	if entry := s.modelCoverage[key]; entry != nil && entry.epoch == s.billingEpoch[key.CredentialID] {
		entry.lastAccess = s.modelCoverageNow()
		return modelCoverageViewAt(entry.view, s.modelCoverageNow()), nil
	}
	view, err := s.modelCoverageInitialViewLocked(key)
	if err != nil {
		return modelCoverageResponse{}, err
	}
	return modelCoverageViewAt(view, s.modelCoverageNow()), nil
}

func modelCoverageViewAt(view modelCoverageResponse, _ time.Time) modelCoverageResponse {
	view.refreshedForRead = false
	return view
}

func (s *Service) modelCoverageInitialViewLocked(key modelCoverageKey) (modelCoverageResponse, error) {
	c, exists := s.creds[key.CredentialID]
	if !exists || s.revoked[key.CredentialID] {
		return modelCoverageResponse{}, fail(http.StatusNotFound, "credential not found")
	}
	state := "not_configured"
	if c.ControlKey != "" && c.WorkspaceID != "" {
		state = "needs_refresh"
	}
	return modelCoverageResponse{
		CredentialID:      key.CredentialID,
		ModelID:           key.ModelID,
		BillingConfigured: c.ControlKey != "",
		WorkspaceID:       c.WorkspaceID,
		State:             state,
	}, nil
}

func (s *Service) modelCoverageEntryLocked(key modelCoverageKey) (*modelCoverageEntry, error) {
	c, exists := s.creds[key.CredentialID]
	if !exists || s.revoked[key.CredentialID] {
		return nil, fail(http.StatusNotFound, "credential not found")
	}
	if entry := s.modelCoverage[key]; entry != nil && entry.epoch == s.billingEpoch[key.CredentialID] {
		entry.lastAccess = s.modelCoverageNow()
		return entry, nil
	}
	if old := s.modelCoverage[key]; old != nil {
		close(old.invalidated)
		delete(s.modelCoverage, key)
	}
	if len(s.modelCoverage) >= modelCoverageCacheCapacity {
		var oldestKey modelCoverageKey
		var oldest *modelCoverageEntry
		now := s.modelCoverageNow()
		for candidateKey, candidate := range s.modelCoverage {
			if candidate.inflight != nil {
				continue
			}
			if !candidate.lastAttempt.IsZero() && now.Before(candidate.lastAttempt.Add(modelCoverageRefreshInterval)) {
				continue
			}
			if oldest == nil || candidate.lastAccess.Before(oldest.lastAccess) {
				oldestKey, oldest = candidateKey, candidate
			}
		}
		if oldest == nil {
			return nil, fail(http.StatusServiceUnavailable, "OpenDesign model coverage cache is busy")
		}
		close(oldest.invalidated)
		delete(s.modelCoverage, oldestKey)
	}
	state := "not_configured"
	if c.ControlKey != "" && c.WorkspaceID != "" {
		state = "needs_refresh"
	}
	now := s.modelCoverageNow()
	entry := &modelCoverageEntry{
		epoch:       s.billingEpoch[key.CredentialID],
		lastAccess:  now,
		invalidated: make(chan struct{}),
		view: modelCoverageResponse{
			CredentialID:      key.CredentialID,
			ModelID:           key.ModelID,
			BillingConfigured: c.ControlKey != "",
			WorkspaceID:       c.WorkspaceID,
			State:             state,
		},
	}
	s.modelCoverage[key] = entry
	return entry, nil
}

func (s *Service) modelCoverageCurrent(key modelCoverageKey, entry *modelCoverageEntry) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.stopped && s.modelCoverage[key] == entry && entry.epoch == s.billingEpoch[key.CredentialID]
}

func (s *Service) fetchModelCoverage(parent context.Context, callbackID string, c Credential, modelID string, entry *modelCoverageEntry, key modelCoverageKey, stop <-chan struct{}) *modelCoverageResponse {
	// The per-request budget is unchanged; it is clamped by the caller's own
	// deadline so one recovery round cannot exceed its total budget.
	ctx, cancel := context.WithTimeout(parent, s.modelCoverageBudget)
	defer cancel()
	s.mu.Lock()
	if s.stopped || s.modelCoverage[key] != entry || entry.epoch != s.billingEpoch[key.CredentialID] {
		s.mu.Unlock()
		return nil
	}
	started := s.modelCoverageNow()
	entry.lastAttempt = started
	next := started.Add(modelCoverageRefreshInterval)
	view := entry.view
	view.NextRefreshAt = &next
	s.mu.Unlock()

	select {
	case s.modelCoverageSlots <- struct{}{}:
		defer func() { <-s.modelCoverageSlots }()
	case <-entry.invalidated:
		return nil
	case <-stop:
		return nil
	case <-ctx.Done():
		return modelCoverageFailureView(view, modelCoverageFailure("timeout"))
	}
	if !s.modelCoverageCurrent(key, entry) {
		return nil
	}
	if len(c.ControlKey) > 8192 || strings.IndexFunc(c.ControlKey, unicode.IsControl) >= 0 ||
		len(c.WorkspaceID) > 512 || strings.IndexFunc(c.WorkspaceID, unicode.IsControl) >= 0 {
		return modelCoverageFailureView(view, modelCoverageFailure("malformed"))
	}
	body, requestStartedAt, failure := s.modelCoverageGET(ctx, callbackID, c, modelID)
	if failure == nil {
		data, valid := parseModelCoverageFunding(body, c.WorkspaceID, modelID)
		if valid {
			now := s.modelCoverageNow()
			view.State = "available"
			view.FetchedAt = &now
			view.ModelCovered = data.ModelCovered
			view.FundingCheck = data.FundingCheck
			funding := data.Funding
			view.Funding = &funding
			view.Error = nil
			check := ""
			if data.FundingCheck != nil {
				check = *data.FundingCheck
			}
			view.evidence = &modelCoverageEvidence{
				credentialID: key.CredentialID, workspaceID: c.WorkspaceID, workspaceMemberID: data.workspaceMemberID,
				modelID: modelID, fundingCheck: check, requestStartedAt: requestStartedAt, generatedAt: data.generatedAt,
			}
		} else {
			failure = modelCoverageFailure("malformed")
		}
	}
	if failure != nil {
		return modelCoverageFailureView(view, failure)
	}
	return &view
}

func modelCoverageFailureView(view modelCoverageResponse, failure *quotaError) *modelCoverageResponse {
	view.Error = failure
	if view.ModelCovered != nil || view.FundingCheck != nil || view.Funding != nil {
		view.State = "stale"
	} else {
		view.State = "error"
	}
	return &view
}

func (s *Service) modelCoverageGET(ctx context.Context, callbackID string, c Credential, modelID string) ([]byte, time.Time, *quotaError) {
	query := url.Values{}
	query.Set("modelId", modelID)
	address := billingAPIURL + "/api/v1/workspaces/" + url.PathEscape(c.WorkspaceID) + "/billing/funding-check?" + query.Encode()
	var out upstreamCall
	var requestStartedAt time.Time
	if err := s.doOperationContextOnStart(ctx, callbackID, "host.http.do", map[string]any{
		"host_callback_id": callbackID,
		"method":           http.MethodGet,
		"url":              address,
		"headers":          upstreamHeaders(c.ControlKey),
	}, &out, func() { requestStartedAt = s.modelCoverageNow() }); err != nil {
		if s.ops.isClosed() {
			return nil, time.Time{}, modelCoverageFailure("stopped")
		}
		var timeout net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() ||
			strings.Contains(strings.ToLower(err.Error()), "deadline exceeded") ||
			strings.Contains(strings.ToLower(err.Error()), "timed out") ||
			strings.Contains(strings.ToLower(err.Error()), "timeout") {
			return nil, time.Time{}, modelCoverageFailure("timeout")
		}
		return nil, time.Time{}, modelCoverageFailure("network")
	}
	switch out.StatusCode {
	case http.StatusUnauthorized:
		return nil, time.Time{}, modelCoverageFailure("unauthorized")
	case http.StatusForbidden:
		return nil, time.Time{}, modelCoverageFailure("forbidden")
	case http.StatusTooManyRequests:
		return nil, time.Time{}, modelCoverageFailure("rate_limited")
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return nil, time.Time{}, modelCoverageFailure("timeout")
	}
	if out.StatusCode < 200 || out.StatusCode >= 300 {
		return nil, time.Time{}, modelCoverageFailure("upstream")
	}
	if len(out.Body) > maxQuotaBody {
		return nil, time.Time{}, modelCoverageFailure("malformed")
	}
	return out.Body, requestStartedAt, nil
}

func modelCoverageFailure(kind string) *quotaError {
	messages := map[string]string{
		"unauthorized": "OpenDesign model coverage authorization was rejected.",
		"forbidden":    "OpenDesign model coverage is unavailable for this account or workspace.",
		"rate_limited": "OpenDesign model coverage requests are temporarily rate limited.",
		"timeout":      "OpenDesign model coverage request timed out.",
		"malformed":    "OpenDesign model coverage response or configuration was invalid.",
		"network":      "OpenDesign model coverage could not be reached.",
		"upstream":     "OpenDesign model coverage is temporarily unavailable.",
		"stopped":      "OpenDesign model coverage request was stopped.",
	}
	return &quotaError{Kind: kind, Message: messages[kind]}
}
