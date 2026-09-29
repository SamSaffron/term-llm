package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

// orderTestNodes are config nodes on a closed port (probes fail at once):
// three with their own serve tokens and one the Hub holds no token for.
func orderTestNodes(extra ...hub.Node) []hub.Node {
	node := func(id, token string) hub.Node {
		return hub.Node{ID: id, Name: strings.ToUpper(id[:1]) + id[1:], Source: hub.SourceConfig, URL: "http://127.0.0.1:1", BasePath: "/chat", Token: token}
	}
	return append([]hub.Node{
		node("alpha", "alpha-token"),
		node("beta", "beta-token"),
		node("gamma", "gamma-token"),
		node("silent", ""),
	}, extra...)
}

// newOrderHub returns a Hub over config nodes plus a local node store, saving
// its dashboard order at orderPath.
func newOrderHub(t *testing.T, orderPath string, nodes []hub.Node) *hubServer {
	t.Helper()
	store := hub.NewStore(filepath.Join(filepath.Dir(orderPath), "nodes.json"))
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: nodes}, store), store)
	s.nodeOrder = hub.NewNodeOrderStore(orderPath)
	return s
}

func newBearerOrderHub(t *testing.T, nodes ...hub.Node) *hubServer {
	t.Helper()
	s := newOrderHub(t, filepath.Join(t.TempDir(), "node-order.json"), orderTestNodes(nodes...))
	s.requireAuth = true
	s.authMode = "bearer"
	s.token = "hub-secret"
	return s
}

