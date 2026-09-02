package cmd

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/samsaffron/term-llm/internal/passkeyauth"
)

const (
	hubPasskeyUserName         = "Hub administrator"
	hubPasskeyRPDisplayName    = "term-llm Hub"
	hubSessionCookieName       = "term_llm_hub_session"
	hubLoginCeremonyCookieName = "term_llm_hub_login"
	hubBootstrapCookieName     = "term_llm_hub_bootstrap"
	hubRecoveryCookieName      = "term_llm_hub_recovery"
	hubReauthCookieName        = "term_llm_hub_reauth"
	hubRegistrationCookieName  = "term_llm_hub_registration"
	hubAuthBodyLimit           = 64 << 10
)

type hubPasskeyRuntime struct {
	endpoint      passkeyauth.Endpoint
	store         *passkeyauth.Store
	rp            *passkeyauth.RelyingParty
	sessions      *passkeyauth.Sessions
	ceremonies    *passkeyauth.Ceremonies
	bootstrap     *passkeyauth.Grants
	recovery      *passkeyauth.Grants
	limiter       *hubAuthLimiter
	peerResolver  *hubClientPeerResolver
	grantCommitMu sync.Mutex
}
type hubPrincipalKey struct{}
type hubExpectedOriginKey struct{}

func hubPrincipal(r *http.Request) (passkeyauth.Principal, bool) {
	p, ok := r.Context().Value(hubPrincipalKey{}).(passkeyauth.Principal)
	return p, ok
}

func newHubPasskeyRuntime(endpoint passkeyauth.Endpoint, store *passkeyauth.Store, sessions *passkeyauth.Sessions, bootstrap, recovery *passkeyauth.Grants, peerResolver *hubClientPeerResolver) (*hubPasskeyRuntime, error) {
	if store == nil || sessions == nil || bootstrap == nil || recovery == nil {
		return nil, fmt.Errorf("passkey credential, session, and grant stores are required")
	}
	if peerResolver == nil {
		peerResolver, _ = newHubClientPeerResolver(nil)
	}
	rp, err := passkeyauth.NewRelyingParty(passkeyauth.RelyingPartyOptions{Endpoint: endpoint, DisplayName: hubPasskeyRPDisplayName})
	if err != nil {
		return nil, err
	}
	return &hubPasskeyRuntime{endpoint: endpoint, store: store, rp: rp, sessions: sessions, ceremonies: passkeyauth.NewCeremonies(nil, nil), bootstrap: bootstrap, recovery: recovery, limiter: newHubAuthLimiter(nil), peerResolver: peerResolver}, nil
}

func (s *hubServer) registerPasskeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/auth/login", s.handlePasskeyPage)
	mux.HandleFunc("/auth/setup", s.handlePasskeyPage)
	mux.HandleFunc("/auth/recover", s.handlePasskeyPage)
	mux.HandleFunc("/api/auth/bootstrap/verify", s.handleBootstrapVerify)
	mux.HandleFunc("/api/auth/bootstrap/register/begin", s.handleBootstrapRegisterBegin)
	mux.HandleFunc("/api/auth/bootstrap/register/finish", s.handleBootstrapRegisterFinish)
	mux.HandleFunc("/api/auth/recovery/verify", s.handleRecoveryVerify)
	mux.HandleFunc("/api/auth/recovery/register/begin", s.handleRecoveryRegisterBegin)
	mux.HandleFunc("/api/auth/recovery/register/finish", s.handleRecoveryRegisterFinish)
	mux.HandleFunc("/api/auth/login/begin", s.handlePasskeyLoginBegin)
	mux.HandleFunc("/api/auth/login/finish", s.handlePasskeyLoginFinish)
	mux.HandleFunc("/api/auth/reauth/begin", s.handlePasskeyReauthBegin)
	mux.HandleFunc("/api/auth/reauth/finish", s.handlePasskeyReauthFinish)
	mux.HandleFunc("/api/auth/session", s.handlePasskeySession)
	mux.HandleFunc("/api/auth/logout", s.handlePasskeyLogout)
	mux.HandleFunc("/api/auth/sessions/revoke-others", s.handlePasskeyRevokeOthers)
	mux.HandleFunc("/api/auth/credentials/register/begin", s.handleAdditionalRegisterBegin)
	mux.HandleFunc("/api/auth/credentials/register/finish", s.handleAdditionalRegisterFinish)
	mux.HandleFunc("/api/auth/credentials/", s.handleCredentialItem)
	mux.HandleFunc("/api/auth/credentials", s.handleCredentials)
}

