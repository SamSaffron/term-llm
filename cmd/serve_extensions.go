package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/extensions"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Main config supplies only directory/legacy defaults. Enablement is written
// exclusively to extensions/extensions.yaml, never to credential-bearing config.
type serveExtensions struct {
	mu                   sync.Mutex
	manager              *extensions.Manager
	configPath           string
	directoryFlag        string
	enabledFlag          []string
	flagSet              bool
	disabled             bool
	activationGeneration string
}
type extensionSettingsState struct {
	directory string
	path      string
	revision  string
	data      []byte
	enabled   []string
	source    string
}

func newServeExtensions(dir string, ids []string, flagSet, disabled bool) (*serveExtensions, error) {
	p := viper.ConfigFileUsed()
	if p == "" {
		var err error
		p, err = config.GetConfigPath()
		if err != nil {
			return nil, err
		}
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	e := &serveExtensions{manager: extensions.NewManager(), configPath: p, directoryFlag: dir, enabledFlag: append([]string{}, ids...), flagSet: flagSet, disabled: disabled}
	if err = e.reload(); err != nil {
		return nil, fmt.Errorf("%w (use --disable-extensions to start without extensions)", err)
	}
	return e, nil
}
func configDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (e *serveExtensions) readConfig() ([]byte, config.ServeConfig, error) {
	b, err := os.ReadFile(e.configPath)
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return nil, config.ServeConfig{}, err
	}
	var v struct {
		Serve config.ServeConfig `yaml:"serve"`
	}
	if len(b) > 0 {
		err = yaml.Unmarshal(b, &v)
	}
	return b, v.Serve, err
}
func (e *serveExtensions) resolveDirectory(c config.ServeConfig) (string, error) {
	dir := c.ExtensionsDir
	if e.directoryFlag != "" {
		dir = e.directoryFlag
	}
	if dir == "" {
		base, err := config.GetConfigDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "extensions")
	}
	return filepath.Abs(dir)
}
func (e *serveExtensions) settingsState() (extensionSettingsState, error) {
	var state extensionSettingsState
	main, c, err := e.readConfig()
	if err != nil {
		return state, err
	}
	state.directory, err = e.resolveDirectory(c)
	if err != nil {
		return state, err
	}
	state.path = filepath.Join(state.directory, extensions.SettingsFile)
	state.source = "config"
	state.enabled = append([]string{}, c.Extensions...)
	exists := false
	if !e.disabled && !e.flagSet {
		state.data, exists, err = extensions.ReadSettings(state.directory)
		if err != nil {
			return state, err
		}
		if exists {
			settings, err := extensions.ParseSettings(state.data)
			if err != nil {
				return state, err
			}
			state.enabled = settings.Enabled
			state.source = "extension-file"
		}
	}
	if e.flagSet {
		state.enabled = append([]string{}, e.enabledFlag...)
		state.source = "command-line"
	}
	if e.disabled {
		state.enabled = []string{}
		state.source = "disabled by --disable-extensions"
	}
	state.revision = configDigest([]byte(fmt.Sprintf("%s\x00%t\x00%s\x00%s", state.directory, exists, main, state.data)))
	return state, nil
}
func (e *serveExtensions) reload() error { e.mu.Lock(); defer e.mu.Unlock(); return e.reloadLocked() }
func (e *serveExtensions) reloadLocked() error {
	state, err := e.settingsState()
	if err != nil {
		return err
	}
	if err := e.manager.Reload(state.directory, state.enabled, state.source, e.disabled, state.revision, state.path); err != nil {
		return err
	}
	// A plain rescan supersedes any pending activation, even when it restores
	// the content fingerprint of an earlier activated snapshot.
	e.activationGeneration = ""
	return nil
}

var errExtensionConflict = errors.New("extension settings changed; rescan the list before saving")

