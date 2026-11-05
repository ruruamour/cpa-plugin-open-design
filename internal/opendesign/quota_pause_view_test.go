package opendesign

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// credentialByID digs the per-credential view out of a management response.
func credentialByID(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()
	items, ok := body["credentials"].([]any)
	if !ok {
		t.Fatalf("response has no credentials list: %#v", body)
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if ok && item["id"] == id {
			return item
		}
	}
	t.Fatalf("credential %q missing from response", id)
	return nil
}

func pauseOf(t *testing.T, view map[string]any) map[string]any {
	t.Helper()
	pause, ok := view["quota_pause"].(map[string]any)
	if !ok {
		t.Fatalf("credential view has no quota_pause detail: %#v", view)
	}
	return pause
}

// failHostSave makes every host.auth.save call fail, which is how a real host
// rejects a credential write.
func failHostSave(host *fakeHost, err error) {
	host.mu.Lock()
	host.saveAuthErr = err
	host.mu.Unlock()
}

// The console draws its "同步待重试" badge purely from the per-credential view.
// A failed auth-file write therefore has to be visible on /status and
// /credentials, not only on the /quota-pause list, or the badge never appears.
func TestQuotaPauseSyncErrorIsVisibleOnCredentialViews(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	// No physical auth file yet: the first persistence attempt fails and the
	// fixed, redacted retry notice is recorded.
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	s.mu.RLock()
	if s.quotaPauseErrors[id] != quotaPauseSyncError {
		s.mu.RUnlock()
		t.Fatal("fixture did not record a pause synchronization error")
	}
	s.mu.RUnlock()

	for _, path := range []string{"/status", "/credentials"} {
		status, body, raw := managementCall(t, s, http.MethodGet, apiBase+path, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, status, raw)
		}
		view := credentialByID(t, body, id)
		if view["quota_paused"] != true || view["effective_disabled"] != true || view["disabled"] != false {
			t.Fatalf("GET %s view = %#v", path, view)
		}
		pause := pauseOf(t, view)
		if pause["error"] != quotaPauseSyncError {
			t.Fatalf("GET %s did not surface the pause sync error: %#v", path, pause)
		}
		// The message is a fixed plugin string: no key, fingerprint or path.
		if strings.Contains(raw, "fixture-inference-key") || strings.Contains(raw, quotaPauseKeyHash("fixture-inference-key")) {
			t.Fatalf("GET %s leaked key material: %s", path, raw)
		}
		if _, ok := pause["key_hash"]; ok {
			t.Fatalf("GET %s exposed a key fingerprint: %#v", path, pause)
		}
	}

	// The list route keeps the same shape after the shared view extraction.
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/quota-pause", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /quota-pause: %d %s", status, raw)
	}
	entries := body["pauses"].([]any)
	if len(entries) != 1 {
		t.Fatalf("quota pause list = %#v", body)
	}
	entry := entries[0].(map[string]any)
	if entry["credential_id"] != id || entry["label"] != "fixture" || entry["error"] != quotaPauseSyncError {
		t.Fatalf("quota pause list entry = %#v", entry)
	}
	if entry["effective_disabled"] != true || entry["manual_disabled"] != false {
		t.Fatalf("list entry lost the manual/effective split: %#v", entry)
	}

	// Once the write succeeds the notice clears on every surface.
	quotaFixtureAuthFile(t, s, host, id, nil)
	s.sweepQuotaBlocks(make(chan struct{}))
	for _, path := range []string{"/status", "/credentials", "/quota-pause"} {
		status, body, raw := managementCall(t, s, http.MethodGet, apiBase+path, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("GET %s after repair: %d %s", path, status, raw)
		}
		var pause map[string]any
		if path == "/quota-pause" {
			entries := body["pauses"].([]any)
			if len(entries) != 1 {
				t.Fatalf("pause list after repair = %#v", body)
			}
			pause = entries[0].(map[string]any)
		} else {
			pause = pauseOf(t, credentialByID(t, body, id))
		}
		if _, ok := pause["error"]; ok {
			t.Fatalf("GET %s still reported a repaired sync error: %#v", path, pause)
		}
	}
}

