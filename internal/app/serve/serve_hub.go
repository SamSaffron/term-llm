package serve

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
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
	store *hub.Store
	// nodeOrder saves the dashboard's node order, which operators and nodes
	// change through PATCH /api/nodes/order. Nil lists nodes in registry
	// order and disables that endpoint.
	nodeOrder    *hub.NodeOrderStore
	nodeOrderLog hubRepeatLog
	prober       *hub.Prober
	// reverse tracks private nodes that dial out to the hub and receive node
	// requests over a websocket instead of direct hub -> node HTTP.
	reverse *hubReverseManager
	proxy   *httputil.ReverseProxy
	// basePath is the external mount prefix (for example /hub) when the hub is
	// served behind a reverse proxy. Empty means root-mounted.
	basePath string
	// delegations is the cross-node delegation ledger; nil disables the
	// /api/delegations endpoints.
	delegations          *hub.DelegationStore
	delegationRefreshSeq atomic.Uint64
	// nodeAPIClient performs hub -> node jobs API calls for delegations. It
	// shares the proxy's direct-dial transport (no env proxy: requests carry
	// node tokens) but, unlike streaming proxy traffic, gets a whole-request
	// timeout.
	nodeAPIClient  *http.Client
	healthCache    hubHealthCache
	healthCancel   context.CancelFunc
	healthWG       sync.WaitGroup
	attentionStore *hub.AttentionProjectionStore
	// changes wakes /api/changes long-polls on attention/delegation writes.
	changes              *hub.ChangeFeed
	attentionCancel      context.CancelFunc
	attentionWG          sync.WaitGroup
	attentionMu          sync.Mutex // guards projection replacement against completed acknowledgements
	attentionGeneration  uint64
	attentionDiagnostics hubAttentionDiagnostics
	attentionSchedule    hubAttentionSchedule

	requireAuth       bool
	authMode          string
	token             string
	registrationToken string
	passkey           *hubPasskeyRuntime
}

func newHubServer(registry *hub.Registry, store *hub.Store) *hubServer {
	if registry == nil {
		registry = hub.NewRegistry()
	}
	transport := newHubTransport()
	s := &hubServer{
		registry:      registry,
		store:         store,
		prober:        hub.NewProber(transport),
		reverse:       newHubReverseManager(),
		changes:       hub.NewChangeFeed(),
		nodeAPIClient: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: hubDoNotFollowRedirects},
	}
	s.reverse.registry = registry
	registry.Observe(s.reverse.reconcile)
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        hubRewriteProxyRequest,
		ModifyResponse: hubRebaseProxyResponse,
		ErrorHandler:   hubProxyErrorHandler,
		Transport:      transport,
	}
	return s
}

// publicPath returns a root-relative path as seen by browsers. Internal hub
// handlers run on stripped paths when --base-path is set, but any redirect,
// dashboard link, or injected node UI prefix must include the external mount.
func (s *hubServer) publicPath(path string) string {
	return s.hubPath(path)
}

// publicURLString rebases an internal URL (normally from a stripped request)
// onto the browser-visible hub mount, preserving query and fragment data.
func (s *hubServer) publicURLString(u *url.URL) string {
	return browserReturnURL(u, s.publicPath)
}

func browserReturnURL(u *url.URL, publicPath func(string) string) string {
	if u == nil {
		return publicPath("/")
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	out := publicPath(path)
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.EscapedFragment()
	}
	return out
}

// newHubTransport returns the proxy's HTTP transport with bounded connection
// and response-header timeouts so a hung node cannot tie up the hub. It
// deliberately sets NO whole-response deadline: long-lived streams (SSE)
// must stay open, so only connection establishment and
// response *headers* are bounded.
func newHubTransport() *http.Transport { return apphub.NewTransport() }

func hubDoNotFollowRedirects(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}
