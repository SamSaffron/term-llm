package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/extensions"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
	"gopkg.in/yaml.v3"
)

func extensionFixture(t *testing.T) *serveExtensions {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "extensions")
	if err := os.MkdirAll(filepath.Join(root, "dracula"), 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{filepath.Join(root, "dracula", "extension.yaml"): "title: Dracula\nformat_version: 1\ncss: style.css\njs: main.js\n", filepath.Join(root, "dracula", "style.css"): "body { background: #282a36; }", filepath.Join(root, "dracula", "main.js"): "export function activate() {}", filepath.Join(dir, "config.yaml"): "# keep this comment\nprovider: test\nproviders:\n  test:\n    env:\n      MY_KEY: preserved\nserve:\n  extensions_dir: " + root + "\n  extensions: [dracula]\n"}
	for p, b := range files {
		if err := os.WriteFile(p, []byte(b), 0644); err != nil {
			t.Fatal(err)
		}
	}
	e := &serveExtensions{manager: extensions.NewManager(), configPath: filepath.Join(dir, "config.yaml")}
	if err := e.reload(); err != nil {
		t.Fatal(err)
	}
	return e
}
func TestExtensionsConfigPreservesAndConflicts(t *testing.T) {
	e := extensionFixture(t)
	s := e.manager.Status()
	before, _ := os.ReadFile(e.configPath)
	if err := e.save([]string{}, s.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(e.configPath)
	if !bytes.Equal(before, b) {
		t.Fatal("main config changed")
	}
	local, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(local), "enabled: []") {
		t.Fatalf("missing local enabled list: %s", local)
	}
	if !strings.Contains(string(b), "MY_KEY: preserved") || !strings.Contains(string(b), "# keep this comment") {
		t.Fatalf("config lost content: %s", b)
	}
	var v map[string]any
	if err := yaml.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(e.manager.Status().Enabled) != 0 {
		t.Fatal("empty list not persisted")
	}
	if err := e.save([]string{"dracula"}, s.ConfigRevision); !errors.Is(err, errExtensionConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	e.flagSet = true
	e.enabledFlag = []string{"dracula"}
	if err := e.reload(); err != nil {
		t.Fatal(err)
	}
	if e.manager.Status().Source != "command-line" || len(e.manager.Status().Enabled) != 1 {
		t.Fatal("CLI precedence")
	}
	if err := e.save(nil, e.manager.Status().ConfigRevision); err == nil {
		t.Fatal("overrode flags")
	}
}
func TestExtensionRoutesAndRecovery(t *testing.T) {
	e := extensionFixture(t)
	s := &serveServer{extensions: e}
	mux := http.NewServeMux()
	s.registerExtensionRoutes(mux)
	status := e.manager.Status()
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{{"GET", "/admin/extensions/status", "", 200}, {"GET", "/extensions/" + status.Generation + "/dracula/style.css", "", 200}, {"GET", "/extensions/old/dracula/style.css", "", 410}, {"GET", "/extensions/" + status.Generation + "/dracula/extension.yaml", "", 410}, {"GET", "/admin/extensions/reload", "", 405}, {"POST", "/admin/extensions/reload", "{}", 200}} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.method == "POST" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
		if strings.HasPrefix(tc.path, "/extensions/") && w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("asset cache policy")
		}
	}
	body, _ := json.Marshal(map[string]any{"enabled": []string{}, "revision": status.ConfigRevision})
	r := httptest.NewRequest("POST", "/admin/extensions/config", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if len(e.manager.Status().Enabled) != 0 {
		t.Fatal("save did not reload")
	}
}
func TestExtensionRoutesRequireAuth(t *testing.T) {
	s := &serveServer{cfg: serveServerConfig{requireAuth: true, token: "secret"}, extensions: extensionFixture(t)}
	mux := http.NewServeMux()
	s.registerExtensionRoutes(mux)
	for _, path := range []string{"/admin/extensions/status", "/admin/extensions/config", "/admin/extensions/activate", "/extensions/test/dracula/style.css"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
	}
}

