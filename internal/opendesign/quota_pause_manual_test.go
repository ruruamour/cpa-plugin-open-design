package opendesign

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func setFixtureQuotaBlock(t *testing.T, s *Service, id string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.creds[id]
	if !ok {
		t.Fatal("credential not found")
	}
	manual := c.Disabled
	c.QuotaBlock = &QuotaBlock{
		KeyHash: quotaPauseKeyHash(c.APIKey), ManualDisabled: &manual,
		Source: "upstream", Reason: "quota_exhausted",
		BlockedAt: time.Now().Add(-time.Minute), RetryAt: time.Now().Add(time.Hour),
	}
	c.QuotaRevision = "fixture-revision"
	s.storeCredentialLocked(c)
}

func TestQuotaBlockManualToggleAndKeyRotation(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key-one")
	setFixtureQuotaBlock(t, s, id)
	for _, disabled := range []bool{true, false} {
		status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "disabled": disabled})
		if status != http.StatusOK {
			t.Fatalf("manual toggle returned %d", status)
		}
		c, ok := liveCredential(s, id)
		if !ok || c.Disabled != disabled || !c.quotaBlocked() || c.QuotaBlock.ManualDisabled == nil || *c.QuotaBlock.ManualDisabled != disabled {
			t.Fatal("manual choice was not kept separately from the quota block")
		}
		if !c.effectiveDisabled() {
			t.Fatal("manual toggle cleared the temporary quota block")
		}
	}
	status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "api_key": "fixture-inference-key-two"})
	if status != http.StatusOK {
		t.Fatalf("key replacement returned %d", status)
	}
	c, ok := liveCredential(s, id)
	if !ok || c.QuotaBlock != nil || c.Disabled {
		t.Fatal("key replacement did not clear only the old key's quota block")
	}
}

func TestQuotaBlockFailedManualToggleKeepsSnapshot(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	setFixtureQuotaBlock(t, s, id)
	host.mu.Lock()
	host.saveAuthErr = errors.New("fixture save failure")
	host.mu.Unlock()
	status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "disabled": true})
	if status != http.StatusInternalServerError {
		t.Fatalf("failed manual toggle returned %d", status)
	}
	c, ok := liveCredential(s, id)
	if !ok || c.Disabled || c.QuotaBlock == nil || c.QuotaBlock.ManualDisabled == nil || *c.QuotaBlock.ManualDisabled {
		t.Fatal("failed host save mutated the live manual state or quota block")
	}
}

func TestQuotaPauseManagementRoutes(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	quotaFixtureAuthFile(t, s, host, id, nil)
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: 402, quotaExhausted: true})

	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/quota-pause", nil, nil)
	if status != http.StatusOK || len(body["pauses"].([]any)) != 1 {
		t.Fatalf("quota pause list: %d %s", status, raw)
	}
	entry := body["pauses"].([]any)[0].(map[string]any)
	if entry["manual_disabled"] != false || entry["effective_disabled"] != true {
		t.Fatalf("pause entry = %#v", entry)
	}
	if _, ok := entry["key_hash"]; ok {
		t.Fatal("quota pause list exposed a key fingerprint")
	}

	// An unknown field must be rejected rather than silently ignored.
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/resume", nil, map[string]any{"credential_id": id, "extra": true}); status != http.StatusBadRequest {
		t.Fatal("strict decoding was not enforced")
	}
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/resume", nil, map[string]any{"credential_id": "missing"}); status != http.StatusNotFound {
		t.Fatal("unknown credential was accepted")
	}
	// A preflight without configured billing cannot prove recovery.
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/recheck", nil, map[string]any{"credential_id": id}); status != http.StatusConflict {
		t.Fatal("unproven preflight cleared the pause")
	}
	if c, _ := liveCredential(s, id); !c.quotaBlocked() {
		t.Fatal("unproven preflight removed the quota block")
	}
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/quota-pause/resume", nil, map[string]any{"credential_id": id}); status != http.StatusOK {
		t.Fatalf("manual resume: %d %s", status, raw)
	}
	c, _ := liveCredential(s, id)
	if c.quotaBlocked() || c.Disabled {
		t.Fatal("manual resume did not clear only the quota block")
	}
}

func TestQuotaPausePolicyToggleAndMaxWindow(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	quotaFixtureAuthFile(t, s, host, id, nil)
	source, _ := liveCredential(s, id)
	disabled := false
	s.mu.Lock()
	s.cfg.Pause = PauseConfig{Enabled: &disabled}
	s.mu.Unlock()
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: 402, quotaExhausted: true})
	if c, _ := liveCredential(s, id); c.quotaBlocked() {
		t.Fatal("the plugin-wide toggle did not disable automatic pauses")
	}
	enabled := true
	s.mu.Lock()
	s.cfg.Pause = PauseConfig{Enabled: &enabled, MaxSeconds: 120}
	s.mu.Unlock()
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: 402, quotaExhausted: true})
	c, _ := liveCredential(s, id)
	if !c.quotaBlocked() {
		t.Fatal("enabling the policy did not restore automatic pauses")
	}
	if window := c.QuotaBlock.RetryAt.Sub(c.QuotaBlock.BlockedAt); window > 120*time.Second {
		t.Fatalf("pause window %s exceeded the configured maximum", window)
	}
	// A malformed max_seconds cannot overflow or exceed the hard ceiling.
	s.mu.Lock()
	s.cfg.Pause = PauseConfig{Enabled: &enabled, MaxSeconds: 1 << 40}
	s.mu.Unlock()
	settings := s.config().pauseSettings()
	if settings.maxWindow != quotaPauseMaxWindow {
		t.Fatalf("clamped max window = %s", settings.maxWindow)
	}
}

func TestQuotaPauseConfigRouteRoundTrip(t *testing.T) {
	s, host := newTestService(t)
	addCredential(t, s, host, "fixture", "fixture-inference-key")
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/config", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("config read: %d %s", status, raw)
	}
	pause, ok := body["pause"].(map[string]any)
	if !ok {
		t.Fatalf("default pause config = %#v", body["pause"])
	}
	wantSeconds := float64(s.config().pauseSettings().maxWindow / time.Second)
	if pause["enabled"] != true || pause["max_seconds"] != wantSeconds {
		t.Fatalf("default pause config = %#v", body["pause"])
	}
	// Unknown fields and out-of-range values are rejected, not clamped silently.
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/config", nil, map[string]any{"pause": map[string]any{"enabled": false, "extra": 1}}); status != http.StatusBadRequest {
		t.Fatal("strict config decoding was not enforced")
	}
	if status, _, _ := managementCall(t, s, http.MethodPost, apiBase+"/config", nil, map[string]any{"pause": map[string]any{"max_seconds": 5}}); status != http.StatusBadRequest {
		t.Fatal("an out-of-range max_seconds was accepted")
	}
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/config", nil, map[string]any{"pause": map[string]any{"enabled": false, "max_seconds": 3600}}); status != http.StatusOK {
		t.Fatalf("config write: %d %s", status, raw)
	}
	// A logs-only update must not reset the pause policy.
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/config", nil, map[string]any{"logs": map[string]any{"enabled": true}}); status != http.StatusOK {
		t.Fatalf("logs-only write: %d %s", status, raw)
	}
	status, body, raw = managementCall(t, s, http.MethodGet, apiBase+"/config", nil, nil)
	pause = body["pause"].(map[string]any)
	if status != http.StatusOK || pause["enabled"] != false || pause["max_seconds"] != float64(3600) {
		t.Fatalf("pause config was lost: %#v", body["pause"])
	}
}
