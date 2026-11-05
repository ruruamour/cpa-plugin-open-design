package opendesign

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// ManagementRequest is the CPA management RPC payload.
type ManagementRequest struct {
	Method         string      `json:"Method"`
	Path           string      `json:"Path"`
	Headers        http.Header `json:"Headers"`
	Query          url.Values  `json:"Query"`
	Body           []byte      `json:"Body"`
	HostCallbackID string      `json:"host_callback_id"`
}

// ManagementResponse is the CPA management RPC result. Field names match the SDK
// struct so the host decodes it without a custom adapter.
type ManagementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

// managementRoute pairs a method with the exact path CPA will dispatch.
type managementRoute struct {
	Method      string
	Path        string
	Menu        string
	Description string
}

// managementRoutes lists everything this plugin serves. CPA dispatches management
// requests by an exact "METHOD full/path" lookup and rejects path parameters, so
// an action on one credential carries its id in the body (POST) or the query
// string (DELETE) rather than in a path segment.
func managementRoutes() []managementRoute {
	return []managementRoute{
		{Method: http.MethodGet, Path: consoleResource, Menu: "OpenDesign", Description: "OpenDesign 模型映射与凭据"},
		{Method: http.MethodGet, Path: consolePath},
		{Method: http.MethodGet, Path: apiBase + "/status"},
		{Method: http.MethodGet, Path: apiBase + "/credentials"},
		{Method: http.MethodGet, Path: apiBase + "/quota"},
		{Method: http.MethodGet, Path: apiBase + "/model-coverage"},
		{Method: http.MethodGet, Path: apiBase + "/request-logs"},
		{Method: http.MethodGet, Path: apiBase + "/request-stats"},
		{Method: http.MethodGet, Path: apiBase + "/logs"},
		{Method: http.MethodGet, Path: apiBase + "/logs/stats"},
		{Method: http.MethodDelete, Path: apiBase + "/request-logs"},
		{Method: http.MethodDelete, Path: apiBase + "/logs"},
		{Method: http.MethodPost, Path: apiBase + "/request-logs/clear"},
		{Method: http.MethodPost, Path: apiBase + "/logs/clear"},
		{Method: http.MethodGet, Path: apiBase + "/config"},
		{Method: http.MethodPost, Path: apiBase + "/config"},
		{Method: http.MethodGet, Path: apiBase + "/runtime"},
		{Method: http.MethodGet, Path: apiBase + "/error-rules"},
		{Method: http.MethodPut, Path: apiBase + "/error-rules"},
		{Method: http.MethodPost, Path: apiBase + "/error-rules/sync"},
		{Method: http.MethodPost, Path: apiBase + "/error-rules/reset"},
		{Method: http.MethodPost, Path: apiBase + "/model-test"},
		{Method: http.MethodPost, Path: apiBase + "/credentials"},
		{Method: http.MethodDelete, Path: apiBase + "/credentials"},
		{Method: http.MethodGet, Path: apiBase + "/upstream-models"},
		{Method: http.MethodGet, Path: apiBase + "/models"},
		{Method: http.MethodPost, Path: apiBase + "/models"},
		{Method: http.MethodPost, Path: apiBase + "/models/reasoning"},
		{Method: http.MethodGet, Path: apiBase + "/quota-pause"},
		{Method: http.MethodPost, Path: apiBase + "/quota-pause/resume"},
		{Method: http.MethodPost, Path: apiBase + "/quota-pause/recheck"},
		{Method: http.MethodPost, Path: apiBase + "/console/session"},
	}
}

func (s *Service) registerManagement() (any, error) {
	routes := make([]map[string]string, 0, len(managementRoutes()))
	resources := make([]map[string]string, 0, 1)
	for _, r := range managementRoutes() {
		if r.Menu != "" {
			resources = append(resources, map[string]string{"Path": r.Path, "Menu": r.Menu, "Description": r.Description})
			continue
		}
		routes = append(routes, map[string]string{"Method": r.Method, "Path": r.Path})
	}
	return map[string]any{"routes": routes, "resources": resources}, nil
}

func managementJSON(status int, v any) (any, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fail(http.StatusInternalServerError, "could not encode management response")
	}
	return ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
		Body:       body,
	}, nil
}

func managementError(status int, message string, keys ...string) (any, error) {
	return managementJSON(status, map[string]any{"error": redact(message, keys...)})
}

