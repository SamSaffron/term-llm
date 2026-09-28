package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/mcp"
)

type fakeMCPRegistry struct {
	result *mcp.SearchResult
	err    error
	opts   []mcp.SearchOptions
}

func (f *fakeMCPRegistry) Search(_ context.Context, opts mcp.SearchOptions) (*mcp.SearchResult, error) {
	f.opts = append(f.opts, opts)
	return f.result, f.err
}

func mcpConfigRequest(t *testing.T, s *serveServer, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	switch {
	case strings.HasPrefix(target, "/v1/mcp/catalogue"):
		s.handleMCPCatalogue(rr, req)
	case target == "/v1/mcp/servers":
		s.handleMCPServers(rr, req)
	default:
		s.handleMCPServerByName(rr, req)
	}
	return rr
}

func TestMCPConfigMutations(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{})
	srv := &serveServer{}
	for _, tc := range []struct{ body, name, transport string }{
		{`{"kind":"url","url":"https://api.example.com/mcp","headers":{"Authorization":"secret"}}`, "example", "http"},
		{`{"kind":"command","command":"npx -y '@sentry/mcp-server'","env":{"TOKEN":"secret"}}`, "sentry", "stdio"},
		{`{"kind":"catalogue","catalogue_id":"bundled:exa"}`, "exa", "http"},
		{`{"kind":"config","name":"restored","config":{"command":"my-bin","args":["--some-option"]}}`, "restored", "stdio"},
	} {
		rr := mcpConfigRequest(t, srv, "POST", "/v1/mcp/servers", tc.body)
		if rr.Code != 201 {
			t.Fatalf("POST %s: %d %s", tc.name, rr.Code, rr.Body.String())
		}
		var resp mcpAddResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Name != tc.name || resp.Transport != tc.transport || resp.ConfigPath == "" {
			t.Fatalf("response: %+v", resp)
		}
	}
	cfg, err := mcp.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 4 || cfg.Servers["example"].Headers["Authorization"] != "secret" || cfg.Servers["sentry"].Env["TOKEN"] != "secret" {
		t.Fatalf("config: %+v", cfg.Servers)
	}
	dry := mcpConfigRequest(t, srv, "POST", "/v1/mcp/servers", `{"kind":"command","name":"sentry","command":"echo hi","dry_run":true}`)
	if dry.Code != 200 || !strings.Contains(dry.Body.String(), `"exists":true`) {
		t.Fatalf("dry: %d %s", dry.Code, dry.Body.String())
	}
	dup := mcpConfigRequest(t, srv, "POST", "/v1/mcp/servers", `{"kind":"command","name":"sentry","command":"echo hi"}`)
	if dup.Code != 409 || !strings.Contains(dup.Body.String(), "conflict_error") {
		t.Fatalf("duplicate: %d %s", dup.Code, dup.Body.String())
	}
	dryNew := mcpConfigRequest(t, srv, "POST", "/v1/mcp/servers", `{"kind":"command","name":"not-saved","command":"echo hi","dry_run":true}`)
	if dryNew.Code != 200 {
		t.Fatalf("dry new: %d %s", dryNew.Code, dryNew.Body.String())
	}
	cfg, err = mcp.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 4 {
		t.Fatalf("dry run wrote config: %+v", cfg.Servers)
	}
	del := mcpConfigRequest(t, srv, "DELETE", "/v1/mcp/servers/restored", "")
	if del.Code != 200 || !strings.Contains(del.Body.String(), `"command":"my-bin"`) {
		t.Fatalf("delete: %d %s", del.Code, del.Body.String())
	}
	del = mcpConfigRequest(t, srv, "DELETE", "/v1/mcp/servers/restored", "")
	if del.Code != 404 {
		t.Fatalf("second delete: %d %s", del.Code, del.Body.String())
	}
}

