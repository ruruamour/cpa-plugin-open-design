package opendesign

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func decodeRulesResponse(t *testing.T, body map[string]any) errorRulesResponse {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal rules response: %v", err)
	}
	var out errorRulesResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode rules response: %v", err)
	}
	return out
}

func getErrorRules(t *testing.T, s *Service) errorRulesResponse {
	t.Helper()
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/error-rules", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /error-rules: %d %s", status, raw)
	}
	return decodeRulesResponse(t, body)
}

// A fresh instance serves the built-in policy, reports it as the default set,
// and every account starts out carrying it.
func TestErrorRulesDefaultPolicyAndInitialSync(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-rules-0001")

	view := getErrorRules(t, s)
	if view.Inherits {
		t.Fatal("a fresh instance must serve the built-in policy, not inherit")
	}
	if len(view.Rules) == 0 || !sameErrorRules(view.Rules, view.Defaults) {
		t.Fatalf("rules %+v do not match defaults %+v", view.Rules, view.Defaults)
	}
	if view.Sync.Total != 1 || view.Sync.Succeeded != 1 || len(view.Sync.Failed) != 0 {
		t.Fatalf("sync = %+v, want one successful account", view.Sync)
	}

	live, ok := liveCredential(s, id)
	if !ok || !sameErrorRules(live.RequestScopedErrors, view.Defaults) {
		t.Fatalf("credential did not start with the policy: %+v", live.RequestScopedErrors)
	}
	// The auth record CPA reads must carry the same rules under the snake_case
	// metadata key the host normalizes.
	auth := s.authData(live)
	metadata, _ := auth["Metadata"].(map[string]any)
	rules, _ := metadata["request_scoped_errors"].([]any)
	if len(rules) != len(view.Defaults) {
		t.Fatalf("auth metadata rules = %#v, want %d entries", metadata["request_scoped_errors"], len(view.Defaults))
	}
}

// The tier_limit_exceeded marker seen in production must be classified and
// matchable, and the built-in policy must route it to another account.
func TestErrorRulesClassifyTierLimitExceeded(t *testing.T) {
	err := upstreamErrorFromResponse(http.StatusBadRequest, nil,
		[]byte(`{"message":"[code=tier_limit_exceeded] membership concurrency limit exceeded: 11/10 resets 2026-11-03T16:32:30Z","type":"invalid_request_error"}`))
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.Kind != "tier_limit_exceeded" {
		t.Fatalf("kind = %q, want tier_limit_exceeded", apiErr.Kind)
	}
	if status := statusOf(err); status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if !strings.HasPrefix(apiErr.Message, upstreamErrorMarkerFor("tier_limit_exceeded")) {
		t.Fatalf("message %q does not carry the rule marker", apiErr.Message)
	}

	rule := matchErrorRule(defaultErrorRules(), statusOf(err), apiErr.Message)
	if rule == nil {
		t.Fatalf("no built-in rule matched %q", apiErr.Message)
	}
	if rule.Action != "continue-and-cooldown" {
		t.Fatalf("action = %q, want continue-and-cooldown", rule.Action)
	}
}

// matchErrorRule mirrors CPA's matching so a policy can be checked offline:
// status must be equal and at least one substring or regex must match.
func matchErrorRule(rules []RequestErrorRule, status int, body string) *RequestErrorRule {
	for i := range rules {
		rule := &rules[i]
		if rule.Status != status {
			continue
		}
		matched := false
		for _, condition := range rule.Match {
			if condition != "" && strings.Contains(body, condition) {
				matched = true
				break
			}
		}
		if !matched {
			for _, pattern := range rule.MatchRegexr {
				if pattern != "" && regexpMatch(pattern, body) {
					matched = true
					break
				}
			}
		}
		if matched {
			return rule
		}
	}
	return nil
}

