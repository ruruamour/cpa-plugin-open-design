package opendesign

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// assertNoSecretFragments looks for the whole secret and for every 8-rune
// fragment of it. A presentation limit applied before redaction can leave a
// short prefix of a rotated key in an otherwise "truncated" field, which the
// whole-value check alone would miss.
func assertNoSecretFragments(t *testing.T, where, value string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(value, secret) {
			t.Errorf("%s leaked the whole secret %q: %q", where, secret, value)
			continue
		}
		runes := []rune(secret)
		window := 8
		if len(runes) < window {
			window = len(runes)
		}
		for start := 0; start+window <= len(runes); start++ {
			fragment := string(runes[start : start+window])
			if strings.Contains(value, fragment) {
				t.Errorf("%s leaked fragment %q of secret %q: %q", where, fragment, secret, value)
				break
			}
		}
	}
}

func seedConsistencyMapping(t *testing.T, s *Service, alias, upstream string) {
	t.Helper()
	status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/models", nil, map[string]any{
		"models": []map[string]string{{"id": alias, "upstream_id": upstream}},
	})
	if status != http.StatusOK {
		t.Fatalf("seed model mapping: %d %s", status, raw)
	}
}

// A request log row is built from a draft snapshot. After the credential is
// rotated, the draft still carries the old api, control and workspace values;
// presentation limits must be applied only after every one of them has been
// redacted across the whole value. The persisted snapshot is checked too, not
// just the in-memory query result.
func TestManagementConsistencyRequestLogDraftRedactsAcrossTruncation(t *testing.T) {
	s, host := newTestService(t)
	const (
		oldAPI       = "plain-old-api-token-0001"
		oldControl   = "plain-old-control-token-0002"
		oldWorkspace = "ws-plain-old-workspace-0003"
		newAPI       = "plain-new-api-token-0004"
		newControl   = "plain-new-control-token-0005"
		newWorkspace = "ws-plain-new-workspace-0006"
		safeRequest  = "req-management-consistency-0001"
	)

	id := addCredential(t, s, host, "primary", oldAPI)
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
		"id": id, "control_key": oldControl, "workspace_id": oldWorkspace,
	}); status != http.StatusOK {
		t.Fatalf("seed billing identity: %d %s", status, raw)
	}
	if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
		"id": id, "api_key": newAPI, "control_key": newControl, "workspace_id": newWorkspace,
	}); status != http.StatusOK {
		t.Fatalf("rotate credential: %d %s", status, raw)
	}
	live, ok := liveCredential(s, id)
	if !ok || live.APIKey != newAPI || live.ControlKey != newControl || live.WorkspaceID != newWorkspace {
		t.Fatalf("rotation did not publish the new identity: %+v", live)
	}
	secrets := []string{oldAPI, oldControl, oldWorkspace, newAPI, newControl, newWorkspace}

	started := time.Now().Add(-time.Second)
	knownErr := upstreamErrorFromResponse(http.StatusTooManyRequests, nil,
		[]byte(`{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`))
	s.recordRequestLog(&requestLogDraft{
		started:           started,
		model:             strings.Repeat("m", 490) + oldAPI + "-tail",
		upstreamModel:     strings.Repeat("u", 500) + oldControl,
		credentialID:      strings.Repeat("c", 120) + oldWorkspace,
		requestID:         safeRequest,
		upstreamAttempted: true,
		httpStatus:        http.StatusTooManyRequests,
		secrets:           []string{oldAPI, oldControl, oldWorkspace},
	}, knownErr)

	// The old api key straddles the request-id boundary and the error kind is an
	// arbitrary, machine-shaped code that is not on the controlled allowlist.
	s.recordRequestLog(&requestLogDraft{
		started:   started.Add(time.Second),
		model:     "alias-two",
		requestID: "req-" + oldAPI,
		secrets:   []string{oldAPI, oldControl, oldWorkspace},
	}, &APIError{Status: http.StatusInternalServerError, Kind: "totally_unknown_code", Message: "upstream detail " + oldAPI})

	// An over-long id is dropped, and the message prefix is also an unknown code.
	s.recordRequestLog(&requestLogDraft{
		started:   started.Add(2 * time.Second),
		model:     "alias-three",
		requestID: strings.Repeat("r", 129),
		secrets:   []string{oldAPI, oldControl, oldWorkspace},
	}, upstreamErrorFromResponse(http.StatusBadRequest, nil,
		[]byte(`{"error":{"message":"totally_unknown_code: rejected"}}`)))

	logs, total, _ := s.requestLogs.query(requestLogFilter{limit: 10})
	if total != 3 || len(logs) != 3 {
		t.Fatalf("logs = %#v total = %d, want three rows", logs, total)
	}

	first, second, third := logs[2], logs[1], logs[0]
	// Every raw value below is longer than its storage bound and the rotated
	// secret straddles that bound. Redaction therefore has to run on the whole
	// value: truncating first would leave an 8+ character prefix of the secret.
	if want := strings.Repeat("m", 490) + "[redacted]-tail"; first.Model != want {
		t.Fatalf("model = %q, want %q (secret straddling the 512 bound)", first.Model, want)
	}
	if want := strings.Repeat("u", 500) + "[redacted]"; first.UpstreamModel != want {
		t.Fatalf("upstream model = %q, want %q (secret straddling the 512 bound)", first.UpstreamModel, want)
	}
	if want := string([]rune(strings.Repeat("c", 120) + "[redacted]")[:128]); first.CredentialID != want {
		t.Fatalf("credential id = %q, want %q (secret straddling the 128 bound)", first.CredentialID, want)
	}
	if first.RequestID != safeRequest {
		t.Fatalf("safe request id = %q, want it preserved verbatim", first.RequestID)
	}
	if first.Status != "error" || first.HTTPStatus == nil || *first.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("first row status metadata = %#v", first)
	}
	if first.UpstreamAttempted == nil || !*first.UpstreamAttempted {
		t.Fatalf("first row lost its upstream attempt: %#v", first)
	}
	if first.ErrorKind == nil || *first.ErrorKind != "rate_limit_exceeded" {
		t.Fatalf("compacted known error kind = %#v", first.ErrorKind)
	}

	if second.RequestID != "" {
		t.Fatalf("request id carrying a rotated key survived: %q", second.RequestID)
	}
	if second.ErrorKind == nil || *second.ErrorKind != "upstream_error" {
		t.Fatalf("arbitrary structured code was not folded into upstream_error: %#v", second.ErrorKind)
	}
	if second.UpstreamAttempted == nil || *second.UpstreamAttempted {
		t.Fatalf("locally rejected row claimed an upstream attempt: %#v", second)
	}
	if third.RequestID != "" {
		t.Fatalf("over-long request id survived: %q", third.RequestID)
	}
	if third.ErrorKind == nil || *third.ErrorKind != "upstream_error" {
		t.Fatalf("arbitrary message-prefix code was not folded into upstream_error: %#v", third.ErrorKind)
	}

	for _, entry := range logs {
		where := "in-memory log row"
		for _, field := range []string{entry.Model, entry.UpstreamModel, entry.CredentialID, entry.RequestID} {
			assertNoSecretFragments(t, where, field, secrets...)
		}
		if entry.ErrorKind != nil {
			assertNoSecretFragments(t, where+" error kind", *entry.ErrorKind, secrets...)
		}
	}

	if err := s.requestLogs.flush(); err != nil {
		t.Fatalf("flush request log snapshot: %v", err)
	}
	snapshot, err := os.ReadFile(filepath.Join(s.config().DataDir, requestLogSnapshotName))
	if err != nil {
		t.Fatalf("read persisted request log snapshot: %v", err)
	}
	persisted := string(snapshot)
	if !strings.Contains(persisted, safeRequest) {
		t.Fatalf("persisted snapshot is missing the safe request id, so the leak scan would be vacuous: %s", persisted)
	}
	assertNoSecretFragments(t, "persisted request log snapshot", persisted, secrets...)
}

