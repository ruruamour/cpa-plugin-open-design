package opendesign

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// reloadCredentialFromHost replays the body the host last accepted through
// auth.parse. That is how a credential reaches a plugin instance after a hot
// reload, a CPA restart or a watcher callback, and it is the only path on which
// Credential.UnmarshalJSON sees the stored file.
func reloadCredentialFromHost(t *testing.T, s *Service, host *fakeHost, name string) {
	t.Helper()
	body := host.savedAuth(name)
	if len(body) == 0 {
		t.Fatalf("no saved auth body for %s", name)
	}
	if err := os.MkdirAll(host.authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host.authDir, name), body, 0o600); err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(map[string]any{
		"Provider": Provider,
		"FileName": name,
		"RawJSON":  body,
		"Host":     map[string]any{"AuthDir": host.authDir},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Handle("auth.parse", request)
	if err != nil {
		t.Fatalf("auth.parse: %v", err)
	}
	if parsed, _ := result.(map[string]any); parsed["Handled"] != true {
		t.Fatalf("auth.parse did not handle %s: %v", name, result)
	}
}

func savedCredentialField(t *testing.T, host *fakeHost, name, field string) (json.RawMessage, bool) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(host.savedAuth(name), &fields); err != nil {
		t.Fatalf("decode saved auth %s: %v", name, err)
	}
	raw, ok := fields[field]
	return raw, ok
}

func statusCreditsPerUSD(t *testing.T, s *Service, id string) any {
	t.Helper()
	_, body, _ := managementCall(t, s, http.MethodGet, apiBase+"/status", nil, nil)
	return credentialByID(t, body, id)["credits_per_usd"]
}

// rateFixture returns an account whose plan rate is stored and whose live
// record came from the auth file, which is the state every account is in after
// the plugin was reloaded.
func rateFixture(t *testing.T) (*Service, *fakeHost, string, string) {
	t.Helper()
	s, host := newTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	name := id + ".json"
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
		map[string]any{"id": id, "control_key": "fixture-control-key", "workspace_id": "fixture-workspace", "credits_per_usd": "5000"}); status != http.StatusOK {
		t.Fatalf("setting the rate = %d %s", status, raw)
	}
	reloadCredentialFromHost(t, s, host, name)
	if got := statusCreditsPerUSD(t, s, id); got != "5000" {
		t.Fatalf("rate after reload = %#v, want 5000", got)
	}
	return s, host, id, name
}

// Clearing the rate used to answer 200 and show an empty value while the saved
// file kept the old number, which came back on the next reload: the key was
// missing from the known-field list, so the stale copy was re-emitted as if it
// were an operator-added field.
func TestClearingCreditsPerUSDSurvivesReload(t *testing.T) {
	s, host, id, name := rateFixture(t)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
		map[string]any{"id": id, "credits_per_usd": ""}); status != http.StatusOK {
		t.Fatalf("clearing the rate = %d %s", status, raw)
	}
	if raw, ok := savedCredentialField(t, host, name, "credits_per_usd"); ok {
		t.Fatalf("saved file still carries credits_per_usd=%s after the clear", raw)
	}
	reloadCredentialFromHost(t, s, host, name)
	if got := statusCreditsPerUSD(t, s, id); got != "" {
		t.Fatalf("rate after clear and reload = %#v, want empty", got)
	}
	// The billing authorization is a separate choice and must not be touched.
	if raw, ok := savedCredentialField(t, host, name, "control_key"); !ok || string(raw) != `"fixture-control-key"` {
		t.Fatalf("clearing the rate changed control_key: %s (present=%v)", raw, ok)
	}
}

