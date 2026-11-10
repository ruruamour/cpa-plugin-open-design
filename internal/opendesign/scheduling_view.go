package opendesign

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// schedulingRuntimeAuth is the routing metadata copied from host.auth.list and
// one host.auth.get_runtime answer. It deliberately has no paths, emails or
// account identifiers: the account card only needs scheduling state.
type schedulingRuntimeAuth struct {
	ID             string                     `json:"id"`
	Name           string                     `json:"name"`
	AuthIndex      string                     `json:"auth_index"`
	Provider       string                     `json:"provider"`
	Type           string                     `json:"type"`
	Status         string                     `json:"status"`
	StatusMessage  string                     `json:"status_message"`
	Disabled       bool                       `json:"disabled"`
	Unavailable    bool                       `json:"unavailable"`
	NextRetryAfter time.Time                  `json:"next_retry_after"`
	Quota          schedulingQuotaState       `json:"quota"`
	ModelStates    map[string]schedulingModel `json:"model_states"`
	RuntimeKnown   bool                       `json:"-"`
}

type schedulingQuotaState struct {
	Exceeded      bool      `json:"exceeded"`
	Reason        string    `json:"reason"`
	NextRecoverAt time.Time `json:"next_recover_at"`
}

type schedulingModel struct {
	Status         string               `json:"status"`
	StatusMessage  string               `json:"status_message"`
	Unavailable    bool                 `json:"unavailable"`
	NextRetryAfter time.Time            `json:"next_retry_after"`
	Quota          schedulingQuotaState `json:"quota"`
}

