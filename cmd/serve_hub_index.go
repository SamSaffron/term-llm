package cmd

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"

	"github.com/samsaffron/term-llm/internal/serveui"
)

//go:embed templates/hub_shell.html
var hubShellHTML string

var hubShellTmpl = template.Must(template.New("hub-shell").Parse(hubShellHTML))

type hubPageConfig struct {
	Page         string                `json:"page"`
	AuthMode     string                `json:"authMode"`
	BasePath     string                `json:"basePath"`
	CanAddNodes  bool                  `json:"canAddNodes"`
	PasskeyAuth  bool                  `json:"passkeyAuth"`
	InvalidToken bool                  `json:"invalidToken"`
	FormAction   string                `json:"formAction"`
	Passkey      *hubPasskeyPageConfig `json:"passkey,omitempty"`
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
		Page:         "dashboard",
		AuthMode:     authMode,
		BasePath:     s.basePath,
		CanAddNodes:  s.store != nil,
		PasskeyAuth:  s.passkey != nil,
		InvalidToken: false,
		FormAction:   s.publicPath("/"),
	})
}

func (s *hubServer) writeHubShell(w http.ResponseWriter, r *http.Request, status int, title string, config hubPageConfig) {
	encoded, err := json.Marshal(config)
	if err != nil {
		http.Error(w, "could not encode Hub configuration", http.StatusInternalServerError)
		return
	}
	view := hubShellView{
		Title:        title,
		StyleURL:     s.publicPath("/dist/hub.css") + "?v=" + url.QueryEscape(serveui.HubAssetVersion()),
		ScriptURL:    s.publicPath("/dist/hub.js"),
		ConfigJSON:   string(encoded),
		Dashboard:    config.Page == "dashboard",
		BearerLogin:  config.Page == "bearer-login",
		InvalidToken: config.InvalidToken,
		FormAction:   config.FormAction,
	}
	var body bytes.Buffer
	if err := hubShellTmpl.Execute(&body, view); err != nil {
		http.Error(w, "could not render Hub", http.StatusInternalServerError)
		return
	}
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
		_, _ = w.Write(body.Bytes())
	}
}

func (s *hubServer) handleHubAsset(w http.ResponseWriter, r *http.Request) {
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
		if r.URL.Query().Get("v") == serveui.HubAssetVersion() {
			cacheControl = "public, max-age=31536000, immutable"
		}
	default:
		http.NotFound(w, r)
		return
	}
	data, err := serveui.StaticAsset(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	serveEmbeddedUIBytes(w, r, data, contentType, cacheControl, true)
}
