package mcpstart

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/testutil/mcptest"
)

func TestMCPPolicySelectionPreflight(t *testing.T) {
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer endpoint.Close()
	mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"open": {URL: endpoint.URL}, "restricted": {URL: endpoint.URL}})
	home := os.Getenv("XDG_CONFIG_HOME")
	if home == "" {
		t.Fatal("test requires isolated config home")
	}
	cfgdir := filepath.Join(home, "term-llm")
	if err := os.MkdirAll(cfgdir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgdir, "config.yaml"), []byte("mcp: {servers: {restricted: {agents: [migration-data]}}}"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := EnableMCPServersWithFeedback(context.Background(), "open,restricted", nil, io.Discard, nil)
	var denied *mcp.PermissionError
	if !errors.As(err, &denied) {
		t.Fatalf("selection error=%v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("server started before whole selection validation")
	}
}

func TestMCPPolicyResumedSessionReresolvesAgent(t *testing.T) {
	for _, source := range []string{"user", "search-path", "path", "removed"} {
		t.Run(source, func(t *testing.T) {
			mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"restricted": {Command: "must-not-start"}})
			home := os.Getenv("XDG_CONFIG_HOME")
			policyDir := filepath.Join(home, "term-llm")
			if err := os.WriteFile(filepath.Join(policyDir, "config.yaml"), []byte("mcp: {servers: {restricted: {agents: [migration-data]}}}"), 0600); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(policyDir, "agents")
			cfg := &config.Config{}
			reference := "migration-data"
			if source == "search-path" || source == "path" {
				root = t.TempDir()
				cfg.Agents.SearchPaths = []string{root}
			}
			dir := filepath.Join(root, "migration-data")
			if source != "removed" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				// Trust is the lookup key, never this YAML display name.
				if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: different-display-name\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if source == "path" || source == "search-path" {
				reference = dir
			}
			store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			sess := &session.Session{ID: "resume-policy", Agent: reference, MCP: "restricted"}
			if err := store.Create(context.Background(), sess); err != nil {
				t.Fatal(err)
			}
			manager, selection, err := LoadSessionManager(context.Background(), cfg, store, sess.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if source == "user" {
				if selection != "restricted" || manager.CheckAccess("restricted") != nil {
					t.Fatal("trusted lookup lost on resume")
				}
			} else {
				if selection != "" || manager.CheckAccess("restricted") == nil {
					t.Fatal("stored name granted identity without trusted resolution")
				}
				saved, err := store.Get(context.Background(), sess.ID)
				if err != nil {
					t.Fatal(err)
				}
				if saved.MCP != "restricted" {
					t.Fatal("read-only listing modified persisted selection")
				}
			}
			if len(manager.EnabledServers()) != 0 {
				t.Fatal("listing started server")
			}
		})
	}
}