func TestClearBillingDropsCreditsPerUSDOnDisk(t *testing.T) {
	s, host, id, name := rateFixture(t)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
		map[string]any{"id": id, "clear_billing": true}); status != http.StatusOK {
		t.Fatalf("clear_billing = %d %s", status, raw)
	}
	for _, field := range []string{"credits_per_usd", "control_key", "workspace_id"} {
		if raw, ok := savedCredentialField(t, host, name, field); ok {
			t.Fatalf("saved file still carries %s=%s after clear_billing", field, raw)
		}
	}
	reloadCredentialFromHost(t, s, host, name)
	if got := statusCreditsPerUSD(t, s, id); got != "" {
		t.Fatalf("rate after clear_billing and reload = %#v, want empty", got)
	}
	// The inference key is what clear_billing promises to keep.
	if raw, ok := savedCredentialField(t, host, name, "api_key"); !ok || string(raw) != `"fixture-inference-key"` {
		t.Fatalf("clear_billing changed api_key: %s (present=%v)", raw, ok)
	}
}

func TestReplacingCreditsPerUSDSurvivesReload(t *testing.T) {
	s, host, id, name := rateFixture(t)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
		map[string]any{"id": id, "credits_per_usd": "6000.5"}); status != http.StatusOK {
		t.Fatalf("replacing the rate = %d %s", status, raw)
	}
	reloadCredentialFromHost(t, s, host, name)
	if got := statusCreditsPerUSD(t, s, id); got != "6000.5" {
		t.Fatalf("rate after replace and reload = %#v, want 6000.5", got)
	}
}

// The list of keys the struct owns is derived from its tags. This pins the
// derivation itself, so adding a persisted field can never again leave it out
// and make it impossible to clear.
func TestCredentialKnownFieldsCoverEveryPersistedField(t *testing.T) {
	known := map[string]bool{}
	for _, name := range credentialJSONFields {
		if known[name] {
			t.Fatalf("known field %q is listed twice", name)
		}
		known[name] = true
	}
	typ := reflect.TypeOf(Credential{})
	persisted := 0
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !field.IsExported() {
			if name != "" {
				t.Fatalf("unexported field %s carries a json tag", field.Name)
			}
			continue
		}
		if name == "" || name == "-" {
			t.Fatalf("exported field %s has no persisted json name; decide explicitly whether it is stored", field.Name)
		}
		persisted++
		if !known[name] {
			t.Fatalf("persisted field %q is missing from the known-field list", name)
		}
	}
	if persisted != len(credentialJSONFields) {
		t.Fatalf("known-field list has %d entries, struct persists %d", len(credentialJSONFields), persisted)
	}
	for _, name := range []string{"api_key", "control_key", "workspace_id", "credits_per_usd", "quota_block", "request_scoped_errors"} {
		if !known[name] {
			t.Fatalf("expected %q to be a known credential field", name)
		}
	}
}

// Every optional field the struct owns must be clearable: once its value is
// zero, a copy of the same key preserved from an earlier file must not come
// back. Fields the plugin does not own keep round-tripping untouched.
func TestCredentialRoundTripDropsClearedOwnedFieldsOnly(t *testing.T) {
	raw := []byte(`{"type":"open-design","id":"x","label":"l","api_key":"k","control_key":"c","workspace_id":"w","credits_per_usd":"5000","base_url":"https://amr-link.open-design.ai/v1","model_revision":7,"quota_revision":"r","priority":3,"proxy_url":"http://p"}`)
	var c Credential
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c.ControlKey, c.WorkspaceID, c.CreditsPerUSD, c.BaseURL, c.ModelRevision, c.QuotaRevision = "", "", "", "", 0, ""
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, cleared := range []string{"control_key", "workspace_id", "credits_per_usd", "base_url", "model_revision", "quota_revision"} {
		if value, ok := fields[cleared]; ok {
			t.Fatalf("cleared owned field %s came back as %s in %s", cleared, value, encoded)
		}
	}
	for field, want := range map[string]string{"priority": "3", "proxy_url": `"http://p"`, "api_key": `"k"`} {
		if got := string(fields[field]); got != want {
			t.Fatalf("field %s = %s, want %s in %s", field, got, want, encoded)
		}
	}
}