func orderRequest(path string, ids ...string) *http.Request {
	body, _ := json.Marshal(map[string]any{"node_ids": ids})
	req := httptest.NewRequest(http.MethodPatch, "http://backend"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func withBearer(req *http.Request, token string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func asNode(req *http.Request, id, token string) *http.Request {
	req.Header.Set(hubNodeIDHeader, id)
	if token != "" {
		withBearer(req, token)
	}
	return req
}

func serveHub(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func committedOrder(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		NodeIDs []string `json:"node_ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode reorder response: %v (%s)", err, rec.Body.String())
	}
	return body.NodeIDs
}

// listedOrder returns the node IDs GET /api/nodes lists, in order.
func listedOrder(t *testing.T, h http.Handler, path string, authorize func(*http.Request)) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://backend"+path, nil)
	if authorize != nil {
		authorize(req)
	}
	rec := serveHub(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d body=%s", path, rec.Code, rec.Body.String())
	}
	var body struct {
		Nodes []hubNodeView `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(body.Nodes))
	for _, node := range body.Nodes {
		ids = append(ids, node.ID)
	}
	return ids
}

func assertNoNodeTokens(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for _, token := range []string{"alpha-token", "beta-token", "gamma-token", "order-token"} {
		if strings.Contains(rec.Body.String(), token) {
			t.Fatalf("response leaked node token %q: %s", token, rec.Body.String())
		}
	}
}

func TestHubSidebarNodesNeverWaitForNodeProbes(t *testing.T) {
	s := newBearerOrderHub(t)
	s.basePath = "/hub"
	// A full dashboard listing would dereference the prober. The lightweight
	// listing must not use health, reverse transport, or live session clients.
	s.prober = nil
	s.nodeAPIClient = nil
	nodes, err := s.registry.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	s.healthCache.retain(nodes)
	wantStatuses := map[string]hub.Status{
		"alpha":  {State: "ok", Reachable: true},
		"beta":   {State: "unreachable"},
		"gamma":  {State: "unknown"},
		"silent": {State: "unknown"},
	}
	for _, n := range nodes {
		if n.ID == "alpha" || n.ID == "beta" {
			s.healthCache.publish(context.Background(), n, time.Now(), wantStatuses[n.ID])
		}
	}
	if _, err := s.nodeOrder.Reorder(orderTestNodes(), []string{"gamma", "beta", "alpha", "silent"}); err != nil {
		t.Fatal(err)
	}
	rec := serveHub(s.handler(), withBearer(httptest.NewRequest(http.MethodGet, "/hub/api/nodes?view=sidebar", nil), "hub-secret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("sidebar status = %d, body = %s", rec.Code, rec.Body.String())
	}
	assertNoNodeTokens(t, rec)
	var body struct {
		Nodes []hubNodeView `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, node := range body.Nodes {
		ids = append(ids, node.ID)
		want := wantStatuses[node.ID]
		if node.Status.State != want.State || node.Status.Reachable != want.Reachable {
			t.Fatalf("sidebar health for %s = %+v, want %+v", node.ID, node.Status, want)
		}
		if node.Sessions == nil || node.Sessions.ResumePath != "/hub/node/"+node.ID+"/" {
			t.Fatalf("sidebar resume path = %+v", node.Sessions)
		}
	}
	if !slices.Equal(ids, []string{"gamma", "beta", "alpha", "silent"}) {
		t.Fatalf("sidebar order = %v", ids)
	}
}

func TestHubNodeOrderOperatorSavesAnOrderThatSurvivesRestart(t *testing.T) {
	s := newBearerOrderHub(t)
	h := s.handler()
	bearer := func(req *http.Request) { withBearer(req, "hub-secret") }
	if got := listedOrder(t, h, "/api/nodes", bearer); !slices.Equal(got, []string{"alpha", "beta", "gamma", "silent"}) {
		t.Fatalf("initial order = %v, want the registry's name order", got)
	}

	rec := serveHub(h, withBearer(orderRequest("/api/nodes/order", "gamma", "silent", "alpha", "beta"), "hub-secret"))
	assertNoNodeTokens(t, rec)
	if got := committedOrder(t, rec); !slices.Equal(got, []string{"gamma", "silent", "alpha", "beta"}) {
		t.Fatalf("committed = %v", got)
	}
	if got := listedOrder(t, h, "/api/nodes", bearer); !slices.Equal(got, []string{"gamma", "silent", "alpha", "beta"}) {
		t.Fatalf("listing after reorder = %v", got)
	}

	// The browser dashboard authenticates with the Hub cookie instead.
	cookie := orderRequest("/api/nodes/order", "alpha", "gamma")
	cookie.AddCookie(&http.Cookie{Name: hubAuthCookieName, Value: "hub-secret"})
	if got := committedOrder(t, serveHub(h, cookie)); !slices.Equal(got, []string{"alpha", "silent", "gamma", "beta"}) {
		t.Fatalf("subset reorder committed = %v", got)
	}

	// A restarted or reloaded Hub reads the same order, and a node that
	// appears later appends after the arranged ones whatever its name.
	restarted := newOrderHub(t, s.nodeOrder.Path(), orderTestNodes(hub.Node{ID: "aardvark", Name: "Aardvark", Source: hub.SourceConfig, URL: "http://127.0.0.1:1", BasePath: "/chat"}))
	if got := listedOrder(t, restarted.handler(), "/api/nodes", nil); !slices.Equal(got, []string{"alpha", "silent", "gamma", "beta", "aardvark"}) {
		t.Fatalf("order after restart = %v", got)
	}
	data, err := os.ReadFile(s.nodeOrder.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"alpha-token", "hub-secret", "127.0.0.1"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("order file holds %q: %s", secret, data)
		}
	}
}

func TestHubNodeOrderBasePathAndRoutes(t *testing.T) {
	s := newBearerOrderHub(t)
	s.basePath = "/hub"
	h := s.handler()
	if got := committedOrder(t, serveHub(h, withBearer(orderRequest("/hub/api/nodes/order", "beta", "alpha"), "hub-secret"))); !slices.Equal(got, []string{"beta", "alpha", "gamma", "silent"}) {
		t.Fatalf("mounted reorder committed = %v", got)
	}
	if got := committedOrder(t, serveHub(h, asNode(orderRequest("/hub/api/nodes/order", "alpha", "beta"), "gamma", "gamma-token"))); !slices.Equal(got, []string{"alpha", "beta", "gamma", "silent"}) {
		t.Fatalf("mounted node reorder committed = %v", got)
	}
	if rec := serveHub(h, withBearer(orderRequest("/api/nodes/order", "alpha"), "hub-secret")); rec.Code != http.StatusNotFound {
		t.Fatalf("unmounted path status = %d, want 404", rec.Code)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
		req := withBearer(httptest.NewRequest(method, "http://backend/hub/api/nodes/order", strings.NewReader(`{}`)), "hub-secret")
		req.Header.Set("Content-Type", "application/json")
		rec := serveHub(h, req)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPatch {
			t.Fatalf("%s status = %d Allow=%q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

func TestHubNodeOrderKeepsRemovingALocalNodeNamedOrder(t *testing.T) {
	s := newBearerOrderHub(t)
	if _, err := s.store.Add(hub.Node{ID: "order", URL: "http://127.0.0.1:1/chat", Token: "order-token"}); err != nil {
		t.Fatal(err)
	}
	h := s.handler()
	// DELETE is not the order endpoint's method, so node credentials never
	// skip Hub authentication for it.
	rec := serveHub(h, asNode(httptest.NewRequest(http.MethodDelete, "http://backend/api/nodes/order", nil), "order", "order-token"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("node DELETE status = %d, want 401", rec.Code)
	}
	if _, ok := s.registry.Lookup("order"); !ok {
		t.Fatal("node credentials removed a node")
	}
	rec = serveHub(h, withBearer(httptest.NewRequest(http.MethodDelete, "http://backend/api/nodes/order", nil), "hub-secret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("operator DELETE status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := s.registry.Lookup("order"); ok {
		t.Fatal("a local node named order was not removed")
	}
}

type orderAuthMode struct {
	name string
	hub  func(t *testing.T) *hubServer
	// operator adds valid Hub operator credentials to a request.
	operator func(t *testing.T, s *hubServer, req *http.Request)
}

func orderAuthModes() []orderAuthMode {
	return []orderAuthMode{
		{
			name: "bearer",
			hub:  func(t *testing.T) *hubServer { return newBearerOrderHub(t) },
			operator: func(_ *testing.T, _ *hubServer, req *http.Request) {
				req.AddCookie(&http.Cookie{Name: hubAuthCookieName, Value: "hub-secret"})
			},
		},
		{
			name: "passkey",
			hub: func(t *testing.T) *hubServer {
				return configureTestPasskeyHub(t, newOrderHub(t, filepath.Join(t.TempDir(), "node-order.json"), orderTestNodes()), "")
			},
			operator: func(t *testing.T, s *hubServer, req *http.Request) {
				issued, err := s.passkey.sessions.Create("credential")
				if err != nil {
					t.Fatal(err)
				}
				req.AddCookie(&http.Cookie{Name: hubSessionCookieName, Value: issued.Token})
				req.Header.Set("Origin", "http://localhost:8090")
			},
		},
	}
}

func TestHubNodeOrderNodeAuthentication(t *testing.T) {
	for _, mode := range orderAuthModes() {
		t.Run(mode.name, func(t *testing.T) {
			s := mode.hub(t)
			h := s.handler()
			cases := []struct {
				name     string
				nodeID   string
				token    string
				operator bool
				want     int
			}{
				{"own token", "alpha", "alpha-token", false, http.StatusOK},
				{"wrong token", "alpha", "wrong-token", false, http.StatusUnauthorized},
				{"another node's token", "alpha", "beta-token", false, http.StatusUnauthorized},
				{"unknown node", "ghost", "alpha-token", false, http.StatusUnauthorized},
				{"node without a token", "silent", "anything", false, http.StatusUnauthorized},
				{"no bearer token", "alpha", "", false, http.StatusUnauthorized},
				{"operator credentials never stand in for a node", "alpha", "wrong-token", true, http.StatusUnauthorized},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					req := asNode(orderRequest("/api/nodes/order", "gamma", "beta", "alpha", "silent"), tc.nodeID, tc.token)
					if tc.operator {
						mode.operator(t, s, req)
					}
					rec := serveHub(h, req)
					assertNoNodeTokens(t, rec)
					if rec.Code != tc.want {
						t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
					}
					if tc.want == http.StatusUnauthorized && !strings.Contains(rec.Body.String(), "node authentication failed") {
						t.Fatalf("failure body = %s, want the uniform node failure", rec.Body.String())
					}
				})
			}
			operator := orderRequest("/api/nodes/order", "alpha", "beta", "gamma", "silent")
			mode.operator(t, s, operator)
			if got := committedOrder(t, serveHub(h, operator)); !slices.Equal(got, []string{"alpha", "beta", "gamma", "silent"}) {
				t.Fatalf("operator committed = %v", got)
			}
		})
	}
}

func TestHubNodeCredentialsReachOnlyTheOrderEndpoint(t *testing.T) {
	jsonBody := func(method, path, body string) *http.Request {
		req := httptest.NewRequest(method, "http://backend"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		return req
	}
	for _, mode := range orderAuthModes() {
		t.Run(mode.name, func(t *testing.T) {
			s := mode.hub(t)
			h := s.handler()
			requests := map[string]*http.Request{
				"list nodes":        httptest.NewRequest(http.MethodGet, "http://backend/api/nodes", nil),
				"add node":          jsonBody(http.MethodPost, "/api/nodes", `{"name":"evil","url":"http://127.0.0.1:2/chat"}`),
				"remove node":       httptest.NewRequest(http.MethodDelete, "http://backend/api/nodes/beta", nil),
				"test node":         jsonBody(http.MethodPost, "/api/nodes/test", `{"url":"http://127.0.0.1:2/chat"}`),
				"registration info": httptest.NewRequest(http.MethodGet, "http://backend/api/registration-info", nil),
				"attention":         httptest.NewRequest(http.MethodGet, "http://backend/api/attention", nil),
				"node proxy":        httptest.NewRequest(http.MethodGet, "http://backend/node/beta/v1/models", nil),
				"order as operator": jsonBody(http.MethodPatch, "/api/nodes/order", `{"node_ids":["beta","alpha"]}`),
			}
			for name, req := range requests {
				t.Run(name, func(t *testing.T) {
					if name == "order as operator" {
						// The node token alone is not a Hub credential.
						withBearer(req, "alpha-token")
					} else {
						asNode(req, "alpha", "alpha-token")
					}
					rec := serveHub(h, req)
					if rec.Code != http.StatusUnauthorized {
						t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
					}
					assertNoNodeTokens(t, rec)
				})
			}
			if _, ok := s.registry.Lookup("beta"); !ok {
				t.Fatal("node credentials changed the registry")
			}
			rec := serveHub(h, asNode(orderRequest("/api/nodes/order", "beta", "alpha"), "alpha", "alpha-token"))
			if got := committedOrder(t, rec); !slices.Equal(got, []string{"beta", "alpha", "gamma", "silent"}) {
				t.Fatalf("node reorder committed = %v", got)
			}
		})
	}
}

func TestHubNodeOrderRejectsUnauthenticatedAndCrossSiteRequests(t *testing.T) {
	crossSite := func(req *http.Request) *http.Request {
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		return req
	}
	for _, mode := range orderAuthModes() {
		t.Run(mode.name, func(t *testing.T) {
			s := mode.hub(t)
			h := s.handler()
			if rec := serveHub(h, orderRequest("/api/nodes/order", "beta", "alpha")); rec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
			}
			operator := orderRequest("/api/nodes/order", "beta", "alpha")
			mode.operator(t, s, operator)
			if rec := serveHub(h, crossSite(operator)); rec.Code != http.StatusForbidden {
				t.Fatalf("cross-site operator status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
			}
			node := asNode(orderRequest("/api/nodes/order", "beta", "alpha"), "alpha", "alpha-token")
			if rec := serveHub(h, crossSite(node)); rec.Code != http.StatusForbidden {
				t.Fatalf("cross-site node status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
			}
			if got := listedOrder(t, h, "/api/nodes", func(req *http.Request) { mode.operator(t, s, req) }); !slices.Equal(got, []string{"alpha", "beta", "gamma", "silent"}) {
				t.Fatalf("rejected requests changed the order: %v", got)
			}
		})
	}

	t.Run("auth none", func(t *testing.T) {
		s := newOrderHub(t, filepath.Join(t.TempDir(), "node-order.json"), orderTestNodes())
		h := s.handler()
		if got := committedOrder(t, serveHub(h, orderRequest("/api/nodes/order", "beta", "alpha"))); !slices.Equal(got, []string{"beta", "alpha", "gamma", "silent"}) {
			t.Fatalf("loopback operator committed = %v", got)
		}
		if rec := serveHub(h, asNode(orderRequest("/api/nodes/order", "alpha", "beta"), "alpha", "beta-token")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("forged node status = %d, want 401", rec.Code)
		}
		if rec := serveHub(h, crossSite(orderRequest("/api/nodes/order", "alpha", "beta"))); rec.Code != http.StatusForbidden {
			t.Fatalf("cross-site status = %d, want 403", rec.Code)
		}
	})
}

func TestHubNodeOrderValidatesRequests(t *testing.T) {
	s := newOrderHub(t, filepath.Join(t.TempDir(), "node-order.json"), orderTestNodes())
	h := s.handler()
	raw := func(contentType, body string) *http.Request {
		req := httptest.NewRequest(http.MethodPatch, "http://backend/api/nodes/order", strings.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		return req
	}
	cases := []struct {
		name string
		req  *http.Request
		want int
		kind string
	}{
		{"text body", raw("text/plain", `{"node_ids":["alpha"]}`), http.StatusUnsupportedMediaType, "invalid_content_type"},
		{"no content type", raw("", `{"node_ids":["alpha"]}`), http.StatusUnsupportedMediaType, "invalid_content_type"},
		{"unknown field", raw("application/json", `{"node_ids":["alpha"],"extra":1}`), http.StatusBadRequest, "invalid_node_order"},
		{"trailing data", raw("application/json", `{"node_ids":["alpha"]}{}`), http.StatusBadRequest, "invalid_node_order"},
		{"not an object", raw("application/json", `["alpha"]`), http.StatusBadRequest, "invalid_node_order"},
		{"empty", orderRequest("/api/nodes/order"), http.StatusBadRequest, "invalid_node_order"},
		{"duplicate", orderRequest("/api/nodes/order", "alpha", "alpha"), http.StatusBadRequest, "invalid_node_order"},
		{"invalid id", orderRequest("/api/nodes/order", "alpha", "../beta"), http.StatusBadRequest, "invalid_node_order"},
		{"unknown node", orderRequest("/api/nodes/order", "alpha", "ghost"), http.StatusNotFound, "node_not_found"},
		{"too large", raw("application/json", `{"node_ids":["`+strings.Repeat("a", hubNodeOrderBodyLimit)+`"]}`), http.StatusRequestEntityTooLarge, "request_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveHub(h, tc.req)
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), `"type":"`+tc.kind+`"`) {
				t.Fatalf("status = %d body=%s, want %d %s", rec.Code, rec.Body.String(), tc.want, tc.kind)
			}
		})
	}
	if _, err := os.Stat(s.nodeOrder.Path()); !os.IsNotExist(err) {
		t.Fatalf("rejected requests wrote the order file (stat err %v)", err)
	}

	s.nodeOrder = nil
	if rec := serveHub(s.handler(), orderRequest("/api/nodes/order", "alpha")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("without an order store status = %d, want 503", rec.Code)
	}
}

func TestHubNodesListingLogsAnUnreadableOrderOnce(t *testing.T) {
	var logs strings.Builder
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	path := filepath.Join(t.TempDir(), "node-order.json")
	if err := os.WriteFile(path, []byte(`{"version":1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newOrderHub(t, path, orderTestNodes())
	h := s.handler()
	for i := 0; i < 3; i++ {
		if got := listedOrder(t, h, "/api/nodes", nil); !slices.Equal(got, []string{"alpha", "beta", "gamma", "silent"}) {
			t.Fatalf("fallback order = %v", got)
		}
	}
	if count := strings.Count(logs.String(), "dashboard node order"); count != 1 {
		t.Fatalf("logged the unreadable order %d times, want once:\n%s", count, logs.String())
	}
	// Saving an order replaces the unreadable file; later listings use it.
	if got := committedOrder(t, serveHub(h, orderRequest("/api/nodes/order", "silent", "alpha"))); !slices.Equal(got, []string{"silent", "beta", "gamma", "alpha"}) {
		t.Fatalf("committed = %v", got)
	}
	if got := listedOrder(t, h, "/api/nodes", nil); !slices.Equal(got, []string{"silent", "beta", "gamma", "alpha"}) {
		t.Fatalf("listing after repair = %v", got)
	}
}