func (s *hubServer) passkeyAPIAllowed(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Origin")), s.passkey.endpoint.Origin) || strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") || strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "same-site") {
			writeOpenAIError(w, http.StatusForbidden, "invalid_origin", "request origin is not allowed")
			return false
		}
	}
	if (strings.HasPrefix(r.URL.Path, "/api/auth/login/") || strings.HasPrefix(r.URL.Path, "/api/auth/bootstrap/") || strings.HasPrefix(r.URL.Path, "/api/auth/recovery/")) && !s.passkey.limiter.allow(s.passkey.peerResolver.peer(r)) {
		writeHubRateLimited(w)
		return false
	}
	return true
}
func hasJSONContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(r.Header.Get("Content-Type")))
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func requireJSONPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return false
	}
	if !hasJSONContentType(r) {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_content_type", "application/json is required")
		return false
	}
	return true
}
func decodeHubAuthJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !hasJSONContentType(r) {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_content_type", "application/json is required")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, hubAuthBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "authentication request is too large")
			return false
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid authentication request")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid authentication request")
		return false
	}
	return true
}
func prepareWebAuthnBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !hasJSONContentType(r) {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_content_type", "application/json is required")
		return false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, hubAuthBodyLimit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid authentication request")
		return false
	}
	if len(data) > hubAuthBodyLimit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "authentication request is too large")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid authentication request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid authentication request")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	return true
}

func writeHubAuthJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *hubServer) handleBootstrapVerify(w http.ResponseWriter, r *http.Request) {
	if s.passkey.store.CredentialCount() != 0 {
		http.NotFound(w, r)
		return
	}
	s.handleGrantVerify(w, r, s.passkey.bootstrap, hubBootstrapCookieName)
}
func (s *hubServer) handleRecoveryVerify(w http.ResponseWriter, r *http.Request) {
	if s.passkey.store.CredentialCount() == 0 || s.passkey.recovery == nil {
		http.NotFound(w, r)
		return
	}
	s.handleGrantVerify(w, r, s.passkey.recovery, hubRecoveryCookieName)
}
func (s *hubServer) handleGrantVerify(w http.ResponseWriter, r *http.Request, g *passkeyauth.Grants, cookieName string) {
	if !requireJSONPost(w, r) || !s.passkeyAPIAllowed(w, r) {
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if !decodeHubAuthJSON(w, r, &in) {
		return
	}
	if cookie, cookieErr := r.Cookie(cookieName); cookieErr == nil {
		if _, grantErr := g.Authenticate(cookie.Value); grantErr == nil {
			writeHubAuthJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
	}
	grant, err := g.Verify([]byte(in.Code))
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_grant", "invalid or expired setup credential")
		return
	}
	s.setShortCookie(w, cookieName, grant.Token)
	writeHubAuthJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": grant.ExpiresAt})
}

func (s *hubServer) grantFromCookie(r *http.Request, g *passkeyauth.Grants, name string) (string, string, error) {
	c, err := r.Cookie(name)
	if err != nil {
		return "", "", passkeyauth.ErrInvalidGrant
	}
	id, err := g.Authenticate(c.Value)
	return id, c.Value, err
}
func (s *hubServer) handleBootstrapRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if s.passkey.store.CredentialCount() != 0 {
		http.NotFound(w, r)
		return
	}
	s.handleGrantRegisterBegin(w, r, s.passkey.bootstrap, hubBootstrapCookieName, hubLoginCeremonyCookieName, passkeyauth.CeremonyBootstrap)
}
func (s *hubServer) handleRecoveryRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if s.passkey.store.CredentialCount() == 0 {
		http.NotFound(w, r)
		return
	}
	s.handleGrantRegisterBegin(w, r, s.passkey.recovery, hubRecoveryCookieName, hubRegistrationCookieName, passkeyauth.CeremonyRecovery)
}
func (s *hubServer) handleGrantRegisterBegin(w http.ResponseWriter, r *http.Request, g *passkeyauth.Grants, grantCookie, ceremonyCookie string, kind passkeyauth.CeremonyKind) {
	if !requireJSONPost(w, r) || !s.passkeyAPIAllowed(w, r) {
		return
	}
	grantID, _, err := s.grantFromCookie(r, g, grantCookie)
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_grant", "invalid or expired setup session")
		return
	}
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if !decodeHubAuthJSON(w, r, &in) {
		return
	}
	name, err := passkeyauth.NormalizeCredentialName(in.DisplayName)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_name", err.Error())
		return
	}
	options, data, err := s.passkey.rp.BeginRegistration(s.passkey.store.User())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "auth_error", "could not begin registration")
		return
	}
	ceremony, err := s.passkey.ceremonies.Create(kind, s.passkey.peerResolver.peer(r), grantID, "", name, s.passkey.store.User().WebAuthnID(), data)
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "auth_capacity", err.Error())
		return
	}
	s.setShortCookie(w, ceremonyCookie, ceremony.CookieToken)
	writeHubAuthJSON(w, http.StatusOK, options)
}
func (s *hubServer) handleBootstrapRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if s.passkey.store.CredentialCount() != 0 {
		writeOpenAIError(w, http.StatusConflict, "already_configured", "the first passkey was already enrolled")
		return
	}
	s.handleGrantRegisterFinish(w, r, s.passkey.bootstrap, hubBootstrapCookieName, hubLoginCeremonyCookieName, passkeyauth.CeremonyBootstrap, true)
}
func (s *hubServer) handleRecoveryRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if s.passkey.store.CredentialCount() == 0 {
		http.NotFound(w, r)
		return
	}
	s.handleGrantRegisterFinish(w, r, s.passkey.recovery, hubRecoveryCookieName, hubRegistrationCookieName, passkeyauth.CeremonyRecovery, false)
}
func (s *hubServer) handleGrantRegisterFinish(w http.ResponseWriter, r *http.Request, g *passkeyauth.Grants, grantCookie, ceremonyCookie string, kind passkeyauth.CeremonyKind, bootstrap bool) {
	if !requireJSONPost(w, r) || !s.passkeyAPIAllowed(w, r) {
		return
	}
	grantID, grantToken, err := s.grantFromCookie(r, g, grantCookie)
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_grant", "invalid setup session")
		return
	}
	cc, err := r.Cookie(ceremonyCookie)
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_ceremony", "invalid registration ceremony")
		return
	}
	ceremony, err := s.passkey.ceremonies.Consume(cc.Value, kind, grantID, "")
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_ceremony", "invalid registration ceremony")
		return
	}
	admin := s.passkey.store.User()
	if !bytes.Equal(ceremony.UserHandle, admin.WebAuthnID()) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_ceremony", "invalid registration ceremony")
		return
	}
	var browserResponse protocol.CredentialCreationResponse
	if !prepareWebAuthnBody(w, r, &browserResponse) {
		return
	}
	credential, err := s.passkey.rp.FinishRegistration(admin, ceremony.Data, r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_credential", "passkey registration failed")
		return
	}
	s.passkey.grantCommitMu.Lock()
	defer s.passkey.grantCommitMu.Unlock()
	if activeID, authErr := g.Authenticate(grantToken); authErr != nil || activeID != grantID {
		writeOpenAIError(w, http.StatusConflict, "grant_consumed", "the enrollment grant was already used")
		return
	}
	var saved passkeyauth.Credential
	if bootstrap {
		saved, err = s.passkey.store.CommitFirstCredential(credential, ceremony.Meta)
	} else {
		saved, err = s.passkey.store.AddCredential(credential, ceremony.Meta)
	}
	if errors.Is(err, passkeyauth.ErrFirstCredentialExists) {
		writeOpenAIError(w, http.StatusConflict, "already_configured", "the first passkey was already enrolled")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusConflict, "credential_conflict", err.Error())
		return
	}
	_ = g.Consume(grantToken)
	s.clearCookie(w, grantCookie)
	s.clearCookie(w, ceremonyCookie)
	if bootstrap {
		issued, err := s.passkey.sessions.Create(saved.RecordID)
		if err != nil {
			s.writePasskeySessionCreateError(w, err)
			return
		}
		s.setSessionCookie(w, issued)
		writeHubAuthJSON(w, http.StatusCreated, map[string]any{"ok": true, "redirect": s.publicPath("/")})
	} else {
		log.Printf("Hub passkey recovery enrolled a replacement credential")
		writeHubAuthJSON(w, http.StatusCreated, map[string]any{"ok": true, "redirect": s.publicPath("/auth/login")})
	}
}

