package serve

import (
	"bytes"
	"context"
	"log"
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

func TestMCPPolicyRequestValueNeedsExplicitMarker(t *testing.T) {
	for _, platform := range []string{runpkg.PlatformTelegram, runpkg.PlatformJob} {
		for _, explicit := range []bool{false, true} {
			t.Run(platform+map[bool]string{false: "/derived", true: "/explicit"}[explicit], func(t *testing.T) {
				writeServeMCPConfig(t, map[string]mcp.ServerConfig{"denied": {Command: "must-not-start"}})
				writeServePolicy(t, "mcp: {servers: {denied: {agents: []}}}")
				cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
				var warnings bytes.Buffer
				runner := newCmdRunner(cfg, cmdRunnerOptions{ErrWriter: &warnings}).(*cmdRunner)
				env, err := runner.prepare(context.Background(), runpkg.Request{Platform: platform, MCP: "denied,removed", MCPExplicit: explicit, ProviderInstance: mockprovider.NewMockProvider("mock"), Cwd: t.TempDir(), DeferSession: true}, nil)
				if explicit {
					if err == nil {
						env.Close()
						t.Fatal("explicit selection not rejected")
					}
					return
				}
				if err != nil {
					t.Fatalf("derived selection was fatal: %v", err)
				}
				defer env.Close()
				if env.runtime.mcpManager != nil || env.runtime.mcpSetting != "" || !strings.Contains(warnings.String(), "dropping MCP server") {
					t.Fatalf("derived selection was not filtered: %q %s", env.runtime.mcpSetting, warnings.String())
				}
			})
		}
	}
}

func TestMCPPolicyRunnerLoadFailurePreservesEveryPersistencePath(t *testing.T) {
	for _, failure := range []string{"connections", "policy"} {
		for _, persist := range []string{"runner", "insert", "hydrate"} {
			t.Run(failure+"/"+persist, func(t *testing.T) {
				writeServeMCPConfig(t, map[string]mcp.ServerConfig{"saved": {Command: "must-not-start"}})
				root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
				path, body := filepath.Join(root, "mcp.json"), "{temporary"
				if failure == "policy" {
					path, body = filepath.Join(root, "config.yaml"), "mcp: {servers: null}"
				}
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				store := newServeMCPTestStore(t)
				cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
				var warnings bytes.Buffer
				runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store, ErrWriter: &warnings}).(*cmdRunner)
				req := runpkg.Request{SessionID: "preserved-selection", Platform: runpkg.PlatformConsole, MCP: "saved", ProviderInstance: mockprovider.NewMockProvider("mock"), Cwd: t.TempDir(), DeferSession: persist != "runner"}
				env, err := runner.prepare(context.Background(), req, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer env.Close()
				if env.runtime.mcpManager != nil || env.runtime.mcpSetting != "saved" || env.settings.MCP != "saved" {
					t.Fatalf("failed load erased runtime selection: %q %+v", env.runtime.mcpSetting, env.settings)
				}
				if persist == "insert" {
					if _, inserted := env.runtime.ensureSessionInStore(context.Background(), req.SessionID, nil); !inserted {
						t.Fatal("insert did not persist")
					}
				}
				if persist == "hydrate" {
					if !env.runtime.ensurePersistedSession(context.Background(), req.SessionID, nil) {
						t.Fatal("hydrate did not persist")
					}
				}
				saved, err := store.Get(context.Background(), req.SessionID)
				if err != nil || saved.MCP != "saved" {
					t.Fatalf("persisted selection lost: %+v %v", saved, err)
				}
			})
		}
	}
}

func TestMCPPolicyReopenedServeUsesAgentModelNotImplicitResumeModel(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{})
	root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "agents", "foo")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agent.yaml"), []byte("name: foo\nprovider: debug\nmodel: agent-default\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "configured-default"}}}
	store := newServeMCPTestStore(t)
	sess := &session.Session{ID: "reopened-model", Agent: "foo", Provider: "debug", ProviderKey: "debug", Model: "stored-model"}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	rt, err := newServeAgentRuntime(context.Background(), serveRuntimeRequest{SessionID: sess.ID, Agent: sess.Agent, RuntimeDir: t.TempDir()}, serveAgentRuntimeOptions{flags: serveRuntimeFixtureOptions(), cfg: cfg, store: store})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if rt.defaultModel != "agent-default" || rt.mcpIdentity != "foo" {
		t.Fatalf("model/provenance coupled: model=%q identity=%q", rt.defaultModel, rt.mcpIdentity)
	}
}

func TestMCPPolicyEmptyServeTurnDoesNotLoadMalformedConnections(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{})
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "mcp.json")
	if err := os.WriteFile(path, []byte("{temporary"), 0600); err != nil {
		t.Fatal(err)
	}
	srv, _ := newServeMCPHandlerTestServer(t, newServeMCPTestStore(t))
	rt, err := srv.sessionMgr.GetOrCreate(context.Background(), "plain-turn")
	if err != nil {
		t.Fatal(err)
	}
	provider := rt.provider.(*mockprovider.MockProvider)
	provider.AddTextResponse("done")
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	for i := 0; i < 2; i++ {
		if err := srv.ensureRuntimeMCPForSession(context.Background(), "plain-turn", rt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rt.Run(context.Background(), false, false, []core.Message{core.UserText("hello")}, core.Request{}); err != nil {
		t.Fatal(err)
	}
	if rt.mcpManager != nil || logs.Len() != 0 || len(provider.RecordedRequests()) != 1 {
		t.Fatalf("empty turn loaded MCP or logged: manager=%v logs=%s", rt.mcpManager, logs.String())
	}
}

func TestMCPPolicyRevocationCleansStoppedServerGrantsAndStrayTools(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{"greeter": {Command: os.Args[0], Env: map[string]string{runServeMCPHandlerTestServerEnv: "1"}}})
	writeServePolicy(t, "{}")
	srv, _ := newServeMCPHandlerTestServer(t, newServeMCPTestStore(t))
	rt, err := srv.sessionMgr.GetOrCreate(context.Background(), "revoked-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.applyMCPSelectionLocked(context.Background(), []string{"greeter"}); err != nil {
		t.Fatal(err)
	}
	rt.mcpAuthRequiredGrants = map[string]string{"greeter": "observed-grant"}
	rt.engine.Tools().Register(serveMCPFakeTool{name: "greeter__stray"})
	writeServePolicy(t, "mcp: {servers: {greeter: {agents: []}}}")
	if err := rt.mcpManager.RefreshPolicy(); err != nil {
		t.Fatal(err)
	}
	if err := rt.applyMCPSelectionLocked(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.mcpAuthRequiredGrants["greeter"]; ok {
		t.Fatal("revoked grant retained")
	}
	if _, ok := rt.engine.Tools().Get("greeter__stray"); ok {
		t.Fatal("revoked stray tool retained")
	}
	if rt.mcpSetting != "" {
		t.Fatal("revoked selection retained")
	}
}
