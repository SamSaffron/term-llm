package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/term-llm/term-llm/internal/agents"
	"github.com/term-llm/term-llm/internal/app/serve/agentcatalog"
	"github.com/term-llm/term-llm/internal/config"
)

func writeAgentCatalogTestAgent(t *testing.T, dir, name, yaml string) {
	t.Helper()
	agentDir := filepath.Join(dir, name)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func agentCatalogRequest(t *testing.T, srv *serveServer, method string) (*httptest.ResponseRecorder, agentcatalog.Response) {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handleAgentCatalog(rr, httptest.NewRequest(method, "/v1/agents", nil))
	var payload agentcatalog.Response
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v body=%s", err, rr.Body.String())
		}
	}
	return rr, payload
}

func TestAgentCatalogListsUserAndBuiltinAgents(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", configHome)
	userDir := filepath.Join(configHome, "term-llm", "agents")
	writeAgentCatalogTestAgent(t, userDir, "mine", "name: mine\ndescription: My agent\nprovider: anthropic\nmodel: claude-x\n")

	builtins := agents.GetBuiltinAgentNames()
	if len(builtins) == 0 {
		t.Fatal("expected built-in agents")
	}
	// A user agent with a built-in's name shadows it, as in `term-llm agents`.
	shadowed := builtins[0]
	writeAgentCatalogTestAgent(t, userDir, shadowed, "name: "+shadowed+"\ndescription: Override\n")

	srv := &serveServer{cfgRef: &config.Config{Agents: config.AgentsConfig{UseBuiltin: true}}}
	rr, payload := agentCatalogRequest(t, srv, http.MethodGet)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	byName := map[string]agentcatalog.Entry{}
	for _, entry := range payload.Agents {
		if _, dup := byName[entry.Name]; dup {
			t.Fatalf("agent %q listed twice", entry.Name)
		}
		byName[entry.Name] = entry
	}
	mine := byName["mine"]
	if mine.Source != "user" || mine.Description != "My agent" || mine.Provider != "anthropic" || mine.Model != "claude-x" {
		t.Fatalf("mine = %+v", mine)
	}
	if got := byName[shadowed]; got.Source != "user" || got.Description != "Override" {
		t.Fatalf("shadowed builtin %q = %+v", shadowed, got)
	}
	if len(builtins) > 1 {
		if got := byName[builtins[1]]; got.Source != "builtin" {
			t.Fatalf("builtin %q = %+v", builtins[1], got)
		}
	}
	if len(payload.Agents) != len(builtins)+1 {
		t.Fatalf("got %d agents, want %d", len(payload.Agents), len(builtins)+1)
	}
}

func TestAgentCatalogHonorsDisabledBuiltins(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))

	srv := &serveServer{cfgRef: &config.Config{Agents: config.AgentsConfig{UseBuiltin: false}}}
	rr, payload := agentCatalogRequest(t, srv, http.MethodGet)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	// An empty list, not null, so the UI can render "No agents" without guards.
	if payload.Agents == nil || len(payload.Agents) != 0 {
		t.Fatalf("agents = %#v body=%s, want empty list", payload.Agents, rr.Body.String())
	}
}

func TestAgentCatalogRejectsNonGet(t *testing.T) {
	rr, _ := agentCatalogRequest(t, &serveServer{}, http.MethodPost)
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET" {
		t.Fatalf("status = %d allow=%q", rr.Code, rr.Header().Get("Allow"))
	}
}
