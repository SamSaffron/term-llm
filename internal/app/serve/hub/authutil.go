package hub

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AuthCookieName carries the Hub bearer token for browser sessions.
const AuthCookieName = "term_llm_hub_token"

// ExpectedOriginKey carries the configured WebAuthn origin in a request context.
type ExpectedOriginKey struct{}

func PublicAssetRoute(path string) bool {
	return path == "/dist/hub.js" || path == "/dist/hub.css"
}

func delegationPath(path string) bool {
	return path == "/api/delegations" || strings.HasPrefix(path, "/api/delegations/")
}

// NodeAuthRoute reports requests that skip Hub operator authentication
// because their handler authenticates the calling node instead: the reverse
// websocket, node-originated delegation calls, and a node saving the
// dashboard order (PATCH only). Bearer and passkey auth share this list.
func NodeAuthRoute(r *http.Request) bool {
	if r.URL.Path == "/api/connect" || NodeOrderNodeRequest(r) {
		return true
	}
	return delegationPath(r.URL.Path) && NodeIDPresented(r)
}

func DelegationOperatorRoute(r *http.Request) bool {
	return delegationPath(r.URL.Path) && !NodeIDPresented(r)
}

func BearerTokenMatches(r *http.Request, want string) bool {
	if TokenMatches(strings.TrimSpace(want), BearerTokenFromHeader(r)) {
		return true
	}
	if c, err := r.Cookie(AuthCookieName); err == nil && TokenMatches(strings.TrimSpace(want), c.Value) {
		return true
	}
	return false
}

func QueryTokenMatches(r *http.Request, want string) bool {
	return TokenMatches(strings.TrimSpace(want), r.URL.Query().Get("token"))
}

func QueryTokenSupplied(r *http.Request) bool {
	_, ok := r.URL.Query()["token"]
	return ok
}

// ShouldRenderLogin reports whether an unauthenticated request is a browser
// navigation that should get the login page rather than a JSON error.
func ShouldRenderLogin(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Mode")), "navigate") {
		return true
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "text/html")
}

func BearerTokenFromHeader(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, rest, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

// TokenMatches compares tokens in constant time over their hashes.
func TokenMatches(want, got string) bool {
	got = strings.TrimSpace(got)
	if got == "" || want == "" {
		return false
	}
	wantHash := sha256.Sum256([]byte(want))
	gotHash := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(wantHash[:], gotHash[:]) == 1
}

func SetAuthCookie(w http.ResponseWriter, token, path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    strings.TrimSpace(token),
		Path:     path,
		Expires:  time.Now().Add(365 * 24 * time.Hour),
		MaxAge:   365 * 24 * 60 * 60,
		SameSite: http.SameSiteStrictMode,
		HttpOnly: true,
	})
}

// BrowserRequestAllowed rejects cross-site browser requests before the hub
// exercises any token-injecting authority or mutates its node registry. This
// is defense-in-depth for --auth none and bearer-authenticated browser use.
// Requests without Origin and Sec-Fetch-Site are allowed for non-browser
// clients; public hubs rely on bearer auth as the primary boundary for those.
func BrowserRequestAllowed(r *http.Request, requireJSON bool) bool {
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
	if expected, ok := r.Context().Value(ExpectedOriginKey{}).(string); ok && expected != "" {
		return strings.EqualFold(origin, expected)
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
