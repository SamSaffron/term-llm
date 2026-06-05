package cmd

import (
	"bytes"
	"context"
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
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/contain"
	"github.com/samsaffron/term-llm/internal/widgets"
	"github.com/spf13/cobra"
)

// gateway is a thin multi-agent reverse proxy fronting N per-agent contain
// serves. An agent's serve is bound to a single agent at startup, so multi-agent
// access is achieved by fronting many serves rather than routing within one
// process. The gateway discovers each agent's published port + bearer token
// from its contain workspace .env (via contain.ReadWebConfig) and injects the
// token server-side, so the per-agent token is never exposed to the client.
//
// Clicking an agent on the landing page opens that agent's full web UI through
// /agent/<name>/*: the proxy rebases the agent's baked-in URL prefix
// (window.TERM_LLM_UI_PREFIX and the <base> tag, both /chat) onto /agent/<name>
// so every SPA-built URL — API calls, the service worker, the auth cookie, and
// img/iframe subresources — routes back through the proxy, where the per-agent
// token is injected. The browser therefore never holds the token.
//
// Gateway-level authentication ({member-token -> allowed agents}) is
// intentionally not yet wired. Do NOT expose it beyond loopback until that
// boundary exists.
type gateway struct {
	// resolve returns the web config (port/token/base path) for a named agent.
	// Overridable in tests.
	resolve func(name string) (contain.WebConfig, error)
	// list enumerates discoverable agent workspaces. Overridable in tests.
	list func() ([]contain.ListEntry, error)
	// host is the host the agents' serves are published on (loopback in the
	// current one-container-per-agent shape).
	host string
	// widgetHost, when non-empty, is the dedicated origin (e.g.
	// "widgets.localhost") on which the gateway serves ONLY the widget subtree
	// (/w/<agent>/<mount>/*). It hosts nothing sensitive — no /agents, no
	// /agent/*, no landing page — so a widget iframe granted allow-same-origin
	// gets only its own throwaway origin. Empty disables the widget host.
	widgetHost string
	// proxy is the single shared reverse proxy for all agents. Per-request
	// target data is threaded through the request context (see proxyTarget) so
	// connections can be pooled across requests rather than allocating a proxy
	// per request.
	proxy *httputil.ReverseProxy
	// fetchWidgets returns an agent's loaded widgets by querying its serve.
	// Overridable in tests. It reuses widgets.WidgetStatus (the type the serve's
	// /admin/widgets/status endpoint marshals) rather than a private copy of the
	// same wire shape; only the mount/title/state fields are rendered.
	fetchWidgets func(web contain.WebConfig) ([]widgets.WidgetStatus, error)
}

func newGateway() *gateway {
	g := &gateway{
		resolve: newWebConfigCache().get,
		list:    contain.Definitions,
		host:    "127.0.0.1",
	}
	g.proxy = &httputil.ReverseProxy{
		Rewrite:        rewriteProxyRequest,
		ModifyResponse: rebaseProxyResponse,
		ErrorHandler:   proxyErrorHandler,
		Transport:      newGatewayTransport(),
	}
	g.fetchWidgets = g.fetchWidgetsHTTP
	return g
}

