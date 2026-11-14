package chat

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/app/runtime/approval"
	"github.com/term-llm/term-llm/internal/app/runtime/bootstrap"
	"github.com/term-llm/term-llm/internal/app/runtime/mcpstart"
	appserve "github.com/term-llm/term-llm/internal/app/serve"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/testutil/mcptest"
	"github.com/term-llm/term-llm/internal/tools"
)

func TestMCPPolicyChatResumeRepairsStaleSelection(t *testing.T) {
	for _, selected := range []string{"denied", "removed", "missing-agent"} {
		t.Run(selected, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Chdir(t.TempDir())
			mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"denied": {Command: "must-not-start"}})
			root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
			if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("default_provider: debug\nproviders: {debug: {model: fast}}\nsessions: {enabled: true}\nskills: {enabled: false}\nchat: {terminal_title: off}\nmcp: {servers: {denied: {agents: []}}}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(t.TempDir(), "sessions.db")
			store, err := session.NewStore(session.Config{Enabled: true, Path: db})
			if err != nil {
				t.Fatal(err)
			}
			sess := &session.Session{ID: session.NewID(), MCP: selected, CWD: t.TempDir(), Provider: "debug", ProviderKey: "debug", Model: "fast", Mode: session.ModeChat}
			if selected == "missing-agent" {
				sess.Agent = "missing-definition"
			}
			if err := store.Create(context.Background(), sess); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(appserve.SwapProcessSessionInputs(appserve.NewSessionInputCoordinator()))
			r := testInvocation()
			var warnings bytes.Buffer
			r.deps.Stderr = &warnings
			r.deps.SessionStore.DBPath = db
			r.deps.MemoryPaths.DBPath = filepath.Join(t.TempDir(), "memory.db")
			r.deps.LoadConfig = bootstrap.LoadConfigWithSetup
			r.deps.CreateDebugLogger = bootstrap.CreateDebugLogger
			r.deps.StdoutIsTerminal = func() bool { return false }
			r.deps.ResolveApproval = func(cfg *config.Config, persisted *session.SessionApprovalMode, carried *tools.ApprovalMode, _ string) (approval.ResolvedMode, error) {
				return approval.Resolve(approval.ResolutionInput{Surface: approval.SurfaceChat, Config: cfg, Session: persisted, CLI: carried})
			}
			rt, err := r.buildChatSessionRuntime(context.Background(), chatSessionLaunch{resumeRequested: true, resumeID: sess.ID}, nil)
			if selected == "missing-agent" {
				if err == nil || !strings.Contains(err.Error(), "agent not found") {
					t.Fatalf("missing agent error=%v", err)
				}
				check, openErr := session.NewStore(session.Config{Enabled: true, Path: db})
				if openErr != nil {
					t.Fatal(openErr)
				}
				defer check.Close()
				saved, getErr := check.Get(context.Background(), sess.ID)
				if getErr != nil || saved.Agent != "missing-definition" || saved.MCP != selected {
					t.Fatalf("failed resume mutated state: %+v %v", saved, getErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resume hard-failed: %v\n%s", err, warnings.String())
			}
			defer rt.cleanupResources()
			saved, err := rt.store.Get(context.Background(), sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.MCP != "" || rt.sess.MCP != "" {
				t.Fatalf("stale selection retained: %+v", saved)
			}
			if !strings.Contains(warnings.String(), "dropping MCP server") {
				t.Fatalf("missing warning: %s", warnings.String())
			}
		})
	}
}

func TestMCPPolicyChatLoadFailureIsNonfatalAndClosed(t *testing.T) {
	mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"db": {Command: "must-not-start"}})
	root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("mcp: [invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	prepared, err := mcpstart.PrepareSelection("", "db", false, &warnings, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := prepared.Manager
	selection, err := prepareChatMCP(context.Background(), prepared, nil, nil)
	if err != nil || selection != "" {
		t.Fatalf("implicit selection=%q error=%v", selection, err)
	}
	if manager.CheckAccess("db") == nil || len(manager.AvailableServers()) != 0 {
		t.Fatal("failed policy became unrestricted")
	}
	if !strings.Contains(warnings.String(), "failed to load MCP config") {
		t.Fatalf("no load warning: %s", warnings.String())
	}
	if _, err := mcpstart.PrepareSelection("", "db", true, &warnings, nil); err == nil {
		t.Fatal("explicit selection ignored load failure")
	}
}

func TestMCPPolicyChatDefaultsAndExplicitSelections(t *testing.T) {
	mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"denied": {}, "open": {}})
	root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("mcp: {servers: {denied: {agents: []}}}"), 0600); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	prepared, err := mcpstart.PrepareSelection("", "denied,removed,open,open", false, &warnings, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := prepared.Manager
	got, err := prepareChatMCP(context.Background(), prepared, nil, nil)
	if err != nil || got != "open" {
		t.Fatalf("defaults=%q error=%v", got, err)
	}
	if _, err := mcpstart.PrepareSelection("", "open,denied", true, &warnings, nil); err == nil {
		t.Fatal("explicit selection not rejected")
	}
	if len(manager.EnabledServers()) != 0 {
		t.Fatal("preflight started a server")
	}
}
