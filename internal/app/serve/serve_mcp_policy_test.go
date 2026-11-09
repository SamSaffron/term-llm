package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/mcp"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

func writeServePolicy(t *testing.T, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestMCPPolicySessionSurfaces(t *testing.T) {
	for _, identity := range []string{"migration-data", "", "other"} {
		t.Run(identity, func(t *testing.T) {
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{"restricted": {Command: os.Args[0], Env: map[string]string{runServeMCPHandlerTestServerEnv: "1"}}})
			writeServePolicy(t, "mcp: {servers: {restricted: {agents: [migration-data]}}}")
			store := newServeMCPTestStore(t)
			srv, created := newServeMCPHandlerTestServer(t, store)
			rt, err := srv.sessionMgr.GetOrCreate(context.Background(), "policy-session")
			if err != nil {
				t.Fatal(err)
			}
			rt.mcpIdentity = identity
			body := bytes.NewBufferString(`{"enabled":["restricted"]}`)
			request := httptest.NewRequest(http.MethodPatch, "/v1/sessions/policy-session/mcp", body)
			request.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.handleSessionByID(rr, request)
			if identity == "migration-data" {
				if rr.Code != http.StatusOK {
					t.Fatalf("allowed PATCH=%d: %s", rr.Code, rr.Body.String())
				}
				response := decodeServeMCPResponse(t, rr)
				if len(response.Servers) != 1 || len(response.Enabled) != 1 {
					t.Fatalf("allowed list=%+v", response)
				}
			} else {
				if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), `"permission_error"`) {
					t.Fatalf("denied PATCH=%d: %s", rr.Code, rr.Body.String())
				}
				if len((*created).mcpManager.EnabledServers()) != 0 {
					t.Fatal("denied server started")
				}
				response := rt.mcpStateLocked()
				if len(response.Servers) != 0 || len(response.Enabled) != 0 {
					t.Fatalf("denied catalogue=%+v", response)
				}
			}
		})
	}
}

