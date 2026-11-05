package opendesign

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func saveReasoningTarget(t *testing.T, s *Service, product string) string {
	t.Helper()
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": product}}},
	})
	if status != http.StatusOK {
		t.Fatalf("save reasoning %s: %d %s", product, status, raw)
	}
	revision, _ := body["reasoning_revision"].(string)
	return revision
}

// A model list rendered before another tab saved a newer rule table must not be
// replayed through POST /models. The revision is checked before anything is
// persisted, so a list that also adds a model leaves no partial save behind.
func TestImportModelsRejectsStaleReasoningRevision(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	saveReasoningTarget(t, s, "high")

	status, list, raw := managementCall(t, s, http.MethodGet, apiBase+"/models", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list models: %d %s", status, raw)
	}
	staleRows := append([]any(nil), list["models"].([]any)...)
	if len(staleRows) != 1 {
		t.Fatalf("unexpected model list: %s", raw)
	}

	// Another tab saves a newer policy after the list above was rendered.
	saveReasoningTarget(t, s, "low")

	// Replaying the stale row plus an unrelated new model must be rejected as a
	// whole: the new model does not appear either.
	staleRows = append(staleRows, map[string]any{"id": "od-small", "upstream_id": "opendesign-small"})
	status, _, raw = managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{"models": staleRows})
	if status != http.StatusConflict {
		t.Fatalf("stale import status = %d, want 409: %s", status, raw)
	}

	mapping := s.reasoningMappingFor("od-large")
	if mapping == nil || len(mapping.Rules) != 1 || mapping.Rules[0].To != "low" {
		t.Fatalf("stale import overwrote the newer policy: %+v", mapping)
	}
	cfg := s.config()
	if len(cfg.Models) != 1 || cfg.Models[0].ID != "od-large" || cfg.Models[0].UpstreamID != "opendesign-large" {
		t.Fatalf("stale import partially changed the model list: %+v", cfg.Models)
	}
	onDisk, err := readSettings(filepath.Join(cfg.DataDir, settingsFile))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if len(onDisk.Models) != 1 || onDisk.Models[0].Reasoning == nil || onDisk.Models[0].Reasoning.Rules[0].To != "low" {
		t.Fatalf("stale import reached disk: %+v", onDisk.Models)
	}
}

// A row carrying the current revision is a normal edit: the new mapping in the
// body is persisted even though it differs from the revision's stored policy.
func TestImportModelsCurrentRevisionSavesNewMapping(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	saveReasoningTarget(t, s, "high")

	status, list, raw := managementCall(t, s, http.MethodGet, apiBase+"/models", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list models: %d %s", status, raw)
	}
	row := list["models"].([]any)[0].(map[string]any)
	row["reasoning_mapping"] = map[string]any{
		"enabled": true,
		"rules":   []map[string]string{{"from": "medium", "to": "low"}},
	}
	status, saved, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []any{row},
	})
	if status != http.StatusOK {
		t.Fatalf("fresh import status = %d, want 200: %s", status, raw)
	}
	if saved["models"] == nil {
		t.Fatalf("fresh import returned no model views: %s", raw)
	}
	mapping := s.reasoningMappingFor("od-large")
	if mapping == nil || len(mapping.Rules) != 1 || mapping.Rules[0].To != "low" {
		t.Fatalf("fresh import did not save the new mapping: %+v", mapping)
	}
}

// Rows without a revision are older callers. They keep the previous behavior:
// an omitted mapping preserves the stored policy, while an explicit mapping
// (an old draft) is last-writer-wins.
func TestImportModelsRowsWithoutRevisionKeepLegacySemantics(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	saveReasoningTarget(t, s, "high")

	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]any{{"id": "od-large", "upstream_id": "opendesign-large"}},
	})
	if status != http.StatusOK {
		t.Fatalf("legacy list save: %d %s", status, raw)
	}
	if mapping := s.reasoningMappingFor("od-large"); mapping == nil || mapping.Rules[0].To != "high" {
		t.Fatalf("legacy list save dropped the stored policy: %+v", mapping)
	}

	status, _, raw = managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]any{{
			"id": "od-large", "upstream_id": "opendesign-large",
			"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "max"}}},
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("legacy draft save: %d %s", status, raw)
	}
	if mapping := s.reasoningMappingFor("od-large"); mapping == nil || mapping.Rules[0].To != "max" {
		t.Fatalf("legacy draft mapping was not applied: %+v", mapping)
	}
}

