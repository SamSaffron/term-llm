package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/spf13/cobra"
)

// hubServer is the `term-llm serve hub` control plane: one pane of glass over
// heterogeneous term-llm web nodes. Nodes come from pluggable resolvers
// (static config file, local contain workspaces, a local JSON store fed by
// the dashboard's Add Node form); the hub dashboard lists them with live
// health, and /node/<id>/* reverse-proxies each node's full web UI with the
// node's bearer token injected server-side, so node tokens never reach the
// browser.
//
// Routing is path-based (/node/<id>/...) in v1. The proxy target is resolved
// per request from the node ID prefix, so a host-based router can later map
// Host -> node and reuse the same proxy plumbing unchanged.
//
// Hub-level authentication is intentionally small: one optional bearer token
// gates the operator dashboard, registry API, and node proxy. Reverse node
// websocket connections and delegation calls from nodes continue to use node
// auth (node id + that node's serve token), so Hub auth does not require user
// accounts or per-member ACLs.
type hubServer struct {
	registry *hub.Registry
	// store backs the dashboard's Add Node form; nil disables mutation.
	store  *hub.Store
	prober *hub.Prober
	// reverse tracks private nodes that dial out to the hub and receive node
	// requests over a websocket instead of direct hub -> node HTTP.
	reverse *hubReverseManager
	proxy   *httputil.ReverseProxy
	// delegations is the cross-node delegation ledger; nil disables the
	// /api/delegations endpoints.
	delegations *hub.DelegationStore
	// nodeAPIClient performs hub -> node jobs API calls for delegations. It
	// shares the proxy's direct-dial transport (no env proxy: requests carry
	// node tokens) but, unlike streaming proxy traffic, gets a whole-request
	// timeout.
	nodeAPIClient *http.Client

	requireAuth bool
	token       string
}

func newHubServer(registry *hub.Registry, store *hub.Store) *hubServer {
	transport := newHubTransport()
	s := &hubServer{
		registry:      registry,
		store:         store,
		prober:        hub.NewProber(transport),
		reverse:       newHubReverseManager(),
		nodeAPIClient: &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        hubRewriteProxyRequest,
		ModifyResponse: hubRebaseProxyResponse,
		ErrorHandler:   hubProxyErrorHandler,
		Transport:      transport,
	}
	return s
}

// newHubTransport returns the proxy's HTTP transport with bounded connection
// and response-header timeouts so a hung node cannot tie up the hub. It
// deliberately sets NO whole-response deadline: long-lived streams (SSE,
// WebRTC signalling) must stay open, so only connection establishment and
// response *headers* are bounded.
func newHubTransport() *http.Transport {
	return &http.Transport{
		// No environment proxy: the hub dials known node hosts directly, and
		// routing a token-injected request through an HTTP_PROXY would leak
		// the per-node bearer token to that proxy.
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
}

// hubProxyTarget carries the per-request reverse-proxy target. It is stashed
// on the inbound request context by handleNodeProxy and read back in the
// proxy's Rewrite, ModifyResponse, and ErrorHandler hooks.
type hubProxyTarget struct {
	nodeID   string // node ID (for diagnostics and hub context)
	nodeName string // node display name (for hub context)
	scheme   string // backend scheme (http or https)
	host     string // backend host:port
	path     string // backend path: node base path + remainder
	token    string // per-node bearer token, injected server-side
	basePath string // node's baked-in prefix, e.g. /chat ("" when root)
	mount    string // hub-facing prefix, e.g. /node/<id>
}

type hubProxyTargetKey struct{}

func withHubProxyTarget(ctx context.Context, t *hubProxyTarget) context.Context {
	return context.WithValue(ctx, hubProxyTargetKey{}, t)
}

func hubProxyTargetFrom(ctx context.Context) *hubProxyTarget {
	t, _ := ctx.Value(hubProxyTargetKey{}).(*hubProxyTarget)
	return t
}

type hubNodeDiagnostic struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// hubNodeView is the public record for one node. It deliberately omits the
// bearer token: tokens are injected server-side and must never be sent to a
// hub client.
type hubNodeView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Source     string `json:"source"`
	Connection string `json:"connection"`
	URL        string `json:"url"`
	BasePath   string `json:"base_path"`
	ProxyPath  string `json:"proxy_path"`
	// HasToken reports whether the hub holds a bearer token for this node
	// (without it, a token-guarded node will answer 401 through the proxy).
	HasToken    bool                `json:"has_token"`
	Status      hub.Status          `json:"status"`
	Diagnostics []hubNodeDiagnostic `json:"diagnostics,omitempty"`
}