func (s *hubServer) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	s.handleLoginBegin(w, r, passkeyauth.CeremonyLogin, hubLoginCeremonyCookieName, "")
}
func requireHubOperatorSession(w http.ResponseWriter, r *http.Request) (passkeyauth.Principal, bool) {
	p, ok := hubPrincipal(r)
	if !ok || p.SessionID == "" {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_session", "Hub browser session is required")
		return passkeyauth.Principal{}, false
	}
	return p, true
}

func (s *hubServer) handlePasskeyReauthBegin(w http.ResponseWriter, r *http.Request) {
	p, ok := requireHubOperatorSession(w, r)
	if !ok {
		return
	}
	s.handleLoginBegin(w, r, passkeyauth.CeremonyReauth, hubReauthCookieName, p.SessionID)
}
func (s *hubServer) handleLoginBegin(w http.ResponseWriter, r *http.Request, kind passkeyauth.CeremonyKind, cookieName, sessionID string) {
	if !requireJSONPost(w, r) || !s.passkeyAPIAllowed(w, r) {
		return
	}
	if s.passkey.store.CredentialCount() == 0 {
		writeOpenAIError(w, http.StatusConflict, "setup_required", "first-passkey setup is required")
		return
	}
	var in struct {
		ReturnPath string `json:"return_path"`
	}
	if !decodeHubAuthJSON(w, r, &in) {
		return
	}
	options, data, err := s.passkey.rp.BeginLogin(s.passkey.store.User())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "auth_error", "could not begin passkey authentication")
		return
	}
	returnPath := s.passkey.endpoint.SafeReturnPath(in.ReturnPath)
	ceremony, err := s.passkey.ceremonies.Create(kind, s.passkey.peerResolver.peer(r), "", sessionID, returnPath, s.passkey.store.User().WebAuthnID(), data)
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "auth_capacity", err.Error())
		return
	}
	s.setShortCookie(w, cookieName, ceremony.CookieToken)
	writeHubAuthJSON(w, http.StatusOK, options)
}
func (s *hubServer) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	s.handleLoginFinish(w, r, passkeyauth.CeremonyLogin, hubLoginCeremonyCookieName, false)
}
func (s *hubServer) handlePasskeyReauthFinish(w http.ResponseWriter, r *http.Request) {
	s.handleLoginFinish(w, r, passkeyauth.CeremonyReauth, hubReauthCookieName, true)
}
func (s *hubServer) handleLoginFinish(w http.ResponseWriter, r *http.Request, kind passkeyauth.CeremonyKind, cookieName string, reauth bool) {
	if !requireJSONPost(w, r) || !s.passkeyAPIAllowed(w, r) {
		return
	}
	sessionID := ""
	var principal passkeyauth.Principal
	if reauth {
		var ok bool
		principal, ok = requireHubOperatorSession(w, r)
		if !ok {
			return
		}
		sessionID = principal.SessionID
	}
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_ceremony", "invalid authentication ceremony")
		return
	}
	ceremony, err := s.passkey.ceremonies.Consume(cookie.Value, kind, "", sessionID)
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_ceremony", "invalid authentication ceremony")
		return
	}
	admin := s.passkey.store.User()
	if !bytes.Equal(ceremony.UserHandle, admin.WebAuthnID()) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_ceremony", "invalid authentication ceremony")
		return
	}
	var browserResponse protocol.CredentialAssertionResponse
	if !prepareWebAuthnBody(w, r, &browserResponse) {
		return
	}
	credential, err := s.passkey.rp.FinishLogin(admin, ceremony.Data, r)
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_assertion", "passkey authentication failed")
		return
	}
	reportedCount := credential.Authenticator.SignCount
	if authenticatorData := []byte(browserResponse.AssertionResponse.AuthenticatorData); len(authenticatorData) >= 37 {
		reportedCount = binary.BigEndian.Uint32(authenticatorData[33:37])
	}
	saved, err := s.passkey.store.UpdateAfterAssertion(credential, reportedCount)
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_assertion", "passkey authentication failed")
		return
	}
	s.clearCookie(w, cookieName)
	if reauth {
		if err := s.passkey.sessions.GrantRecentAuth(principal); err != nil {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_session", "invalid session")
			return
		}
		writeHubAuthJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	issued, err := s.passkey.sessions.Create(saved.RecordID)
	if err != nil {
		s.writePasskeySessionCreateError(w, err)
		return
	}
	s.setSessionCookie(w, issued)
	writeHubAuthJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": ceremony.Meta})
}

