package opendesign

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

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
		})
	}
	return map[string]any{"Provider": Provider, "Models": models}
}

// resolveModel maps a client alias onto the upstream model ID. An unknown alias is
// rejected locally so the request never reaches the network.
func (s *Service) resolveModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	for _, m := range s.config().Models {
		if m.ID == model {
			return m.UpstreamID, nil
		}
	}
	// An exact upstream ID is also callable, which keeps logs and mirrors usable.
	for _, m := range s.config().Models {
		if m.UpstreamID == model {
			return m.UpstreamID, nil
		}
	}
	return "", &APIError{Status: http.StatusBadRequest, Kind: "model_not_enabled", Message: "model is not enabled in the OpenDesign plugin: " + model}
}

// discoverModels calls GET /models with the supplied credential. The key comes
// from the stored credential, never from the browser.
func (s *Service) discoverModels(callbackID string, c Credential) ([]UpstreamModel, error) {
	cfg := s.config()
	status, _, body, err := s.doJSON(callbackID, http.MethodGet, upstreamURL(cfg.BaseURL, "/models", nil), upstreamHeaders(c.APIKey), nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, upstreamErrorFromResponse(status, nil, body, c.APIKey)
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
	cfg.Models = append([]Model(nil), in.Models...)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := s.saveSettings(cfg); err != nil {
		return nil, fail(http.StatusInternalServerError, "could not persist model mapping")
	}
	s.mu.Lock()
	s.cfg.Models = cfg.Models
	s.mu.Unlock()

	// Publishing the mapping is enough for this plugin's own executor; the extra
	// step below is what makes the host re-register the catalog. A failure here
	// is reported but does not roll back the persisted mapping.
	if err := s.refreshAuthModels(); err != nil {
		return nil, fail(http.StatusBadGateway, "model mapping saved but the host model refresh failed: "+err.Error())
	}
	return map[string]any{"models": cfg.Models}, nil
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
		s.creds[c.ID] = c
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
