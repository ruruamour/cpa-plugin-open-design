package opendesign

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestService wires a Service to a fake host and a temp data dir.
func newTestService(t *testing.T) (*Service, *fakeHost) {
	t.Helper()
	host := newFakeHost()
	dir := t.TempDir()
	host.authDir = dir + "/auth"
	s := NewService()
	s.SetHost(host.call)
	registrar(t, s, dir)
	return s, host
}

// registrar configures s with dir as its data directory.
func registrar(t *testing.T, s *Service, dir string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"config_yaml": []byte("data_dir: " + dir + "\n")})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if _, err := s.Handle("plugin.register", raw); err != nil {
		t.Fatalf("plugin.register: %v", err)
	}
}

// callRPC runs one plugin RPC and returns the decoded result.
func callRPC(t *testing.T, s *Service, method string, payload any) any {
	t.Helper()
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s payload: %v", method, err)
		}
		raw = b
	}
	result, err := s.Handle(method, raw)
	if err != nil {
		t.Fatalf("%s returned error: %v", method, err)
	}
	return result
}

// managementCall drives the management handler and returns the response plus its
// decoded JSON body. Non-JSON bodies are returned as raw text.
func managementCall(t *testing.T, s *Service, method, path string, query map[string][]string, body any) (int, map[string]any, string) {
	t.Helper()
	req := ManagementRequest{Method: method, Path: path, Query: query}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal management body: %v", err)
		}
		req.Body = b
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal management request: %v", err)
	}
	result, err := s.Handle("management.handle", raw)
	if err != nil {
		t.Fatalf("management.handle %s %s: %v", method, path, err)
	}
	resp, ok := result.(ManagementResponse)
	if !ok {
		t.Fatalf("management.handle returned %T", result)
	}
	var decoded map[string]any
	if json.Unmarshal(resp.Body, &decoded) != nil {
		return resp.StatusCode, nil, string(resp.Body)
	}
	return resp.StatusCode, decoded, string(resp.Body)
}

func addCredential(t *testing.T, s *Service, host *fakeHost, label, key string) string {
	t.Helper()
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"label": label, "api_key": key})
	if status != http.StatusCreated {
		t.Fatalf("create credential: status %d body %s", status, raw)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("create credential returned no id: %s", raw)
	}
	if strings.Contains(raw, key) {
		t.Fatalf("create response leaked the api key: %s", raw)
	}
	return id
}

func TestRegistrationDeclaresOnlyImplementedCapabilities(t *testing.T) {
	// A fresh instance: re-registering an already configured service with a
	// different data_dir is rejected on purpose.
	s := NewService()
	s.SetHost(newFakeHost().call)
	registrar(t, s, t.TempDir())
	result := callRPC(t, s, "plugin.register", map[string]any{})
	reg, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("registration is %T", result)
	}
	if reg["schema_version"] != 6 {
		t.Fatalf("schema_version = %v, want 6", reg["schema_version"])
	}
	caps := reg["capabilities"].(map[string]any)
	for _, name := range []string{"auth_provider", "model_provider", "executor", "management_api"} {
		if caps[name] != true {
			t.Errorf("capability %s not declared", name)
		}
	}
	for _, name := range []string{"quota_provider", "model_registrar", "scheduler", "model_router", "request_translator"} {
		if _, exists := caps[name]; exists {
			t.Errorf("capability %s declared but not implemented", name)
		}
	}
	meta := reg["metadata"].(map[string]any)
	if meta["Version"] != Version || meta["Name"] != "OpenDesign" {
		t.Fatalf("unexpected metadata: %v", meta)
	}
	if !strings.Contains(meta["GitHubRepository"].(string), "ruruamour") {
		t.Fatalf("repository attribution lost: %v", meta["GitHubRepository"])
	}
}

