package cmd

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	mcpoauth "github.com/samsaffron/term-llm/internal/mcp/oauth"
	"golang.org/x/oauth2"
)

func TestServeMCPOAuthRoutesProtectFlowButNotStateGatedCallback(t *testing.T) {
	s := &serveServer{cfg: serveServerConfig{basePath: "/ui", requireAuth: true, token: "serve-token"}}
	handler := s.httpHandler()

	flowReq := httptest.NewRequest("GET", "http://example.test/ui/v1/mcp/oauth/flows/unknown", nil)
	flowRec := httptest.NewRecorder()
	handler.ServeHTTP(flowRec, flowReq)
	if flowRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated flow status = %d, want 401", flowRec.Code)
	}

	callbackReq := httptest.NewRequest("GET", "http://hostile.test/ui/v1/mcp/oauth/callback?state=invalid&code=secret-code&redirect_uri=https://evil.example", nil)
	callbackRec := httptest.NewRecorder()
	handler.ServeHTTP(callbackRec, callbackReq)
	if callbackRec.Code != http.StatusBadRequest {
		t.Fatalf("state-gated callback status = %d, want 400 (not auth rejection)", callbackRec.Code)
	}
	body := callbackRec.Body.String()
	if strings.Contains(body, "secret-code") || strings.Contains(body, "evil.example") {
		t.Fatalf("callback reflected request secrets: %q", body)
	}
}

func TestServeMCPOAuthCallbackURL(t *testing.T) {
	tests := []struct {
		name      string
		publicURL string
		basePath  string
		host      string
		tls       bool
		want      string
	}{
		{name: "derived http", basePath: "/ui", host: "127.0.0.1:8080", want: "http://127.0.0.1:8080/ui/v1/mcp/oauth/callback"},
		{name: "derived https", basePath: "/chat", host: "chat.example", tls: true, want: "https://chat.example/chat/v1/mcp/oauth/callback"},
		{name: "explicit hub mount", publicURL: "https://hub.example/node/demo", basePath: "/ui", host: "internal:8080", want: "https://hub.example/node/demo/v1/mcp/oauth/callback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &serveServer{cfg: serveServerConfig{basePath: tt.basePath, publicURL: tt.publicURL}}
			r := httptest.NewRequest("POST", "http://"+tt.host+"/", nil)
			r.Host = tt.host
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			got, err := s.mcpOAuthCallbackURL(r)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("callback URL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeServePublicURL(t *testing.T) {
	if got, err := normalizeServePublicURL(" https://example.com/node/demo/ "); err != nil || got != "https://example.com/node/demo" {
		t.Fatalf("normalize = %q, %v", got, err)
	}
	for _, raw := range []string{"javascript:alert(1)", "https://example.com/path?redirect=evil", "https://user@example.com"} {
		if _, err := normalizeServePublicURL(raw); err == nil {
			t.Errorf("normalizeServePublicURL(%q) succeeded", raw)
		}
	}
}

func TestParseSessionMCPOAuthSuffix(t *testing.T) {
	server, action, ok := parseSessionMCPOAuthSuffix("mcp/github/oauth/start")
	if !ok || server != "github" || action != "start" {
		t.Fatalf("parse = %q, %q, %v", server, action, ok)
	}
	if _, _, ok := parseSessionMCPOAuthSuffix("mcp/github/oauth/start/extra"); ok {
		t.Fatal("accepted extra path segment")
	}
}

func runServeMCPOAuthTestIsolated(t *testing.T) bool {
	t.Helper()
	const childEnv = "TERM_LLM_TEST_MCP_OAUTH_COMPLETION"
	if os.Getenv(childEnv) == t.Name() {
		// Bind the global coordinator to this isolated home before helpers
		// temporarily change XDG_CONFIG_HOME to write individual mcp.json files.
		mcpoauth.DefaultCoordinator()
		return true
	}
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"="+t.Name(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CACHE_HOME="+filepath.Join(home, "cache"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated OAuth test: %v\n%s", err, output)
	}
	return false
}

func waitForServeMCPTool(t *testing.T, rt *serveRuntime, name string) llm.Tool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if tool, ok := rt.engine.Tools().Get(name); ok {
			return tool
		}
		select {
		case <-ctx.Done():
			t.Fatalf("planner did not register tool %s", name)
		case <-ticker.C:
		}
	}
}

