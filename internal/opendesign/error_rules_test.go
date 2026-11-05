package opendesign

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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

// A bare 402 with no machine-readable code stays unclassified on purpose. The
// plugin cannot tell "this account is out of quota" from "payment required for
// another reason", and guessing would cool down healthy accounts: production
// saw six such 402s in one day, none of them an exhausted OpenDesign account.
func TestErrorRulesBarePaymentRequiredStaysUnclassified(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"error":{"message":"payment required"}}`),
		[]byte(`{"error":{"type":"invalid_request_error","message":"Insufficient credits"}}`),
		[]byte(`{"detail":"Payment Required"}`),
		nil,
	} {
		err := upstreamErrorFromResponse(http.StatusPaymentRequired, nil, body)
		var apiErr *APIError
		if !asAPIError(err, &apiErr) {
			t.Fatalf("error is %T, want *APIError", err)
		}
		if apiErr.Kind != "upstream_error" {
			t.Fatalf("body %q: kind = %q, want upstream_error", body, apiErr.Kind)
		}
		if status := statusOf(err); status != http.StatusPaymentRequired {
			t.Fatalf("body %q: status = %d, want 402", body, status)
		}
		// The payment-required rule only matches the insufficient_quota marker,
		// so an unclassified 402 must not be routed to another account.
		if rule := matchErrorRule(defaultErrorRules(), statusOf(err), apiErr.Message); rule != nil {
			t.Fatalf("body %q: unclassified 402 matched rule %+v", body, rule)
		}
	}
}

// The 402 that does carry the allowlisted code is still classified and routed.
func TestErrorRulesClassifyPaymentRequiredWithQuotaCode(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"error":{"code":"insufficient_quota","message":"quota exhausted"}}`),
		[]byte(`{"error":{"message":"[code=insufficient_quota] out of credits"}}`),
	} {
		err := upstreamErrorFromResponse(http.StatusPaymentRequired, nil, body)
		var apiErr *APIError
		if !asAPIError(err, &apiErr) {
			t.Fatalf("error is %T, want *APIError", err)
		}
		if apiErr.Kind != "insufficient_quota" {
			t.Fatalf("body %q: kind = %q, want insufficient_quota", body, apiErr.Kind)
		}
		rule := matchErrorRule(defaultErrorRules(), statusOf(err), apiErr.Message)
		if rule == nil || rule.Action != "continue-and-cooldown" {
			t.Fatalf("body %q: rule = %+v, want continue-and-cooldown", body, rule)
		}
	}
}

// The real production exhaustion payload from OpenDesign: 402 with its own
// insufficient_balance code and a coding_plan_exhausted flag. This is the only
// signal that distinguishes "this account is out of quota" from the bare
// "payment required" text, so it must classify and route to another account.
func TestErrorRulesClassifyInsufficientBalanceWithPlanFlag(t *testing.T) {
	body := []byte(`{"error":{"code":"insufficient_balance","details":{"coding_plan_exhausted":true,"reset_at":"2026-11-03T12:02:06Z"},"message":"[code=insufficient_balance] Coding Plan quota is exhausted and wallet balance is insufficient. Please top up your wallet to continue now.","type":"insufficient_balance"}}`)
	err := upstreamErrorFromResponse(http.StatusPaymentRequired, nil, body)
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.Kind != "insufficient_balance" {
		t.Fatalf("kind = %q, want insufficient_balance", apiErr.Kind)
	}
	if status := statusOf(err); status != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", status)
	}
	// The message must stay a stable plugin-owned marker: the upstream text
	// names the wallet, the plan and a reset timestamp.
	if strings.Contains(apiErr.Message, "wallet") || strings.Contains(apiErr.Message, "2026-11-03") {
		t.Fatalf("message echoed upstream text: %q", apiErr.Message)
	}
	rule := matchErrorRule(defaultErrorRules(), statusOf(err), apiErr.Message)
	if rule == nil || rule.Action != "continue-and-cooldown" {
		t.Fatalf("rule = %+v, want continue-and-cooldown", rule)
	}
}

