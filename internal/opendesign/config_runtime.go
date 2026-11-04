package opendesign

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxConfigBodyBytes = 8 << 10

type configLogUpdate struct {
	Enabled       *bool `json:"enabled"`
	RetentionDays *int  `json:"retention_days"`
	MaxEntries    *int  `json:"max_entries"`
}

// configPauseUpdate is the plugin-owned quota-pause policy. It only bounds what
// this plugin does to its own credentials; CPA scheduling and cooldowns are
// untouched.
type configPauseUpdate struct {
	Enabled    *bool `json:"enabled"`
	MaxSeconds *int  `json:"max_seconds"`
}

type configUpdateInput struct {
	Logs  *configLogUpdate   `json:"logs"`
	Pause *configPauseUpdate `json:"pause"`
}

func configView(cfg Config, logView map[string]any) map[string]any {
	settings := cfg.pauseSettings()
	return map[string]any{
		"logs": logView,
		"pause": map[string]any{
			"enabled":     settings.enabled,
			"max_seconds": int64(settings.maxWindow / time.Second),
			// The pause is plugin-owned and bounded; it never changes CPA's own
			// scheduling or cooldown behavior.
			"owner": "plugin",
		},
		"upstream": map[string]any{
			"origin": DefaultBaseURL,
			"fixed":  true,
		},
		"proxy": map[string]any{
			"owner":     "CPA",
			"read_only": true,
			"mode":      "host_managed",
		},
		"routing": map[string]any{
			"owner":     "CPA",
			"read_only": true,
			"mode":      "host_managed",
		},
		"log_level": cfg.LogLevel,
	}
}

