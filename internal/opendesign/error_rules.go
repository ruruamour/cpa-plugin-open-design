package opendesign

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
)

// errorRulesFile is the plugin-wide retry-scheduling policy. It is written next
// to settings.json inside the plugin data directory and never contains keys.
const errorRulesFile = "error-rules.json"

// maxErrorRulesBodyBytes bounds one management body for the policy routes.
const maxErrorRulesBodyBytes = 1 << 20

const (
	maxErrorRules          = 100
	maxErrorRuleConditions = 32
	maxErrorRuleCondition  = 4096
)

// errorRuleActions are the four request-scoped actions CPA understands. The
// values are forwarded verbatim in each credential's auth file.
var errorRuleActions = map[string]bool{
	"stop":                  true,
	"stop-and-cooldown":     true,
	"continue":              true,
	"continue-and-cooldown": true,
}

// RequestErrorRule is one CPA request-scoped error rule. Field names match
// CPA's internalconfig.RequestScopedErrorRule so the host decodes the value
// straight out of auth metadata and the auth file.
type RequestErrorRule struct {
	Status      int      `json:"status"`
	Match       []string `json:"match,omitempty"`
	MatchRegexr []string `json:"match-regexr,omitempty"`
	Action      string   `json:"action"`
}

// errorRulesDocument is the persisted shape of <data_dir>/error-rules.json.
type errorRulesDocument struct {
	Rules []RequestErrorRule `json:"rules"`
}

type errorRuleSyncFailure struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Error string `json:"error"`
}

type errorRuleSyncResult struct {
	Total     int                    `json:"total"`
	Succeeded int                    `json:"succeeded"`
	Failed    []errorRuleSyncFailure `json:"failed"`
}

type errorRulesResponse struct {
	Rules    []RequestErrorRule  `json:"rules"`
	Defaults []RequestErrorRule  `json:"defaults"`
	Sync     errorRuleSyncResult `json:"sync"`
	Inherits bool                `json:"inherits_cpa"`
	// LoadError reports a persisted policy that could not be parsed. The plugin
	// keeps running on the defaults instead of refusing registration, and the
	// console shows the problem so an operator can re-save.
	LoadError string `json:"load_error,omitempty"`
}

// upstreamErrorMarker is the stable, machine-shaped prefix the plugin puts in
// front of every classified upstream failure. CPA matches request-scoped rules
// against the plugin error message, so this is the contract between the two.
// The prefix deliberately names no upstream provider: this message reaches the
// downstream client, which must not learn which provider backs the route.
const upstreamErrorMarker = "[upstream:"

// legacyUpstreamErrorMarker is the previous provider-named prefix. Policies
// saved by older builds are rewritten on load so upgraded credentials keep
// matching without an operator-triggered sync.
const legacyUpstreamErrorMarker = "[open-design:"

func upstreamErrorMarkerFor(kind string) string {
	return upstreamErrorMarker + kind + "]"
}

// migrateLegacyErrorMarkers rewrites the retired provider-named prefix in a
// persisted policy. It returns a copy; rule text is operator-owned, so the
// rewrite is limited to the exact legacy prefix.
func migrateLegacyErrorMarkers(rules []RequestErrorRule) ([]RequestErrorRule, bool) {
	out := cloneErrorRules(rules)
	changed := false
	rewrite := func(value string) string {
		if !strings.Contains(value, legacyUpstreamErrorMarker) {
			return value
		}
		changed = true
		return strings.ReplaceAll(value, legacyUpstreamErrorMarker, upstreamErrorMarker)
	}
	for i := range out {
		for j := range out[i].Match {
			out[i].Match[j] = rewrite(out[i].Match[j])
		}
		for j := range out[i].MatchRegexr {
			out[i].MatchRegexr[j] = rewrite(out[i].MatchRegexr[j])
		}
	}
	return out, changed
}

