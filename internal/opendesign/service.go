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

	"gopkg.in/yaml.v3"
)

// Service holds plugin state. It is deliberately small: credentials, model
// mappings and the in-flight upstream/output streams needed to shut down cleanly.
// Nothing here persists request bodies, responses or log lines.
type Service struct {
	mu           sync.RWMutex
	configMu     sync.Mutex
	credentialMu sync.Mutex
	authFileMu   sync.Mutex

	cfg       Config
	creds     map[string]Credential
	authFiles map[string]string
	revoked   map[string]bool
	host      HostCall
	authDir   string
	streams   map[string]struct{}
	ops       *operationTracker
	loaded    bool
	stopped   bool
	active    sync.WaitGroup
	activeN   atomic.Int64
	stopCh    chan struct{}
}

// NewService returns an unconfigured plugin instance.
func NewService() *Service {
	return &Service{
		cfg:       defaultConfig(),
		creds:     map[string]Credential{},
		authFiles: map[string]string{},
		revoked:   map[string]bool{},
		streams:   map[string]struct{}{},
		ops:       newOperationTracker(),
		stopCh:    make(chan struct{}),
	}
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
	c.Models = append([]Model{}, c.Models...)
	return c
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
	defer s.mu.Unlock()
	if s.loaded && filepath.Clean(cfg.DataDir) != filepath.Clean(s.cfg.DataDir) {
		return fail(http.StatusConflict, "changing data_dir requires restarting the plugin with its complete data directory")
	}
	if s.stopped {
		// The host re-registers a quiesced instance when replacement fails.
		s.stopped = false
		s.stopCh = make(chan struct{})
		s.ops.reopen()
	}
	s.cfg = cfg
	s.loaded = true
	return nil
}

func (s *Service) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return fail(http.StatusServiceUnavailable, "OpenDesign plugin is shutting down")
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
// waits for in-flight handlers. It never persists anything.
func (s *Service) quiesce() (any, error) {
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
	for _, id := range streams {
		s.closeUpstream(id)
	}
	// Cancel host operations too: a buffered callback or a stream still waiting
	// for headers blocks inside the host, where closing a stream id cannot help.
	s.cancelOperations()
	s.active.Wait()
	return map[string]any{}, nil
}

func (s *Service) saveSettings(cfg Config) error {
	b, err := json.MarshalIndent(Config{DataDir: cfg.DataDir, BaseURL: cfg.BaseURL, Models: cfg.Models}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(cfg.DataDir, settingsFile)
	tmp := path + ".tmp-" + newID()
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, path)
}

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