func patchServeMCPOAuthSelection(t *testing.T, srv *serveServer, sessionID string, selected bool) {
	t.Helper()
	body := `{"enabled":[]}`
	if selected {
		body = `{"enabled":["protected"]}`
	}
	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/"+sessionID+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("selection status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestServeMCPOAuthCompletionReconnectsSelectedServer(t *testing.T) {
	if !runServeMCPOAuthTestIsolated(t) {
		return
	}
	for _, tc := range []struct {
		name    string
		mode    string
		initial bool
		final   bool
	}{
		{name: "default planner", initial: true, final: true},
		{name: "eager planner", mode: "eager", initial: true, final: true},
		{name: "deferred planner", mode: "deferred", initial: true, final: true},
		{name: "deselected during flow", initial: true},
		{name: "selected during flow", final: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mcpServer := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "protected", Version: "1"}, nil)
			sdkmcp.AddTool(mcpServer, &sdkmcp.Tool{Name: "greet"}, func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, any, error) {
				return &sdkmcp.CallToolResult{}, nil, nil
			})
			mcpHandler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return mcpServer }, &sdkmcp.StreamableHTTPOptions{Stateless: true})
			var server *httptest.Server
			mux := http.NewServeMux()
			mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer signed-in" {
					mcpHandler.ServeHTTP(w, r)
					return
				}
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, server.URL+"/.well-known/oauth-protected-resource/mcp"))
				w.WriteHeader(http.StatusUnauthorized)
			})
			mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{"resource": server.URL + "/mcp", "authorization_servers": []string{server.URL}})
			})
			mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"}})
			})
			mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{"access_token": "signed-in", "token_type": "Bearer", "expires_in": 3600})
			})
			server = httptest.NewServer(mux)
			t.Cleanup(server.Close)
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{"protected": {URL: server.URL + "/mcp", OAuth: &mcp.OAuthConfig{ClientID: "registered-client"}}})
			store := newServeMCPTestStore(t)
			srv, _ := newServeMCPHandlerTestServer(t, store)
			rt, err := srv.metadataRuntime(t.Context(), "sess_oauth_completion")
			if err != nil {
				t.Fatal(err)
			}
			rt.toolDiscovery.Mode = tc.mode
			patchServeMCPOAuthSelection(t, srv, "sess_oauth_completion", tc.initial)
			if _, attached := rt.engine.ToolDiscoveryDiagnostics(""); !attached {
				t.Fatal("discovery planner was not attached")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			flow, err := rt.mcpManager.StartOAuth(ctx, "protected", mcp.OAuthStartOptions{RedirectURL: "http://127.0.0.1/callback", SkipReconnect: true})
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(flow.AuthorizationURL)
			if err != nil {
				t.Fatal(err)
			}
			patchServeMCPOAuthSelection(t, srv, "sess_oauth_completion", tc.final)
			coordinator := mcpoauth.DefaultCoordinator()
			if _, accepted := coordinator.CompleteCallback(u.Query().Get("state"), "test-code", "", ""); !accepted {
				t.Fatal("callback not accepted")
			}
			completed, err := coordinator.Wait(ctx, flow.ID)
			if err != nil || completed == nil || completed.State != mcpoauth.FlowSucceeded {
				t.Fatalf("completion=%#v err=%v", completed, err)
			}
			updates := make(chan mcp.StatusUpdate, 10)
			rt.mcpManager.SetStatusChannel(updates)
			srv.publishMCPOAuthCompletion("sess_oauth_completion", "protected", flow.ID, rt.mcpManager)
			if !tc.final {
				if status, _ := rt.mcpManager.ServerStatus("protected"); status != mcp.StatusStopped {
					t.Fatalf("deselected server restarted: %s", status)
				}
				if _, ok := rt.engine.Tools().Get("protected__greet"); ok {
					t.Fatal("deselected server's tool was registered")
				}
				return
			}
			for {
				select {
				case update := <-updates:
					if update.Status == mcp.StatusStopped || update.Status == mcp.StatusStarting {
						continue
					}
					if update.Status != mcp.StatusReady {
						t.Fatalf("reconnect status=%s err=%v", update.Status, update.Error)
					}
					if tools := rt.mcpManager.AllTools(); len(tools) != 1 || tools[0].Name != "protected__greet" {
						t.Fatalf("reconnected tools=%v", tools)
					}
					tool := waitForServeMCPTool(t, rt, "protected__greet")
					if _, err := tool.Execute(ctx, nil); err != nil {
						t.Fatalf("reconnected tool call: %v", err)
					}
					return
				case <-ctx.Done():
					t.Fatal("selected server did not reconnect after sign-in")
				}
			}
		})
	}
}

