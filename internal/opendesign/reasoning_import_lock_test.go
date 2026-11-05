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

// A whole-list token plus an explicit original identity lets a normal alias
// rename keep the original row (and its policy when the upstream is unchanged).
// Without the explicit original identity a rename is a new row: the old row
// token has no identity left to match.
func TestImportModelsAliasRenameWithCurrentModelsRevision(t *testing.T) {
	t.Run("explicit original keeps policy", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		renamed := cloneModelRow(t, rows[0])
		renamed["id"] = "renamed-large"
		renamed["original_id"] = "od-large"
		renamed["original_upstream_id"] = "opendesign-large"
		delete(renamed, "reasoning_mapping")
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
			t.Fatalf("rename lost the original policy: %+v", mapping)
		}
	})

	t.Run("explicit mapping is honored", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		renamed := cloneModelRow(t, rows[0])
		renamed["id"] = "renamed-large"
		renamed["original_id"] = "od-large"
		renamed["original_upstream_id"] = "opendesign-large"
		renamed["reasoning_mapping"] = map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "low"}}}
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{renamed}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("rename status = %d, want 200: %s", status, raw)
		}
		if mapping := s.reasoningMappingFor("renamed-large"); mapping == nil || mapping.Rules[0].To != "low" {
			t.Fatalf("explicit mapping was not saved on the renamed row: %+v", mapping)
		}
	})

	t.Run("row token without original treats rename as a new row", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		// Omitting the optional mapping states nothing about the old policy, so the
		// row is a plain new row and the token has nothing left to compare.
		renamed := cloneModelRow(t, rows[0])
		renamed["id"] = "renamed-large"
		delete(renamed, "reasoning_mapping")
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{renamed}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("rename without original identity status = %d, want 200: %s", status, raw)
		}
		if mapping := s.reasoningMappingFor("renamed-large"); mapping != nil {
			t.Fatalf("a row token alone silently carried the policy across a rename: %+v", mapping)
		}
	})

	t.Run("rename plus old alias reuse is a conflicting claim", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		renamed := cloneModelRow(t, rows[0])
		renamed["id"] = "renamed-large"
		renamed["original_id"] = "od-large"
		renamed["original_upstream_id"] = "opendesign-large"
		delete(renamed, "reasoning_mapping")
		reused := map[string]any{"id": "od-large", "upstream_id": "opendesign-large"}
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{renamed, reused}, "expected_models_revision": revision,
		})
		if status != http.StatusConflict {
			t.Fatalf("rename plus alias reuse status = %d, want 409: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 1 || models[0].ID != "od-large" || models[0].Reasoning == nil || models[0].Reasoning.Rules[0].To != "high" {
			t.Fatalf("conflicting claim changed the current row: %+v", models)
		}
	})

	t.Run("current frontend shape can rename as a new row", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, _ := fetchModels(t, s)

		// The current frontend sends id/upstream and the whole-list token only.
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models":                   []map[string]string{{"id": "renamed-large", "upstream_id": "opendesign-large"}},
			"expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("frontend rename status = %d, want 200: %s", status, raw)
		}
		if mapping := s.reasoningMappingFor("renamed-large"); mapping != nil {
			t.Fatalf("a rename without an original identity silently carried a policy: %+v", mapping)
		}
	})
}

