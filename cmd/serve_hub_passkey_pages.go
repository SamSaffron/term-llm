package cmd

import (
	"net/http"

	"github.com/samsaffron/term-llm/internal/passkeyauth"
)

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
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	page := &hubPasskeyPageConfig{
		Mode:        "login",
		Title:       "Sign in",
		Heading:     "Sign in to Hub",
		Description: "Use an enrolled passkey to continue.",
		Button:      "Sign in with a passkey",
	}
	switch r.URL.Path {
	case "/auth/setup":
		if !s.bootstrapAvailable(r) {
			http.NotFound(w, r)
			return
		}
		page.Mode = "setup"
		page.Title = "Set up passkey"
		page.Heading = "Set up Hub administrator"
		page.Description = "Enter the one-time code shown by the Hub process, then create your first passkey."
		page.Button = "Verify and create passkey"
		page.NeedsCode = true
		page.NeedsName = true
		page.DefaultName = passkeyauth.DefaultCredentialName
	case "/auth/recover":
		if !s.recoveryAvailable(r) {
			http.NotFound(w, r)
			return
		}
		page.Mode = "recover"
		page.Title = "Recover access"
		page.Heading = "Add a recovery passkey"
		page.Description = "Enter the short-lived recovery secret configured on the Hub host."
		page.Button = "Verify and add passkey"
		page.NeedsCode = true
		page.NeedsName = true
		page.DefaultName = "Recovery passkey"
	case "/auth/login":
		if s.bootstrapAvailable(r) {
			http.Redirect(w, r, s.publicPath("/auth/setup"), http.StatusSeeOther)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	s.writeHubShell(w, r, http.StatusOK, page.Title+" - term-llm Hub", hubPageConfig{
		Page:        "passkey-auth",
		AuthMode:    "passkey",
		BasePath:    s.basePath,
		PasskeyAuth: true,
		FormAction:  s.publicPath(r.URL.EscapedPath()),
		Passkey:     page,
	})
}
