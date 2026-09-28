package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/passkeyauth"
)

const nativeTestVerifier = "native-test-verifier-0123456789abcdefghijklmnopqrstuvwxyz"

func nativeTestServer(t *testing.T, web bool) (http.Handler, *hubPasskeyRuntime, string, string) {
	t.Helper()
	base := "/hub"
	if web {
		base = "/ui"
	}
	hub := newTestPasskeyHub(t, base)
	hub.passkey.bootstrap, _ = passkeyauth.NewGrants(passkeyauth.GrantBootstrap, nil, nil, nil)
	cookieName := hubSessionCookieName
	if web {
		cookieName = "term_llm_web_session"
		server := &serveServer{cfg: serveServerConfig{ui: true, requireAuth: true, basePath: base}, browserAuth: newWebPasskeyHandler(hub.passkey)}
		return server.httpHandler(), hub.passkey, base, cookieName
	}
	return hub.handler(), hub.passkey, base, cookieName
}

func nativeRequest(handler http.Handler, method, path, body, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost:8090"+path, strings.NewReader(body))
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func nativeChallenge() string { return nativeChallengeFor(nativeTestVerifier) }

// nativeSession creates a browser session. When recent is set, the session
// has just completed a passkey assertion (as /api/auth/reauth/finish would do).
func nativeSession(t *testing.T, runtime *hubPasskeyRuntime, cookieName string, recent bool) (*http.Cookie, passkeyauth.Principal) {
	t.Helper()
	issued, err := runtime.sessions.Create("credential")
	if err != nil {
		t.Fatal(err)
	}
	principal := passkeyauth.Principal{SessionID: issued.Info.ID, CredentialRecordID: issued.Info.CredentialRecordID}
	if recent {
		if err := runtime.sessions.GrantRecentAuth(principal); err != nil {
			t.Fatal(err)
		}
	}
	return &http.Cookie{Name: cookieName, Value: issued.Token}, principal
}

func nativeAuthorize(handler http.Handler, base, origin, challenge string, cookie *http.Cookie) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"challenge": challenge})
	return nativeRequest(handler, http.MethodPost, base+"/api/auth/native/authorize", string(body), origin, cookie)
}

