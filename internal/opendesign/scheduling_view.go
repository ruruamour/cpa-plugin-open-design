package opendesign

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// schedulingRuntimeAuth copies only the fields the public plugin callbacks
// actually return. The current host's HostAuthFileEntry has no model_states,
// last_error or cooldowns, so this view must not invent a model list from them.
type schedulingRuntimeAuth struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	AuthIndex      string    `json:"auth_index"`
	Provider       string    `json:"provider"`
	Type           string    `json:"type"`
	Status         string    `json:"status"`
	StatusMessage  string    `json:"status_message"`
	Disabled       bool      `json:"disabled"`
	Unavailable    bool      `json:"unavailable"`
	NextRetryAfter time.Time `json:"next_retry_after"`
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
		accounts = append(accounts, schedulingAccountView(credential, file, now, secrets...))
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

// schedulingRuntimeDetail prefers the live auth when the host has an index.
// Both callbacks expose the same account-level fields on the current host, so a
// missing runtime answer keeps the list entry instead of failing the whole view.
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

func schedulingAccountView(credential Credential, file *schedulingRuntimeAuth, now time.Time, secrets ...string) map[string]any {
	view := map[string]any{
		"id":           requestLogText(credential.ID, 128, secrets...),
		"quota_paused": credential.quotaBlocked(),
		"host":         nil,
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
	if file.Unavailable && file.NextRetryAfter.After(now) {
		host["next_retry_after"] = file.NextRetryAfter.UTC()
	}
	view["host"] = host
	view["state"] = schedulingState(credential, file, now)
	return view
}

// schedulingState only uses account-level flags. A future retry with
// unavailable is an account cooldown; unavailable without one is restricted,
// because the plugin callback cannot say which model is cooling. An arbitrary
// status message is never turned into a cooldown reason.
func schedulingState(credential Credential, file *schedulingRuntimeAuth, now time.Time) string {
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
	if file.Unavailable && file.NextRetryAfter.After(now) {
		return "credential_cooldown"
	}
	if file.Unavailable {
		return "restricted"
	}
	return "available"
}