// management dispatches one management request.
func (s *Service) management(raw json.RawMessage) (any, error) {
	var r ManagementRequest
	if err := json.Unmarshal(raw, &r); err != nil {
		return managementError(http.StatusBadRequest, "invalid management request")
	}
	path := strings.TrimRight(r.Path, "/")
	if path == "" {
		path = r.Path
	}
	switch {
	case r.Method == http.MethodGet && (path == consoleResource || path == consolePath):
		page := consolePage()
		return ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":           []string{"text/html; charset=utf-8"},
				"Cache-Control":          []string{"no-store"},
				"X-Content-Type-Options": []string{"nosniff"},
			},
			Body: page,
		}, nil
	case r.Method == http.MethodPost && path == apiBase+"/console/session":
		// The console verifies a management key by touching a cheap authenticated
		// route; this endpoint exists only so a failure is a clear 401 instead of
		// a 404 from the host's route table.
		return s.withActive(func() (any, error) { return managementJSON(http.StatusOK, map[string]any{"ok": true}) })
	case r.Method == http.MethodGet && path == apiBase+"/status":
		return s.withActive(func() (any, error) { return s.statusResponse() })
	case r.Method == http.MethodGet && path == apiBase+"/credentials":
		return s.withActive(func() (any, error) { return s.listCredentials() })
	case r.Method == http.MethodGet && path == apiBase+"/quota":
		return s.quotaRequest(r)
	case r.Method == http.MethodGet && path == apiBase+"/model-coverage":
		return s.modelCoverageRequest(r)
	case r.Method == http.MethodGet && (path == apiBase+"/request-logs" || path == apiBase+"/logs"):
		return s.requestLogsRequest(r)
	case r.Method == http.MethodGet && (path == apiBase+"/request-stats" || path == apiBase+"/logs/stats"):
		return s.requestLogStatsRequest(r)
	case (r.Method == http.MethodDelete && (path == apiBase+"/request-logs" || path == apiBase+"/logs")) ||
		(r.Method == http.MethodPost && (path == apiBase+"/request-logs/clear" || path == apiBase+"/logs/clear")):
		return s.requestLogClearRequest(r)
	case r.Method == http.MethodGet && path == apiBase+"/config":
		return s.configRequest()
	case r.Method == http.MethodPost && path == apiBase+"/config":
		return s.updateConfigRequest(r)
	case r.Method == http.MethodGet && path == apiBase+"/runtime":
		return s.runtimeRequest()
	case (r.Method == http.MethodGet || r.Method == http.MethodPut) && path == apiBase+"/error-rules":
		return s.withActive(func() (any, error) { return s.errorRulesRequest(r) })
	case r.Method == http.MethodPost && (path == apiBase+"/error-rules/sync" || path == apiBase+"/error-rules/reset"):
		return s.withActive(func() (any, error) { return s.errorRulesRequest(r) })
	case r.Method == http.MethodPost && path == apiBase+"/model-test":
		return s.modelTestRequest(r)
	case r.Method == http.MethodPost && path == apiBase+"/credentials":
		return s.withActive(func() (any, error) { return s.saveCredentialRequest(r) })
	case r.Method == http.MethodDelete && path == apiBase+"/credentials":
		return s.withActive(func() (any, error) { return s.deleteCredential(r.Query.Get("id")) })

	case r.Method == http.MethodGet && path == apiBase+"/upstream-models":
		return s.upstreamModels(r)
	case r.Method == http.MethodGet && path == apiBase+"/models":
		return s.withActive(func() (any, error) {
			cfg := s.config()
			return managementJSON(http.StatusOK, map[string]any{
				"models":          modelViews(cfg.Models),
				"models_revision": modelsRevision(cfg.Models),
			})
		})
	case r.Method == http.MethodPost && path == apiBase+"/models":
		return s.withActive(func() (any, error) {
			result, err := s.importModels(r.Body)
			if err != nil {
				return managementError(statusOf(err), err.Error(), s.allKeys()...)
			}
			return managementJSON(http.StatusOK, result)
		})
	case r.Method == http.MethodGet && path == apiBase+"/quota-pause":
		return s.withActive(func() (any, error) { return managementJSON(http.StatusOK, s.quotaPauseResponse()) })
	case r.Method == http.MethodPost && (path == apiBase+"/quota-pause/resume" || path == apiBase+"/quota-pause/recheck"):
		return s.withActive(func() (any, error) { return s.quotaPauseAction(r) })
	case r.Method == http.MethodPost && path == apiBase+"/models/reasoning":
		return s.withActive(func() (any, error) {
			result, err := s.saveReasoningMapping(r.Body)
			if err != nil {
				return managementError(statusOf(err), err.Error(), s.allKeys()...)
			}
			return managementJSON(http.StatusOK, result)
		})
	default:
		return managementError(http.StatusNotFound, "not found")
	}
}

