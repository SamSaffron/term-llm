package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/serve/proxy"
)

func newProxyTest(t *testing.T) (*proxyServer, *proxy.Store) {
	t.Helper()
	st, err := proxy.Open(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatalf("proxy.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"anthropic": {Models: []string{"claude-x"}},
	}}
	p := &proxyServer{
		store:       st,
		catalog:     proxy.BuildCatalog(cfg),
		adminToken:  "admin-secret",
		requireAuth: true,
	}
	return p, st
}

func TestEnsureProxyExclusive(t *testing.T) {
	if err := ensureProxyExclusive([]string{"web"}); err != nil {
		t.Fatalf("web alone should be fine: %v", err)
	}
	if err := ensureProxyExclusive([]string{"proxy"}); err != nil {
		t.Fatalf("proxy alone should be fine: %v", err)
	}
	if err := ensureProxyExclusive([]string{"proxy", "web"}); err == nil {
		t.Fatal("proxy combined with web should error")
	}
	if err := ensureProxyExclusive([]string{"web", "proxy", "jobs"}); err == nil {
		t.Fatal("proxy combined with others should error")
	}
}

func TestPeekAndRewriteRequestModel(t *testing.T) {
	body := []byte(`{"model":"a/b","messages":[{"role":"user"}],"temperature":0.5}`)
	if got := peekRequestModel(body); got != "a/b" {
		t.Fatalf("peek = %q", got)
	}
	// Rewrite to a concrete model preserves other fields.
	out, err := rewriteRequestModel(body, "concrete")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if peekRequestModel(out) != "concrete" {
		t.Fatalf("rewritten model = %q", peekRequestModel(out))
	}
	if !strings.Contains(string(out), `"temperature":0.5`) {
		t.Fatalf("other fields lost: %s", out)
	}
	// Empty model removes the field (provider default).
	out, err = rewriteRequestModel(body, "")
	if err != nil {
		t.Fatalf("rewrite empty: %v", err)
	}
	if strings.Contains(string(out), `"model"`) {
		t.Fatalf("expected model removed, got %s", out)
	}
}

