package opendesign

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

func (s *Service) quotaRequest(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	defer s.done()
	id := strings.TrimSpace(r.Query.Get("credential_id"))
	if id == "" {
		return managementError(http.StatusBadRequest, "credential_id is required")
	}
	view, err := s.readQuota(id, r.HostCallbackID, r.Query.Get("refresh") == "1")
	if err != nil {
		return managementError(statusOf(err), err.Error())
	}
	return managementJSON(http.StatusOK, view)
}

func (s *Service) readQuota(id, callbackID string, refresh bool) (quotaResponse, error) {
	s.mu.Lock()
	entry, err := s.quotaEntryLocked(id)
	if err != nil {
		s.mu.Unlock()
		return quotaResponse{}, err
	}
	if !refresh || !entry.view.BillingConfigured {
		view := entry.view
		s.mu.Unlock()
		return view, nil
	}
	stop := s.stopCh
	if done := entry.inflight; done != nil {
		invalidated := entry.invalidated
		s.mu.Unlock()
		// Join without starting a second operation. Key changes wake waiters
		// immediately, even while the obsolete host operation is still pending.
		select {
		case <-done:
		case <-invalidated:
		case <-stop:
		}
		return s.cachedQuota(id)
	}
	if !entry.lastAttempt.IsZero() && s.quotaNow().Before(entry.lastAttempt.Add(quotaRefreshInterval)) {
		view := entry.view
		s.mu.Unlock()
		return view, nil
	}
	done := make(chan struct{})
	entry.inflight = done
	c := s.creds[id]
	s.mu.Unlock()

	view := s.fetchQuota(callbackID, c, entry, stop)
	s.mu.Lock()
	if current := s.quota[id]; current == entry && current.epoch == s.billingEpoch[id] && view != nil {
		entry.view = *view
	}
	entry.inflight = nil
	close(done)
	result, err := s.quotaViewLocked(id)
	s.mu.Unlock()
	return result, err
}

func (s *Service) cachedQuota(id string) (quotaResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quotaViewLocked(id)
}

func (s *Service) quotaViewLocked(id string) (quotaResponse, error) {
	if s.stopped {
		return quotaResponse{}, fail(http.StatusServiceUnavailable, "OpenDesign plugin is shutting down")
	}
	entry, err := s.quotaEntryLocked(id)
	if err != nil {
		return quotaResponse{}, err
	}
	return entry.view, nil
}

func (s *Service) quotaEntryLocked(id string) (*quotaEntry, error) {
	c, exists := s.creds[id]
	if !exists || s.revoked[id] {
		return nil, fail(http.StatusNotFound, "credential not found")
	}
	if entry := s.quota[id]; entry != nil && entry.epoch == s.billingEpoch[id] {
		return entry, nil
	}
	state, fundingState := "needs_refresh", "needs_refresh"
	if c.ControlKey == "" {
		state, fundingState = "not_configured", "not_configured"
	} else if c.WorkspaceID == "" {
		fundingState = "not_requested"
	}
	entry := &quotaEntry{epoch: s.billingEpoch[id], invalidated: make(chan struct{}), view: quotaResponse{
		CredentialID: id, BillingConfigured: c.ControlKey != "", WorkspaceID: c.WorkspaceID, State: state,
		Wallet: quotaPart[quotaWalletData]{State: state}, Funding: quotaPart[quotaFundingData]{State: fundingState},
	}}
	s.quota[id] = entry
	return entry, nil
}

func (s *Service) quotaCurrent(id string, entry *quotaEntry) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.stopped && s.quota[id] == entry && entry.epoch == s.billingEpoch[id]
}

