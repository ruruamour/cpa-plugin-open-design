package opendesign

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// Service holds plugin state. It is deliberately small: credentials, model
// mappings and the in-flight upstream/output streams needed to shut down cleanly.
// Nothing here persists request or response bodies; request logs persist only
// the explicitly allowlisted metadata fields.
type Service struct {
	mu           sync.RWMutex
	lifecycleMu  sync.Mutex
	configMu     sync.Mutex
	credentialMu sync.Mutex
	authFileMu   sync.Mutex
	// pendingMu guards pendingSaves only. It is deliberately separate from
	// authFileMu: host.auth.save re-enters auth.parse on the same goroutine, and
	// auth.parse takes authFileMu, so holding it across the host call deadlocks.
	pendingMu    sync.Mutex
	pendingSaves map[string]pendingSave

	cfg                 Config
	creds               map[string]Credential
	billingEpoch        map[string]uint64 // guarded by mu; changes on either key/workspace replacement or clear/delete
	quota               map[string]*quotaEntry
	quotaSlots          chan struct{}
	quotaNow            func() time.Time
	quotaBudget         time.Duration
	modelCoverage       map[modelCoverageKey]*modelCoverageEntry
	modelCoverageSlots  chan struct{}
	modelCoverageNow    func() time.Time
	modelCoverageBudget time.Duration
	authFiles           map[string]string
	revoked             map[string]bool
	errorRules          []RequestErrorRule
	errorRulesLoadError string
	errorRuleSynced     map[string]string
	errorRuleSyncErrors map[string]string
	quotaPauseErrors    map[string]string // fixed, redacted persistence failures; guarded by mu
	quotaRecheckStatus  map[string]string // fixed recovery error codes only; guarded by mu
	// quotaRecheckRetryAt delays the next automatic recheck after a failed round.
	// quotaRecheckFailures escalates that delay, so a multi-day coding-plan
	// exhaustion settles into slow polling instead of hitting the official API
	// every ticker. Guarded by mu; a manual recheck ignores both.
	quotaRecheckRetryAt  map[string]time.Time
	quotaRecheckFailures map[string]int
	quotaPauseWake       chan struct{}
	quotaPauseStarted    bool
	quotaPauseWorker     sync.WaitGroup
	requestLogs          *requestLogStore
	modelTests           map[string]modelTestRecord
	modelTestRunning     map[string]int
	modelMappingRevision uint64
	credentialEpoch      map[string]uint64
	host                 HostCall
	authDir              string
	streams              map[string]struct{}
	ops                  *operationTracker
	loaded               bool
	stopped              bool
	active               sync.WaitGroup
	activeN              atomic.Int64
	stopCh               chan struct{}

	// quotaInflight counts admitted inferences per credential key. It is a
	// recovery fence for the parent session's automatic resume, never a
	// concurrency limit; guarded by mu.
	quotaInflight map[quotaInFlightKey]int
	// Recovery checks are serialized across accounts without limiting inference.
	quotaRecoveryGate chan struct{}
	// quotaCommitGate serializes a quota-file commit with quiesce marking the
	// plugin stopped. It is never taken on an inference path, so it adds no
	// concurrency limit or queue to requests.
	quotaCommitGate chan struct{}
	// pauseGeneration changes whenever the effective pause policy changes.
	// Automatic recheck rounds capture it and abort before writing when the
	// operator toggled the policy (including off -> on) meanwhile. Guarded by mu.
	pauseGeneration uint64
	// quotaRecoveryBudget bounds one automatic/manual multi-model round.
	quotaRecoveryBudget time.Duration
	// quotaInFlightTestHook is nil in production. It is per instance so a test
	// can observe admission/release interleaving without any global state.
	quotaInFlightTestHook atomic.Pointer[quotaInFlightTestHook]
}

// NewService returns an unconfigured plugin instance.
func NewService() *Service {
	requestLogs := newRequestLogStore()
	s := &Service{
		cfg:                  defaultConfig(),
		creds:                map[string]Credential{},
		billingEpoch:         map[string]uint64{},
		quota:                map[string]*quotaEntry{},
		quotaSlots:           make(chan struct{}, 3),
		quotaNow:             time.Now,
		quotaBudget:          quotaRefreshBudget,
		modelCoverage:        map[modelCoverageKey]*modelCoverageEntry{},
		modelCoverageSlots:   make(chan struct{}, 3),
		modelCoverageNow:     time.Now,
		modelCoverageBudget:  modelCoverageBudget,
		authFiles:            map[string]string{},
		revoked:              map[string]bool{},
		errorRules:           defaultErrorRules(),
		errorRuleSynced:      map[string]string{},
		errorRuleSyncErrors:  map[string]string{},
		quotaPauseErrors:     map[string]string{},
		quotaRecheckStatus:   map[string]string{},
		quotaRecheckRetryAt:  map[string]time.Time{},
		quotaRecheckFailures: map[string]int{},
		quotaInflight:        map[quotaInFlightKey]int{},
		quotaRecoveryGate:    make(chan struct{}, 1),
		quotaCommitGate:      make(chan struct{}, 1),
		quotaRecoveryBudget:  quotaRecoveryBudget,
		quotaPauseWake:       make(chan struct{}, 1),
		requestLogs:          requestLogs,
		modelTests:           map[string]modelTestRecord{},
		modelTestRunning:     map[string]int{},
		credentialEpoch:      map[string]uint64{},
		streams:              map[string]struct{}{},
		pendingSaves:         map[string]pendingSave{},
		ops:                  newOperationTracker(),
		stopCh:               make(chan struct{}),
	}
	requestLogs.keys = s.metadataSecrets
	return s
}