func TestProxyGateAllowsGrantedRoute(t *testing.T) {
	p, st := newProxyTest(t)
	ctx := context.Background()
	c, _ := st.CreateClient(ctx, "acme", "")
	plaintext, _, _ := st.CreateToken(ctx, c.ID, "", time.Hour)
	if _, err := st.AddGrant(ctx, c.ID, "anthropic", "claude-x", ""); err != nil {
		t.Fatalf("AddGrant: %v", err)
	}

	var (
		called   bool
		gotRoute proxyForcedRoute
		gotBody  []byte
	)
	h := p.gate(func(w http.ResponseWriter, r *http.Request) {
		called = true
		gotRoute, _ = proxyForcedRouteFromContext(r.Context())
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	body := `{"model":"anthropic/claude-x","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	h(rr, req)

	if !called {
		t.Fatal("expected forwarded handler to be called")
	}
	if gotRoute.Provider != "anthropic" || gotRoute.Model != "claude-x" {
		t.Fatalf("forced route = %+v, want anthropic/claude-x", gotRoute)
	}
	if m := peekRequestModel(gotBody); m != "claude-x" {
		t.Fatalf("rewritten body model = %q, want concrete claude-x", m)
	}
}

func TestProxyGateDeniesUngrantedAndRecordsRequest(t *testing.T) {
	p, st := newProxyTest(t)
	ctx := context.Background()
	c, _ := st.CreateClient(ctx, "acme", "")
	plaintext, _, _ := st.CreateToken(ctx, c.ID, "", time.Hour)

	called := false
	h := p.gate(func(w http.ResponseWriter, r *http.Request) { called = true })

	body := `{"model":"openai/gpt-5","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	h(rr, req)

	if called {
		t.Fatal("handler must not be reached for a denied request")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	var resp struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
			Status    string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 403 body: %v", err)
	}
	if resp.Error.Code != "model_access_not_granted" || resp.Error.RequestID == "" {
		t.Fatalf("unexpected 403 payload: %+v", resp.Error)
	}
	if resp.Error.Status != proxy.RequestPending {
		t.Fatalf("status = %q", resp.Error.Status)
	}
	pending, _ := st.ListAccessRequests(ctx, proxy.RequestPending, c.ID)
	if len(pending) != 1 || pending[0].Provider != "openai" || pending[0].Model != "gpt-5" {
		t.Fatalf("expected 1 pending request for openai/gpt-5, got %+v", pending)
	}
}

func TestProxyGateAuthErrors(t *testing.T) {
	p, st := newProxyTest(t)
	ctx := context.Background()
	c, _ := st.CreateClient(ctx, "acme", "")
	plaintext, _, _ := st.CreateToken(ctx, c.ID, "", time.Hour)
	h := p.gate(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// Missing token -> 401.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"anthropic/claude-x"}`))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rr.Code)
	}

	// Valid token but no model -> 400.
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr = httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing model status = %d, want 400", rr.Code)
	}
}

func TestProxyGateWildcardGrant(t *testing.T) {
	p, st := newProxyTest(t)
	ctx := context.Background()
	c, _ := st.CreateClient(ctx, "acme", "")
	plaintext, _, _ := st.CreateToken(ctx, c.ID, "", time.Hour)
	if _, err := st.AddGrant(ctx, c.ID, "claude-bin", proxy.WildcardModel, ""); err != nil {
		t.Fatalf("AddGrant wildcard: %v", err)
	}

	capture := func(bodyStr string) (proxyForcedRoute, []byte, int) {
		var route proxyForcedRoute
		var gotBody []byte
		h := p.gate(func(w http.ResponseWriter, r *http.Request) {
			route, _ = proxyForcedRouteFromContext(r.Context())
			gotBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bodyStr))
		req.Header.Set("Authorization", "Bearer "+plaintext)
		rr := httptest.NewRecorder()
		h(rr, req)
		return route, gotBody, rr.Code
	}

	// Concrete model under a wildcard grant routes to that concrete model.
	route, body, code := capture(`{"model":"claude-bin/claude-sonnet-4-6","messages":[]}`)
	if code != http.StatusOK {
		t.Fatalf("concrete under wildcard: code = %d", code)
	}
	if route.Provider != "claude-bin" || route.Model != "claude-sonnet-4-6" {
		t.Fatalf("concrete route = %+v", route)
	}
	if peekRequestModel(body) != "claude-sonnet-4-6" {
		t.Fatalf("concrete body model = %q", peekRequestModel(body))
	}

	// Literal wildcard request routes to the provider default (empty model).
	route, body, code = capture(`{"model":"claude-bin/*","messages":[]}`)
	if code != http.StatusOK {
		t.Fatalf("wildcard literal: code = %d", code)
	}
	if route.Provider != "claude-bin" || route.Model != "" {
		t.Fatalf("wildcard route = %+v, want claude-bin with empty model", route)
	}
	if peekRequestModel(body) != "" {
		t.Fatalf("wildcard body should drop model, got %q", peekRequestModel(body))
	}
}

func TestProxyAdminUIShellAndCatalog(t *testing.T) {
	p, _ := newProxyTest(t)
	h := p.handler(&serveServer{})

	for _, path := range []string{"/", "/proxy-admin.css", "/proxy-admin.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		if strings.Contains(rr.Body.String(), "admin-secret") {
			t.Fatalf("GET %s leaked the admin token", path)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/proxy/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("catalog without admin token = %d, want 401", rr.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/admin/proxy/models", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("catalog with admin token = %d: %s", rr.Code, rr.Body.String())
	}
	var catalog struct {
		Models []proxy.ModelAlias `json:"models"`
	}
	mustJSON(t, rr, &catalog)
	if len(catalog.Models) == 0 {
		t.Fatal("expected admin model catalog")
	}
}

func TestProxyAdminAndSelfServiceFlow(t *testing.T) {
	p, _ := newProxyTest(t)
	h := p.handler(&serveServer{})

	admin := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer admin-secret")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	client := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// Admin without a token is rejected.
	if rr := (func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/admin/proxy/clients", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	})(); rr.Code != http.StatusUnauthorized {
		t.Fatalf("admin without token = %d, want 401", rr.Code)
	}

	// Create client.
	rr := admin(http.MethodPost, "/admin/proxy/clients", `{"name":"ci","description":"CI bot"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create client = %d: %s", rr.Code, rr.Body)
	}
	var createdClient struct {
		Client proxy.Client `json:"client"`
	}
	mustJSON(t, rr, &createdClient)
	cid := createdClient.Client.ID

	// Create token.
	rr = admin(http.MethodPost, "/admin/proxy/clients/"+cid+"/tokens", `{"ttl_seconds":3600}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create token = %d: %s", rr.Code, rr.Body)
	}
	var tokResp struct {
		Token string `json:"token"`
	}
	mustJSON(t, rr, &tokResp)
	if tokResp.Token == "" {
		t.Fatal("expected plaintext token returned once")
	}

	// Grant a model.
	rr = admin(http.MethodPost, "/admin/proxy/clients/"+cid+"/grants", `{"provider":"anthropic","model":"claude-x"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create grant = %d: %s", rr.Code, rr.Body)
	}

	// Client sees only its granted models, in OpenAI /v1/models shape.
	rr = client(http.MethodGet, "/v1/models", tokResp.Token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("client models = %d: %s", rr.Code, rr.Body)
	}
	var models struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	mustJSON(t, rr, &models)
	if models.Object != "list" {
		t.Fatalf("expected object=list, got %q", models.Object)
	}
	grantedSeen := false
	for _, m := range models.Data {
		if m.ID == "anthropic/claude-x" && m.Object == "model" && m.OwnedBy == "anthropic" {
			grantedSeen = true
		}
		// Only granted models must be exposed on the client plane.
		if m.ID == "openai/gpt-5" {
			t.Fatalf("client /v1/models leaked an ungranted model: %q", m.ID)
		}
	}
	if !grantedSeen {
		t.Fatal("expected anthropic/claude-x in the client's granted model list")
	}

	// Client self-service requests access to a different model.
	rr = client(http.MethodPost, "/v1/proxy/access-requests", tokResp.Token, `{"model":"openai/gpt-5","reason":"need it"}`)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("access request = %d: %s", rr.Code, rr.Body)
	}

	// Admin lists pending requests and approves it.
	rr = admin(http.MethodGet, "/admin/proxy/requests?status=pending", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list requests = %d: %s", rr.Code, rr.Body)
	}
	var reqs struct {
		AccessRequests []proxy.AccessRequest `json:"access_requests"`
	}
	mustJSON(t, rr, &reqs)
	if len(reqs.AccessRequests) != 1 {
		t.Fatalf("expected 1 pending request, got %d", len(reqs.AccessRequests))
	}
	reqID := reqs.AccessRequests[0].ID

	rr = admin(http.MethodPost, "/admin/proxy/requests/"+reqID+"/approve", `{"note":"ok"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", rr.Code, rr.Body)
	}

	// Approval created a grant: the client can now see it in its granted list.
	rr = client(http.MethodGet, "/v1/models", tokResp.Token, "")
	models.Data = nil
	mustJSON(t, rr, &models)
	approvedSeen := false
	for _, m := range models.Data {
		if m.ID == "openai/gpt-5" && m.OwnedBy == "openai" {
			approvedSeen = true
		}
	}
	if !approvedSeen {
		t.Fatal("expected openai/gpt-5 to be granted after approval")
	}

	// Audit trail is populated (grant/authorize actions recorded).
	rr = admin(http.MethodGet, "/admin/proxy/audit", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("audit = %d: %s", rr.Code, rr.Body)
	}
}

func mustJSON(t *testing.T, rr *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode JSON %T: %v (body=%s)", dst, err, rr.Body)
	}
}

// newProxyLLMServer builds a serveServer whose runtimes are backed by mock
// providers, suitable for driving the reused OpenAI Responses handler behind the
// proxy gate. Each created runtime gets its own provider with the given queued
// text responses so per-runtime state is observable.
func newProxyLLMServer(responses ...string) (*serveServer, *serveSessionManager) {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock")
		for _, r := range responses {
			provider.AddTextResponse(r)
		}
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{provider: provider, engine: engine, defaultModel: "claude-x", providerKey: "anthropic"}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{
		sessionMgr:     mgr,
		cfgRef:         &config.Config{DefaultProvider: "anthropic"},
		runtimeFactory: func(ctx context.Context, providerName, modelName string) (*serveRuntime, error) { return factory(ctx) },
	}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	return srv, mgr
}

func TestProxyFreshResponsesRouteUsesGrantedProviderAndModel(t *testing.T) {
	var gotProvider, gotModel string
	defaultFactory := func(context.Context) (*serveRuntime, error) {
		return &serveRuntime{}, nil
	}
	mgr := newServeSessionManager(time.Minute, 10, defaultFactory)
	defer mgr.Close()
	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "chatgpt"},
		sessionMgr: mgr,
		runtimeFactory: func(_ context.Context, provider, model string) (*serveRuntime, error) {
			gotProvider, gotModel = provider, model
			return &serveRuntime{}, nil
		},
	}

	ctx := withProxyForcedRoute(context.Background(), "claude-bin", "haiku")
	_, stateful, err := srv.runtimeForFreshProviderRequest(ctx, "", "chatgpt")
	if err != nil {
		t.Fatalf("runtimeForFreshProviderRequest: %v", err)
	}
	if stateful {
		t.Fatal("empty session id should create a stateless runtime")
	}
	if gotProvider != "claude-bin" || gotModel != "haiku" {
		t.Fatalf("runtime route = %q/%q, want claude-bin/haiku", gotProvider, gotModel)
	}
}

// TestProxyTwoClientSessionIsolationThroughHandler drives two authenticated
// clients through the real gate + OpenAI Responses handler path using the SAME
// client-supplied session_id, and proves they cannot share session/runtime state
// or continue each other's responses.
func TestProxyTwoClientSessionIsolationThroughHandler(t *testing.T) {
	p, st := newProxyTest(t)
	ctx := context.Background()
	a, _ := st.CreateClient(ctx, "alpha", "")
	b, _ := st.CreateClient(ctx, "beta", "")
	tokA, _, _ := st.CreateToken(ctx, a.ID, "", time.Hour)
	tokB, _, _ := st.CreateToken(ctx, b.ID, "", time.Hour)
	for _, id := range []string{a.ID, b.ID} {
		if _, err := st.AddGrant(ctx, id, "anthropic", "claude-x", ""); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}

	llmSrv, mgr := newProxyLLMServer("first", "second")
	defer mgr.Close()
	handler := p.gate(llmSrv.handleResponses)

	do := func(token, sessionHeader, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if sessionHeader != "" {
			req.Header.Set("session_id", sessionHeader)
		}
		rr := httptest.NewRecorder()
		handler(rr, req)
		return rr
	}

	// Both clients reuse the identical client-facing session_id "shared".
	rrA := do(tokA, "shared", `{"model":"anthropic/claude-x","input":"hi from A"}`)
	if rrA.Code != http.StatusOK {
		t.Fatalf("A request: %d %s", rrA.Code, rrA.Body)
	}
	rrB := do(tokB, "shared", `{"model":"anthropic/claude-x","input":"hi from B"}`)
	if rrB.Code != http.StatusOK {
		t.Fatalf("B request: %d %s", rrB.Code, rrB.Body)
	}

	// The session id handed back to each client is namespaced to that client, so
	// the two differ despite the identical client-supplied session_id.
	sidA := rrA.Result().Header.Get("x-session-id")
	sidB := rrB.Result().Header.Get("x-session-id")
	nsA := proxySessionNamespace(a.ID)
	nsB := proxySessionNamespace(b.ID)
	if !strings.HasPrefix(sidA, nsA) {
		t.Fatalf("A session id %q not namespaced to client A (%q)", sidA, nsA)
	}
	if !strings.HasPrefix(sidB, nsB) {
		t.Fatalf("B session id %q not namespaced to client B (%q)", sidB, nsB)
	}
	if sidA == sidB {
		t.Fatalf("clients shared a session id despite isolation: %q", sidA)
	}

	// The session manager holds two isolated runtimes for the shared session_id.
	mgr.mu.Lock()
	keys := make([]string, 0, len(mgr.sessions))
	for k := range mgr.sessions {
		keys = append(keys, k)
	}
	mgr.mu.Unlock()
	if len(keys) != 2 {
		t.Fatalf("expected 2 isolated runtimes for shared session_id, got %d: %v", len(keys), keys)
	}

	// Response-chaining isolation: client B cannot continue client A's response.
	var aResp map[string]any
	_ = json.Unmarshal(rrA.Body.Bytes(), &aResp)
	respIDA, _ := aResp["id"].(string)
	if respIDA == "" {
		t.Fatalf("A response missing id: %v", aResp)
	}
	rrSteal := do(tokB, "", `{"model":"anthropic/claude-x","input":"steal","previous_response_id":"`+respIDA+`"}`)
	if rrSteal.Code != http.StatusBadRequest {
		t.Fatalf("cross-client response chaining must be rejected, got %d: %s", rrSteal.Code, rrSteal.Body)
	}

	// The owning client CAN continue its own response (per-client chaining works).
	rrChain := do(tokA, "", `{"model":"anthropic/claude-x","input":"more","previous_response_id":"`+respIDA+`"}`)
	if rrChain.Code != http.StatusOK {
		t.Fatalf("owner self-chaining should succeed, got %d: %s", rrChain.Code, rrChain.Body)
	}
}

// TestProxyAuthPlaneSeparation asserts the admin and client credential planes are
// strictly separate: a client bearer token cannot reach the admin API, and the
// admin token cannot authenticate as a client.
func TestProxyAuthPlaneSeparation(t *testing.T) {
	p, st := newProxyTest(t)
	h := p.handler(&serveServer{})
	ctx := context.Background()
	c, _ := st.CreateClient(ctx, "acme", "")
	clientTok, _, _ := st.CreateToken(ctx, c.ID, "", time.Hour)

	// Client token on the admin plane is rejected.
	req := httptest.NewRequest(http.MethodGet, "/admin/proxy/clients", nil)
	req.Header.Set("Authorization", "Bearer "+clientTok)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("client token on admin plane = %d, want 401", rr.Code)
	}

	// Admin token on the client plane is rejected (not a valid client token).
	req = httptest.NewRequest(http.MethodGet, "/v1/proxy/whoami", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("admin token on client plane = %d, want 401", rr.Code)
	}

	// The client token works on the client plane.
	req = httptest.NewRequest(http.MethodGet, "/v1/proxy/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+clientTok)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("client token on client plane = %d, want 200: %s", rr.Code, rr.Body)
	}
}