func (s *hubServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHubHealth)
	mux.HandleFunc("/api/nodes/test", s.handleTestNode)
	mux.HandleFunc("/api/nodes/", s.handleNodeItem)
	mux.HandleFunc("/api/nodes", s.handleNodes)
	mux.HandleFunc("/api/delegations/", s.handleDelegationItem)
	mux.HandleFunc("/api/delegations", s.handleDelegations)
	mux.HandleFunc("/api/connect", s.handleReverseConnect)
	mux.HandleFunc("/node/", s.handleNodeProxy)
	mux.HandleFunc("/", s.handleIndex)
	return s.auth(mux)
}

func (s *hubServer) auth(next http.Handler) http.Handler {
	if !s.requireAuth {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || hubNodeAuthRoute(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !hubBearerTokenMatches(r, s.token) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid hub authentication credentials")
			return
		}
		if hubDelegationOperatorRoute(r) {
			clone := r.Clone(r.Context())
			clone.Header = r.Header.Clone()
			clone.Header.Del("Authorization")
			r = clone
		}
		next.ServeHTTP(w, r)
	})
}

func hubNodeAuthRoute(r *http.Request) bool {
	if r.URL.Path == "/api/connect" {
		return true
	}
	return (r.URL.Path == "/api/delegations" || strings.HasPrefix(r.URL.Path, "/api/delegations/")) && strings.TrimSpace(r.Header.Get(hubNodeIDHeader)) != ""
}

func hubDelegationOperatorRoute(r *http.Request) bool {
	return (r.URL.Path == "/api/delegations" || strings.HasPrefix(r.URL.Path, "/api/delegations/")) && strings.TrimSpace(r.Header.Get(hubNodeIDHeader)) == ""
}

func hubBearerTokenMatches(r *http.Request, want string) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, rest, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got := strings.TrimSpace(rest)
	if got == "" || strings.TrimSpace(want) == "" {
		return false
	}
	wantHash := sha256.Sum256([]byte(strings.TrimSpace(want)))
	gotHash := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(wantHash[:], gotHash[:]) == 1
}

// hubBrowserRequestAllowed rejects cross-site browser requests before the hub
// exercises any token-injecting authority or mutates its node registry. This is
// defense-in-depth for --auth none and for bearer-authenticated browser use.
// Same-origin proxied node content is still trusted in v1; long-term host-based
// node isolation should remove that caveat.
func hubBrowserRequestAllowed(r *http.Request, requireJSON bool) bool {
	if requireJSON {
		ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
		if ct == "" || (!strings.HasPrefix(ct, "application/json") && !strings.HasPrefix(ct, "application/merge-patch+json")) {
			return false
		}
	}
	if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site == "cross-site" || site == "same-site" {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (s *hubServer) handleHubHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "role": "hub"})
}

// collectNodes resolves all nodes and probes them concurrently. Resolver
// errors are soft: surviving sources still render, the error is reported
// alongside.
func (s *hubServer) collectNodes(ctx context.Context) ([]hubNodeView, error) {
	nodes, err := s.registry.Nodes()
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	statuses := s.prober.ProbeAll(probeCtx, nodes)
	views := make([]hubNodeView, 0, len(nodes))
	for _, n := range nodes {
		view := hubNodeView{
			ID:         n.ID,
			Name:       n.Name,
			Source:     n.Source,
			Connection: n.Connection,
			URL:        n.URL,
			BasePath:   n.BasePath,
			ProxyPath:  "/node/" + n.ID + "/",
			HasToken:   n.Token != "",
			Status:     statuses[n.ID],
		}
		if n.UsesReverseConnection() {
			connected, connectedAt, lastSeen := s.reverse.status(n.ID)
			if connected {
				view.Status = hub.Status{Reachable: true, State: "connected", LatencyMS: 0, Details: map[string]string{
					"connection":   "reverse",
					"connected_at": connectedAt.Format(time.RFC3339),
					"last_seen":    lastSeen.Format(time.RFC3339),
				}}
			} else {
				view.Status = hub.Status{State: "disconnected", Error: "waiting for reverse connection", Details: map[string]string{"connection": "reverse"}}
			}
		}
		views = append(views, view)
	}
	for i := range views {
		views[i].Diagnostics = hubNodeDiagnostics(nodes[i], views[i], nodes)
	}
	return views, err
}

