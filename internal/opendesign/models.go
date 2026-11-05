package opendesign

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// modelView is the model shape the console reads. It carries the stored
// thinking-strength policy so the console can render it without a second route,
// plus a stable reasoning_revision token the console echoes back when saving so
// a stale tab is rejected instead of clobbering a newer edit. A model with no
// policy omits the mapping field, which is what tells the console to pre-fill the
// vendor table instead of showing an empty one.
func modelView(m Model) map[string]any {
	view := map[string]any{"id": m.ID, "upstream_id": m.UpstreamID, "reasoning_revision": reasoningRevision(m)}
	if m.Reasoning != nil {
		view["reasoning_mapping"] = m.Reasoning
	}
	return view
}

func modelViews(models []Model) []map[string]any {
	views := make([]map[string]any, 0, len(models))
	for _, m := range models {
		views = append(views, modelView(m))
	}
	return views
}

func sameModels(left, right []Model) bool {
	// Models carry an optional policy object, so a field-by-field comparison
	// would compare pointers and report a change for two identical mappings.
	// The canonical encoding is the same bytes a restart would read back.
	return bytes.Equal(jsonBytes(left), jsonBytes(right))
}

// modelRegistration advertises the mapped models to CPA. It is dynamic on
// purpose: with no credential the list is empty so CPA cannot route a request
// that can never succeed.
func (s *Service) modelRegistration() any {
	cfg := s.config()
	// Without a live credential nothing can serve these models; advertising them
	// would keep them in /v1/models after the last credential is deleted.
	s.mu.RLock()
	hasCredential := len(s.creds) > 0
	s.mu.RUnlock()
	if !hasCredential {
		return map[string]any{"Provider": Provider, "Models": []map[string]any{}}
	}
	models := make([]map[string]any, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		models = append(models, map[string]any{
			"ID":          m.ID,
			"Object":      "model",
			"OwnedBy":     Provider,
			"Name":        m.UpstreamID,
			"DisplayName": m.ID,
			// These aliases and upstream IDs come from the operator's model
			// mapping, not a verified OpenDesign capability catalogue. Marking
			// them user-defined lets CPA forward reasoning parameters for the
			// plugin/upstream to handle without asserting support for any level.
			"UserDefined": true,
		})
	}
	return map[string]any{"Provider": Provider, "Models": models}
}