// Runtime fields are also redacted before they are bounded. The host facts carry
// the same boundary problem plus a zero retry time that must stay JSON null.
func TestManagementConsistencyRuntimeRedactsTruncatedHostFacts(t *testing.T) {
	s, host := newTestService(t)
	const (
		apiKey     = "plainRuntimeApiSecret-1001"
		controlKey = "plainRuntimeControlSecret-1002"
		workspace  = "ws-runtime-workspace-1003"
	)
	id := strings.Repeat("i", 120) + controlKey
	label := strings.Repeat("L", 96) + apiKey
	fileName := id + ".json"
	body := mustJSON(t, map[string]any{
		"type": Provider, "id": id, "label": label,
		"api_key": apiKey, "control_key": controlKey, "workspace_id": workspace,
		"disabled": false,
	})
	host.writeAuthFile(t, fileName, body)
	out, err := s.parseAuth(mustJSON(t, map[string]any{
		"Provider": Provider,
		"FileName": fileName,
		"RawJSON":  body,
		"Host":     map[string]any{"AuthDir": host.authDir},
	}))
	if err != nil {
		t.Fatalf("load credential through auth.parse: %v", err)
	}
	if handled, _ := out.(map[string]any)["Handled"].(bool); !handled {
		t.Fatalf("auth.parse rejected the credential")
	}
	host.mu.Lock()
	host.authList = []map[string]any{{
		"name":             fileName,
		"id":               id,
		"provider":         Provider,
		"status":           strings.Repeat("s", 60) + controlKey,
		"status_message":   strings.Repeat("x", 190) + workspace + " tail",
		"disabled":         false,
		"unavailable":      true,
		"next_retry_after": "0001-01-01T00:00:00Z",
	}}
	host.mu.Unlock()

	status, bodyView, raw := managementCall(t, s, http.MethodGet, apiBase+"/runtime", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("runtime status = %d body = %s", status, raw)
	}
	accounts, ok := bodyView["credentials"].([]any)
	if !ok || len(accounts) != 1 {
		t.Fatalf("runtime accounts = %#v", bodyView["credentials"])
	}
	account := accounts[0].(map[string]any)
	idView, _ := account["id"].(string)
	labelView, _ := account["label"].(string)
	hostView, ok := account["host"].(map[string]any)
	if !ok {
		t.Fatalf("runtime host view = %#v", account["host"])
	}
	statusView, _ := hostView["status"].(string)
	messageView, _ := hostView["status_message"].(string)

	for name, field := range map[string]struct {
		value string
		limit int
		pad   string
	}{
		"id":             {idView, 128, strings.Repeat("i", 120)},
		"label":          {labelView, 100, strings.Repeat("L", 96)},
		"status":         {statusView, 64, strings.Repeat("s", 60)},
		"status_message": {messageView, 200, strings.Repeat("x", 190)},
	} {
		if got := len([]rune(field.value)); got != field.limit {
			t.Errorf("%s rune length = %d, want %d: %q", name, got, field.limit, field.value)
		}
		if !strings.HasPrefix(field.value, field.pad) {
			t.Errorf("%s lost its prefix (truncated before redaction?): %q", name, field.value)
		}
		if !strings.Contains(field.value, "[red") {
			t.Errorf("%s has no redaction marker, so a secret prefix may have survived: %q", name, field.value)
		}
	}
	if hostView["next_retry_after"] != nil {
		t.Fatalf("zero host retry time was serialized as %#v, want null", hostView["next_retry_after"])
	}
	if hostView["disabled"] != false || hostView["unavailable"] != true {
		t.Fatalf("host flags = %#v", hostView)
	}

	secrets := []string{apiKey, controlKey, workspace}
	assertNoSecretFragments(t, "runtime response", raw, secrets...)
	for name, field := range map[string]string{
		"id": idView, "label": labelView, "status": statusView, "status_message": messageView,
	} {
		assertNoSecretFragments(t, "runtime "+name, field, secrets...)
	}
}

