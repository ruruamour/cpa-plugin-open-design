package opendesign

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// modelView is the model shape the console reads. It carries the stored
// thinking-strength policy so the console can render it without a second route,
// plus a stable reasoning_revision token the console echoes back when saving so
// a stale tab is rejected instead of clobbering a newer edit. A model with no
// policy omits the mapping field, which is what tells the console to pre-fill the
// vendor table instead of showing an empty one.
func modelView(m Model) map[string]any {
	view := map[string]any{"id": m.ID, "upstream_id": m.UpstreamID, "reasoning_revision": reasoningRevision(m)}
	if m.Reasoning != nil {
		view["reasoning_mapping"] = m.Reasoning
	}
	return view
}

// modelsRevision is a deterministic digest of the complete model list,
// including every row's optional reasoning policy. The console echoes it back as
// expected_models_revision when it saves the whole list, so a list rendered
// before another tab changed the list (deleted, added, renamed, re-pointed, or
// re-policied a row) is rejected instead of overwriting the newer state. The
// token is opaque and only ever compared for equality.
func modelsRevision(models []Model) string {
	if models == nil {
		models = []Model{}
	}
	// The prefix keeps this digest from ever colliding with another payload that
	// happens to encode to the same bytes.
	payload := append([]byte("open-design/models/v1\x00"), jsonBytes(models)...)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// sameReasoningMapping compares two optional policies by their canonical
// encoding, so two equal mappings with different slice backing are treated as
// the same policy and a nil policy is distinct from an empty one. Rule values
// are normalized first: validation would otherwise canonicalize padding/case
// after the migration decision, letting an equivalent echoed policy look new.
func sameReasoningMapping(left, right *ReasoningMapping) bool {
	return bytes.Equal(jsonBytes(normalizedReasoningMapping(left)), jsonBytes(normalizedReasoningMapping(right)))
}

func modelViews(models []Model) []map[string]any {
	views := make([]map[string]any, 0, len(models))
	for _, m := range models {
		views = append(views, modelView(m))
	}
	return views
}

func sameModels(left, right []Model) bool {
	// Models carry an optional policy object, so a field-by-field comparison
	// would compare pointers and report a change for two identical mappings.
	// The canonical encoding is the same bytes a restart would read back.
	return bytes.Equal(jsonBytes(left), jsonBytes(right))
}

// modelRegistration advertises the mapped models to CPA. It is dynamic on
// purpose: with no credential the list is empty so CPA cannot route a request
// that can never succeed.
func (s *Service) modelRegistration() any {
	cfg := s.config()
	// Without a live credential nothing can serve these models; advertising them
	// would keep them in /v1/models after the last credential is deleted.
	s.mu.RLock()
	hasCredential := len(s.creds) > 0
	s.mu.RUnlock()
	if !hasCredential {
		return map[string]any{"Provider": Provider, "Models": []map[string]any{}}
	}
	models := make([]map[string]any, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		models = append(models, map[string]any{
			"ID":          m.ID,
			"Object":      "model",
			"OwnedBy":     Provider,
			"Name":        m.UpstreamID,
			"DisplayName": m.ID,
			// These aliases and upstream IDs come from the operator's model
			// mapping, not a verified OpenDesign capability catalogue. Marking
			// them user-defined lets CPA forward reasoning parameters for the
			// plugin/upstream to handle without asserting support for any level.
			"UserDefined": true,
		})
	}
	return map[string]any{"Provider": Provider, "Models": models}
}

// saveReasoningMapping replaces one model's thinking-strength policy.
//
// The whole cycle runs under configMu so two concurrent saves cannot interleave
// persist and publish, and a stale tab cannot overwrite a newer edit. The
// mapping only changes what this plugin sends upstream, so unlike a model-list
// change it does not rewrite any auth file: the host's model catalog is
// unaffected.
//
// expected_revision is an optional optimistic-lock token: when supplied it must
// equal the current reasoning revision, so a tab that opened before another save
// is rejected with 409 instead of silently clobbering it. Omitting it keeps the
// old last-writer-wins behavior for existing callers.
func (s *Service) saveReasoningMapping(raw json.RawMessage) (any, error) {
	var in struct {
		Model            string            `json:"model"`
		UpstreamID       string            `json:"upstream_id"`
		Mapping          *ReasoningMapping `json:"reasoning_mapping"`
		ExpectedRevision *string           `json:"expected_revision"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || in.Mapping == nil {
		return nil, localFail(http.StatusBadRequest, "invalid_request", "invalid reasoning mapping JSON")
	}
	model := strings.TrimSpace(in.Model)
	upstream := strings.TrimSpace(in.UpstreamID)
	if model == "" || upstream == "" {
		return nil, localFail(http.StatusBadRequest, "invalid_request", "model and upstream_id are required")
	}
	if err := validateReasoningMapping(in.Mapping); err != nil {
		return nil, err
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	cfg := s.config()
	index := -1
	for i := range cfg.Models {
		if cfg.Models[i].ID == model {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, fail(http.StatusNotFound, "model not found")
	}
	// A mapping written against one upstream ID must not silently follow the
	// alias onto a different model, so the console is told to refresh instead.
	if cfg.Models[index].UpstreamID != upstream {
		return nil, fail(http.StatusConflict, "model upstream changed; refresh before saving")
	}
	// The revision check and the mutation run under the same configMu, so a
	// concurrent save cannot slip between the compare and the publish.
	currentRevision := reasoningRevision(cfg.Models[index])
	if in.ExpectedRevision != nil && *in.ExpectedRevision != "" && *in.ExpectedRevision != currentRevision {
		return nil, localFail(http.StatusConflict, "reasoning_revision_conflict", "reasoning mapping changed since this page was loaded; refresh before saving")
	}
	cfg.Models[index].Reasoning = cloneReasoningMapping(in.Mapping)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := s.saveSettings(cfg); err != nil {
		return nil, fail(http.StatusInternalServerError, "could not persist reasoning mapping")
	}
	s.mu.Lock()
	s.cfg.Models = cfg.Models
	// Results produced under the previous policy no longer describe what this
	// model does, so they are dropped rather than shown as current.
	s.modelMappingRevision++
	s.clearModelTestsLocked()
	s.mu.Unlock()
	return map[string]any{"models": modelViews(cfg.Models), "reasoning_revision": reasoningRevision(cfg.Models[index]), "models_revision": modelsRevision(cfg.Models)}, nil
}

// resolveModel maps a client alias onto the upstream model ID. An unknown alias is
// rejected locally so the request never reaches the network. The lookup shares
// modelSnapshotFor so it always observes one published configuration.
func (s *Service) resolveModel(model string) (string, error) {
	snapshot, ok := s.modelSnapshotFor(model)
	if !ok {
		return "", &APIError{Status: http.StatusBadRequest, Kind: "model_not_enabled", Message: "model is not enabled in the OpenDesign plugin"}
	}
	return snapshot.upstream, nil
}

// discoverModels calls GET /models with the supplied credential. The key comes
// from the stored credential, never from the browser.
func (s *Service) discoverModels(callbackID string, c Credential) ([]UpstreamModel, error) {
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = s.config().BaseURL
	}
	if err := validateBaseURL(baseURL); err != nil {
		return nil, err
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	status, _, body, err := s.doJSON(callbackID, http.MethodGet, upstreamURL(baseURL, "/models", nil), upstreamHeaders(c.APIKey), nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, upstreamErrorFromResponse(status, nil, body, c.APIKey, c.ControlKey)
	}
	return decodeModels(body)
}

// credentialByID returns a live credential or a 404 the UI can display.
func (s *Service) credentialByID(id string) (Credential, error) {
	id = strings.TrimSpace(id)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id == "" {
		// Single-credential installs may omit the parameter.
		if len(s.creds) == 1 {
			for _, c := range s.creds {
				return c, nil
			}
		}
		return Credential{}, fail(http.StatusBadRequest, "credential_id is required")
	}
	c, ok := s.creds[id]
	if !ok {
		return Credential{}, fail(http.StatusNotFound, "credential not found")
	}
	if c.APIKey == "" {
		return Credential{}, fail(http.StatusUnauthorized, "credential has no api key")
	}
	return c, nil
}

// modelImport is one submitted model row. The embedded Model keeps the row
// shape open to future optional fields: anything added to Model later decodes
// and re-encodes through this adapter without an import-side list to update.
//
// reasoning_revision is the per-row token from GET /models. Without
// expected_models_revision it keeps the original strict check: a token must
// still describe the current row with the submitted alias, so a deleted or
// renamed row cannot be revived through a stale token.
//
// original_id/original_upstream_id are the explicit identity of the row in the
// snapshot the caller rendered. They are only meaningful together with a valid
// expected_models_revision; they let a rename keep the original row's policy
// without guessing by alias. carry_reasoning_mapping is the explicit opt-in for
// keeping that policy when the upstream ID itself changes.
type modelImport struct {
	Model
	ReasoningRevision     *string `json:"reasoning_revision"`
	OriginalID            *string `json:"original_id"`
	OriginalUpstreamID    *string `json:"original_upstream_id"`
	CarryReasoningMapping *bool   `json:"carry_reasoning_mapping"`
}

// normalizedReasoningMapping makes a comparison copy of a policy with the same
// canonical rule values cfg.validate would store. Without it, rule values with
// padding or different case could bypass an "is this the old policy?" check and
// silently look like a new intent. Rules are also sorted and an empty list is
// folded to nil, because neither changes what the policy does.
func normalizedReasoningMapping(mapping *ReasoningMapping) *ReasoningMapping {
	if mapping == nil {
		return nil
	}
	copied := cloneReasoningMapping(mapping)
	for i := range copied.Rules {
		copied.Rules[i].From = strings.ToLower(strings.TrimSpace(copied.Rules[i].From))
		copied.Rules[i].To = strings.ToLower(strings.TrimSpace(copied.Rules[i].To))
	}
	if len(copied.Rules) == 0 {
		copied.Rules = nil
		return copied
	}
	sort.Slice(copied.Rules, func(i, j int) bool {
		if copied.Rules[i].From != copied.Rules[j].From {
			return copied.Rules[i].From < copied.Rules[j].From
		}
		return copied.Rules[i].To < copied.Rules[j].To
	})
	return copied
}

// checkLegacyRowRevisions is the ff3c2c0 behavior for callers that do not send
// a whole-list token: every non-empty row token must still describe the current
// row with the submitted alias. A missing alias means the row was deleted or
// renamed, so replaying its token must fail instead of resurrecting it.
func checkLegacyRowRevisions(current []Model, rows []modelImport) error {
	byID := make(map[string]Model, len(current))
	for _, model := range current {
		byID[strings.TrimSpace(model.ID)] = model
	}
	for _, row := range rows {
		if row.ReasoningRevision == nil {
			continue
		}
		token := strings.TrimSpace(*row.ReasoningRevision)
		if token == "" {
			continue
		}
		model, ok := byID[strings.TrimSpace(row.ID)]
		if !ok || reasoningRevision(model) != token {
			return localFail(http.StatusConflict, "reasoning_revision_conflict", "model or reasoning mapping changed since this page was loaded; refresh before saving")
		}
	}
	return nil
}

// checkModelsRevision rejects a submitted list rendered before the current one.
// The caller runs this under configMu, so no save can land between the compare
// and the mutation that follows. The second return value reports whether a
// non-empty whole-list token was supplied and matched.
func checkModelsRevision(current []Model, expected *string) (bool, error) {
	if expected == nil {
		return false, nil
	}
	token := strings.TrimSpace(*expected)
	if token == "" {
		return false, nil
	}
	if token != modelsRevision(current) {
		return false, localFail(http.StatusConflict, "models_revision_conflict", "model list changed since this page was loaded; refresh before saving")
	}
	return true, nil
}

// originalModelForImport resolves the explicit original identity of a row
// against the current snapshot. Both fields are required as a pair, and the
// pair must name exactly one current row: an identity that is missing, partial,
// or borrowed from another upstream is rejected instead of being guessed.
func originalModelForImport(current []Model, row modelImport) (Model, bool, error) {
	hasID := row.OriginalID != nil
	hasUpstream := row.OriginalUpstreamID != nil
	if !hasID && !hasUpstream {
		return Model{}, false, nil
	}
	if !hasID || !hasUpstream {
		return Model{}, false, localFail(http.StatusBadRequest, "invalid_request", "original_id and original_upstream_id must be provided together")
	}
	id := strings.TrimSpace(*row.OriginalID)
	upstream := strings.TrimSpace(*row.OriginalUpstreamID)
	if id == "" || upstream == "" {
		return Model{}, false, localFail(http.StatusBadRequest, "invalid_request", "original_id and original_upstream_id must be nonempty")
	}
	for _, model := range current {
		if strings.TrimSpace(model.ID) == id && strings.TrimSpace(model.UpstreamID) == upstream {
			return model, true, nil
		}
	}
	return Model{}, false, localFail(http.StatusConflict, "original_model_conflict", "original model identity does not match the current model list; refresh before saving")
}

// buildLegacyImportedModels keeps the pre-whole-list-token semantics. A row
// token is strict; an omitted policy is kept only when both alias and upstream
// match, while an explicit policy always wins, including across an upstream
// change. Legacy callers therefore never see the new migration guard.
func buildLegacyImportedModels(current []Model, rows []modelImport) ([]Model, error) {
	if err := checkLegacyRowRevisions(current, rows); err != nil {
		return nil, err
	}
	previousByID := make(map[string]Model, len(current))
	for _, model := range current {
		previousByID[strings.TrimSpace(model.ID)] = model
	}
	models := make([]Model, 0, len(rows))
	for _, imported := range rows {
		model := imported.Model
		model.ID = strings.TrimSpace(model.ID)
		model.UpstreamID = strings.TrimSpace(model.UpstreamID)
		if model.Reasoning == nil {
			if previousModel, ok := previousByID[model.ID]; ok && strings.TrimSpace(previousModel.UpstreamID) == model.UpstreamID {
				model.Reasoning = cloneReasoningMapping(previousModel.Reasoning)
			}
		}
		models = append(models, model)
	}
	return models, nil
}

// validateImportMetadata keeps the explicit original-identity fields scoped to a
// valid whole-list snapshot. Without expected_models_revision there is no
// snapshot to resolve them against, so they are rejected rather than silently
// ignored; the carry opt-in also requires an explicit original identity so it
// can never mean "carry whatever the alias happens to find".
func validateImportMetadata(hasWholeTableToken bool, rows []modelImport) error {
	for _, row := range rows {
		hasOriginal := row.OriginalID != nil || row.OriginalUpstreamID != nil
		hasCarry := row.CarryReasoningMapping != nil && *row.CarryReasoningMapping
		// A false carry flag states no intent and stays harmless for callers that
		// serialize it as a default value. A true flag is an opt-in and therefore
		// needs the whole-list snapshot to resolve the original row.
		if !hasWholeTableToken && (hasOriginal || hasCarry) {
			return localFail(http.StatusBadRequest, "invalid_request", "original model identity requires expected_models_revision")
		}
		if hasCarry && !hasOriginal {
			return localFail(http.StatusBadRequest, "invalid_request", "carry_reasoning_mapping requires original_id and original_upstream_id")
		}
	}
	return nil
}

// buildWholeTableImportedModels resolves explicit original identities, rejects
// stale per-row tokens and conflicting claims, then decides each row's policy.
// The whole-list token has already proved that current is the snapshot the
// caller rendered, so an explicit identity can safely support a rename.
func buildWholeTableImportedModels(current []Model, rows []modelImport) ([]Model, error) {
	byID := make(map[string]Model, len(current))
	for _, model := range current {
		byID[strings.TrimSpace(model.ID)] = model
	}

	originals := make([]Model, len(rows))
	linked := make([]bool, len(rows))
	claimedBy := make(map[string]int, len(rows))
	claimedIDs := make(map[string]bool, len(rows))
	for i := range rows {
		original, ok, err := originalModelForImport(current, rows[i])
		if err != nil {
			return nil, err
		}
		originals[i], linked[i] = original, ok
		if !ok {
			continue
		}
		key := strings.TrimSpace(original.ID) + "\x00" + strings.TrimSpace(original.UpstreamID)
		if _, exists := claimedBy[key]; exists {
			return nil, localFail(http.StatusConflict, "original_model_conflict", "two rows claim the same original model; refresh before saving")
		}
		claimedBy[key] = i
		claimedIDs[strings.TrimSpace(original.ID)] = true
	}

	// The whole-list token has already checked the submitted rows against the
	// snapshot. Row tokens only add detail for a same-alias row; they are ignored
	// for a new or renamed alias, where the explicit original identity (if any) is
	// the sole link to the old row. A token that no longer matches its explicit
	// original is still rejected below.
	for i := range rows {
		if rows[i].ReasoningRevision == nil {
			continue
		}
		token := strings.TrimSpace(*rows[i].ReasoningRevision)
		if token == "" {
			continue
		}
		if linked[i] {
			if reasoningRevision(originals[i]) != token {
				return nil, localFail(http.StatusConflict, "reasoning_revision_conflict", "original model changed since this page was loaded; refresh before saving")
			}
			continue
		}
		model, ok := byID[strings.TrimSpace(rows[i].ID)]
		if !ok {
			// The alias names no current row, so this row's token has nothing left to
			// compare against locally. Without an explicit mapping the row states no
			// policy intent, which is the ordinary new-row shape, so it stays a
			// plain addition.
			//
			// An explicit mapping is different: its content cannot prove intent. It
			// may be the stored policy simply echoed back by a caller that renamed or
			// re-pointed the row, and a rule table can also legitimately equal another
			// row's policy. Silently binding it to a new alias/upstream would then
			// either drop the old policy or migrate it without the caller saying so,
			// so this ambiguous case fails closed. The supported ways forward are an
			// explicit original_id/original_upstream_id (plus carry_reasoning_mapping
			// when the upstream changes) or dropping the row token.
			if rows[i].Reasoning != nil {
				return nil, localFail(http.StatusConflict, "reasoning_mapping_migration_conflict", "row token no longer matches any model while an explicit reasoning mapping was submitted; provide original_id and original_upstream_id, or drop the row token")
			}
			continue
		}
		if reasoningRevision(model) != token {
			return nil, localFail(http.StatusConflict, "reasoning_revision_conflict", "model or reasoning mapping changed since this page was loaded; refresh before saving")
		}
		if claimedIDs[strings.TrimSpace(rows[i].ID)] {
			return nil, localFail(http.StatusConflict, "original_model_conflict", "a row token points at an original model already claimed by another row; refresh before saving")
		}
	}

	// An alias that names another row's original identity could shadow or
	// impersonate the row that legitimately owns that identity. Reject the whole
	// list instead of guessing which row the policy belongs to.
	originalIDs := make(map[string]int, len(claimedBy))
	for key, owner := range claimedBy {
		originalIDs[strings.SplitN(key, "\x00", 2)[0]] = owner
	}
	for i := range rows {
		if owner, exists := originalIDs[strings.TrimSpace(rows[i].ID)]; exists && owner != i {
			return nil, localFail(http.StatusConflict, "original_model_conflict", "a conflicting alias reuses another row's original model; refresh before saving")
		}
	}

	models := make([]Model, 0, len(rows))
	for i, imported := range rows {
		model := imported.Model
		model.ID = strings.TrimSpace(model.ID)
		model.UpstreamID = strings.TrimSpace(model.UpstreamID)
		original, hasOriginal := originals[i], linked[i]
		if !hasOriginal {
			// No explicit original identity: the alias is the identity, unless another
			// row explicitly claimed that same current row. In that case this row is a
			// new row reusing the old alias: it gets no implicit policy, so a rename
			// plus alias reuse (or an alias swap) cannot borrow the old rules.
			if sameAlias, ok := byID[model.ID]; ok && !claimedIDs[model.ID] {
				original, hasOriginal = sameAlias, true
			}
		}
		carry := imported.CarryReasoningMapping
		if carry != nil && *carry && !hasOriginal {
			return nil, localFail(http.StatusBadRequest, "invalid_request", "carry_reasoning_mapping requires an original model identity")
		}
		if !hasOriginal {
			models = append(models, model)
			continue
		}
		sameUpstream := model.UpstreamID == strings.TrimSpace(original.UpstreamID)
		if model.Reasoning == nil {
			if sameUpstream {
				model.Reasoning = cloneReasoningMapping(original.Reasoning)
			} else if carry != nil && *carry {
				// Replacing the upstream clears the policy by default. The explicit
				// opt-in is the only way to carry it, whether the caller echoed the old
				// table or omitted it.
				model.Reasoning = cloneReasoningMapping(original.Reasoning)
			}
			models = append(models, model)
			continue
		}
		// An explicit mapping normally wins. The one ambiguous case is the exact
		// stored policy echoed back together with an upstream change: that must not
		// be silently migrated or silently dropped. A different mapping clearly
		// states a new policy; the same mapping needs the carry opt-in, while an
		// explicit false clears it.
		if !sameUpstream && sameReasoningMapping(model.Reasoning, original.Reasoning) {
			switch {
			case carry == nil:
				return nil, localFail(http.StatusConflict, "reasoning_mapping_migration_conflict", "upstream changed while the old reasoning mapping was echoed back; send a new mapping, set carry_reasoning_mapping, or omit the mapping to clear it")
			case *carry:
				model.Reasoning = cloneReasoningMapping(original.Reasoning)
			default:
				model.Reasoning = nil
			}
		}
		models = append(models, model)
	}
	return models, nil
}

// importModels validates and persists the submitted alias→upstream mapping.
//
// The whole cycle runs under configMu so two concurrent imports cannot interleave
// persist and publish (disk would then hold B while memory held A):
//
//	snapshot -> validate -> persist -> publish
//
// importModels also has to make CPA re-run model.for_auth, otherwise a console
// flow of "add the key first, import models second" leaves the host with the
// empty catalog it registered when the credential appeared, and every request
// fails with model_not_found. CPA only rebuilds a credential's catalog when the
// auth file content changes, so the mapping revision is written into every
// credential through host.auth.save.
func (s *Service) importModels(raw json.RawMessage) (any, error) {
	var in struct {
		Models                 []modelImport `json:"models"`
		ExpectedModelsRevision *string       `json:"expected_models_revision"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fail(http.StatusBadRequest, "invalid models JSON")
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()

	cfg := s.config()
	// The whole-list check rejects delete/add/rename/re-point in one comparison,
	// before anything is built or persisted. Without that token the legacy
	// row-token path below keeps ff3c2c0's strict identity check.
	hasWholeTableToken, err := checkModelsRevision(cfg.Models, in.ExpectedModelsRevision)
	if err != nil {
		return nil, err
	}
	if err := validateImportMetadata(hasWholeTableToken, in.Models); err != nil {
		return nil, err
	}
	var models []Model
	if hasWholeTableToken {
		models, err = buildWholeTableImportedModels(cfg.Models, in.Models)
	} else {
		models, err = buildLegacyImportedModels(cfg.Models, in.Models)
	}
	if err != nil {
		return nil, err
	}
	cfg.Models = models
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := s.saveSettings(cfg); err != nil {
		return nil, fail(http.StatusInternalServerError, "could not persist model mapping")
	}
	s.mu.Lock()
	mappingChanged := !sameModels(s.cfg.Models, cfg.Models)
	s.cfg.Models = cfg.Models
	if mappingChanged {
		s.modelMappingRevision++
		s.clearModelTestsLocked()
	}
	s.mu.Unlock()

	// Publishing the mapping is enough for this plugin's own executor; the extra
	// step below is what makes the host re-register the catalog. A failure here
	// is reported but does not roll back the persisted mapping.
	if err := s.refreshAuthModels(); err != nil {
		return nil, localFail(http.StatusBadGateway, "host_contract_error", "model mapping saved but the host model refresh failed: host rejected the refresh")
	}
	return map[string]any{"models": modelViews(cfg.Models), "models_revision": modelsRevision(cfg.Models)}, nil
}

// refreshAuthModels rewrites every live credential with a new model revision so
// CPA treats the auth file as changed and re-runs model.for_auth for it.
func (s *Service) refreshAuthModels() error {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	s.mu.RLock()
	creds := make([]Credential, 0, len(s.creds))
	for _, c := range s.creds {
		creds = append(creds, c)
	}
	s.mu.RUnlock()
	if len(creds) == 0 {
		return nil
	}
	revision := nextModelRevision()
	var firstErr error
	for _, c := range creds {
		filename := s.fileNameFor(c)
		if filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\`) {
			continue
		}
		c.ModelRevision = revision
		if err := s.saveCredential(c, filename); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s.mu.Lock()
		s.storeCredentialLocked(c)
		s.authFiles[c.ID] = filename
		s.mu.Unlock()
	}
	return firstErr
}

var modelRevisionSeq atomic.Int64

// nextModelRevision returns a strictly increasing revision stamp. A wall clock
// value keeps the file content meaningful when inspected by an operator, and the
// atomic counter guarantees uniqueness for writes inside the same nanosecond.
func nextModelRevision() int64 {
	for {
		now := time.Now().UnixNano()
		last := modelRevisionSeq.Load()
		next := now
		if next <= last {
			next = last + 1
		}
		if modelRevisionSeq.CompareAndSwap(last, next) {
			return next
		}
	}
}
