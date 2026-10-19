package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
	"github.com/samsaffron/term-llm/internal/hub"
)

// TestServeRoutesGolden snapshots the actual ServeMux registrations, including
// their ordered per-route wrappers. The complete Web and Hub outer chains
// are recorded separately because they wrap a mux, not individual routes.
// This is deliberately a source-level invariant: net/http has no API to
// enumerate a ServeMux's registered patterns or its middleware chain.
func TestServeRoutesGolden(t *testing.T) {
	sources := map[string]string{
		"web":        "serve_http.go",
		"hub":        "serve_hub_auth.go",
		"passkey":    "serve_hub_passkey.go",
		"widgets":    "serve_widgets.go",
		"extensions": "serve_extensions.go",
	}
	var routes, outer []string
	labels := hubAuthMatrix(t)
	for group, source := range sources {
		f, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if group == "hub" {
			outer = append(outer, "[outer hub]\n"+outerRouteChain(t, f, "hubServer", "handler"))
		}
		if group == "web" {
			outer = append(outer, "[outer web]\n"+outerRouteChain(t, f, "serveServer", "httpHandler"))
		}
		ast.Inspect(f, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			var pattern string
			switch p := call.Args[0].(type) {
			case *ast.BasicLit:
				pattern, err = strconv.Unquote(p.Value)
				if err != nil {
					t.Fatal(err)
				}
			case *ast.Ident:
				if p.Name != "hubNodeOrderPath" {
					return true
				}
				pattern = apphub.NodeOrderPath
			default:
				return true // outer base-path mounting, not an inner route
			}
			if !strings.HasPrefix(pattern, "/") && !strings.Contains(pattern, " /") {
				return true
			}
			method := "*"
			if parts := strings.SplitN(pattern, " ", 2); len(parts) == 2 {
				method, pattern = parts[0], parts[1]
			}
			auth := "public"
			if group == "hub" || group == "passkey" {
				auth = labels[group+" "+pattern]
				if auth == "" {
					t.Fatalf("missing auth representative for %s %s", group, pattern)
				}
			} else if strings.HasPrefix(routeWrapperChain(call.Args[1]), "auth(") {
				auth = "serve-auth"
			}
			routes = append(routes, group+" "+method+" "+pattern+" "+auth+" wrappers="+routeWrapperChain(call.Args[1]))
			return true
		})
	}
	if len(routes) != 104 {
		t.Fatalf("route count = %d, want 104", len(routes))
	}
	sort.Strings(routes)
	sort.Strings(outer)
	checkServeGolden(t, "routes.golden", strings.Join(routes, "\n")+"\n\n"+strings.Join(outer, "\n"))
}

func checkServeGolden(t *testing.T, name, content string) {
	t.Helper()
	got := []byte(content)
	golden := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_SERVE_ROUTES_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("serve HTTP contract changed; regenerate only after reviewing %s\nwant:\n%s\ngot:\n%s", golden, want, got)
	}
}

// Only applications around the handler are wrappers. Do not search inside
// closures, where an unrelated auth call could otherwise produce a false pass.
func routeWrapperChain(expr ast.Expr) string {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return "h"
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) != 1 {
		return "unknown-wrapper"
	}
	return sel.Sel.Name + "(" + routeWrapperChain(call.Args[0]) + ")"
}

// Snapshot actual outer statements, including branch conditions and data flow,
// rather than looking for an auth call anywhere in the file. This pins both
// reloadTransport returns, the jobs-only root mount, the conditional passkey
// wrapper, StripPrefix ordering, and the UI-only root redirect. Formatting the
// AST keeps this independent of source whitespace and comments. No handler
// body is included: these builders' first return is their outer-chain branch.
func outerRouteChain(t *testing.T, file *ast.File, receiver, name string) string {
	t.Helper()
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Recv == nil {
			continue
		}
		recv, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := recv.X.(*ast.Ident)
		if !ok || ident.Name != receiver {
			continue
		}
		var output bytes.Buffer
		started := false
		for _, statement := range fn.Body.List {
			ast.Inspect(statement, func(node ast.Node) bool {
				if _, ok := node.(*ast.ReturnStmt); ok {
					started = true
				}
				return true
			})
			if !started {
				continue
			}
			if err := format.Node(&output, token.NewFileSet(), statement); err != nil {
				t.Fatal(err)
			}
			output.WriteByte('\n')
		}
		if !started {
			t.Fatalf("%s.%s has no outer return", receiver, name)
		}
		return output.String()
	}
	t.Fatalf("missing %s.%s", receiver, name)
	return ""
}