func identityFromSnapshot(snapshot modelTestSnapshot, alias string) modelTestIdentity {
	return modelTestIdentity{
		credentialID:    snapshot.credential.ID,
		modelID:         alias,
		apiKey:          snapshot.credential.APIKey,
		controlKey:      snapshot.credential.ControlKey,
		workspaceID:     snapshot.credential.WorkspaceID,
		baseURL:         snapshot.credential.BaseURL,
		disabled:        snapshot.credential.Disabled,
		credentialEpoch: snapshot.credentialEpoch,
		upstreamModel:   snapshot.upstreamModel,
		mappingRevision: snapshot.mappingRevision,
	}
}

func assertStaleSnapshotFenced(t *testing.T, s *Service, snapshot modelTestSnapshot, alias string) {
	t.Helper()
	_, err := s.selectedCredential(ExecutorRequest{CredentialOverride: &snapshot.credential})
	if err == nil {
		t.Fatalf("stale credential snapshot was accepted after an identity round trip")
	}
	if status := statusOf(err); status != http.StatusConflict {
		t.Fatalf("stale snapshot status = %d, want 409: %v", status, err)
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Kind != "credential_changed" {
		t.Fatalf("stale snapshot error = %v, want credential_changed", err)
	}

	key := snapshot.credential.ID + "\x00" + alias
	if !s.modelTestStarted(key) {
		t.Fatalf("could not reserve the model-test slot")
	}
	record := modelTestRecord{
		ModelID:       alias,
		UpstreamModel: snapshot.upstreamModel,
		Status:        "success",
		TestedAt:      time.Now().UTC(),
	}
	s.modelTestFinished(key, record, identityFromSnapshot(snapshot, alias))
	s.mu.RLock()
	stored, exists := s.modelTests[key]
	s.mu.RUnlock()
	if exists {
		t.Fatalf("model-test result from a stale epoch was stored: %+v", stored)
	}
}

func assertFreshSnapshotAccepted(t *testing.T, s *Service, snapshot modelTestSnapshot, alias string) {
	t.Helper()
	selected, err := s.selectedCredential(ExecutorRequest{CredentialOverride: &snapshot.credential})
	if err != nil {
		t.Fatalf("fresh credential snapshot was rejected: %v", err)
	}
	if selected.ID != snapshot.credential.ID || selected.APIKey != snapshot.credential.APIKey {
		t.Fatalf("fresh snapshot resolved a different credential: %+v", selected)
	}
	key := snapshot.credential.ID + "\x00" + alias
	if !s.modelTestStarted(key) {
		t.Fatalf("could not reserve the model-test slot")
	}
	record := modelTestRecord{
		ModelID:       alias,
		UpstreamModel: snapshot.upstreamModel,
		Status:        "success",
		TestedAt:      time.Now().UTC(),
	}
	s.modelTestFinished(key, record, identityFromSnapshot(snapshot, alias))
	s.mu.RLock()
	stored, exists := s.modelTests[key]
	s.mu.RUnlock()
	if !exists || stored.Status != "success" {
		t.Fatalf("fresh model-test result was not stored: exists=%v record=%+v", exists, stored)
	}
}

// The credential epoch must fence ABA key rotation and disable->enable, not just
// a single replacement: the live key/disabled value can look identical to the
// snapshot while the credential was nevertheless replaced in between.
func TestCredentialABAFencesModelTestSnapshot(t *testing.T) {
	const (
		alias    = "consistency-alias"
		upstream = "consistency-upstream"
	)
	t.Run("key A to B to A", func(t *testing.T) {
		s, host := newTestService(t)
		const keyA = "plain-epoch-key-a-0001"
		id := addCredential(t, s, host, "primary", keyA)
		seedConsistencyMapping(t, s, alias, upstream)
		snapshot, err := s.modelTestSnapshot(id, alias)
		if err != nil {
			t.Fatalf("snapshot before ABA: %v", err)
		}
		if snapshot.credential.APIKey != keyA {
			t.Fatalf("snapshot api key = %q", snapshot.credential.APIKey)
		}
		if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
			"id": id, "api_key": "plain-epoch-key-b-0002",
		}); status != http.StatusOK {
			t.Fatalf("rotate to B: %d %s", status, raw)
		}
		if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
			"id": id, "api_key": keyA,
		}); status != http.StatusOK {
			t.Fatalf("rotate back to A: %d %s", status, raw)
		}
		current, ok := liveCredential(s, id)
		if !ok || current.APIKey != keyA {
			t.Fatalf("ABA rotation did not restore the live key: %+v", current)
		}
		if current.runtimeEpoch == snapshot.credential.runtimeEpoch {
			t.Fatalf("ABA rotation did not advance the credential epoch (%d)", current.runtimeEpoch)
		}
		assertStaleSnapshotFenced(t, s, snapshot, alias)

		fresh, err := s.modelTestSnapshot(id, alias)
		if err != nil {
			t.Fatalf("fresh snapshot after ABA: %v", err)
		}
		assertFreshSnapshotAccepted(t, s, fresh, alias)
	})

	t.Run("disable then enable", func(t *testing.T) {
		s, host := newTestService(t)
		id := addCredential(t, s, host, "primary", "plain-epoch-disable-key-0003")
		seedConsistencyMapping(t, s, alias, upstream)
		snapshot, err := s.modelTestSnapshot(id, alias)
		if err != nil {
			t.Fatalf("snapshot before disable: %v", err)
		}
		if snapshot.credential.Disabled {
			t.Fatalf("snapshot unexpectedly disabled")
		}
		for _, disabled := range []bool{true, false} {
			if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
				"id": id, "disabled": disabled,
			}); status != http.StatusOK {
				t.Fatalf("set disabled=%v: %d %s", disabled, status, raw)
			}
		}
		current, ok := liveCredential(s, id)
		if !ok || current.Disabled {
			t.Fatalf("disable->enable did not restore the live state: %+v", current)
		}
		if current.runtimeEpoch == snapshot.credential.runtimeEpoch {
			t.Fatalf("disable->enable did not advance the credential epoch (%d)", current.runtimeEpoch)
		}
		assertStaleSnapshotFenced(t, s, snapshot, alias)

		fresh, err := s.modelTestSnapshot(id, alias)
		if err != nil {
			t.Fatalf("fresh snapshot after enable: %v", err)
		}
		assertFreshSnapshotAccepted(t, s, fresh, alias)
	})
}

