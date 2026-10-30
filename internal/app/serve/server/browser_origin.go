package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/samsaffron/term-llm/internal/app/serve/wire"
)

// GuardNoAuthBrowser prevents browser-simple requests from exercising loopback
// authority in no-auth mode. CORS controls response access, not whether a remote
// page can send a request. Authenticated modes keep their existing checks.
// trustedURLs must come from operator-configured public/Hub URLs, not headers.
func GuardNoAuthBrowser(next http.Handler, noAuth bool, corsOrigins []string, trustedURLs ...string) http.Handler {
	if !noAuth {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !browserMutationAllowed(r, corsOrigins, trustedURLs...) {
			wire.WriteOpenAIError(w, http.StatusForbidden, "invalid_origin", "cross-origin requests require an explicit --cors-origin in no-auth mode")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// browserMutationAllowed checks browser signals before exercising unauthenticated
// loopback authority. It allows safe methods, explicitly configured CORS origins,
// same-origin browser traffic, and non-browser clients without browser signals.
// This is not an authentication boundary against other local processes.
func browserMutationAllowed(r *http.Request, corsOrigins []string, trustedURLs ...string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	// Cross-origin clients remain supported when the operator explicitly
	// opts in. A wildcard deliberately grants this authority to every origin.
	if origin != "" {
		for _, allowed := range corsOrigins {
			if allowed = strings.TrimSpace(allowed); allowed == "*" || allowed == origin {
				return true
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "cross-site", "same-site":
		return false
	case "same-origin":
		// Like shell requests, Hub/proxy requests retain the browser-owned
		// metadata even when the backend Host differs from the UI origin.
		return true
	}
	if origin == "" {
		return true // Non-browser clients do not send either browser signal.
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if strings.EqualFold(origin, scheme+"://"+r.Host) {
		return true
	}
	// Older browsers may omit Fetch Metadata. Use only configured authority
	// for named reverse proxies, never client-supplied forwarded headers.
	for _, configured := range trustedURLs {
		u, err := url.Parse(configured)
		if err == nil && u.Host != "" && strings.EqualFold(origin, u.Scheme+"://"+u.Host) {
			return true
		}
	}
	return false
}
