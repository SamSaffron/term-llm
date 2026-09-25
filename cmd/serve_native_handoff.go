package cmd

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/passkeyauth"
)

// Native app handoff (PKCE-style, RFC 7636 S256):
//
//  1. The app generates a code_verifier and opens
//     {base}/auth/native/{b64url(sha256(code_verifier))} in a browser.
//  2. The browser signs in if needed, then shows an approval page. Opening
//     the URL never mints anything by itself: a same-origin POST to
//     /api/auth/native/authorize must consume a fresh passkey assertion.
//  3. The browser is sent to termllm-auth://callback?code=…&state={challenge}.
//  4. The app POSTs {code, code_verifier} to /api/auth/native/redeem and
//     receives its own browser session cookie.
//
// Because every ticket needs a fresh passkey ceremony, a session cannot use
// the handoff to extend its own absolute lifetime without the user present.
const (
	nativeTicketLifetime = 60 * time.Second
	nativeTicketCapacity = 1024
	nativeHandoffPrefix  = "/auth/native/"
	nativeCallbackScheme = "termllm-auth"
)

type nativeHandoffTicket struct {
	principal passkeyauth.Principal
	challenge string
	scope     string // mount that issued the ticket; see nativeTicketScope
	expiresAt time.Time
}

// Stored on the shared passkey runtime: Hub creates a new browser handler per request.
// Only hashes of one-use ticket values are retained in memory.
type nativeHandoffTickets struct {
	mu     sync.Mutex
	byHash map[[32]byte]nativeHandoffTicket
	now    func() time.Time // nil uses the wall clock
}