// Representatives use a handler-supported verb and a concrete suffix rather
// than probing every route with GET (which can hide auth behind a 404/405).
// Bodies are deliberately minimal: passing auth need not make the operation
// succeed. The gate sentinel and the real handler response are separate facts.
type hubAuthRequest struct {
	group, pattern, method, path, body string
}

func hubAuthRepresentatives() []hubAuthRequest {
	hubRequests := map[string]string{
		"/": "GET /", "/healthz": "GET /healthz",
		"/dist/hub.js": "GET /dist/hub.js", "/dist/hub.css": "GET /dist/hub.css",
		"/api/nodes/test": "POST /api/nodes/test", "/api/registration-info": "GET /api/registration-info",
		"/api/register-node": "POST /api/register-node", "/api/register-node/": "DELETE /api/register-node/missing",
		apphub.NodeOrderPath: "PATCH " + apphub.NodeOrderPath,
		"/api/nodes/":        "DELETE /api/nodes/missing", "/api/nodes": "GET /api/nodes?view=sidebar",
		"/api/attention": "GET /api/attention", "/api/attention/clear": "POST /api/attention/clear",
		"/api/attention/seen": "POST /api/attention/seen",
		"/api/delegations/":   "GET /api/delegations/missing", "/api/delegations": "GET /api/delegations",
		"/api/connect": "GET /api/connect", "/node/": "GET /node/missing/",
	}
	var requests []hubAuthRequest
	for pattern, target := range hubRequests {
		parts := strings.SplitN(target, " ", 2)
		body := "{}"
		if pattern == apphub.NodeOrderPath {
			body = `{"node_ids":["n1"]}`
		}
		requests = append(requests, hubAuthRequest{"hub", pattern, parts[0], parts[1], body})
	}
	// The shared passkey routes exist only on the configured passkey fixture.
	for _, path := range []string{
		"/auth/login", "/auth/setup", "/auth/recover", "/auth/native/",
		"/api/auth/native/authorize", "/api/auth/native/redeem",
		"/api/auth/bootstrap/verify", "/api/auth/bootstrap/register/begin", "/api/auth/bootstrap/register/finish",
		"/api/auth/recovery/verify", "/api/auth/recovery/register/begin", "/api/auth/recovery/register/finish",
		"/api/auth/login/begin", "/api/auth/login/finish", "/api/auth/reauth/begin", "/api/auth/reauth/finish",
		"/api/auth/session", "/api/auth/logout", "/api/auth/sessions/revoke-others",
		"/api/auth/credentials/register/begin", "/api/auth/credentials/register/finish",
		"/api/auth/credentials/", "/api/auth/credentials",
	} {
		method, target := http.MethodPost, path
		if strings.HasPrefix(path, "/auth/") || path == "/api/auth/session" || path == "/api/auth/credentials" {
			method = http.MethodGet
		}
		if path == "/auth/native/" {
			target += strings.Repeat("A", 43) // a valid base64url challenge, not a routing error
		}
		if path == "/api/auth/credentials/" {
			method, target = http.MethodDelete, path+"missing"
		}
		requests = append(requests, hubAuthRequest{"passkey", path, method, target, "{}"})
	}
	sort.Slice(requests, func(i, j int) bool {
		return requests[i].group+requests[i].pattern < requests[j].group+requests[j].pattern
	})
	return requests
}

type hubAuthVariant struct {
	name, token, nodeID                string
	options, session, cookie, navigate bool
}

