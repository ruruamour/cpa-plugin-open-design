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
	modelUntil := now.Add(30 * time.Minute).Truncate(time.Second)
	expired := now.Add(-time.Minute).Truncate(time.Second)
	host.mu.Lock()
	host.authList = []map[string]any{
		{"id": id, "name": id + ".json", "provider": Provider, "auth_index": "idx-primary", "status": "active"},
		{"id": other, "name": other + ".json", "provider": Provider, "auth_index": "idx-other", "status": "active"},
		{"id": "foreign", "name": "foreign.json", "provider": "other", "auth_index": "idx-foreign"},
	}
	host.authRuntime = map[string]map[string]any{
		"idx-primary": {
			"id": id, "name": id + ".json", "provider": Provider, "auth_index": "idx-primary",
			"unavailable": false,
			"model_states": map[string]any{
				Provider + "/public-model": map[string]any{
					"unavailable": true, "next_retry_after": modelUntil, "quota": map[string]any{"reason": "quota"},
					"status_message": "quota",
				},
				"public-model": map[string]any{
					"unavailable": true, "next_retry_after": modelUntil.Add(-time.Minute), "quota": map[string]any{"reason": "quota"},
				},
				"old-model":         map[string]any{"unavailable": true, "next_retry_after": expired},
				"sk-primary-secret": map[string]any{"unavailable": true, "next_retry_after": modelUntil, "status_message": "contains sk-primary-secret"},
			},
		},
		"idx-other": {
			"id": other, "name": other + ".json", "provider": Provider, "auth_index": "idx-other",
			"unavailable": true, "next_retry_after": credentialUntil,
			"status_message": "transient upstream error",
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
	if primary["state"] != "model_cooldown" {
		t.Fatalf("primary state = %#v, want model_cooldown", primary["state"])
	}
	cooldowns := primary["cooldowns"].([]any)
	if len(cooldowns) != 2 {
		t.Fatalf("primary cooldowns = %#v, want the live model and the redacted key", cooldowns)
	}
	model := cooldowns[0].(map[string]any)
	if model["scope"] != "model" || model["model"] != "public-model" || model["reason"] != "quota" || model["retry_at"] != modelUntil.Format(time.RFC3339) {
		t.Fatalf("model cooldown = %#v", model)
	}
	secret := cooldowns[1].(map[string]any)
	if secret["model"] != "[redacted]" {
		t.Fatalf("secret model key = %#v", secret)
	}
	second := accounts[other]
	if second["state"] != "credential_cooldown" {
		t.Fatalf("other state = %#v", second["state"])
	}
	rows := second["cooldowns"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["scope"] != "credential" || rows[0].(map[string]any)["reason"] != "transient_error" {
		t.Fatalf("credential cooldown = %#v", rows)
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
		"unavailable": true, "next_retry_after": time.Now().UTC().Add(time.Hour),
	}}
	host.mu.Unlock()

	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/scheduling", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s", status, raw)
	}
	account := indexAccounts(t, body)[id]
	if account["state"] != "restricted" {
		t.Fatalf("state = %#v, want restricted when the runtime callback is unavailable", account["state"])
	}
	if rows := account["cooldowns"].([]any); len(rows) != 0 {
		t.Fatalf("cooldowns = %#v, want none without a runtime model list", rows)
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
