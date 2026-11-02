package opendesign

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestPublicBillingRevisionTracksSecretChangesWithoutPersistingThem(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "account", "inference-revision-fixture")
	readRevision := func(service *Service) string {
		t.Helper()
		status, body, raw := managementCall(t, service, http.MethodGet, apiBase+"/status", nil, nil)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if strings.Contains(raw, "inference-revision-fixture") || strings.Contains(raw, "control-revision-fixture") {
			t.Fatal("public status leaked a fixture secret")
		}
		items := body["credentials"].([]any)
		if len(items) != 1 {
			t.Fatalf("credential count = %d", len(items))
		}
		revision, _ := items[0].(map[string]any)["billing_revision"].(string)
		if revision == "" {
			t.Fatal("missing opaque revision")
		}
		return revision
	}
	initial := readRevision(s)
	update := func(body map[string]any) string {
		t.Helper()
		body["id"] = id
		status, result, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, body)
		if status != http.StatusOK {
			t.Fatalf("update = %d", status)
		}
		revision := readRevision(s)
		if result["billing_revision"] != revision {
			t.Fatal("save response returned an obsolete revision")
		}
		return revision
	}
	if got := update(map[string]any{"label": "renamed"}); got != initial {
		t.Fatal("label edit invalidated unchanged billing credentials")
	}
	control := update(map[string]any{"control_key": "control-revision-fixture"})
	if control == initial {
		t.Fatal("control key replacement did not change revision")
	}
	api := update(map[string]any{"api_key": "replacement-inference-fixture"})
	if api == control {
		t.Fatal("inference key replacement did not change revision")
	}
	workspace := update(map[string]any{"workspace_id": "workspace-revision-fixture"})
	if workspace == api {
		t.Fatal("workspace replacement did not change revision")
	}
	filename := id + ".json"
	disk := host.savedAuth(filename)
	if strings.Contains(string(disk), "billing_revision") || strings.Contains(string(disk), workspace) {
		t.Fatal("ephemeral revision was persisted in auth-dir")
	}
	host.saveAuthErr = errString("fixture persistence failure")
	status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
		map[string]any{"id": id, "control_key": "failed-control-revision-fixture"})
	if status != http.StatusInternalServerError || readRevision(s) != workspace {
		t.Fatal("failed save changed the published revision")
	}
	host.saveAuthErr = nil

	// A new instance has no relationship to a previous page's snapshots even
	// when it loads identical auth bytes, so its identity must also be new.
	restarted, _ := newTestService(t)
	raw, err := json.Marshal(map[string]any{"Provider": Provider, "FileName": filename, "RawJSON": disk})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.parseAuth(raw); err != nil {
		t.Fatal(err)
	}
	if readRevision(restarted) == workspace {
		t.Fatal("restart reused an old cache identity")
	}
}