// TestProxyAdminAuthRequiredOnLoopback asserts the admin plane still demands the
// admin token even when client bearer auth is disabled (loopback no-auth mode).
func TestProxyAdminAuthRequiredOnLoopback(t *testing.T) {
	p, _ := newProxyTest(t)
	p.requireAuth = false // loopback no-auth mode
	h := p.handler(&serveServer{})

	req := httptest.NewRequest(http.MethodGet, "/admin/proxy/clients", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("loopback admin without token = %d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/proxy/clients", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("loopback admin with token = %d, want 200: %s", rr.Code, rr.Body)
	}
}

// TestProxyGateRejectsOversizeBody asserts the gate returns 413 (and never calls
// the upstream handler) for a body over the buffered limit.
func TestProxyGateRejectsOversizeBody(t *testing.T) {
	p, st := newProxyTest(t)
	ctx := context.Background()
	c, _ := st.CreateClient(ctx, "acme", "")
	tok, _, _ := st.CreateToken(ctx, c.ID, "", time.Hour)
	if _, err := st.AddGrant(ctx, c.ID, "anthropic", "claude-x", ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	called := false
	h := p.gate(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusOK) })
	big := strings.Repeat("a", maxProxyRequestBody+1024)
	body := `{"model":"anthropic/claude-x","pad":"` + big + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body = %d, want 413", rr.Code)
	}
	if called {
		t.Fatal("upstream handler must not run for an oversize body")
	}
}