// saveReasoningMapping replaces one model's thinking-strength policy.
//
// The whole cycle runs under configMu so two concurrent saves cannot interleave
// persist and publish, and a stale tab cannot overwrite a newer edit. The
// mapping only changes what this plugin sends upstream, so unlike a model-list
// change it does not rewrite any auth file: the host's model catalog is
// unaffected.
//
// expected_revision is an optional optimistic-lock token: when supplied it must
// equal the current reasoning revision, so a tab that opened before another save
// is rejected with 409 instead of silently clobbering it. Omitting it keeps the
// old last-writer-wins behavior for existing callers.
func (s *Service) saveReasoningMapping(raw json.RawMessage) (any, error) {
	var in struct {
		Model            string            `json:"model"`
		UpstreamID       string            `json:"upstream_id"`
		Mapping          *ReasoningMapping `json:"reasoning_mapping"`
		ExpectedRevision *string           `json:"expected_revision"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || in.Mapping == nil {
		return nil, localFail(http.StatusBadRequest, "invalid_request", "invalid reasoning mapping JSON")
	}
	model := strings.TrimSpace(in.Model)
	upstream := strings.TrimSpace(in.UpstreamID)
	if model == "" || upstream == "" {
		return nil, localFail(http.StatusBadRequest, "invalid_request", "model and upstream_id are required")
	}
	if err := validateReasoningMapping(in.Mapping); err != nil {
		return nil, err
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	cfg := s.config()
	index := -1
	for i := range cfg.Models {
		if cfg.Models[i].ID == model {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, fail(http.StatusNotFound, "model not found")
	}
	// A mapping written against one upstream ID must not silently follow the
	// alias onto a different model, so the console is told to refresh instead.
	if cfg.Models[index].UpstreamID != upstream {
		return nil, fail(http.StatusConflict, "model upstream changed; refresh before saving")
	}
	// The revision check and the mutation run under the same configMu, so a
	// concurrent save cannot slip between the compare and the publish.
	currentRevision := reasoningRevision(cfg.Models[index])
	if in.ExpectedRevision != nil && *in.ExpectedRevision != "" && *in.ExpectedRevision != currentRevision {
		return nil, localFail(http.StatusConflict, "reasoning_revision_conflict", "reasoning mapping changed since this page was loaded; refresh before saving")
	}
	cfg.Models[index].Reasoning = cloneReasoningMapping(in.Mapping)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := s.saveSettings(cfg); err != nil {
		return nil, fail(http.StatusInternalServerError, "could not persist reasoning mapping")
	}
	s.mu.Lock()
	s.cfg.Models = cfg.Models
	// Results produced under the previous policy no longer describe what this
	// model does, so they are dropped rather than shown as current.
	s.modelMappingRevision++
	s.clearModelTestsLocked()
	s.mu.Unlock()
	return map[string]any{"models": modelViews(cfg.Models), "reasoning_revision": reasoningRevision(cfg.Models[index])}, nil
}

// resolveModel maps a client alias onto the upstream model ID. An unknown alias is
// rejected locally so the request never reaches the network. The lookup shares
// modelSnapshotFor so it always observes one published configuration.
func (s *Service) resolveModel(model string) (string, error) {
	snapshot, ok := s.modelSnapshotFor(model)
	if !ok {
		return "", &APIError{Status: http.StatusBadRequest, Kind: "model_not_enabled", Message: "model is not enabled in the OpenDesign plugin"}
	}
	return snapshot.upstream, nil
}

// discoverModels calls GET /models with the supplied credential. The key comes
// from the stored credential, never from the browser.
func (s *Service) discoverModels(callbackID string, c Credential) ([]UpstreamModel, error) {
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = s.config().BaseURL
	}
	if err := validateBaseURL(baseURL); err != nil {
		return nil, err
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	status, _, body, err := s.doJSON(callbackID, http.MethodGet, upstreamURL(baseURL, "/models", nil), upstreamHeaders(c.APIKey), nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, upstreamErrorFromResponse(status, nil, body, c.APIKey, c.ControlKey)
	}
	return decodeModels(body)
}

// credentialByID returns a live credential or a 404 the UI can display.
func (s *Service) credentialByID(id string) (Credential, error) {
	id = strings.TrimSpace(id)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id == "" {
		// Single-credential installs may omit the parameter.
		if len(s.creds) == 1 {
			for _, c := range s.creds {
				return c, nil
			}
		}
		return Credential{}, fail(http.StatusBadRequest, "credential_id is required")
	}
	c, ok := s.creds[id]
	if !ok {
		return Credential{}, fail(http.StatusNotFound, "credential not found")
	}
	if c.APIKey == "" {
		return Credential{}, fail(http.StatusUnauthorized, "credential has no api key")
	}
	return c, nil
}

// importModels validates and persists the submitted alias→upstream mapping.
//
// The whole cycle runs under configMu so two concurrent imports cannot interleave
// persist and publish (disk would then hold B while memory held A):
//
//	snapshot -> validate -> persist -> publish
//
// importModels also has to make CPA re-run model.for_auth, otherwise a console
// flow of "add the key first, import models second" leaves the host with the
// empty catalog it registered when the credential appeared, and every request
// fails with model_not_found. CPA only rebuilds a credential's catalog when the
// auth file content changes, so the mapping revision is written into every
// credential through host.auth.save.
func (s *Service) importModels(raw json.RawMessage) (any, error) {
	var in struct {
		Models []Model `json:"models"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fail(http.StatusBadRequest, "invalid models JSON")
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()

	cfg := s.config()
	previous := cfg.Models
	cfg.Models = append([]Model(nil), in.Models...)
	// Canonicalize the incoming alias/upstream IDs before matching, so a
	// whitespace-only edit cannot make the keep-existing-policy lookup miss and
	// silently drop a saved rule table. validate() would trim them later, but by
	// then the policy had already been decided.
	for i := range cfg.Models {
		cfg.Models[i].ID = strings.TrimSpace(cfg.Models[i].ID)
		cfg.Models[i].UpstreamID = strings.TrimSpace(cfg.Models[i].UpstreamID)
	}
	// An incoming list that omits the policy keeps the stored one. The upstream ID
	// has to match as well: a rule written for one upstream must not be silently
	// applied to another model that happens to reuse the alias. A policy sent in
	// the body always wins.
	for i := range cfg.Models {
		if cfg.Models[i].Reasoning != nil {
			continue
		}
		for _, existing := range previous {
			if strings.TrimSpace(existing.ID) == cfg.Models[i].ID && strings.TrimSpace(existing.UpstreamID) == cfg.Models[i].UpstreamID {
				cfg.Models[i].Reasoning = cloneReasoningMapping(existing.Reasoning)
				break
			}
		}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := s.saveSettings(cfg); err != nil {
		return nil, fail(http.StatusInternalServerError, "could not persist model mapping")
	}
	s.mu.Lock()
	mappingChanged := !sameModels(s.cfg.Models, cfg.Models)
	s.cfg.Models = cfg.Models
	if mappingChanged {
		s.modelMappingRevision++
		s.clearModelTestsLocked()
	}
	s.mu.Unlock()

	// Publishing the mapping is enough for this plugin's own executor; the extra
	// step below is what makes the host re-register the catalog. A failure here
	// is reported but does not roll back the persisted mapping.
	if err := s.refreshAuthModels(); err != nil {
		return nil, localFail(http.StatusBadGateway, "host_contract_error", "model mapping saved but the host model refresh failed: host rejected the refresh")
	}
	return map[string]any{"models": modelViews(cfg.Models)}, nil
}

// refreshAuthModels rewrites every live credential with a new model revision so
// CPA treats the auth file as changed and re-runs model.for_auth for it.
func (s *Service) refreshAuthModels() error {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	s.mu.RLock()
	creds := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		creds = append(creds, c)
	}
	s.mu.RUnlock()
	if len(creds) == 0 {
		return nil
	}
	revision := nextModelRevision()
	var firstErr error
	for _, c := range creds {
		filename := s.fileNameFor(c)
		if filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) {
			continue
		}
		c.ModelRevision = revision
		if err := s.saveCredential(c, filename); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s.mu.Lock()
		s.storeCredentialLocked(c)
		s.authFiles[c.ID] = filename
		s.mu.Unlock()
	}
	return firstErr
}

var modelRevisionSeq atomic.Int64

// nextModelRevision returns a strictly increasing revision stamp. A wall clock
// value keeps the file content meaningful when inspected by an operator, and the
// atomic counter guarantees uniqueness for writes inside the same nanosecond.
func nextModelRevision() int64 {
	for {
		now := time.Now().UnixNano()
		last := modelRevisionSeq.Load()
		next := now
		if next <= last {
			next = last + 1
		}
		if modelRevisionSeq.CompareAndSwap(last, next) {
			return next
		}
	}
}
