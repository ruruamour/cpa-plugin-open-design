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
		invalidated := entry.invalidated
		stop := s.stopCh
		s.mu.Unlock()
		select {
		case <-done:
		case <-invalidated:
		case <-stop:
		}
		return s.cachedModelCoverage(key)
	}
	if !entry.lastAttempt.IsZero() && s.modelCoverageNow().Before(entry.lastAttempt.Add(modelCoverageRefreshInterval)) {
		view := modelCoverageViewAt(entry.view, s.modelCoverageNow())
		s.mu.Unlock()
		return view, nil
	}
	done := make(chan struct{})
	entry.inflight = done
	c := s.creds[credentialID]
	stop := s.stopCh
	s.mu.Unlock()

	view := s.fetchModelCoverage(callbackID, c, modelID, entry, key, stop)
	s.mu.Lock()
	if current := s.modelCoverage[key]; current == entry && current.epoch == s.billingEpoch[credentialID] && view != nil {
		entry.view = *view
	}
	entry.inflight = nil
	close(done)
	result, resultErr := s.modelCoverageViewLocked(key)
	s.mu.Unlock()
	return result, resultErr
}

func (s *Service) cachedModelCoverage(key modelCoverageKey) (modelCoverageResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelCoverageViewLocked(key)
}

func (s *Service) modelCoverageViewLocked(key modelCoverageKey) (modelCoverageResponse, error) {
	if s.stopped {
		return modelCoverageResponse{}, fail(http.StatusServiceUnavailable, "OpenDesign plugin is shutting down")
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

func (s *Service) fetchModelCoverage(callbackID string, c Credential, modelID string, entry *modelCoverageEntry, key modelCoverageKey, stop <-chan struct{}) *modelCoverageResponse {
	ctx, cancel := context.WithTimeout(context.Background(), s.modelCoverageBudget)
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
	body, failure := s.modelCoverageGET(ctx, callbackID, c, modelID)
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

func (s *Service) modelCoverageGET(ctx context.Context, callbackID string, c Credential, modelID string) ([]byte, *quotaError) {
	query := url.Values{}
	query.Set("modelId", modelID)
	address := billingAPIURL + "/api/v1/workspaces/" + url.PathEscape(c.WorkspaceID) + "/billing/funding-check?" + query.Encode()
	var out upstreamCall
	if err := s.doOperationContext(ctx, callbackID, "host.http.do", map[string]any{
		"host_callback_id": callbackID,
		"method":           http.MethodGet,
		"url":              address,
		"headers":          upstreamHeaders(c.ControlKey),
	}, &out); err != nil {
		if s.ops.isClosed() {
			return nil, modelCoverageFailure("stopped")
		}
		var timeout net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() ||
			strings.Contains(strings.ToLower(err.Error()), "deadline exceeded") ||
			strings.Contains(strings.ToLower(err.Error()), "timed out") ||
			strings.Contains(strings.ToLower(err.Error()), "timeout") {
			return nil, modelCoverageFailure("timeout")
		}
		return nil, modelCoverageFailure("network")
	}
	switch out.StatusCode {
	case http.StatusUnauthorized:
		return nil, modelCoverageFailure("unauthorized")
	case http.StatusForbidden:
		return nil, modelCoverageFailure("forbidden")
	case http.StatusTooManyRequests:
		return nil, modelCoverageFailure("rate_limited")
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return nil, modelCoverageFailure("timeout")
	}
	if out.StatusCode < 200 || out.StatusCode >= 300 {
		return nil, modelCoverageFailure("upstream")
	}
	if len(out.Body) > maxQuotaBody {
		return nil, modelCoverageFailure("malformed")
	}
	return out.Body, nil
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