func (s *hubServer) writePasskeySessionCreateError(w http.ResponseWriter, err error) {
	if errors.Is(err, passkeyauth.ErrSessionCapacity) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "session_capacity", "session capacity reached; remove the Hub sessions file to sign out all browsers")
		return
	}
	log.Printf("hub passkey session creation failed: %v", err)
	writeOpenAIError(w, http.StatusInternalServerError, "session_store_error", "session could not be durably created")
}

func (s *hubServer) handlePasskeySession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	p, ok := hubPrincipal(r)
	if !ok || p.SessionID == "" {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_session", "Hub passkey authentication is required")
		return
	}
	info, err := s.passkey.sessions.Info(p)
	if err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_session", "Hub passkey authentication is required")
		return
	}
	writeHubAuthJSON(w, http.StatusOK, map[string]any{"administrator": s.passkey.store.User().Name, "session": info, "recently_authenticated": s.passkey.sessions.HasRecentAuth(p), "active_sessions": s.passkey.sessions.Count()})
}
func (s *hubServer) handlePasskeyLogout(w http.ResponseWriter, r *http.Request) {
	if !requireJSONPost(w, r) || !s.passkeyAPIAllowed(w, r) {
		return
	}
	p, ok := requireHubOperatorSession(w, r)
	if !ok {
		return
	}
	if err := s.passkey.sessions.Logout(p); err != nil {
		s.clearCookie(w, hubSessionCookieName)
		if errors.Is(err, passkeyauth.ErrInvalidSession) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_session", "invalid session")
		} else {
			log.Printf("hub passkey session logout persistence failed: %v", err)
			writeOpenAIError(w, http.StatusInternalServerError, "session_store_error", "session could not be durably revoked")
		}
		return
	}
	s.clearCookie(w, hubSessionCookieName)
	writeHubAuthJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": s.publicPath("/auth/login")})
}
func (s *hubServer) handlePasskeyRevokeOthers(w http.ResponseWriter, r *http.Request) {
	if !requireJSONPost(w, r) || !s.passkeyAPIAllowed(w, r) {
		return
	}
	p, ok := requireHubOperatorSession(w, r)
	if !ok {
		return
	}
	n, err := s.passkey.sessions.RevokeOthers(p)
	if err != nil {
		if errors.Is(err, passkeyauth.ErrInvalidSession) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_session", "invalid session")
		} else {
			log.Printf("hub passkey session revocation persistence failed: %v", err)
			writeOpenAIError(w, http.StatusInternalServerError, "session_store_error", "sessions could not be durably revoked")
		}
		return
	}
	writeHubAuthJSON(w, http.StatusOK, map[string]any{"revoked": n})
}