func TestMCPConfigErrorsAndPermissions(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{})
	srv := &serveServer{}
	for _, body := range []string{`{"kind":"url","url":"file:///tmp/mcp"}`, `{"kind":"command","command":"  "}`, `{"kind":"command","name":"bad__name","command":"echo"}`, `{"kind":"config","name":"empty"}`, `{"kind":"catalogue","catalogue_id":"bundled:missing"}`, `{"kind":"nonsense"}`} {
		rr := mcpConfigRequest(t, srv, "POST", "/v1/mcp/servers", body)
		if rr.Code != 400 {
			t.Errorf("%s: %d %s", body, rr.Code, rr.Body.String())
		}
	}
	req := httptest.NewRequest("POST", "/v1/mcp/servers", strings.NewReader(`{"kind":"command","command":"echo"}`))
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.handleMCPServers(rr, req)
	if rr.Code != 415 {
		t.Fatalf("content type: %d", rr.Code)
	}
	req = httptest.NewRequest("POST", "/v1/mcp/servers", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	rr = httptest.NewRecorder()
	srv.handleMCPServers(rr, req)
	if rr.Code != 403 || !strings.Contains(rr.Body.String(), "permission_error") {
		t.Fatalf("remote POST: %d %s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest("DELETE", "/v1/mcp/servers/foo", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	rr = httptest.NewRecorder()
	srv.handleMCPServerByName(rr, req)
	if rr.Code != 403 {
		t.Fatalf("remote DELETE: %d", rr.Code)
	}
	for _, tc := range []struct{ method, path, allow string }{{"PUT", "/v1/mcp/catalogue", "GET"}, {"GET", "/v1/mcp/servers", "POST"}, {"GET", "/v1/mcp/servers/foo", "DELETE"}} {
		rr := mcpConfigRequest(t, srv, tc.method, tc.path, "")
		if rr.Code != 405 || rr.Header().Get("Allow") != tc.allow {
			t.Errorf("method: %+v %d", tc, rr.Code)
		}
	}
}

func TestMCPCatalogueRegistryAndFailure(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{"exa": {URL: "https://mcp.exa.ai/mcp"}})
	fake := &fakeMCPRegistry{result: &mcp.SearchResult{Servers: []mcp.RegistryServerWrapper{
		{Server: mcp.RegistryServer{Name: "exa", Packages: []mcp.PackageInfo{{RegistryType: "npm", Identifier: "exa"}}}},
		{Server: mcp.RegistryServer{Name: "@acme/tool-mcp", Description: "A tool", Packages: []mcp.PackageInfo{{RegistryType: "npm", Identifier: "@acme/tool-mcp", Arguments: []mcp.ArgumentInfo{{Name: "token", IsRequired: true}}}}}},
		{Server: mcp.RegistryServer{Name: "unsupported"}},
	}}}
	srv := &serveServer{mcpRegistry: fake}
	rr := mcpConfigRequest(t, srv, "GET", "/v1/mcp/catalogue?q=exa", "")
	if rr.Code != 200 {
		t.Fatalf("status: %d %s", rr.Code, rr.Body.String())
	}
	var resp mcpCatalogueResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(fake.opts) != 1 || fake.opts[0].Limit != 30 || len(resp.Servers) != 2 || resp.Servers[0].ID != "bundled:exa" || !resp.Servers[0].Installed {
		t.Fatalf("catalogue: %+v opts=%+v", resp, fake.opts)
	}
	rr = mcpConfigRequest(t, srv, "GET", "/v1/mcp/catalogue?q=tool", "")
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range resp.Servers {
		if item.ID == "registry:@acme/tool-mcp" {
			found = true
			if item.Name != "tool" || !item.NeedsInput || item.Transport != "npm" {
				t.Fatalf("registry item: %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("missing registry item: %+v", resp.Servers)
	}
	fake.err = errors.New("registry offline")
	rr = mcpConfigRequest(t, srv, "GET", "/v1/mcp/catalogue?q=exa", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "registry offline") {
		t.Fatalf("registry error: %d %s", rr.Code, rr.Body.String())
	}
	fake.opts = nil
	rr = mcpConfigRequest(t, srv, "GET", "/v1/mcp/catalogue", "")
	if len(fake.opts) != 0 || rr.Code != 200 {
		t.Fatalf("empty query searched registry: %+v", fake.opts)
	}
}

func TestMCPConfigRegistryInstallAndSessionEnable(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{})
	store := newServeMCPTestStore(t)
	srv, _ := newServeMCPHandlerTestServer(t, store)
	sessionID := "sess_new_mcp"
	initial := httptest.NewRecorder()
	srv.handleSessionMCP(initial, httptest.NewRequest("GET", "/v1/sessions/"+sessionID+"/mcp", nil), sessionID)
	if initial.Code != 200 {
		t.Fatalf("initial GET: %d %s", initial.Code, initial.Body.String())
	}

	// The registry entry is resolved by exact registry name, never by a fuzzy result.
	fake := &fakeMCPRegistry{result: &mcp.SearchResult{Servers: []mcp.RegistryServerWrapper{{Server: mcp.RegistryServer{
		Name: "@acme/tool-mcp", Packages: []mcp.PackageInfo{{RegistryType: "npm", Identifier: "@acme/tool-mcp", Arguments: []mcp.ArgumentInfo{{Name: "token", IsRequired: true}}}},
	}}}}}
	srv.mcpRegistry = fake
	rr := mcpConfigRequest(t, srv, "POST", "/v1/mcp/servers", `{"kind":"catalogue","catalogue_id":"registry:@acme/tool-mcp","env":{"TOKEN":"secret"}}`)
	if rr.Code != 201 || !strings.Contains(rr.Body.String(), `"needs_input":true`) {
		t.Fatalf("registry POST: %d %s", rr.Code, rr.Body.String())
	}
	cfg, err := mcp.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers["tool"].Env["TOKEN"] != "secret" || fake.opts[0].Query != "@acme/tool-mcp" {
		t.Fatalf("registry config: %+v opts=%+v", cfg.Servers, fake.opts)
	}

	// A server added after the runtime was created can be enabled by PATCH.
	install := mcpConfigRequest(t, srv, "POST", "/v1/mcp/servers", `{"kind":"config","name":"greeter","config":{"command":"`+os.Args[0]+`","env":{"`+runServeMCPHandlerTestServerEnv+`":"1"}}}`)
	if install.Code != 201 {
		t.Fatalf("install greeter: %d %s", install.Code, install.Body.String())
	}
	req := httptest.NewRequest("PATCH", "/v1/sessions/"+sessionID+"/mcp", strings.NewReader(`{"enabled":["greeter"]}`))
	req.Header.Set("Content-Type", "application/json")
	patch := httptest.NewRecorder()
	srv.handleSessionMCP(patch, req, sessionID)
	if patch.Code != 200 {
		t.Fatalf("PATCH: %d %s", patch.Code, patch.Body.String())
	}
	state := decodeServeMCPResponse(t, patch)
	if len(state.Enabled) != 1 || state.Enabled[0] != "greeter" {
		t.Fatalf("enabled: %+v", state)
	}
}

func TestMCPConfigSessionRefresh(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{})
	srv, _ := newServeMCPHandlerTestServer(t, nil)
	sessionID := "sess_config_refresh"
	get := func() serveMCPSessionResponse {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.handleSessionMCP(rr, httptest.NewRequest("GET", "/v1/sessions/"+sessionID+"/mcp", nil), sessionID)
		if rr.Code != 200 {
			t.Fatalf("GET: %d %s", rr.Code, rr.Body.String())
		}
		return decodeServeMCPResponse(t, rr)
	}
	if got := get(); len(got.Servers) != 0 {
		t.Fatalf("initial: %+v", got)
	}
	rr := mcpConfigRequest(t, srv, "POST", "/v1/mcp/servers", `{"kind":"command","name":"new-server","command":"echo hello"}`)
	if rr.Code != 201 {
		t.Fatalf("POST: %d %s", rr.Code, rr.Body.String())
	}
	if got := get(); len(got.Servers) != 1 || got.Servers[0].Name != "new-server" {
		t.Fatalf("after POST: %+v", got)
	}
	rr = mcpConfigRequest(t, srv, "DELETE", "/v1/mcp/servers/new-server", "")
	if rr.Code != 200 {
		t.Fatalf("DELETE: %d %s", rr.Code, rr.Body.String())
	}
	if got := get(); len(got.Servers) != 0 {
		t.Fatalf("after DELETE: %+v", got)
	}
}

func TestMCPConfigDeleteLegacyName(t *testing.T) {
	writeServeMCPConfig(t, map[string]mcp.ServerConfig{"legacy__name": {Command: "echo"}})
	srv := &serveServer{}
	rr := mcpConfigRequest(t, srv, "DELETE", "/v1/mcp/servers/legacy__name", "")
	if rr.Code != 200 {
		t.Fatalf("delete legacy: %d %s", rr.Code, rr.Body.String())
	}
	rr = mcpConfigRequest(t, srv, "DELETE", "/v1/mcp/servers/a/b", "")
	if rr.Code != 400 {
		t.Fatalf("nested path: %d %s", rr.Code, rr.Body.String())
	}
}
