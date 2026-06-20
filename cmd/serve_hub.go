package cmd

import (
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
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

	requireAuth       bool
	token             string
	registrationToken string
}

func newHubServer(registry *hub.Registry, store *hub.Store) *hubServer {
	transport := newHubTransport()
	s := &hubServer{
		registry:      registry,
		store:         store,
		prober:        hub.NewProber(transport),
		reverse:       newHubReverseManager(),
		nodeAPIClient: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: hubDoNotFollowRedirects},
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

func hubDoNotFollowRedirects(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}