func (t *nativeHandoffTickets) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func nativeTokenValid(value string) bool {
	if len(value) != 43 { // unpadded base64url encoding of 32 bytes
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

// validNativeChallenge reports whether value is an S256 code challenge.
func validNativeChallenge(value string) bool { return nativeTokenValid(value) }

// validNativeVerifier enforces the RFC 7636 code_verifier grammar.
func validNativeVerifier(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	return true
}

func nativeChallengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (t *nativeHandoffTickets) issue(principal passkeyauth.Principal, challenge, scope string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock()
	if t.byHash == nil {
		t.byHash = make(map[[32]byte]nativeHandoffTicket)
	}
	for hash, entry := range t.byHash {
		if !now.Before(entry.expiresAt) {
			delete(t.byHash, hash)
		}
	}
	if len(t.byHash) >= nativeTicketCapacity {
		return "", false
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", false
	}
	code := base64.RawURLEncoding.EncodeToString(random[:])
	hash := sha256.Sum256([]byte(code))
	if _, exists := t.byHash[hash]; exists {
		return "", false
	}
	t.byHash[hash] = nativeHandoffTicket{principal: principal, challenge: challenge, scope: scope, expiresAt: now.Add(nativeTicketLifetime)}
	return code, true
}

// consume removes the ticket unconditionally, so a wrong verifier or a
// redemption through another mount burns it.
func (t *nativeHandoffTickets) consume(code, verifier, scope string) (passkeyauth.Principal, bool) {
	if !nativeTokenValid(code) {
		return passkeyauth.Principal{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	hash := sha256.Sum256([]byte(code))
	entry, exists := t.byHash[hash]
	delete(t.byHash, hash) // consume even if expired, mismatched, or the session was revoked
	if !exists || !t.clock().Before(entry.expiresAt) || entry.scope != scope || !validNativeVerifier(verifier) {
		return passkeyauth.Principal{}, false
	}
	if subtle.ConstantTimeCompare([]byte(nativeChallengeFor(verifier)), []byte(entry.challenge)) != 1 {
		return passkeyauth.Principal{}, false
	}
	return entry.principal, true
}

// handleNativeHandoff renders the approval page. It never mints a ticket.
func (s *browserPasskeyHandler) handleNativeHandoff(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	challenge := strings.TrimPrefix(r.URL.Path, nativeHandoffPrefix)
	if !validNativeChallenge(challenge) {
		http.Error(w, "invalid native login challenge", http.StatusBadRequest)
		return
	}
	if !s.nativeSessionValid(r) {
		// Normally handled by passkeyAuth; kept for sessions revoked mid-request.
		s.redirectNativeHandoffLogin(w, r)
		return
	}
	appName := "Hub"
	if s.web {
		appName = "term-llm"
	}
	s.writeHubShell(w, r, http.StatusOK, "Approve app sign-in - term-llm", hubPageConfig{
		Page:        "passkey-auth",
		AuthMode:    "passkey",
		BasePath:    s.basePath,
		PasskeyAuth: true,
		FormAction:  s.publicPath(r.URL.EscapedPath()),
		Passkey: &hubPasskeyPageConfig{
			Mode:        "native",
			Title:       "Approve app sign-in",
			Heading:     "Sign in the term-llm app",
			Description: "The term-llm app on this device is asking to sign in to " + appName + ". Only continue if you just started this sign-in. Confirm with your passkey to approve.",
			Button:      "Approve with a passkey",
			Challenge:   challenge,
			Origin:      s.passkey.endpoint.Origin,
		},
	})
}

// nativeTicketScope identifies the mount (Hub or Web, and its base path) so a
// ticket is redeemed only where it was approved, even if handlers share a
// passkey runtime.
func (s *browserPasskeyHandler) nativeTicketScope() string {
	return s.cookieName(hubSessionCookieName) + "|" + s.basePath
}

func (s *browserPasskeyHandler) nativeSessionValid(r *http.Request) bool {
	principal, ok := hubPrincipal(r)
	if !ok || principal.SessionID == "" {
		return false
	}
	_, err := s.passkey.sessions.Info(principal)
	return err == nil
}

// handleNativeAuthorize mints a one-use ticket bound to the PKCE challenge.
// It requires a same-origin POST from a browser session that has just
// completed a passkey assertion; that one-use grant is consumed.
func (s *browserPasskeyHandler) handleNativeAuthorize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !requireJSONPost(w, r) || !s.passkeyAPIAllowed(w, r) {
		return
	}
	principal, ok := hubPrincipal(r)
	if !ok || principal.SessionID == "" {
		s.writeNativeAuthorizeFailure(w, r, http.StatusUnauthorized, "invalid_session", "Passkey browser session is required")
		return
	}
	var in struct {
		Challenge string `json:"challenge"`
	}
	if !decodeHubAuthJSON(w, r, &in) {
		return
	}
	if !validNativeChallenge(in.Challenge) {
		s.writeNativeAuthorizeFailure(w, r, http.StatusBadRequest, "invalid_challenge", "invalid native login challenge")
		return
	}
	if err := s.passkey.sessions.ConsumeNativeApproval(principal, in.Challenge); err != nil {
		if errors.Is(err, passkeyauth.ErrRecentAuthRequired) {
			// The approval page's normal first step; not charged (see below).
			writeOpenAIError(w, http.StatusForbidden, "recent_auth_required", err.Error())
		} else {
			s.writeNativeAuthorizeFailure(w, r, http.StatusUnauthorized, "invalid_session", "Passkey browser session is required")
		}
		return
	}
	code, ok := s.passkey.nativeTickets.issue(principal, in.Challenge, s.nativeTicketScope())
	if !ok {
		writeOpenAIError(w, http.StatusServiceUnavailable, "auth_capacity", "native login unavailable")
		return
	}
	callback := url.URL{Scheme: nativeCallbackScheme, Host: "callback"}
	callback.RawQuery = url.Values{"code": {code}, "state": {in.Challenge}}.Encode()
	writeHubAuthJSON(w, http.StatusOK, map[string]any{"ok": true, "redirect": callback.String()})
}

// writeNativeAuthorizeFailure charges malformed or unauthenticated approvals
// to the per-peer auth limiter. Successful approvals and the expected
// recent_auth_required step (after which the page runs a passkey ceremony) are
// not charged: each ticket already needs a passkey assertion, and charging
// them would exhaust the small per-peer burst that a first-passkey setup
// shares with the app's redemption from the same device.
func (s *browserPasskeyHandler) writeNativeAuthorizeFailure(w http.ResponseWriter, r *http.Request, status int, errorType, message string) {
	if !s.passkey.limiter.allow(s.passkey.peerResolver.peer(r)) {
		writeHubRateLimited(w)
		return
	}
	writeOpenAIError(w, status, errorType, message)
}

func (s *browserPasskeyHandler) handleNativeRedeem(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !requireJSONPost(w, r) || !s.passkeyAPIAllowed(w, r) {
		return
	}
	var in struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
	}
	if !decodeHubAuthJSON(w, r, &in) {
		return
	}
	principal, ok := s.passkey.nativeTickets.consume(in.Code, in.CodeVerifier, s.nativeTicketScope())
	if !ok || principal.SessionID == "" {
		writeNativeRedeemFailure(w)
		return
	}
	info, err := s.passkey.sessions.Info(principal)
	if err != nil {
		writeNativeRedeemFailure(w)
		return
	}
	issued, err := s.passkey.sessions.Create(info.CredentialRecordID)
	if err != nil {
		// Do not expose whether the original session still exists or why a new
		// session could not be created to a holder of a stolen ticket.
		writeNativeRedeemFailure(w)
		return
	}
	s.setSessionCookie(w, issued)
	writeHubAuthJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeNativeRedeemFailure(w http.ResponseWriter) {
	writeOpenAIError(w, http.StatusUnauthorized, "invalid_code", "invalid or expired native login code")
}