func TestIdentifiersUseDedicatedProviderAndPluginID(t *testing.T) {
	s, _ := newTestService(t)
	for _, method := range []string{"auth.identifier", "executor.identifier"} {
		got := callRPC(t, s, method, nil).(map[string]any)["identifier"]
		if got != "open-design" {
			t.Fatalf("%s identifier = %v", method, got)
		}
	}
	// The data dir must not collide with the upstream plugin's directory.
	if dir := defaultConfig().DataDir; dir != "plugins/open-design-data" {
		t.Fatalf("default data dir = %q", dir)
	}
}

func TestCredentialRoundTripNeverEchoesKey(t *testing.T) {
	s, host := newTestService(t)
	const key = "sk-amr-test-roundtrip-0001"
	id := addCredential(t, s, host, "primary", key)

	saved := host.savedAuth(id + ".json")
	if len(saved) == 0 {
		t.Fatalf("credential was not written through host.auth.save")
	}
	var stored Credential
	if err := json.Unmarshal(saved, &stored); err != nil {
		t.Fatalf("stored credential is not valid JSON: %v", err)
	}
	if stored.Type != Provider || stored.ID != id || stored.APIKey != key {
		t.Fatalf("stored credential mismatch: %+v", stored)
	}

	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/status", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if strings.Contains(raw, key) {
		t.Fatalf("status leaked the api key: %s", raw)
	}
	creds := body["credentials"].([]any)
	if len(creds) != 1 {
		t.Fatalf("status credentials = %v", creds)
	}
	entry := creds[0].(map[string]any)
	if entry["id"] != id || entry["label"] != "primary" || entry["disabled"] != false {
		t.Fatalf("status credential entry = %v", entry)
	}
	if _, exists := entry["api_key"]; exists {
		t.Fatalf("status entry exposes api_key")
	}
}

func TestTwoCredentialsDoNotOverwriteEachOther(t *testing.T) {
	s, host := newTestService(t)
	first := addCredential(t, s, host, "alpha", "sk-amr-alpha-0001")
	second := addCredential(t, s, host, "beta", "sk-amr-beta-0002")
	if first == second {
		t.Fatalf("credential ids collided")
	}
	if len(host.savedAuths) != 2 {
		t.Fatalf("expected 2 saved auth files, got %d", len(host.savedAuths))
	}
	// Updating one must not touch the other's file.
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": first, "label": "alpha-renamed"})
	if status != http.StatusOK {
		t.Fatalf("update status = %d body %s", status, raw)
	}
	if body["label"] != "alpha-renamed" {
		t.Fatalf("update did not apply: %s", raw)
	}
	var alpha Credential
	if err := json.Unmarshal(host.savedAuth(first+".json"), &alpha); err != nil {
		t.Fatalf("decode alpha: %v", err)
	}
	var beta Credential
	if err := json.Unmarshal(host.savedAuth(second+".json"), &beta); err != nil {
		t.Fatalf("decode beta: %v", err)
	}
	if alpha.APIKey != "sk-amr-alpha-0001" || beta.APIKey != "sk-amr-beta-0002" {
		t.Fatalf("keys crossed over: alpha=%q beta=%q", alpha.APIKey, beta.APIKey)
	}
}

func TestCredentialDisableAndDelete(t *testing.T) {
	s, _ := newTestService(t)
	id := addCredential(t, s, newFakeHost(), "gamma", "sk-amr-gamma-0003")
	status, body, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "disabled": true})
	if status != http.StatusOK || body["disabled"] != true {
		t.Fatalf("disable failed: %d %v", status, body)
	}
	// Deleting without a resolved auth dir must not silently succeed.
	status, _, _ = managementCall(t, s, http.MethodDelete, apiBase+"/credentials", map[string][]string{"id": {id}}, nil)
	if status == http.StatusOK {
		t.Fatalf("delete succeeded without an auth file on disk")
	}
}

func TestUnknownCredentialIs404(t *testing.T) {
	s, _ := newTestService(t)
	status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": "missing", "label": "x"})
	if status != http.StatusNotFound {
		t.Fatalf("unknown credential update status = %d", status)
	}
	status, _, _ = managementCall(t, s, http.MethodDelete, apiBase+"/credentials", map[string][]string{"id": {"missing"}}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown credential delete status = %d", status)
	}
}