// observedThenErrorHost fills the host response object with a real HTTP status
// before failing the callback, which is what happens when a host hands back
// headers but the transport then reports an error.
type observedThenErrorHost struct {
	*fakeHost
	status int
}

func (h *observedThenErrorHost) call(method string, in, out any) error {
	if method == "host.http.do" {
		assign(out, map[string]any{"StatusCode": h.status, "Headers": http.Header{}, "Body": []byte(nil)})
	}
	return h.fakeHost.call(method, in, out)
}

// lateOpenHost parks host.http.operation_open until the test releases it, so the
// operation tracker can be closed before the host returns an operation id.
type lateOpenHost struct {
	*fakeHost
	opened  chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *lateOpenHost) call(method string, in, out any) error {
	if method == "host.http.operation_open" {
		h.once.Do(func() { close(h.opened) })
		<-h.release
	}
	return h.fakeHost.call(method, in, out)
}

func TestManagementConsistencyTransportAttemptSemantics(t *testing.T) {
	t.Run("a real transport call returning plugin_unavailable stays attempted", func(t *testing.T) {
		s, host := newTestService(t)
		id := seedModels(t, s, host)
		host.doResponses = append(host.doResponses, fakeHTTPResponse{Err: &APIError{
			Status: http.StatusServiceUnavailable, Kind: "plugin_unavailable", Message: "OpenDesign plugin is shutting down",
		}})
		_, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
		if err == nil {
			t.Fatalf("transport APIError was treated as success")
		}
		var apiErr *APIError
		if !asAPIError(err, &apiErr) || !apiErr.UpstreamAttempted() {
			t.Fatalf("transport APIError lost its upstream attempt: %v", err)
		}
		if apiErr.UpstreamHTTPStatus() != 0 {
			t.Fatalf("transport APIError invented an upstream status: %d", apiErr.UpstreamHTTPStatus())
		}
		logs, total, _ := s.requestLogs.query(requestLogFilter{limit: 10})
		if total != 1 || len(logs) != 1 {
			t.Fatalf("transport failure logs = %#v total=%d", logs, total)
		}
		entry := logs[0]
		if entry.UpstreamAttempted == nil || !*entry.UpstreamAttempted {
			t.Fatalf("request log lost the attempted transport call: %#v", entry)
		}
		if entry.ErrorKind == nil || *entry.ErrorKind != "upstream_transport_error" {
			t.Fatalf("request log error kind = %#v", entry.ErrorKind)
		}
		if entry.HTTPStatus != nil {
			t.Fatalf("transport failure stored an invented HTTP status: %d", *entry.HTTPStatus)
		}

		// The explicit model test must make the same claim.
		s2, host2 := newTestService(t)
		id2 := addCredential(t, s2, host2, "primary", "plain-transport-attempt-key")
		seedConsistencyMapping(t, s2, "open-design-attempt", "od/attempt")
		host2.doResponses = append(host2.doResponses, fakeHTTPResponse{Err: &APIError{
			Status: http.StatusServiceUnavailable, Kind: "plugin_unavailable", Message: "OpenDesign plugin is shutting down",
		}})
		status, body, raw := managementCall(t, s2, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
			"credential_id": id2, "model_id": "open-design-attempt",
		})
		if status != http.StatusOK {
			t.Fatalf("model-test status = %d body = %s", status, raw)
		}
		if body["status"] != "error" || body["upstream_attempted"] != true || body["real_inference"] != true {
			t.Fatalf("model-test lost the attempted transport call: %s", raw)
		}
	})

	t.Run("an observed 429 survives a host error in the model-test response and log", func(t *testing.T) {
		s, host := newTestService(t)
		id := addCredential(t, s, host, "primary", "plain-observed-status-key")
		seedConsistencyMapping(t, s, "open-design-observed", "od/observed")
		s.SetHost((&observedThenErrorHost{fakeHost: host, status: http.StatusTooManyRequests}).call)
		host.doResponses = append(host.doResponses, fakeHTTPResponse{Err: errString("host connection broke after headers")})

		status, body, raw := managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
			"credential_id": id, "model_id": "open-design-observed",
		})
		if status != http.StatusOK {
			t.Fatalf("model-test status = %d body = %s", status, raw)
		}
		if body["status"] != "error" || body["upstream_attempted"] != true {
			t.Fatalf("model-test error shape = %s", raw)
		}
		if observed, ok := body["http_status"].(float64); !ok || observed != http.StatusTooManyRequests {
			t.Fatalf("model-test http_status = %#v, want the already received 429", body["http_status"])
		}
		logs, total, _ := s.requestLogs.query(requestLogFilter{limit: 10})
		if total != 1 || len(logs) != 1 {
			t.Fatalf("observed-status logs = %#v total=%d", logs, total)
		}
		if logs[0].HTTPStatus == nil || *logs[0].HTTPStatus != http.StatusTooManyRequests {
			t.Fatalf("request log dropped the received 429: %#v", logs[0])
		}
		if logs[0].UpstreamAttempted == nil || !*logs[0].UpstreamAttempted {
			t.Fatalf("request log lost the attempt after the host error: %#v", logs[0])
		}
	})

	t.Run("cancellation during operation_open is not an attempt", func(t *testing.T) {
		s, host := newTestService(t)
		id := addCredential(t, s, host, "primary", "plain-late-open-key")
		seedConsistencyMapping(t, s, "open-design-late", "od/late")
		late := &lateOpenHost{fakeHost: host, opened: make(chan struct{}), release: make(chan struct{})}
		s.SetHost(late.call)

		request := mustJSON(t, ManagementRequest{
			Method: http.MethodPost,
			Path:   apiBase + "/model-test",
			Body:   mustJSON(t, map[string]any{"credential_id": id, "model_id": "open-design-late"}),
		})
		resultCh := make(chan ManagementResponse, 1)
		go func() {
			result, err := s.Handle("management.handle", request)
			if err != nil {
				resultCh <- ManagementResponse{StatusCode: http.StatusInternalServerError, Body: []byte(err.Error())}
				return
			}
			resultCh <- result.(ManagementResponse)
		}()
		select {
		case <-late.opened:
		case <-time.After(5 * time.Second):
			t.Fatal("operation_open was never entered")
		}
		// Same cancellation quiesce performs: the tracker closes before the host
		// returns an operation id, so host.http.do must never start.
		s.cancelOperations()
		close(late.release)
		var response ManagementResponse
		select {
		case response = <-resultCh:
		case <-time.After(5 * time.Second):
			t.Fatal("model-test did not return after the late operation id")
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body, &body); err != nil {
			t.Fatalf("decode model-test body: %v (%s)", err, response.Body)
		}
		if body["upstream_attempted"] != false || body["real_inference"] != false {
			t.Fatalf("operation_open cancellation claimed an attempt: %s", response.Body)
		}
		errorView, _ := body["error"].(map[string]any)
		if errorView["kind"] != "plugin_unavailable" {
			t.Fatalf("model-test error kind = %#v, want plugin_unavailable", errorView["kind"])
		}
		if body["http_status"] != nil {
			t.Fatalf("operation_open cancellation invented an HTTP status: %#v", body["http_status"])
		}
		if host.countMethod("host.http.do") != 0 {
			t.Fatalf("cancelled operation_open still reached host.http.do")
		}
		logs, total, _ := s.requestLogs.query(requestLogFilter{limit: 10})
		if total != 1 || len(logs) != 1 {
			t.Fatalf("late-open logs = %#v total=%d", logs, total)
		}
		entry := logs[0]
		if entry.UpstreamAttempted == nil || *entry.UpstreamAttempted {
			t.Fatalf("late-open log claimed an upstream attempt: %#v", entry)
		}
		if entry.ErrorKind == nil || *entry.ErrorKind != "plugin_unavailable" {
			t.Fatalf("late-open log error kind = %#v", entry.ErrorKind)
		}
		if entry.HTTPStatus != nil {
			t.Fatalf("late-open log invented an HTTP status: %d", *entry.HTTPStatus)
		}
	})
}

