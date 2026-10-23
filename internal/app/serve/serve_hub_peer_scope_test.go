package serve

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/hub"
)

func TestPeerChatPositiveAllowlist(t *testing.T) {
	allowed := [][2]string{{"GET", "/"}, {"GET", "/chat/12"}, {"GET", "/dist/main-123.js"}, {"GET", "/v1/providers"}, {"GET", "/v1/sidebar"}, {"GET", "/v1/sessions"}, {"GET", "/v1/sessions/session-1/state"}, {"GET", "/v1/events"}, {"POST", "/v1/sessions"}, {"POST", "/v1/responses"}, {"GET", "/v1/responses/resp_1/events"}, {"POST", "/v1/responses/resp_1/cancel"}, {"POST", "/v1/sessions/session_1/approval"}, {"POST", "/v1/sessions/session_1/ask_user"}}
	for _, v := range allowed {
		if !peerChatRoute(v[0], v[1]) {
			t.Errorf("chat denied %v", v)
		}
	}
	denied := [][2]string{{"POST", "/api/nodes/test"}, {"POST", "/api/nodes"}, {"DELETE", "/api/nodes/escape"}, {"GET", "/api/nodes"}, {"GET", "/node/escape/"}, {"POST", "/v1/mcp/servers"}, {"DELETE", "/v1/mcp/servers/remote"}, {"POST", "/admin/widgets/mount/stop"}, {"POST", "/admin/extensions/config"}, {"PATCH", "/v1/sessions/id"}, {"POST", "/v1/sessions/id/mcp/remote/oauth/start"}, {"POST", "/v1/sessions/id/runtime/approvals"}, {"GET", "/api/registration-info"}, {"GET", "/api/auth/credentials"}, {"POST", "/v1/projects"}, {"GET", "/v1/live/sessions"}, {"POST", "/v2/jobs"}}
	for _, v := range denied {
		if peerChatRoute(v[0], v[1]) {
			t.Errorf("authority escaped %v", v)
		}
	}
}

// Exercise a REAL nested Hub and real server-injected bearer, not just a path
// predicate. A valid chat landing request proves the injected token reaches
// the inner authenticated server; management and recursive proxy requests must
// never reach it, and must not add an arbitrary target or trigger its probe.
func TestHubPeerCannotEscalateThroughNestedHub(t *testing.T) {
	var probes atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { probes.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	store := hub.NewStore(filepath.Join(t.TempDir(), "inner-nodes.json"))
	inner := newHubServer(hub.NewRegistry(store), store)
	inner.requireAuth = true
	inner.token = "private-nested-hub-token"
	var authenticated atomic.Int32
	innerHandler := inner.handler()
	nested := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+inner.token {
			authenticated.Add(1)
		}
		innerHandler.ServeHTTP(w, r)
	}))
	defer nested.Close()
	outer := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{{ID: "nested", Name: "Nested", URL: nested.URL, Token: inner.token}}}), nil)
	h := outer.peerHandler(func(*http.Request) bool { return true }, func(string) bool { return true })
	landing := httptest.NewRecorder()
	h.ServeHTTP(landing, httptest.NewRequest("GET", "/node/nested/", nil))
	if landing.Code != 200 || authenticated.Load() != 1 {
		t.Fatalf("nested injected-token control failed: %d count%d", landing.Code, authenticated.Load())
	}
	payload := `{"id":"escape","name":"Escape","url":"` + target.URL + `/chat","token":"test-probe-token"}`
	for _, route := range [][2]string{{"POST", "/api/nodes/test"}, {"POST", "/api/nodes"}, {"DELETE", "/api/nodes/escape"}, {"GET", "/node/escape/"}, {"POST", "/v1/mcp/servers"}, {"POST", "/admin/widgets/demo/stop"}, {"POST", "/admin/extensions/config"}} {
		req := httptest.NewRequest(route[0], "/node/nested"+route[1], strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 || rec.Header().Get("X-Term-LLM-Peer-Error") != "scope_denied" {
			t.Errorf("%v got %d", route, rec.Code)
		}
	}
	if authenticated.Load() != 1 || probes.Load() != 0 {
		t.Fatalf("unlisted request reached nested Hub or target: nested=%d target=%d", authenticated.Load(), probes.Load())
	}
	nodes, err := store.Nodes()
	if err != nil || len(nodes) != 0 {
		t.Fatalf("nested registry mutated: %+v %v", nodes, err)
	}
}