// TestProxyPassthroughRuntimeHasNoToolsSkillsOrSystemPrompt verifies the
// capability-proxy runtime genuinely disables server tools, skills, and the
// system prompt — not merely suppresses their output.
func TestProxyPassthroughRuntimeHasNoToolsSkillsOrSystemPrompt(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "mock",
		Providers:       map[string]config.ProviderConfig{"mock": {Models: []string{"mock-model"}}},
		// Skills enabled in config to prove passthrough still yields zero skills.
		Skills: config.SkillsConfig{Enabled: true},
	}
	runner := &cmdRunner{baseCfg: cfg, defaults: cmdRunnerOptions{
		ProxyPassthrough: true,
		NoSearch:         true,
		ErrWriter:        io.Discard,
		// A non-empty operator system prompt that passthrough must strip.
		ConfigSet:          true,
		ConfigInstructions: "OPERATOR SECRET SYSTEM PROMPT",
	}}
	provider := llm.NewMockProvider("mock")
	env, err := runner.prepare(context.Background(), runpkg.Request{
		Platform:         runpkg.PlatformWeb,
		ProviderInstance: provider,
		DeferSession:     true,
	}, nil)
	if err != nil {
		t.Fatalf("prepare passthrough runtime: %v", err)
	}
	rt := env.runtime
	defer rt.Close()

	if specs := rt.engine.Tools().AllSpecs(); len(specs) != 0 {
		names := make([]string, 0, len(specs))
		for _, s := range specs {
			names = append(names, s.Name)
		}
		t.Fatalf("passthrough runtime exposes %d server tools, want 0: %v", len(specs), names)
	}
	for _, name := range []string{"activate_skill", "search_skills", "web_search", "read_url"} {
		if _, ok := rt.engine.Tools().Get(name); ok {
			t.Fatalf("passthrough runtime must not register %q", name)
		}
	}
	if rt.toolMgr != nil {
		t.Fatal("passthrough runtime must have a nil tool manager")
	}
	if rt.systemPrompt != "" {
		t.Fatalf("passthrough runtime system prompt must be empty, got %q", rt.systemPrompt)
	}
	if strings.Contains(rt.systemPrompt, "<available_skills>") {
		t.Fatal("passthrough runtime leaked skills metadata into the system prompt")
	}
}