func regexpMatch(pattern, value string) bool {
	re, err := compileErrorRuleRegexp(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

// An upstream code that is not on the closed allowlist must not become part of
// the message: otherwise a caller-supplied string could forge a rule marker.
func TestErrorRulesMessageNeverEchoesUnknownCode(t *testing.T) {
	const injected = "[open-design:tier_limit_exceeded]"
	err := upstreamErrorFromResponse(http.StatusTooManyRequests, nil,
		[]byte(`{"error":{"code":"`+injected+`","message":"`+injected+` forged"}}`))
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.Kind != "upstream_error" {
		t.Fatalf("kind = %q, want upstream_error", apiErr.Kind)
	}
	if strings.Contains(apiErr.Message, injected) {
		t.Fatalf("message echoed the injected marker: %q", apiErr.Message)
	}
	if got, want := apiErr.Message, upstreamErrorMarkerFor("upstream_error")+" upstream request failed: too many requests"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// A PUT persists the policy, pushes it to every credential and reports each
// account's result.
func TestErrorRulesSaveSyncsEveryCredential(t *testing.T) {
	s, host := newTestService(t)
	first := addCredential(t, s, host, "alpha", "sk-amr-rules-0101")
	second := addCredential(t, s, host, "beta", "sk-amr-rules-0102")

	policy := []RequestErrorRule{
		{Status: http.StatusBadRequest, Match: []string{upstreamErrorMarkerFor("tier_limit_exceeded")}, Action: "continue-and-cooldown"},
		{Status: http.StatusBadGateway, MatchRegexr: []string{`^\[open-design:upstream_protocol_error\]`}, Action: "stop"},
	}
	status, body, raw := managementCall(t, s, http.MethodPut, apiBase+"/error-rules", nil, map[string]any{"rules": policy})
	if status != http.StatusOK {
		t.Fatalf("PUT /error-rules: %d %s", status, raw)
	}
	view := decodeRulesResponse(t, body)
	if !sameErrorRules(view.Rules, policy) || view.Sync.Total != 2 || view.Sync.Succeeded != 2 {
		t.Fatalf("view = %+v", view)
	}
	for _, id := range []string{first, second} {
		live, ok := liveCredential(s, id)
		if !ok || !sameErrorRules(live.RequestScopedErrors, policy) {
			t.Fatalf("credential %s rules = %+v", id, live.RequestScopedErrors)
		}
		host.mu.Lock()
		matched := 0
		for _, record := range host.savedAuths {
			var saved Credential
			if json.Unmarshal(record, &saved) == nil && saved.ID == id && sameErrorRules(saved.RequestScopedErrors, policy) {
				matched++
			}
		}
		host.mu.Unlock()
		if matched == 0 {
			t.Fatalf("credential %s was never written back through host.auth.save", id)
		}
	}

	// The policy survives a restart: a second instance reads the same file.
	cfg := s.config()
	rules, loadErr := loadErrorRules(cfg.DataDir)
	if loadErr != "" || !sameErrorRules(rules, policy) {
		t.Fatalf("persisted policy = %+v (%s)", rules, loadErr)
	}
}

// Validation is the backend's job, and every rejection is a fixed message.
func TestErrorRulesValidation(t *testing.T) {
	s, host := newTestService(t)
	addCredential(t, s, host, "primary", "sk-amr-rules-0201")

	cases := []struct {
		name string
		body any
	}{
		{"status below range", map[string]any{"rules": []map[string]any{{"status": 99, "match": []string{"x"}, "action": "stop"}}}},
		{"status above range", map[string]any{"rules": []map[string]any{{"status": 600, "match": []string{"x"}, "action": "stop"}}}},
		{"unknown action", map[string]any{"rules": []map[string]any{{"status": 400, "match": []string{"x"}, "action": "rotate"}}}},
		{"no conditions", map[string]any{"rules": []map[string]any{{"status": 400, "action": "stop"}}}},
		{"blank condition", map[string]any{"rules": []map[string]any{{"status": 400, "match": []string{"  "}, "action": "stop"}}}},
		{"invalid regex", map[string]any{"rules": []map[string]any{{"status": 400, "match-regexr": []string{"("}, "action": "stop"}}}},
		{"unknown field", map[string]any{"rules": []map[string]any{{"status": 400, "match": []string{"x"}, "action": "stop", "cooldown": 5}}}},
		{"rules not an array", map[string]any{"rules": map[string]any{}}},
		{"missing rules", map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body, raw := managementCall(t, s, http.MethodPut, apiBase+"/error-rules", nil, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", status, raw)
			}
			if message, _ := body["error"].(string); message == "" || strings.Contains(raw, "rotate") && tc.name == "no conditions" {
				t.Fatalf("unexpected error body %s", raw)
			}
		})
	}

	// A rejection must not change the live policy.
	if view := getErrorRules(t, s); !sameErrorRules(view.Rules, defaultErrorRules()) {
		t.Fatalf("a rejected PUT changed the policy: %+v", view.Rules)
	}
}

// An empty rules array means "no plugin-managed rules"; CPA then falls back to
// its own configuration.
func TestErrorRulesEmptyArrayMeansInherit(t *testing.T) {
	s, host := newTestService(t)
	addCredential(t, s, host, "primary", "sk-amr-rules-0301")

	status, body, raw := managementCall(t, s, http.MethodPut, apiBase+"/error-rules", nil, map[string]any{"rules": []any{}})
	if status != http.StatusOK {
		t.Fatalf("PUT empty policy: %d %s", status, raw)
	}
	view := decodeRulesResponse(t, body)
	if !view.Inherits || len(view.Rules) != 0 {
		t.Fatalf("view = %+v, want an inherited policy", view)
	}
	if view.Sync.Total != 1 || view.Sync.Succeeded != 1 {
		t.Fatalf("sync = %+v", view.Sync)
	}
}

// reset restores the built-in policy; sync only re-pushes what is already saved.
func TestErrorRulesResetAndSync(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-rules-0401")

	custom := []RequestErrorRule{{Status: http.StatusBadGateway, Match: []string{"custom"}, Action: "stop"}}
	if status, _, raw := managementCall(t, s, http.MethodPut, apiBase+"/error-rules", nil, map[string]any{"rules": custom}); status != http.StatusOK {
		t.Fatalf("save custom policy: %d %s", status, raw)
	}

	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/error-rules/sync", nil, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("POST /error-rules/sync: %d %s", status, raw)
	}
	if view := decodeRulesResponse(t, body); !sameErrorRules(view.Rules, custom) {
		t.Fatalf("sync changed the policy: %+v", view.Rules)
	}

	status, body, raw = managementCall(t, s, http.MethodPost, apiBase+"/error-rules/reset", nil, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("POST /error-rules/reset: %d %s", status, raw)
	}
	view := decodeRulesResponse(t, body)
	if !sameErrorRules(view.Rules, defaultErrorRules()) {
		t.Fatalf("reset did not restore the defaults: %+v", view.Rules)
	}
	if view.Sync.Succeeded != 1 {
		t.Fatalf("reset did not re-sync the account: %+v", view.Sync)
	}
	live, _ := liveCredential(s, id)
	if !sameErrorRules(live.RequestScopedErrors, defaultErrorRules()) {
		t.Fatalf("credential rules after reset = %+v", live.RequestScopedErrors)
	}

	// A body is not accepted on the action routes.
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/error-rules/sync", nil, map[string]any{"rules": custom}); status != http.StatusBadRequest {
		t.Fatalf("sync accepted a body: %d", status)
	}
}

