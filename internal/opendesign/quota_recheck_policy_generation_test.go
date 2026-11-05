package opendesign

import (
	"encoding/json"
	"net/http"
	"testing"
)

func enablePausePolicy(t *testing.T, s *Service, maxSeconds int) {
	t.Helper()
	body := map[string]any{"pause": map[string]any{"enabled": true}}
	if maxSeconds > 0 {
		body["pause"].(map[string]any)["max_seconds"] = maxSeconds
	}
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/config", nil, body)
	if status != http.StatusOK {
		t.Fatalf("enable pause policy %d: %s", status, raw)
	}
}

func reconfigure(t *testing.T, s *Service, dataDir string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"config_yaml": []byte("data_dir: " + dataDir + "\n")})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if _, err := s.Handle("plugin.reconfigure", raw); err != nil {
		t.Fatalf("plugin.reconfigure: %v", err)
	}
}

// A host re-registration re-runs configure with a freshly decoded config. The
// pause policy is unchanged, so it must not invalidate an automatic round that is
// already running: comparing the *bool pointers instead of the effective values
// aborted every round on the first re-register.
func TestQuotaRecheckSurvivesReregistrationWithSamePolicy(t *testing.T) {
	s, host, id, _ := quotaRecoveryFixture(t, false, "model-a")
	enablePausePolicy(t, s, 0)
	dataDir := s.config().DataDir

	for i := 0; i < 2; i++ {
		s.mu.RLock()
		before := s.pauseGeneration
		s.mu.RUnlock()
		reconfigure(t, s, dataDir)
		s.mu.RLock()
		after := s.pauseGeneration
		s.mu.RUnlock()
		if after != before {
			t.Fatalf("re-register %d bumped the pause generation %d -> %d", i, before, after)
		}
	}

	queueRecoveryEvidence(t, host, "model-a", "member", "available")
	if err := s.recheckQuotaBlock("callback", id); err != nil {
		t.Fatalf("recheck after re-registration failed: %v", err)
	}
	current, _ := liveCredential(s, id)
	if current.quotaBlocked() {
		t.Fatal("recheck after re-registration kept the pause")
	}
}

// The same applies to the config update path: re-posting an identical policy must
// not discard an in-flight proof.
func TestQuotaRecheckSurvivesIdenticalConfigUpdate(t *testing.T) {
	s, host, id, _ := quotaRecoveryFixture(t, false, "model-a")
	enablePausePolicy(t, s, 604800)
	s.mu.RLock()
	before := s.pauseGeneration
	s.mu.RUnlock()
	enablePausePolicy(t, s, 604800)
	s.mu.RLock()
	after := s.pauseGeneration
	s.mu.RUnlock()
	if after != before {
		t.Fatalf("an identical config update bumped the pause generation %d -> %d", before, after)
	}

	queueRecoveryEvidence(t, host, "model-a", "member", "available")
	if err := s.recheckQuotaBlock("callback", id); err != nil {
		t.Fatalf("recheck after identical config update failed: %v", err)
	}
}

// A real policy change must still abort automatic rounds.
func TestQuotaRecheckAbortsOnRealPolicyChange(t *testing.T) {
	s, _, id, path := quotaRecoveryFixture(t, false, "model-a")
	enablePausePolicy(t, s, 0)
	s.mu.RLock()
	before := s.pauseGeneration
	s.mu.RUnlock()
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/config", nil,
		map[string]any{"pause": map[string]any{"enabled": false}})
	if status != http.StatusOK {
		t.Fatalf("disable policy %d: %s", status, raw)
	}
	s.mu.RLock()
	after := s.pauseGeneration
	s.mu.RUnlock()
	if after == before {
		t.Fatal("disabling the policy did not bump the pause generation")
	}
	requireQuotaStillBlocked(t, s, id, path)
}
