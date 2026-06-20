package cmd

import (
	"crypto/sha256"
	"crypto/subtle"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const hubAuthCookieName = "term_llm_hub_token"

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
		if r.Method == http.MethodOptions || r.URL.Path == "/healthz" || hubNodeAuthRoute(r) {
			next.ServeHTTP(w, r)
			return
		}
		if hubQueryTokenMatches(r, s.token) {
			setHubAuthCookie(w, r.URL.Query().Get("token"))
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				clean := *r.URL
				q := clean.Query()
				q.Del("token")
				clean.RawQuery = q.Encode()
				if clean.RawQuery == "" {
					clean.ForceQuery = false
				}
				http.Redirect(w, r, clean.String(), http.StatusFound)
				return
			}
		}
		if !hubBearerTokenMatches(r, s.token) {
			if hubShouldRenderLogin(r) {
				writeHubLoginPage(w, r, hubQueryTokenSupplied(r))
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
	if hubTokenMatches(strings.TrimSpace(want), bearerTokenFromHeader(r)) {
		return true
	}
	if c, err := r.Cookie(hubAuthCookieName); err == nil && hubTokenMatches(strings.TrimSpace(want), c.Value) {
		return true
	}
	return false
}

func hubQueryTokenMatches(r *http.Request, want string) bool {
	return hubTokenMatches(strings.TrimSpace(want), r.URL.Query().Get("token"))
}

func hubQueryTokenSupplied(r *http.Request) bool {
	_, ok := r.URL.Query()["token"]
	return ok
}

func hubShouldRenderLogin(r *http.Request) bool {
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

var hubLoginTemplate = template.Must(template.New("hub-login").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>term-llm Hub</title>
  <style>
    :root { color-scheme: dark light; }
    body {
      margin: 0;
      min-height: 100vh;
      display: grid;
      place-items: center;
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      background: #0d1117;
      color: #e6edf3;
    }
    main {
      width: min(24rem, calc(100vw - 2rem));
      padding: 1.5rem;
      border: 1px solid rgba(255,255,255,0.12);
      border-radius: 16px;
      background: #161b22;
      box-shadow: 0 20px 60px rgba(0,0,0,0.35);
    }
    h1 { margin: 0 0 0.35rem; font-size: 1.35rem; }
    p { margin: 0 0 1rem; color: #8b949e; line-height: 1.45; }
    .error {
      margin-bottom: 1rem;
      padding: 0.65rem 0.75rem;
      border: 1px solid rgba(248,81,73,0.35);
      border-radius: 10px;
      background: rgba(248,81,73,0.10);
      color: #ffa198;
      font-size: 0.92rem;
    }
    label { display: block; margin-bottom: 0.45rem; font-weight: 650; }
    input {
      box-sizing: border-box;
      width: 100%;
      padding: 0.72rem 0.8rem;
      border-radius: 10px;
      border: 1px solid rgba(255,255,255,0.16);
      background: #0d1117;
      color: inherit;
      font: inherit;
    }
    button {
      width: 100%;
      margin-top: 0.9rem;
      padding: 0.72rem 0.8rem;
      border: 0;
      border-radius: 10px;
      background: #238636;
      color: white;
      font: inherit;
      font-weight: 750;
      cursor: pointer;
    }
    button:hover { background: #2ea043; }
    code { color: #a5d6ff; }
    @media (prefers-color-scheme: light) {
      body { background: #f6f8fa; color: #24292f; }
      main { background: #fff; border-color: rgba(27,31,36,0.15); box-shadow: 0 20px 60px rgba(27,31,36,0.12); }
      p { color: #57606a; }
      input { background: #fff; border-color: rgba(27,31,36,0.18); }
    }
  </style>
</head>
<body>
  <main>
    <h1>term-llm Hub</h1>
    <p>Enter your hub access token to continue. I’ll store it in an HTTP-only cookie on this host.</p>
    {{if .Invalid}}<div class="error">That hub token was not accepted.</div>{{end}}
    <form method="get" action="{{.Action}}">
      <label for="token">Hub token</label>
      <input id="token" name="token" type="password" autocomplete="current-password" autofocus required>
      <button type="submit">Connect to Hub</button>
    </form>
  </main>
</body>
</html>`))

func writeHubLoginPage(w http.ResponseWriter, r *http.Request, invalid bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	action := r.URL.EscapedPath()
	if action == "" {
		action = "/"
	}
	if err := hubLoginTemplate.Execute(w, struct {
		Action  string
		Invalid bool
	}{Action: action, Invalid: invalid}); err != nil {
		return
	}
}

func bearerTokenFromHeader(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, rest, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

func hubTokenMatches(want, got string) bool {
	got = strings.TrimSpace(got)
	if got == "" || want == "" {
		return false
	}
	wantHash := sha256.Sum256([]byte(want))
	gotHash := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(wantHash[:], gotHash[:]) == 1
}

func setHubAuthCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     hubAuthCookieName,
		Value:    strings.TrimSpace(token),
		Path:     "/",
		Expires:  time.Now().Add(365 * 24 * time.Hour),
		MaxAge:   365 * 24 * 60 * 60,
		SameSite: http.SameSiteStrictMode,
		HttpOnly: true,
	})
}

// hubBrowserRequestAllowed rejects cross-site browser requests before the hub
// exercises any token-injecting authority or mutates its node registry. This is
// defense-in-depth for --auth none and for bearer-authenticated browser use.
// Same-origin proxied node content is still trusted in v1; long-term host-based
// node isolation should remove that caveat. Requests without Origin and without
// Sec-Fetch-Site are allowed for non-browser clients; public hubs rely on bearer
// auth as the primary boundary for those requests.
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
