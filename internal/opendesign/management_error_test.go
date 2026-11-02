package opendesign

import (
	"net/http"
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
