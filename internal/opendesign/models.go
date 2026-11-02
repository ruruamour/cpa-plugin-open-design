package opendesign

import (
	"encoding/json"
	"net/http"
	"strings"
)

// modelRegistration advertises the mapped models to CPA. It is dynamic on
// purpose: with no credential the list is empty so CPA cannot route a request
// that can never succeed.
func (s *Service) modelRegistration() any {
	cfg := s.config()
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
func (s *Service) discoverModels(callbackID, credentialID string) ([]UpstreamModel, error) {
	c, err := s.credentialByID(credentialID)
	if err != nil {
		return nil, err
	}
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
func (s *Service) importModels(raw json.RawMessage) (any, error) {
	var in struct {
		Models []Model `json:"models"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fail(http.StatusBadRequest, "invalid models JSON")
	}
	cfg := s.config()
	cfg.Models = in.Models
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := s.saveSettings(cfg); err != nil {
		return nil, fail(http.StatusInternalServerError, "could not persist model mapping")
	}
	s.mu.Lock()
	s.cfg.Models = cfg.Models
	s.mu.Unlock()
	return map[string]any{"models": cfg.Models}, nil
}