// statusResponse is the shape the console reads. It never contains a key.
func (s *Service) statusResponse() (any, error) {
	cfg := s.config()
	// Resolve the clock before taking the read lock: quotaPauseTime takes the
	// same lock, and a recursive RLock can deadlock behind a pending writer.
	now := s.quotaPauseTime()
	s.mu.RLock()
	credentials := make([]map[string]any, 0, len(s.creds))
	for _, c := range s.creds {
		credentials = append(credentials, credentialView(c, now, s.quotaPauseErrors[c.ID], s.quotaRecheckStatus[c.ID]))
	}
	s.mu.RUnlock()
	sortCredentials(credentials)
	models := modelViews(cfg.Models)
	return managementJSON(http.StatusOK, map[string]any{"version": Version,
		"base_url":        cfg.BaseURL,
		"credentials":     credentials,
		"models":          models,
		"models_revision": modelsRevision(cfg.Models),
	})
}

func sortCredentials(items []map[string]any) {
	// Stable, predictable order for the UI without exposing map iteration order.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && stringOf(items[j]["label"]) < stringOf(items[j-1]["label"]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// quotaPauseView builds the console-facing detail of one plugin-owned pause.
// It carries no key material: reason/source are fixed plugin codes, and
// syncError is only ever the fixed, redacted persistence failure recorded in
// quotaPauseErrors. The list surface (GET /quota-pause) and the per-credential
// views (GET /status, /credentials and mutation responses) share this shape so
// a synchronization failure can never be visible on only one of them.
func quotaPauseView(c Credential, now time.Time, syncError, recheckStatus string) map[string]any {
	block := c.QuotaBlock
	_, modelsComplete := quotaBlockModels(block)
	pause := map[string]any{
		"reason":           block.Reason,
		"source":           block.Source,
		"blocked_at":       block.BlockedAt,
		"observed_at":      block.ObservedAt,
		"resume_at":        block.RetryAt,
		"manual_disabled":  c.Disabled,
		"model_id":         block.ModelID,
		"multiple_models":  block.MultipleModels,
		"models_ambiguous": !modelsComplete,
	}
	if remaining := block.RetryAt.Sub(now); remaining > 0 {
		pause["remaining_seconds"] = int64(remaining / time.Second)
	} else {
		// The deadline is a review hint, not a recovery. Report that a recheck is
		// now due instead of a countdown, and never claim the account is enabled
		// again: nothing in the plugin verifies the upstream quota here.
		pause["awaiting_recheck"] = true
	}
	if syncError != "" {
		pause["error"] = syncError
	}
	if recheckStatus != "" {
		pause["recheck_status"] = recheckStatus
	}
	return pause
}

func credentialView(c Credential, now time.Time, syncError, recheckStatus string) map[string]any {
	view := map[string]any{
		"id": c.ID, "label": c.Label, "disabled": c.Disabled,
		"billing_configured": c.ControlKey != "", "workspace_id": c.WorkspaceID,
		"billing_revision": c.billingRevision,
	}
	// A quota block is a separate, plugin-owned scheduling restriction. The
	// `disabled` field keeps the operator's own choice so the console can show
	// the two independently, and no key fingerprint is ever exposed.
	blocked := c.quotaBlocked()
	view["quota_paused"] = blocked
	view["effective_disabled"] = c.effectiveDisabled()
	if blocked {
		view["quota_pause"] = quotaPauseView(c, now, syncError, recheckStatus)
	}
	return view
}

func (s *Service) listCredentials() (any, error) {
	now := s.quotaPauseTime()
	s.mu.RLock()
	items := make([]map[string]any, 0, len(s.creds))
	for _, c := range s.creds {
		items = append(items, credentialView(c, now, s.quotaPauseErrors[c.ID], s.quotaRecheckStatus[c.ID]))
	}
	s.mu.RUnlock()
	sortCredentials(items)
	return managementJSON(http.StatusOK, map[string]any{"credentials": items})
}

type credentialInput struct {
	ID           string  `json:"id"`
	Label        string  `json:"label"`
	APIKey       *string `json:"api_key"`
	ControlKey   *string `json:"control_key"`
	WorkspaceID  *string `json:"workspace_id"`
	ClearBilling bool    `json:"clear_billing"`
	Disabled     *bool   `json:"disabled"`
}

// saveCredentialRequest creates a credential, or updates the one named by "id".
func (s *Service) saveCredentialRequest(r ManagementRequest) (any, error) {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	var in credentialInput
	if err := json.Unmarshal(r.Body, &in); err != nil {
		return managementError(http.StatusBadRequest, "invalid credential JSON")
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = strings.TrimSpace(r.Query.Get("id"))
	}
	if id == "" {
		return s.createCredential(in)
	}
	return s.updateCredential(id, in)
}

func applyBillingInput(c *Credential, in credentialInput) error {
	if in.ClearBilling {
		c.ControlKey, c.WorkspaceID = "", ""
		return nil
	}
	if in.ControlKey != nil && strings.TrimSpace(*in.ControlKey) != "" {
		key := strings.TrimSpace(*in.ControlKey)
		if len(key) > 8192 || strings.IndexFunc(key, unicode.IsControl) >= 0 {
			return fail(http.StatusBadRequest, "invalid control_key")
		}
		c.ControlKey = key
	}
	if in.WorkspaceID != nil && strings.TrimSpace(*in.WorkspaceID) != "" {
		id := strings.TrimSpace(*in.WorkspaceID)
		if len(id) > 512 || strings.IndexFunc(id, unicode.IsControl) >= 0 {
			return fail(http.StatusBadRequest, "invalid workspace_id")
		}
		c.WorkspaceID = id
	}
	return nil
}

func (s *Service) createCredential(in credentialInput) (any, error) {
	label, apiKey := in.Label, in.APIKey
	if apiKey == nil {
		return managementError(http.StatusBadRequest, "api_key is required")
	}
	key := strings.TrimSpace(*apiKey)
	if len(key) < 8 || strings.ContainsAny(key, "\r\n") {
		return managementError(http.StatusBadRequest, "invalid api_key")
	}
	label = strings.TrimSpace(label)
	if len(label) > 100 {
		return managementError(http.StatusBadRequest, "label exceeds 100 characters")
	}
	cfg := s.config()
	c := Credential{Type: Provider, ID: PluginID + "-" + newID(), Label: label, APIKey: key, BaseURL: cfg.BaseURL, RequestScopedErrors: s.currentErrorRules()}
	if err := applyBillingInput(&c, in); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	if c.Label == "" {
		c.Label = "OpenDesign"
	}
	filename := c.ID + ".json"
	if err := s.saveCredential(c, filename); err != nil {
		return managementError(http.StatusInternalServerError, "credential persistence failed")
	}
	s.mu.Lock()
	s.storeCredentialLocked(c)
	c = s.creds[c.ID]
	s.authFiles[c.ID] = filename
	delete(s.revoked, c.ID)
	syncError := s.quotaPauseErrors[c.ID]
	recheckStatus := s.quotaRecheckStatus[c.ID]
	s.mu.Unlock()
	// The response deliberately omits the key; a pause sync failure is reported
	// with the same fixed, redacted text as GET /status.
	return managementJSON(http.StatusCreated, credentialView(c, s.quotaPauseTime(), syncError, recheckStatus))
}

func (s *Service) updateCredential(id string, in credentialInput) (any, error) {
	label, apiKey, disabled := in.Label, in.APIKey, in.Disabled
	s.mu.RLock()
	c, ok := s.creds[id]
	s.mu.RUnlock()
	if !ok {
		return managementError(http.StatusNotFound, "credential not found")
	}
	if label = strings.TrimSpace(label); label != "" {
		if len(label) > 100 {
			return managementError(http.StatusBadRequest, "label exceeds 100 characters")
		}
		c.Label = label
	}
	if apiKey != nil && strings.TrimSpace(*apiKey) != "" {
		key := strings.TrimSpace(*apiKey)
		if len(key) < 8 || strings.ContainsAny(key, "\r\n") {
			return managementError(http.StatusBadRequest, "invalid api_key")
		}
		if c.APIKey != key {
			// A quota block is bound to the inference key, not its label or ID.
			// A failed host save leaves the old key and its block untouched.
			c.QuotaBlock = nil
			c.QuotaRevision = newID()
		}
		c.APIKey = key
	}
	if err := applyBillingInput(&c, in); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	if disabled != nil {
		c.Disabled = *disabled
		if c.quotaBlocked() {
			// Never mutate the live pointer before host.auth.save succeeds.
			block := cloneQuotaBlock(c.QuotaBlock)
			manual := *disabled
			block.ManualDisabled = &manual
			c.QuotaBlock = block
			c.QuotaRevision = newID()
		}
	}
	if c.Label == "" {
		c.Label = "OpenDesign"
	}
	if c.BaseURL == "" {
		c.BaseURL = s.config().BaseURL
	}
	// Every credential writer runs under credentialMu, and the rule sync runs
	// under the same lock, so reading the live policy here guarantees a late
	// credential save can never write a previous policy back into an auth file.
	c.RequestScopedErrors = s.currentErrorRules()
	filename := s.fileNameFor(c)
	if filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) {
		return managementError(http.StatusBadRequest, "invalid credential filename")
	}
	if err := s.saveCredential(c, filename); err != nil {
		return managementError(http.StatusInternalServerError, "credential persistence failed")
	}
	s.mu.Lock()
	s.storeCredentialLocked(c)
	c = s.creds[c.ID]
	if in.ClearBilling {
		s.invalidateBillingLocked(c.ID)
	}
	s.authFiles[c.ID] = filename
	syncError := s.quotaPauseErrors[c.ID]
	recheckStatus := s.quotaRecheckStatus[c.ID]
	s.mu.Unlock()
	return managementJSON(http.StatusOK, credentialView(c, s.quotaPauseTime(), syncError, recheckStatus))
}