func (s *Service) configRequest() (any, error) {
	if err := s.begin(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	defer s.done()
	cfg := s.config()
	return managementJSON(http.StatusOK, configView(cfg, s.requestLogs.settingsView()))
}

// updateConfigRequest changes only plugin-owned settings. Upstream origin,
// proxy, routing and host-wide scheduling are intentionally not accepted here.
func (s *Service) updateConfigRequest(r ManagementRequest) (any, error) {
	if err := s.begin(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	defer s.done()
	if len(r.Body) > maxConfigBodyBytes {
		return managementError(http.StatusRequestEntityTooLarge, "config request is too large")
	}
	var in configUpdateInput
	if err := jsonUnmarshalObject(r.Body, &in); err != nil {
		return managementError(http.StatusBadRequest, "invalid config JSON")
	}
	if in.Logs == nil && in.Pause == nil {
		return managementError(http.StatusBadRequest, "no plugin-owned config fields supplied")
	}
	if in.Logs != nil && in.Logs.RetentionDays != nil && (*in.Logs.RetentionDays < 1 || *in.Logs.RetentionDays > maxRequestLogRetentionDays) {
		return managementError(http.StatusBadRequest, "logs.retention_days must be between 1 and 90")
	}
	if in.Logs != nil && in.Logs.MaxEntries != nil && (*in.Logs.MaxEntries < 1 || *in.Logs.MaxEntries > maxRequestLogMaxEntries) {
		return managementError(http.StatusBadRequest, "logs.max_entries must be between 1 and 500")
	}
	if in.Pause != nil && in.Pause.MaxSeconds != nil && (*in.Pause.MaxSeconds < int(quotaPauseMinWindow/time.Second) || *in.Pause.MaxSeconds > int(quotaPauseMaxWindow/time.Second)) {
		return managementError(http.StatusBadRequest, "pause.max_seconds must be between 60 and 604800")
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	cfg := s.config()
	s.mu.RLock()
	loaded := s.loaded
	s.mu.RUnlock()
	if !loaded || strings.TrimSpace(cfg.DataDir) == "" {
		return managementError(http.StatusConflict, "plugin is not configured")
	}
	if in.Logs != nil {
		if in.Logs.Enabled != nil {
			value := *in.Logs.Enabled
			cfg.Logs.Enabled = &value
		}
		if in.Logs.RetentionDays != nil {
			cfg.Logs.RetentionDays = *in.Logs.RetentionDays
		}
		if in.Logs.MaxEntries != nil {
			cfg.Logs.MaxEntries = *in.Logs.MaxEntries
		}
	}
	if in.Pause != nil {
		if in.Pause.Enabled != nil {
			value := *in.Pause.Enabled
			cfg.Pause.Enabled = &value
		}
		if in.Pause.MaxSeconds != nil {
			cfg.Pause.MaxSeconds = *in.Pause.MaxSeconds
		}
	}
	if err := cfg.validate(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	if err := s.saveSettings(cfg); err != nil {
		return managementError(http.StatusInternalServerError, "could not persist plugin config")
	}
	s.mu.Lock()
	s.cfg.Logs = cfg.Logs
	s.cfg.Pause = cfg.Pause
	s.mu.Unlock()
	s.requestLogs.setSettings(cfg.logSettings())
	s.wakeQuotaPauseWorker()
	return managementJSON(http.StatusOK, configView(cfg, s.requestLogs.settingsView()))
}

// jsonUnmarshalObject rejects unknown fields so an operator cannot mistake a
// silently ignored host-owned setting for a successful plugin update.
func jsonUnmarshalObject(raw []byte, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errInvalidJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

var errInvalidJSON = &APIError{Status: http.StatusBadRequest, Kind: "invalid_json", Message: "invalid JSON"}

type runtimeHostAuth struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Provider       string     `json:"provider"`
	Type           string     `json:"type"`
	Status         string     `json:"status"`
	StatusMessage  string     `json:"status_message"`
	Disabled       *bool      `json:"disabled"`
	Unavailable    *bool      `json:"unavailable"`
	NextRetryAfter *time.Time `json:"next_retry_after"`
}

func (s *Service) runtimeRequest() (any, error) {
	if err := s.begin(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	defer s.done()
	cfg := s.config()
	// Host facts may describe the credential that existed when the callback
	// started. Keep both that identity and the later credential snapshot in the
	// redaction set if a key is replaced while host.auth.list is in flight.
	runtimeKeys := s.metadataSecrets()
	hostAuths, hostErr := s.runtimeHostAuths()
	runtimeKeys = append(runtimeKeys, s.metadataSecrets()...)
	s.mu.RLock()
	creds := make([]Credential, 0, len(s.creds))
	for _, credential := range s.creds {
		creds = append(creds, credential)
		runtimeKeys = append(runtimeKeys, credential.APIKey, credential.ControlKey, credential.WorkspaceID)
	}
	tests := make(map[string]modelTestRecord, len(s.modelTests))
	for key, record := range s.modelTests {
		tests[key] = record
	}
	running := make(map[string]int, len(s.modelTestRunning))
	for key, count := range s.modelTestRunning {
		running[key] = count
	}
	authFiles := make(map[string]string, len(s.authFiles))
	for id, filename := range s.authFiles {
		authFiles[id] = filename
	}
	active := s.activeN.Load() - 1
	if active < 0 {
		active = 0
	}
	stopped, loaded := s.stopped, s.loaded
	s.mu.RUnlock()
	sort.Slice(creds, func(i, j int) bool {
		if creds[i].Label != creds[j].Label {
			return creds[i].Label < creds[j].Label
		}
		return creds[i].ID < creds[j].ID
	})
	accounts := make([]map[string]any, 0, len(creds))
	for _, credential := range creds {
		row := map[string]any{
			"id":           safeRuntimeText(credential.ID, 128, runtimeKeys...),
			"label":        safeRuntimeText(credential.Label, 100, runtimeKeys...),
			"disabled":     credential.Disabled,
			"model_count":  len(cfg.Models),
			"models_scope": "shared",
			"testing":      false,
			"last_test":    nil,
			"host":         nil,
		}
		for _, model := range cfg.Models {
			if running[credential.ID+"\x00"+model.ID] > 0 {
				row["testing"] = true
				break
			}
		}
		var latest *modelTestRecord
		latestKey := ""
		for key, record := range tests {
			if !strings.HasPrefix(key, credential.ID+"\x00") {
				continue
			}
			if running[key] > 0 {
				row["testing"] = true
			}
			if latest == nil || record.TestedAt.After(latest.TestedAt) ||
				(record.TestedAt.Equal(latest.TestedAt) && key > latestKey) {
				copy := record
				latest = &copy
				latestKey = key
			}
		}
		if latest != nil {
			row["last_test"] = runtimeModelTestView(*latest, runtimeKeys...)
		}
		if host := runtimeAuthFor(credential, authFiles[credential.ID], hostAuths); host != nil {
			var nextRetryAfter any
			if host.NextRetryAfter != nil && !host.NextRetryAfter.IsZero() {
				nextRetryAfter = host.NextRetryAfter
			}
			row["host"] = map[string]any{
				"disabled":         optionalBool(host.Disabled),
				"unavailable":      optionalBool(host.Unavailable),
				"status":           safeRuntimeText(host.Status, 64, runtimeKeys...),
				"status_message":   safeRuntimeText(host.StatusMessage, 200, runtimeKeys...),
				"next_retry_after": nextRetryAfter,
			}
		}
		accounts = append(accounts, row)
	}
	hostView := map[string]any{"available": hostErr == nil}
	if hostErr != nil {
		hostView["note"] = "host.auth.list 不可用；以下仅为插件侧状态，调度、代理与冷却以 CPA 实际结果为准。"
	}
	return managementJSON(http.StatusOK, map[string]any{
		"observed_at":        time.Now().UTC(),
		"loaded":             loaded,
		"stopped":            stopped,
		"active_requests":    active,
		"pending_operations": s.ops.pendingCount(),
		"model_count":        len(cfg.Models),
		"credentials":        accounts,
		"host":               hostView,
		"capabilities": map[string]any{
			"proxy":    map[string]any{"read_only": true, "owner": "CPA"},
			"routing":  map[string]any{"read_only": true, "owner": "CPA"},
			"cooldown": map[string]any{"read_only": true, "owner": "CPA"},
		},
	})
}

func (s *Service) runtimeHostAuths() ([]runtimeHostAuth, error) {
	var response struct {
		Files []runtimeHostAuth `json:"files"`
	}
	if err := s.call("host.auth.list", map[string]any{}, &response); err != nil {
		return nil, err
	}
	filtered := make([]runtimeHostAuth, 0, len(response.Files))
	for _, entry := range response.Files {
		if entry.Provider == Provider || entry.Type == Provider {
			filtered = append(filtered, entry)
		}
	}
	return filtered, nil
}

func optionalBool(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

func safeRuntimeText(value string, limit int, keys ...string) string {
	return requestLogText(value, limit, keys...)
}

func runtimeModelTestView(record modelTestRecord, keys ...string) map[string]any {
	kind := record.ErrorKind
	if !safeDiagnosticKind(kind, keys) {
		kind = ""
	}
	status := record.Status
	if status != "success" && status != "error" {
		status = "unknown"
	}
	message := ""
	if kind != "" {
		message = modelTestErrorMessage(kind)
	}
	return map[string]any{
		"model_id":       requestLogText(record.ModelID, 512, keys...),
		"upstream_model": requestLogText(record.UpstreamModel, 512, keys...),
		"status":         status,
		"http_status":    record.HTTPStatus,
		"duration_ms":    record.DurationMS,
		"tested_at":      record.TestedAt,
		"error_kind":     kind,
		"error_message":  message,
	}
}

func runtimeAuthFor(credential Credential, configuredName string, entries []runtimeHostAuth) *runtimeHostAuth {
	filename := credential.ID + ".json"
	if configuredName != "" {
		filename = filepath.Base(strings.TrimSpace(configuredName))
	}
	for index := range entries {
		entryName := filepath.Base(strings.TrimSpace(entries[index].Name))
		if entries[index].ID == credential.ID || entryName == filename {
			return &entries[index]
		}
	}
	return nil
}
