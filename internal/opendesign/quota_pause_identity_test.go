package opendesign

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestQuotaPauseKeyFingerprintIsStableAndKeySpecific(t *testing.T) {
	const first = "fixture-inference-key-one"
	const second = "fixture-inference-key-two"
	a := quotaPauseKeyHash(first)
	if a == "" || a != quotaPauseKeyHash(first) || a == quotaPauseKeyHash(second) {
		t.Fatal("fingerprint is not stable and key-specific")
	}
	if quotaPauseKeyHash("") != "" || strings.Contains(a, first) {
		t.Fatal("empty or sensitive input was not handled safely")
	}
	raw, err := json.Marshal(QuotaBlock{KeyHash: a})
	if err != nil || strings.Contains(string(raw), first) || strings.Contains(string(raw), second) {
		t.Fatal("serialized quota block exposed an inference key")
	}
}

func TestQuotaBlockHostDisabledRoundTripPreservesManualChoice(t *testing.T) {
	const key = "fixture-inference-key-one"
	for _, tc := range []struct {
		name          string
		manual        *bool
		storedKey     string
		wantDisabled  bool
		invalidReason bool
	}{
		{"enabled before pause", new(bool), key, false, false},
		{"disabled before pause", boolPointer(true), key, true, false},
		{"legacy snapshot missing", nil, key, true, false},
		{"key replaced", new(bool), "fixture-old-inference-key", true, false},
		{"invalid reason", new(bool), key, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := &QuotaBlock{
				KeyHash: quotaPauseKeyHash(tc.storedKey), ManualDisabled: tc.manual,
				Source: "upstream", Reason: "quota_exhausted",
				BlockedAt: time.Unix(42, 0).UTC(), RetryAt: time.Unix(102, 0).UTC(),
			}
			if tc.invalidReason {
				block.Reason = "invalid"
			}
			// Simulate CPA FileStore writing the effective disabled flag to disk.
			raw, err := json.Marshal(map[string]any{
				"type": Provider, "id": "fixture", "api_key": key, "disabled": true,
				"quota_block": block, "other_extension": json.Number("12345678901234567890"),
			})
			if err != nil {
				t.Fatal(err)
			}
			var credential Credential
			if err := json.Unmarshal(raw, &credential); err != nil {
				t.Fatal(err)
			}
			if credential.Disabled != tc.wantDisabled {
				t.Fatalf("manual disabled = %t, want %t", credential.Disabled, tc.wantDisabled)
			}
			again, err := json.Marshal(credential)
			if err != nil || !strings.Contains(string(again), `12345678901234567890`) {
				t.Fatal("quota handling discarded an unknown extension")
			}
		})
	}
}

func TestQuotaBlockUnknownManualStateNeverEnables(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	const key = "fixture-inference-key"
	block := &QuotaBlock{
		KeyHash: quotaPauseKeyHash(key), Source: "upstream", Reason: "quota_exhausted",
		BlockedAt: now.Add(-time.Hour), RetryAt: now.Add(time.Hour),
	}
	for _, disk := range []map[string]any{
		{"type": Provider, "id": "fixture", "api_key": key, "disabled": false, "quota_block": block},
		{"type": Provider, "id": "fixture", "api_key": key, "quota_block": block},
	} {
		var credential Credential
		if err := json.Unmarshal(jsonBytes(disk), &credential); err != nil {
			t.Fatal(err)
		}
		if !credential.Disabled {
			t.Fatal("unknown manual state decoded as enabled")
		}
		normalized, changed := normalizeQuotaBlock(credential, now)
		if !changed || normalized.ManualDisabled == nil || !*normalized.ManualDisabled {
			t.Fatal("unknown manual state normalized as enabled")
		}
	}
	// Normalization must also be conservative for callers that construct a
	// Credential directly rather than going through UnmarshalJSON.
	normalized, _ := normalizeQuotaBlock(Credential{APIKey: key, Disabled: false, QuotaBlock: block}, now)
	if normalized.ManualDisabled == nil || !*normalized.ManualDisabled {
		t.Fatal("directly constructed unknown state normalized as enabled")
	}
}

func boolPointer(value bool) *bool { return &value }

func TestQuotaBlockEffectiveDisabledAndExecutionGuard(t *testing.T) {
	s, _ := newTestService(t)
	key := "fixture-inference-key"
	manual := false
	c := Credential{
		Type: Provider, ID: "fixture-id", APIKey: key,
		QuotaBlock: &QuotaBlock{
			KeyHash: quotaPauseKeyHash(key), ManualDisabled: &manual,
			Source: "upstream", Reason: "quota_exhausted",
			BlockedAt: time.Now().Add(-2 * time.Hour), RetryAt: time.Now().Add(-time.Hour),
		},
	}
	s.mu.Lock()
	s.storeCredentialLocked(c)
	s.mu.Unlock()
	if disabled, _ := s.authData(c)["Disabled"].(bool); !disabled {
		t.Fatal("quota block did not disable host auth")
	}
	_, err := s.selectedCredential(ExecutorRequest{AuthID: c.ID})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests || apiErr.Kind != "quota_blocked" || !strings.Contains(apiErr.Message, quotaBlockMarker) {
		t.Fatalf("blocked credential returned unexpected error: %v", err)
	}
	// Expiry is not permission to bypass persistence; the block stays effective
	// until the recovery path explicitly clears and publishes it.
	c.Disabled = true
	s.mu.Lock()
	s.storeCredentialLocked(c)
	s.mu.Unlock()
	_, err = s.selectedCredential(ExecutorRequest{AuthID: c.ID})
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("manual disable should take priority: %v", err)
	}
}

func TestNormalizeQuotaBlockBoundsPersistedDeadlineAndKeepsLegacyManualDisable(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	key := "fixture-inference-key"
	c := Credential{
		APIKey: key, Disabled: true,
		QuotaBlock: &QuotaBlock{
			KeyHash: quotaPauseKeyHash(key), Source: "upstream", Reason: "quota_exhausted",
			BlockedAt: now.Add(-time.Hour), RetryAt: now.Add(365 * 24 * time.Hour),
		},
	}
	block, changed := normalizeQuotaBlock(c, now)
	if !changed || block == nil || block.ManualDisabled == nil || !*block.ManualDisabled {
		t.Fatal("legacy block did not conservatively retain the manual disable")
	}
	if want := c.QuotaBlock.BlockedAt.Add(quotaPauseMaxWindow); !block.RetryAt.Equal(want) {
		t.Fatalf("persisted deadline = %s, want %s", block.RetryAt, want)
	}
	if c.QuotaBlock.ManualDisabled != nil || c.QuotaBlock.RetryAt.Equal(block.RetryAt) {
		t.Fatal("normalization mutated the source snapshot")
	}
}

func TestCloneQuotaBlockDoesNotShareManualDisabled(t *testing.T) {
	if cloneQuotaBlock(nil) != nil {
		t.Fatal("nil block should remain nil")
	}
	manual := true
	original := &QuotaBlock{ManualDisabled: &manual, BlockedAt: time.Unix(42, 0), Reason: "quota_exhausted"}
	cloned := cloneQuotaBlock(original)
	if cloned == original || cloned.ManualDisabled == original.ManualDisabled || !*cloned.ManualDisabled {
		t.Fatal("quota block or its manual-state pointer was not cloned")
	}
	*cloned.ManualDisabled = false
	if !*original.ManualDisabled {
		t.Fatal("editing a cloned snapshot changed the original")
	}
}
