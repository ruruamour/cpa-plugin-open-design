package opendesign

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelDiscoverySeparatesUpstreamAuthentication(t *testing.T) {
	for _, upstreamStatus := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(upstreamStatus), func(t *testing.T) {
			s, host := newTestService(t)
			const key = "sk-model-source-secret-0001"
			id := addCredential(t, s, host, "primary", key)
			host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: upstreamStatus,
				Body: []byte(`{"error":{"code":"invalid_api_key","message":"rejected ` + key + `"}}`)})
			status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/upstream-models", map[string][]string{"credential_id": {id}}, nil)
			if status != upstreamStatus || body["source"] != "upstream" || body["code"] != "invalid_api_key" || strings.Contains(raw, key) {
				t.Fatalf("upstream error contract: %d %s", status, raw)
			}
			// A local lookup failure keeps the existing management error shape.
			status, body, raw = managementCall(t, s, http.MethodGet, apiBase+"/upstream-models", map[string][]string{"credential_id": {"missing"}}, nil)
			if status != http.StatusNotFound || body["source"] != nil || body["code"] != nil {
				t.Fatalf("local error contract changed: %d %s", status, raw)
			}
		})
	}
}

func TestPersistedMappingRefreshFailureRemainsVisibleAndRedacted(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-mapping-persist-secret")
	const controlKey = "mapping-control-secret"
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
		map[string]any{"id": id, "control_key": controlKey}); status != http.StatusOK {
		t.Fatal("could not configure control key")
	}
	host.saveAuthErr = errString("host refresh rejected " + controlKey)
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil,
		map[string]any{"models": []Model{{ID: "alias", UpstreamID: "official-model"}}})
	message, _ := body["error"].(string)
	if status != http.StatusBadGateway || !strings.HasPrefix(message, "model mapping saved but the host model refresh failed:") ||
		strings.Contains(raw, controlKey) || body["source"] != nil || body["code"] != nil {
		t.Fatalf("persisted host-refresh failure was swallowed/reclassified/leaked: %d %s", status, raw)
	}
	stored, err := readSettings(filepath.Join(s.config().DataDir, settingsFile))
	if err != nil {
		t.Fatal(err)
	}
	memory := s.config().Models
	if len(stored.Models) != 1 || len(memory) != 1 || stored.Models[0].ID != "alias" || memory[0] != stored.Models[0] {
		t.Fatal("host-refresh failure rolled back the persisted mapping")
	}
}

func TestModelDiscoveryErrorCodeCannotEchoSecretOrArbitraryText(t *testing.T) {
	for _, code := range []string{"sk-model-code-secret-0002", "message with arbitrary text", strings.Repeat("x", 81)} {
		s, host := newTestService(t)
		id := addCredential(t, s, host, "primary", "sk-model-code-secret-0002")
		host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusUnauthorized,
			Body: mustJSON(t, map[string]any{"error": map[string]any{"code": code, "message": "rejected"}})})
		_, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/upstream-models", map[string][]string{"credential_id": {id}}, nil)
		if body["code"] != "upstream_error" || strings.Contains(raw, "sk-model-code-secret-0002") {
			t.Fatalf("unsafe error kind: %s", raw)
		}
	}
}