// Changing a row's upstream while echoing back the exact stored policy is
// ambiguous: the backend must neither silently migrate it nor silently drop it.
// The caller has to state the intent explicitly.
func TestImportModelsUpstreamChangeRequiresExplicitPolicyIntent(t *testing.T) {
	t.Run("echoed policy without opt-in is rejected", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		switched := cloneModelRow(t, rows[0])
		switched["upstream_id"] = "opendesign-large-v2"
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{switched}, "expected_models_revision": revision,
		})
		if status != http.StatusConflict {
			t.Fatalf("ambiguous upstream switch status = %d, want 409: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 1 || models[0].UpstreamID != "opendesign-large" || models[0].Reasoning == nil || models[0].Reasoning.Rules[0].To != "high" {
			t.Fatalf("rejected switch changed the current row: %+v", models)
		}
	})

	t.Run("explicit delete clears the policy", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		switched := cloneModelRow(t, rows[0])
		switched["upstream_id"] = "opendesign-large-v2"
		delete(switched, "reasoning_mapping")
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{switched}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("upstream switch status = %d, want 200: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 1 || models[0].UpstreamID != "opendesign-large-v2" || models[0].Reasoning != nil {
			t.Fatalf("upstream switch kept the old policy: %+v", models)
		}
	})

	t.Run("explicit opt-out clears the ambiguous policy", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		switched := cloneModelRow(t, rows[0])
		switched["upstream_id"] = "opendesign-large-v2"
		switched["original_id"] = "od-large"
		switched["original_upstream_id"] = "opendesign-large"
		switched["carry_reasoning_mapping"] = false
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{switched}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("explicit opt-out status = %d, want 200: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 1 || models[0].UpstreamID != "opendesign-large-v2" || models[0].Reasoning != nil {
			t.Fatalf("explicit opt-out kept the old policy: %+v", models)
		}
	})

	t.Run("explicit carry opt-in migrates", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		switched := cloneModelRow(t, rows[0])
		switched["upstream_id"] = "opendesign-large-v2"
		switched["original_id"] = "od-large"
		switched["original_upstream_id"] = "opendesign-large"
		switched["carry_reasoning_mapping"] = true
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{switched}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("explicit carry status = %d, want 200: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 1 || models[0].UpstreamID != "opendesign-large-v2" || models[0].Reasoning == nil || models[0].Reasoning.Rules[0].To != "high" {
			t.Fatalf("explicit carry did not keep the policy: %+v", models)
		}
	})

	t.Run("explicit carry opt-in works without echoing the mapping", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		switched := cloneModelRow(t, rows[0])
		switched["upstream_id"] = "opendesign-large-v2"
		switched["original_id"] = "od-large"
		switched["original_upstream_id"] = "opendesign-large"
		switched["carry_reasoning_mapping"] = true
		delete(switched, "reasoning_mapping")
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{switched}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("explicit carry without echo status = %d, want 200: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 1 || models[0].UpstreamID != "opendesign-large-v2" || models[0].Reasoning == nil || models[0].Reasoning.Rules[0].To != "high" {
			t.Fatalf("explicit carry without echo did not keep the policy: %+v", models)
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

	t.Run("padding and case cannot bypass the ambiguity check", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		switched := cloneModelRow(t, rows[0])
		switched["upstream_id"] = "opendesign-large-v2"
		switched["reasoning_mapping"] = map[string]any{"enabled": true, "rules": []map[string]string{{"from": " Medium ", "to": " HIGH "}}}
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{switched}, "expected_models_revision": revision,
		})
		if status != http.StatusConflict {
			t.Fatalf("padded echoed mapping status = %d, want 409: %s", status, raw)
		}
	})

	t.Run("renamed row with original identity and upstream change is ambiguous", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		switched := cloneModelRow(t, rows[0])
		switched["id"] = "renamed-large"
		switched["upstream_id"] = "opendesign-large-v2"
		switched["original_id"] = "od-large"
		switched["original_upstream_id"] = "opendesign-large"
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{switched}, "expected_models_revision": revision,
		})
		if status != http.StatusConflict {
			t.Fatalf("renamed ambiguous switch status = %d, want 409: %s", status, raw)
		}
	})

	t.Run("renamed row with original identity can keep policy on same upstream", func(t *testing.T) {
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)

		renamed := cloneModelRow(t, rows[0])
		renamed["id"] = "renamed-large"
		renamed["original_id"] = "od-large"
		renamed["original_upstream_id"] = "opendesign-large"
		delete(renamed, "reasoning_mapping")
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{renamed}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("rename status = %d, want 200: %s", status, raw)
		}
		if mapping := s.reasoningMappingFor("renamed-large"); mapping == nil || mapping.Rules[0].To != "high" {
			t.Fatalf("rename lost the original policy: %+v", mapping)
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

// Without a whole-list token, the ff3c2c0 row-token semantics stay strict: a
// token for a deleted alias cannot resurrect it, and an explicit upstream change
// with an explicit mapping keeps the legacy last-writer-wins behavior rather
// than being reinterpreted by the new migration guard.
func TestImportModelsLegacyRowTokenCannotResurrectDeletedAlias(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	saveReasoningTarget(t, s, "high")
	_, rows := fetchModels(t, s)
	oldRow := cloneModelRow(t, rows[0])

	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": "od-small", "upstream_id": "opendesign-small"}},
	}); status != http.StatusOK {
		t.Fatalf("delete alias: %d %s", status, raw)
	}

	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []any{oldRow},
	})
	if status != http.StatusConflict {
		t.Fatalf("deleted alias with row token status = %d, want 409: %s", status, raw)
	}
	models := s.config().Models
	if len(models) != 1 || models[0].ID != "od-small" {
		t.Fatalf("legacy row token resurrected a deleted alias: %+v", models)
	}

	// The same old alias reused as a brand-new row without a token is a normal
	// addition and must not inherit the deleted row's policy.
	status, _, raw = managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{
			{"id": "od-small", "upstream_id": "opendesign-small"},
			{"id": "od-large", "upstream_id": "opendesign-large"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("re-add alias without token: %d %s", status, raw)
	}
	if mapping := s.reasoningMappingFor("od-large"); mapping != nil {
		t.Fatalf("reused alias inherited a deleted row's policy: %+v", mapping)
	}

	// A legacy row token for a renamed alias is still stale: without a whole-list
	// snapshot there is no original identity to resolve it against.
	saveReasoningTarget(t, s, "high")
	_, rows = fetchModels(t, s)
	renamed := cloneModelRow(t, rows[1])
	renamed["id"] = "renamed-large"
	status, _, raw = managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []any{renamed},
	})
	if status != http.StatusConflict {
		t.Fatalf("legacy renamed row token status = %d, want 409: %s", status, raw)
	}
}