func TestUpstreamModelDiscoveryAndImport(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-discovery-0004")
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"object":"list","data":[{"id":"opendesign-large","object":"model","owned_by":"open-design"},{"id":"opendesign-small"},{"id":"opendesign-large"}]}`),
	})

	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/upstream-models", map[string][]string{"credential_id": {id}}, nil)
	if status != http.StatusOK {
		t.Fatalf("upstream-models status = %d body %s", status, raw)
	}
	data := body["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("expected deduplicated 2 models, got %v", data)
	}
	first := data[0].(map[string]any)
	if first["id"] != "opendesign-large" || first["owned_by"] != "open-design" {
		t.Fatalf("model entry = %v", first)
	}

	// The key must travel in the Authorization header and nowhere else.
	sent := host.lastPayload("host.http.do")
	url, _ := sent["url"].(string)
	if !strings.HasPrefix(url, DefaultBaseURL+"/models") {
		t.Fatalf("discovery url = %q", url)
	}
	headers, _ := sent["headers"].(http.Header)
	if headers.Get("Authorization") != "Bearer sk-amr-discovery-0004" {
		t.Fatalf("authorization header = %q", headers.Get("Authorization"))
	}

	// Import maps aliases onto upstream ids and survives a reload.
	importBody := map[string]any{"models": []map[string]string{
		{"id": "large", "upstream_id": "opendesign-large"},
		{"id": "small", "upstream_id": "opendesign-small"},
	}}
	status, body, raw = managementCall(t, s, http.MethodPost, apiBase+"/models", nil, importBody)
	if status != http.StatusOK {
		t.Fatalf("models import status = %d body %s", status, raw)
	}
	models := body["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("import returned %v", models)
	}

	status, body, _ = managementCall(t, s, http.MethodGet, apiBase+"/status", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status after import = %d", status)
	}
	listed := body["models"].([]any)
	if len(listed) != 2 || listed[0].(map[string]any)["upstream_id"] != "opendesign-large" {
		t.Fatalf("status models = %v", listed)
	}

	// A fresh instance reading the same data dir sees the same mapping.
	reloaded := NewService()
	reloaded.SetHost(host.call)
	cfgYAML := []byte("data_dir: " + s.config().DataDir + "\n")
	rawCfg, _ := json.Marshal(map[string]any{"config_yaml": cfgYAML})
	if _, err := reloaded.Handle("plugin.reconfigure", rawCfg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.config().Models; len(got) != 2 || got[1].UpstreamID != "opendesign-small" {
		t.Fatalf("reloaded models = %v", got)
	}
}

func TestImportRejectsDuplicateAliases(t *testing.T) {
	s, _ := newTestService(t)
	status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{"models": []map[string]string{
		{"id": "dup", "upstream_id": "a"},
		{"id": "dup", "upstream_id": "b"},
	}})
	if status != http.StatusBadRequest {
		t.Fatalf("duplicate alias import status = %d", status)
	}
}

func TestUpstreamDiscoveryPropagates401WithoutLeakingKey(t *testing.T) {
	s, host := newTestService(t)
	const key = "sk-amr-unauthorized-0005"
	id := addCredential(t, s, host, "bad", key)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusUnauthorized,
		Body:   []byte(`{"error":{"message":"invalid_api_key: api key not found"}}`),
	})
	status, _, raw := managementCall(t, s, http.MethodGet, apiBase+"/upstream-models", map[string][]string{"credential_id": {id}}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if strings.Contains(raw, key) {
		t.Fatalf("error body leaked the key: %s", raw)
	}
	if !strings.Contains(raw, "invalid_api_key") {
		t.Fatalf("error body lost the upstream code: %s", raw)
	}
}

func TestUpstreamModelsRejectsUnknownCredential(t *testing.T) {
	s, _ := newTestService(t)
	status, _, _ := managementCall(t, s, http.MethodGet, apiBase+"/upstream-models", map[string][]string{"credential_id": {"nope"}}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	status, _, _ = managementCall(t, s, http.MethodGet, apiBase+"/upstream-models", nil, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status without credential_id = %d, want 400", status)
	}
}

func TestManagementRoutesAreExactPaths(t *testing.T) {
	s, _ := newTestService(t)
	result := callRPC(t, s, "management.register", nil)
	reg := result.(map[string]any)
	routes := reg["routes"].([]map[string]string)
	seen := map[string]bool{}
	for _, r := range routes {
		path := r["Path"]
		if strings.ContainsAny(path, ":*{}") {
			t.Fatalf("route %q uses a path parameter CPA cannot dispatch", path)
		}
		key := r["Method"] + " " + path
		if seen[key] {
			t.Fatalf("duplicate route %s", key)
		}
		seen[key] = true
	}
	for _, want := range []string{
		"GET " + apiBase + "/status",
		"POST " + apiBase + "/credentials",
		"DELETE " + apiBase + "/credentials",
		"GET " + apiBase + "/upstream-models",
		"POST " + apiBase + "/models",
	} {
		if !seen[want] {
			t.Errorf("missing route %s", want)
		}
	}
	resources := reg["resources"].([]map[string]string)
	if len(resources) != 1 || resources[0]["Path"] != consoleResource {
		t.Fatalf("resources = %v", resources)
	}
}

func TestConsolePageIsServed(t *testing.T) {
	s, _ := newTestService(t)
	for _, path := range []string{consoleResource, consolePath} {
		status, _, raw := managementCall(t, s, http.MethodGet, path, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("%s status = %d", path, status)
		}
		if !strings.Contains(raw, "<html") {
			t.Fatalf("%s did not return HTML: %.120s", path, raw)
		}
	}
}

func TestUnknownModelIsRejectedBeforeAnyNetworkCall(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "sk-amr-modelcheck-0006")
	host.doResponses = nil
	raw, _ := json.Marshal(executorPayload{AuthID: id, Model: "not-mapped", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
	_, err := s.Handle("executor.execute", raw)
	if err == nil {
		t.Fatalf("unknown model was accepted")
	}
	if status := statusOf(err); status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if host.countMethod("host.http.do") != 0 {
		t.Fatalf("unknown model reached the network")
	}
}

func TestExecutorRequiresCredential(t *testing.T) {
	s, host := newTestService(t)
	raw, _ := json.Marshal(executorPayload{Model: "m", Payload: []byte(`{"messages":[]}`)})
	_, err := s.Handle("executor.execute", raw)
	if err == nil {
		t.Fatalf("execute without a credential succeeded")
	}
	if status := statusOf(err); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if host.countMethod("host.http.do") != 0 {
		t.Fatalf("credential-less request reached the network")
	}
}

// executorPayload keeps the RPC JSON field names in one place for the tests.
type executorPayload struct {
	AuthID      string          `json:"AuthID"`
	Model       string          `json:"Model"`
	Format      string          `json:"Format"`
	Stream      bool            `json:"Stream"`
	StreamID    string          `json:"stream_id"`
	AuthMeta    json.RawMessage `json:"-"`
	Payload     []byte          `json:"Payload"`
	StorageJSON []byte          `json:"StorageJSON"`
	Metadata    map[string]any  `json:"Metadata"`
}

var _ = bytes.Equal
var _ = errors.Is

// CPA dispatches management requests by exact "METHOD path", so credential
// actions use the collection route: id in the body for updates, ?id= for deletes.
func TestCredentialRoutesUseCollectionPath(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "delta", "sk-amr-delta-0007")

	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", map[string][]string{"id": {id}}, map[string]any{"disabled": true})
	if status != http.StatusOK {
		t.Fatalf("update status = %d body %s", status, raw)
	}
	if body["disabled"] != true {
		t.Fatalf("update did not disable: %s", raw)
	}
	if _, exists := body["api_key"]; exists {
		t.Fatalf("update echoed api_key")
	}

	// The id in the query string is also accepted for updates.
	status, body, raw = managementCall(t, s, http.MethodPost, apiBase+"/credentials", map[string][]string{"id": {id}}, map[string]any{"label": "delta-2"})
	if status != http.StatusOK || body["label"] != "delta-2" {
		t.Fatalf("query-id update failed: %d %s", status, raw)
	}

	// Unknown ids are 404, never a silent create.
	status, _, _ = managementCall(t, s, http.MethodDelete, apiBase+"/credentials", map[string][]string{"id": {"unknown"}}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown delete status = %d", status)
	}
	// A path segment is not a registered route and must not be faked locally.
	status, _, _ = managementCall(t, s, http.MethodDelete, apiBase+"/credentials/"+id, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("path-style delete status = %d, want 404 from the route table", status)
	}
}

// status must report a boolean disabled flag for every credential.
func TestStatusDisabledIsBoolean(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "epsilon", "sk-amr-epsilon-0008")
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "disabled": true}); status != http.StatusOK {
		t.Fatalf("disable: %d %s", status, raw)
	}
	_, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/status", nil, nil)
	creds := body["credentials"].([]any)
	if len(creds) != 1 {
		t.Fatalf("credentials = %v", creds)
	}
	entry := creds[0].(map[string]any)
	value, ok := entry["disabled"].(bool)
	if !ok || !value {
		t.Fatalf("disabled is %T/%v in %s", entry["disabled"], entry["disabled"], raw)
	}
}

// model.for_auth must reflect the mapping saved through POST /models.
func TestModelForAuthReflectsSavedMapping(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "zeta", "sk-amr-zeta-0009")
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{"models": []map[string]string{
		{"id": "alias-a", "upstream_id": "od/a"},
	}}); status != http.StatusOK {
		t.Fatalf("import: %d %s", status, raw)
	}
	for _, method := range []string{"model.for_auth", "model.static"} {
		raw, _ := json.Marshal(map[string]any{"AuthID": id, "AuthProvider": Provider})
		result, err := s.Handle(method, raw)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		reg := result.(map[string]any)
		if reg["Provider"] != Provider {
			t.Fatalf("%s provider = %v", method, reg["Provider"])
		}
		models := reg["Models"].([]map[string]any)
		if len(models) != 1 || models[0]["ID"] != "alias-a" || models[0]["Name"] != "od/a" {
			t.Fatalf("%s models = %v", method, models)
		}
	}
}

// Auth files written by the operator are parsed and never echo the key back in
// the status view.
func TestParseAuthHandlesExternalFile(t *testing.T) {
	s, host := newTestService(t)
	dir := s.config().DataDir + "/auth"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const key = "sk-amr-external-0010"
	cred := Credential{Type: Provider, ID: "open-design-external", Label: "external", APIKey: key, BaseURL: DefaultBaseURL}
	rawBody, _ := json.Marshal(cred)
	file := "open-design-external.json"
	if err := os.WriteFile(filepath.Join(dir, file), rawBody, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	request, _ := json.Marshal(map[string]any{
		"Provider": Provider,
		"FileName": file,
		"RawJSON":  rawBody,
		"Host":     map[string]any{"AuthDir": dir},
	})
	result, err := s.Handle("auth.parse", request)
	if err != nil {
		t.Fatalf("auth.parse: %v", err)
	}
	parsed := result.(map[string]any)
	if parsed["Handled"] != true {
		t.Fatalf("auth.parse did not handle the file: %v", parsed)
	}
	auth := parsed["Auth"].(map[string]any)
	if auth["Provider"] != Provider || auth["ID"] != "open-design-external" {
		t.Fatalf("auth record = %v", auth)
	}
	storage, _ := auth["StorageJSON"].([]byte)
	if len(storage) == 0 {
		t.Fatalf("StorageJSON missing")
	}
	_, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/status", nil, nil)
	if strings.Contains(raw, key) {
		t.Fatalf("status leaked an external key: %s", raw)
	}
	if len(body["credentials"].([]any)) != 1 {
		t.Fatalf("external credential not listed: %s", raw)
	}
	host.mu.Lock()
	saved := len(host.savedAuths)
	host.mu.Unlock()
	if saved != 0 {
		t.Fatalf("parsing an external file rewrote it through host.auth.save")
	}
}

// After a host restart the plugin may hold no in-memory record yet, so execute
// must resolve the credential from the StorageJSON the host passes back.
func TestExecuteResolvesCredentialFromStorageJSON(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)})
	storage, _ := json.Marshal(Credential{Type: Provider, ID: id, Label: "primary", APIKey: "sk-amr-exec-0001", BaseURL: DefaultBaseURL})
	raw, _ := json.Marshal(map[string]any{
		"AuthID":      id,
		"Model":       "od-large",
		"Payload":     []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		"StorageJSON": storage,
	})
	if _, err := s.Handle("executor.execute", raw); err != nil {
		t.Fatalf("execute with StorageJSON: %v", err)
	}
	headers, _ := host.lastPayload("host.http.do")["headers"].(http.Header)
	if headers.Get("Authorization") != "Bearer sk-amr-exec-0001" {
		t.Fatalf("authorization = %q", headers.Get("Authorization"))
	}
}

// A static API key never rotates, so refresh returns the same record.
func TestAuthRefreshReturnsStableRecord(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "eta", "sk-amr-eta-0011")
	storage, _ := json.Marshal(Credential{Type: Provider, ID: id})
	raw, _ := json.Marshal(map[string]any{"StorageJSON": storage})
	result, err := s.Handle("auth.refresh", raw)
	if err != nil {
		t.Fatalf("auth.refresh: %v", err)
	}
	out := result.(map[string]any)
	auth := out["Auth"].(map[string]any)
	if auth["ID"] != id || auth["Provider"] != Provider {
		t.Fatalf("refreshed auth = %v", auth)
	}
	if out["NextRefreshAfter"] == nil {
		t.Fatalf("NextRefreshAfter missing")
	}
	// An unknown credential must not be invented from a stale snapshot.
	unknownStorage, _ := json.Marshal(Credential{Type: Provider, ID: "nope", APIKey: "sk-should-not-appear"})
	refreshed, err := s.Handle("auth.refresh", mustJSON(t, map[string]any{"StorageJSON": unknownStorage}))
	if err != nil {
		t.Fatalf("auth.refresh: %v", err)
	}
	empty := refreshed.(map[string]any)["Auth"].(map[string]any)
	if len(empty) != 0 {
		t.Fatalf("refresh invented a credential: %v", empty)
	}
}

// A quiesced plugin refuses new work but can be re-registered by the host.
func TestQuiesceThenReRegister(t *testing.T) {
	s, _ := newTestService(t)
	if _, err := s.Handle("plugin.quiesce", nil); err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	if _, err := s.Handle("executor.execute", mustJSON(t, executorPayload{Model: "m"})); err == nil {
		t.Fatalf("execute accepted work after quiesce")
	}
	// Re-registering the same data dir revives the instance.
	registrar(t, s, s.config().DataDir)
	if _, err := s.Handle("plugin.register", mustJSON(t, map[string]any{})); err != nil {
		t.Fatalf("register after quiesce: %v", err)
	}
}

// Wrong methods and unknown paths must not mutate state.
func TestManagementRejectsUnknownRoutes(t *testing.T) {
	s, _ := newTestService(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, apiBase + "/credentials"},
		{http.MethodDelete, apiBase + "/models"},
		{http.MethodGet, apiBase + "/secrets"},
		{http.MethodPost, apiBase + "/upstream-models"},
	} {
		status, _, _ := managementCall(t, s, tc.method, tc.path, nil, map[string]any{})
		if status != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", tc.method, tc.path, status)
		}
	}
}

// The console session probe exists so a bad management key yields 401 from the
// auth middleware rather than a confusing 404.
func TestConsoleSessionProbe(t *testing.T) {
	s, _ := newTestService(t)
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/console/session", nil, nil)
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("session probe: %d %s", status, raw)
	}
}
