package serve

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/mcp"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestMCPPolicyRunnerFiltersImplicitSelections(t *testing.T) {
	for _, source := range []string{"agent", "runner", "restored", "serve"} {
		t.Run(source, func(t *testing.T) {
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{"denied": {Command: "must-not-start"}})
			writeServePolicy(t, "mcp: {servers: {denied: {agents: []}}}")
			dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "agents", "foo")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			body := "name: foo\n"
			if source == "agent" {
				body += "mcp: [{name: denied}, {name: removed}]\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
			var warnings bytes.Buffer
			opts := cmdRunnerOptions{ErrWriter: &warnings}
			if source == "runner" {
				opts.MCP = "denied,removed"
			}
			if source == "restored" {
				opts.RestoreSettings = &SessionSettings{MCP: "denied,removed"}
			}
			if source == "serve" {
				opts = serveRuntimeRunnerDefaults(serveAgentRuntimeOptions{flags: Options{MCP: "denied,removed"}}, serveRuntimeRequest{}, 0)
				opts.ErrWriter = &warnings
			}
			runner := newCmdRunner(cfg, opts).(*cmdRunner)
			env, err := runner.prepare(context.Background(), runpkg.Request{AgentName: "foo", Platform: runpkg.PlatformConsole, ProviderInstance: mockprovider.NewMockProvider("mock"), Cwd: t.TempDir(), DeferSession: true, Messages: []core.Message{core.UserText("hello")}}, nil)
			if err != nil {
				t.Fatalf("implicit %s selection prevented run: %v", source, err)
			}
			defer env.Close()
			if env.runtime.mcpSetting != "" || (env.runtime.mcpManager != nil && len(env.runtime.mcpManager.EnabledServers()) != 0) {
				t.Fatal("denied implicit server started")
			}
			if !strings.Contains(warnings.String(), "dropping MCP server") {
				t.Fatalf("no repair warning: %s", warnings.String())
			}
		})
	}
}

func TestMCPPolicyServeNextTurnRefreshesAndCleansSelection(t *testing.T) {
	for _, state := range []string{"persisted", "no-store", "no-row"} {
		t.Run(state, func(t *testing.T) {
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{"greeter": {Command: os.Args[0], Env: map[string]string{runServeMCPHandlerTestServerEnv: "1"}}})
			writeServePolicy(t, "{}")
			store := newServeMCPTestStore(t)
			const id = "next-turn-policy"
			if state == "persisted" {
				if err := store.Create(context.Background(), &session.Session{ID: id, MCP: "greeter"}); err != nil {
					t.Fatal(err)
				}
			}
			srv, _ := newServeMCPHandlerTestServer(t, store)
			rt, err := srv.sessionMgr.GetOrCreate(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if err := rt.applyMCPSelectionLocked(context.Background(), []string{"greeter"}); err != nil {
				t.Fatal(err)
			}
			if _, ok := rt.engine.Tools().Get("greeter__greet"); !ok {
				t.Fatal("fixture did not register tool")
			}
			if state == "no-store" {
				srv.store = nil
			}
			writeServePolicy(t, "mcp: {servers: {greeter: {agents: []}}}")
			if err := srv.ensureRuntimeMCPForSession(context.Background(), id, rt); err != nil {
				t.Fatal(err)
			}
			if status, _ := rt.mcpManager.ServerStatus("greeter"); status != mcp.StatusStopped {
				t.Fatalf("next turn did not revoke: %s", status)
			}
			if len(rt.mcpManager.CatalogueSnapshot().Tools) != 0 || rt.mcpSetting != "" {
				t.Fatalf("next turn retained state: %q", rt.mcpSetting)
			}
			if _, ok := rt.engine.Tools().Get("greeter__greet"); ok {
				t.Fatal("next turn retained revoked execution wrapper")
			}
			if state == "persisted" {
				saved, err := store.Get(context.Background(), id)
				if err != nil || saved.MCP != "" {
					t.Fatalf("durable selection=%+v error=%v", saved, err)
				}
			}
		})
	}
}

func TestMCPPolicyServeAccessErrorsKeepTheirType(t *testing.T) {
	for _, state := range []string{"denied", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{"db": {Command: "must-not-start"}})
			body := "mcp: {servers: {db: {agents: []}}}"
			if state == "unavailable" {
				body = "mcp: {servers: null}"
			}
			writeServePolicy(t, body)
			rt := &serveRuntime{mcpManager: mcp.NewManager()}
			err := rt.validateMCPAccess([]string{"db"})
			if err == nil {
				t.Fatal("access unexpectedly granted")
			}
			var denied *mcp.PermissionError
			if errors.As(err, &denied) != (state == "denied") {
				t.Fatalf("permission error flattened or fabricated: %v", err)
			}
			rr := httptest.NewRecorder()
			writeServeMCPFailure(rr, err)
			code, kind := http.StatusForbidden, "permission_error"
			if state == "unavailable" {
				code, kind = http.StatusInternalServerError, "server_error"
			}
			if rr.Code != code || !strings.Contains(rr.Body.String(), kind) {
				t.Fatalf("mapped response=%d %s", rr.Code, rr.Body.String())
			}
			srv, _ := newServeMCPHandlerTestServer(t, newServeMCPTestStore(t))
			rr = httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/access-policy/mcp", bytes.NewBufferString(`{"enabled":["db"]}`))
			req.Header.Set("Content-Type", "application/json")
			srv.handleSessionByID(rr, req)
			if rr.Code != code || !strings.Contains(rr.Body.String(), kind) {
				t.Fatalf("HTTP response=%d %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestMCPPolicyServeFactoryReresolvesLegacySessionProvenance(t *testing.T) {
	for _, kind := range []string{"search", "user-and-search", "user", "missing"} {
		t.Run(kind, func(t *testing.T) {
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{})
			writeServePolicy(t, "{}")
			search := t.TempDir()
			root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "agents")
			for _, source := range []string{"search", "user"} {
				if (source == "search" && (kind == "search" || kind == "user-and-search")) || (source == "user" && (kind == "user" || kind == "user-and-search")) {
					base := root
					if source == "search" {
						base = search
					}
					dir := filepath.Join(base, "foo")
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: foo\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "fast"}}, Agents: config.AgentsConfig{SearchPaths: []string{search}}}
			store := newServeMCPTestStore(t)
			sess := &session.Session{ID: "legacy-factory", Agent: "foo", Provider: "debug", ProviderKey: "debug", Model: "fast"}
			if err := store.Create(context.Background(), sess); err != nil {
				t.Fatal(err)
			}
			rt, err := newServeAgentRuntime(context.Background(), serveRuntimeRequest{SessionID: sess.ID, Agent: sess.Agent, RuntimeDir: t.TempDir()}, serveAgentRuntimeOptions{flags: serveRuntimeFixtureOptions(), cfg: cfg, store: store})
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			want := ""
			if kind == "user" {
				want = "foo"
			}
			if rt.mcpIdentity != want {
				t.Fatalf("legacy factory identity=%q want=%q", rt.mcpIdentity, want)
			}
			saved, err := store.Get(context.Background(), sess.ID)
			if err != nil || saved.Agent != "foo" {
				t.Fatalf("factory erased reference: %+v %v", saved, err)
			}
		})
	}
}