func (s *Service) fetchQuota(callbackID string, c Credential, entry *quotaEntry, stop <-chan struct{}) *quotaResponse {
	select {
	case s.quotaSlots <- struct{}{}:
		defer func() { <-s.quotaSlots }()
	case <-entry.invalidated:
		return nil
	case <-stop:
		return nil
	}
	s.mu.Lock()
	if s.stopped || s.quota[c.ID] != entry || entry.epoch != s.billingEpoch[c.ID] {
		s.mu.Unlock()
		return nil
	}
	started := s.quotaNow()
	entry.lastAttempt = started
	next := started.Add(quotaRefreshInterval)
	view := entry.view
	view.NextRefreshAt = &next
	s.mu.Unlock()

	// Auth files can be operator-written, so reject header/control characters
	// here too rather than assuming every credential passed through POST.
	if len(c.ControlKey) > 8192 || strings.IndexFunc(c.ControlKey, unicode.IsControl) >= 0 ||
		len(c.WorkspaceID) > 512 || strings.IndexFunc(c.WorkspaceID, unicode.IsControl) >= 0 {
		view.Wallet = quotaFailed(view.Wallet, quotaFailure("malformed"))
		if c.WorkspaceID != "" {
			view.Funding = quotaFundingFailed(view.Funding, quotaFailure("malformed"))
		}
		return completeQuotaView(view)
	}
	body, failure := s.quotaGET(callbackID, billingAPIURL+"/api/v1/wallet/balance", c.ControlKey)
	if failure == nil {
		data, valid := parseQuotaWallet(body)
		if valid {
			now := s.quotaNow()
			view.Wallet = quotaPart[quotaWalletData]{State: "available", Data: data, FetchedAt: &now}
		} else {
			failure = quotaFailure("malformed")
		}
	}
	if failure != nil {
		view.Wallet = quotaFailed(view.Wallet, failure)
	}
	if c.WorkspaceID != "" && s.quotaCurrent(c.ID, entry) {
		// The official client allows a model-less read and requires modelId:null
		// in its response. No model selection or account scheduling is implied.
		body, failure = s.quotaGET(callbackID, billingAPIURL+"/api/v1/workspaces/"+url.PathEscape(c.WorkspaceID)+"/billing/funding-check", c.ControlKey)
		if failure == nil {
			data, valid := parseQuotaFunding(body, c.WorkspaceID)
			if valid {
				now := s.quotaNow()
				view.Funding = quotaPart[quotaFundingData]{State: "available", Data: data, FetchedAt: &now}
			} else {
				failure = quotaFailure("malformed")
			}
		}
		if failure != nil {
			view.Funding = quotaFundingFailed(view.Funding, failure)
		}
	}
	return completeQuotaView(view)
}

func completeQuotaView(view quotaResponse) *quotaResponse {
	view.FetchedAt = view.Wallet.FetchedAt
	wallet, funding := view.Wallet.State, view.Funding.State
	switch {
	case wallet == "stale" || funding == "stale":
		view.State = "stale"
	case wallet == "available" && (funding == "available" || funding == "not_requested"):
		view.State = "available"
	case wallet == "available" || funding == "available":
		view.State = "partial"
	default:
		view.State = "error"
	}
	return &view
}

func quotaFailed[T any](part quotaPart[T], failure *quotaError) quotaPart[T] {
	part.State, part.Error = "error", failure
	if part.Data != nil {
		part.State = "stale"
	}
	return part
}

func quotaFundingFailed(part quotaPart[quotaFundingData], failure *quotaError) quotaPart[quotaFundingData] {
	part = quotaFailed(part, failure)
	if part.Data != nil {
		data := *part.Data
		data.CodingPlan.Windows = append([]quotaWindow{}, data.CodingPlan.Windows...)
		for i := range data.CodingPlan.Windows {
			data.CodingPlan.Windows[i].Forecast = quotaForecast{State: "insufficient_samples"}
		}
		part.Data = &data
	}
	return part
}

// quotaGET uses only the documented host transport and operation callbacks.
// The host owns the deadline and cancels the operation with the request context;
// there is no invented timeout field or local HTTP client/background sampler.
func (s *Service) quotaGET(callbackID, address, controlKey string) ([]byte, *quotaError) {
	var out upstreamCall
	err := s.doOperation(callbackID, "host.http.do", map[string]any{
		"host_callback_id": callbackID, "method": http.MethodGet, "url": address, "headers": upstreamHeaders(controlKey),
	}, &out)
	if err != nil {
		if s.ops.isClosed() {
			return nil, quotaFailure("stopped")
		}
		var timeout net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() ||
			strings.Contains(strings.ToLower(err.Error()), "deadline exceeded") || strings.Contains(strings.ToLower(err.Error()), "timed out") {
			return nil, quotaFailure("timeout")
		}
		return nil, quotaFailure("network")
	}
	switch out.StatusCode {
	case http.StatusUnauthorized:
		return nil, quotaFailure("unauthorized")
	case http.StatusForbidden:
		return nil, quotaFailure("forbidden")
	case http.StatusTooManyRequests:
		return nil, quotaFailure("rate_limited")
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return nil, quotaFailure("timeout")
	}
	if out.StatusCode < 200 || out.StatusCode >= 300 {
		return nil, quotaFailure("upstream")
	}
	if len(out.Body) > maxQuotaBody {
		return nil, quotaFailure("malformed")
	}
	return out.Body, nil
}

func quotaFailure(kind string) *quotaError {
	messages := map[string]string{
		"unauthorized": "OpenDesign quota authorization was rejected.",
		"forbidden":    "OpenDesign quota is unavailable for this account or workspace.",
		"rate_limited": "OpenDesign quota requests are temporarily rate limited.",
		"timeout":      "OpenDesign quota request timed out.",
		"malformed":    "OpenDesign quota response or configuration was invalid.",
		"network":      "OpenDesign quota could not be reached.",
		"upstream":     "OpenDesign quota is temporarily unavailable.",
		"stopped":      "OpenDesign quota request was stopped.",
	}
	return &quotaError{Kind: kind, Message: messages[kind]}
}