// The same exhaustion code reaches the plugin through a streamed close, where
// only the structured detail survives. Both the code and the marker are fixed.
func TestErrorRulesInsufficientBalanceMarkerIsClosed(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"error":{"code":"insufficient_balance","message":"x"}}`),
		[]byte(`{"error":{"message":"[code=insufficient_balance] Coding Plan quota is exhausted"}}`),
	} {
		err := upstreamErrorFromResponse(http.StatusPaymentRequired, nil, body)
		var apiErr *APIError
		if !asAPIError(err, &apiErr) {
			t.Fatalf("error is %T, want *APIError", err)
		}
		if got, want := apiErr.Message, upstreamErrorMarkerFor("insufficient_balance")+" upstream request failed: payment required"; got != want {
			t.Fatalf("body %q: message = %q, want %q", body, got, want)
		}
		if !safeDiagnosticKind(apiErr.Kind, nil) {
			t.Fatalf("body %q: kind %q is not a safe diagnostic kind", body, apiErr.Kind)
		}
	}
}

// A caller-supplied marker must not be able to forge the exhaustion rule: an
// unknown code never reaches the message, so the marker cannot be injected.
func TestErrorRulesUnknownBalanceCodeStaysUnclassified(t *testing.T) {
	err := upstreamErrorFromResponse(http.StatusPaymentRequired, nil,
		[]byte(`{"error":{"code":"insufficient_balance_evil","message":"payment required"}}`))
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.Kind != "upstream_error" {
		t.Fatalf("kind = %q, want upstream_error", apiErr.Kind)
	}
	if rule := matchErrorRule(defaultErrorRules(), statusOf(err), apiErr.Message); rule != nil {
		t.Fatalf("unclassified 402 matched rule %+v", rule)
	}
}

// An upstream code that is not on the closed allowlist must not become part of
// the message: otherwise a caller-supplied string could forge a rule marker.
func TestErrorRulesMessageNeverEchoesUnknownCode(t *testing.T) {
	const injected = "[upstream:tier_limit_exceeded]"
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
		{Status: http.StatusBadGateway, MatchRegexr: []string{`^\[upstream:upstream_protocol_error\]`}, Action: "stop"},
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
	if runtime.GOOS != "windows" {
		// Windows has no POSIX mode bits; the owner-only ACL is not readable
		// through os.Stat, so the assertion only applies to the Unix targets.
		if mode := fileMode(t, path); mode != 0o600 {
			t.Fatalf("rewritten file mode = %o, want 600", mode)
		}
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

// A credential must not reveal the upstream provider in a downstream error, so
// the marker names only "upstream". Policies saved by older builds still carry
// the provider-named prefix and are migrated when the plugin loads.
func TestErrorRulesLegacyMarkerMigratesOnLoadAndParse(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-rules-0802")
	dataDir := s.config().DataDir
	legacy := []RequestErrorRule{
		{Status: http.StatusBadRequest, Match: []string{"[open-design:tier_limit_exceeded]"}, Action: "continue-and-cooldown"},
		{Status: http.StatusBadGateway, MatchRegexr: []string{`^\[open-design:upstream_`}, Action: "stop"},
	}
	body, err := json.Marshal(errorRulesDocument{Rules: legacy})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, errorRulesFile), body, 0o600); err != nil {
		t.Fatalf("write legacy policy: %v", err)
	}
	registrar(t, s, dataDir)

	view := getErrorRules(t, s)
	want := []RequestErrorRule{
		{Status: http.StatusBadRequest, Match: []string{"[upstream:tier_limit_exceeded]"}, Action: "continue-and-cooldown"},
		{Status: http.StatusBadGateway, MatchRegexr: []string{`^\[upstream:upstream_`}, Action: "stop"},
	}
	if view.LoadError != "" || !sameErrorRules(view.Rules, want) {
		t.Fatalf("legacy policy was not migrated: %+v (%s)", view.Rules, view.LoadError)
	}

	// auth.parse reapplies the migrated policy, so an upgraded credential file
	// stops carrying the provider-named prefix even without an explicit sync.
	dir := t.TempDir()
	filename := id + ".json"
	path := filepath.Join(dir, filename)
	onDisk := []byte(`{"type":"open-design","id":"` + id + `","label":"primary","api_key":"sk-amr-rules-0802","request_scoped_errors":[{"status":400,"match":["[open-design:tier_limit_exceeded]"],"action":"continue-and-cooldown"}]}`)
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
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewritten credential: %v", err)
	}
	if strings.Contains(string(rewritten), legacyUpstreamErrorMarker) || !strings.Contains(string(rewritten), upstreamErrorMarker) {
		t.Fatalf("credential kept the provider-named marker: %s", rewritten)
	}
	if !sameErrorRules(c.RequestScopedErrors, want) {
		t.Fatalf("parsed rules = %+v", c.RequestScopedErrors)
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

	// The failure is injected at the rename so the case behaves the same on
	// every platform: a read-only directory does not stop the owner on Windows,
	// and running as root bypasses it elsewhere.
	restore := failErrorRulesPublish(t)

	next := []RequestErrorRule{{Status: http.StatusBadRequest, Match: []string{"next"}, Action: "continue"}}
	status, _, _ := managementCall(t, s, http.MethodPut, apiBase+"/error-rules", nil, map[string]any{"rules": next})
	restore()
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	if view := getErrorRules(t, s); !sameErrorRules(view.Rules, previous) {
		t.Fatalf("failed write changed the policy: %+v", view.Rules)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(s.config().DataDir, errorRulesFile) + ".tmp-*"); len(leftovers) != 0 {
		t.Fatalf("failed write left staged files: %v", leftovers)
	}
}

// failErrorRulesPublish makes the policy-file rename fail, which is how a failed
// write is injected on platforms where a read-only directory is not enforced.
// The returned function restores the real rename.
func failErrorRulesPublish(t *testing.T) func() {
	t.Helper()
	original := publishErrorRulesFile
	publishErrorRulesFile = func(string, string) error { return errors.New("Access is denied.") }
	return func() { publishErrorRulesFile = original }
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
