package opendesign

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSchedulingViewProjectsHostCooldowns(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-primary-secret")
	other := addCredential(t, s, host, "other", "sk-other-secret")
	now := time.Now().UTC()
	credentialUntil := now.Add(2 * time.Hour).Truncate(time.Second)
	expired := now.Add(-time.Minute).Truncate(time.Second)
	host.mu.Lock()
	host.authList = []map[string]any{
		{"id": id, "name": id + ".json", "provider": Provider, "auth_index": "idx-primary", "status": "active"},
		{"id": other, "name": other + ".json", "provider": Provider, "auth_index": "idx-other", "status": "error"},
		{"id": "foreign", "name": "foreign.json", "provider": "other", "auth_index": "idx-foreign"},
	}
	host.authRuntime = map[string]map[string]any{
		"idx-primary": {
			"id": id, "name": id + ".json", "provider": Provider, "auth_index": "idx-primary",
			"unavailable": true, "next_retry_after": expired,
			"status_message": "contains sk-primary-secret",
			// The live plugin callback does not return these. They must not become a
			// model cooldown even if a newer host starts sending them undecoded.
			"model_states": map[string]any{"public-model": map[string]any{"unavailable": true}},
		},
		"idx-other": {
			"id": other, "name": other + ".json", "provider": Provider, "auth_index": "idx-other",
			"unavailable": true, "next_retry_after": credentialUntil,
			"status_message": "Error 403: the user is not subscribed to required model plan",
		},
	}
	host.calls = nil
	host.mu.Unlock()

	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/scheduling", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s", status, raw)
	}
	if strings.Contains(raw, "sk-primary-secret") || strings.Contains(raw, "sk-other-secret") {
		t.Fatalf("scheduling response leaked a credential secret: %s", raw)
	}
	if strings.Contains(raw, "foreign") {
		t.Fatalf("scheduling response included a foreign provider: %s", raw)
	}
	accounts := indexAccounts(t, body)
	primary := accounts[id]
	if primary["state"] != "restricted" {
		t.Fatalf("primary state = %#v, want restricted once the retry time has passed", primary["state"])
	}
	if _, ok := primary["cooldowns"]; ok {
		t.Fatalf("plugin view invented cooldowns: %#v", primary["cooldowns"])
	}
	primaryHost := primary["host"].(map[string]any)
	if primaryHost["next_retry_after"] != nil || !strings.Contains(primaryHost["message"].(string), "[redacted]") {
		t.Fatalf("primary host = %#v", primaryHost)
	}
	second := accounts[other]
	if second["state"] != "credential_cooldown" {
		t.Fatalf("other state = %#v", second["state"])
	}
	secondHost := second["host"].(map[string]any)
	if secondHost["next_retry_after"] != credentialUntil.Format(time.RFC3339) {
		t.Fatalf("credential retry = %#v", secondHost)
	}
	if secondHost["message"] != "Error 403: the user is not subscribed to required model plan" {
		t.Fatalf("status message was rewritten: %#v", secondHost["message"])
	}
	if strings.Contains(raw, `"reason"`) {
		t.Fatalf("account view invented a cooldown reason: %s", raw)
	}
	methods := []string{}
	for _, call := range host.calls {
		if call.Method == "host.routing.reset_cooldown" || call.Method == "host.auth.save" {
			methods = append(methods, call.Method)
		}
	}
	if len(methods) != 0 {
		t.Fatalf("scheduling view mutated host state through %v", methods)
	}
}

func TestSchedulingViewKeepsUnknownHostRestricted(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "listed", "sk-listed-secret")
	host.mu.Lock()
	host.authList = []map[string]any{{
		"id": id, "name": id + ".json", "provider": Provider,
		"unavailable": true,
	}}
	host.mu.Unlock()

	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/scheduling", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s", status, raw)
	}
	account := indexAccounts(t, body)[id]
	if account["state"] != "restricted" {
		t.Fatalf("state = %#v, want restricted when unavailable has no retry time", account["state"])
	}
	hostView := account["host"].(map[string]any)
	if hostView["next_retry_after"] != nil {
		t.Fatalf("restricted account invented a retry time: %#v", hostView)
	}
}

func TestSchedulingViewReportsHostUnavailable(t *testing.T) {
	s, host := newTestService(t)
	addCredential(t, s, host, "offline", "sk-offline-secret")
	host.authListErr = errTestHost

	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/scheduling", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s", status, raw)
	}
	hostView := body["host"].(map[string]any)
	if hostView["available"] != false || hostView["note"] == "" {
		t.Fatalf("host = %#v", hostView)
	}
	accounts := body["accounts"].([]any)
	if len(accounts) != 1 || accounts[0].(map[string]any)["state"] != "unknown" {
		t.Fatalf("accounts = %#v", accounts)
	}
}

func indexAccounts(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := body["accounts"].([]any)
	if !ok {
		t.Fatalf("accounts = %#v", body["accounts"])
	}
	out := map[string]map[string]any{}
	for _, item := range raw {
		account := item.(map[string]any)
		out[account["id"].(string)] = account
	}
	return out
}

var errTestHost = errors.New("host auth list failed")