func hubNodeDiagnostics(n hub.Node, view hubNodeView, all []hub.Node) []hubNodeDiagnostic {
	diagnostics := []hubNodeDiagnostic{}
	add := func(severity, code, message string) {
		diagnostics = append(diagnostics, hubNodeDiagnostic{Severity: severity, Code: code, Message: message})
	}
	if n.UsesReverseConnection() && !view.Status.Reachable {
		add("error", "reverse_disconnected", "Reverse node is waiting for its outbound websocket connection.")
	}
	if n.Token == "" {
		add("warning", "missing_token", "No bearer token is configured; authenticated health, proxy, and delegation calls may fail.")
	}
	if n.Delegation != nil && n.Delegation.Enabled {
		if strings.TrimSpace(n.Delegation.Workdir) == "" {
			add("warning", "delegation_missing_workdir", "Delegation is enabled, but no workdir is configured; this node cannot accept delegated jobs.")
		} else if !hubStatusHasCapability(view.Status, "jobs") {
			if view.Status.Reachable && len(view.Status.Capabilities) > 0 {
				add("warning", "delegation_jobs_missing", "This node accepts delegation, but its health capabilities do not include jobs; start it with jobs enabled.")
			} else {
				add("warning", "delegation_jobs_unknown", "This node accepts delegation, but the Hub cannot verify the jobs capability; check the token and node version.")
			}
		}
	}
	for _, msg := range hubPolicyMismatchDiagnostics(n, all) {
		add("warning", "delegation_policy_mismatch", msg)
		if len(diagnostics) >= 8 {
			break
		}
	}
	return diagnostics
}

func hubStatusHasCapability(st hub.Status, capability string) bool {
	for _, c := range st.Capabilities {
		if strings.EqualFold(strings.TrimSpace(c), capability) {
			return true
		}
	}
	return false
}

func hubPolicyMismatchDiagnostics(n hub.Node, all []hub.Node) []string {
	if n.Delegation == nil || !n.Delegation.Enabled {
		return nil
	}
	messages := []string{}
	for _, target := range all {
		if target.ID == n.ID || !n.CanDelegateTo(target.ID) {
			continue
		}
		if target.Delegation == nil || !target.Delegation.Enabled {
			messages = append(messages, fmt.Sprintf("Can delegate to %q, but that node has delegation disabled.", target.ID))
			continue
		}
		if strings.TrimSpace(target.Delegation.Workdir) == "" {
			messages = append(messages, fmt.Sprintf("Can delegate to %q, but that node has no delegation workdir.", target.ID))
			continue
		}
		acceptFrom := target.Delegation.AcceptFrom
		if len(acceptFrom) == 0 {
			acceptFrom = []string{"*"}
		}
		if !hubNodeListMatches(acceptFrom, n.ID) {
			messages = append(messages, fmt.Sprintf("Can delegate to %q, but that node does not accept from %q.", target.ID, n.ID))
		}
	}
	if len(n.Delegation.AcceptFrom) > 0 && !hubNodeListMatches(n.Delegation.AcceptFrom, "*") && strings.TrimSpace(n.Delegation.Workdir) != "" {
		for _, origin := range all {
			if origin.ID == n.ID || !hubNodeListMatches(n.Delegation.AcceptFrom, origin.ID) {
				continue
			}
			if !origin.CanDelegateTo(n.ID) {
				messages = append(messages, fmt.Sprintf("Accepts from %q, but that node is not configured to delegate here.", origin.ID))
			}
		}
	}
	return messages
}

func hubNodeListMatches(patterns []string, id string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "*" || p == id {
			return true
		}
	}
	return false
}