func nativeCode(t *testing.T, w *httptest.ResponseRecorder, challenge string) string {
	t.Helper()
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Location") != "" {
		t.Fatalf("authorize=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
	}
	var result struct {
		Redirect string `json:"redirect"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(result.Redirect)
	if err != nil || u.Scheme != "termllm-auth" || u.Host != "callback" || u.Path != "" || u.Query().Get("state") != challenge || !nativeTokenValid(u.Query().Get("code")) || len(u.Query()) != 2 {
		t.Fatalf("invalid native callback: %q (%v)", result.Redirect, err)
	}
	return u.Query().Get("code")
}

func nativeRedeem(handler http.Handler, base, origin, code, verifier string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"code": code, "code_verifier": verifier})
	return nativeRequest(handler, http.MethodPost, base+"/api/auth/native/redeem", string(body), origin, nil)
}

func TestNativeHandoffWebAndHub(t *testing.T) {
	for _, web := range []bool{false, true} {
		name := "hub"
		if web {
			name = "web"
		}
		t.Run(name, func(t *testing.T) {
			h, runtime, base, cookieName := nativeTestServer(t, web)
			cookie, _ := nativeSession(t, runtime, cookieName, true)
			challenge := nativeChallenge()
			path := base + "/auth/native/" + challenge

			// Opening the handoff URL only renders an approval page.
			page := nativeRequest(h, http.MethodGet, path+"?redirect=https://evil.example/&code=leak", "", "", cookie)
			if page.Code != http.StatusOK || page.Header().Get("Location") != "" || strings.Contains(page.Body.String(), "termllm-auth") ||
				!strings.Contains(page.Body.String(), challenge) || page.Header().Get("X-Frame-Options") != "DENY" || page.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatalf("approval page=%d %v %s", page.Code, page.Header(), page.Body.String())
			}
			if len(runtime.nativeTickets.byHash) != 0 {
				t.Fatal("GET minted a native ticket")
			}

			for _, origin := range []string{"", "https://evil.example", "null"} {
				if w := nativeAuthorize(h, base, origin, challenge, cookie); w.Code != http.StatusForbidden {
					t.Fatalf("authorize from origin %q: %d %s", origin, w.Code, w.Body.String())
				}
			}
			code := nativeCode(t, nativeAuthorize(h, base, runtime.endpoint.Origin, challenge, cookie), challenge)
			if runtime.sessions.Count() != 1 {
				t.Fatal("ticket creation minted a browser session")
			}
			for _, origin := range []string{"", "https://evil.example", "null"} {
				w := nativeRedeem(h, base, origin, code, nativeTestVerifier)
				if w.Code != http.StatusForbidden || w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Set-Cookie") != "" {
					t.Fatalf("wrong origin %q: %d %v", origin, w.Code, w.Header())
				}
			}
			w := nativeRedeem(h, base, runtime.endpoint.Origin, code, nativeTestVerifier)
			if w.Code != http.StatusOK {
				t.Fatalf("redeem=%d %v %s", w.Code, w.Header(), w.Body.String())
			}
			cookies := w.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Name != cookieName || cookies[0].Path != base+"/" || cookies[0].SameSite != http.SameSiteStrictMode || !cookies[0].HttpOnly || cookies[0].Value == cookie.Value {
				t.Fatalf("wrong fresh cookie: %+v", cookies)
			}
			if runtime.sessions.Count() != 2 {
				t.Fatalf("want separate session, got %d", runtime.sessions.Count())
			}
			if w := nativeRequest(h, http.MethodGet, base+"/api/auth/session", "", "", cookies[0]); w.Code != http.StatusOK {
				t.Fatalf("new browser session unusable: %d %s", w.Code, w.Body.String())
			}
			if w := nativeRedeem(h, base, runtime.endpoint.Origin, code, nativeTestVerifier); w.Code != http.StatusUnauthorized || w.Header().Get("Set-Cookie") != "" {
				t.Fatalf("replayed ticket=%d %v", w.Code, w.Header())
			}
			// The redeemed session cannot chain a further handoff without a passkey.
			if w := nativeAuthorize(h, base, runtime.endpoint.Origin, challenge, cookies[0]); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "recent_auth_required") {
				t.Fatalf("redeemed session authorized without passkey: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestNativeHandoffRequiresFreshPasskeyPerTicket(t *testing.T) {
	for _, web := range []bool{false, true} {
		h, runtime, base, cookieName := nativeTestServer(t, web)
		challenge := nativeChallenge()
		stale, _ := nativeSession(t, runtime, cookieName, false)
		if w := nativeAuthorize(h, base, runtime.endpoint.Origin, challenge, stale); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "recent_auth_required") {
			t.Fatalf("authorize without recent auth=%d %s", w.Code, w.Body.String())
		}
		cookie, principal := nativeSession(t, runtime, cookieName, true)
		_ = nativeCode(t, nativeAuthorize(h, base, runtime.endpoint.Origin, challenge, cookie), challenge)
		if runtime.sessions.HasRecentAuth(principal) {
			t.Fatal("authorize did not consume recent authentication")
		}
		if w := nativeAuthorize(h, base, runtime.endpoint.Origin, challenge, cookie); w.Code != http.StatusForbidden {
			t.Fatalf("second ticket without passkey=%d", w.Code)
		}

		// Viewing the page never consumes or needs recent authentication.
		fresh, freshPrincipal := nativeSession(t, runtime, cookieName, true)
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			if w := nativeRequest(h, method, base+"/auth/native/"+challenge, "", "", fresh); w.Code != http.StatusOK || w.Header().Get("Location") != "" {
				t.Fatalf("%s approval page=%d %v", method, w.Code, w.Header())
			}
		}
		if !runtime.sessions.HasRecentAuth(freshPrincipal) || len(runtime.nativeTickets.byHash) != 1 {
			t.Fatal("viewing the approval page changed authorization state")
		}
		if w := nativeRequest(h, http.MethodPost, base+"/auth/native/"+challenge, "{}", runtime.endpoint.Origin, fresh); w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST to approval page=%d", w.Code)
		}
	}
}

func TestNativeHandoffPKCE(t *testing.T) {
	for _, web := range []bool{false, true} {
		h, runtime, base, cookieName := nativeTestServer(t, web)
		challenge := nativeChallenge()
		authorize := func() string {
			t.Helper()
			cookie, _ := nativeSession(t, runtime, cookieName, true)
			return nativeCode(t, nativeAuthorize(h, base, runtime.endpoint.Origin, challenge, cookie), challenge)
		}
		// Every mismatched verifier fails and burns the ticket, so a stolen code
		// cannot be brute forced.
		for _, verifier := range []string{"", "short", nativeTestVerifier + "x", strings.Repeat("a", 43), strings.Repeat("!", 43), challenge} {
			code, ok := runtime.nativeTickets.issue(passkeyauth.Principal{SessionID: "s", CredentialRecordID: "credential"}, challenge, "scope")
			if !ok {
				t.Fatal("issue failed")
			}
			if _, ok := runtime.nativeTickets.consume(code, verifier, "scope"); ok {
				t.Fatalf("verifier %q accepted", verifier)
			}
			if _, ok := runtime.nativeTickets.consume(code, nativeTestVerifier, "scope"); ok {
				t.Fatalf("ticket survived wrong verifier %q", verifier)
			}
		}
		code := authorize()
		if w := nativeRedeem(h, base, runtime.endpoint.Origin, code, strings.Repeat("a", 43)); w.Code != http.StatusUnauthorized || w.Header().Get("Set-Cookie") != "" {
			t.Fatalf("wrong verifier redeemed over HTTP: %d", w.Code)
		}
		if w := nativeRedeem(h, base, runtime.endpoint.Origin, code, nativeTestVerifier); w.Code != http.StatusUnauthorized {
			t.Fatalf("ticket survived a wrong HTTP verifier: %d", w.Code)
		}
		if w := nativeRedeem(h, base, runtime.endpoint.Origin, authorize(), nativeTestVerifier); w.Code != http.StatusOK {
			t.Fatalf("correct verifier=%d %s", w.Code, w.Body.String())
		}
		body, _ := json.Marshal(map[string]string{"code": authorize()})
		if w := nativeRequest(h, http.MethodPost, base+"/api/auth/native/redeem", string(body), runtime.endpoint.Origin, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("redeem without verifier=%d", w.Code)
		}
	}
}

func TestNativeVerifierGrammar(t *testing.T) {
	for value, want := range map[string]bool{
		strings.Repeat("a", 42):            false,
		strings.Repeat("a", 43):            true,
		strings.Repeat("a", 128):           true,
		strings.Repeat("a", 129):           false,
		strings.Repeat("-._~", 11):         true,
		strings.Repeat("a", 42) + "+":      false,
		strings.Repeat("a", 42) + "é":      false,
		strings.Repeat("a", 42) + " ":      false,
		strings.Repeat("A1z9", 10) + "_Z~": true,
	} {
		if got := validNativeVerifier(value); got != want {
			t.Errorf("validNativeVerifier(%q)=%v want %v", value, got, want)
		}
	}
}

func TestNativeHandoffSetupRedirect(t *testing.T) {
	for _, web := range []bool{false, true} {
		base := "/hub"
		if web {
			base = "/ui"
		}
		s := newTestPasskeyHub(t, base)
		var h http.Handler = s.handler()
		if web {
			h = (&serveServer{cfg: serveServerConfig{ui: true, requireAuth: true, basePath: base}, browserAuth: newWebPasskeyHandler(s.passkey)}).httpHandler()
		}
		path := base + "/auth/native/" + nativeChallenge()
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := nativeRequest(h, method, path+"?ignored=secret", "", "", nil)
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != base+"/auth/setup?return="+url.QueryEscape(path) {
				t.Fatalf("%s setup redirect=%d %v", method, w.Code, w.Header())
			}
		}
	}
}

// A login that returned to one handoff approves only that challenge.
func TestNativeHandoffLoginGrantIsBoundToChallenge(t *testing.T) {
	h, runtime, base, cookieName := nativeTestServer(t, false)
	cookie, principal := nativeSession(t, runtime, cookieName, false)
	if err := runtime.sessions.GrantNativeApproval(principal, nativeChallenge()); err != nil {
		t.Fatal(err)
	}
	other := nativeChallengeFor(nativeTestVerifier + "-other")
	if w := nativeAuthorize(h, base, runtime.endpoint.Origin, other, cookie); w.Code != http.StatusForbidden {
		t.Fatalf("grant approved a different challenge: %d %s", w.Code, w.Body.String())
	}
	// The mismatched attempt discarded the grant.
	if w := nativeAuthorize(h, base, runtime.endpoint.Origin, nativeChallenge(), cookie); w.Code != http.StatusForbidden {
		t.Fatalf("grant survived a mismatched approval: %d", w.Code)
	}
	if err := runtime.sessions.GrantNativeApproval(principal, nativeChallenge()); err != nil {
		t.Fatal(err)
	}
	_ = nativeCode(t, nativeAuthorize(h, base, runtime.endpoint.Origin, nativeChallenge(), cookie), nativeChallenge())
}

// Tickets are redeemable only through the mount that approved them, even when
// Hub and Web handlers share one passkey runtime; a wrong-mount attempt burns it.
func TestNativeHandoffTicketBoundToMount(t *testing.T) {
	hub := newTestPasskeyHub(t, "/hub")
	web := (&serveServer{cfg: serveServerConfig{ui: true, requireAuth: true, basePath: "/hub"}, browserAuth: newWebPasskeyHandler(hub.passkey)}).httpHandler()
	runtime := hub.passkey
	cookie, _ := nativeSession(t, runtime, hubSessionCookieName, true)
	code := nativeCode(t, nativeAuthorize(hub.handler(), "/hub", runtime.endpoint.Origin, nativeChallenge(), cookie), nativeChallenge())
	if w := nativeRedeem(web, "/hub", runtime.endpoint.Origin, code, nativeTestVerifier); w.Code != http.StatusUnauthorized || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("ticket redeemed through another mount: %d %s", w.Code, w.Body.String())
	}
	if w := nativeRedeem(hub.handler(), "/hub", runtime.endpoint.Origin, code, nativeTestVerifier); w.Code != http.StatusUnauthorized {
		t.Fatalf("ticket survived a wrong-mount redemption: %d", w.Code)
	}
}

// Malformed approvals are rate limited; successful approvals and the expected
// recent_auth_required step are not charged.
func TestNativeHandoffAuthorizeRateLimit(t *testing.T) {
	h, runtime, base, cookieName := nativeTestServer(t, false)
	cookie, principal := nativeSession(t, runtime, cookieName, false)
	for i := range 8 {
		if w := nativeAuthorize(h, base, runtime.endpoint.Origin, nativeChallenge(), cookie); w.Code != http.StatusForbidden {
			t.Fatalf("approval without passkey %d=%d", i, w.Code)
		}
		if err := runtime.sessions.GrantRecentAuth(principal); err != nil {
			t.Fatal(err)
		}
		if w := nativeAuthorize(h, base, runtime.endpoint.Origin, nativeChallenge(), cookie); w.Code != http.StatusOK {
			t.Fatalf("successful approval %d=%d", i, w.Code)
		}
	}
	limited := false
	for range 8 {
		w := nativeAuthorize(h, base, runtime.endpoint.Origin, "not-a-challenge", cookie)
		if w.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid challenge=%d", w.Code)
		}
	}
	if !limited {
		t.Fatal("failed approvals were not rate limited")
	}
}

func TestNativeHandoffAfterPasskeyLogin(t *testing.T) {
	for _, web := range []bool{false, true} {
		h, runtime, base, cookieName := nativeTestServer(t, web)
		key, credential := hubTestAssertionCredential(t)
		if _, err := runtime.store.CommitFirstCredential(credential, "Local key"); err != nil {
			t.Fatal(err)
		}
		challenge := nativeChallenge()
		path := base + "/auth/native/" + challenge
		login := nativeRequest(h, http.MethodGet, path, "", "", nil)
		if login.Code != http.StatusSeeOther || login.Header().Get("Location") != base+"/auth/login?return="+url.QueryEscape(path) {
			t.Fatalf("login redirect=%d %v", login.Code, login.Header())
		}
		passkeyLogin := func(returnPath string) *http.Cookie {
			t.Helper()
			beginBody, _ := json.Marshal(map[string]string{"return_path": returnPath})
			begin := nativeRequest(h, http.MethodPost, base+"/api/auth/login/begin", string(beginBody), runtime.endpoint.Origin, nil)
			if begin.Code != http.StatusOK {
				t.Fatalf("login begin=%d %s", begin.Code, begin.Body.String())
			}
			var options struct {
				PublicKey struct {
					Challenge string `json:"challenge"`
				} `json:"publicKey"`
			}
			if err := json.Unmarshal(begin.Body.Bytes(), &options); err != nil {
				t.Fatal(err)
			}
			assertion := hubTestAssertionBody(t, key, credential, options.PublicKey.Challenge, runtime.endpoint.Origin, runtime.endpoint.RPID)
			finish := nativeRequest(h, http.MethodPost, base+"/api/auth/login/finish", assertion, runtime.endpoint.Origin, begin.Result().Cookies()[0])
			if finish.Code != http.StatusOK {
				t.Fatalf("login finish=%d %s", finish.Code, finish.Body.String())
			}
			var result struct {
				Redirect string `json:"redirect"`
			}
			if err := json.Unmarshal(finish.Body.Bytes(), &result); err != nil || result.Redirect != strings.SplitN(returnPath, "?", 2)[0] {
				t.Fatalf("login return=%q err=%v", result.Redirect, err)
			}
			for _, c := range finish.Result().Cookies() {
				if c.Name == cookieName && c.SameSite == http.SameSiteStrictMode {
					return c
				}
			}
			t.Fatalf("login session=%v", finish.Result().Cookies())
			return nil
		}

		// An ordinary login is not recent authentication for native approval.
		ordinary := passkeyLogin(base + "/")
		if w := nativeAuthorize(h, base, runtime.endpoint.Origin, challenge, ordinary); w.Code != http.StatusForbidden {
			t.Fatalf("ordinary login authorized native handoff: %d", w.Code)
		}
		// The extra login and rejected approval above spent this peer's auth
		// burst; the flow below is what a real client performs.
		runtime.limiter = newHubAuthLimiter(nil)
		// A login returning to the handoff counts, avoiding a second prompt,
		// but still needs the explicit approval POST.
		session := passkeyLogin(path + "?code=not-retained")
		if w := nativeRequest(h, http.MethodGet, path, "", "", session); w.Code != http.StatusOK {
			t.Fatalf("approval page after login=%d", w.Code)
		}
		// The login approves only the native sign-in, not credential changes.
		var state struct {
			Recent bool `json:"recently_authenticated"`
		}
		if w := nativeRequest(h, http.MethodGet, base+"/api/auth/session", "", "", session); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Recent {
			t.Fatalf("native login widened recent auth: %d %s", w.Code, w.Body.String())
		}
		code := nativeCode(t, nativeAuthorize(h, base, runtime.endpoint.Origin, challenge, session), challenge)
		if w := nativeRedeem(h, base, runtime.endpoint.Origin, code, nativeTestVerifier); w.Code != http.StatusOK {
			t.Fatalf("redeem after login=%d %s", w.Code, w.Body.String())
		}
	}
}

func TestNativeHandoffUnauthenticatedAndInvalidChallenge(t *testing.T) {
	for _, web := range []bool{false, true} {
		h, runtime, base, cookieName := nativeTestServer(t, web)
		challenge := nativeChallenge()
		path := base + "/auth/native/" + challenge
		w := nativeRequest(h, http.MethodGet, path+"?code=secret&return=https://evil.example", "", "", nil)
		want := base + "/auth/login?return=" + url.QueryEscape(path)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != want || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("unauth redirect=%d %v", w.Code, w.Header())
		}
		for _, bad := range []string{"", "short", challenge + "/extra", strings.Repeat("!", 43), strings.Repeat("A", 42) + "B"} {
			w := nativeRequest(h, http.MethodGet, base+"/auth/native/"+bad, "", "", nil)
			if w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" {
				t.Fatalf("bad challenge %q: %d %v", bad, w.Code, w.Header())
			}
		}
		if w := nativeAuthorize(h, base, runtime.endpoint.Origin, challenge, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated authorize=%d", w.Code)
		}
		cookie, principal := nativeSession(t, runtime, cookieName, true)
		if w := nativeAuthorize(h, base, runtime.endpoint.Origin, "short", cookie); w.Code != http.StatusBadRequest {
			t.Fatalf("bad authorize challenge=%d", w.Code)
		}
		code := nativeCode(t, nativeAuthorize(h, base, runtime.endpoint.Origin, challenge, cookie), challenge)
		if err := runtime.sessions.Logout(principal); err != nil {
			t.Fatal(err)
		}
		if w := nativeRedeem(h, base, runtime.endpoint.Origin, code, nativeTestVerifier); w.Code != http.StatusUnauthorized || w.Header().Get("Set-Cookie") != "" {
			t.Fatalf("revoked principal redeemed=%d %v", w.Code, w.Header())
		}
	}
}

func TestNativeHandoffTicketExpiryAndConcurrency(t *testing.T) {
	h, runtime, base, cookieName := nativeTestServer(t, false)
	now := time.Now()
	runtime.nativeTickets.now = func() time.Time { return now }
	challenge := nativeChallenge()
	cookie, principal := nativeSession(t, runtime, cookieName, true)
	code := nativeCode(t, nativeAuthorize(h, base, runtime.endpoint.Origin, challenge, cookie), challenge)
	now = now.Add(nativeTicketLifetime)
	if w := nativeRedeem(h, base, runtime.endpoint.Origin, code, nativeTestVerifier); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired code=%d", w.Code)
	}
	if err := runtime.sessions.GrantRecentAuth(principal); err != nil {
		t.Fatal(err)
	}
	code = nativeCode(t, nativeAuthorize(h, base, runtime.endpoint.Origin, challenge, cookie), challenge)
	var wg sync.WaitGroup
	statuses := make(chan int, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- nativeRedeem(h, base, runtime.endpoint.Origin, code, nativeTestVerifier).Code
		}()
	}
	wg.Wait()
	close(statuses)
	successes := 0
	for status := range statuses {
		if status == http.StatusOK {
			successes++
		} else if status != http.StatusUnauthorized && status != http.StatusTooManyRequests {
			t.Fatalf("concurrent redeem=%d", status)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent redeems succeeded %d times", successes)
	}
}

func TestNativeHandoffTicketCapacity(t *testing.T) {
	var tickets nativeHandoffTickets
	now := time.Now()
	tickets.now = func() time.Time { return now }
	tickets.byHash = make(map[[32]byte]nativeHandoffTicket)
	for i := range nativeTicketCapacity {
		var hash [32]byte
		hash[0], hash[1] = byte(i>>8), byte(i)
		tickets.byHash[hash] = nativeHandoffTicket{expiresAt: now.Add(nativeTicketLifetime)}
	}
	if _, ok := tickets.issue(passkeyauth.Principal{SessionID: "test"}, nativeChallenge(), "scope"); ok || len(tickets.byHash) != nativeTicketCapacity {
		t.Fatal("issued beyond bounded capacity")
	}
	now = now.Add(nativeTicketLifetime)
	if _, ok := tickets.issue(passkeyauth.Principal{SessionID: "test"}, nativeChallenge(), "scope"); !ok || len(tickets.byHash) != 1 {
		t.Fatal("expired tickets did not release capacity")
	}
}

func TestNativeHandoffRejectsBearerAndBadRedeemRequests(t *testing.T) {
	s := newTestPasskeyHub(t, "/hub")
	s.token = "legacy-token"
	s.passkey.bootstrap, _ = passkeyauth.NewGrants(passkeyauth.GrantBootstrap, nil, nil, nil)
	h := s.handler()
	body, _ := json.Marshal(map[string]string{"challenge": nativeChallenge()})
	r := httptest.NewRequest(http.MethodPost, "http://localhost:8090/hub/api/auth/native/authorize", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer legacy-token")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", s.passkey.endpoint.Origin)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "termllm-auth") {
		t.Fatalf("bearer minted native ticket: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		method, body, contentType string
		want                      int
	}{
		{http.MethodGet, "", "", http.StatusMethodNotAllowed},
		{http.MethodPost, `{"code":"bad","code_verifier":"bad"}`, "application/json", http.StatusUnauthorized},
		{http.MethodPost, `{"code":123}`, "application/json", http.StatusBadRequest},
		{http.MethodPost, `{"code":"x"}`, "text/plain", http.StatusUnsupportedMediaType},
	} {
		r := httptest.NewRequest(tc.method, "http://localhost:8090/hub/api/auth/native/redeem", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.contentType)
		r.Header.Set("Origin", s.passkey.endpoint.Origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want || w.Header().Get("Set-Cookie") != "" {
			t.Fatalf("bad redeem request %s %q: %d %v", tc.method, tc.body, w.Code, w.Header())
		}
	}
}

// First-passkey setup started from a native handoff returns to the approval
// page, and the registration ceremony approves that one sign-in.
func TestNativeHandoffAfterFirstPasskeySetup(t *testing.T) {
	for _, tc := range []struct{ name, returnPath, wantRedirect string }{
		{"native", "/auth/native/" + nativeChallenge(), "/auth/native/" + nativeChallenge()},
		{"default", "", "/"},
		{"foreign", "https://evil.example/auth/native/" + nativeChallenge(), "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestPasskeyHub(t, "")
			h := s.handler()
			verify := nativeRequest(h, http.MethodPost, "/api/auth/bootstrap/verify", `{"code":"abcdefghijklmnopqrst"}`, s.passkey.endpoint.Origin, nil)
			if verify.Code != http.StatusOK {
				t.Fatalf("verify=%d %s", verify.Code, verify.Body.String())
			}
			grant := verify.Result().Cookies()[0]
			challenge, ceremony := beginHubTestBootstrapRegistration(t, s, grant, "Primary", "192.0.2.1:1234")
			_, credential := hubTestAssertionCredential(t)
			body := hubTestRegistrationBody(t, credential, challenge, s.passkey.endpoint.Origin, s.passkey.endpoint.RPID)
			r := httptest.NewRequest(http.MethodPost, "http://localhost:8090/api/auth/bootstrap/register/finish?return="+url.QueryEscape(tc.returnPath), strings.NewReader(body))
			r.Header.Set("Origin", s.passkey.endpoint.Origin)
			r.Header.Set("Content-Type", "application/json")
			r.AddCookie(grant)
			r.AddCookie(ceremony)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var result struct {
				Redirect string `json:"redirect"`
			}
			if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Redirect != tc.wantRedirect {
				t.Fatalf("setup finish=%d %s", w.Code, w.Body.String())
			}
			var session *http.Cookie
			for _, c := range w.Result().Cookies() {
				if c.Name == hubSessionCookieName {
					session = c
				}
			}
			if session == nil {
				t.Fatalf("no session cookie: %v", w.Result().Cookies())
			}
			authorized := nativeAuthorize(h, "", s.passkey.endpoint.Origin, nativeChallenge(), session)
			if tc.name != "native" {
				if authorized.Code != http.StatusForbidden {
					t.Fatalf("setup without native return approved a handoff: %d", authorized.Code)
				}
				return
			}
			code := nativeCode(t, authorized, nativeChallenge())
			if w := nativeRedeem(h, "", s.passkey.endpoint.Origin, code, nativeTestVerifier); w.Code != http.StatusOK {
				t.Fatalf("redeem after setup=%d %s", w.Code, w.Body.String())
			}
		})
	}
}