func TestMCPPolicyPersistedSelectionCleanup(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{"restricted": {}, "orphan": {}})
	writeServePolicy(t, "mcp: {servers: {restricted: {agents: [migration-data]}, orphan: {agents: []}}}")
	store := newServeMCPTestStore(t)
	sess := &session.Session{ID: "persisted-policy", Agent: "migration-data", MCP: "restricted,orphan", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	srv, _ := newServeMCPHandlerTestServer(t, store)
	// An arbitrary stored agent name cannot itself confer an identity.
	state, err := srv.configuredMCPState(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Servers) != 0 || len(state.Enabled) != 0 {
		t.Fatalf("fallback leaked restricted servers: %+v", state)
	}
	saved, err := store.Get(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.MCP != "restricted,orphan" {
		t.Fatalf("read-only fallback changed selection: %q", saved.MCP)
	}
	saved.MCP = "restricted,orphan"
	if err := store.Update(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	rt := &serveRuntime{}
	if err := srv.applyPersistedMCPSelectionLocked(context.Background(), sess.ID, rt); err != nil {
		t.Fatal(err)
	}
	if rt.mcpSetting != "" {
		t.Fatalf("restore resurrected restricted selection: %q", rt.mcpSetting)
	}
	saved, _ = store.Get(context.Background(), sess.ID)
	if saved.MCP != "" {
		t.Fatal("restore did not save cleanup")
	}
}

func TestMCPPolicyRunnerOwnIdentity(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{"restricted": {Command: os.Args[0], Env: map[string]string{runServeMCPHandlerTestServerEnv: "1"}}})
	writeServePolicy(t, "mcp: {servers: {restricted: {agents: [migration-data]}}}")
	root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "agents")
	for _, name := range []string{"migration-data", "other"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		// Both definitions self-assert the allowed name; only the lookup counts.
		if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: migration-data\nmcp:\n  - name: restricted\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
	for _, platform := range []string{runpkg.PlatformConsole, runpkg.PlatformJob} {
		for _, child := range []bool{false, true} {
			t.Run(platform+"/child="+map[bool]string{true: "yes", false: "no"}[child], func(t *testing.T) {
				runner := newCmdRunner(cfg, cmdRunnerOptions{}).(*cmdRunner)
				var previous *mcp.Manager
				for _, name := range []string{"migration-data", "other", "", "migration-data"} {
					req := runpkg.Request{AgentName: name, Platform: platform, IsSubagent: child, ProviderInstance: mockprovider.NewMockProvider("mock"), Cwd: t.TempDir(), DeferSession: true, MCP: "restricted", MCPExplicit: true, Messages: []core.Message{core.UserText("hello")}}
					env, err := runner.prepare(context.Background(), req, nil)
					if name != "migration-data" {
						var denied *mcp.PermissionError
						if !errors.As(err, &denied) {
							t.Fatalf("agent=%q error=%v", name, err)
						}
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					if env.runtime.mcpIdentity != "migration-data" {
						t.Fatalf("identity=%q", env.runtime.mcpIdentity)
					}
					if previous != nil && previous == env.runtime.mcpManager {
						t.Fatal("manager shared between runs")
					}
					previous = env.runtime.mcpManager
					env.Close()
				}
			})
		}
	}
}

func TestMCPPolicyDeleteReaddKeepsPolicy(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{"restricted": {Command: "original"}})
	writeServePolicy(t, "mcp: {servers: {restricted: {agents: []}}}")
	policyPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "config.yaml")
	before, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, remove := range []bool{true, false} {
		if err := mcp.UpdateConfig(func(cfg *mcp.Config) error {
			if remove {
				delete(cfg.Servers, "restricted")
			} else {
				cfg.Servers["restricted"] = mcp.ServerConfig{Command: "replacement"}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := os.ReadFile(policyPath)
	if !bytes.Equal(before, after) {
		t.Fatal("mcp.json update modified policy")
	}
	manager := mcp.NewManager()
	if err := manager.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	var denied *mcp.PermissionError
	if err := manager.Enable(context.Background(), "restricted"); !errors.As(err, &denied) {
		t.Fatalf("re-add lifted restriction: %v", err)
	}
	// The persisted connection schema still contains only connection fields.
	path, _ := mcp.DefaultConfigPath()
	data, _ := os.ReadFile(path)
	var connections map[string]any
	if err := json.Unmarshal(data, &connections); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "agents") {
		t.Fatal("policy leaked into mcp.json")
	}
}

func TestMCPPolicyReloadUnregistersTools(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{"restricted": {Command: os.Args[0], Env: map[string]string{runServeMCPHandlerTestServerEnv: "1"}}})
	writeServePolicy(t, "{}")
	store := newServeMCPTestStore(t)
	srv, _ := newServeMCPHandlerTestServer(t, store)
	rt, err := srv.sessionMgr.GetOrCreate(context.Background(), "revoked-tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.applyMCPSelectionLocked(context.Background(), []string{"restricted"}); err != nil {
		t.Fatal(err)
	}
	if _, exists := rt.engine.Tools().Get("restricted__greet"); !exists {
		t.Fatal("missing initial wrapper")
	}
	writeServePolicy(t, "mcp: {servers: {restricted: {agents: []}}}")
	if err := rt.refreshMCPManagerLocked(); err != nil {
		t.Fatal(err)
	}
	if _, exists := rt.engine.Tools().Get("restricted__greet"); exists {
		t.Fatal("revocation left executable wrapper registered")
	}
	state := rt.mcpStateLocked()
	if len(state.Servers) != 0 || len(state.Enabled) != 0 {
		t.Fatalf("revoked server remains visible: %+v", state)
	}
}

func TestMCPPolicyRuntimeAdmissionReturns403(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, func(context.Context) (*serveRuntime, error) {
		return nil, fmt.Errorf("agent default MCP selection: %w", &mcp.PermissionError{Server: "restricted"})
	})
	defer manager.Close()
	srv := &serveServer{sessionMgr: manager}
	request := httptest.NewRequest(http.MethodPatch, "/v1/sessions/denied-admission/mcp", strings.NewReader(`{"enabled":["restricted"]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.handleSessionByID(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"permission_error"`) {
		t.Fatalf("admission response=%d: %s", response.Code, response.Body.String())
	}
}

func TestMCPPolicyCleanPersistedSelectionNormalizesAndDropsUnknown(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{"a": {}, "z": {}, "denied": {}})
	writeServePolicy(t, "mcp: {servers: {denied: {agents: []}}}")
	store := newServeMCPTestStore(t)
	sess := &session.Session{ID: "normalize-policy", MCP: " z, a,z,removed,denied,a ", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	srv, _ := newServeMCPHandlerTestServer(t, store)
	rt := &serveRuntime{}
	for i := 0; i < 2; i++ {
		names, err := srv.cleanPersistedMCPSelection(context.Background(), sess, rt)
		if err != nil {
			t.Fatal(err)
		}
		if len(names) != 2 || names[0] != "a" || names[1] != "z" {
			t.Fatalf("non-normalized return: %v", names)
		}
		saved, err := store.Get(context.Background(), sess.ID)
		if err != nil || saved.MCP != "a,z" {
			t.Fatalf("persisted cleanup=%+v %v", saved, err)
		}
	}
	if len(rt.mcpManager.EnabledServers()) != 0 {
		t.Fatal("cleanup started servers")
	}
}
