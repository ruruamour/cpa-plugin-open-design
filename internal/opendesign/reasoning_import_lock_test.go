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

func fetchModels(t *testing.T, s *Service) (string, []any) {
	t.Helper()
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/models", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list models: %d %s", status, raw)
	}
	revision, _ := body["models_revision"].(string)
	if revision == "" {
		t.Fatalf("GET /models has no models_revision: %s", raw)
	}
	rows, _ := body["models"].([]any)
	return revision, rows
}

func cloneModelRow(t *testing.T, row any) map[string]any {
	t.Helper()
	var cloned map[string]any
	if err := json.Unmarshal(mustJSON(t, row), &cloned); err != nil {
		t.Fatalf("clone model row: %v", err)
	}
	return cloned
}

// models_revision is a deterministic digest of the whole list. It is exposed by
// every route that returns the list, changes when a policy or a row changes, and
// stays put when only an unrelated field is echoed back.
func TestModelsRevisionIsExposedAndContentAddressed(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	saveReasoningTarget(t, s, "high")

	first, rows := fetchModels(t, s)
	second, _ := fetchModels(t, s)
	if first != second {
		t.Fatalf("models_revision changed across identical reads: %q then %q", first, second)
	}
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/status", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status: %d %s", status, raw)
	}
	if statusRevision, _ := body["models_revision"].(string); statusRevision != first {
		t.Fatalf("GET /status models_revision = %q, want %q", statusRevision, first)
	}

	// Re-submitting the same list with its current token keeps the digest.
	status, saved, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": rows, "expected_models_revision": first,
	})
	if status != http.StatusOK {
		t.Fatalf("re-submit list: %d %s", status, raw)
	}
	if revision, _ := saved["models_revision"].(string); revision != first {
		t.Fatalf("POST /models response revision = %q, want %q", revision, first)
	}

	// A policy change moves the digest, and the per-model save route reports the
	// new whole-list token too.
	status, reasoning, raw := managementCall(t, s, http.MethodPost, apiBase+"/models/reasoning", nil, map[string]any{
		"model": "od-large", "upstream_id": "opendesign-large",
		"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "low"}}},
	})
	if status != http.StatusOK {
		t.Fatalf("save reasoning: %d %s", status, raw)
	}
	afterPolicy, _ := reasoning["models_revision"].(string)
	current, _ := fetchModels(t, s)
	if afterPolicy == "" || afterPolicy != current || current == first {
		t.Fatalf("models_revision after a policy change: save=%q list=%q old=%q", afterPolicy, current, first)
	}
}

// A list rendered before another tab saved a newer rule table must not be
// replayed through POST /models. The whole-list token is checked before anything
// is persisted, so a list that also adds a model leaves no partial save behind.
func TestImportModelsRejectsStaleModelsRevision(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	saveReasoningTarget(t, s, "high")

	staleRevision, staleRows := fetchModels(t, s)
	if len(staleRows) != 1 {
		t.Fatalf("unexpected model list: %+v", staleRows)
	}

	// Another tab saves a newer policy after the list above was rendered.
	saveReasoningTarget(t, s, "low")

	// Replaying the stale row plus an unrelated new model must be rejected as a
	// whole: the new model does not appear either.
	replayed := append([]any(nil), staleRows...)
	replayed = append(replayed, map[string]any{"id": "od-small", "upstream_id": "opendesign-small"})
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": replayed, "expected_models_revision": staleRevision,
	})
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

	// With the current whole-list token the same edit is a normal save: the new
	// policy and the added row both land.
	currentRevision, currentRows := fetchModels(t, s)
	row := cloneModelRow(t, currentRows[0])
	row["reasoning_mapping"] = map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "max"}}}
	status, saved, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models":                   []any{row, map[string]any{"id": "od-small", "upstream_id": "opendesign-small"}},
		"expected_models_revision": currentRevision,
	})
	if status != http.StatusOK {
		t.Fatalf("fresh import status = %d, want 200: %s", status, raw)
	}
	if revision, _ := saved["models_revision"].(string); revision == "" || revision == currentRevision {
		t.Fatalf("fresh import did not advance models_revision: %q", revision)
	}
	mapping = s.reasoningMappingFor("od-large")
	if mapping == nil || mapping.Rules[0].To != "max" {
		t.Fatalf("fresh import did not save the new mapping: %+v", mapping)
	}
	if models := s.config().Models; len(models) != 2 {
		t.Fatalf("fresh import model count = %d, want 2: %+v", len(models), models)
	}
}