func hubAuthMatrix(t *testing.T) map[string]string {
	t.Helper()
	labels := make(map[string]string)
	var golden strings.Builder
	golden.WriteString("# gate=true means only that the outer middleware reached a sentinel, NOT handler authentication.\n# body records error text, JSON top-level keys, shell page, or asset content type; random session/ceremony data is omitted.\n")
	variants := []hubAuthVariant{
		{name: "anon"}, {name: "hub-bearer", token: "hub-secret"},
		{name: "node-missing", nodeID: "n1"}, {name: "node-bad", nodeID: "n1", token: "wrong"},
		{name: "node-good", nodeID: "n1", token: "node-secret"},
		{name: "node-unknown", nodeID: "unknown", token: "node-secret"},
		{name: "registration", token: "reg-secret"}, {name: "OPTIONS", options: true},
	}
	requests := hubAuthRepresentatives()
	// Additional method-specific paths pin PATCH-only node-order bypass and
	// delegation mutation auth, not just delegation read auth.
	extra := []hubAuthRequest{
		{"extra", "order-get", "GET", apphub.NodeOrderPath, "{}"},
		{"extra", "order-delete", "DELETE", apphub.NodeOrderPath, "{}"},
		{"extra", "delegation-create", "POST", "/api/delegations", "{}"},
		{"extra", "delegation-cancel", "POST", "/api/delegations/missing/cancel", "{}"},
	}
	for _, mode := range []string{"bearer", "passkey"} {
		columns := append([]hubAuthVariant(nil), variants...)
		if mode == "passkey" {
			columns = append(columns, hubAuthVariant{name: "session", session: true})
		}
		for _, request := range append(append([]hubAuthRequest(nil), requests...), extra...) {
			if mode == "bearer" && request.group == "passkey" {
				continue
			}
			fmt.Fprintf(&golden, "%s %s %s [%s %s]\n", mode, request.method, request.path, request.group, request.pattern)
			var admitted []string
			for _, variant := range columns {
				gate, result := probeHubAuth(t, mode, request, variant, false)
				fmt.Fprintf(&golden, "  %s gate=%t %s\n", variant.name, gate, result)
				if gate {
					admitted = append(admitted, variant.name)
				}
			}
			if (mode == "bearer" && request.group == "hub") || request.group == "passkey" {
				labels[request.group+" "+request.pattern] = "outer-admits=" + strings.Join(admitted, ",")
			}
		}
		// Disabled registration is a 404 after a valid method, not an auth
		// denial. OPTIONS still reaches the handler and gets its method error.
		for _, request := range requests {
			if !strings.HasPrefix(request.pattern, "/api/register-node") {
				continue
			}
			fmt.Fprintf(&golden, "%s registration-disabled %s %s\n", mode, request.method, request.path)
			for _, variant := range columns {
				gate, result := probeHubAuth(t, mode, request, variant, true)
				fmt.Fprintf(&golden, "  %s gate=%t %s\n", variant.name, gate, result)
			}
		}
		// Browser navigation, query-token cleanup and bearer-cookie behavior
		// differ from API denials. Keep the base mount in Location/cookie paths.
		for _, variant := range []hubAuthVariant{
			{name: "navigation", navigate: true}, {name: "query-token"},
			{name: "bearer-cookie", token: "hub-secret", cookie: true},
		} {
			request := hubAuthRequest{"extra", "browser", "GET", "/", "{}"}
			if variant.name == "query-token" {
				request.path = "/?token=hub-secret&keep=1"
			}
			gate, result := probeHubAuth(t, mode, request, variant, false)
			fmt.Fprintf(&golden, "%s browser %s gate=%t %s\n", mode, variant.name, gate, result)
		}
	}
	checkServeGolden(t, "hub_auth.golden", golden.String())
	return labels
}