// fetchWidgetsHTTP asks an agent's serve for its loaded widgets via the same
// token-injected channel the proxy uses. The bearer token is sent server-side
// and the response carries no secrets (mount/title/state only).
func (g *gateway) fetchWidgetsHTTP(web contain.WebConfig) ([]widgets.WidgetStatus, error) {
	base := strings.TrimRight(web.BasePath, "/")
	u := "http://" + net.JoinHostPort(g.host, web.Port) + base + "/admin/widgets/status"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+web.Token)

	client := &http.Client{Transport: g.proxy.Transport, Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("widget status: %s", resp.Status)
	}
	var body struct {
		Widgets []widgets.WidgetStatus `json:"widgets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Widgets, nil
}

// newGatewayTransport returns the proxy's HTTP transport with bounded
// connection and response-header timeouts so a hung backend cannot tie up the
// proxy indefinitely. It deliberately sets NO whole-response/read deadline:
// long-lived streams (SSE, WebRTC signalling) must stay open, so only the time
// to establish the connection and to receive the response *headers* is bounded.
func newGatewayTransport() *http.Transport {
	return &http.Transport{
		// No environment proxy: the gateway dials known agent hosts directly, and
		// routing a token-injected request (Authorization: Bearer) through an
		// HTTP_PROXY would leak the per-agent bearer token to that proxy.
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

// webConfigCache memoises per-agent WebConfig lookups so the gateway does not
// re-read (and re-parse) each agent's 0600 .env on every proxied request. An
// entry is reused only while the .env's mtime is unchanged; any change — or a
// failed stat — forces a fresh read, so edits to a workspace are picked up
// without a restart and a stale entry is never served.
type webConfigCache struct {
	mu    sync.Mutex
	items map[string]webConfigEntry
	// load reads an agent's web config; stat returns its .env mtime. Both are
	// fields so tests can substitute deterministic implementations.
	load func(name string) (contain.WebConfig, error)
	stat func(name string) (time.Time, error)
}

type webConfigEntry struct {
	cfg   contain.WebConfig
	mtime time.Time
}

func newWebConfigCache() *webConfigCache {
	return &webConfigCache{
		items: map[string]webConfigEntry{},
		load:  contain.ReadWebConfig,
		stat:  statEnvMtime,
	}
}

// get returns the agent's web config, reusing the cached value while the .env
// mtime is unchanged. The load happens under the lock: with one container per
// agent the contention is negligible and it keeps a concurrent miss from
// stampeding the parser.
func (c *webConfigCache) get(name string) (contain.WebConfig, error) {
	mtime, statErr := c.stat(name)

	c.mu.Lock()
	defer c.mu.Unlock()
	if statErr == nil {
		if e, ok := c.items[name]; ok && e.mtime.Equal(mtime) {
			return e.cfg, nil
		}
	}
	cfg, err := c.load(name)
	if err != nil {
		return contain.WebConfig{}, err
	}
	if statErr == nil {
		c.items[name] = webConfigEntry{cfg: cfg, mtime: mtime}
	}
	return cfg, nil
}

// statEnvMtime returns the modification time of an agent's workspace .env.
func statEnvMtime(name string) (time.Time, error) {
	path, err := contain.EnvPath(name)
	if err != nil {
		return time.Time{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// proxyTarget carries the per-request reverse-proxy target. It is stashed on the
// inbound request context by handleProxy and read back in the proxy's Rewrite,
// ModifyResponse, and ErrorHandler hooks.
type proxyTarget struct {
	name       string // agent name (for diagnostics)
	host       string // backend host:port (loopback)
	targetPath string // backend path: agent base path + remainder
	token      string // per-agent bearer token, injected server-side
	basePath   string // agent's baked-in prefix, e.g. /chat
	mount      string // gateway-facing prefix, e.g. /agent/<name>
	// rebaseUI requests the SPA prefix rewrite in ModifyResponse. It is set for
	// the agent proxy (whose served HTML bakes in window.TERM_LLM_UI_PREFIX and
	// a <base> tag) and cleared for the widget proxy, whose responses resolve
	// against their own origin and must pass through untouched.
	rebaseUI bool
}

type proxyTargetKey struct{}

func withProxyTarget(ctx context.Context, t *proxyTarget) context.Context {
	return context.WithValue(ctx, proxyTargetKey{}, t)
}

func proxyTargetFrom(ctx context.Context) *proxyTarget {
	t, _ := ctx.Value(proxyTargetKey{}).(*proxyTarget)
	return t
}

// agentInfo is the public discovery record for one agent. It deliberately omits
// the bearer token: the token is injected server-side and must never be sent to
// a gateway client.
type agentInfo struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Port      string `json:"port"`
	BasePath  string `json:"base_path"`
	ProxyPath string `json:"proxy_path"`
	// Reachable reports whether the agent has a provisioned web token and can
	// therefore be proxied.
	Reachable bool `json:"reachable"`
}

func (g *gateway) handler() http.Handler {
	agentMux := http.NewServeMux()
	agentMux.HandleFunc("/agents", g.handleAgents)
	agentMux.HandleFunc("/agent/", g.handleProxy)
	agentMux.HandleFunc("/", g.handleIndex)

	if g.widgetHost == "" {
		return agentMux
	}

	// The widget origin exposes ONLY the widget subtree. Every other path 404s
	// (the default for an otherwise-empty mux), which is the isolation property.
	widgetMux := http.NewServeMux()
	widgetMux.HandleFunc("/w/", g.handleWidgetProxy)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.isWidgetHost(r.Host) {
			widgetMux.ServeHTTP(w, r)
			return
		}
		agentMux.ServeHTTP(w, r)
	})
}

// isWidgetHost reports whether the request's Host (port stripped) addresses the
// dedicated widget origin. The same-origin policy is per origin, so routing by
// host — not just by path — is what isolates widgets from the agent gateway.
func (g *gateway) isWidgetHost(host string) bool {
	if g.widgetHost == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.EqualFold(host, g.widgetHost)
}

// collectAgents enumerates discoverable agents and their resolved web config.
// The bearer token is never carried on the returned records.
func (g *gateway) collectAgents() ([]agentInfo, error) {
	entries, err := g.list()
	if err != nil {
		return nil, err
	}
	agents := make([]agentInfo, 0, len(entries))
	for _, e := range entries {
		info := agentInfo{
			Name:      e.Name,
			Status:    e.Status,
			ProxyPath: "/agent/" + e.Name + "/",
		}
		if web, err := g.resolve(e.Name); err == nil {
			info.Port = web.Port
			info.BasePath = web.BasePath
			info.Reachable = web.Token != ""
		}
		agents = append(agents, info)
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	return agents, nil
}

func (g *gateway) handleAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := g.collectAgents()
	if err != nil {
		http.Error(w, fmt.Sprintf("list agents: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Agents []agentInfo `json:"agents"`
	}{Agents: agents}); err != nil {
		http.Error(w, fmt.Sprintf("encode agents: %v", err), http.StatusInternalServerError)
	}
}

// handleIndex serves the gateway landing page: a list of discoverable agents,
// each linking to its proxied web UI at /agent/<name>/. Clicking a reachable
// agent opens its full UI through the proxy with the token injected server-side.
func (g *gateway) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	agents, err := g.collectAgents()
	if err != nil {
		http.Error(w, fmt.Sprintf("list agents: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Headers are already committed if Execute fails mid-stream; nothing useful
	// to surface to the client at that point.
	_ = gatewayIndexTmpl.Execute(w, g.buildIndexView(agents, r.Host))
}

// gatewayIndexView is the data the landing page renders.
type gatewayIndexView struct {
	// CSS is the embedded stylesheet, inlined into the page verbatim.
	CSS        template.CSS
	Agents     []indexAgentView
	WidgetHost string
	// AnyWidgets is true when at least one agent reported a widget, so the
	// Widgets section can show the grouped list rather than an empty-state hint.
	AnyWidgets bool
}

// indexAgentView is one agent plus its resolved widget links for rendering.
type indexAgentView struct {
	agentInfo
	Widgets []widgetLink
}

// widgetLink is a single openable widget on the widget origin.
type widgetLink struct {
	Title string
	State string
	URL   string
}

// buildIndexView assembles the landing-page data. When the widget host is
// enabled, it enriches each reachable agent with its widgets, fetched
// concurrently; a per-agent fetch failure is soft (that agent simply shows no
// widgets) so one unreachable agent never breaks the page.
func (g *gateway) buildIndexView(agents []agentInfo, reqHost string) gatewayIndexView {
	view := gatewayIndexView{
		CSS:        template.CSS(gatewayIndexCSS),
		Agents:     make([]indexAgentView, len(agents)),
		WidgetHost: g.widgetHost,
	}
	origin := ""
	if g.widgetHost != "" {
		origin = widgetOrigin(reqHost, g.widgetHost)
	}

	var wg sync.WaitGroup
	for i, a := range agents {
		view.Agents[i] = indexAgentView{agentInfo: a}
		if g.widgetHost == "" || !a.Reachable {
			continue
		}
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			web, err := g.resolve(name)
			if err != nil {
				return
			}
			summaries, err := g.fetchWidgets(web)
			if err != nil {
				return
			}
			links := make([]widgetLink, 0, len(summaries))
			for _, s := range summaries {
				links = append(links, widgetLink{
					Title: s.Title,
					State: s.State,
					URL:   origin + "/w/" + name + "/" + s.Mount + "/",
				})
			}
			view.Agents[i].Widgets = links
		}(i, a.Name)
	}
	wg.Wait()
	for _, a := range view.Agents {
		if len(a.Widgets) > 0 {
			view.AnyWidgets = true
			break
		}
	}
	return view
}

// widgetOrigin returns the http origin of the widget host, preserving the port
// the gateway was reached on (so the link resolves to this same listener under
// the widget hostname).
func widgetOrigin(reqHost, widgetHost string) string {
	host := widgetHost
	if _, port, err := net.SplitHostPort(reqHost); err == nil && port != "" {
		host = net.JoinHostPort(widgetHost, port)
	}
	return "http://" + host
}

// The landing page markup and styles live in editable .html/.css files so they
// keep editor support (highlighting, formatting) while the gateway stays a
// single go:embed binary. The CSS is injected into the page as template.CSS so
// the page remains self-contained on any origin (no extra stylesheet request).
//
//go:embed templates/gateway_index.html
var gatewayIndexHTML string

//go:embed templates/gateway.css
var gatewayIndexCSS string

var gatewayIndexTmpl = template.Must(template.New("gateway-index").Parse(gatewayIndexHTML))

func (g *gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	// Reject encoded path separators outright. They have no legitimate use in a
	// proxied path and are the encoded-slash traversal vector: r.URL.Path is
	// already decoded, so %2f would otherwise smuggle a separator the segment
	// checks below cannot see.
	if containsEncodedSeparator(r.URL.EscapedPath()) {
		http.Error(w, "bad request: encoded path separators not allowed", http.StatusBadRequest)
		return
	}
	name, rest, ok := parseAgentPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := contain.ValidateName(name); err != nil {
		http.Error(w, fmt.Sprintf("invalid agent name %q", name), http.StatusBadRequest)
		return
	}
	if hasDotDotSegment(rest) {
		http.Error(w, "bad request: path traversal not allowed", http.StatusBadRequest)
		return
	}
	web, err := g.resolve(name)
	if err != nil {
		http.Error(w, fmt.Sprintf("unknown agent %q", name), http.StatusNotFound)
		return
	}
	if web.Token == "" {
		http.Error(w, fmt.Sprintf("agent %q has no provisioned web token", name), http.StatusBadGateway)
		return
	}

	t := &proxyTarget{
		name:       name,
		host:       net.JoinHostPort(g.host, web.Port),
		targetPath: joinBasePath(web.BasePath, rest),
		token:      web.Token,
		basePath:   strings.TrimRight(web.BasePath, "/"),
		mount:      "/agent/" + name,
		rebaseUI:   true,
	}
	g.serveProxy(w, r, t)
}

// serveProxy threads the per-request target onto the context and hands the
// request to the single shared reverse proxy. Both the agent proxy and the
// widget proxy funnel through here so they share one connection pool, one
// Transport, and one set of Rewrite/ModifyResponse/ErrorHandler hooks.
func (g *gateway) serveProxy(w http.ResponseWriter, r *http.Request, t *proxyTarget) {
	g.proxy.ServeHTTP(w, r.WithContext(withProxyTarget(r.Context(), t)))
}

// handleWidgetProxy serves the widget origin: it routes /w/<agent>/<mount>/*
// to that agent's serve at {BasePath}/widgets/<mount>/*, injecting the agent's
// bearer token server-side (via the shared Rewrite hook). It deliberately
// proxies ONLY the widgets subtree — never /v1, the session API, or anything
// else same-origin-sensitive. Responses are passed through without the SPA
// prefix rebasing: widgets resolve against this origin via relative URLs (a
// widget that builds absolute URLs from BASE_PATH/X-Forwarded-Prefix would point
// at the agent-internal /chat/widgets/<mount> path, which 404s here).
//
// This mirrors the agent serve's own widget proxy (cmd/serve_widgets.go
// handleWidgetProxy) but for a different mount shape and across the origin
// boundary; keep the two mount-parse/trailing-slash behaviors in sync.
//
// NOTE: there is no access control here yet — like the agent gateway, this is
// loopback-only until the Part 2 HMAC access grant lands.
func (g *gateway) handleWidgetProxy(w http.ResponseWriter, r *http.Request) {
	// Same encoded-separator guard as the agent proxy: r.URL.Path is decoded,
	// so reject %2f/%5c before the segment checks can be bypassed.
	if containsEncodedSeparator(r.URL.EscapedPath()) {
		http.Error(w, "bad request: encoded path separators not allowed", http.StatusBadRequest)
		return
	}
	agent, mount, rest, ok := parseWidgetPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := contain.ValidateName(agent); err != nil {
		http.Error(w, fmt.Sprintf("invalid agent name %q", agent), http.StatusBadRequest)
		return
	}
	if !widgets.ValidMount(mount) {
		http.Error(w, fmt.Sprintf("invalid widget mount %q", mount), http.StatusBadRequest)
		return
	}
	// Bare mount with no trailing slash: redirect on the widget origin so the
	// Location stays here and the widget's relative asset URLs resolve correctly.
	// Preserve any query string so a widget that reads its initial config from
	// query params still receives them.
	if rest == "" {
		target := "/w/" + agent + "/" + mount + "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}
	if hasDotDotSegment(rest) {
		http.Error(w, "bad request: path traversal not allowed", http.StatusBadRequest)
		return
	}
	web, err := g.resolve(agent)
	if err != nil {
		http.Error(w, fmt.Sprintf("unknown agent %q", agent), http.StatusNotFound)
		return
	}
	if web.Token == "" {
		http.Error(w, fmt.Sprintf("agent %q has no provisioned web token", agent), http.StatusBadGateway)
		return
	}

	t := &proxyTarget{
		name:       agent,
		host:       net.JoinHostPort(g.host, web.Port),
		targetPath: joinBasePath(web.BasePath, "/widgets/"+mount+rest),
		token:      web.Token,
		basePath:   strings.TrimRight(web.BasePath, "/"),
		mount:      "/w/" + agent + "/" + mount,
		rebaseUI:   false,
	}
	g.serveProxy(w, r, t)
}

// parseWidgetPath splits "/w/<agent>/<mount>/<rest>" into its parts. <rest>
// includes its leading slash; "/w/<agent>/<mount>" yields an empty rest (the
// caller redirects to add the trailing slash). A path missing the mount segment
// is not ok.
func parseWidgetPath(p string) (agent, mount, rest string, ok bool) {
	const prefix = "/w/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", "", false
	}
	tail := p[len(prefix):]
	slash := strings.IndexByte(tail, '/')
	if slash < 0 {
		return "", "", "", false // "/w/<agent>" with no mount
	}
	agent, tail = tail[:slash], tail[slash+1:]
	if agent == "" {
		return "", "", "", false
	}
	if slash = strings.IndexByte(tail, '/'); slash >= 0 {
		mount, rest = tail[:slash], tail[slash:]
	} else {
		mount, rest = tail, ""
	}
	if mount == "" {
		return "", "", "", false
	}
	return agent, mount, rest, true
}

// rewriteProxyRequest is the shared ReverseProxy Rewrite hook. Using Rewrite (vs
// the legacy Director) means ReverseProxy does NOT auto-append X-Forwarded-*; we
// also explicitly drop any client-supplied forwarding/credential headers so they
// can neither spoof metadata nor reach the backend.
func rewriteProxyRequest(pr *httputil.ProxyRequest) {
	t := proxyTargetFrom(pr.In.Context())
	if t == nil {
		return
	}
	out := pr.Out
	out.URL.Scheme = "http"
	out.URL.Host = t.host
	out.URL.Path = t.targetPath
	out.URL.RawPath = "" // force Go to re-encode Path from the cleaned value
	out.Host = t.host

	// Inject the real per-agent token server-side; strip every client-supplied
	// credential the backend would otherwise honor (Authorization Bearer,
	// x-api-key, and the term_llm_token cookie).
	out.Header.Set("Authorization", "Bearer "+t.token)
	out.Header.Del("Cookie")
	out.Header.Del("X-Api-Key")

	// Take ownership of response encoding so ModifyResponse sees decompressed
	// HTML: with Accept-Encoding cleared, the Transport transparently negotiates
	// and decodes gzip itself.
	out.Header.Del("Accept-Encoding")

	// Drop spoofable forwarding metadata; the gateway does not advertise itself
	// as an upstream prefix to the backend.
	out.Header.Del("X-Forwarded-For")
	out.Header.Del("X-Forwarded-Host")
	out.Header.Del("X-Forwarded-Proto")
	out.Header.Del("X-Forwarded-Prefix")
	out.Header.Del("Forwarded")
}

// rebaseProxyResponse rewrites the agent's baked-in /chat prefix onto the
// gateway mount (/agent/<name>) for the HTML document, and fixes redirect
// Location headers. Because the SPA derives every URL it builds from the single
// window.TERM_LLM_UI_PREFIX value and the <base> tag, rebasing those two strings
// re-homes all subsequent requests onto /agent/<name>/* where the token is
// injected — with zero JS changes. Non-HTML bodies (JS, JSON, SSE, images) are
// passed through untouched.
func rebaseProxyResponse(resp *http.Response) error {
	t := proxyTargetFrom(resp.Request.Context())
	if t == nil || !t.rebaseUI || t.basePath == "" || t.basePath == t.mount {
		return nil
	}

	rewriteLocationHeader(resp, t)

	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil
	}
	// A HEAD response carries the real Content-Length but no body. Rewriting it
	// would clobber that length to 0 and fire a spurious drift warning, so leave
	// it untouched.
	if resp.Request.Method == http.MethodHead {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if len(body) == 0 {
		// Empty text/html body (e.g. a needle-less fragment): nothing to rewrite.
		// Restore the body and skip the rewrite so we neither clobber
		// Content-Length nor log a false drift warning.
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	rewritten, baseHits, prefixHits := rebaseUIPrefix(body, t.basePath, t.mount)
	if baseHits == 0 || prefixHits == 0 {
		// One of the two prefix tokens the SPA bakes into its HTML no longer
		// matches our needle (serveui's emitted shape drifted). Click-to-open
		// silently breaks when this happens, so make it loud rather than
		// failing the response.
		log.Printf("WARNING: gateway agent %q: UI prefix rebase matched base=%d prefix=%d (expected >=1 each); click-to-open may be broken — check serveui's <base>/TERM_LLM_UI_PREFIX shape",
			t.name, baseHits, prefixHits)
	}
	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	// The Transport already decoded any gzip (we cleared Accept-Encoding); make
	// sure no stale encoding header survives the body rewrite.
	resp.Header.Del("Content-Encoding")
	return nil
}

// rebaseUIPrefix replaces the two prefix tokens the agent bakes into its served
// HTML. The needles are built with the SAME html.EscapeString / json.Marshal
// shapes the serve uses (internal/serveui/embed.go and cmd/serve_handlers.go) so
// they match byte-for-byte regardless of the agent's actual base path.
// It reports how many times each needle matched so the caller can warn loudly
// if either matched zero times (a sign serveui's emitted shape has drifted and
// click-to-open has silently broken).
func rebaseUIPrefix(body []byte, basePath, mount string) (out []byte, baseHits, prefixHits int) {
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

// rewriteLocationHeader rebases a root-relative redirect Location that points at
// the agent base path onto the gateway mount, so backend redirects (e.g. the
// /chat -> /chat/ normalization) land back inside /agent/<name>/.
func rewriteLocationHeader(resp *http.Response, t *proxyTarget) {
	loc := resp.Header.Get("Location")
	if loc == "" {
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

func proxyErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	name := "agent"
	if t := proxyTargetFrom(r.Context()); t != nil {
		name = t.name
	}
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintf(w, "gateway: agent %q backend unreachable: %v\n", name, err)
}

// containsEncodedSeparator reports whether an escaped path smuggles an encoded
// path separator (%2f) or backslash (%5c).
func containsEncodedSeparator(escapedPath string) bool {
	lower := strings.ToLower(escapedPath)
	return strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c")
}

// hasDotDotSegment reports whether any segment of the decoded path is "..".
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// parseAgentPath splits "/agent/<name>/<rest>" into name and the remainder
// (including its leading slash). "/agent/<name>" yields an empty rest.
func parseAgentPath(p string) (name, rest string, ok bool) {
	const prefix = "/agent/"
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

// joinBasePath joins an agent's mount base path with the proxied remainder,
// collapsing the slash seam. An empty remainder targets the base path root.
func joinBasePath(base, rest string) string {
	base = strings.TrimRight(base, "/")
	if rest == "" {
		return base + "/"
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return base + rest
}

var (
	serveGatewayHost       string
	serveGatewayPort       int
	serveGatewayAgentHost  string
	serveGatewayWidgetHost string
)

var serveGatewayCmd = &cobra.Command{
	Use:   "serve-gateway",
	Short: "Run a multi-agent gateway fronting per-agent contain serves (experimental)",
	Long: `Run a thin multi-agent gateway that fronts the per-agent web serves of
contain workspaces.

A term-llm serve binds a single agent at startup, so multi-agent access is
achieved by fronting many serves rather than routing within one process. The
gateway discovers each agent's published port and bearer token from its contain
workspace .env and injects the token server-side, so per-agent tokens are never
exposed to the client.

Routes (agent origin):
  GET  /agents              list discoverable agents (never includes tokens)
  ANY  /agent/<name>/...     reverse proxy to that agent's serve

When --widget-host is set, the gateway also answers on that dedicated origin,
serving ONLY the widget subtree so a widget iframe is isolated to a throwaway
origin that hosts nothing sensitive:
  ANY  /w/<agent>/<mount>/... reverse proxy to the agent's widget (widget origin)

EXPERIMENTAL: gateway-level authentication and the widget access grant are not
yet implemented. Bind to loopback only.`,
	Args: cobra.NoArgs,
	RunE: runServeGateway,
}

// validateGatewayBind rejects a bind that would expose the gateway unsafely.
// The gateway has no authentication of its own yet (per-agent tokens are
// injected server-side, but nothing gates who may reach the gateway), so it
// must stay on a loopback interface. To serve it publicly, front it with a
// real authenticating reverse proxy and keep the gateway itself on loopback.
func validateGatewayBind(host string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid --port %d (must be 1-65535)", port)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("serve-gateway has no authentication yet, so --host must be a loopback address (127.0.0.1, localhost, or ::1); got %q. To expose it, put an authenticating proxy in front and keep the gateway on loopback", host)
	}
	return nil
}

// widgetHostWarning returns a startup warning when the widget host is
// misconfigured for the gateway's loopback-only posture, or "" when it resolves
// to loopback addresses only. It flags two cases: the host does not resolve (so
// a browser can't reach widgets — purely an ergonomics hint), or it resolves to
// a non-loopback address. The latter is only a heads-up, not a hole: the gateway
// still binds loopback (validateGatewayBind) and routes by Host header, so an
// off-host client cannot reach it regardless of what the name resolves to.
func widgetHostWarning(host string, lookup func(string) ([]string, error)) string {
	addrs, err := lookup(host)
	if err != nil || len(addrs) == 0 {
		return fmt.Sprintf("widget host %q does not resolve; add %q to your hosts file or DNS (e.g. \"127.0.0.1 %s\") so widgets open in the browser", host, host, host)
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Sprintf("widget host %q resolves to a non-loopback address (%s); the gateway has no authentication and binds loopback only, so point %q at 127.0.0.1/::1", host, a, host)
		}
	}
	return ""
}

// defaultLookupHost resolves a hostname with a short timeout so a slow resolver
// cannot stall gateway startup.
func defaultLookupHost(host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupHost(ctx, host)
}

// validateWidgetHost rejects a widget host that would shadow the agent gateway.
// The agent origin is served on loopback, so a widget host that is itself a
// loopback literal (localhost/127.0.0.1/::1) matches the same requests and the
// widgets-only mux would swallow /agents, /agent/*, and the landing page. The
// widget host must be a distinct hostname, e.g. widgets.localhost.
func validateWidgetHost(widgetHost string) error {
	widgetHost = strings.TrimSpace(widgetHost)
	if widgetHost == "" {
		return nil
	}
	if isLoopbackHost(strings.ToLower(widgetHost)) {
		return fmt.Errorf("--widget-host %q must be a distinct hostname, not a loopback literal (it would shadow the agent gateway); use e.g. widgets.localhost", widgetHost)
	}
	return nil
}

func runServeGateway(cmd *cobra.Command, args []string) error {
	if err := validateGatewayBind(serveGatewayHost, serveGatewayPort); err != nil {
		return err
	}
	if err := validateWidgetHost(serveGatewayWidgetHost); err != nil {
		return err
	}
	g := newGateway()
	if strings.TrimSpace(serveGatewayAgentHost) != "" {
		g.host = serveGatewayAgentHost
	}
	g.widgetHost = strings.TrimSpace(serveGatewayWidgetHost)

	addr := net.JoinHostPort(serveGatewayHost, strconv.Itoa(serveGatewayPort))
	srv := &http.Server{Addr: addr, Handler: g.handler()}

	fmt.Fprintf(cmd.OutOrStdout(), "term-llm multi-agent gateway listening on http://%s\n", addr)
	fmt.Fprintf(cmd.OutOrStdout(), "  GET http://%s/agents\n", addr)
	fmt.Fprintf(cmd.OutOrStdout(), "  ANY http://%s/agent/<name>/...\n", addr)
	if g.widgetHost != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "  ANY http://%s:%d/w/<agent>/<mount>/... (widget origin)\n", g.widgetHost, serveGatewayPort)
		if warn := widgetHostWarning(g.widgetHost, defaultLookupHost); warn != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: %s\n", warn)
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), "WARNING: experimental, no gateway auth or widget grant yet; bind to loopback only.")
	return srv.ListenAndServe()
}

func init() {
	rootCmd.AddCommand(serveGatewayCmd)
	serveGatewayCmd.Flags().StringVar(&serveGatewayHost, "host", "127.0.0.1", "Bind host")
	serveGatewayCmd.Flags().IntVar(&serveGatewayPort, "port", 8090, "Bind port")
	serveGatewayCmd.Flags().StringVar(&serveGatewayAgentHost, "agent-host", "127.0.0.1", "Host the per-agent serves are published on")
	serveGatewayCmd.Flags().StringVar(&serveGatewayWidgetHost, "widget-host", "widgets.localhost", "Dedicated origin for the widgets-only proxy (empty to disable)")
}