func TestManagementConsistencyShutdownRejectsDynamicManagement(t *testing.T) {
	s, host := newTestService(t)
	addCredential(t, s, host, "primary", "plain-shutdown-key-0001")
	if _, err := s.Handle("plugin.quiesce", nil); err != nil {
		t.Fatalf("quiesce: %v", err)
	}

	// The static console entry points are not dynamic management work.
	for _, path := range []string{consoleResource, consolePath} {
		status, _, raw := managementCall(t, s, http.MethodGet, path, nil, nil)
		if status != http.StatusOK || !strings.Contains(raw, "OpenDesign") {
			t.Fatalf("static console %s after shutdown: status=%d body=%s", path, status, raw)
		}
	}

	for _, route := range managementRoutes() {
		if route.Path == consoleResource || route.Path == consolePath {
			continue
		}
		route := route
		t.Run(route.Method+" "+route.Path, func(t *testing.T) {
			status, body, raw := managementCall(t, s, route.Method, route.Path,
				map[string][]string{"credential_id": {"any"}, "id": {"any"}}, map[string]any{})
			if status != http.StatusServiceUnavailable {
				t.Fatalf("%s %s after shutdown status = %d, want 503: %s", route.Method, route.Path, status, raw)
			}
			if body["error"] == nil {
				t.Fatalf("%s %s did not return a management error envelope: %s", route.Method, route.Path, raw)
			}
		})
	}
}

