package serve

import (
	"bytes"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"

	"github.com/samsaffron/term-llm/internal/app/serve/wire"
)

// serveIndexHTML serves the SPA shell. Authorized, non-safe-mode documents link
// the enabled extension stylesheets in <head> so they apply before first paint;
// frontend/src/api/extensions.ts adopts those links instead of adding copies.
func (s *serveServer) serveIndexHTML(w http.ResponseWriter, r *http.Request) {
	const contentType, cacheControl = "text/html; charset=utf-8", "no-cache, no-store, must-revalidate"
	page, links := s.renderIndexHTML(), ""
	if s.extensions != nil && r.URL.Query().Get("safe-mode") != "1" && s.requestAuthorized(r) {
		links = s.extensions.manager.StylesheetLinks()
	}
	if links == "" || !bytes.Contains(page, []byte("</head>")) {
		serveEmbeddedUIBytes(w, r, page, contentType, cacheControl, false)
		return
	}
	// Splice into a copy of the shared cached shell. Variants change with each
	// extension generation, so they stay out of the process-lifetime uiAssetCache.
	w.Header().Set("Cache-Control", cacheControl)
	wire.WriteGzipBody(w, r, http.StatusOK, contentType, bytes.Replace(page, []byte("</head>"), []byte(links+"</head>"), 1))
}

// requestAuthorized reports whether r would pass s.auth.
func (s *serveServer) requestAuthorized(r *http.Request) bool {
	if s.browserAuth != nil {
		p, ok := hubPrincipal(r)
		return ok && p.SessionID != ""
	}
	return !s.cfg.requireAuth || s.bearerTokenValid(r)
}

// bearerTokenValid checks the configured token from the Authorization header,
// x-api-key, or (where cookieAuthAllowed permits) the term_llm_token cookie.
func (s *serveServer) bearerTokenValid(r *http.Request) bool {
	var gotToken string
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if scheme, rest, ok := strings.Cut(auth, " "); ok && strings.EqualFold(scheme, "Bearer") {
		gotToken = strings.TrimSpace(rest)
	}
	if gotToken == "" {
		gotToken = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if gotToken == "" && cookieAuthAllowed(r) {
		// Cookie fallback is needed for browser-initiated requests that cannot set
		// Authorization headers. Keep it GET-only for normal API/UI routes, but
		// allow all widget methods: widget iframe fetches include same-site cookies,
		// and the widget proxy strips Cookie before forwarding to the app.
		if cookie, err := r.Cookie("term_llm_token"); err == nil && cookie.Value != "" {
			if decoded, decErr := url.QueryUnescape(cookie.Value); decErr == nil {
				gotToken = decoded
			} else {
				gotToken = cookie.Value
			}
		}
	}
	return gotToken != "" && subtle.ConstantTimeCompare([]byte(gotToken), []byte(s.cfg.token)) == 1
}
