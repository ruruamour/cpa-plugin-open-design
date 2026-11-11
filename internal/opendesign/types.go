package opendesign

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	// Version is the plugin version advertised at registration.
	Version = "0.4.16"
	// PluginID is the CPA plugin identifier and management path segment.
	PluginID = "open-design"
	// Provider is the CPA provider key for this plugin.
	Provider = "open-design"
	// DefaultBaseURL is the fixed inference and model discovery endpoint.
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

	// These flags distinguish a local validation failure from an error after
	// the executor actually attempted the upstream HTTP request. They are
	// intentionally not serialized into any management response.
	upstreamAttempted bool
	upstreamStatus    int
	// These internal fields are set only for the narrowly recognized upstream
	// quota signal. They are never serialized into the client-facing error.
	quotaExhausted bool
	quotaResetAt   time.Time
}

func (e *APIError) Error() string { return e.Message }

// StatusCode lets the host map plugin errors onto client HTTP statuses.
func (e *APIError) StatusCode() int { return e.Status }

// Code exposes the error kind in the RPC envelope.
func (e *APIError) Code() string { return e.Kind }

// UpstreamAttempted reports whether an executor reached the host HTTP path.
// It is used only by the explicit model-test metadata response.
func (e *APIError) UpstreamAttempted() bool { return e != nil && e.upstreamAttempted }

// UpstreamHTTPStatus returns a real status received from the upstream HTTP
// response. A generated 502 for transport/protocol failures is returned as 0.
func (e *APIError) UpstreamHTTPStatus() int {
	if e == nil || e.upstreamStatus < 100 || e.upstreamStatus > 599 {
		return 0
	}
	return e.upstreamStatus
}

func fail(status int, message string) error {
	return &APIError{Status: status, Kind: "upstream_error", Message: message}
}

func localFail(status int, kind, message string) error {
	return &APIError{Status: status, Kind: kind, Message: message}
}

// Model maps a client-facing alias onto an OpenDesign upstream model ID.
type Model struct {
	ID         string `json:"id"`
	UpstreamID string `json:"upstream_id"`
	// Reasoning is the per-model thinking-strength policy. It is optional so a
	// mapping written by an older version keeps working unchanged.
	Reasoning *ReasoningMapping `json:"reasoning_mapping,omitempty"`
}

// ReasoningMapping is one model's thinking-strength policy: whether the mapping
// runs, and which incoming strengths it rewrites. Rules are an ordered list so
// settings.json stays byte-stable across saves; the source is the address, so a
// duplicate source cannot be stored.
type ReasoningMapping struct {
	Enabled bool            `json:"enabled" yaml:"enabled"`
	Rules   []ReasoningRule `json:"rules,omitempty" yaml:"rules,omitempty"`
}

// ReasoningRule rewrites one incoming strength onto the value sent upstream.
type ReasoningRule struct {
	From string `json:"from" yaml:"from"`
	To   string `json:"to" yaml:"to"`
}

// Config is the plugin-owned configuration. There is no field for credentials:
// keys live only in the CPA auth directory. Proxy/routing are deliberately
// absent: the upstream origin is fixed and CPA owns scheduling.
type Config struct {
	DataDir  string    `json:"data_dir" yaml:"data_dir"`
	BaseURL  string    `json:"base_url" yaml:"base_url"`
	Models   []Model   `json:"models" yaml:"models"`
	LogLevel string    `json:"log_level,omitempty" yaml:"log_level,omitempty"`
	Logs     LogConfig `json:"logs,omitempty" yaml:"logs,omitempty"`
	// Pause is the plugin-owned quota-exhaustion policy. A nil Enabled defaults
	// to off while manual-disable attribution is under review, so upgrading an
	// older settings.json cannot silently activate an unsafe pause.
	Pause PauseConfig `json:"pause,omitempty" yaml:"pause,omitempty"`
}

// PauseConfig bounds the plugin-owned quota pause. MaxSeconds caps any single
// pause and is itself clamped to quotaPauseMaxWindow.
type PauseConfig struct {
	Enabled    *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	MaxSeconds int   `json:"max_seconds,omitempty" yaml:"max_seconds,omitempty"`
}

// LogConfig is the request log policy. Zero values mean "use the plugin
// default", so an older settings.json without this object keeps the previous
// behavior (logging on, 7 days, 500 entries).
type LogConfig struct {
	Enabled       *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	RetentionDays int   `json:"retention_days,omitempty" yaml:"retention_days,omitempty"`
	MaxEntries    int   `json:"max_entries,omitempty" yaml:"max_entries,omitempty"`
}

func defaultConfig() Config {
	return Config{DataDir: "plugins/" + PluginID + "-data", BaseURL: DefaultBaseURL}
}

