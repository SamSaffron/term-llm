package serve

import (
	"net/http"
	"strings"

	appserver "github.com/samsaffron/term-llm/internal/app/serve/server"
)

// peerHandler is a private ingress, never mounted on the HTTP listener. Its
// caller must verify the SDP identity/fingerprint binding before attaching a
// DataChannel. authorized must consult the persisted grant on EVERY request;
// possession of a once-open channel is not an authorization decision.
//
// Initial grants permit Hub reads/attention and chat on the approved nodes, not
// Hub credential management, node registration, or arbitrary proxy targets.
func (s *hubServer) peerHandler(authorized func(*http.Request) bool, nodeAllowed func(string) bool) http.Handler {
	routes := s.routes()
	ingress := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if authorized == nil || !authorized(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_peer_grant", "paired device authorization expired or revoked")
			return
		}
		p := r.URL.Path
		if appserver.ContainsEncodedSeparator(r.URL.EscapedPath()) || appserver.HasDotDotSegment(p) || strings.ContainsAny(p, "\\\x00") || r.URL.Query().Has("token") {
			http.Error(w, "invalid peer request path", http.StatusBadRequest)
			return
		}
		// These values are meaningful only at the public HTTP ingress. A paired
		// peer cannot provide node credentials, browser sessions, or forwarding
		// authority. Its local bridge performs browser-origin checks independently.
		r = r.Clone(r.Context())
		r.Header = r.Header.Clone()
		for key := range r.Header {
			lower := strings.ToLower(key)
			if lower == "authorization" || lower == "cookie" || lower == "origin" || lower == "forwarded" || lower == "x-term-llm-node-id" || strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "sec-fetch-") || lower == "connection" || lower == "upgrade" {
				r.Header.Del(key)
			}
		}
		r.Host = "peer.term-llm.invalid"
		switch {
		case p == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
			// Authentication is the approved, fingerprint-bound peer session, not a
			// master-token cookie or an HTTP WebAuthn ceremony at the loopback origin.
			s.writeHubShell(w, r, http.StatusOK, "Hub - term-llm", hubPageConfig{
				Page: "dashboard", AuthMode: "none", BasePath: s.basePath,
				CacheDisplayAllowed: false, FormAction: s.publicPath("/"),
			})
		case p == "/api/auth/session" && r.Method == http.MethodGet:
			writeHubAuthJSON(w, http.StatusOK, map[string]any{"administrator": "Paired device", "session": map[string]any{"transport": "webrtc"}, "recently_authenticated": false, "active_sessions": 1})
		case p == "/api/registration-info" && r.Method == http.MethodGet:
			// Registration credentials must never escape through this ingress.
			writeHubAuthJSON(w, http.StatusOK, hubRegistrationInfo{})
		case p == "/healthz" && r.Method == http.MethodGet,
			p == "/api/nodes" && r.Method == http.MethodGet,
			p == "/api/attention" && r.Method == http.MethodGet,
			p == "/api/delegations" && r.Method == http.MethodGet,
			(p == "/api/attention/seen" || p == "/api/attention/clear") && r.Method == http.MethodPost,
			hubPublicAssetRoute(p) && (r.Method == http.MethodGet || r.Method == http.MethodHead):
			routes.ServeHTTP(w, r)
		case strings.HasPrefix(p, "/node/"):
			id, rest, ok := parseHubNodePath(p)
			if !ok || nodeAllowed == nil || !nodeAllowed(id) || !peerChatRoute(r.Method, rest) {
				w.Header().Set("X-Term-LLM-Peer-Error", "scope_denied")
				http.Error(w, "node outside paired grant", http.StatusForbidden)
				return
			}
			if r.Header.Get("Sec-WebSocket-Key") != "" {
				http.Error(w, "peer websocket transport is not supported", http.StatusNotImplemented)
				return
			}
			routes.ServeHTTP(w, r)
		default:
			w.Header().Set("X-Term-LLM-Peer-Error", "scope_denied")
			http.Error(w, "route outside paired grant", http.StatusForbidden)
		}
	})
	return s.mountBasePath(ingress)
}