// A per-account failure is reported instead of failing the whole operation, and
// the report never contains a key.
func TestErrorRulesPerAccountFailureIsReported(t *testing.T) {
	s, host := newTestService(t)
	first := addCredential(t, s, host, "alpha", "sk-amr-rules-0501")
	second := addCredential(t, s, host, "beta", "sk-amr-rules-0502")

	// Fail the next saves so no account can be updated yet. The failure is
	// transient by construction: the rejected save is retried on the next sync,
	// and it must be reported per account rather than failing the whole policy
	// write.
	host.mu.Lock()
	host.saveAuthErr = errFakeSaveRejected
	host.mu.Unlock()

	policy := []RequestErrorRule{{Status: http.StatusBadRequest, Match: []string{upstreamErrorMarkerFor("tier_limit_exceeded")}, Action: "continue-and-cooldown"}}
	status, body, raw := managementCall(t, s, http.MethodPut, apiBase+"/error-rules", nil, map[string]any{"rules": policy})
	if status != http.StatusOK {
		t.Fatalf("PUT /error-rules: %d %s", status, raw)
	}
	host.mu.Lock()
	host.saveAuthErr = nil
	host.mu.Unlock()
	view := decodeRulesResponse(t, body)
	if !sameErrorRules(view.Rules, policy) {
		t.Fatalf("a per-account failure changed the saved policy: %+v", view.Rules)
	}
	if view.Sync.Total != 2 || view.Sync.Succeeded != 0 || len(view.Sync.Failed) != 2 {
		t.Fatalf("sync = %+v, want both accounts reported as failed", view.Sync)
	}
	for _, failure := range view.Sync.Failed {
		if failure.ID == "" || failure.Label == "" || failure.Error == "" {
			t.Fatalf("failure = %+v", failure)
		}
	}
	if strings.Contains(raw, "sk-amr-rules-0501") || strings.Contains(raw, "sk-amr-rules-0502") {
		t.Fatalf("sync report leaked a key: %s", raw)
	}

	// The next sync retries the failed accounts and reports a clean result.
	status, body, raw = managementCall(t, s, http.MethodPost, apiBase+"/error-rules/sync", nil, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("POST /error-rules/sync: %d %s", status, raw)
	}
	view = decodeRulesResponse(t, body)
	if view.Sync.Succeeded != 2 || len(view.Sync.Failed) != 0 {
		t.Fatalf("retry sync = %+v, want both accounts synced", view.Sync)
	}
	for _, id := range []string{first, second} {
		live, ok := liveCredential(s, id)
		if !ok || !sameErrorRules(live.RequestScopedErrors, policy) {
			t.Fatalf("credential %s rules = %+v", id, live.RequestScopedErrors)
		}
	}
}