// defaultErrorRules is the built-in policy. Every entry keys off a marker this
// plugin itself emits, so a rule can never be confused with an unrelated
// upstream message.
func defaultErrorRules() []RequestErrorRule {
	return []RequestErrorRule{
		{Status: http.StatusBadRequest, Match: []string{upstreamErrorMarkerFor("tier_limit_exceeded")}, Action: "continue-and-cooldown"},
		{Status: http.StatusTooManyRequests, Match: []string{upstreamErrorMarkerFor("rate_limit_exceeded")}, Action: "continue-and-cooldown"},
		{Status: http.StatusTooManyRequests, Match: []string{upstreamErrorMarkerFor("insufficient_quota")}, Action: "continue-and-cooldown"},
		// A quota-blocked credential may still be selected briefly while CPA's
		// auth-file watcher catches up. This local marker permits another account
		// without attributing a new upstream failure or changing host cooldowns.
		{Status: http.StatusTooManyRequests, Match: []string{quotaBlockMarker}, Action: "continue"},
		{Status: http.StatusPaymentRequired, Match: []string{upstreamErrorMarkerFor("insufficient_quota")}, Action: "continue-and-cooldown"},
		// OpenDesign reports a spent coding plan (with a reset time) or an
		// uncovered wallet as 402 with its own insufficient_balance code. The
		// marker only exists when that machine code was present, so a bare
		// 402 with free text still stays unclassified.
		{Status: http.StatusPaymentRequired, Match: []string{upstreamErrorMarkerFor("insufficient_balance")}, Action: "continue-and-cooldown"},
		{Status: http.StatusUnauthorized, Match: []string{upstreamErrorMarkerFor("invalid_api_key")}, Action: "stop-and-cooldown"},
		{Status: http.StatusForbidden, Match: []string{upstreamErrorMarkerFor("invalid_api_key")}, Action: "stop-and-cooldown"},
		{Status: http.StatusInternalServerError, Match: []string{upstreamErrorMarkerFor("server_error")}, Action: "continue-and-cooldown"},
		{Status: http.StatusServiceUnavailable, Match: []string{upstreamErrorMarkerFor("server_overloaded")}, Action: "continue-and-cooldown"},
	}
}

func cloneErrorRules(rules []RequestErrorRule) []RequestErrorRule {
	if rules == nil {
		return nil
	}
	out := make([]RequestErrorRule, 0, len(rules))
	for _, rule := range rules {
		copied := rule
		if rule.Match != nil {
			copied.Match = append([]string{}, rule.Match...)
		}
		if rule.MatchRegexr != nil {
			copied.MatchRegexr = append([]string{}, rule.MatchRegexr...)
		}
		out = append(out, copied)
	}
	return out
}

func sameErrorRules(a, b []RequestErrorRule) bool {
	return bytes.Equal(jsonBytes(a), jsonBytes(b))
}

// errorRulesForMetadata renders the rules for the auth Metadata map. An empty
// policy becomes an empty array rather than null so the host reads "no rules"
// instead of "field absent".
func errorRulesForMetadata(rules []RequestErrorRule) []any {
	out := make([]any, 0, len(rules))
	for _, rule := range rules {
		out = append(out, rule)
	}
	return out
}

// currentErrorRules returns the live policy.
func (s *Service) currentErrorRules() []RequestErrorRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneErrorRules(s.errorRules)
}

