package serve

import (
	"net/http"

	apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"
)

const hubAuthCookieName = apphub.AuthCookieName

func (s *hubServer) handler() http.Handler {
	return s.mountBasePath(s.auth(s.routes()))
}

// routes is shared by the HTTP listener and the separately authenticated peer
// ingress. The latter applies its own device grant and route restrictions.
func (s *hubServer) routes() http.Handler {
	mux := http.NewServeMux()
	if s.passkey != nil {
		s.browserAuth().registerPasskeyRoutes(mux)
	}
	mux.HandleFunc("/healthz", s.handleHubHealth)
	mux.HandleFunc("/dist/hub.js", s.handleHubAsset)
	mux.HandleFunc("/dist/hub.css", s.handleHubAsset)
	mux.HandleFunc("/api/nodes/test", s.handleTestNode)
	mux.HandleFunc("/api/registration-info", s.handleRegistrationInfo)
	mux.HandleFunc("/api/register-node/", s.handleRegisterNode)
	mux.HandleFunc("/api/register-node", s.handleRegisterNode)
	mux.HandleFunc(hubNodeOrderPath, s.handleNodesOrder)
	mux.HandleFunc("/api/nodes/", s.handleNodeItem)
	mux.HandleFunc("/api/nodes", s.handleNodes)
	mux.HandleFunc("/api/attention", s.handleHubAttention)
	mux.HandleFunc("/api/attention/clear", s.handleClearHubAttention)
	mux.HandleFunc("/api/attention/seen", s.handleSeenHubAttention)
	mux.HandleFunc("/api/delegations/", s.handleDelegationItem)
	mux.HandleFunc("/api/delegations", s.handleDelegations)
	mux.HandleFunc("/api/connect", s.handleReverseConnect)
	mux.HandleFunc("/node/", s.handleNodeProxy)
	mux.HandleFunc("/", s.handleIndex)
	return mux
}

func (s *hubServer) auth(next http.Handler) http.Handler {
	if s.authMode == "passkey" && s.passkey != nil {
		return s.browserAuth().passkeyAuth(next)
	}
	if !s.requireAuth {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || r.URL.Path == "/healthz" || hubPublicAssetRoute(r.URL.Path) || hubNodeAuthRoute(r) || hubRegistrationRoute(r) {
			next.ServeHTTP(w, r)
			return
		}
		if hubQueryTokenMatches(r, s.token) {
			setHubAuthCookie(w, r.URL.Query().Get("token"), s.publicPath("/"))
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				clean := *r.URL
				q := clean.Query()
				q.Del("token")
				clean.RawQuery = q.Encode()
				if clean.RawQuery == "" {
					clean.ForceQuery = false
				}
				http.Redirect(w, r, s.publicURLString(&clean), http.StatusFound)
				return
			}
		}
		if !hubBearerTokenMatches(r, s.token) {
			if hubShouldRenderLogin(r) {
				s.writeHubLoginPage(w, r, hubQueryTokenSupplied(r))
				return
			}
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

// Thin adapters keep existing cmd call sites on the extracted hub helpers.
func hubPublicAssetRoute(path string) bool { return apphub.PublicAssetRoute(path) }

// hubNodeAuthRoute reports requests whose handler authenticates the calling
// node instead of the Hub operator; see apphub.NodeAuthRoute.
func hubNodeAuthRoute(r *http.Request) bool { return apphub.NodeAuthRoute(r) }

func hubDelegationOperatorRoute(r *http.Request) bool { return apphub.DelegationOperatorRoute(r) }

func hubBearerTokenMatches(r *http.Request, want string) bool {
	return apphub.BearerTokenMatches(r, want)
}

func hubQueryTokenMatches(r *http.Request, want string) bool {
	return apphub.QueryTokenMatches(r, want)
}

func hubQueryTokenSupplied(r *http.Request) bool { return apphub.QueryTokenSupplied(r) }

func hubShouldRenderLogin(r *http.Request) bool { return apphub.ShouldRenderLogin(r) }

func (s *hubServer) writeHubLoginPage(w http.ResponseWriter, r *http.Request, invalid bool) {
	action := s.publicPath(r.URL.EscapedPath())
	if action == "" {
		action = s.publicPath("/")
	}
	s.writeHubShell(w, r, http.StatusUnauthorized, "Hub - term-llm", hubPageConfig{
		Page:         "bearer-login",
		AuthMode:     "bearer",
		BasePath:     s.basePath,
		InvalidToken: invalid,
		FormAction:   action,
	})
}

func bearerTokenFromHeader(r *http.Request) string { return apphub.BearerTokenFromHeader(r) }

func hubTokenMatches(want, got string) bool { return apphub.TokenMatches(want, got) }

func setHubAuthCookie(w http.ResponseWriter, token, path string) {
	apphub.SetAuthCookie(w, token, path)
}

// hubBrowserRequestAllowed rejects cross-site browser requests; see
// apphub.BrowserRequestAllowed.
func hubBrowserRequestAllowed(r *http.Request, requireJSON bool) bool {
	return apphub.BrowserRequestAllowed(r, requireJSON)
}