// A stale tab may try to delete a row another tab added, or add a row that no
// longer fits the list. The whole-list token rejects both without touching the
// current configuration.
func TestImportModelsWholeTableRevisionBlocksStaleDeleteAndAdd(t *testing.T) {
	s, host := newTestService(t)
	addCredential(t, s, host, "primary", "sk-models-revision-secret")
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{
			{"id": "od-large", "upstream_id": "opendesign-large"},
			{"id": "od-small", "upstream_id": "opendesign-small"},
		},
	}); status != http.StatusOK {
		t.Fatalf("seed two models: %d %s", status, raw)
	}
	baseRevision, _ := fetchModels(t, s)

	// Another tab adds a third row.
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{
			{"id": "od-large", "upstream_id": "opendesign-large"},
			{"id": "od-small", "upstream_id": "opendesign-small"},
			{"id": "od-medium", "upstream_id": "opendesign-medium"},
		},
		"expected_models_revision": baseRevision,
	}); status != http.StatusOK {
		t.Fatalf("concurrent add: %d %s", status, raw)
	}

	// Stale delete: the old list no longer describes the current one.
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models":                   []map[string]string{{"id": "od-large", "upstream_id": "opendesign-large"}},
		"expected_models_revision": baseRevision,
	})
	if status != http.StatusConflict {
		t.Fatalf("stale delete status = %d, want 409: %s", status, raw)
	}
	if models := s.config().Models; len(models) != 3 {
		t.Fatalf("stale delete changed the list: %+v", models)
	}

	// Stale add: the token no longer matches, so the extra row is not persisted.
	status, _, raw = managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{
			{"id": "od-large", "upstream_id": "opendesign-large"},
			{"id": "od-small", "upstream_id": "opendesign-small"},
			{"id": "od-medium", "upstream_id": "opendesign-medium"},
			{"id": "od-extra", "upstream_id": "opendesign-extra"},
		},
		"expected_models_revision": baseRevision,
	})
	if status != http.StatusConflict {
		t.Fatalf("stale add status = %d, want 409: %s", status, raw)
	}
	if models := s.config().Models; len(models) != 3 {
		t.Fatalf("stale add changed the list: %+v", models)
	}
}

// A whole-list token validates the list, not the row identity, so a normal alias
// rename is allowed. The per-row reasoning_revision is accepted but must not
// mis-reject the new alias.
func TestImportModelsAliasRenameWithCurrentModelsRevision(t *testing.T) {
	t.Run("explicit mapping", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		renamed := cloneModelRow(t, rows[0])
		renamed["id"] = "renamed-large"
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{renamed}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("rename status = %d, want 200: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 1 || models[0].ID != "renamed-large" || models[0].UpstreamID != "opendesign-large" {
			t.Fatalf("rename did not apply: %+v", models)
		}
		if mapping := s.reasoningMappingFor("renamed-large"); mapping == nil || mapping.Rules[0].To != "high" {
			t.Fatalf("rename dropped the explicit mapping: %+v", mapping)
		}
	})

	t.Run("linked by row token", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		renamed := cloneModelRow(t, rows[0])
		renamed["id"] = "renamed-large"
		delete(renamed, "reasoning_mapping")
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{renamed}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("rename status = %d, want 200: %s", status, raw)
		}
		if mapping := s.reasoningMappingFor("renamed-large"); mapping == nil || mapping.Rules[0].To != "high" {
			t.Fatalf("rename lost the policy linked by its row token: %+v", mapping)
		}
	})
}