func (e *serveExtensions) save(ids []string, revision string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.flagSet || e.disabled {
		return fmt.Errorf("extensions are controlled by process flags; change the flags and restart")
	}
	if err := extensions.ValidateEnabled(ids); err != nil {
		return err
	}
	state, err := e.settingsState()
	if err != nil {
		return err
	}
	if state.revision != revision {
		return errExtensionConflict
	}
	data, err := extensions.SettingsBytes(state.data, ids)
	if err != nil {
		return err
	}
	latest, err := e.settingsState()
	if err != nil {
		return err
	}
	if latest.revision != revision {
		return errExtensionConflict
	}
	if err := extensions.WriteSettings(state.directory, data); err != nil {
		return err
	}
	return e.reloadLocked()
}
func (e *serveExtensions) authorizeBuilder(rt *serveRuntime) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	registry := rt.toolMgr.Registry
	// Clear first, including on config errors or a non-builtin agent override.
	if err := registry.SetExtensionDirectory("", e.configPath); err != nil {
		return err
	}
	if !rt.extensionBuilder {
		return nil
	}
	_, c, err := e.readConfig()
	if err != nil {
		return err
	}
	dir, err := e.resolveDirectory(c)
	if err != nil {
		return err
	}
	return registry.SetExtensionDirectory(dir, e.configPath)
}

// Activation is separate from rescan: ordinary file/config discovery must not
// unexpectedly reload connected browsers. A generation is activated only once
// it has been fully snapshotted without extension errors.
func (e *serveExtensions) activate() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disabled {
		return fmt.Errorf("extensions are disabled by a boot flag")
	}
	if err := e.reloadLocked(); err != nil {
		return err
	}
	s := e.manager.Status()
	if len(s.Errors) > 0 {
		return fmt.Errorf("repair extension errors before activation: %s", strings.Join(s.Errors, "; "))
	}
	e.activationGeneration = s.Generation
	return nil
}
func (e *serveExtensions) status() struct {
	extensions.Snapshot
	ActivationGeneration string `json:"activation_generation"`
} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return struct {
		extensions.Snapshot
		ActivationGeneration string `json:"activation_generation"`
	}{e.manager.Status(), e.activationGeneration}
}
func (s *serveServer) handleExtensionsActivate(w http.ResponseWriter, r *http.Request) {
	if !extensionMutation(w, r) {
		return
	}
	if err := s.extensions.activate(); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "extension_activation", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.extensions.status())
}

func (s *serveServer) registerExtensionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/extensions/activate", s.auth(s.cors(s.handleExtensionsActivate)))
	mux.HandleFunc("/admin/extensions/status", s.auth(s.cors(s.handleExtensionsStatus)))
	mux.HandleFunc("/admin/extensions/reload", s.auth(s.cors(s.handleExtensionsReload)))
	mux.HandleFunc("/admin/extensions/config", s.auth(s.cors(s.handleExtensionsConfig)))
	mux.HandleFunc("/extensions/", s.auth(s.handleExtensionAsset))
}
func (s *serveServer) handleExtensionsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.extensions.status())
}
func extensionMutation(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return false
	}
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request", err.Error())
		return false
	}
	return true
}
func (s *serveServer) handleExtensionsReload(w http.ResponseWriter, r *http.Request) {
	if !extensionMutation(w, r) {
		return
	}
	if err := s.extensions.reload(); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "extension_reload", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.extensions.status())
}
func (s *serveServer) handleExtensionsConfig(w http.ResponseWriter, r *http.Request) {
	if !extensionMutation(w, r) {
		return
	}
	var body struct {
		Enabled  []string `json:"enabled"`
		Revision string   `json:"revision"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeOpenAIError(w, 400, "invalid_request", err.Error())
		return
	}
	if err := s.extensions.save(body.Enabled, body.Revision); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errExtensionConflict) {
			status = http.StatusConflict
		}
		writeOpenAIError(w, status, "extension_config", err.Error())
		return
	}
	writeJSON(w, 200, s.extensions.status())
}
func (s *serveServer) handleExtensionAsset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(405)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/extensions/"), "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	data, ok := s.extensions.manager.Asset(parts[0], parts[1])
	if !ok {
		http.Error(w, "Extension asset unavailable. Reload the page to use the current extensions.", http.StatusGone)
		return
	}
	t := mime.TypeByExtension(path.Ext(parts[1]))
	if path.Ext(parts[1]) == ".js" || path.Ext(parts[1]) == ".mjs" {
		t = "text/javascript; charset=utf-8"
	}
	if t == "" {
		t = "application/octet-stream"
	}
	w.Header().Set("Content-Type", t)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}
