package opendesign

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestOptionalBillingCredentialRoundTripAndClear(t *testing.T) {
	s, host := newTestService(t)
	const apiKey, controlKey = "sk-billing-api-0001", "control-billing-secret-0001"
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
		"label": "billing", "api_key": apiKey, "control_key": controlKey, "workspace_id": "ws-1",
	})
	if status != http.StatusCreated || body["billing_configured"] != true || body["workspace_id"] != "ws-1" {
		t.Fatalf("create billing credential: %d %s", status, raw)
	}
	id := body["id"].(string)
	assertStored := func(wantKey, wantWorkspace string) {
		t.Helper()
		var c Credential
		if err := json.Unmarshal(host.savedAuth(id+".json"), &c); err != nil {
			t.Fatal(err)
		}
		if c.APIKey != apiKey || c.ControlKey != wantKey || c.WorkspaceID != wantWorkspace || c.Label != "billing" {
			t.Fatal("billing credential lost fields during auth save")
		}
	}
	assertStored(controlKey, "ws-1")
	for _, update := range []map[string]any{
		{"id": id, "label": "", "api_key": "", "control_key": "", "workspace_id": ""},
		{"id": id, "control_key": nil, "workspace_id": nil, "disabled": false},
	} {
		status, _, raw = managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, update)
		if status != http.StatusOK {
			t.Fatalf("empty update: %d %s", status, raw)
		}
		assertStored(controlKey, "ws-1")
	}
	for _, path := range []string{"/status", "/credentials"} {
		_, result, response := managementCall(t, s, http.MethodGet, apiBase+path, nil, nil)
		if strings.Contains(response, apiKey) || strings.Contains(response, controlKey) || strings.Contains(response, "control_key") {
			t.Fatalf("secret in %s response", path)
		}
		view := result["credentials"].([]any)[0].(map[string]any)
		if view["billing_configured"] != true || view["workspace_id"] != "ws-1" {
			t.Fatalf("billing metadata missing: %s", response)
		}
	}
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
		"id": id, "clear_billing": true, "control_key": "ignored", "workspace_id": "ignored",
	}); status != http.StatusOK {
		t.Fatalf("clear status = %d", status)
	}
	assertStored("", "")
	_, body, _ = managementCall(t, s, http.MethodGet, apiBase+"/credentials", nil, nil)
	view := body["credentials"].([]any)[0].(map[string]any)
	if view["billing_configured"] != false || view["workspace_id"] != "" {
		t.Fatalf("clear metadata = %v", view)
	}
}