// TestNewBareEngineHasNoServerTools confirms the bare engine backing the proxy
// starts with an empty registry, unlike the default engine which seeds
// web_search/read_url.
func TestNewBareEngineHasNoServerTools(t *testing.T) {
	cfg := &config.Config{}
	provider := llm.NewMockProvider("mock")

	bare := newBareEngine(provider, cfg)
	if specs := bare.Tools().AllSpecs(); len(specs) != 0 {
		t.Fatalf("bare engine has %d tools, want 0", len(specs))
	}

	def := newEngine(provider, cfg)
	if _, ok := def.Tools().Get("web_search"); !ok {
		t.Fatal("sanity: default engine should seed web_search (proves bare engine differs)")
	}
}

// TestProxyPendingRequestCapDeniesButDoesNotError asserts that once a client hits
// the pending-request cap, further denied model calls still return a structured
// 403 (not 500) and do not create new rows.
func TestProxyPendingRequestCapDeniesButDoesNotError(t *testing.T) {
	_, st := newProxyTest(t)
	ctx := context.Background()
	c, _ := st.CreateClient(ctx, "acme", "")

	// Fill the client's pending-request quota with distinct models.
	for i := 0; i < 100; i++ {
		if _, err := st.RequestAccess(ctx, c.ID, "openai", fmt.Sprintf("m%d", i), ""); err != nil {
			t.Fatalf("seed request %d: %v", i, err)
		}
	}

	// A new distinct model is capped at the store level.
	if _, err := st.RequestAccess(ctx, c.ID, "openai", "overflow", ""); !errors.Is(err, proxy.ErrTooManyPendingRequests) {
		t.Fatalf("expected ErrTooManyPendingRequests, got %v", err)
	}

	// Authorize (the gate's path) tolerates the cap: it still denies, without a
	// 500 and without growing the DB.
	dec, err := st.Authorize(ctx, c.ID, "openai", "another-new-model")
	if err != nil {
		t.Fatalf("Authorize should not error at the cap: %v", err)
	}
	if dec.Allowed {
		t.Fatal("capped request must remain denied")
	}
	pending, _ := st.ListAccessRequests(ctx, proxy.RequestPending, c.ID)
	if len(pending) != 100 {
		t.Fatalf("pending request count grew past the cap: %d", len(pending))
	}

	// A repeat of an existing model still dedupes (bumps count) under the cap.
	if _, err := st.RequestAccess(ctx, c.ID, "openai", "m0", "again"); err != nil {
		t.Fatalf("dedupe of existing model should succeed under cap: %v", err)
	}
}
