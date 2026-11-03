package opendesign

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	maxModelTestBodyBytes   = 8 << 10
	maxModelTestMessages    = 4
	maxModelTestTextRunes   = 1024
	maxModelTestPromptRunes = 512
	modelTestOutputTokens   = 16
	modelTestDefaultPrompt  = "Reply with OK."
	maxConcurrentModelTests = 3
)

type modelTestInput struct {
	CredentialID string          `json:"credential_id"`
	ModelID      string          `json:"model_id"`
	Prompt       string          `json:"prompt"`
	Messages     json.RawMessage `json:"messages"`
}

type modelTestUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
}

type modelTestError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type modelTestResponse struct {
	CredentialID      string          `json:"credential_id"`
	ModelID           string          `json:"model_id"`
	UpstreamModel     string          `json:"upstream_model"`
	Status            string          `json:"status"`
	HTTPStatus        *int            `json:"http_status"`
	DurationMS        int64           `json:"duration_ms"`
	Usage             *modelTestUsage `json:"usage"`
	Error             *modelTestError `json:"error,omitempty"`
	RealInference     bool            `json:"real_inference"`
	UpstreamAttempted bool            `json:"upstream_attempted"`
	Streaming         bool            `json:"streaming"`
	RequestKind       string          `json:"request_kind"`
}

type modelTestRecord struct {
	ModelID       string    `json:"model_id"`
	UpstreamModel string    `json:"upstream_model"`
	Status        string    `json:"status"`
	HTTPStatus    *int      `json:"http_status"`
	DurationMS    int64     `json:"duration_ms"`
	TestedAt      time.Time `json:"tested_at"`
	ErrorKind     string    `json:"error_kind,omitempty"`
	ErrorMessage  string    `json:"error_message,omitempty"`
}

type modelTestIdentity struct {
	credentialID    string
	modelID         string
	apiKey          string
	controlKey      string
	workspaceID     string
	baseURL         string
	disabled        bool
	credentialEpoch uint64
	upstreamModel   string
	mappingRevision uint64
}

type modelTestSnapshot struct {
	credential      Credential
	alias           string
	upstreamModel   string
	mappingRevision uint64
	credentialEpoch uint64
}

// modelTestRequest is deliberately explicit. It accepts only an enabled alias,
// constructs a tiny chat payload itself, and calls the regular buffered executor
// once. The regular executor records one metadata row and never exposes its body.
func (s *Service) modelTestRequest(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	defer s.done()
	if len(r.Body) > maxModelTestBodyBytes {
		return managementError(http.StatusRequestEntityTooLarge, "model-test request is too large")
	}
	var in modelTestInput
	if err := jsonUnmarshalObject(r.Body, &in); err != nil {
		return managementError(http.StatusBadRequest, "invalid model-test JSON")
	}
	credentialID := strings.TrimSpace(in.CredentialID)
	if credentialID == "" {
		return managementError(http.StatusBadRequest, "credential_id is required")
	}
	alias := strings.TrimSpace(in.ModelID)
	if alias == "" {
		return managementError(http.StatusBadRequest, "model_id is required")
	}
	if len(alias) > 512 || strings.IndexFunc(alias, unicode.IsControl) >= 0 {
		return managementError(http.StatusBadRequest, "invalid model_id")
	}
	snapshot, err := s.modelTestSnapshot(credentialID, alias)
	if err != nil {
		return managementError(statusOf(err), err.Error(), s.allKeys()...)
	}
	c := snapshot.credential
	messages, err := modelTestMessages(in)
	if err != nil {
		return managementError(statusOf(err), err.Error())
	}
	identity := modelTestIdentity{
		credentialID: c.ID, modelID: alias, apiKey: c.APIKey, controlKey: c.ControlKey,
		workspaceID: c.WorkspaceID, baseURL: c.BaseURL, disabled: c.Disabled,
		upstreamModel: snapshot.upstreamModel, mappingRevision: snapshot.mappingRevision,
		credentialEpoch: snapshot.credentialEpoch,
	}

	payload := map[string]any{
		"messages":   messages,
		"max_tokens": modelTestOutputTokens,
	}
	raw := jsonBytes(ExecutorRequest{
		AuthID:         c.ID,
		AuthProvider:   Provider,
		Model:          alias,
		Payload:        jsonBytes(payload),
		StorageJSON:    jsonBytes(c),
		HostCallbackID: r.HostCallbackID,
	})
	key := c.ID + "\x00" + alias
	if !s.modelTestStarted(key) {
		return managementError(http.StatusTooManyRequests, "model-test concurrency limit reached")
	}
	started := time.Now()
	record := modelTestRecord{
		ModelID:       alias,
		UpstreamModel: snapshot.upstreamModel,
		Status:        "error",
		DurationMS:    0,
		TestedAt:      time.Now().UTC(),
	}
	finish := sync.OnceFunc(func() { s.modelTestFinished(key, record, identity) })
	defer finish()
	result, callErr := s.executePinned(raw, snapshot.upstreamModel, &c, false)
	duration := time.Since(started).Milliseconds()
	keys := append(s.metadataSecrets(), c.APIKey, c.ControlKey, c.WorkspaceID)
	response := modelTestResponse{
		CredentialID:      requestLogText(c.ID, 128, keys...),
		ModelID:           requestLogText(alias, 512, keys...),
		UpstreamModel:     requestLogText(snapshot.upstreamModel, 512, keys...),
		Status:            "success",
		DurationMS:        duration,
		RealInference:     true,
		UpstreamAttempted: true,
		Streaming:         false,
		RequestKind:       "chat.completions",
	}
	record.Status = "success"
	record.DurationMS = duration
	record.TestedAt = time.Now().UTC()
	if callErr != nil {
		kind := modelTestErrorKind(callErr, keys)
		message := modelTestErrorMessage(kind)
		response.Status = "error"
		response.RealInference = modelTestAttempted(callErr)
		response.UpstreamAttempted = modelTestAttempted(callErr)
		response.Error = &modelTestError{Kind: kind, Message: message}
		record.Status = "error"
		record.ErrorKind = kind
		record.ErrorMessage = message
		if status := modelTestUpstreamStatus(callErr); status != 0 {
			response.HTTPStatus = &status
			record.HTTPStatus = &status
		}
	} else {
		resultMap, resultOK := result.(map[string]any)
		if resultOK {
			if metadata, metadataOK := resultMap["Metadata"].(map[string]any); metadataOK {
				if status, statusOK := metadata["http_status"].(int); statusOK && status >= 100 && status <= 599 {
					response.HTTPStatus = &status
					record.HTTPStatus = &status
				}
			}
			if payload, payloadOK := resultMap["Payload"].([]byte); payloadOK {
				if root, decodeErr := decodeObject(payload); decodeErr == nil {
					response.Usage = modelTestUsageFrom(root)
				}
			}
		}
	}
	finish()
	return managementJSON(http.StatusOK, response)
}