// Exercise the real agent/tool runtime without a live model or user credentials:
// inspect source -> write extension -> enable in config -> reload snapshots.
func TestExtensionBuilderEngineInstallsAndEnablesExtension(t *testing.T) {
	e := extensionFixture(t)
	dir := e.manager.Status().Directory
	originalConfig, _ := os.ReadFile(e.configPath)
	provider := llm.NewMockProvider("mock").
		AddToolCall("source", tools.UIGetSourceToolName, map[string]any{"target": "compiled", "operation": "read", "path": "compiled/index.html", "start_line": 1, "end_line": 3}).
		AddToolCall("status", tools.UIExtensionsToolName, map[string]any{"operation": "status"}).
		AddToolCall("manifest", tools.WriteFileToolName, map[string]any{"path": filepath.Join(dir, "moon", "extension.yaml"), "content": "title: Moon\nformat_version: 1\ncss: style.css\n"}).
		AddToolCall("css", tools.WriteFileToolName, map[string]any{"path": filepath.Join(dir, "moon", "style.css"), "content": ":root { --bg: #282a36; --accent: #bd93f9; }"}).
		AddToolCall("enable", tools.WriteFileToolName, map[string]any{"path": filepath.Join(dir, extensions.SettingsFile), "content": "enabled: [dracula, moon]\n"}).
		AddToolCall("activate", tools.UIActivateToolName, map[string]any{}).AddTextResponse("Your moonlit interface is ready; automatic activation is requested.")
	var runtime *serveRuntime
	server := &serveServer{extensions: e, agentRuntimeFactory: func(_ context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		settings := SessionSettings{Tools: "ui_get_source,ui_extensions,ui_activate_extensions,read_file,write_file,edit_file", BaseDir: filepath.Dir(e.configPath), PrimaryWorkspace: filepath.Dir(e.configPath)}
		engine, mgr, err := newServeEngineWithTools(&config.Config{}, settings, provider, "mock", "mock-model", false, false, nil, nil)
		if err != nil {
			return nil, err
		}
		runtime = &serveRuntime{provider: provider, providerKey: "mock", defaultModel: "mock-model", agentName: "extension-builder", extensionBuilder: true, engine: engine, toolMgr: mgr}
		return runtime, nil
	}}
	rt, err := server.createRequestRuntime(context.Background(), serveRuntimeRequest{Provider: "mock", Model: "mock-model", Agent: "extension-builder"})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = rt.Run(ctx, false, false, []llm.Message{llm.UserText("Create and enable a Dracula-like theme")}, llm.Request{Model: "mock-model", MaxTurns: 10, Tools: rt.toolMgr.GetSpecs()})
	if err != nil {
		t.Fatal(err)
	}
	status := e.manager.Status()
	if e.status().ActivationGeneration != status.Generation {
		t.Fatal("builder did not activate the completed snapshot")
	}
	after, _ := os.ReadFile(e.configPath)
	if !bytes.Equal(originalConfig, after) {
		t.Fatal("builder changed main config")
	}
	if status.Source != "extension-file" {
		t.Fatalf("unexpected source %s", status.Source)
	}
	for _, cap := range rt.toolMgr.ApprovalMgr.WorkspaceCapabilities() {
		if cap.Primary && cap.Status != "proposed" {
			t.Fatal("extension grant confirmed the entire primary workspace")
		}
	}
	if len(status.Enabled) != 2 || status.Enabled[1] != "moon" {
		t.Fatalf("agent did not enable extension: %+v", status)
	}
	data, ok := e.manager.Asset(status.Generation, "moon/style.css")
	if !ok || !strings.Contains(string(data), "#bd93f9") {
		t.Fatalf("missing agent-authored snapshot: %s", data)
	}
	requests := provider.RecordedRequests()
	if len(requests) != 7 {
		t.Fatalf("tool loop turns = %d", len(requests))
	}
	transcript, _ := json.Marshal(requests[len(requests)-1].Messages)
	if !strings.Contains(string(transcript), "compiled-only") {
		t.Fatal("source tool result never reached agent")
	}
}

func TestExtensionLocalSettingsPrecedence(t *testing.T) {
	e := extensionFixture(t)
	if err := extensions.WriteSettings(e.manager.Status().Directory, []byte("enabled: []\n")); err != nil {
		t.Fatal(err)
	}
	if err := e.reload(); err != nil {
		t.Fatal(err)
	}
	if s := e.manager.Status(); len(s.Enabled) != 0 || s.Source != "extension-file" {
		t.Fatalf("local empty list ignored: %+v", s)
	}
	e.flagSet = true
	e.enabledFlag = []string{"dracula"}
	if err := e.reload(); err != nil {
		t.Fatal(err)
	}
	if s := e.manager.Status(); len(s.Enabled) != 1 || s.Source != "command-line" {
		t.Fatalf("flag ignored: %+v", s)
	}
	e.disabled = true
	if err := e.reload(); err != nil {
		t.Fatal(err)
	}
	if len(e.manager.Status().Enabled) != 0 {
		t.Fatal("kill switch ignored")
	}
}

func TestExtensionBuilderNameAloneDoesNotAuthorizeFiles(t *testing.T) {
	e := extensionFixture(t)
	settings := SessionSettings{Tools: "write_file", BaseDir: filepath.Dir(e.configPath), PrimaryWorkspace: filepath.Dir(e.configPath)}
	engine, mgr, err := newServeEngineWithTools(&config.Config{}, settings, llm.NewMockProvider("mock"), "mock", "mock-model", false, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := &serveRuntime{agentName: "extension-builder", engine: engine, toolMgr: mgr}
	defer rt.Close()
	if err := e.authorizeBuilder(rt); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.manager.Status().Directory, "unauthorized.css")
	if outcome, err := mgr.ApprovalMgr.CheckPathApprovalWithContext(context.Background(), tools.WriteFileToolName, p, p, true); err == nil && outcome != tools.Cancel {
		t.Fatal("custom agent received builtin file access")
	}
}

func TestExtensionActivationIsExplicitAndRespectsOverrides(t *testing.T) {
	e := extensionFixture(t)
	if e.status().ActivationGeneration != "" {
		t.Fatal("rescan automatically activated")
	}
	if err := e.activate(); err != nil {
		t.Fatal(err)
	}
	active := e.status().ActivationGeneration
	if active == "" || active != e.status().Generation {
		t.Fatal("missing activation generation")
	}
	if err := os.WriteFile(filepath.Join(e.status().Directory, "dracula", "style.css"), []byte("body{color:red}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.reload(); err != nil {
		t.Fatal(err)
	}
	if e.status().ActivationGeneration != "" || e.status().Generation == active {
		t.Fatal("rescan implicitly activated new files")
	}
	e.disabled = true
	if err := e.activate(); err == nil {
		t.Fatal("activation bypassed kill switch")
	}
	e.disabled = false
	e.flagSet = true
	e.enabledFlag = []string{}
	if err := e.activate(); err != nil {
		t.Fatal(err)
	}
	if len(e.status().Enabled) != 0 {
		t.Fatal("activation bypassed explicit empty flag")
	}
	e.enabledFlag = []string{"missing"}
	if err := e.activate(); err == nil {
		t.Fatal("activated broken extension")
	}
	if e.status().ActivationGeneration != "" {
		t.Fatal("failed activation published a request")
	}
}
