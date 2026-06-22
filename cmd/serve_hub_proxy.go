package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	htmlpkg "html"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/hub"
)

const hubHTMLRebaseMaxBytes = 4 << 20

type hubProxyTarget struct {
	nodeID   string // node ID (for diagnostics and hub context)
	nodeName string // node display name (for hub context)
	scheme   string // backend scheme (http or https)
	host     string // backend host:port
	path     string // backend path: node base path + remainder
	token    string // per-node bearer token, injected server-side
	basePath string // node's baked-in prefix, e.g. /chat ("" when root)
	mount    string // hub-facing prefix, e.g. /hub/node/<id>
	hubURL   string // hub-facing dashboard URL, e.g. /hub/
}

type hubProxyTargetKey struct{}

func withHubProxyTarget(ctx context.Context, t *hubProxyTarget) context.Context {
	return context.WithValue(ctx, hubProxyTargetKey{}, t)
}

func hubProxyTargetFrom(ctx context.Context) *hubProxyTarget {
	t, _ := ctx.Value(hubProxyTargetKey{}).(*hubProxyTarget)
	return t
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
	// Bare /node/<id> -> /node/<id>/ (including any external hub base path)
	// so the node UI's relative URLs resolve under the mount. Preserve the query
	// string.
	if rest == "" {
		target := s.hubPath("/node/" + id + "/")
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
		mount:    s.hubNodeMount(node.ID),
		hubURL:   s.hubPath("/"),
	}
	s.proxy.ServeHTTP(w, r.WithContext(withHubProxyTarget(r.Context(), t)))
}

func (s *hubServer) handleReverseNodeProxy(w http.ResponseWriter, r *http.Request, node hub.Node, rest string) {
	if !s.reverse.isConnected(node.ID) {
		http.Error(w, fmt.Sprintf("node %q reverse connection is not connected", node.ID), http.StatusBadGateway)
		return
	}
	if rest == "" {
		target := s.hubPath("/node/" + node.ID + "/")
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
		mount:    s.hubNodeMount(node.ID),
		hubURL:   s.hubPath("/"),
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
	hubStripHopByHopHeaders(resp.Header)
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func hubStripHopByHopHeaders(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				h.Del(token)
			}
		}
	}
	for _, key := range []string{
		"Connection",
		"Proxy-Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		h.Del(key)
	}
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

// hubRewriteProxyRequest is the ReverseProxy Rewrite hook. Using Rewrite means
// ReverseProxy does NOT auto-append X-Forwarded-*; we also explicitly drop
// client-supplied forwarding/credential headers so they can neither spoof
// metadata nor reach the node.
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

type hubPrefixReadCloser struct {
	reader io.Reader
	closer io.Closer
}

func (r hubPrefixReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r hubPrefixReadCloser) Close() error {
	return r.closer.Close()
}

func hubReadHTMLBodyForRebase(body io.ReadCloser) (data []byte, overLimit bool, err error) {
	data, err = io.ReadAll(io.LimitReader(body, hubHTMLRebaseMaxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > hubHTMLRebaseMaxBytes {
		return data, true, nil
	}
	return data, false, nil
}

// hubRebaseProxyResponse rewrites the node's baked-in base path onto the hub
// mount (/node/<id>, or /<base-path>/node/<id> when mounted under a prefix)
// for HTML documents, fixes redirect Location headers, and injects the
// window.TERM_LLM_HUB context so the node UI can render its "Back to Hub" link.
// Because the SPA derives every URL it builds from the single
// window.TERM_LLM_UI_PREFIX value and the <base> tag, rebasing those two
// strings re-homes all subsequent requests onto the hub node mount where the
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
	originalBody := resp.Body
	body, overLimit, err := hubReadHTMLBodyForRebase(originalBody)
	if err != nil {
		return err
	}
	if overLimit {
		log.Printf("WARNING: hub node %q: HTML response exceeded %d bytes; serving without hub rebase/injection", t.nodeID, hubHTMLRebaseMaxBytes)
		resp.Body = hubPrefixReadCloser{
			reader: io.MultiReader(bytes.NewReader(body), originalBody),
			closer: originalBody,
		}
		return nil
	}
	originalBody.Close()
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
// to Hub" link. The hub URL is root-relative because the proxied UI is
// same-origin with the hub; it includes the hub mount when served under a
// prefix such as /hub.
func hubInjectContext(body []byte, t *hubProxyTarget) []byte {
	hubURL := t.hubURL
	if hubURL == "" {
		hubURL = "/"
	}
	ctxJSON, err := json.Marshal(map[string]string{
		"url":      hubURL,
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

// hubRewriteLocationHeader rebases redirects back inside the hub mount. The
// common node redirects are root-relative under the node base path (e.g.
// /chat -> /chat/), but some handlers emit absolute URLs to their own origin.
// Root-relative redirects outside the configured base path cannot be represented
// exactly through v1 path-based proxying; keep them inside the node mount rather
// than leaking/bouncing to the hub origin.
func hubRewriteLocationHeader(resp *http.Response, t *hubProxyTarget) {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return
	}
	u, err := url.Parse(loc)
	if err != nil {
		return
	}
	if u.IsAbs() {
		if t.scheme == "" || t.host == "" || !strings.EqualFold(u.Scheme, t.scheme) || !strings.EqualFold(u.Host, t.host) {
			return
		}
		u.Scheme = ""
		u.Host = ""
		u.User = nil
		u.Path = hubRebaseLocationPath(u.Path, t)
		u.RawPath = ""
		resp.Header.Set("Location", u.String())
		return
	}
	if strings.HasPrefix(loc, "/") {
		u.Path = hubRebaseLocationPath(u.Path, t)
		u.RawPath = ""
		resp.Header.Set("Location", u.String())
	}
}

func hubRebaseLocationPath(path string, t *hubProxyTarget) string {
	if path == "" || !strings.HasPrefix(path, "/") {
		return path
	}
	if t.basePath != "" {
		if path == t.basePath {
			return t.mount
		}
		if strings.HasPrefix(path, t.basePath+"/") {
			return t.mount + path[len(t.basePath):]
		}
	}
	if path == "/" {
		return t.mount + "/"
	}
	return t.mount + path
}

func hubProxyErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	id := "node"
	if t := hubProxyTargetFrom(r.Context()); t != nil {
		id = t.nodeID
	}
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintf(w, "hub: node %q backend unreachable: %v\n", id, err)
}
