package opendesign

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestConfigRouteOnlyUpdatesPluginOwnedLogPolicy(t *testing.T) {
	s, host := newTestService(t)
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/config", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("config GET status = %d body = %s", status, raw)
	}
	logs, ok := body["logs"].(map[string]any)
	if !ok || logs["enabled"] != true || logs["retention_days"] != float64(defaultRequestLogRetentionDays) {
		t.Fatalf("default logs = %#v", body["logs"])
	}
	upstream := body["upstream"].(map[string]any)
	if upstream["origin"] != DefaultBaseURL || upstream["fixed"] != true {
		t.Fatalf("upstream config = %#v", upstream)
	}
	proxy := body["proxy"].(map[string]any)
	if proxy["read_only"] != true || proxy["owner"] != "CPA" {
		t.Fatalf("proxy config = %#v", proxy)
	}

	status, body, raw = managementCall(t, s, http.MethodPost, apiBase+"/config", nil, map[string]any{
		"logs": map[string]any{"enabled": false, "retention_days": 3, "max_entries": 12},
	})
	if status != http.StatusOK {
		t.Fatalf("config POST status = %d body = %s", status, raw)
	}
	logs = body["logs"].(map[string]any)
	if logs["enabled"] != false || logs["retention_days"] != float64(3) || logs["max_entries"] != float64(12) {
		t.Fatalf("updated logs = %#v", logs)
	}
	status, body, raw = managementCall(t, s, http.MethodPost, apiBase+"/config", nil, map[string]any{
		"base_url": "https://attacker.invalid/v1",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("foreign config field status = %d body = %s", status, raw)
	}
	status, body, raw = managementCall(t, s, http.MethodGet, apiBase+"/config", nil, nil)
	if status != http.StatusOK || body["upstream"].(map[string]any)["origin"] != DefaultBaseURL {
		t.Fatalf("fixed origin changed: status=%d body=%s", status, raw)
	}
	if host.countMethod("host.http.do") != 0 {
		t.Fatal("config operations unexpectedly used upstream HTTP")
	}
}

func TestConfigRejectsUnknownFieldsMultipleValuesAndExplicitZero(t *testing.T) {
	s, _ := newTestService(t)
	for _, raw := range [][]byte{
		[]byte(`{"logs":{"enabled":true,"foreign":1}}`),
		[]byte(`{"logs":{"retention_days":0}}`),
		[]byte(`{"logs":{"max_entries":0}}`),
		[]byte(`{"logs":{"enabled":true}} {"logs":{"enabled":false}}`),
	} {
		status, _, response := managementRawCall(t, s, http.MethodPost, apiBase+"/config", raw)
		if status != http.StatusBadRequest {
			t.Fatalf("invalid config %s status = %d response = %s", raw, status, response)
		}
	}
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/config", nil, nil)
	if status != http.StatusOK || body["logs"].(map[string]any)["enabled"] != true {
		t.Fatalf("invalid config changed state: status=%d body=%s", status, raw)
	}
}

func managementRawCall(t *testing.T, s *Service, method, path string, body []byte) (int, map[string]any, string) {
	t.Helper()
	request, err := json.Marshal(ManagementRequest{Method: method, Path: path, Body: body})
	if err != nil {
		t.Fatalf("marshal raw management request: %v", err)
	}
	result, err := s.Handle("management.handle", request)
	if err != nil {
		t.Fatalf("raw management request: %v", err)
	}
	response := result.(ManagementResponse)
	var decoded map[string]any
	if json.Unmarshal(response.Body, &decoded) != nil {
		return response.StatusCode, nil, string(response.Body)
	}
	return response.StatusCode, decoded, string(response.Body)
}

func TestRuntimeRouteMatchesConfiguredCustomAuthFilename(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-runtime-custom-name")
	s.mu.Lock()
	s.authFiles[id] = "custom-open-design.json"
	s.mu.Unlock()
	host.mu.Lock()
	host.authList = []map[string]any{{
		"name":     "/var/lib/cpa/auth/custom-open-design.json",
		"provider": Provider,
		"status":   "cooldown sk-runtime-custom-name",
	}}
	host.mu.Unlock()

	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/runtime", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("runtime status = %d body = %s", status, raw)
	}
	accounts := body["credentials"].([]any)
	account := accounts[0].(map[string]any)
	hostView, ok := account["host"].(map[string]any)
	if !ok || hostView["status"] != "cooldown [redacted]" || containsSecret(raw, "sk-runtime-custom-name") {
		t.Fatalf("custom runtime auth match or status redaction failed: %s", raw)
	}
}

