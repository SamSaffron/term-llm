package chat

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/term-llm/term-llm/internal/app/runtime/mcpstart"
	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/testutil/mcptest"
)

func TestMCPPolicyChatTransientLoadFailurePreservesSavedSelection(t *testing.T) {
	for _, failure := range []string{"connections", "policy"} {
		t.Run(failure, func(t *testing.T) {
			mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"open": {Command: "must-not-start"}})
			root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
			if failure == "connections" {
				if err := os.WriteFile(filepath.Join(root, "mcp.json"), []byte("{temporary"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("mcp: {servers: null}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			sess := &session.Session{ID: session.NewID(), Agent: "saved-agent", MCP: "open"}
			if err := store.Create(context.Background(), sess); err != nil {
				t.Fatal(err)
			}
			var warnings bytes.Buffer
			prepared, err := mcpstart.PrepareSelection("", sess.MCP, false, &warnings, nil)
			if err != nil {
				t.Fatal(err)
			}
			manager := prepared.Manager
			selection, err := prepareChatMCP(context.Background(), prepared, store, sess)
			if err != nil || selection != "" {
				t.Fatalf("transient restore=%q %v", selection, err)
			}
			saved, err := store.Get(context.Background(), sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.MCP != "open" || saved.Agent != "saved-agent" || sess.MCP != "open" {
				t.Fatalf("transient load erased durable state: %+v", saved)
			}
			if len(manager.EnabledServers()) != 0 || warnings.Len() == 0 {
				t.Fatal("failure did not stay closed and warn")
			}
		})
	}
}