func probeHubAuth(t *testing.T, mode string, request hubAuthRequest, variant hubAuthVariant, disabled bool) (gate bool, result string) {
	t.Helper()
	t.Run(mode+"/"+request.pattern+"/"+request.method+"/"+variant.name+fmt.Sprint(disabled), func(t *testing.T) {
		dir := t.TempDir()
		store := hub.NewStore(filepath.Join(dir, "nodes.json"))
		// Reverse nodes never initiate network probes. Missing proxy and
		// delegation IDs likewise exercise routing without outbound requests.
		s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{{ID: "n1", Name: "Node", Token: "node-secret", Connection: "reverse"}}}, store), store)
		s.requireAuth, s.authMode, s.token, s.registrationToken = true, "bearer", "hub-secret", "reg-secret"
		s.basePath = "/hub"
		s.nodeOrder = hub.NewNodeOrderStore(filepath.Join(dir, "order.json"))
		s.delegations = hub.NewDelegationStore(filepath.Join(dir, "delegations.json"))
		if disabled {
			s.registrationToken = ""
		}
		var session *http.Cookie
		if mode == "passkey" {
			configureTestPasskeyHub(t, s, "/hub")
			issued, err := s.passkey.sessions.Create("credential")
			if err != nil {
				t.Fatal(err)
			}
			session = &http.Cookie{Name: hubSessionCookieName, Value: issued.Token}
		}
		newRequest := func(mounted bool) *http.Request {
			method, path := request.method, request.path
			if variant.options {
				method = http.MethodOptions
			}
			if mounted {
				path = "/hub" + path
			}
			r := httptest.NewRequest(method, "http://localhost:8090"+path, strings.NewReader(request.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", "http://localhost:8090")
			if variant.cookie {
				r.AddCookie(&http.Cookie{Name: hubAuthCookieName, Value: variant.token})
			} else if variant.token != "" {
				r.Header.Set("Authorization", "Bearer "+variant.token)
			}
			if variant.nodeID != "" {
				r.Header.Set(hubNodeIDHeader, variant.nodeID)
			}
			if variant.session {
				r.AddCookie(session)
			}
			if variant.navigate {
				r.Header.Set("Accept", "text/html")
			}
			return r
		}
		s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gate = true; w.WriteHeader(http.StatusNoContent) })).ServeHTTP(httptest.NewRecorder(), newRequest(false))
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, newRequest(true))
		result = hubAuthResponse(t, w)
	})
	return gate, result
}

func hubAuthResponse(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	body := strings.TrimSpace(w.Body.String())
	var object map[string]json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &object) == nil && object != nil {
		if raw, ok := object["error"]; ok {
			var failure struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(raw, &failure); err != nil {
				t.Fatal(err)
			}
			body = failure.Message
		} else {
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			body = "json:" + strings.Join(keys, ",")
		}
	} else if strings.Contains(w.Header().Get("Content-Type"), "text/html") && strings.Contains(strings.ToLower(body), "<!doctype html>") {
		body = html.UnescapeString(body)
		// The shell contains dynamic cache scopes; only its page is an auth sentinel.
		for _, page := range []string{"dashboard", "bearer-login", "passkey-auth", "native-handoff"} {
			if strings.Contains(body, `"page":"`+page+`"`) {
				body = "shell:" + page
				break
			}
		}
		if strings.Contains(strings.ToLower(body), "<!doctype html>") {
			t.Fatal("unrecognized Hub shell")
		}
	} else if w.Code == http.StatusOK && (strings.Contains(w.Header().Get("Content-Type"), "javascript") || strings.Contains(w.Header().Get("Content-Type"), "text/css")) {
		body = "asset:" + w.Header().Get("Content-Type")
	}
	result := fmt.Sprintf("status=%d body=%q", w.Code, body)
	for _, header := range []string{"Allow", "Location", "X-Term-LLM-Login-URL"} {
		if value := w.Header().Get(header); value != "" {
			result += fmt.Sprintf(" %s=%q", header, value)
		}
	}
	// Expires and randomly issued cookie values are intentionally excluded.
	for _, cookie := range w.Result().Cookies() {
		value := "set"
		if cookie.MaxAge < 0 {
			value = "clear"
		}
		result += fmt.Sprintf(" cookie=%s:%s:path=%s:httpOnly=%t:secure=%t:sameSite=%d", cookie.Name, value, cookie.Path, cookie.HttpOnly, cookie.Secure, cookie.SameSite)
	}
	return result
}