// SetHost installs the host callback bridge provided by cmd/passbridge.
func (s *Service) SetHost(h func(string, any, any) error) {
	s.mu.Lock()
	s.host = h
	s.mu.Unlock()
}

func (s *Service) call(method string, in, out any) error {
	s.mu.RLock()
	h := s.host
	s.mu.RUnlock()
	if h == nil {
		return errors.New("host callback is not initialized")
	}
	return h(method, in, out)
}

func (s *Service) config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	c.Models = make([]Model, len(s.cfg.Models))
	for i := range s.cfg.Models {
		c.Models[i] = cloneModel(s.cfg.Models[i])
	}
	return c
}

// cloneModel copies a Model including its optional reasoning policy. Without the
// deep copy a caller would share the slice backing the published configuration,
// so cfg.validate() rewriting Rules[i].From/To would race a concurrent reader
// (and a concurrent save) on the same published slice.
func cloneModel(m Model) Model {
	m.Reasoning = cloneReasoningMapping(m.Reasoning)
	return m
}

// Handle dispatches one plugin RPC. Only methods we actually implement are
// advertised, so an unsupported method is a client bug, not a fallback path.
func (s *Service) Handle(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := s.configure(raw); err != nil {
			return nil, err
		}
		return registration(), nil
	case "auth.identifier", "executor.identifier":
		return map[string]any{"identifier": Provider}, nil
	case "auth.parse":
		return s.parseAuth(raw)
	case "auth.refresh":
		return s.refreshAuth(raw)
	case "auth.login.start":
		return nil, fail(http.StatusBadRequest, "interactive login is not supported; add an OpenDesign API key instead")
	case "auth.login.poll":
		return map[string]any{"Status": "error", "Message": "Use an OpenDesign API key"}, nil
	case "model.static", "model.for_auth":
		return s.modelRegistration(), nil
	case "executor.execute":
		return s.execute(raw)
	case "executor.execute_stream":
		return s.executeStream(raw)
	case "executor.count_tokens":
		// Stock CPA treats a missing counter as availability-neutral. Returning
		// 501 here would cool down a perfectly usable credential.
		return nil, &APIError{Status: http.StatusNotFound, Kind: "count_tokens_unavailable", Message: "OpenDesign does not expose a token counting endpoint"}
	case "executor.http_request":
		return nil, fail(http.StatusBadRequest, "use the OpenDesign model executor")
	case "management.register":
		return s.registerManagement()
	case "management.handle":
		return s.management(raw)
	case "plugin.quiesce", "plugin.shutdown":
		return s.quiesce()
	default:
		return nil, fail(http.StatusBadRequest, "unsupported plugin method: "+method)
	}
}

// registration reports the capabilities this plugin really implements. Quota and
// model registrar capabilities are intentionally absent.
func registration() any {
	return map[string]any{
		"schema_version": 6,
		"metadata": map[string]any{
			"Name":             "OpenDesign",
			"Version":          Version,
			"Author":           Author,
			"GitHubRepository": Repository,
			"Description":      "OpenDesign (amr-link) OpenAI-compatible provider for CLIProxyAPI",
			"ConfigFields": []map[string]any{
				{"Name": "data_dir", "Type": "string", "Description": "Persistent plugin state directory"},
			},
		},
		"capabilities": map[string]any{
			"auth_provider":           true,
			"model_provider":          true,
			"executor":                true,
			"executor_model_scope":    "oauth",
			"executor_input_formats":  []string{"chat-completions"},
			"executor_output_formats": []string{"chat-completions"},
			"management_api":          true,
		},
	}
}