func (s *hubServer) handleNodes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		views, err := s.collectNodes(r.Context())
		resp := map[string]any{"nodes": views}
		if err != nil {
			resp["resolver_error"] = err.Error()
		}
		writeJSON(w, http.StatusOK, resp)
	case http.MethodPost:
		if !hubBrowserRequestAllowed(r, true) {
			http.Error(w, "forbidden cross-site hub request", http.StatusForbidden)
			return
		}
		s.handleAddNode(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// addNodeRequest is the Add Node form payload. Token is accepted here (over
// the loopback-only hub) and persisted to the 0600 local store; it is never
// echoed back.
type addNodeRequest struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	BasePath string `json:"base_path"`
	Token    string `json:"token"`
}

func (s *hubServer) handleAddNode(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "node persistence is disabled", http.StatusForbidden)
		return
	}
	var req addNodeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	node, err := s.store.Add(hub.Node{
		ID:       strings.TrimSpace(req.ID),
		Name:     strings.TrimSpace(req.Name),
		URL:      req.URL,
		BasePath: strings.TrimSpace(req.BasePath),
		Token:    strings.TrimSpace(req.Token),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Pre-existing config/contain nodes win registry precedence; warn the
	// caller when the stored node is shadowed by an identical ID elsewhere.
	if existing, ok := s.registry.Lookup(node.ID); ok && existing.Source != hub.SourceLocal {
		writeJSON(w, http.StatusCreated, map[string]any{
			"id":      node.ID,
			"warning": fmt.Sprintf("node id %q is shadowed by a %s node with the same id", node.ID, existing.Source),
		})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": node.ID})
}

// handleNodeItem handles DELETE /api/nodes/<id> (local-store nodes only).
func (s *hubServer) handleNodeItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !hubBrowserRequestAllowed(r, false) {
		http.Error(w, "forbidden cross-site hub request", http.StatusForbidden)
		return
	}
	if s.store == nil {
		http.Error(w, "node persistence is disabled", http.StatusForbidden)
		return
	}
	if err := s.store.Remove(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": id})
}

// handleTestNode probes an ad-hoc node spec (the Add Node form's Test
// connection button) without persisting anything. The supplied token is used
// for the probe only and never stored or echoed.
func (s *hubServer) handleTestNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !hubBrowserRequestAllowed(r, true) {
		http.Error(w, "forbidden cross-site hub request", http.StatusForbidden)
		return
	}
	var req addNodeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	n := hub.Node{ID: "test", Name: "test", URL: req.URL, BasePath: strings.TrimSpace(req.BasePath), Token: strings.TrimSpace(req.Token)}
	if err := n.Normalize(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"status": s.prober.Probe(ctx, n)})
}

// The dashboard markup and styles live in editable .html/.css files so they
// keep editor support while the hub stays a single go:embed binary, matching
// the serveui convention. The CSS is inlined as template.CSS so the page is
// self-contained (no extra stylesheet request).
//
//go:embed templates/hub_index.html
var hubIndexHTML string

//go:embed templates/hub.css
var hubIndexCSS string

var hubIndexTmpl = template.Must(template.New("hub-index").Parse(hubIndexHTML))

type hubIndexView struct {
	CSS template.CSS
	// CanAddNodes toggles the Add Node UI.
	CanAddNodes bool
}

func (s *hubServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Headers are committed if Execute fails mid-stream; nothing useful to
	// surface to the client at that point.
	_ = hubIndexTmpl.Execute(w, hubIndexView{
		CSS:         template.CSS(hubIndexCSS),
		CanAddNodes: s.store != nil,
	})
}

