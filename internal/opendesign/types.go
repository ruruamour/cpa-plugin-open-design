package opendesign

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const (
	// Version is the plugin version advertised at registration.
	Version = "0.1.0"
	// PluginID is the CPA plugin identifier and management path segment.
	PluginID = "open-design"
	// Provider is the CPA provider key for this plugin.
	Provider = "open-design"
	// DefaultBaseURL is the only upstream this plugin talks to.
	DefaultBaseURL = "https://amr-link.open-design.ai/v1"
	// Author and Repository keep attribution explicit for the fork.
	Author     = "ruruamour"
	Repository = "https://github.com/ruruamour/cpa-plugin-open-design"

	settingsFile = "settings.json"
)

// apiBase is the management route prefix. Every route below it requires CPA
// management authentication.
const apiBase = "/v0/management/" + PluginID

// consoleResource is the browser-navigable console entry the host exposes
// without management auth; the page performs the auth handshake itself.
const consoleResource = "/v0/resource/plugins/" + PluginID + "/console"

// consolePath is the authenticated management path serving the same page.
const consolePath = apiBase + "/console.html"

// HostCall is the host callback signature injected by cmd/passbridge.
type HostCall func(string, any, any) error

// APIError carries an HTTP status and machine-readable kind to the host.
type APIError struct {
	Status  int    `json:"-"`
	Kind    string `json:"-"`
	Message string `json:"-"`
}

func (e *APIError) Error() string { return e.Message }

// StatusCode lets the host map plugin errors onto client HTTP statuses.
func (e *APIError) StatusCode() int { return e.Status }

// Code exposes the error kind in the RPC envelope.
func (e *APIError) Code() string { return e.Kind }

func fail(status int, message string) error {
	return &APIError{Status: status, Kind: "upstream_error", Message: message}
}

// Model maps a client-facing alias onto an OpenDesign upstream model ID.
type Model struct {
	ID         string `json:"id"`
	UpstreamID string `json:"upstream_id"`
}

// Config is the plugin-owned configuration. There is no field for credentials:
// keys live only in the CPA auth directory.
type Config struct {
	DataDir  string  `json:"data_dir" yaml:"data_dir"`
	BaseURL  string  `json:"base_url" yaml:"base_url"`
	Models   []Model `json:"models" yaml:"models"`
	LogLevel string  `json:"log_level,omitempty" yaml:"log_level,omitempty"`
}

func defaultConfig() Config {
	return Config{DataDir: "plugins/" + PluginID + "-data", BaseURL: DefaultBaseURL}
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.DataDir) == "" {
		return fail(http.StatusBadRequest, "data_dir is required")
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		c.BaseURL = DefaultBaseURL
	}
	if err := validateBaseURL(c.BaseURL); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i := range c.Models {
		m := &c.Models[i]
		m.ID = strings.TrimSpace(m.ID)
		m.UpstreamID = strings.TrimSpace(m.UpstreamID)
		if m.ID == "" || m.UpstreamID == "" {
			return fail(http.StatusBadRequest, "model id and upstream_id must be nonempty")
		}
		if seen[m.ID] {
			return fail(http.StatusBadRequest, "model aliases must be unique: "+m.ID)
		}
		seen[m.ID] = true
	}
	return nil
}

// validateBaseURL pins the upstream to the OpenDesign origin. A redirected or
// reconfigured host would leak the bearer key, so the value is not user-editable
// beyond the documented default.
func validateBaseURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host != "amr-link.open-design.ai" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.TrimRight(u.Path, "/") != "/v1" {
		return fail(http.StatusBadRequest, "base_url must be "+DefaultBaseURL)
	}
	return nil
}

// Credential is the auth record persisted through host.auth.save. Secret keys
// are never echoed by management routes.
type Credential struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	Label       string `json:"label"`
	APIKey      string `json:"api_key"`
	ControlKey  string `json:"control_key,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	Disabled    bool   `json:"disabled"`
	BaseURL     string `json:"base_url,omitempty"`
	// ModelRevision changes whenever the model mapping changes. CPA rebuilds a
	// credential's model list only when the auth file content changes, so a
	// mapping update without a revision bump would never reach the host.
	ModelRevision int64 `json:"model_revision,omitempty"`
	extraFields   map[string]json.RawMessage
}

var credentialJSONFields = []string{"type", "id", "label", "api_key", "control_key", "workspace_id", "disabled", "base_url", "model_revision"}

// UnmarshalJSON keeps host- or operator-added fields so a rewrite through
// host.auth.save cannot silently drop them.
func (c *Credential) UnmarshalJSON(raw []byte) error {
	type known Credential
	var decoded known
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key := range fields {
		for _, known := range credentialJSONFields {
			if strings.EqualFold(key, known) {
				delete(fields, key)
				break
			}
		}
	}
	*c = Credential(decoded)
	if len(fields) > 0 {
		c.extraFields = fields
	}
	return nil
}

// MarshalJSON re-emits the preserved fields next to the known ones.
func (c Credential) MarshalJSON() ([]byte, error) {
	type known Credential
	b, err := json.Marshal(known(c))
	if err != nil || len(c.extraFields) == 0 {
		return b, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(b, &merged); err != nil {
		return nil, err
	}
	for key, value := range c.extraFields {
		if _, exists := merged[key]; !exists {
			merged[key] = value
		}
	}
	return json.Marshal(merged)
}

var secretPattern = regexp.MustCompile(`(?i)(?:bearer\s+|sk[-_])[a-z0-9_.\-]+`)

// redact strips bearer/sk- shaped tokens plus any known key value from text
// before it can reach a client, a log line or an error envelope.
func redact(value string, keys ...string) string {
	for _, key := range keys {
		if key == "" {
			continue
		}
		value = strings.ReplaceAll(value, key, "[redacted]")
	}
	value = secretPattern.ReplaceAllString(value, "[redacted]")
	// Bound the message without splitting a multi-byte rune.
	if runes := []rune(value); len(runes) > 500 {
		value = string(runes[:500])
	}
	return value
}