// schedulingResponse is the read-only view behind the account cards. It never
// resets a cooldown and never writes a credential: CPA owns both.
func (s *Service) schedulingResponse() (any, error) {
	if err := s.begin(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	defer s.done()
	now := time.Now().UTC()
	files, hostErr := s.schedulingRuntimeAuths()
	secrets := s.metadataSecrets()
	s.mu.RLock()
	creds := make([]Credential, 0, len(s.creds))
	authFiles := make(map[string]string, len(s.authFiles))
	for _, credential := range s.creds {
		creds = append(creds, credential)
		authFiles[credential.ID] = s.authFiles[credential.ID]
		secrets = append(secrets, credential.APIKey, credential.ControlKey, credential.WorkspaceID)
	}
	models := append([]Model(nil), s.cfg.Models...)
	s.mu.RUnlock()
	sort.Slice(creds, func(i, j int) bool {
		if creds[i].Label != creds[j].Label {
			return creds[i].Label < creds[j].Label
		}
		return creds[i].ID < creds[j].ID
	})
	accounts := make([]map[string]any, 0, len(creds))
	for _, credential := range creds {
		file := schedulingAuthFor(credential, authFiles[credential.ID], files)
		accounts = append(accounts, schedulingAccountView(credential, file, models, now, secrets...))
	}
	host := map[string]any{"available": hostErr == nil}
	if hostErr != nil {
		host["note"] = "宿主账号状态暂时不可读；额度暂停仍以插件记录为准，冷却以 CPA 实际调度为准。"
	}
	return managementJSON(http.StatusOK, map[string]any{
		"observed_at": now,
		"accounts":    accounts,
		"host":        host,
	})
}

func (s *Service) schedulingRuntimeAuths() ([]schedulingRuntimeAuth, error) {
	var listed struct {
		Files []schedulingRuntimeAuth `json:"files"`
	}
	if err := s.call("host.auth.list", map[string]any{}, &listed); err != nil {
		return nil, err
	}
	files := make([]schedulingRuntimeAuth, 0, len(listed.Files))
	for _, entry := range listed.Files {
		if entry.Provider != Provider && entry.Type != Provider {
			continue
		}
		files = append(files, s.schedulingRuntimeDetail(entry))
	}
	return files, nil
}

// schedulingRuntimeDetail asks for the live auth, which is where model timers
// live. host.auth.list on the current host omits them. A host that has no
// runtime callback, or no index, keeps the list entry and is shown as restricted
// rather than as a fabricated model list.
func (s *Service) schedulingRuntimeDetail(entry schedulingRuntimeAuth) schedulingRuntimeAuth {
	index := strings.TrimSpace(entry.AuthIndex)
	if index == "" {
		return entry
	}
	var response struct {
		Auth schedulingRuntimeAuth `json:"auth"`
	}
	if err := s.call("host.auth.get_runtime", map[string]any{"auth_index": index}, &response); err != nil {
		return entry
	}
	detail := response.Auth
	detail.RuntimeKnown = true
	if detail.ID == "" {
		detail.ID = entry.ID
	}
	if detail.Name == "" {
		detail.Name = entry.Name
	}
	if detail.Provider == "" {
		detail.Provider = entry.Provider
	}
	if detail.AuthIndex == "" {
		detail.AuthIndex = index
	}
	return detail
}

func schedulingAuthFor(credential Credential, configuredName string, files []schedulingRuntimeAuth) *schedulingRuntimeAuth {
	filename := credential.ID + ".json"
	if strings.TrimSpace(configuredName) != "" {
		filename = schedulingBaseName(configuredName)
	}
	for index := range files {
		if files[index].ID == credential.ID || schedulingBaseName(files[index].Name) == filename {
			return &files[index]
		}
	}
	return nil
}

func schedulingBaseName(name string) string {
	name = strings.TrimSpace(name)
	slash := strings.LastIndexAny(name, `/\`)
	if slash >= 0 {
		return name[slash+1:]
	}
	return name
}

func schedulingAccountView(credential Credential, file *schedulingRuntimeAuth, models []Model, now time.Time, secrets ...string) map[string]any {
	view := map[string]any{
		"id":           requestLogText(credential.ID, 128, secrets...),
		"quota_paused": credential.quotaBlocked(),
		"host":         nil,
		"cooldowns":    []map[string]any{},
		"state":        "unknown",
	}
	if file == nil {
		return view
	}
	host := map[string]any{
		"disabled":    file.Disabled,
		"unavailable": file.Unavailable,
		"status":      requestLogText(file.Status, 64, secrets...),
		"message":     requestLogText(file.StatusMessage, 200, secrets...),
	}
	rows := schedulingCooldowns(file, models, now, secrets...)
	if retry := schedulingCredentialRetry(file, now); !retry.IsZero() {
		host["next_retry_after"] = retry.UTC()
	}
	view["host"] = host
	view["cooldowns"] = rows
	view["state"] = schedulingState(credential, file, rows)
	return view
}

// schedulingCooldowns follows the host's own cooldown snapshot: a credential-wide
// quota gate, otherwise one row per model, and a credential row only when the
// runtime has no model states at all. Expired timers are dropped.
func schedulingCooldowns(file *schedulingRuntimeAuth, models []Model, now time.Time, secrets ...string) []map[string]any {
	if file == nil || !file.RuntimeKnown {
		return []map[string]any{}
	}
	rows := []map[string]any{}
	if file.Quota.Exceeded && file.Quota.Reason == "credential_quota" && file.Quota.NextRecoverAt.After(now) {
		rows = append(rows, schedulingCooldownRow("credential", "", "credential_quota", file.Quota.NextRecoverAt, models, secrets...))
		return rows
	}
	if len(file.ModelStates) == 0 {
		if retry := schedulingCredentialRetry(file, now); !retry.IsZero() {
			rows = append(rows, schedulingCooldownRow("credential", "", schedulingReason(file.Quota, file.StatusMessage), retry, models, secrets...))
		}
		return rows
	}
	keys := make([]string, 0, len(file.ModelStates))
	for key := range file.ModelStates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seen := map[string]time.Time{}
	order := []string{}
	for _, key := range keys {
		state := file.ModelStates[key]
		retry := schedulingBlockRetry(state.Unavailable, state.Quota.Exceeded, state.NextRetryAfter, state.Quota.NextRecoverAt, now)
		model := schedulingModelName(key, models, secrets...)
		if retry.IsZero() || model == "" {
			continue
		}
		if _, ok := seen[model]; !ok {
			order = append(order, model)
		}
		if previous, ok := seen[model]; ok && !retry.After(previous) {
			continue
		}
		seen[model] = retry
		rows = append(rows, schedulingCooldownRow("model", model, schedulingReason(state.Quota, state.StatusMessage), retry, models, secrets...))
	}
	compact := make([]map[string]any, 0, len(order))
	for _, model := range order {
		for _, row := range rows {
			if row["model"] == model && row["retry_at"].(time.Time).Equal(seen[model]) {
				compact = append(compact, row)
				break
			}
		}
	}
	return compact
}

func schedulingCredentialRetry(file *schedulingRuntimeAuth, now time.Time) time.Time {
	if file == nil {
		return time.Time{}
	}
	return schedulingBlockRetry(file.Unavailable, file.Quota.Exceeded, file.NextRetryAfter, file.Quota.NextRecoverAt, now)
}

// schedulingBlockRetry copies the host availability gate: neither flag means no
// block, and a recovery time that has already passed means the block expired.
func schedulingBlockRetry(unavailable, quotaExceeded bool, nextRetryAfter, nextRecoverAt, now time.Time) time.Time {
	if !unavailable && !quotaExceeded {
		return time.Time{}
	}
	var next time.Time
	for _, candidate := range []time.Time{nextRetryAfter, nextRecoverAt} {
		if candidate.After(now) && (next.IsZero() || candidate.After(next)) {
			next = candidate
		}
	}
	return next
}

func schedulingReason(quota schedulingQuotaState, statusMessage string) string {
	switch strings.TrimSpace(quota.Reason) {
	case "credential_quota", "quota":
		return quota.Reason
	case "cloudflare challenge":
		return "cloudflare_challenge"
	}
	switch strings.TrimSpace(statusMessage) {
	case "quota", "quota exhausted":
		return "quota"
	case "unauthorized", "payment_required", "not_found", "model_not_supported":
		return strings.TrimSpace(statusMessage)
	case "transient upstream error":
		return "transient_error"
	default:
		return "unknown"
	}
}

func schedulingCooldownRow(scope, model, reason string, retry time.Time, _ []Model, _ ...string) map[string]any {
	if !safeSchedulingReason(reason) {
		reason = "unknown"
	}
	row := map[string]any{
		"scope":    scope,
		"reason":   reason,
		"retry_at": retry.UTC(),
	}
	if scope == "model" {
		row["model"] = model
	}
	return row
}

func safeSchedulingReason(value string) bool {
	if value == "" || len(value) > 40 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func schedulingModelName(key string, models []Model, secrets ...string) string {
	key = strings.TrimSpace(key)
	name := strings.TrimPrefix(key, Provider+"/")
	for _, model := range models {
		if key == model.ID || key == Provider+"/"+model.ID || key == model.UpstreamID {
			name = model.ID
			break
		}
	}
	return requestLogText(name, 128, secrets...)
}

// schedulingState is the badge the card shows. One cooling model does not make
// the whole account unavailable, matching how CPA keeps the other models
// schedulable. Without a runtime model list the account is only "restricted".
func schedulingState(credential Credential, file *schedulingRuntimeAuth, rows []map[string]any) string {
	if credential.Disabled {
		return "manual_disabled"
	}
	if credential.quotaBlocked() {
		return "quota_paused"
	}
	if file == nil {
		return "unknown"
	}
	if file.Disabled {
		return "host_disabled"
	}
	credentialWide, modelScoped := false, false
	for _, row := range rows {
		switch row["scope"] {
		case "credential":
			credentialWide = true
		case "model":
			modelScoped = true
		}
	}
	if credentialWide {
		return "credential_cooldown"
	}
	if modelScoped {
		return "model_cooldown"
	}
	if !file.RuntimeKnown && (file.Unavailable || file.NextRetryAfter.After(time.Now().UTC())) {
		return "restricted"
	}
	if file.Unavailable {
		return "restricted"
	}
	return "available"
}
