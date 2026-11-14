package ask

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/testutil/mcptest"
)

func TestMCPPolicyAskResumeUsesSameAgentAndCleansSelection(t *testing.T) {
	for _, selected := range []string{"", "original", "other"} {
		t.Run("explicit="+selected, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Chdir(t.TempDir())
			mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"denied": {Command: "must-not-start"}})
			db := filepath.Join(t.TempDir(), "sessions.db")
			root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
			body := fmt.Sprintf("default_provider: mock\nproviders: {mock: {model: mock-model}}\nsessions: {enabled: true, path: %q}\nskills: {enabled: false}\nmcp: {servers: {denied: {agents: []}}}\n", db)
			if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"original", "other"} {
				dir := filepath.Join(root, "agents", name)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: self-asserted\nmcp: [{name: denied}]\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "system.md"), []byte("Prompt for "+name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: db})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			sess := &session.Session{ID: session.NewID(), Agent: "original", MCP: "denied,removed", Provider: "mock", ProviderKey: "mock", Model: "mock-model", Mode: session.ModeAsk}
			if err := store.Create(context.Background(), sess); err != nil {
				t.Fatal(err)
			}
			provider := mockprovider.NewMockProvider("mock").AddTextResponse("answer")
			var output bytes.Buffer
			deps := testDependencies(&output, &output)
			deps.SessionStore.DBPath = db
			deps.NewProvider = func(*config.Config, bool) (core.Provider, error) { return provider, nil }
			opts := Options{Agent: selected, Resume: sess.ID, MCP: "denied,removed", Changed: FlagState{Resume: true}, Text: true, Porcelain: true, MaxTurns: 5}
			err = Run(context.Background(), []string{"question"}, opts, deps)
			if selected == "other" {
				if err == nil || !strings.Contains(err.Error(), "cannot resume session for agent") {
					t.Fatalf("agent mismatch error=%v", err)
				}
				if len(provider.RecordedRequests()) != 0 {
					t.Fatal("mismatch sent request")
				}
				saved, _ := store.Get(context.Background(), sess.ID)
				if saved.Agent != "original" || saved.MCP != "denied,removed" {
					t.Fatalf("mismatch mutated session: %+v", saved)
				}
				return
			}
			if err != nil {
				t.Fatalf("resume failed: %v\n%s", err, output.String())
			}
			requests := provider.RecordedRequests()
			if len(requests) != 1 {
				t.Fatalf("requests=%d", len(requests))
			}
			var prompt strings.Builder
			for _, message := range requests[0].Messages {
				for _, part := range message.Parts {
					prompt.WriteString(part.Text)
				}
			}
			if !strings.Contains(prompt.String(), "Prompt for original") || strings.Contains(prompt.String(), "Prompt for other") {
				t.Fatalf("wrong run agent: %s", prompt.String())
			}
			saved, err := store.Get(context.Background(), sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Agent != "original" || saved.MCP != "" {
				t.Fatalf("resume metadata=%+v", saved)
			}
			if !strings.Contains(output.String(), "dropping MCP server") {
				t.Fatalf("missing cleanup warning: %s", output.String())
			}
		})
	}
}