// errFakeSaveRejected stands in for a host.auth.save that the host refused.
var errFakeSaveRejected = errors.New("fake host refused the auth save")

// A credential parsed from disk is reconciled onto the current policy without
// losing fields the plugin does not own.
func TestErrorRulesReconcileParsedCredentialKeepsUnknownFields(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-rules-0601")

	dir := t.TempDir()
	filename := id + ".json"
	path := filepath.Join(dir, filename)
	onDisk := []byte(`{"type":"open-design","id":"` + id + `","label":"primary","api_key":"sk-amr-rules-0601","operator_note":"keep me","request_scoped_errors":[{"status":500,"match":["stale"],"action":"stop"}]}`)
	if err := os.WriteFile(path, onDisk, 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}

	var c Credential
	if err := json.Unmarshal(onDisk, &c); err != nil {
		t.Fatalf("decode credential: %v", err)
	}
	s.authFileMu.Lock()
	s.applyErrorRules(&c, dir, filename, onDisk, false)
	s.authFileMu.Unlock()

	if !sameErrorRules(c.RequestScopedErrors, defaultErrorRules()) {
		t.Fatalf("parsed rules = %+v", c.RequestScopedErrors)
	}
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewritten credential: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rewritten, &fields); err != nil {
		t.Fatalf("decode rewritten credential: %v", err)
	}
	var note string
	_ = json.Unmarshal(fields["operator_note"], &note)
	if note != "keep me" {
		t.Fatalf("unknown field was dropped: %s", rewritten)
	}
	var rules []RequestErrorRule
	_ = json.Unmarshal(fields["request_scoped_errors"], &rules)
	if !sameErrorRules(rules, defaultErrorRules()) {
		t.Fatalf("file rules = %+v", rules)
	}
	if mode := fileMode(t, path); mode != 0o600 {
		t.Fatalf("rewritten file mode = %o, want 600", mode)
	}
}