// A revision belongs to the model row it was rendered from. Deleting that row,
// or replaying it after another tab changed the row's upstream, makes the token
// stale instead of letting a stale tab resurrect the row. An intentional
// upstream change submitted with the current revision is allowed, but the old
// policy is not silently carried onto the new upstream.
func TestImportModelsRevisionRejectsDeletedAndChangedRows(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)

	status, list, raw := managementCall(t, s, http.MethodGet, apiBase+"/models", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list models: %d %s", status, raw)
	}
	oldRow := list["models"].([]any)[0].(map[string]any)
	oldRevision, _ := oldRow["reasoning_revision"].(string)
	if oldRevision == "" {
		t.Fatalf("model view has no revision: %s", raw)
	}

	// A deliberate upstream change on the current revision is accepted, but the
	// old policy is not carried onto the new upstream: the keep-existing lookup
	// requires the same upstream ID.
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]any{{
			"id": "od-large", "upstream_id": "opendesign-large-v2", "reasoning_revision": oldRevision,
		}},
	}); status != http.StatusOK {
		t.Fatalf("current-revision upstream change: %d %s", status, raw)
	}
	if models := s.config().Models; len(models) != 1 || models[0].UpstreamID != "opendesign-large-v2" || models[0].Reasoning != nil {
		t.Fatalf("old policy was assigned to the new upstream: %+v", models)
	}

	// Delete od-large through a list that does not mention it.
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "od-small", "upstream_id": "opendesign-small"}},
	}); status != http.StatusOK {
		t.Fatalf("delete model: %d %s", status, raw)
	}
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []any{oldRow},
	}); status != http.StatusConflict {
		t.Fatalf("replaying a deleted row status = %d, want 409: %s", status, raw)
	}
	if models := s.config().Models; len(models) != 1 || models[0].ID != "od-small" {
		t.Fatalf("stale list resurrected a deleted row: %+v", models)
	}

	// Re-add the alias with a different upstream, then replay the old row. Its
	// revision no longer describes the current model, so it must be rejected.
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{
			{"id": "od-large", "upstream_id": "opendesign-large-v2"},
			{"id": "od-small", "upstream_id": "opendesign-small"},
		},
	}); status != http.StatusOK {
		t.Fatalf("re-add changed upstream: %d %s", status, raw)
	}
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []any{oldRow},
	}); status != http.StatusConflict {
		t.Fatalf("stale upstream row status = %d, want 409: %s", status, raw)
	}
	models := s.config().Models
	if len(models) != 2 || models[0].ID != "od-large" || models[0].UpstreamID != "opendesign-large-v2" {
		t.Fatalf("stale upstream row changed the current mapping: %+v", models)
	}
}

// The stale-revision compare runs under configMu. A list rendered before a
// concurrent save must wait for that save to publish, then observe the new
// revision and fail instead of writing its older mapping.
func TestStaleImportWaitsForConcurrentReasoningSave(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	saveReasoningTarget(t, s, "high")

	status, list, raw := managementCall(t, s, http.MethodGet, apiBase+"/models", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list models: %d %s", status, raw)
	}
	staleRows := append([]any(nil), list["models"].([]any)...)

	parked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	hook := func() {
		once.Do(func() {
			close(parked)
			<-release
		})
	}
	testSettingsWriteHook.Store(&hook)
	defer testSettingsWriteHook.Store(nil)

	saveDone := make(chan int, 1)
	go func() {
		status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
			"model": "od-large", "upstream_id": "opendesign-large",
			"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "low"}}},
		})
		saveDone <- status
	}()
	<-parked

	importDone := make(chan int, 1)
	go func() {
		status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{"models": staleRows})
		importDone <- status
	}()
	select {
	case status := <-importDone:
		t.Fatalf("stale import returned %d while the newer save still held configMu", status)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if status := <-saveDone; status != http.StatusOK {
		t.Fatalf("concurrent save status = %d", status)
	}
	if status := <-importDone; status != http.StatusConflict {
		t.Fatalf("stale import status after the save = %d, want 409", status)
	}
	if mapping := s.reasoningMappingFor("od-large"); mapping == nil || mapping.Rules[0].To != "low" {
		t.Fatalf("stale import overwrote the concurrently saved policy: %+v", mapping)
	}
}

// An explicit model test snapshots a nil policy. A concurrent save that adds
// rules must not be read back by the executor: the test keeps sending exactly
// the strength it captured, without applying the newer mapping.
func TestModelTestPinsNilReasoningSnapshotAgainstConcurrentSave(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)

	// The model starts with no policy. Park the upstream call so a save can land
	// after the model test captured its nil reasoning snapshot.
	host.blockCall()
	host.mu.Lock()
	host.doResponses = append(host.doResponses, fakeHTTPResponse{
		Status: http.StatusOK,
		Body:   []byte(`{"model":"opendesign-large","choices":[]}`),
	})
	host.mu.Unlock()

	request := mustJSON(t, ManagementRequest{
		Method: http.MethodPost,
		Path:   apiBase + "/model-test",
		Body:   mustJSON(t, map[string]any{"credential_id": id, "model_id": "od-large", "reasoning_effort": "medium"}),
	})
	resultCh := make(chan ManagementResponse, 1)
	go func() {
		result, err := s.Handle("management.handle", request)
		if err != nil {
			resultCh <- ManagementResponse{StatusCode: http.StatusInternalServerError, Body: []byte(err.Error())}
			return
		}
		resultCh <- result.(ManagementResponse)
	}()
	host.awaitBlocked(t)

	// Now the latest configuration has a rule that would map medium to high.
	saveReasoningTarget(t, s, "high")

	// Release the parked call by cancelling the operation the way quiesce does.
	s.cancelOperations()
	var response ManagementResponse
	select {
	case response = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("model test did not finish after the concurrent save")
	}

	sent := host.lastPayload("host.http.do")
	if sent == nil {
		t.Fatal("model test never reached host.http.do")
	}
	upstreamBody, _ := sent["body"].([]byte)
	if strings.Contains(string(upstreamBody), `"reasoning_effort":"high"`) {
		t.Fatalf("pinned nil snapshot re-read the newer rule table: %s", upstreamBody)
	}
	if !strings.Contains(string(upstreamBody), `"reasoning_effort":"medium"`) {
		t.Fatalf("pinned nil snapshot did not keep the captured strength: %s", upstreamBody)
	}

	var body map[string]any
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatalf("decode model-test body: %v (%s)", err, response.Body)
	}
	if body["reasoning_effort"] == "high" {
		t.Fatalf("model test falsely reported the newer mapping: %s", response.Body)
	}
	if _, exists := body["reasoning_mapping"]; exists {
		t.Fatalf("model test reported a mapping that was not in its snapshot: %s", response.Body)
	}
}
