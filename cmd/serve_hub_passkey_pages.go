package cmd

import (
	_ "embed"
	"html/template"
	"net/http"

	"github.com/samsaffron/term-llm/internal/passkeyauth"
)

//go:embed templates/hub_auth.html
var hubAuthHTML string

//go:embed templates/hub_auth.js
var hubAuthJS string
var hubAuthTmpl = template.Must(template.New("hub-auth").Parse(hubAuthHTML))

type hubAuthView struct {
	Title, Heading, Description, Mode, BasePath, ScriptURL, Nonce, Button, DefaultName string
	NeedsCode, NeedsName                                                               bool
}

func (s *hubServer) hasGrantSession(r *http.Request, grants *passkeyauth.Grants, cookieName string) bool {
	if grants == nil {
		return false
	}
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	_, err = grants.Authenticate(cookie.Value)
	return err == nil
}

func (s *hubServer) bootstrapAvailable(r *http.Request) bool {
	return s.passkey != nil && s.passkey.store.CredentialCount() == 0 && (s.passkey.bootstrap.Enabled() || s.hasGrantSession(r, s.passkey.bootstrap, hubBootstrapCookieName))
}

func (s *hubServer) recoveryAvailable(r *http.Request) bool {
	return s.passkey != nil && s.passkey.store.CredentialCount() > 0 && s.passkey.recovery != nil && (s.passkey.recovery.Enabled() || s.hasGrantSession(r, s.passkey.recovery, hubRecoveryCookieName))
}

func (s *hubServer) handlePasskeyPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	mode := "login"
	view := hubAuthView{Title: "Sign in", Heading: "Sign in to Hub", Description: "Use an enrolled passkey to continue.", Mode: "login", BasePath: s.basePath, ScriptURL: s.publicPath("/auth/hub_auth.js"), Button: "Sign in with a passkey"}
	switch r.URL.Path {
	case "/auth/setup":
		if !s.bootstrapAvailable(r) {
			http.NotFound(w, r)
			return
		}
		mode = "setup"
		view.Title = "Set up passkey"
		view.Heading = "Set up Hub administrator"
		view.Description = "Enter the one-time code shown by the Hub process, then create your first passkey."
		view.Button = "Verify and create passkey"
		view.NeedsCode = true
		view.NeedsName = true
		view.DefaultName = passkeyauth.DefaultCredentialName
	case "/auth/recover":
		if !s.recoveryAvailable(r) {
			http.NotFound(w, r)
			return
		}
		mode = "recover"
		view.Title = "Recover access"
		view.Heading = "Add a recovery passkey"
		view.Description = "Enter the short-lived recovery secret configured on the Hub host."
		view.Button = "Verify and add passkey"
		view.NeedsCode = true
		view.NeedsName = true
		view.DefaultName = "Recovery passkey"
	case "/auth/login":
		if s.bootstrapAvailable(r) {
			http.Redirect(w, r, s.publicPath("/auth/setup"), http.StatusSeeOther)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	view.Mode = mode
	nonce, _ := generateServeToken()
	view.Nonce = nonce
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'nonce-"+nonce+"'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_ = hubAuthTmpl.Execute(w, view)
}
func (s *hubServer) handlePasskeyScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(hubAuthJS))
}