func (s *Service) modelTestSnapshot(credentialID, alias string) (modelTestSnapshot, error) {
	s.mu.RLock()
	c, credentialOK := s.creds[credentialID]
	revoked := s.revoked[credentialID]
	upstream := ""
	for _, model := range s.cfg.Models {
		if model.ID == alias {
			upstream = model.UpstreamID
			break
		}
	}
	revision := s.modelMappingRevision
	epoch := s.credentialEpoch[credentialID]
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = s.cfg.BaseURL
	}
	s.mu.RUnlock()
	if !credentialOK {
		return modelTestSnapshot{}, fail(http.StatusNotFound, "credential not found")
	}
	if revoked || c.APIKey == "" {
		return modelTestSnapshot{}, fail(http.StatusUnauthorized, "credential has no usable api key")
	}
	if c.Disabled {
		return modelTestSnapshot{}, fail(http.StatusConflict, "credential is disabled; enable it before testing")
	}
	if upstream == "" {
		return modelTestSnapshot{}, localFail(http.StatusBadRequest, "model_not_enabled", "model alias is not enabled in the OpenDesign plugin")
	}
	c.BaseURL = baseURL
	if err := validateBaseURL(c.BaseURL); err != nil {
		return modelTestSnapshot{}, err
	}
	return modelTestSnapshot{credential: c, alias: alias, upstreamModel: upstream, mappingRevision: revision, credentialEpoch: epoch}, nil
}

func (s *Service) modelTestStarted(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	running := 0
	for _, count := range s.modelTestRunning {
		running += count
	}
	if running >= maxConcurrentModelTests {
		return false
	}
	s.modelTestRunning[key]++
	return true
}

func (s *Service) modelTestIdentity(c Credential, alias, upstream string) modelTestIdentity {
	s.mu.RLock()
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = s.cfg.BaseURL
	}
	identity := modelTestIdentity{
		credentialID:    c.ID,
		modelID:         alias,
		apiKey:          c.APIKey,
		controlKey:      c.ControlKey,
		workspaceID:     c.WorkspaceID,
		baseURL:         baseURL,
		disabled:        c.Disabled,
		upstreamModel:   upstream,
		mappingRevision: s.modelMappingRevision,
		credentialEpoch: s.credentialEpoch[c.ID],
	}
	s.mu.RUnlock()
	return identity
}

func (s *Service) modelTestFinished(key string, record modelTestRecord, identity modelTestIdentity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if count := s.modelTestRunning[key]; count > 1 {
		s.modelTestRunning[key] = count - 1
	} else {
		delete(s.modelTestRunning, key)
	}
	current, valid := s.creds[identity.credentialID]
	valid = valid && !s.stopped && !s.revoked[identity.credentialID]
	if valid {
		baseURL := current.BaseURL
		if baseURL == "" {
			baseURL = s.cfg.BaseURL
		}
		valid = current.APIKey == identity.apiKey && current.ControlKey == identity.controlKey &&
			current.WorkspaceID == identity.workspaceID && baseURL == identity.baseURL &&
			current.Disabled == identity.disabled &&
			s.modelMappingRevision == identity.mappingRevision &&
			s.credentialEpoch[identity.credentialID] == identity.credentialEpoch
	}
	if valid {
		modelFound := false
		for _, model := range s.cfg.Models {
			if model.ID == identity.modelID {
				modelFound = true
				valid = model.UpstreamID == identity.upstreamModel
				break
			}
		}
		valid = valid && modelFound
	}
	if !valid {
		return
	}
	if previous, exists := s.modelTests[key]; !exists || !previous.TestedAt.After(record.TestedAt) {
		s.modelTests[key] = record
	}
}