func TestImportModelsLegacyExplicitMappingOnUpstreamChange(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	saveReasoningTarget(t, s, "high")
	_, rows := fetchModels(t, s)

	// Legacy caller: an explicit mapping wins even when the upstream changes, and
	// padding/case are normalized by validation rather than compared literally.
	switched := cloneModelRow(t, rows[0])
	switched["upstream_id"] = "opendesign-large-v2"
	switched["reasoning_mapping"] = map[string]any{"enabled": true, "rules": []map[string]string{{"from": " Medium ", "to": " HIGH "}}}
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []any{switched},
	})
	if status != http.StatusOK {
		t.Fatalf("legacy explicit upstream change: %d %s", status, raw)
	}
	models := s.config().Models
	if len(models) != 1 || models[0].UpstreamID != "opendesign-large-v2" || models[0].Reasoning == nil {
		t.Fatalf("legacy explicit mapping was not kept: %+v", models)
	}
	if rules := models[0].Reasoning.Rules; len(rules) != 1 || rules[0].From != "medium" || rules[0].To != "high" {
		t.Fatalf("legacy mapping was not normalized: %+v", rules)
	}
}

// Explicit original identities are only valid against the snapshot a whole-list
// token names. A mismatched, partial, duplicated, or conflicting claim is
// rejected instead of guessing which row owns the policy.
func TestImportModelsOriginalIdentityValidation(t *testing.T) {
	seed := func(t *testing.T) (*Service, *fakeHost, string, map[string]any) {
		t.Helper()
		s, host := newTestService(t)
		addCredential(t, s, host, "primary", "sk-original-identity")
		if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []map[string]string{
				{"id": "od-large", "upstream_id": "opendesign-large"},
				{"id": "od-small", "upstream_id": "opendesign-small"},
			},
		}); status != http.StatusOK {
			t.Fatalf("seed models: %d %s", status, raw)
		}
		revision, rows := fetchModels(t, s)
		return s, host, revision, cloneModelRow(t, rows[0])
	}

	t.Run("original identity requires whole-list token", func(t *testing.T) {
		s, _, _, row := seed(t)
		row["original_id"] = "od-large"
		row["original_upstream_id"] = "opendesign-large"
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{"models": []any{row}})
		if status != http.StatusBadRequest {
			t.Fatalf("original identity without token status = %d, want 400: %s", status, raw)
		}
	})

	t.Run("partial original identity", func(t *testing.T) {
		s, _, revision, row := seed(t)
		row["original_id"] = "od-large"
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{row}, "expected_models_revision": revision,
		})
		if status != http.StatusBadRequest {
			t.Fatalf("partial original identity status = %d, want 400: %s", status, raw)
		}
	})

	t.Run("original identity must exist in the snapshot", func(t *testing.T) {
		s, _, revision, row := seed(t)
		row["id"] = "renamed-large"
		row["original_id"] = "missing"
		row["original_upstream_id"] = "opendesign-large"
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{row}, "expected_models_revision": revision,
		})
		if status != http.StatusConflict {
			t.Fatalf("unknown original identity status = %d, want 409: %s", status, raw)
		}
	})

	t.Run("original upstream must match", func(t *testing.T) {
		s, _, revision, row := seed(t)
		row["id"] = "renamed-large"
		row["original_id"] = "od-large"
		row["original_upstream_id"] = "opendesign-other"
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{row}, "expected_models_revision": revision,
		})
		if status != http.StatusConflict {
			t.Fatalf("mismatched original upstream status = %d, want 409: %s", status, raw)
		}
	})

	t.Run("two rows cannot claim the same original", func(t *testing.T) {
		s, _, revision, row := seed(t)
		first := cloneModelRow(t, row)
		first["id"] = "renamed-a"
		first["original_id"] = "od-large"
		first["original_upstream_id"] = "opendesign-large"
		second := cloneModelRow(t, row)
		second["id"] = "renamed-b"
		second["original_id"] = "od-large"
		second["original_upstream_id"] = "opendesign-large"
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{first, second}, "expected_models_revision": revision,
		})
		if status != http.StatusConflict {
			t.Fatalf("duplicate original claim status = %d, want 409: %s", status, raw)
		}
	})

	t.Run("conflicting alias cannot shadow another original", func(t *testing.T) {
		s, _, revision, row := seed(t)
		renamed := cloneModelRow(t, row)
		renamed["id"] = "renamed-large"
		renamed["original_id"] = "od-large"
		renamed["original_upstream_id"] = "opendesign-large"
		conflict := map[string]any{"id": "od-large", "upstream_id": "opendesign-large"}
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{renamed, conflict}, "expected_models_revision": revision,
		})
		if status != http.StatusConflict {
			t.Fatalf("conflicting alias status = %d, want 409: %s", status, raw)
		}
	})

	t.Run("carry opt-in requires an original identity", func(t *testing.T) {
		s, _, revision, row := seed(t)
		row["carry_reasoning_mapping"] = true
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{row}, "expected_models_revision": revision,
		})
		if status != http.StatusBadRequest {
			t.Fatalf("carry without original identity status = %d, want 400: %s", status, raw)
		}
	})
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
// policy change. Without a whole-list token the strict identity check applies to
// every non-empty row token.
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