// quiesce must wait for a handler that is already inside host.auth.save. The
// save is not a cancellable HTTP operation, so returning early would let it
// publish after shutdown claimed to be complete.
func TestManagementConsistencyQuiesceWaitsForBlockedAuthSave(t *testing.T) {
	s, host := newTestService(t)
	id := addCredential(t, s, host, "primary", "plain-quiesce-save-key-0001")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	host.mu.Lock()
	host.saveHook = func(string, []byte) {
		once.Do(func() { close(entered) })
		<-release
	}
	host.mu.Unlock()

	request := mustJSON(t, ManagementRequest{
		Method: http.MethodPost,
		Path:   apiBase + "/credentials",
		Body:   mustJSON(t, map[string]any{"id": id, "label": "updated-while-shutting-down"}),
	})
	type saveResult struct {
		status int
		body   string
	}
	saveDone := make(chan saveResult, 1)
	go func() {
		result, err := s.Handle("management.handle", request)
		if err != nil {
			saveDone <- saveResult{status: http.StatusInternalServerError, body: err.Error()}
			return
		}
		response := result.(ManagementResponse)
		saveDone <- saveResult{status: response.StatusCode, body: string(response.Body)}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("host.auth.save was never entered")
	}

	quiesceDone := make(chan error, 1)
	go func() {
		_, err := s.Handle("plugin.quiesce", nil)
		quiesceDone <- err
	}()
	select {
	case err := <-quiesceDone:
		t.Fatalf("quiesce returned while host.auth.save was still blocked: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case result := <-saveDone:
		if result.status != http.StatusOK {
			t.Fatalf("blocked save status = %d body = %s", result.status, result.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked credential save did not complete after release")
	}
	select {
	case err := <-quiesceDone:
		if err != nil {
			t.Fatalf("quiesce after blocked save: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("quiesce did not finish after the save was released")
	}
	live, ok := liveCredential(s, id)
	if !ok || live.Label != "updated-while-shutting-down" {
		t.Fatalf("save released during shutdown was not published: %+v", live)
	}
	var stored Credential
	if err := json.Unmarshal(host.savedAuth(id+".json"), &stored); err != nil {
		t.Fatalf("decode stored credential: %v", err)
	}
	if stored.Label != "updated-while-shutting-down" {
		t.Fatalf("released save never reached the host: %q", stored.Label)
	}
}

// Arbitrary machine-shaped codes and message prefixes from an upstream body must
// be folded into the controlled allowlist, while a real transport error must not
// carry its URL, workspace or key into any management response or runtime view.
func TestManagementConsistencyErrorKindWhitelistAndDiagnosticPrivacy(t *testing.T) {
	t.Run("arbitrary codes and message prefixes fall back", func(t *testing.T) {
		s, host := newTestService(t)
		id := seedModels(t, s, host)
		cases := []struct {
			name     string
			body     string
			wantKind string
		}{
			{"structured unknown", `{"error":{"code":"totally_unknown_code","message":"rejected"}}`, "upstream_error"},
			{"message prefix unknown", `{"error":{"message":"totally_unknown_code: rejected"}}`, "upstream_error"},
			{"allowlisted code preserved", `{"error":{"code":"context_length_exceeded","message":"too long"}}`, "context_length_exceeded"},
		}
		for _, tc := range cases {
			host.doResponses = append(host.doResponses, fakeHTTPResponse{Status: http.StatusBadRequest, Body: []byte(tc.body)})
			_, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)})
			if err == nil {
				t.Fatalf("%s: upstream error was treated as success", tc.name)
			}
			if kind := streamErrorCode(err); kind != tc.wantKind {
				t.Fatalf("%s: error kind = %q, want %q", tc.name, kind, tc.wantKind)
			}
			if strings.Contains(err.Error(), "totally_unknown_code") {
				t.Fatalf("%s: arbitrary code leaked into the message: %v", tc.name, err)
			}
		}
		logs, total, _ := s.requestLogs.query(requestLogFilter{limit: 10})
		if total != 3 {
			t.Fatalf("log rows = %d, want 3", total)
		}
		if logs[0].ErrorKind == nil || *logs[0].ErrorKind != "context_length_exceeded" {
			t.Fatalf("allowlisted code was not preserved in the log: %#v", logs[0].ErrorKind)
		}
		for _, entry := range logs[1:] {
			if entry.ErrorKind == nil || *entry.ErrorKind != "upstream_error" {
				t.Fatalf("arbitrary code reached the log: %#v", entry.ErrorKind)
			}
		}

		s2, host2 := newTestService(t)
		id2 := seedModels(t, s2, host2)
		host2.doResponses = append(host2.doResponses, fakeHTTPResponse{
			Status: http.StatusBadRequest,
			Body:   []byte(`{"error":{"code":"totally_unknown_code","message":"rejected"}}`),
		})
		status, body, raw := managementCall(t, s2, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
			"credential_id": id2, "model_id": "od-large",
		})
		if status != http.StatusOK {
			t.Fatalf("model-test status = %d body = %s", status, raw)
		}
		errorView, _ := body["error"].(map[string]any)
		if errorView["kind"] != "upstream_error" || strings.Contains(raw, "totally_unknown_code") {
			t.Fatalf("model-test let an arbitrary code through: %s", raw)
		}

		s3, host3 := newTestService(t)
		id3 := seedModels(t, s3, host3)
		host3.doStreams = append(host3.doStreams, scriptedStream{
			status: http.StatusOK,
			chunks: [][]byte{[]byte(`data: {"error":{"code":"totally_unknown_code","message":"rejected"}}` + "\n\n")},
		})
		if _, err := executeStream(t, s3, executorPayload{AuthID: id3, Model: "od-large", StreamID: "out-whitelist", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err != nil {
			t.Fatalf("execute_stream: %v", err)
		}
		s3.waitIdle()
		closePayload, detail := closeDetail(t, host3)
		if detail["code"] != "upstream_error" {
			t.Fatalf("stream error_detail.code = %#v, want upstream_error", detail["code"])
		}
		message, _ := closePayload["error"].(string)
		if strings.Contains(message, "totally_unknown_code") || strings.Contains(join(host3.emitted()), "totally_unknown_code") {
			t.Fatalf("stream close leaked the arbitrary code: %v", closePayload)
		}
	})

	t.Run("transport errors keep URL workspace and keys out of management", func(t *testing.T) {
		s, host := newTestService(t)
		const apiKey = "sk-management-diagnostic-key-0001"
		const workspace = "ws-diagnostic-private-0002"
		id := addCredential(t, s, host, "primary", apiKey)
		if status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/credentials", nil, map[string]any{
			"id": id, "workspace_id": workspace,
		}); status != http.StatusOK {
			t.Fatalf("save workspace: %d %s", status, raw)
		}
		seedConsistencyMapping(t, s, "od-large", "opendesign-large")
		transportURL := DefaultBaseURL + "/chat/completions"
		leakText := "POST " + transportURL + " failed for workspace " + workspace + " with Bearer " + apiKey
		secrets := []string{apiKey, workspace}

		host.doResponses = append(host.doResponses, fakeHTTPResponse{Err: errString(leakText)})
		status, _, raw := managementCall(t, s, http.MethodPost, apiBase+"/model-test", nil, map[string]any{
			"credential_id": id, "model_id": "od-large",
		})
		if status != http.StatusOK {
			t.Fatalf("model-test status = %d body = %s", status, raw)
		}
		for _, forbidden := range []string{transportURL, "chat/completions"} {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("model-test response leaked transport URL %q: %s", forbidden, raw)
			}
		}
		assertNoSecretFragments(t, "model-test response", raw, secrets...)

		host.doResponses = append(host.doResponses, fakeHTTPResponse{Err: errString(leakText)})
		if _, err := execute(t, s, executorPayload{AuthID: id, Model: "od-large", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}); err == nil {
			t.Fatalf("transport failure was treated as success")
		}

		host.doResponses = append(host.doResponses, fakeHTTPResponse{Err: errString(leakText)})
		status, body, raw := managementCall(t, s, http.MethodGet, apiBase+"/upstream-models",
			map[string][]string{"credential_id": {id}}, nil)
		if status != http.StatusBadGateway {
			t.Fatalf("upstream-models transport status = %d body = %s", status, raw)
		}
		if body["source"] != "upstream" || body["code"] != "upstream_transport_error" {
			t.Fatalf("upstream-models transport shape = %s", raw)
		}
		for _, forbidden := range []string{transportURL, "chat/completions"} {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("upstream-models response leaked transport URL %q: %s", forbidden, raw)
			}
		}
		assertNoSecretFragments(t, "upstream-models response", raw, secrets...)

		status, logsBody, raw := managementCall(t, s, http.MethodGet, apiBase+"/request-logs", nil, nil)
		if status != http.StatusOK {
			t.Fatalf("request-logs status = %d body = %s", status, raw)
		}
		logs, _ := logsBody["logs"].([]any)
		if len(logs) < 2 {
			t.Fatalf("request logs = %#v, want model-test and executor rows", logsBody["logs"])
		}
		for _, forbidden := range []string{transportURL, "chat/completions"} {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("request log leaked transport URL %q: %s", forbidden, raw)
			}
		}
		assertNoSecretFragments(t, "request log response", raw, secrets...)

		status, runtimeBody, raw := managementCall(t, s, http.MethodGet, apiBase+"/runtime", nil, nil)
		if status != http.StatusOK {
			t.Fatalf("runtime status = %d body = %s", status, raw)
		}
		for _, forbidden := range []string{transportURL, "chat/completions"} {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("runtime leaked transport URL %q: %s", forbidden, raw)
			}
		}
		assertNoSecretFragments(t, "runtime response", raw, secrets...)
		accounts, _ := runtimeBody["credentials"].([]any)
		if len(accounts) != 1 {
			t.Fatalf("runtime accounts = %#v", runtimeBody["credentials"])
		}
		account := accounts[0].(map[string]any)
		lastTest, _ := account["last_test"].(map[string]any)
		if lastTest == nil || lastTest["error_kind"] != "upstream_transport_error" {
			t.Fatalf("runtime last_test = %#v", account["last_test"])
		}
		if message, _ := lastTest["error_message"].(string); strings.Contains(message, transportURL) || strings.Contains(message, workspace) {
			t.Fatalf("runtime last_test echoed transport diagnostics: %q", message)
		}
	})
}
