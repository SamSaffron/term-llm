package cmd

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/samsaffron/term-llm/internal/htmlesc"
	"github.com/samsaffron/term-llm/internal/serveui"
)

type hubPageConfig struct {
	CacheScope          string                `json:"cacheScope,omitempty"`
	CacheDisplayAllowed bool                  `json:"cacheDisplayAllowed"`
	Page                string                `json:"page"`
	AuthMode            string                `json:"authMode"`
	BasePath            string                `json:"basePath"`
	CanAddNodes         bool                  `json:"canAddNodes"`
	PasskeyAuth         bool                  `json:"passkeyAuth"`
	InvalidToken        bool                  `json:"invalidToken"`
	FormAction          string                `json:"formAction"`
	Passkey             *hubPasskeyPageConfig `json:"passkey,omitempty"`
}

type hubPasskeyPageConfig struct {
	Mode        string `json:"mode"`
	Title       string `json:"title"`
	Heading     string `json:"heading"`
	Description string `json:"description"`
	Button      string `json:"button"`
	NeedsCode   bool   `json:"needsCode"`
	NeedsName   bool   `json:"needsName"`
	DefaultName string `json:"defaultName"`
	Challenge   string `json:"challenge,omitempty"`
	// Origin is the configured WebAuthn origin. Passkeys and session cookies
	// only work there, so the page steers other origins (e.g. 127.0.0.1 for
	// localhost) to it instead of failing the ceremony's origin check.
	Origin string `json:"origin,omitempty"`
}

type hubShellView struct {
	Title        string
	StyleURL     string
	ScriptURL    string
	ConfigJSON   string
	Dashboard    bool
	BearerLogin  bool
	InvalidToken bool
	FormAction   string
}

func (s *hubServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	authMode := s.authMode
	if authMode != "passkey" {
		if s.requireAuth {
			authMode = "bearer"
		} else {
			authMode = "none"
		}
	}
	s.writeHubShell(w, r, http.StatusOK, "Hub - term-llm", hubPageConfig{
		// Unlike the chat bearer shell, the Hub dashboard is gated by auth:
		// bearer navigation requires the Hub token cookie (or Authorization),
		// passkey navigation requires a session; none grants access outright.
		CacheScope:          s.hubCacheScope(authMode),
		CacheDisplayAllowed: true,
		Page:                "dashboard",
		AuthMode:            authMode,
		BasePath:            s.basePath,
		CanAddNodes:         s.store != nil,
		PasskeyAuth:         s.passkey != nil,
		InvalidToken:        false,
		FormAction:          s.publicPath("/"),
	})
}

func (s *hubServer) writeHubShell(w http.ResponseWriter, r *http.Request, status int, title string, config hubPageConfig) {
	writeBrowserAuthShell(w, r, status, title, config)
}

func writeBrowserAuthShell(w http.ResponseWriter, r *http.Request, status int, title string, config hubPageConfig) {
	publicPath := func(p string) string { return config.BasePath + p }
	encoded, err := json.Marshal(config)
	if err != nil {
		http.Error(w, "could not encode Hub configuration", http.StatusInternalServerError)
		return
	}
	view := hubShellView{
		Title:        title,
		StyleURL:     publicPath("/dist/hub.css") + "?v=" + url.QueryEscape(serveui.HubAssetVersion()),
		ScriptURL:    publicPath("/dist/hub.js") + "?v=" + url.QueryEscape(serveui.HubAssetVersion()),
		ConfigJSON:   string(encoded),
		Dashboard:    config.Page == "dashboard",
		BearerLogin:  config.Page == "bearer-login",
		InvalidToken: config.InvalidToken,
		FormAction:   config.FormAction,
	}
	body := renderHubShell(view)
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	csp := "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	if config.Page == "dashboard" {
		csp += "; img-src 'self' data: http: https:"
	}
	header.Set("Content-Security-Policy", csp)
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

func (s *hubServer) handleHubAsset(w http.ResponseWriter, r *http.Request) {
	handleBrowserAuthAsset(w, r)
}

func handleBrowserAuthAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var name, contentType, cacheControl string
	switch r.URL.Path {
	case "/dist/hub.js":
		name = "dist/hub.js"
		contentType = "text/javascript; charset=utf-8"
		cacheControl = "no-cache"
	case "/dist/hub.css":
		name = "dist/hub.css"
		contentType = "text/css; charset=utf-8"
		cacheControl = "no-cache"
	default:
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("v") == serveui.HubAssetVersion() {
		cacheControl = "public, max-age=31536000, immutable"
	}
	data, err := serveui.StaticAsset(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	serveEmbeddedUIBytes(w, r, data, contentType, cacheControl, true)
}

// hubDashboardIcon is the inline SVG favicon shown on the dashboard page.
const hubDashboardIcon = `<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 64 64'%3E%3Crect width='64' height='64' rx='14' fill='%230d1117'/%3E%3Cg fill='none' stroke='%23e6edf3' stroke-width='5' stroke-linecap='round' stroke-linejoin='round'%3E%3Ccircle cx='32' cy='32' r='8'/%3E%3Ccircle cx='14' cy='16' r='5'/%3E%3Ccircle cx='50' cy='16' r='5'/%3E%3Ccircle cx='14' cy='48' r='5'/%3E%3Ccircle cx='50' cy='48' r='5'/%3E%3Cpath d='M18 20 27 29M46 20 37 29M18 44 27 35M46 44 37 35'/%3E%3C/g%3E%3C/svg%3E">`

// renderHubShell renders the Hub and passkey browser shell. It is written by
// hand rather than with html/template, whose reflection-based execution
// defeats the linker's dead-code elimination (see internal/htmlesc).
func renderHubShell(v hubShellView) string {
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="dark light">
  <title>` + htmlesc.Text(v.Title) + `</title>
  `)
	if v.Dashboard {
		b.WriteString(hubDashboardIcon)
	}
	b.WriteString(`
  <link rel="stylesheet" href="` + htmlesc.URL(v.StyleURL) + `">
  <script type="module" src="` + htmlesc.URL(v.ScriptURL) + `"></script>
</head>
<body>
  <div id="root" data-hub-config="` + htmlesc.Text(v.ConfigJSON) + `"></div>
  `)
	if v.BearerLogin {
		b.WriteString(`
  <noscript>
    <main>
      <h1>term-llm Hub</h1>
      <p>Enter your hub access token to continue.</p>
      `)
		if v.InvalidToken {
			b.WriteString(`<p role="alert">That hub token was not accepted.</p>`)
		}
		b.WriteString(`
      <form method="get" action="` + htmlesc.URL(v.FormAction) + `">
        <label for="noscript-hub-token">Hub token</label>
        <input id="noscript-hub-token" name="token" type="password" autocomplete="current-password" required>
        <button type="submit">Connect to Hub</button>
      </form>
    </main>
  </noscript>
  `)
	}
	b.WriteString(`
</body>
</html>
`)
	return b.String()
}