func (s *Service) clearModelTestsLocked(credentialID ...string) {
	if len(credentialID) == 0 {
		s.modelTests = map[string]modelTestRecord{}
		return
	}
	prefix := credentialID[0] + "\x00"
	for key := range s.modelTests {
		if strings.HasPrefix(key, prefix) {
			delete(s.modelTests, key)
		}
	}
}

func modelTestMessages(in modelTestInput) ([]map[string]any, error) {
	raw := bytes.TrimSpace(in.Messages)
	if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fail(http.StatusBadRequest, "messages must be an array of {role, content}")
		}
		if len(items) == 0 || len(items) > maxModelTestMessages {
			return nil, fail(http.StatusBadRequest, "messages count is outside the model-test limit")
		}
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			for key := range item {
				if key != "role" && key != "content" {
					return nil, fail(http.StatusBadRequest, "messages entries may only contain role and content")
				}
			}
			var role, content string
			if json.Unmarshal(item["role"], &role) != nil || json.Unmarshal(item["content"], &content) != nil {
				return nil, fail(http.StatusBadRequest, "messages entries need string role and content")
			}
			role = strings.TrimSpace(role)
			if role != "system" && role != "user" && role != "assistant" {
				return nil, fail(http.StatusBadRequest, "messages role must be system, user or assistant")
			}
			if err := validateModelTestText(content, maxModelTestTextRunes); err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"role": role, "content": content})
		}
		return out, nil
	}
	if err := validateModelTestText(in.Prompt, maxModelTestTextRunes); err != nil {
		return nil, err
	}
	if len([]rune(in.Prompt)) > maxModelTestPromptRunes {
		return nil, fail(http.StatusBadRequest, "prompt exceeds the model-test limit")
	}
	prompt := in.Prompt
	if strings.TrimSpace(prompt) == "" {
		prompt = modelTestDefaultPrompt
	}
	return []map[string]any{{"role": "user", "content": prompt}}, nil
}

func validateModelTestText(text string, limit int) error {
	if len([]rune(text)) > limit {
		return fail(http.StatusBadRequest, "model-test text exceeds the limit")
	}
	for _, r := range text {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return fail(http.StatusBadRequest, "model-test text contains a control character")
		}
	}
	return nil
}

func modelTestUsageFrom(root map[string]any) *modelTestUsage {
	usage, ok := root["usage"].(map[string]any)
	if !ok {
		return nil
	}
	return &modelTestUsage{
		PromptTokens:     modelTestToken(usage["prompt_tokens"]),
		CompletionTokens: modelTestToken(usage["completion_tokens"]),
		TotalTokens:      modelTestToken(usage["total_tokens"]),
	}
}

func modelTestToken(value any) *int64 {
	var number int64
	switch v := value.(type) {
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return nil
		}
		number = parsed
	case float64:
		if v < 0 || v > float64(maxRequestLogTokenValue) || v != math.Trunc(v) {
			return nil
		}
		number = int64(v)
	case int64:
		number = v
	case int:
		number = int64(v)
	default:
		return nil
	}
	if number < 0 || number > maxRequestLogTokenValue {
		return nil
	}
	return &number
}

func modelTestErrorKind(err error, keys []string) string {
	kind := streamErrorCode(err)
	if !safeDiagnosticKind(kind, keys) {
		return "upstream_error"
	}
	return kind
}

func modelTestErrorMessage(kind string) string {
	switch kind {
	case "invalid_request", "invalid_json":
		return "model test request is invalid"
	case "model_not_enabled":
		return "model is not enabled in the OpenDesign plugin"
	case "credential_changed":
		return "OpenDesign credential changed while the model test was starting"
	case "credential_unavailable":
		return "OpenDesign credential is unavailable"
	case "plugin_unavailable":
		return "OpenDesign plugin is shutting down"
	case "host_contract_error":
		return "host could not complete the model test operation"
	case "request_canceled":
		return "model test request was canceled"
	case "response_limit_exceeded":
		return "upstream response exceeds plugin limit"
	default:
		return "upstream request failed"
	}
}

func modelTestAttempted(err error) bool {
	return upstreamErrorWasAttempted(err)
}

func modelTestUpstreamStatus(err error) int {
	var status interface{ UpstreamHTTPStatus() int }
	if errors.As(err, &status) {
		return status.UpstreamHTTPStatus()
	}
	return 0
}
