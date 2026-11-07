package serve

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/testutil/mcptest"
)

func TestSessionMCPAgentScope(t *testing.T) {
	for _, agent := range []string{"", "developer", "migration-data"} {
		t.Run("agent="+agent, func(t *testing.T) {
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{"db": {Command: os.Args[0], Env: map[string]string{mcptest.ChildEnv: "1"}, Agents: []string{"migration-data"}}})
			srv, _ := newServeMCPHandlerTestServer(t, nil)
			rt, err := srv.sessionMgr.GetOrCreate(t.Context(), "scope")
			if err != nil {
				t.Fatal(err)
			}
			rt.agentName = agent
			rr := httptest.NewRecorder()
			srv.handleSessionMCP(rr, httptest.NewRequest(http.MethodPatch, "/v1/sessions/scope/mcp", bytes.NewBufferString(`{"enabled":["db"]}`)), "scope")
			if agent == "migration-data" {
				if rr.Code != http.StatusOK {
					t.Fatalf("PATCH = %d %s", rr.Code, rr.Body.String())
				}
				if len(rt.engine.Tools().AllSpecsIncludingDeferred()) == 0 {
					t.Fatal("allowed tools not registered")
				}
			} else {
				if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "permission_error") || !strings.Contains(rr.Body.String(), "restricted to agents: migration-data") {
					t.Fatalf("PATCH = %d %s", rr.Code, rr.Body.String())
				}
				if rt.mcpSetting != "" || len(rt.engine.Tools().AllSpecsIncludingDeferred()) != 0 {
					t.Fatal("denied request changed runtime")
				}
				rr = httptest.NewRecorder()
				srv.handleSessionMCP(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions/scope/mcp", nil), "scope")
				if resp := decodeServeMCPResponse(t, rr); len(resp.Servers) != 0 {
					t.Fatalf("restricted server offered: %+v", resp)
				}
			}
		})
	}
}

func TestPersistedMCPAgentScope(t *testing.T) {
	for _, agent := range []string{"", "developer"} {
		t.Run("agent="+agent, func(t *testing.T) {
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{"db": {Command: "must-not-start", Agents: []string{"migration-data"}}})
			store := newServeMCPTestStore(t)
			now := time.Now()
			if err := store.Create(t.Context(), &session.Session{ID: "restore", Provider: "mock", Model: "mock", Agent: agent, MCP: "db", Mode: session.ModeChat, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			srv, _ := newServeMCPHandlerTestServer(t, store)
			rt, err := srv.sessionMgr.GetOrCreate(t.Context(), "restore")
			if err != nil {
				t.Fatal(err)
			}
			rt.agentName = agent
			if err := srv.applyPersistedMCPSelectionLocked(t.Context(), "restore", rt); err != nil {
				t.Fatal(err)
			}
			if rt.mcpSetting != "" || len(rt.mcpManager.EnabledServers()) != 0 {
				t.Fatal("restricted selection restored")
			}
			saved, err := store.Get(t.Context(), "restore")
			if err != nil || saved.MCP != "" {
				t.Fatalf("persisted selection = %+v, %v", saved, err)
			}
			view, err := srv.configuredMCPState(t.Context(), "restore")
			if err != nil || len(view.Servers) != 0 {
				t.Fatalf("config view = %+v, %v", view, err)
			}
		})
	}
}

func TestMCPConfigWritersPreserveAgentScope(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{"db": {Command: "db", Agents: []string{"migration-data"}}})
	path, err := mcp.DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	// Exercise unknown fields too: future config must survive the UI writers.
	if err := os.WriteFile(path, []byte(`{"future":true,"servers":{"db":{"command":"db","agents":["migration-data"],"extension":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{}
	for _, action := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/v1/mcp/servers", `{"kind":"config","name":"other","config":{"command":"other"}}`, 201},
		{"DELETE", "/v1/mcp/servers/other", "", 200},
	} {
		rr := mcpConfigRequest(t, srv, action.method, action.path, action.body)
		if rr.Code != action.status {
			t.Fatalf("%s = %d %s", action.method, rr.Code, rr.Body.String())
		}
		cfg, err := mcp.LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Servers["db"].Agents; len(got) != 1 || got[0] != "migration-data" {
			t.Fatalf("agents lost: %v", got)
		}
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), `"future"`) || !strings.Contains(string(data), `"extension"`) {
			t.Fatalf("extensions lost: %s, %v", data, err)
		}
	}
}