// deleteCredential removes the auth file and revokes the id so a queued watcher
// callback cannot resurrect it.
//
// Locking is the delicate part. host.auth.save synchronously re-enters auth.parse
// on this goroutine (CPA's saveAuthFile calls buildAuthsFromFileData before it
// writes the file), and auth.parse takes authFileMu. Holding authFileMu across
// the save therefore deadlocks the plugin. credentialMu alone serializes deletes
// and updates; authFileMu is taken only for the unlink phase, which never calls
// back into the host.
func (s *Service) deleteCredential(credentialID string) (any, error) {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()

	s.mu.RLock()
	c, ok := s.creds[credentialID]
	dir := s.authDir
	s.mu.RUnlock()
	if !ok {
		return managementError(http.StatusNotFound, "credential not found")
	}
	if dir == "" {
		return managementError(http.StatusConflict, "credential storage directory not resolved")
	}
	if filepath.Base(c.ID) != c.ID || strings.ContainsAny(c.ID, `/\`) {
		return managementError(http.StatusBadRequest, "invalid credential ID")
	}
	filename := s.fileNameFor(c)
	if filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) {
		return managementError(http.StatusBadRequest, "invalid credential filename")
	}
	path := filepath.Join(dir, filename)

	// Step 1 (no authFileMu): make the host withdraw the credential's models.
	// CPA 0.48 has no host.auth.delete (only list/get/get_runtime/save/models),
	// but a disabled auth re-runs model.for_auth and unregisters its catalog via
	// registerModelsForAuthWithConfigAndCache -> unregisterModelsForAuthFenced.
	disabled := c
	disabled.Disabled = true
	if err := s.saveCredential(disabled, filename); err != nil {
		// Nothing changed: the credential stays enabled and usable.
		return managementError(http.StatusBadGateway, "credential could not be disabled before removal: "+redact(err.Error(), s.allKeys()...))
	}
	s.mu.Lock()
	s.storeCredentialLocked(disabled)
	s.mu.Unlock()

	// Step 2 (authFileMu held, no host calls): verify ownership, then unlink.
	// The host record is already disabled, so a failure here leaves a state that
	// matches what the host reports: present but disabled, never "still enabled".
	if status, message := s.unlinkCredentialFile(path, c); status != 0 {
		if status == http.StatusInternalServerError {
			return managementError(status, message+" (the credential is now disabled and its models are withdrawn; retry the delete)")
		}
		return managementError(status, message)
	}

	s.mu.Lock()
	delete(s.creds, credentialID)
	s.invalidateBillingLocked(credentialID)
	delete(s.authFiles, credentialID)
	delete(s.quotaPauseErrors, credentialID)
	delete(s.quotaDiskStale, credentialID)
	delete(s.quotaRecheckStatus, credentialID)
	delete(s.quotaRecheckRetryAt, credentialID)
	delete(s.quotaRecheckFailures, credentialID)
	// The id stays revoked so a watcher callback queued before the unlink cannot
	// recreate the credential.
	s.revoked[credentialID] = true
	s.mu.Unlock()
	return managementJSON(http.StatusOK, map[string]any{"deleted": true})
}

// unlinkCredentialFile validates ownership and removes the auth file. A non-zero
// status reports a failure; no host callback is made while it runs.
func (s *Service) unlinkCredentialFile(path string, c Credential) (int, string) {
	s.authFileMu.Lock()
	defer s.authFileMu.Unlock()
	b, err := os.ReadFile(path)
	if err != nil {
		return http.StatusConflict, "credential file missing"
	}
	var disk Credential
	if json.Unmarshal(b, &disk) != nil || disk.Type != Provider || disk.ID != c.ID {
		return http.StatusConflict, "credential file ownership mismatch"
	}
	if err := os.Remove(path); err != nil {
		return http.StatusInternalServerError, "credential removal failed: " + err.Error()
	}
	return 0, ""
}

// upstreamModels proxies GET /models for the given credential.
func (s *Service) upstreamModels(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	defer s.done()
	c, err := s.credentialByID(r.Query.Get("credential_id"))
	if err != nil {
		return managementError(statusOf(err), err.Error(), s.allKeys()...)
	}
	models, err := s.discoverModels(r.HostCallbackID, c)
	if err != nil {
		keys := append(s.allKeys(), c.APIKey, c.ControlKey)
		code := "upstream_error"
		var apiErr *APIError
		if errors.As(err, &apiErr) && safeManagementCode(apiErr.Kind, keys) {
			code = apiErr.Kind
		}
		return managementJSON(statusOf(err), map[string]any{
			"error": redact(err.Error(), keys...), "source": "upstream", "code": code,
		})
	}
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		entry := map[string]any{"id": m.ID}
		if m.OwnedBy != "" {
			entry["owned_by"] = m.OwnedBy
		}
		if m.Object != "" {
			entry["object"] = m.Object
		}
		data = append(data, entry)
	}
	return managementJSON(http.StatusOK, map[string]any{"data": data})
}

// allKeys collects the live key values so redaction can strip them from any
// upstream message before it is shown to the operator.
func (s *Service) allKeys() []string {
	return s.collectMetadataSecrets(false)
}

// metadataSecrets also removes workspace identifiers from diagnostic metadata.
// Unlike credential/quota views, logs and runtime never need that identifier.
func (s *Service) metadataSecrets() []string {
	return s.collectMetadataSecrets(true)
}

func (s *Service) collectMetadataSecrets(includeWorkspace bool) []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	keys := make([]string, 0, 3*len(s.creds))
	for _, c := range s.creds {
		keys = append(keys, c.APIKey, c.ControlKey)
		if includeWorkspace {
			keys = append(keys, c.WorkspaceID)
		}
	}
	s.mu.RUnlock()
	// Never nest pendingMu with mu: auth.parse re-enters while a save is pending.
	s.pendingMu.Lock()
	for _, pending := range s.pendingSaves {
		var c Credential
		if json.Unmarshal(pending.body, &c) == nil {
			keys = append(keys, c.APIKey, c.ControlKey)
			if includeWorkspace {
				keys = append(keys, c.WorkspaceID)
			}
		}
	}
	s.pendingMu.Unlock()
	return keys
}

// safeManagementCode bounds the upstream-controlled error kind before exposing
// it separately from the already-redacted message.
func safeManagementCode(code string, keys []string) bool {
	if code == "" || len(code) > 80 || redact(code, keys...) != code {
		return false
	}
	for _, c := range code {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func safeDiagnosticKind(kind string, keys []string) bool {
	if !safeManagementCode(kind, keys) {
		return false
	}
	if isKnownUpstreamCode(kind) {
		return true
	}
	switch kind {
	case "upstream_error", "upstream_protocol_error", "upstream_transport_error", "response_limit_exceeded",
		"invalid_request", "invalid_json", "model_not_enabled", "credential_unavailable", "credential_changed",
		"plugin_unavailable", "host_contract_error", "request_canceled", "count_tokens_unavailable":
		return true
	default:
		return false
	}
}

func statusOf(err error) int {
	var coded interface{ StatusCode() int }
	if errors.As(err, &coded) && coded.StatusCode() >= 400 && coded.StatusCode() <= 599 {
		return coded.StatusCode()
	}
	return http.StatusInternalServerError
}