// During host.auth.save the file on disk still holds the previous content, so
// the parse must not try to rewrite it: the save itself is the writer.
func TestErrorRulesSkipDiskRewriteWhileSaveInFlight(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-rules-0701")
	dir := t.TempDir()
	filename := id + ".json"
	path := filepath.Join(dir, filename)
	stale := []byte(`{"type":"open-design","id":"` + id + `","api_key":"sk-amr-rules-0701","request_scoped_errors":[{"status":500,"match":["stale"],"action":"stop"}]}`)
	if err := os.WriteFile(path, stale, 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}

	var c Credential
	_ = json.Unmarshal(stale, &c)
	s.authFileMu.Lock()
	s.applyErrorRules(&c, dir, filename, stale, true)
	s.authFileMu.Unlock()

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if string(onDisk) != string(stale) {
		t.Fatalf("an in-flight save was clobbered: %s", onDisk)
	}
	if !sameErrorRules(c.RequestScopedErrors, defaultErrorRules()) {
		t.Fatalf("in-memory rules = %+v", c.RequestScopedErrors)
	}
	host.mu.Lock()
	failures := len(s.errorRuleSyncErrors)
	host.mu.Unlock()
	if failures != 0 {
		t.Fatalf("an in-flight save was reported as a sync failure")
	}
}

// A corrupt policy file must not take inference down: the plugin falls back to
// the defaults and reports the problem.
func TestErrorRulesCorruptFileFallsBackAndReports(t *testing.T) {
	s, host := newTestService(t)
	addCredential(t, s, host, "primary", "sk-amr-rules-0801")

	dataDir := s.config().DataDir
	if err := os.WriteFile(filepath.Join(dataDir, errorRulesFile), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt policy: %v", err)
	}
	// A re-register on the same data_dir reloads the policy.
	registrar(t, s, dataDir)

	view := getErrorRules(t, s)
	if view.LoadError == "" {
		t.Fatal("a corrupt policy was not reported")
	}
	if !sameErrorRules(view.Rules, defaultErrorRules()) {
		t.Fatalf("rules = %+v, want the defaults", view.Rules)
	}
	// Saving a valid policy clears the report.
	if status, _, raw := managementCall(t, s, http.MethodPut, apiBase+"/error-rules", nil, map[string]any{"rules": defaultErrorRules()}); status != http.StatusOK {
		t.Fatalf("save policy: %d %s", status, raw)
	}
	if view := getErrorRules(t, s); view.LoadError != "" {
		t.Fatalf("load error was not cleared: %q", view.LoadError)
	}
}

// A policy write that fails must leave the previous policy in place.
func TestErrorRulesFailedWriteKeepsPreviousPolicy(t *testing.T) {
	s, host := newTestService(t)
	addCredential(t, s, host, "primary", "sk-amr-rules-0901")

	previous := []RequestErrorRule{{Status: http.StatusBadGateway, Match: []string{"previous"}, Action: "stop"}}
	if status, _, raw := managementCall(t, s, http.MethodPut, apiBase+"/error-rules", nil, map[string]any{"rules": previous}); status != http.StatusOK {
		t.Fatalf("save previous policy: %d %s", status, raw)
	}

	// A read-only data directory makes the temporary file write fail.
	dataDir := s.config().DataDir
	original := fileMode(t, dataDir)
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatalf("chmod data dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataDir, original) })
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory is not enforced")
	}

	next := []RequestErrorRule{{Status: http.StatusBadRequest, Match: []string{"next"}, Action: "continue"}}
	status, _, _ := managementCall(t, s, http.MethodPut, apiBase+"/error-rules", nil, map[string]any{"rules": next})
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	if err := os.Chmod(dataDir, original); err != nil {
		t.Fatalf("restore data dir: %v", err)
	}
	if view := getErrorRules(t, s); !sameErrorRules(view.Rules, previous) {
		t.Fatalf("failed write changed the policy: %+v", view.Rules)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