// Creation and update responses reuse the same view, so a pause stays visible
// with its sync notice and without any key material.
func TestQuotaPauseSyncErrorSurvivesCredentialMutationResponses(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	// The host save now fails, so a label edit cannot reach the auth file and the
	// live pause keeps its pending notice.
	quotaFixtureAuthFile(t, s, host, id, nil)
	failHostSave(host, os.ErrPermission)
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: http.StatusPaymentRequired, quotaExhausted: true})
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "label": "renamed"}); status != http.StatusInternalServerError {
		t.Fatalf("failed update response = %d %s", status, raw)
	}

	// A successful manual toggle keeps the block; its response carries the pause
	// and still separates the operator switch from the plugin pause.
	host.mu.Lock()
	host.saveAuthErr = nil
	host.mu.Unlock()
	quotaFixtureAuthFile(t, s, host, id, nil)
	status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{"id": id, "disabled": false})
	if status != http.StatusOK {
		t.Fatalf("manual toggle = %d %s", status, raw)
	}
	view := credentialByID(t, map[string]any{"credentials": []any{body}}, id)
	pause := pauseOf(t, view)
	if view["disabled"] != false || view["quota_paused"] != true || pause["manual_disabled"] != false {
		t.Fatalf("toggle response conflated manual and pause state: %#v", body)
	}
	if strings.Contains(raw, "fixture-inference-key") {
		t.Fatalf("credential response leaked the api key: %s", raw)
	}

	// A create response must not carry a pause notice for a fresh account.
	status, created, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil,
		map[string]any{"label": "second", "api_key": "fixture-inference-key-two"})
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, raw)
	}
	if created["quota_paused"] != false {
		t.Fatalf("new credential = %#v", created)
	}
	if _, ok := created["quota_pause"]; ok {
		t.Fatalf("new credential reported a pause: %#v", created)
	}
	if strings.Contains(raw, "fixture-inference-key-two") || strings.Contains(raw, "fixture-inference-key") {
		t.Fatalf("create response leaked a key: %s", raw)
	}
}

// remaining_seconds and awaiting_recheck are presentation hints only. They are
// derived from the deadline the plugin already recorded, never from a fresh
// quota request, and the deadline itself never reactivates an account.
func TestQuotaPauseViewReportsRemainingSecondsOnlyBeforeDeadline(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	quotaFixtureAuthFile(t, s, host, id, nil)
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	setNow := setQuotaPauseFixtureClock(s, now)
	source, _ := liveCredential(s, id)
	s.observeQuotaBlock(source, "fixture-model", &APIError{Status: 402, quotaExhausted: true, quotaResetAt: now.Add(90 * time.Minute)})

	_, body, _ := managementCall(t, s, http.MethodGet, apiBase+"/status", nil, nil)
	pause := pauseOf(t, credentialByID(t, body, id))
	if pause["remaining_seconds"] != float64(90*60) {
		t.Fatalf("remaining_seconds = %#v, want 5400", pause["remaining_seconds"])
	}
	if _, ok := pause["awaiting_recheck"]; ok {
		t.Fatalf("a not-yet-due pause was marked as awaiting recheck: %#v", pause)
	}
	// The value follows the plugin clock, not the wall clock, and shrinks.
	now = now.Add(30 * time.Minute)
	setNow(now)
	_, body, _ = managementCall(t, s, http.MethodGet, apiBase+"/status", nil, nil)
	pause = pauseOf(t, credentialByID(t, body, id))
	if pause["remaining_seconds"] != float64(60*60) {
		t.Fatalf("remaining_seconds after 30m = %#v, want 3600", pause["remaining_seconds"])
	}

	// Past the deadline the pause stays recorded and is marked as awaiting a
	// recheck instead of a countdown. This is a read-only view: it must not clear
	// the block or issue a quota request.
	now = now.Add(70 * time.Minute)
	setNow(now)
	current, _ := liveCredential(s, id)
	view := credentialView(current, now, "")
	pause = pauseOf(t, view)
	if _, ok := pause["remaining_seconds"]; ok {
		t.Fatalf("overdue pause still advertised a countdown: %#v", pause)
	}
	if pause["awaiting_recheck"] != true {
		t.Fatalf("overdue pause was not marked as awaiting recheck: %#v", pause)
	}
	if view["quota_paused"] != true || view["disabled"] != false {
		t.Fatalf("overdue pause silently reactivated the account: %#v", view)
	}
	if after, _ := liveCredential(s, id); after.QuotaBlock == nil || after.QuotaRevision != current.QuotaRevision {
		t.Fatal("building a view mutated the live pause")
	}
}

// The console never sees a pause detail without the record that created it, and
// a view built for an unpaused credential stays exactly as small as before.
func TestCredentialViewOmitsPauseDetailWhenNotPaused(t *testing.T) {
	s, host := newQuotaPauseTestService(t)
	id := addCredential(t, s, host, "fixture", "fixture-inference-key")
	status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/status", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d %s", status, raw)
	}
	view := credentialByID(t, body, id)
	if view["quota_paused"] != false || view["effective_disabled"] != false {
		t.Fatalf("unpaused view = %#v", view)
	}
	if _, ok := view["quota_pause"]; ok {
		t.Fatalf("unpaused view carried pause detail: %#v", view)
	}
	if _, err := json.Marshal(view); err != nil {
		t.Fatalf("credential view is not serialisable: %v", err)
	}
}