func TestServeMCPEnsurePicksUpExternalSignIn(t *testing.T) {
	if !runServeMCPOAuthTestIsolated(t) {
		return
	}
	for _, tc := range []struct {
		name      string
		expired   bool
		refresh   bool
		retry     bool
		rejected  bool
		wantState mcpoauth.AuthState
	}{
		{name: "signed in elsewhere", wantState: mcpoauth.AuthSignedIn},
		{name: "expired refreshable", expired: true, refresh: true, wantState: mcpoauth.AuthExpired},
		{name: "needs sign-in", expired: true, wantState: mcpoauth.AuthRequired},
		{name: "temporary refresh failure", expired: true, refresh: true, retry: true, wantState: mcpoauth.AuthRetry},
		{name: "rejected external grant does not restart repeatedly", rejected: true, wantState: mcpoauth.AuthSignedIn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := mcpoauth.DefaultStorePath()
			if err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "protected", Version: "1"}, nil)
			sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "greet"}, func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, any, error) {
				return &sdkmcp.CallToolResult{}, nil, nil
			})
			handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true})
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				requests.Add(1)
				if r.Header.Get("Authorization") == "Bearer external" && !tc.rejected {
					handler.ServeHTTP(w, r)
					return
				}
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			t.Cleanup(httpServer.Close)
			endpoint := httpServer.URL + "/mcp"
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{"protected": {URL: endpoint}})
			store := newServeMCPTestStore(t)
			srv, created := newServeMCPHandlerTestServer(t, store)
			patchServeMCPOAuthSelection(t, srv, "sess_external_sign_in", true)
			rt := *created

			grantStore := mcpoauth.NewFileStore(path)
			expiry := time.Now().Add(time.Hour)
			if tc.expired {
				expiry = time.Now().Add(-time.Hour)
			}
			refresh := ""
			if tc.refresh {
				refresh = "refresh"
			}
			_, err = grantStore.Update(endpoint, func(*mcpoauth.Session) (*mcpoauth.Session, error) {
				return &mcpoauth.Session{Endpoint: endpoint, Config: mcpoauth.OAuth2Config{ClientID: "client", Endpoint: oauth2.Endpoint{TokenURL: httpServer.URL + "/token"}}, Token: &oauth2.Token{AccessToken: "external", RefreshToken: refresh, Expiry: expiry}}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.retry {
				handler, err := mcpoauth.DefaultCoordinator().Handler(endpoint, mcpoauth.Options{HTTPClient: httpServer.Client()})
				if err != nil {
					t.Fatal(err)
				}
				source, err := handler.TokenSource(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := source.Token(); err == nil {
					t.Fatal("expected refresh failure")
				}
			}
			if got := rt.mcpManager.AuthStatuses()["protected"].State; got != tc.wantState {
				t.Fatalf("stored grant state=%s, want %s", got, tc.wantState)
			}
			before := requests.Load()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := srv.ensureRuntimeMCPForSession(ctx, "sess_external_sign_in", rt); err != nil {
				t.Fatal(err)
			}
			if tc.wantState != mcpoauth.AuthSignedIn {
				if got := requests.Load(); got != before {
					t.Fatalf("non-signed-in grant triggered reconnect: %d -> %d", before, got)
				}
				return
			}
			if got := requests.Load(); got == before {
				t.Fatal("external sign-in did not restart selected server")
			}
			if tc.rejected {
				if status, _ := rt.mcpManager.ServerStatus("protected"); status != mcp.StatusAuthRequired {
					t.Fatalf("rejected external grant status=%s", status)
				}
				before = requests.Load()
				if err := srv.ensureRuntimeMCPForSession(ctx, "sess_external_sign_in", rt); err != nil {
					t.Fatal(err)
				}
				if got := requests.Load(); got != before {
					t.Fatalf("rejected grant retried per request: %d -> %d", before, got)
				}
				return
			}
			if status, _ := rt.mcpManager.ServerStatus("protected"); status != mcp.StatusReady {
				t.Fatalf("external sign-in status=%s", status)
			}
			tool := waitForServeMCPTool(t, rt, "protected__greet")
			if _, err := tool.Execute(ctx, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestServeMCPInsufficientScopeDoesNotRestartUnchangedGrant(t *testing.T) {
	if !runServeMCPOAuthTestIsolated(t) {
		return
	}
	for _, noExpiry := range []bool{false, true} {
		t.Run(fmt.Sprintf("no expiry=%v", noExpiry), func(t *testing.T) {
			path, err := mcpoauth.DefaultStorePath()
			if err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "scoped", Version: "1"}, nil)
			sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "greet"}, func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, any, error) {
				return &sdkmcp.CallToolResult{}, nil, nil
			})
			handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true})
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") == "Bearer broader" {
					handler.ServeHTTP(w, r)
					return
				}
				w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="write"`)
				w.WriteHeader(http.StatusForbidden)
			}))
			t.Cleanup(httpServer.Close)
			store := mcpoauth.NewFileStore(path)
			expiry := time.Now().Add(time.Hour)
			if noExpiry {
				expiry = time.Time{}
			}
			_, err = store.Update(httpServer.URL, func(*mcpoauth.Session) (*mcpoauth.Session, error) {
				return &mcpoauth.Session{Endpoint: httpServer.URL, Config: mcpoauth.OAuth2Config{ClientID: "client"}, Token: &oauth2.Token{AccessToken: "limited", Expiry: expiry}}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{"protected": {URL: httpServer.URL}})
			srv, created := newServeMCPHandlerTestServer(t, newServeMCPTestStore(t))
			patchServeMCPOAuthSelection(t, srv, "sess_scope_step_up", true)
			rt := *created
			if rt.mcpManager.AuthStatuses()["protected"].State != mcpoauth.AuthSignedIn {
				t.Fatal("scope challenge invalidated stored grant")
			}
			before := requests.Load()
			for range 3 {
				if err := srv.ensureRuntimeMCPForSession(t.Context(), "sess_scope_step_up", rt); err != nil {
					t.Fatal(err)
				}
			}
			if got := requests.Load(); got != before {
				t.Errorf("unchanged insufficient-scope grant restarted per request: %d -> %d", before, got)
			}
			// New sign-ins need detection even when expiry is unchanged or absent.
			_, err = store.Update(httpServer.URL, func(current *mcpoauth.Session) (*mcpoauth.Session, error) {
				current.Token.AccessToken = "broader"
				return current, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.ensureRuntimeMCPForSession(t.Context(), "sess_scope_step_up", rt); err != nil {
				t.Fatal(err)
			}
			if status, _ := rt.mcpManager.ServerStatus("protected"); status != mcp.StatusReady {
				t.Fatalf("new grant did not reconnect: %s", status)
			}
			waitForServeMCPTool(t, rt, "protected__greet")
		})
	}
}