// Changing a row's upstream must not silently migrate the exact stored policy
// that was echoed back from GET /models. A different policy in the body is an
// explicit new intent and is kept.
func TestImportModelsUpstreamChangeDoesNotMigrateEchoedMapping(t *testing.T) {
	t.Run("echoed policy is dropped", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		switched := cloneModelRow(t, rows[0])
		switched["upstream_id"] = "opendesign-large-v2"
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{switched}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("upstream switch status = %d, want 200: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 1 || models[0].UpstreamID != "opendesign-large-v2" || models[0].Reasoning != nil {
			t.Fatalf("echoed old policy migrated to the new upstream: %+v", models)
		}
	})

	t.Run("different policy is explicit intent", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		switched := cloneModelRow(t, rows[0])
		switched["upstream_id"] = "opendesign-large-v2"
		switched["reasoning_mapping"] = map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "low"}}}
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{switched}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("upstream switch with a new policy status = %d, want 200: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 1 || models[0].UpstreamID != "opendesign-large-v2" || models[0].Reasoning == nil || models[0].Reasoning.Rules[0].To != "low" {
			t.Fatalf("explicit new policy was not saved: %+v", models)
		}
	})

	t.Run("renamed row token prevents migration", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		switched := cloneModelRow(t, rows[0])
		switched["id"] = "renamed-large"
		switched["upstream_id"] = "opendesign-large-v2"
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{switched}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("rename with upstream switch status = %d, want 200: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 1 || models[0].ID != "renamed-large" || models[0].UpstreamID != "opendesign-large-v2" || models[0].Reasoning != nil {
			t.Fatalf("renamed echoed policy migrated to the new upstream: %+v", models)
		}
	})
}

// A caller that omits expected_models_revision keeps the previous behavior: an
// omitted mapping preserves the stored policy, while an explicit mapping is
// last-writer-wins. Unknown row fields are ignored so a future row shape stays
// decodable.
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
			"future_optional":   "ignored",
		}},
		"future_top_level": true,
	})
	if status != http.StatusOK {
		t.Fatalf("legacy draft save: %d %s", status, raw)
	}
	if mapping := s.reasoningMappingFor("od-large"); mapping == nil || mapping.Rules[0].To != "max" {
		t.Fatalf("legacy draft mapping was not applied: %+v", mapping)
	}
}

// The whole-list compare runs under configMu. A list rendered before a
// concurrent save must wait for that save to publish, then observe the new
// revision and fail instead of writing its older mapping.
func TestStaleImportWaitsForConcurrentReasoningSave(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	saveReasoningTarget(t, s, "high")

	staleRevision, staleRows := fetchModels(t, s)

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
		status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": staleRows, "expected_models_revision": staleRevision,
		})
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

// A caller that sends only the per-row reasoning_revision (the shape the first
// optimistic-lock revision introduced) is still protected for a same-alias
// policy change. The row token is ignored only for new or renamed aliases, where
// it has no identity to compare against.
func TestImportModelsSameAliasRowTokenStillGuardsPolicy(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	saveReasoningTarget(t, s, "high")
	_, staleRows := fetchModels(t, s)

	saveReasoningTarget(t, s, "low")
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{"models": staleRows})
	if status != http.StatusConflict {
		t.Fatalf("stale row token status = %d, want 409: %s", status, raw)
	}
	if mapping := s.reasoningMappingFor("od-large"); mapping == nil || mapping.Rules[0].To != "low" {
		t.Fatalf("stale row token overwrote the newer policy: %+v", mapping)
	}
}