// logSettings resolves the persisted policy into the effective policy.
func (c Config) logSettings() requestLogSettings {
	in := defaultRequestLogSettings()
	if c.Logs.Enabled != nil {
		in.Enabled = *c.Logs.Enabled
	}
	if c.Logs.RetentionDays > 0 {
		in.RetentionDays = c.Logs.RetentionDays
	}
	if c.Logs.MaxEntries > 0 {
		in.MaxEntries = c.Logs.MaxEntries
	}
	return normalizeRequestLogSettings(in)
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
			return localFail(http.StatusBadRequest, "invalid_request", "model id and upstream_id must be nonempty")
		}
		if len(m.ID) > 512 || len(m.UpstreamID) > 512 || strings.IndexFunc(m.ID+m.UpstreamID, unicode.IsControl) >= 0 {
			return localFail(http.StatusBadRequest, "invalid_request", "model id and upstream_id must be at most 512 bytes without control characters")
		}
		if seen[m.ID] {
			return localFail(http.StatusBadRequest, "invalid_request", "model aliases must be unique")
		}
		seen[m.ID] = true
		if err := validateReasoningMapping(m.Reasoning); err != nil {
			return err
		}
	}
	if c.Logs.RetentionDays != 0 && (c.Logs.RetentionDays < 1 || c.Logs.RetentionDays > maxRequestLogRetentionDays) {
		return fail(http.StatusBadRequest, "logs.retention_days must be between 1 and 90")
	}
	if c.Logs.MaxEntries != 0 && (c.Logs.MaxEntries < 1 || c.Logs.MaxEntries > maxRequestLogMaxEntries) {
		return fail(http.StatusBadRequest, "logs.max_entries must be between 1 and 500")
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
		return localFail(http.StatusBadRequest, "invalid_request", "base_url must be "+DefaultBaseURL)
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
	// CreditsPerUSD is the decimal string the operator recorded for this
	// account's plan: how many official credits one US dollar of usage is worth.
	// It is presentation-only (the console shows an approximate USD equivalent)
	// and it differs between plans, so it is never inferred from a shared
	// default or derived from another account.
	CreditsPerUSD string `json:"credits_per_usd,omitempty"`
	Disabled      bool   `json:"disabled"`
	BaseURL       string `json:"base_url,omitempty"`
	// ModelRevision changes whenever the model mapping changes. CPA rebuilds a
	// credential's model list only when the auth file content changes, so a
	// mapping update without a revision bump would never reach the host.
	ModelRevision int64 `json:"model_revision,omitempty"`
	// RequestScopedErrors is the plugin-wide retry-scheduling policy carried by
	// this credential. CPA reads it from the auth file metadata and decides
	// whether an upstream failure stops the request or rotates to another
	// credential, and whether the failed credential is cooled down. The plugin
	// owns the policy; CPA owns the scheduling decision.
	RequestScopedErrors []RequestErrorRule `json:"request_scoped_errors,omitempty"`
	// Disabled retains the operator's setting. QuotaBlock is a separate,
	// temporary plugin-owned scheduling restriction, following ClinePass.
	QuotaBlock    *QuotaBlock `json:"quota_block,omitempty"`
	QuotaRevision string      `json:"quota_revision,omitempty"`
	// billingRevision is an opaque in-memory generation for UI cache invalidation
	// after identity changes or quota recovery. It is random, never derived from
	// a key and never persisted in auth files.
	// runtimeEpoch fences model tests against credential identity changes, even
	// when a key or disabled flag is changed and later restored.
	runtimeEpoch    uint64
	billingRevision string
	extraFields     map[string]json.RawMessage
}

// credentialJSONFields is every key the Credential struct itself reads and
// writes. It is derived from the struct tags rather than maintained by hand: a
// key missing from this list is treated as a host- or operator-added field and
// re-emitted verbatim by MarshalJSON, so a hand-kept list silently made a
// newly added field impossible to clear (an omitempty zero value disappears
// from the known encoding and the stale preserved copy takes its place).
var credentialJSONFields = credentialKnownJSONFields()

func credentialKnownJSONFields() []string {
	typ := reflect.TypeOf(Credential{})
	fields := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		// Same naming rule encoding/json applies: "-" is never written, an
		// empty name falls back to the Go field name.
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = field.Name
		}
		fields = append(fields, name)
	}
	return fields
}

// UnmarshalJSON keeps host- or operator-added fields so a rewrite through
// host.auth.save cannot silently drop them.
func (c *Credential) UnmarshalJSON(raw []byte) error {
	type known Credential
	var decoded known
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	// CPA may persist the effective disabled flag back into the top-level JSON.
	// Only an intact block bound to this key may restore the separate manual
	// choice. When the snapshot is missing, its origin is unknown: fail closed
	// rather than treating an absent or false on-disk flag as permission to
	// re-enable this credential after the block expires.
	if quotaBlockBelongsToKey(decoded.QuotaBlock, decoded.APIKey) {
		decoded.Disabled = decoded.QuotaBlock.ManualDisabled == nil || *decoded.QuotaBlock.ManualDisabled
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
	value = redactComplete(value, keys...)
	if runes := []rune(value); len(runes) > 500 {
		value = string(runes[:500])
	}
	return value
}

// redactComplete examines the entire original value. Presentation limits are
// applied only afterwards, so a secret straddling a cutoff cannot survive as a
// prefix. Longer known secrets are replaced before overlapping shorter ones.
func redactComplete(value string, keys ...string) string {
	ordered := append([]string(nil), keys...)
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, key := range ordered {
		if key != "" {
			value = strings.ReplaceAll(value, key, "[redacted]")
		}
	}
	return secretPattern.ReplaceAllString(value, "[redacted]")
}