// A valid whole-list token does not make a stale per-row token acceptable: a
// caller that mixes a fresh list token with an old row snapshot is rejected and
// no partial save occurs.
func TestImportModelsWholeTableTokenStillRejectsStaleRowToken(t *testing.T) {
	s, host := newTestService(t)
	seedModels(t, s, host)
	saveReasoningTarget(t, s, "high")
	_, staleRows := fetchModels(t, s)

	saveReasoningTarget(t, s, "low")
	currentRevision, currentRows := fetchModels(t, s)
	mixed := cloneModelRow(t, staleRows[0])
	mixed["id"] = currentRows[0].(map[string]any)["id"]
	mixed["upstream_id"] = currentRows[0].(map[string]any)["upstream_id"]

	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []any{mixed}, "expected_models_revision": currentRevision,
	})
	if status != http.StatusConflict {
		t.Fatalf("mixed stale row token status = %d, want 409: %s", status, raw)
	}
	if mapping := s.reasoningMappingFor("od-large"); mapping == nil || mapping.Rules[0].To != "low" {
		t.Fatalf("mixed stale row token overwrote the policy: %+v", mapping)
	}
}

// A row that still carries the token of a row it no longer names (the alias was
// renamed) and also submits an explicit reasoning mapping must not be treated as
// an unrelated new row: the mapping may simply be the stored policy echoed back,
// so binding it to a new alias/upstream would silently drop or migrate it. The
// save fails closed with 409. The supported ways forward are the explicit
// original identity (plus carry_reasoning_mapping when the upstream changes) or
// dropping the row token when the row really is new.
func TestImportModelsUnlinkedRowTokenCannotSilentlyRebindPolicy(t *testing.T) {
	seed := func(t *testing.T) (*Service, string, map[string]any) {
		t.Helper()
		s, host := newTestService(t)
		seedModels(t, s, host)
		saveReasoningTarget(t, s, "high")
		revision, rows := fetchModels(t, s)
		return s, revision, cloneModelRow(t, rows[0])
	}

	// The minimal case: the row was renamed and re-pointed, the caller kept the
	// old mapping and the old row token, and stated no original identity.
	t.Run("renamed and re-pointed row with echoed policy is rejected", func(t *testing.T) {
		s, revision, row := seed(t)
		switched := cloneModelRow(t, row)
		switched["id"] = "renamed-large"
		switched["upstream_id"] = "opendesign-large-v2"
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{switched}, "expected_models_revision": revision,
		})
		if status != http.StatusConflict {
			t.Fatalf("unlinked row token with echoed policy status = %d, want 409: %s", status, raw)
		}
		// POST /models reports errors as a redacted message only, so the remedy has
		// to be spelled out there instead of through a separate code field.
		if !strings.Contains(raw, "original_id") {
			t.Fatalf("conflict does not point at the explicit identity remedy: %s", raw)
		}
		models := s.config().Models
		if len(models) != 1 || models[0].ID != "od-large" || models[0].UpstreamID != "opendesign-large" {
			t.Fatalf("rejected save changed the model list: %+v", models)
		}
		if mapping := s.reasoningMappingFor("od-large"); mapping == nil || mapping.Rules[0].To != "high" {
			t.Fatalf("rejected save changed the policy: %+v", mapping)
		}
		if mapping := s.reasoningMappingFor("renamed-large"); mapping != nil {
			t.Fatalf("the old policy was silently bound to the new alias: %+v", mapping)
		}
	})

	t.Run("alias-only rename with explicit policy is rejected", func(t *testing.T) {
		s, revision, row := seed(t)
		renamed := cloneModelRow(t, row)
		renamed["id"] = "renamed-large"
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{renamed}, "expected_models_revision": revision,
		})
		if status != http.StatusConflict {
			t.Fatalf("unlinked row token with explicit policy status = %d, want 409: %s", status, raw)
		}
		if mapping := s.reasoningMappingFor("renamed-large"); mapping != nil {
			t.Fatalf("rejected rename created a new row: %+v", mapping)
		}
	})

	// The reverse must not become a new rule either: an explicit mapping that
	// happens to equal some existing policy is still ambiguous, so it cannot be
	// used to prove intent.
	t.Run("mapping equal to another row's policy is still ambiguous", func(t *testing.T) {
		s, revision, row := seed(t)
		// Add a second row whose policy equals the first one.
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{row, map[string]any{
				"id": "od-small", "upstream_id": "opendesign-small",
				"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "high"}}},
			}},
			"expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("seed second row: %d %s", status, raw)
		}
		revision, rows := fetchModels(t, s)
		renamed := cloneModelRow(t, rows[0])
		renamed["id"] = "renamed-large"
		status, _, raw = managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{renamed, cloneModelRow(t, rows[1])}, "expected_models_revision": revision,
		})
		if status != http.StatusConflict {
			t.Fatalf("surviving row with an equal policy status = %d, want 409: %s", status, raw)
		}
	})

	// Regressions that must keep working: a tokenless row is a normal new row even
	// when its explicit policy equals a stored one, an explicit original identity
	// still carries the policy, and a row that omits the optional fields is added
	// cleanly.
	t.Run("new row without a token is still a normal addition", func(t *testing.T) {
		s, revision, row := seed(t)
		fresh := map[string]any{
			"id": "od-small", "upstream_id": "opendesign-small",
			"reasoning_mapping": map[string]any{"enabled": true, "rules": []map[string]string{{"from": "medium", "to": "high"}}},
		}
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{row, fresh}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("new row with an equal policy status = %d, want 200: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 2 || models[1].ID != "od-small" || models[1].Reasoning == nil || models[1].Reasoning.Rules[0].To != "high" {
			t.Fatalf("new row with an explicit policy was not saved: %+v", models)
		}
	})

	t.Run("new row without optional fields is still a normal addition", func(t *testing.T) {
		s, revision, row := seed(t)
		fresh := map[string]any{"id": "od-small", "upstream_id": "opendesign-small"}
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{row, fresh}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("plain new row status = %d, want 200: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 2 || models[1].ID != "od-small" || models[1].Reasoning != nil {
			t.Fatalf("plain new row was not added cleanly: %+v", models)
		}
		if mapping := s.reasoningMappingFor("od-large"); mapping == nil || mapping.Rules[0].To != "high" {
			t.Fatalf("adding a row dropped the echoed policy: %+v", mapping)
		}
	})

	t.Run("explicit original identity still carries the policy", func(t *testing.T) {
		s, revision, row := seed(t)
		renamed := cloneModelRow(t, row)
		renamed["id"] = "renamed-large"
		renamed["upstream_id"] = "opendesign-large-v2"
		renamed["original_id"] = "od-large"
		renamed["original_upstream_id"] = "opendesign-large"
		renamed["carry_reasoning_mapping"] = true
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
			"models": []any{renamed}, "expected_models_revision": revision,
		})
		if status != http.StatusOK {
			t.Fatalf("explicit carry status = %d, want 200: %s", status, raw)
		}
		models := s.config().Models
		if len(models) != 1 || models[0].ID != "renamed-large" || models[0].UpstreamID != "opendesign-large-v2" {
			t.Fatalf("explicit carry did not rename: %+v", models)
		}
		if mapping := s.reasoningMappingFor("renamed-large"); mapping == nil || mapping.Rules[0].To != "high" {
			t.Fatalf("explicit carry lost the policy: %+v", mapping)
		}
	})
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