func (s *hubServer) handleNodeProxy(w http.ResponseWriter, r *http.Request) {
	if !hubBrowserRequestAllowed(r, false) {
		http.Error(w, "forbidden cross-site hub request", http.StatusForbidden)
		return
	}
	// Reject encoded path separators outright: r.URL.Path is already decoded,
	// so %2f would otherwise smuggle a separator past the segment checks.
	if hubContainsEncodedSeparator(r.URL.EscapedPath()) {
		http.Error(w, "bad request: encoded path separators not allowed", http.StatusBadRequest)
		return
	}
	id, rest, ok := parseHubNodePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := hub.ValidateID(id); err != nil {
		http.Error(w, fmt.Sprintf("invalid node id %q", id), http.StatusBadRequest)
		return
	}
	if hubHasDotDotSegment(rest) {
		http.Error(w, "bad request: path traversal not allowed", http.StatusBadRequest)
		return
	}
	node, ok := s.registry.Lookup(id)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown node %q", id), http.StatusNotFound)
		return
	}
	if node.UsesReverseConnection() {
		s.handleReverseNodeProxy(w, r, node, rest)
		return
	}
	// Bare /node/<id> -> /node/<id>/ so the node UI's relative URLs resolve
	// under the mount. Preserve the query string.
	if rest == "" {
		target := "/node/" + id + "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}
	scheme, host, ok := strings.Cut(node.URL, "://")
	if !ok {
		http.Error(w, fmt.Sprintf("node %q has an invalid url", id), http.StatusBadGateway)
		return
	}
	t := &hubProxyTarget{
		nodeID:   node.ID,
		nodeName: node.Name,
		scheme:   scheme,
		host:     host,
		path:     hubJoinBasePath(node.BasePath, rest),
		token:    node.Token,
		basePath: strings.TrimRight(node.BasePath, "/"),
		mount:    "/node/" + node.ID,
	}
	s.proxy.ServeHTTP(w, r.WithContext(withHubProxyTarget(r.Context(), t)))
}