type publicCredential struct {
	RecordID    string    `json:"record_id"`
	DisplayName string    `json:"display_name"`
	Transports  any       `json:"transports"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsedAt  time.Time `json:"last_used_at"`
}

func (s *hubServer) handleCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, ok := requireHubOperatorSession(w, r); !ok {
		return
	}
	out := []publicCredential{}
	for _, c := range s.passkey.store.Credentials() {
		out = append(out, publicCredential{c.RecordID, c.DisplayName, c.WebAuthn.Transport, c.CreatedAt, c.LastUsedAt})
	}
	writeHubAuthJSON(w, http.StatusOK, map[string]any{"credentials": out})
}
func (s *hubServer) handleCredentialItem(w http.ResponseWriter, r *http.Request) {
	if !s.passkeyAPIAllowed(w, r) {
		return
	}
	p, ok := requireHubOperatorSession(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/auth/credentials/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var in struct {
			DisplayName string `json:"display_name"`
		}
		if !decodeHubAuthJSON(w, r, &in) {
			return
		}
		c, err := s.passkey.store.RenameCredential(id, in.DisplayName)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "credential_error", err.Error())
			return
		}
		writeHubAuthJSON(w, http.StatusOK, publicCredential{c.RecordID, c.DisplayName, c.WebAuthn.Transport, c.CreatedAt, c.LastUsedAt})
	case http.MethodDelete:
		if err := s.passkey.sessions.ConsumeRecentAuth(p); err != nil {
			writeOpenAIError(w, http.StatusForbidden, "recent_auth_required", err.Error())
			return
		}
		c, err := s.passkey.store.DeleteCredential(id)
		if err != nil {
			writeOpenAIError(w, http.StatusConflict, "credential_error", err.Error())
			return
		}
		if _, err := s.passkey.sessions.RevokeCredential(c.RecordID); err != nil {
			log.Printf("hub passkey credential session revocation persistence failed: %v", err)
			writeOpenAIError(w, http.StatusInternalServerError, "session_store_error", "credential removed, but its sessions could not be durably revoked")
			return
		}
		writeHubAuthJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "PATCH, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
func (s *hubServer) handleAdditionalRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !requireJSONPost(w, r) || !s.passkeyAPIAllowed(w, r) {
		return
	}
	p, ok := requireHubOperatorSession(w, r)
	if !ok {
		return
	}
	if !s.passkey.sessions.HasRecentAuth(p) {
		writeOpenAIError(w, http.StatusForbidden, "recent_auth_required", passkeyauth.ErrRecentAuthRequired.Error())
		return
	}
	var in struct {
		DisplayName string `json:"display_name"`
	}
	if !decodeHubAuthJSON(w, r, &in) {
		return
	}
	name, err := passkeyauth.NormalizeCredentialName(in.DisplayName)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_name", err.Error())
		return
	}
	options, data, err := s.passkey.rp.BeginRegistration(s.passkey.store.User())
	if err != nil {
		writeOpenAIError(w, 500, "auth_error", "could not begin registration")
		return
	}
	ceremony, err := s.passkey.ceremonies.Create(passkeyauth.CeremonyAddCredential, s.passkey.peerResolver.peer(r), "", p.SessionID, name, s.passkey.store.User().WebAuthnID(), data)
	if err != nil {
		writeOpenAIError(w, 503, "auth_capacity", err.Error())
		return
	}
	s.setShortCookie(w, hubRegistrationCookieName, ceremony.CookieToken)
	writeHubAuthJSON(w, 200, options)
}
func (s *hubServer) handleAdditionalRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !requireJSONPost(w, r) || !s.passkeyAPIAllowed(w, r) {
		return
	}
	p, ok := requireHubOperatorSession(w, r)
	if !ok {
		return
	}
	cookie, err := r.Cookie(hubRegistrationCookieName)
	if err != nil {
		writeOpenAIError(w, 401, "invalid_ceremony", "invalid registration ceremony")
		return
	}
	ceremony, err := s.passkey.ceremonies.Consume(cookie.Value, passkeyauth.CeremonyAddCredential, "", p.SessionID)
	if err != nil {
		writeOpenAIError(w, 401, "invalid_ceremony", "invalid registration ceremony")
		return
	}
	admin := s.passkey.store.User()
	if !bytes.Equal(ceremony.UserHandle, admin.WebAuthnID()) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_ceremony", "invalid registration ceremony")
		return
	}
	var browserResponse protocol.CredentialCreationResponse
	if !prepareWebAuthnBody(w, r, &browserResponse) {
		return
	}
	credential, err := s.passkey.rp.FinishRegistration(admin, ceremony.Data, r)
	if err != nil {
		writeOpenAIError(w, 400, "invalid_credential", "passkey registration failed")
		return
	}
	if err := s.passkey.sessions.ConsumeRecentAuth(p); err != nil {
		writeOpenAIError(w, http.StatusForbidden, "recent_auth_required", err.Error())
		return
	}
	saved, err := s.passkey.store.AddCredential(credential, ceremony.Meta)
	if err != nil {
		writeOpenAIError(w, 409, "credential_conflict", err.Error())
		return
	}
	s.clearCookie(w, hubRegistrationCookieName)
	writeHubAuthJSON(w, 201, publicCredential{saved.RecordID, saved.DisplayName, saved.WebAuthn.Transport, saved.CreatedAt, saved.LastUsedAt})
}

func (s *hubServer) setShortCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: s.passkey.endpoint.CookiePath, HttpOnly: true, Secure: s.passkey.endpoint.Secure, SameSite: http.SameSiteStrictMode, MaxAge: 300, Expires: time.Now().Add(5 * time.Minute)})
}
func (s *hubServer) setSessionCookie(w http.ResponseWriter, issued passkeyauth.IssuedSession) {
	max := int(passkeyauth.SessionAbsoluteLifetime.Seconds())
	if max < 1 {
		max = 1
	}
	http.SetCookie(w, &http.Cookie{Name: hubSessionCookieName, Value: issued.Token, Path: s.passkey.endpoint.CookiePath, HttpOnly: true, Secure: s.passkey.endpoint.Secure, SameSite: http.SameSiteStrictMode, MaxAge: max, Expires: issued.Info.AbsoluteExpiresAt})
}
func (s *hubServer) clearCookie(w http.ResponseWriter, name string) {
	path := s.publicPath("/")
	if s.passkey != nil {
		path = s.passkey.endpoint.CookiePath
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, HttpOnly: true, Secure: s.passkey != nil && s.passkey.endpoint.Secure, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}

func (s *hubServer) passkeyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/auth/") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Add("Vary", "Origin")
		}
		r = r.WithContext(context.WithValue(r.Context(), hubExpectedOriginKey{}, s.passkey.endpoint.Origin))
		if legacy, err := r.Cookie(hubAuthCookieName); err == nil && legacy.Value != "" {
			s.clearCookie(w, hubAuthCookieName)
		}
		if r.Method == http.MethodOptions || r.URL.Path == "/healthz" || hubNodeAuthRoute(r) || hubRegistrationRoute(r) || passkeyPublicRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if p, ok := s.authenticatePasskeyRequest(r); ok {
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && p.SessionID != "" {
				w.Header().Add("Vary", "Origin")
				if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Origin")), s.passkey.endpoint.Origin) {
					writeOpenAIError(w, http.StatusForbidden, "invalid_origin", "request origin is not allowed")
					return
				}
			}
			r = withHubPrincipal(r, p)
			if hubDelegationOperatorRoute(r) {
				clone := r.Clone(r.Context())
				clone.Header = r.Header.Clone()
				clone.Header.Del("Authorization")
				r = clone
			}
			next.ServeHTTP(w, r)
			return
		}
		if hubShouldRenderLogin(r) {
			returnURL := *r.URL
			query := returnURL.Query()
			query.Del("token")
			returnURL.RawQuery = query.Encode()
			target := s.publicPath("/auth/login") + "?return=" + url.QueryEscape(s.publicURLString(&returnURL))
			if s.bootstrapAvailable(r) {
				target = s.publicPath("/auth/setup")
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/node/") {
			w.Header().Set("X-Term-LLM-Login-URL", s.publicPath("/auth/login"))
			writeOpenAIError(w, http.StatusUnauthorized, "hub_auth_required", "Hub passkey authentication is required")
			return
		}
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_session", "Hub passkey authentication is required")
	})
}

func passkeyPublicRoute(path string) bool {
	switch path {
	case "/auth/login", "/auth/setup", "/auth/recover", "/dist/hub.js", "/dist/hub.css",
		"/api/auth/login/begin", "/api/auth/login/finish",
		"/api/auth/bootstrap/verify", "/api/auth/bootstrap/register/begin", "/api/auth/bootstrap/register/finish",
		"/api/auth/recovery/verify", "/api/auth/recovery/register/begin", "/api/auth/recovery/register/finish":
		return true
	default:
		return false
	}
}
func (s *hubServer) authenticatePasskeyRequest(r *http.Request) (passkeyauth.Principal, bool) {
	if c, err := r.Cookie(hubSessionCookieName); err == nil {
		if p, err := s.passkey.sessions.Authenticate(c.Value); err == nil {
			return p, true
		}
	}
	if s.token != "" && hubTokenMatches(s.token, bearerTokenFromHeader(r)) {
		return passkeyauth.Principal{CredentialRecordID: "bearer"}, true
	}
	return passkeyauth.Principal{}, false
}

func withHubPrincipal(r *http.Request, p passkeyauth.Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), hubPrincipalKey{}, p))
}