func TestRuntimeRouteReportsFirstModelTestAsRunning(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-runtime-testing")
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "open-design-test", "upstream_id": "od/test"}},
	})
	if status != http.StatusOK {
		t.Fatalf("save models status = %d body = %s", status, raw)
	}
	key := id + "\x00open-design-test"
	s.modelTestStarted(key)
	defer func() {
		s.mu.Lock()
		delete(s.modelTestRunning, key)
		s.mu.Unlock()
	}()

	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/runtime", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("runtime status = %d body = %s", status, raw)
	}
	account := body["credentials"].([]any)[0].(map[string]any)
	if account["testing"] != true {
		t.Fatalf("first model test was not reported as running: %s", raw)
	}
}

func TestRuntimeRouteFallsBackWhenHostAuthListIsUnavailable(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-runtime-secret")
	host.mu.Lock()
	host.authListErr = errors.New("unsupported host callback host.auth.list")
	host.mu.Unlock()
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/runtime", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("runtime status = %d body = %s", status, raw)
	}
	if body["stopped"] != false || body["loaded"] != true {
		t.Fatalf("runtime lifecycle = %#v", body)
	}
	hostView, ok := body["host"].(map[string]any)
	if !ok || hostView["available"] != false || hostView["note"] == nil {
		t.Fatalf("runtime host fallback = %#v", body["host"])
	}
	accounts, ok := body["credentials"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("runtime accounts = %#v", body["credentials"])
	}
	account := accounts[0].(map[string]any)
	if account["id"] != id || account["testing"] != false {
		t.Fatalf("runtime account = %#v", account)
	}
	if containsSecret(raw, "sk-runtime-secret") {
		t.Fatalf("runtime leaked credential secret: %s", raw)
	}
}

func TestRuntimeRoutePublishesNullableHostFactsAndRedactsStatusText(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-runtime-host-secret")
	host.mu.Lock()
	host.authList = []map[string]any{{
		"name":             id + ".json",
		"provider":         Provider,
		"status":           "cooldown",
		"status_message":   "retry with sk-runtime-host-secret",
		"disabled":         nil,
		"unavailable":      nil,
		"next_retry_after": "2026-11-03T00:05:00Z",
	}}
	host.mu.Unlock()

	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/runtime", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("runtime status = %d body = %s", status, raw)
	}
	accounts, ok := body["credentials"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("runtime accounts = %#v", body["credentials"])
	}
	account := accounts[0].(map[string]any)
	hostView, ok := account["host"].(map[string]any)
	if !ok {
		t.Fatalf("runtime account host = %#v", account["host"])
	}
	if hostView["disabled"] != nil || hostView["unavailable"] != nil || hostView["status"] != "cooldown" {
		t.Fatalf("nullable host facts = %#v", hostView)
	}
	if hostView["next_retry_after"] == nil || containsSecret(raw, "sk-runtime-host-secret") {
		t.Fatalf("runtime host facts leaked secret or retry time: %s", raw)
	}
	if !strings.Contains(raw, "[redacted]") {
		t.Fatalf("runtime status message was not redacted: %s", raw)
	}
}

func containsSecret(value, secret string) bool {
	return secret != "" && strings.Contains(value, secret)
}

func TestRuntimeRedactsCredentialReplacedDuringHostRead(t *testing.T) {
	s, host := newTestService(t)
	oldKey := "arbitraryOldRuntimeKeyWithoutAPattern"
	newKey := "arbitraryNewRuntimeKeyWithoutAPattern"
	id := addCredential(t, s, host, "primary", oldKey)
	s.SetHost(func(method string, in, out any) error {
		if method != "host.auth.list" {
			return host.call(method, in, out)
		}
		// A key replacement may complete while the host is still returning the old
		// credential's status. The response includes metadata from both identities.
		s.mu.Lock()
		c := s.creds[id]
		c.APIKey, c.Label = newKey, newKey
		s.storeCredentialLocked(c)
		s.mu.Unlock()
		assign(out, map[string]any{"files": []map[string]any{{
			"id": id, "provider": Provider, "status_message": "old diagnostic " + oldKey,
		}}})
		return nil
	})
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/runtime", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("runtime status = %d: %s", status, raw)
	}
	if strings.Contains(raw, oldKey) || strings.Contains(raw, newKey) {
		t.Fatalf("runtime leaked a key across the host snapshot boundary: %s", raw)
	}
	row := body["credentials"].([]any)[0].(map[string]any)
	if row["label"] != "[redacted]" || row["host"].(map[string]any)["status_message"] != "old diagnostic [redacted]" {
		t.Fatalf("runtime did not retain safe facts from both identities: %s", raw)
	}
}