func (s *Service) configure(raw json.RawMessage) (err error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.configMu.Lock()
	defer s.configMu.Unlock()
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if err = yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return err
		}
	} else if s.loaded {
		// A reconfigure without YAML (e.g. a host reload) must not reset the data
		// directory the instance is already using.
		cfg.DataDir = s.config().DataDir
	}
	// UI-owned state (model mappings) lives next to the plugin data, never keys.
	if b, readErr := os.ReadFile(filepath.Join(cfg.DataDir, settingsFile)); readErr == nil {
		if err = json.Unmarshal(b, &cfg); err != nil {
			return err
		}
	}
	if err = cfg.validate(); err != nil {
		return err
	}
	if err = os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	s.mu.Lock()
	if s.loaded && filepath.Clean(cfg.DataDir) != filepath.Clean(s.cfg.DataDir) {
		s.mu.Unlock()
		return fail(http.StatusConflict, "changing data_dir requires restarting the plugin with its complete data directory")
	}
	mappingChanged := s.loaded && !sameModels(s.cfg.Models, cfg.Models)
	if mappingChanged {
		s.modelMappingRevision++
		s.clearModelTestsLocked()
	}
	if s.stopped {
		// The host re-registers a quiesced instance when replacement fails.
		s.stopped = false
		s.stopCh = make(chan struct{})
		s.quotaPauseStarted = false
		s.ops.reopen()
		// A round that started before quiesce must not resume under the new
		// lifecycle.
		s.pauseGeneration++
	}
	// Compare the effective policy values, not the *bool pointers: the host
	// re-registers the plugin (and this re-runs) with a freshly decoded config,
	// so pointer identity would abort an automatic round on every re-register.
	if s.cfg.pauseSettings() != cfg.pauseSettings() {
		s.pauseGeneration++
	}
	s.cfg = cfg
	s.loaded = true
	s.mu.Unlock()
	// A corrupt or unreadable log store must never block registration: the
	// store falls back to an empty, writable state. Loading happens outside the
	// service lock because it may parse up to the bounded snapshot and journal.
	_ = s.requestLogs.configure(cfg.DataDir, cfg.logSettings())
	// The retry-scheduling policy is operator-editable and lives next to the
	// settings. A corrupt file falls back to the built-in defaults and is
	// reported on GET /error-rules instead of blocking registration.
	rules, loadErr := loadErrorRules(cfg.DataDir)
	s.mu.Lock()
	s.errorRules = rules
	s.errorRulesLoadError = loadErr
	s.mu.Unlock()
	s.startQuotaPauseWorker()
	return nil
}

func (s *Service) withActive(fn func() (any, error)) (any, error) {
	if err := s.begin(); err != nil {
		return managementError(statusOf(err), err.Error())
	}
	defer s.done()
	return fn()
}

func (s *Service) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return localFail(http.StatusServiceUnavailable, "plugin_unavailable", "OpenDesign plugin is shutting down")
	}
	s.active.Add(1)
	s.activeN.Add(1)
	return nil
}

// done marks one in-flight handler finished. It is safe to call once per begin.
func (s *Service) done() {
	s.activeN.Add(-1)
	s.active.Done()
}

// waitIdle blocks until every in-flight handler has finished. Tests use it to
// observe the asynchronous stream pump deterministically.
func (s *Service) waitIdle() {
	s.active.Wait()
}

// quiesce stops accepting work, closes upstream and downstream streams, then
// waits for in-flight handlers and flushes bounded request metadata.
func (s *Service) quiesce() (any, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	// Serialize with a quota-file commit: once stopped is published, no commit
	// may still write a cleared block to disk. Holding the gate only while
	// publishing the stopped flag keeps the wait short and cannot deadlock: a
	// blocked commit re-checks stopped under the same gate and aborts.
	s.quotaCommitGate <- struct{}{}
	s.mu.Lock()
	if !s.stopped {
		close(s.stopCh)
	}
	s.stopped = true
	streams := make([]string, 0, len(s.streams))
	for id := range s.streams {
		streams = append(streams, id)
	}
	s.mu.Unlock()
	<-s.quotaCommitGate
	for _, id := range streams {
		s.closeUpstream(id)
	}
	// Cancel host operations too: a buffered callback or a stream still waiting
	// for headers blocks inside the host, where closing a stream id cannot help.
	s.cancelOperations()
	s.quotaPauseWorker.Wait()
	s.active.Wait()
	// Invalidate and join any deferred log task before the synchronous shutdown
	// flush, so an old task cannot write after this lifecycle boundary.
	s.requestLogs.cancelScheduledFlush()
	flushErr := s.requestLogs.flush()
	if flushErr != nil {
		return map[string]any{}, fail(http.StatusInternalServerError, "request log persistence failed during shutdown: "+flushErr.Error())
	}
	return map[string]any{}, nil
}

func (s *Service) saveSettings(cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(cfg.DataDir, settingsFile)
	tmp := path + ".tmp-" + newID()
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	// The swap goes through the same retrying publish as the request log, so a
	// transient Windows sharing violation on settings.json does not fail a save.
	if err := publishStagedFile(tmp, path); err != nil {
		return err
	}
	if hook := testSettingsWriteHook.Load(); hook != nil {
		(*hook)()
	}
	return nil
}

// testSettingsWriteHook lets a test park a writer between persist and publish to
// prove the two steps are atomic. It is nil in production; only tests set it.
var testSettingsWriteHook atomic.Pointer[func()]

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HostErrorDetails returns the client-safe message plus the HTTP status the host
// should surface. The signature is what cmd/passbridge expects.
func (s *Service) HostErrorDetails(err error) (string, int) {
	if err == nil {
		return "", http.StatusInternalServerError
	}
	return redact(err.Error(), s.allKeys()...), statusOf(err)
}