// strictErrorRulesObject rejects duplicate keys, unknown fields and trailing
// JSON. Decoder and pattern text is never echoed into an API error.
func strictErrorRulesObject(raw []byte, out any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > maxErrorRulesBodyBytes || raw[0] != '{' {
		return localFail(http.StatusBadRequest, "invalid_json", "request must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := checkErrorRulesJSONKeys(decoder); err != nil {
		return localFail(http.StatusBadRequest, "invalid_json", "JSON is malformed or contains duplicate fields")
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return localFail(http.StatusBadRequest, "invalid_json", "JSON is malformed or contains unknown fields")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return localFail(http.StatusBadRequest, "invalid_json", "request must contain exactly one JSON object")
	}
	return nil
}

func checkErrorRulesJSONKeys(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		if delim == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate JSON key")
			}
			seen[name] = true
		}
		if err := checkErrorRulesJSONKeys(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

// decodeErrorRules parses and fully validates a policy body. It returns
// *APIError values whose messages are fixed strings.
func decodeErrorRules(raw []byte) ([]RequestErrorRule, error) {
	var document struct {
		Rules json.RawMessage `json:"rules"`
	}
	if err := strictErrorRulesObject(raw, &document); err != nil {
		return nil, err
	}
	array := bytes.TrimSpace(document.Rules)
	if len(array) == 0 || array[0] != '[' {
		return nil, localFail(http.StatusBadRequest, "invalid_json", "rules must be an array; an empty array means no plugin-managed rules")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(array, &items); err != nil || len(items) > maxErrorRules {
		return nil, localFail(http.StatusBadRequest, "invalid_json", fmt.Sprintf("rules must contain at most %d entries", maxErrorRules))
	}
	rules := make([]RequestErrorRule, 0, len(items))
	for i, item := range items {
		var rule RequestErrorRule
		if err := strictErrorRulesObject(item, &rule); err != nil {
			return nil, localFail(http.StatusBadRequest, "invalid_json", fmt.Sprintf("rule %d is malformed or contains unknown fields", i+1))
		}
		if rule.Status < 100 || rule.Status > 599 {
			return nil, localFail(http.StatusBadRequest, "invalid_json", fmt.Sprintf("rule %d status must be an integer between 100 and 599", i+1))
		}
		rule.Action = strings.ToLower(strings.TrimSpace(rule.Action))
		if !errorRuleActions[rule.Action] {
			return nil, localFail(http.StatusBadRequest, "invalid_json", fmt.Sprintf("rule %d action must be stop, stop-and-cooldown, continue or continue-and-cooldown", i+1))
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(item, &fields)
		for _, key := range []string{"match", "match-regexr"} {
			if value, present := fields[key]; present && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, localFail(http.StatusBadRequest, "invalid_json", fmt.Sprintf("rule %d match fields must be arrays of strings", i+1))
			}
		}
		if len(rule.Match)+len(rule.MatchRegexr) == 0 ||
			len(rule.Match) > maxErrorRuleConditions || len(rule.MatchRegexr) > maxErrorRuleConditions {
			return nil, localFail(http.StatusBadRequest, "invalid_json", fmt.Sprintf("rule %d needs at least one condition and at most %d per kind", i+1, maxErrorRuleConditions))
		}
		for _, conditions := range [][]string{rule.Match, rule.MatchRegexr} {
			for _, value := range conditions {
				if strings.TrimSpace(value) == "" || len(value) > maxErrorRuleCondition {
					return nil, localFail(http.StatusBadRequest, "invalid_json", fmt.Sprintf("rule %d conditions must be non-empty and at most %d bytes", i+1, maxErrorRuleCondition))
				}
			}
		}
		for _, pattern := range rule.MatchRegexr {
			if _, err := regexp.Compile(pattern); err != nil {
				return nil, localFail(http.StatusBadRequest, "invalid_json", fmt.Sprintf("rule %d contains an invalid regular expression", i+1))
			}
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// loadErrorRules reads the persisted policy. A missing file means "defaults".
// A corrupt file is reported instead of being fatal: refusing registration
// would take inference down over an operator-editable file.
func loadErrorRules(dataDir string) ([]RequestErrorRule, string) {
	if strings.TrimSpace(dataDir) == "" {
		return defaultErrorRules(), ""
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, errorRulesFile))
	if os.IsNotExist(err) {
		return defaultErrorRules(), ""
	}
	if err != nil {
		return defaultErrorRules(), "error-rules.json could not be read; using the built-in defaults"
	}
	rules, decodeErr := decodeErrorRules(raw)
	if decodeErr != nil {
		return defaultErrorRules(), "error-rules.json is invalid; using the built-in defaults until it is saved again"
	}
	// An upgraded policy may still name the retired provider-named marker.
	// Rewriting it in memory is enough: auth.parse reapplies the current policy
	// to every credential, so host-side rules migrate on the same start.
	if migrated, changed := migrateLegacyErrorMarkers(rules); changed {
		return migrated, ""
	}
	return rules, ""
}

// testErrorRulesWriteHook lets a test park the writer between the temporary
// write and the rename to prove the published policy and the file agree.
var testErrorRulesWriteHook atomic.Pointer[func()]

// publishErrorRulesFile renames a staged policy file into place through the
// shared retrying publish. It stays a variable so a test can inject the failure
// on platforms where a read-only directory does not stop the owner writing.
var publishErrorRulesFile = publishStagedFile

func saveErrorRules(dataDir string, rules []RequestErrorRule) error {
	path := filepath.Join(dataDir, errorRulesFile)
	body, err := json.MarshalIndent(errorRulesDocument{Rules: rules}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp-" + newID()
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if hook := testErrorRulesWriteHook.Load(); hook != nil {
		(*hook)()
	}
	return publishErrorRulesFile(tmp, path)
}

// markErrorRulesSyncedLocked records a bounded, fixed sync state. Caller holds mu.
// Host callback text can carry keys or whole credential JSON, so it is never
// stored or returned.
func (s *Service) markErrorRulesSyncedLocked(credentialID string, rules []RequestErrorRule, err error) {
	if s.errorRuleSynced == nil {
		s.errorRuleSynced = map[string]string{}
	}
	if s.errorRuleSyncErrors == nil {
		s.errorRuleSyncErrors = map[string]string{}
	}
	if err != nil {
		s.errorRuleSyncErrors[credentialID] = "凭据规则同步失败，请重试同步"
		return
	}
	s.errorRuleSynced[credentialID] = string(jsonBytes(rules))
	delete(s.errorRuleSyncErrors, credentialID)
}

// redactErrorRuleIdentity strips any live key from a credential id or label
// before it is echoed in a sync report.
func (s *Service) redactErrorRuleIdentity(value string, creds []Credential) string {
	for _, c := range creds {
		if c.APIKey != "" {
			value = strings.ReplaceAll(value, c.APIKey, "[redacted]")
		}
		if c.ControlKey != "" {
			value = strings.ReplaceAll(value, c.ControlKey, "[redacted]")
		}
		if c.WorkspaceID != "" {
			value = strings.ReplaceAll(value, c.WorkspaceID, "[redacted]")
		}
	}
	return value
}

func (s *Service) errorRulesResponse() errorRulesResponse {
	s.mu.RLock()
	rules := cloneErrorRules(s.errorRules)
	loadError := s.errorRulesLoadError
	creds := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		if !s.revoked[c.ID] {
			creds = append(creds, c)
		}
	}
	s.mu.RUnlock()
	sort.Slice(creds, func(i, j int) bool {
		if creds[i].Label != creds[j].Label {
			return creds[i].Label < creds[j].Label
		}
		return creds[i].ID < creds[j].ID
	})
	s.mu.RLock()
	serialized := string(jsonBytes(rules))
	sync := errorRuleSyncResult{Total: len(creds), Failed: []errorRuleSyncFailure{}}
	for _, c := range creds {
		detail := s.errorRuleSyncErrors[c.ID]
		if detail == "" && s.errorRuleSynced[c.ID] != serialized {
			detail = "待同步到 CPA 凭据"
		}
		if detail == "" {
			sync.Succeeded++
			continue
		}
		sync.Failed = append(sync.Failed, errorRuleSyncFailure{
			ID:    s.redactErrorRuleIdentity(c.ID, creds),
			Label: s.redactErrorRuleIdentity(c.Label, creds),
			Error: detail,
		})
	}
	s.mu.RUnlock()
	return errorRulesResponse{
		Rules:     rules,
		Defaults:  defaultErrorRules(),
		Sync:      sync,
		Inherits:  len(rules) == 0,
		LoadError: loadError,
	}
}

// errorRulesRequest serves GET/PUT /error-rules and POST /error-rules/{sync,reset}.
//
// Locking mirrors the credential routes: configMu serializes the policy file
// against settings writes, and credentialMu serializes the per-credential push
// against credential edits and deletes. authFileMu is only ever taken inside
// saveCredential, never across the host callback.
func (s *Service) errorRulesRequest(r ManagementRequest) (any, error) {
	if len(r.Body) > maxErrorRulesBodyBytes {
		return managementError(http.StatusRequestEntityTooLarge, "error rules request is too large")
	}
	if r.Method == http.MethodGet {
		return managementJSON(http.StatusOK, s.errorRulesResponse())
	}
	var rules []RequestErrorRule
	switch {
	case r.Method == http.MethodPut:
		decoded, err := decodeErrorRules(r.Body)
		if err != nil {
			return managementError(statusOf(err), err.Error())
		}
		rules = decoded
	default:
		// sync and reset take an empty object so an accidental body is rejected.
		var empty struct{}
		if err := strictErrorRulesObject(r.Body, &empty); err != nil {
			return managementError(statusOf(err), "request body must be an empty JSON object")
		}
		if strings.HasSuffix(strings.TrimRight(r.Path, "/"), "/reset") {
			rules = defaultErrorRules()
		}
	}
	s.configMu.Lock()
	if rules != nil {
		s.mu.RLock()
		dataDir := s.cfg.DataDir
		loaded := s.loaded
		s.mu.RUnlock()
		if !loaded || strings.TrimSpace(dataDir) == "" {
			s.configMu.Unlock()
			return managementError(http.StatusConflict, "plugin is not configured")
		}
		if err := saveErrorRules(dataDir, rules); err != nil {
			s.configMu.Unlock()
			return managementError(http.StatusInternalServerError, "could not persist the error rules")
		}
		s.mu.Lock()
		s.errorRules = cloneErrorRules(rules)
		s.errorRulesLoadError = ""
		s.mu.Unlock()
	}
	s.configMu.Unlock()
	s.credentialMu.Lock()
	s.syncErrorRulesLocked()
	s.credentialMu.Unlock()
	return managementJSON(http.StatusOK, s.errorRulesResponse())
}

// syncErrorRulesLocked pushes the live policy into every credential. Caller
// holds credentialMu so a late credential save cannot publish a stale policy
// afterwards. Each account is attempted independently.
func (s *Service) syncErrorRulesLocked() {
	rules := s.currentErrorRules()
	serialized := string(jsonBytes(rules))
	s.mu.RLock()
	creds := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		if !s.revoked[c.ID] {
			creds = append(creds, c)
		}
	}
	s.mu.RUnlock()
	sort.Slice(creds, func(i, j int) bool {
		if creds[i].Label != creds[j].Label {
			return creds[i].Label < creds[j].Label
		}
		return creds[i].ID < creds[j].ID
	})
	for _, c := range creds {
		s.mu.RLock()
		alreadySynced := s.errorRuleSynced[c.ID] == serialized && sameErrorRules(s.creds[c.ID].RequestScopedErrors, rules)
		s.mu.RUnlock()
		if alreadySynced {
			continue
		}
		err := s.syncCredentialErrorRules(c, rules)
		s.mu.Lock()
		s.markErrorRulesSyncedLocked(c.ID, rules, err)
		s.mu.Unlock()
	}
}

// syncCredentialErrorRules rewrites one credential with the given policy. It
// goes through host.auth.save so disk and the host record change together; the
// rules field is the only field that differs from the live credential.
func (s *Service) syncCredentialErrorRules(c Credential, rules []RequestErrorRule) error {
	s.mu.RLock()
	current, ok := s.creds[c.ID]
	filename := s.authFiles[c.ID]
	s.mu.RUnlock()
	if !ok {
		return localFail(http.StatusConflict, "credential_changed", "credential no longer exists")
	}
	if current.APIKey != c.APIKey {
		return localFail(http.StatusConflict, "credential_changed", "credential changed during the rule sync")
	}
	if filename == "" {
		filename = c.ID + ".json"
	}
	if filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) {
		return localFail(http.StatusBadRequest, "invalid_request", "invalid credential filename")
	}
	current.RequestScopedErrors = cloneErrorRules(rules)
	return s.saveCredential(current, filename)
}

// applyErrorRules reconciles a parsed credential with the plugin-wide policy.
// Caller holds authFileMu. Only the rule field is rewritten, so any host- or
// operator-added JSON survives. inFlight is true when a host.auth.save is
// currently writing this file: disk still holds the previous content, so the
// rewrite is skipped and the save itself is the writer.
func (s *Service) applyErrorRules(c *Credential, dir, filename string, raw []byte, inFlight bool) {
	rules := s.currentErrorRules()
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	var existing []RequestErrorRule
	_ = json.Unmarshal(fields["request_scoped_errors"], &existing)
	var err error
	if !inFlight && !sameErrorRules(existing, rules) {
		if dir == "" || filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) {
			err = errors.New("unresolved credential file")
		} else {
			path := filepath.Join(dir, filename)
			latest, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(latest, raw) {
				err = errors.New("credential file changed")
			} else {
				fields["request_scoped_errors"] = jsonBytes(rules)
				err = writeJSONAtomic(path, fields)
			}
		}
	}
	c.RequestScopedErrors = cloneErrorRules(rules)
	s.mu.Lock()
	s.markErrorRulesSyncedLocked(c.ID, rules, err)
	s.mu.Unlock()
}

// writeJSONAtomic replaces one file through a same-directory temporary file.
func writeJSONAtomic(path string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp-" + newID()
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return publishErrorRulesFile(tmp, path)
}

// compileErrorRuleRegexp compiles one operator-supplied pattern. It exists so a
// rule can be evaluated outside the host without duplicating the compile step.
func compileErrorRuleRegexp(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile(pattern)
}