func TestBillingFieldsSurviveReentrantParseModelRefreshAndAuthRefresh(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-billing-reentrant-api-0002")
	filename := id + ".json"
	var onDisk map[string]any
	if err := json.Unmarshal(host.savedAuth(filename), &onDisk); err != nil {
		t.Fatal(err)
	}
	onDisk["control_key"], onDisk["workspace_id"] = "control-billing-old-secret", "old-ws"
	onDisk["operator_extension"] = map[string]any{"retained": true}
	stale := mustJSON(t, onDisk)
	host.writeAuthFile(t, filename, stale)
	if _, err := s.parseAuth(mustJSON(t, map[string]any{"FileName": filename, "RawJSON": stale, "Host": map[string]any{"AuthDir": host.authDir}})); err != nil {
		t.Fatal(err)
	}
	var parsed Credential
	host.saveHook = func(name string, raw []byte) {
		out, err := s.parseAuth(mustJSON(t, map[string]any{"FileName": name, "RawJSON": stale, "Host": map[string]any{"AuthDir": host.authDir}}))
		if err != nil {
			t.Errorf("reentrant parse: %v", err)
			return
		}
		auth := out.(map[string]any)["Auth"].(map[string]any)
		if err := json.Unmarshal(auth["StorageJSON"].([]byte), &parsed); err != nil {
			t.Error(err)
		}
	}
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
		"id": id, "control_key": "control-billing-new-secret", "workspace_id": "new-ws",
	})
	if status != http.StatusOK || strings.Contains(raw, "control-billing-") {
		t.Fatalf("replace billing credential: %d %s", status, raw)
	}
	if parsed.ControlKey != "control-billing-new-secret" || parsed.WorkspaceID != "new-ws" || parsed.extraFields["operator_extension"] == nil {
		t.Fatal("reentrant parse trusted stale disk or dropped optional fields")
	}
	if _, err := s.importModels(mustJSON(t, map[string]any{"models": []Model{{ID: "m", UpstreamID: "official-m"}}})); err != nil {
		t.Fatalf("model refresh: %v", err)
	}
	if parsed.ControlKey != "control-billing-new-secret" || parsed.WorkspaceID != "new-ws" || parsed.ModelRevision == 0 {
		t.Fatal("model registration rewrite dropped billing fields")
	}
	out, err := s.refreshAuth(mustJSON(t, map[string]any{"StorageJSON": stale}))
	if err != nil {
		t.Fatal(err)
	}
	var refreshed Credential
	auth := out.(map[string]any)["Auth"].(map[string]any)
	if err := json.Unmarshal(auth["StorageJSON"].([]byte), &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.ControlKey != parsed.ControlKey || refreshed.WorkspaceID != parsed.WorkspaceID {
		t.Fatal("auth.refresh restored old billing fields")
	}
	if _, pending := s.pendingFor(filename, id); pending {
		t.Fatal("pending billing save leaked")
	}
}

func TestBillingKeysAreRedactedIncludingPendingAndUpstreamCode(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-redact-billing-api-0003")
	const control = "plain-control-secret-0003"
	status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": control})
	if status != http.StatusOK {
		t.Fatal("could not save control key")
	}
	message, _ := s.HostErrorDetails(errString("failed " + control))
	if strings.Contains(message, control) {
		t.Fatal("HostErrorDetails leaked control key")
	}
	pending := Credential{Type: Provider, ID: id, APIKey: "pending-api-secret", ControlKey: "pending-control-secret"}
	s.mirrorPending(id+".json", jsonBytes(pending), id)
	message, _ = s.HostErrorDetails(errString("pending-api-secret pending-control-secret"))
	s.clearPending(id+".json", id)
	if strings.Contains(message, "pending-api-secret") || strings.Contains(message, "pending-control-secret") {
		t.Fatal("pending secret was not redacted")
	}
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusUnauthorized, Body: mustJSON(t,
		map[string]any{"error": map[string]any{"code": control, "message": control}})})
	_, _, raw := managementCall(t, s, http.MethodGet, apiBase+"/upstream-models", map[string][]string{"credential_id": {id}}, nil)
	if strings.Contains(raw, control) {
		t.Fatal("upstream error message/code leaked control key")
	}
}

func TestInvalidBillingUpdateDoesNotPersistAndFailureClearsPending(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-invalid-billing-api-0004")
	before := host.countMethod("host.auth.save")
	for _, fields := range []map[string]any{
		{"id": id, "control_key": "bad\r\nheader"},
		{"id": id, "workspace_id": "bad\nworkspace"},
	} {
		if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, fields); status != http.StatusBadRequest {
			t.Fatalf("invalid billing input status = %d", status)
		}
	}
	if host.countMethod("host.auth.save") != before {
		t.Fatal("invalid billing update reached persistence")
	}
	host.saveAuthErr = errString("host failure containing control-failed-save-secret")
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "control_key": "control-failed-save-secret"})
	if status != http.StatusInternalServerError || strings.Contains(raw, "control-failed-save-secret") {
		t.Fatalf("unsafe save failure: %d %s", status, raw)
	}
	if _, pending := s.pendingFor(id+".json", id); pending {
		t.Fatal("failed billing save left pending secret")
	}
}