func (s *hubServer) handleReverseNodeProxy(w http.ResponseWriter, r *http.Request, node hub.Node, rest string) {
	if !s.reverse.isConnected(node.ID) {
		http.Error(w, fmt.Sprintf("node %q reverse connection is not connected", node.ID), http.StatusBadGateway)
		return
	}
	if rest == "" {
		target := "/node/" + node.ID + "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}
	t := &hubProxyTarget{
		nodeID:   node.ID,
		nodeName: node.Name,
		path:     hubJoinBasePath(node.BasePath, rest),
		token:    node.Token,
		basePath: strings.TrimRight(node.BasePath, "/"),
		mount:    "/node/" + node.ID,
	}
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(withHubProxyTarget(r.Context(), t), r.Method, "http://reverse.local"+t.path, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.URL.RawQuery = r.URL.RawQuery
	req.Header = r.Header.Clone()
	if node.Token != "" {
		req.Header.Set("Authorization", "Bearer "+node.Token)
	} else {
		req.Header.Del("Authorization")
	}
	req.Header.Del("Cookie")
	req.Header.Del("X-Api-Key")
	req.Header.Del("Accept-Encoding")
	req.Header.Del("X-Forwarded-For")
	req.Header.Del("X-Forwarded-Host")
	req.Header.Del("X-Forwarded-Proto")
	req.Header.Del("X-Forwarded-Prefix")
	req.Header.Del("Forwarded")
	resp, err := s.reverse.do(r.Context(), node, req)
	if err != nil {
		hubProxyErrorHandler(w, r.WithContext(withHubProxyTarget(r.Context(), t)), err)
		return
	}
	defer resp.Body.Close()
	if err := hubRebaseProxyResponse(resp); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// parseHubNodePath splits "/node/<id>/<rest>" into id and the remainder
// (including its leading slash). "/node/<id>" yields an empty rest.
func parseHubNodePath(p string) (id, rest string, ok bool) {
	const prefix = "/node/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	tail := p[len(prefix):]
	if tail == "" {
		return "", "", false
	}
	if slash := strings.IndexByte(tail, '/'); slash >= 0 {
		return tail[:slash], tail[slash:], true
	}
	return tail, "", true
}

// hubJoinBasePath joins a node's base path with the proxied remainder,
// collapsing the slash seam. An empty remainder targets the base path root.
func hubJoinBasePath(base, rest string) string {
	base = strings.TrimRight(base, "/")
	if rest == "" {
		return base + "/"
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return base + rest
}

// hubContainsEncodedSeparator reports whether an escaped path smuggles an
// encoded path separator (%2f) or backslash (%5c).
func hubContainsEncodedSeparator(escapedPath string) bool {
	lower := strings.ToLower(escapedPath)
	return strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c")
}

// hubHasDotDotSegment reports whether any segment of the decoded path is "..".
func hubHasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// hubRewriteProxyRequest is the ReverseProxy Rewrite hook. Using Rewrite (vs
// the legacy Director) means ReverseProxy does NOT auto-append X-Forwarded-*;
// we also explicitly drop client-supplied forwarding/credential headers so
// they can neither spoof metadata nor reach the node.
func hubRewriteProxyRequest(pr *httputil.ProxyRequest) {
	t := hubProxyTargetFrom(pr.In.Context())
	if t == nil {
		return
	}
	out := pr.Out
	out.URL.Scheme = t.scheme
	out.URL.Host = t.host
	out.URL.Path = t.path
	out.URL.RawPath = "" // force Go to re-encode Path from the cleaned value
	out.Host = t.host

	// Inject the real per-node token server-side; strip every client-supplied
	// credential the node would otherwise honor (Authorization Bearer,
	// x-api-key, and the term_llm_token cookie).
	if t.token != "" {
		out.Header.Set("Authorization", "Bearer "+t.token)
	} else {
		out.Header.Del("Authorization")
	}
	out.Header.Del("Cookie")
	out.Header.Del("X-Api-Key")

	// Take ownership of response encoding so ModifyResponse sees decompressed
	// HTML: with Accept-Encoding cleared, the Transport transparently
	// negotiates and decodes gzip itself.
	out.Header.Del("Accept-Encoding")

	// Drop spoofable forwarding metadata.
	out.Header.Del("X-Forwarded-For")
	out.Header.Del("X-Forwarded-Host")
	out.Header.Del("X-Forwarded-Proto")
	out.Header.Del("X-Forwarded-Prefix")
	out.Header.Del("Forwarded")
}

// hubRebaseProxyResponse rewrites the node's baked-in base path onto the hub
// mount (/node/<id>) for HTML documents, fixes redirect Location headers, and
// injects the window.TERM_LLM_HUB context so the node UI can render its
// "Back to Hub" link. Because the SPA derives every URL it builds from the
// single window.TERM_LLM_UI_PREFIX value and the <base> tag, rebasing those
// two strings re-homes all subsequent requests onto /node/<id>/* where the
// token is injected. Non-HTML bodies (JS, JSON, SSE, images) pass through
// untouched, which keeps streaming responses streaming.
func hubRebaseProxyResponse(resp *http.Response) error {
	t := hubProxyTargetFrom(resp.Request.Context())
	if t == nil {
		return nil
	}

	hubRewriteLocationHeader(resp, t)

	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil
	}
	// A HEAD response carries the real Content-Length but no body; rewriting
	// would clobber that length to 0.
	if resp.Request.Method == http.MethodHead {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if len(body) == 0 {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}

	if t.basePath != "" && t.basePath != t.mount {
		var baseHits, prefixHits int
		body, baseHits, prefixHits = hubRebaseUIPrefix(body, t.basePath, t.mount)
		if (baseHits == 0 || prefixHits == 0) && resp.StatusCode == http.StatusOK {
			// One of the two prefix tokens the SPA bakes into its HTML no
			// longer matches our needle (serveui's emitted shape drifted).
			// Open-via-hub silently breaks when this happens, so be loud.
			log.Printf("WARNING: hub node %q: UI prefix rebase matched base=%d prefix=%d (expected >=1 each); open-via-hub may be broken — check serveui's <base>/TERM_LLM_UI_PREFIX shape",
				t.nodeID, baseHits, prefixHits)
		}
	}
	body = hubInjectContext(body, t)

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	// The Transport already decoded any gzip (we cleared Accept-Encoding);
	// make sure no stale encoding header survives the body rewrite.
	resp.Header.Del("Content-Encoding")
	return nil
}

// hubRebaseUIPrefix replaces the two prefix tokens the node bakes into its
// served HTML. The needles are built with the SAME html.EscapeString /
// json.Marshal shapes the serve uses (internal/serveui/embed.go and
// cmd/serve_handlers.go) so they match byte-for-byte regardless of the node's
// actual base path. It reports how many times each needle matched so the
// caller can warn loudly on drift.
func hubRebaseUIPrefix(body []byte, basePath, mount string) (out []byte, baseHits, prefixHits int) {
	oldBase := []byte(`<base href="` + htmlpkg.EscapeString(basePath) + `/">`)
	newBase := []byte(`<base href="` + htmlpkg.EscapeString(mount) + `/">`)
	baseHits = bytes.Count(body, oldBase)
	body = bytes.ReplaceAll(body, oldBase, newBase)

	oldPrefix, _ := json.Marshal(basePath)
	newPrefix, _ := json.Marshal(mount)
	oldPrefixNeedle := []byte("window.TERM_LLM_UI_PREFIX=" + string(oldPrefix))
	prefixHits = bytes.Count(body, oldPrefixNeedle)
	body = bytes.ReplaceAll(body, oldPrefixNeedle,
		[]byte("window.TERM_LLM_UI_PREFIX="+string(newPrefix)))
	return body, baseHits, prefixHits
}

// hubInjectContext injects window.TERM_LLM_HUB into the served HTML head so
// the node's web UI knows it was opened via the hub and can render a "Back
// to Hub" link. The hub URL is root-relative ("/") because the proxied UI is
// same-origin with the hub.
func hubInjectContext(body []byte, t *hubProxyTarget) []byte {
	ctxJSON, err := json.Marshal(map[string]string{
		"url":      "/",
		"nodeId":   t.nodeID,
		"nodeName": t.nodeName,
	})
	if err != nil {
		return body
	}
	snippet := []byte(`<script>window.TERM_LLM_HUB=` + string(ctxJSON) + `;</script></head>`)
	if bytes.Contains(body, []byte("</head>")) {
		return bytes.Replace(body, []byte("</head>"), snippet, 1)
	}
	return body
}

// hubRewriteLocationHeader rebases a root-relative redirect Location that
// points at the node base path onto the hub mount, so node redirects (e.g.
// the /chat -> /chat/ normalization) land back inside /node/<id>/.
func hubRewriteLocationHeader(resp *http.Response, t *hubProxyTarget) {
	loc := resp.Header.Get("Location")
	if loc == "" || t.basePath == "" {
		return
	}
	if loc == t.basePath {
		resp.Header.Set("Location", t.mount)
		return
	}
	if strings.HasPrefix(loc, t.basePath+"/") {
		resp.Header.Set("Location", t.mount+loc[len(t.basePath):])
	}
}

func hubProxyErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	id := "node"
	if t := hubProxyTargetFrom(r.Context()); t != nil {
		id = t.nodeID
	}
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintf(w, "hub: node %q backend unreachable: %v\n", id, err)
}

var (
	serveHubHost      string
	serveHubPort      int
	serveHubConfig    string
	serveHubContain   bool
	serveHubNodesFile string
	serveHubAuthMode  string
	serveHubToken     string
)

var serveHubCmd = &cobra.Command{
	Use:   "hub",
	Short: "Run the term-llm Hub: one dashboard over many term-llm web nodes (experimental)",
	Long: `Run the term-llm Hub, a launcher and control plane over many term-llm web
nodes (serves). Nodes are discovered from a static config file (--config),
from local contain workspaces, and from nodes added in the dashboard UI
(persisted to a local JSON store).

The dashboard lists every node with live reachability, latency, and any
detected agent/version/capabilities, and opens a node's full web UI through
the hub at /node/<id>/ with the node's bearer token injected server-side —
node tokens never reach the browser.

Routes:
  GET  /                  hub dashboard
  GET  /api/nodes         list nodes with probe status (never includes tokens)
  POST /api/nodes         add a node to the local store
  DELETE /api/nodes/<id>  remove a local-store node
  POST /api/nodes/test    probe a node spec without persisting it
  GET  /api/connect      reverse-node websocket endpoint (node auth)
  ANY  /node/<id>/...     reverse proxy to that node's serve
  POST /api/delegations   create a cross-node delegation (node auth)
  GET  /api/delegations   list delegations
  GET  /api/delegations/<id>         delegation status
  POST /api/delegations/<id>/cancel  cancel (originating node only)

Config file (--config), YAML or JSON:
  nodes:
    - name: jarvis
      url: http://127.0.0.1:8081/chat
      token: <web bearer token>

EXPERIMENTAL: Hub auth is intentionally simple: --auth bearer (the default)
protects the dashboard, registry API, and node proxy with one Hub bearer token.
/api/connect and node-originated delegation calls use node auth instead. Use
--auth none only for loopback-only local development.`,
	Args: cobra.NoArgs,
	RunE: runServeHub,
}

// validateHubBind rejects unauthenticated public binds. A Hub with bearer auth
// may bind publicly for use behind a reverse proxy, but --auth none stays
// loopback-only because the Hub injects node tokens server-side.
func validateHubBind(host string, port int, requireAuth bool) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid --port %d (must be 1-65535)", port)
	}
	if !requireAuth && !isLoopbackHost(host) {
		return fmt.Errorf("--auth none is only allowed on loopback hosts (got %q)", host)
	}
	return nil
}

// defaultHubNodesFile is where dashboard-added nodes persist when
// --nodes-file is not given.
func defaultHubNodesFile() (string, error) {
	dir, err := appdata.GetDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub", "nodes.json"), nil
}

func runServeHub(cmd *cobra.Command, args []string) error {
	authMode, err := resolveServeAuthMode(cmd.Flags().Changed("auth"), serveHubAuthMode, false, false)
	if err != nil {
		return err
	}
	requireAuth := authMode != "none"
	if err := validateHubBind(serveHubHost, serveHubPort, requireAuth); err != nil {
		return err
	}
	token, tokenSource, err := resolveServeToken(serveHubToken, os.Getenv("TERM_LLM_HUB_TOKEN"), requireAuth, generateServeToken)
	if err != nil {
		return err
	}

	var resolvers []hub.Resolver
	if strings.TrimSpace(serveHubConfig) != "" {
		resolvers = append(resolvers, hub.NewStaticResolver(serveHubConfig))
	}
	nodesFile := strings.TrimSpace(serveHubNodesFile)
	if nodesFile == "" {
		var err error
		nodesFile, err = defaultHubNodesFile()
		if err != nil {
			return fmt.Errorf("resolve hub nodes file: %w", err)
		}
	}
	store := hub.NewStore(nodesFile)
	resolvers = append(resolvers, store)
	if serveHubContain {
		resolvers = append(resolvers, hub.NewContainResolver())
	}

	s := newHubServer(hub.NewRegistry(resolvers...), store)
	s.requireAuth = requireAuth
	s.token = token
	// The delegation ledger lives beside the node store (same private dir).
	s.delegations = hub.NewDelegationStore(filepath.Join(filepath.Dir(nodesFile), "delegations.json"))
	addr := net.JoinHostPort(serveHubHost, strconv.Itoa(serveHubPort))
	srv := &http.Server{Addr: addr, Handler: s.handler()}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "term-llm Hub listening on http://%s\n", addr)
	fmt.Fprintf(out, "  GET http://%s/api/nodes\n", addr)
	fmt.Fprintf(out, "  ANY http://%s/node/<id>/...\n", addr)
	fmt.Fprintf(out, "  node store: %s\n", nodesFile)
	fmt.Fprintf(out, "  auth: %s\n", authSummary(requireAuth))
	if requireAuth {
		switch tokenSource {
		case tokenSourceGenerated:
			fmt.Fprintf(out, "  generated Hub bearer token: %s\n", token)
		case tokenSourceEnv:
			fmt.Fprintln(out, "  Hub bearer token: from TERM_LLM_HUB_TOKEN")
		case tokenSourceFlag:
			fmt.Fprintln(out, "  Hub bearer token: from --token")
		}
	} else {
		fmt.Fprintln(out, "WARNING: hub auth disabled; bind to loopback only.")
	}
	return srv.ListenAndServe()
}

func init() {
	serveCmd.AddCommand(serveHubCmd)
	serveHubCmd.Flags().StringVar(&serveHubHost, "host", "127.0.0.1", "Host to bind")
	serveHubCmd.Flags().IntVar(&serveHubPort, "port", 8090, "Port to bind")
	serveHubCmd.Flags().StringVar(&serveHubConfig, "config", "", "Path to a static nodes config file (YAML or JSON)")
	serveHubCmd.Flags().BoolVar(&serveHubContain, "contain", true, "Discover nodes from local contain workspaces")
	serveHubCmd.Flags().StringVar(&serveHubNodesFile, "nodes-file", "", "Path to the JSON store for dashboard-added nodes (default: <data-dir>/hub/nodes.json)")
	serveHubCmd.Flags().StringVar(&serveHubAuthMode, "auth", "bearer", "Hub auth mode: bearer or none (none is loopback-only)")
	serveHubCmd.Flags().StringVar(&serveHubToken, "token", "", "Hub bearer token (defaults to $TERM_LLM_HUB_TOKEN, else auto-generated)")
}