// The model-test route itself must pass an explicit pin. This test lands the
// save in the exact snapshot->prepare window through the test hook, so it is the
// route behavior that is checked, not only the executor helper.
func TestModelTestRoutePinsNilReasoningSnapshot(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)

	hook := func() { saveReasoningTarget(t, s, "high") }
	testModelTestBeforeExecute.Store(&hook)
	defer testModelTestBeforeExecute.Store(nil)

	host.mu.Lock()
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)})
	host.mu.Unlock()

	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
		"credential_id": id, "model_id": "od-large", "reasoning_effort": "medium",
	})
	if status != http.StatusOK {
		t.Fatalf("model test: %d %s", status, raw)
	}
	if body["reasoning_effort"] == "high" {
		t.Fatalf("model test read the new policy after its nil snapshot: %s", raw)
	}
	sent := host.lastPayload("host.http.do")
	upstreamBody, _ := sent["body"].([]byte)
	if !strings.Contains(string(upstreamBody), `"reasoning_effort":"medium"`) {
		t.Fatalf("model test sent the wrong strength: %s", upstreamBody)
	}
	if strings.Contains(string(upstreamBody), `"reasoning_effort":"high"`) {
		t.Fatalf("model test applied a policy saved after its snapshot: %s", upstreamBody)
	}
}

// Recreate the exact model-test window deterministically: the snapshot is taken
// while the model has no policy, a save lands before prepare, and the executor
// must still send the pinned nil policy instead of the new rule table.
func TestModelTestPinsNilReasoningSnapshotBetweenSaveAndPrepare(t *testing.T) {
	s, host := newTestService(t)
	id := seedModels(t, s, host)

	snapshot, err := s.modelTestSnapshot(id, "od-large")
	if err != nil {
		t.Fatalf("model test snapshot: %v", err)
	}
	if snapshot.reasoning != nil {
		t.Fatalf("snapshot reasoning = %+v, want nil", snapshot.reasoning)
	}

	// The save lands after the snapshot and before prepare.
	saveReasoningTarget(t, s, "high")

	payload := jsonBytes(map[string]any{
		"messages":         []map[string]any{{"role": "user", "content": "hi"}},
		"reasoning_effort": "medium",
	})
	raw := jsonBytes(ExecutorRequest{
		AuthID: snapshot.credential.ID, AuthProvider: Provider, Model: "od-large",
		Payload: payload, StorageJSON: jsonBytes(snapshot.credential),
	})

	host.mu.Lock()
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)})
	host.mu.Unlock()
	_, decision, err := s.executePinnedWithReasoning(raw, snapshot.upstreamModel, snapshot.reasoning, true, &snapshot.credential, false)
	if err != nil {
		t.Fatalf("pinned nil execute: %v", err)
	}
	sent := host.lastPayload("host.http.do")
	body, _ := sent["body"].([]byte)
	if !strings.Contains(string(body), `"reasoning_effort":"medium"`) {
		t.Fatalf("pinned nil snapshot did not keep the captured strength: %s", body)
	}
	if strings.Contains(string(body), `"reasoning_effort":"high"`) {
		t.Fatalf("pinned nil snapshot re-read the newer rule table: %s", body)
	}
	if decision.mapping != nil || decision.effort != "medium" {
		t.Fatalf("pinned nil decision = %+v, want medium with no mapping", decision)
	}

	// The legacy helper intentionally does not pin the policy, so it keeps
	// resolving the current configuration: that behavior must not regress.
	host.mu.Lock()
	host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusOK, Body: []byte(`{"model":"opendesign-large","choices":[]}`)})
	host.mu.Unlock()
	if _, err := s.executePinned(raw, snapshot.upstreamModel, &snapshot.credential, false); err != nil {
		t.Fatalf("legacy pinned execute: %v", err)
	}
	sent = host.lastPayload("host.http.do")
	body, _ = sent["body"].([]byte)
	if !strings.Contains(string(body), `"reasoning_effort":"high"`) {
		t.Fatalf("legacy helper stopped resolving the current policy: %s", body)
	}
}
