package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

func TestCoordinatorUsesSDKAuthorizationAndPersistsSession(t *testing.T) {
	var server *httptest.Server
	var registrations atomic.Int32
	var revocations atomic.Int32
	var sawResource atomic.Bool
	var sawPKCE atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/resource", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/resource", scope="read write"`, server.URL))
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/resource", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"resource": server.URL + "/resource", "authorization_servers": []string{server.URL},
			"scopes_supported": []string{"read", "write"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize",
			"token_endpoint": server.URL + "/token", "registration_endpoint": server.URL + "/register",
			"revocation_endpoint": server.URL + "/revoke", "response_types_supported": []string{"code"},
			"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported":          []string{"none"},
			"code_challenge_methods_supported":               []string{"S256"},
			"authorization_response_iss_parameter_supported": true,
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		registrations.Add(1)
		var metadata oauthex.ClientRegistrationMetadata
		if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil {
			t.Errorf("decode registration: %v", err)
		}
		if !containsString(metadata.GrantTypes, "refresh_token") {
			t.Errorf("registration grant_types = %v, want refresh_token", metadata.GrantTypes)
		}
		writeTestJSON(w, map[string]any{
			"client_id": "dynamic-client", "redirect_uris": metadata.RedirectURIs,
			"token_endpoint_auth_method": "none", "grant_types": metadata.GrantTypes,
			"response_types": []string{"code"},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		if r.Form.Get("resource") == server.URL+"/resource" {
			sawResource.Store(true)
		}
		verifier := r.Form.Get("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		if verifier != "" && base64.RawURLEncoding.EncodeToString(sum[:]) != "" {
			sawPKCE.Store(true)
		}
		writeTestJSON(w, map[string]any{
			"access_token": "access-secret", "refresh_token": "refresh-secret",
			"token_type": "Bearer", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		revocations.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	store := NewFileStore(t.TempDir() + "/mcp_oauth.json")
	coordinator := NewCoordinator(store)
	flow, err := coordinator.Start(t.Context(), server.URL+"/resource", Options{HTTPClient: server.Client(), Scopes: []string{"configured"}}, "http://127.0.0.1/callback", false)
	if err != nil {
		t.Fatal(err)
	}
	authorizeURL, err := url.Parse(flow.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	state := authorizeURL.Query().Get("state")
	challenge := authorizeURL.Query().Get("code_challenge")
	if state == "" || challenge == "" || authorizeURL.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization URL missing SDK PKCE/state: %s", flow.AuthorizationURL)
	}
	if authorizeURL.Query().Get("resource") != server.URL+"/resource" {
		t.Fatalf("authorization resource = %q", authorizeURL.Query().Get("resource"))
	}
	if !containsString(strings.Fields(authorizeURL.Query().Get("scope")), "configured") {
		t.Fatalf("authorization scope = %q, want configured scope", authorizeURL.Query().Get("scope"))
	}
	if _, ok := coordinator.CompleteCallback("wrong-state", "code", server.URL, ""); ok {
		t.Fatal("mismatched state was accepted")
	}
	if id, ok := coordinator.CompleteCallback(state, "code", server.URL, ""); !ok || id != flow.ID {
		t.Fatalf("callback accepted = %v, id = %q", ok, id)
	}
	completed, err := coordinator.Wait(t.Context(), flow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != FlowSucceeded {
		t.Fatalf("flow state = %s, error = %s", completed.State, completed.Error)
	}
	if registrations.Load() != 1 || !sawResource.Load() || !sawPKCE.Load() {
		t.Fatalf("DCR=%d resource=%v PKCE=%v", registrations.Load(), sawResource.Load(), sawPKCE.Load())
	}
	status := coordinator.Status(server.URL + "/resource")
	if status.State != AuthSignedIn || status.Issuer != server.URL || !containsString(status.Scopes, "read") {
		t.Fatalf("status = %+v", status)
	}

	// A new coordinator restores both the DCR client and token through v1.7's
	// InitialTokenSource hook without another registration or browser flow.
	restarted := NewCoordinator(store)
	handler, err := restarted.Handler(server.URL+"/resource", Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	source, err := handler.TokenSource(t.Context())
	if err != nil || source == nil {
		t.Fatalf("restored TokenSource = %v, err = %v", source, err)
	}
	token, err := source.Token()
	if err != nil || token.AccessToken != "access-secret" {
		t.Fatalf("restored token = %#v, err = %v", token, err)
	}
	if registrations.Load() != 1 {
		t.Fatalf("registrations after restart = %d, want 1", registrations.Load())
	}
	if err := restarted.Logout(t.Context(), server.URL+"/resource", false); err != nil {
		t.Fatal(err)
	}
	if revocations.Load() != 1 {
		t.Fatalf("revocations = %d, want 1", revocations.Load())
	}
	if _, err := store.Load(server.URL + "/resource"); err != ErrNotFound {
		t.Fatalf("Load after logout error = %v, want ErrNotFound", err)
	}
}

func TestCoordinatorDefaultsToAllAdvertisedScopes(t *testing.T) {
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/resource", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/resource", scope="profile"`, server.URL))
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/resource", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"resource": server.URL + "/resource", "authorization_servers": []string{server.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize",
			"token_endpoint": server.URL + "/token", "response_types_supported": []string{"code"},
			"token_endpoint_auth_methods_supported": []string{"none"},
			"code_challenge_methods_supported":      []string{"S256"},
			"scopes_supported":                      []string{"profile", "content:read", "content:write"},
		})
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	tests := []struct {
		name        string
		options     Options
		wantScopes  []string
		rejectScope string
	}{
		{
			name:       "omitted scopes request every advertised scope",
			options:    Options{HTTPClient: server.Client(), ClientID: "registered-client"},
			wantScopes: []string{"profile", "content:read", "content:write"},
		},
		{
			name: "configured scopes take precedence",
			options: Options{
				HTTPClient: server.Client(), ClientID: "registered-client",
				Scopes: []string{"content:read"}, ScopesConfigured: true,
			},
			wantScopes:  []string{"profile", "content:read"},
			rejectScope: "content:write",
		},
		{
			name: "explicitly empty scopes retain required challenge scopes only",
			options: Options{
				HTTPClient: server.Client(), ClientID: "registered-client",
				Scopes: []string{}, ScopesConfigured: true,
			},
			wantScopes:  []string{"profile"},
			rejectScope: "content:read",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := NewCoordinator(NewFileStore(t.TempDir() + "/oauth.json"))
			flow, err := coordinator.Start(t.Context(), server.URL+"/resource", test.options, "http://127.0.0.1/callback", false)
			if err != nil {
				t.Fatal(err)
			}
			defer coordinator.Cancel(server.URL+"/resource", flow.ID)
			authorizeURL, err := url.Parse(flow.AuthorizationURL)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Fields(authorizeURL.Query().Get("scope"))
			for _, scope := range test.wantScopes {
				if !containsString(got, scope) {
					t.Errorf("authorization scopes = %v, missing %q", got, scope)
				}
			}
			if test.rejectScope != "" && containsString(got, test.rejectScope) {
				t.Errorf("authorization scopes = %v, unexpectedly included %q", got, test.rejectScope)
			}
		})
	}
}

func TestConcurrentCoordinatorsAdoptRotatedRefresh(t *testing.T) {
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		refreshes.Add(1)
		writeTestJSON(w, map[string]any{
			"access_token": "rotated-access", "refresh_token": "rotated-refresh",
			"token_type": "Bearer", "expires_in": 3600,
		})
	}))
	defer server.Close()
	endpoint := server.URL + "/resource"
	store := NewFileStore(t.TempDir() + "/mcp_oauth.json")
	_, err := store.Update(endpoint, func(*Session) (*Session, error) {
		return &Session{
			Endpoint: endpoint, Issuer: server.URL,
			Config: OAuth2Config{ClientID: "client", Endpoint: oauth2.Endpoint{AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token"}},
			Token:  &oauth2.Token{AccessToken: "expired", RefreshToken: "original-refresh", Expiry: time.Now().Add(-time.Hour)},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	coordinators := []*Coordinator{NewCoordinator(store), NewCoordinator(NewFileStore(store.Path()))}
	var wg sync.WaitGroup
	errs := make(chan error, len(coordinators))
	for _, coordinator := range coordinators {
		wg.Add(1)
		go func(coordinator *Coordinator) {
			defer wg.Done()
			handler, err := coordinator.Handler(endpoint, Options{HTTPClient: server.Client()})
			if err != nil {
				errs <- err
				return
			}
			source, err := handler.TokenSource(context.Background())
			if err == nil {
				var token *oauth2.Token
				token, err = source.Token()
				if err == nil && token.AccessToken != "rotated-access" {
					err = fmt.Errorf("access token = %q", token.AccessToken)
				}
			}
			errs <- err
		}(coordinator)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", refreshes.Load())
	}
	stored, err := store.Load(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Token.RefreshToken != "rotated-refresh" {
		t.Fatalf("stored refresh token = %q", stored.Token.RefreshToken)
	}
}

func TestRefreshSendsResourceIndicatorAndKeepsUnrotatedRefreshToken(t *testing.T) {
	var form atomic.Pointer[url.Values]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		values := r.Form
		form.Store(&values)
		// No refresh_token in the response: the server does not rotate.
		writeTestJSON(w, map[string]any{"access_token": "fresh-access", "token_type": "Bearer", "expires_in": 3600})
	}))
	defer server.Close()
	endpoint := server.URL + "/resource"
	store := NewFileStore(t.TempDir() + "/mcp_oauth.json")
	_, err := store.Update(endpoint, func(*Session) (*Session, error) {
		return &Session{
			Endpoint: endpoint, Issuer: server.URL,
			Config: OAuth2Config{
				ClientID: "client",
				Endpoint: oauth2.Endpoint{
					AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token",
					// Public DCR clients are persisted with in-params auth.
					AuthStyle: oauth2.AuthStyleInParams,
				},
				RedirectURL: "http://127.0.0.1/callback",
			},
			Token: &oauth2.Token{AccessToken: "expired", RefreshToken: "original-refresh", Expiry: time.Now().Add(-time.Hour)},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewCoordinator(store).Handler(endpoint, Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	source, err := handler.TokenSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "fresh-access" || token.RefreshToken != "original-refresh" {
		t.Fatalf("token = %+v, want fresh access with retained refresh token", token)
	}
	sent := form.Load()
	if sent == nil {
		t.Fatal("no token request was made")
	}
	if got := sent.Get("grant_type"); got != "refresh_token" {
		t.Fatalf("grant_type = %q", got)
	}
	if got := sent.Get("refresh_token"); got != "original-refresh" {
		t.Fatalf("refresh_token = %q", got)
	}
	if got := sent.Get("resource"); got != endpoint {
		t.Fatalf("resource = %q, want %q", got, endpoint)
	}
	if got := sent.Get("client_id"); got != "client" {
		t.Fatalf("client_id = %q", got)
	}
	if sent.Has("redirect_uri") {
		t.Fatalf("refresh request carried redirect_uri: %v", *sent)
	}
	stored, err := store.Load(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Token.AccessToken != "fresh-access" || stored.Token.RefreshToken != "original-refresh" {
		t.Fatalf("stored token = %+v", stored.Token)
	}
}

func TestClassifyRefreshErrorTreatsBindingRejectionsAsRejected(t *testing.T) {
	for _, code := range []string{"invalid_grant", "invalid_client", "unauthorized_client", "invalid_target"} {
		err := classifyRefreshError(&oauth2.RetrieveError{ErrorCode: code, ErrorDescription: "nope"})
		if !errors.Is(err, ErrRefreshRejected) {
			t.Errorf("%s classified as %v, want ErrRefreshRejected", code, err)
		}
	}
	for _, code := range []string{"server_error", "temporarily_unavailable", ""} {
		err := classifyRefreshError(&oauth2.RetrieveError{ErrorCode: code, Response: &http.Response{StatusCode: http.StatusServiceUnavailable}})
		if errors.Is(err, ErrRefreshRejected) {
			t.Errorf("%q classified as rejected, want temporary", code)
		}
	}
}

func TestInteractiveStartHonorsStoredClientRedirectCompatibility(t *testing.T) {
	var server *httptest.Server
	var registrations atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/resource", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/resource"`, server.URL))
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/resource", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"resource": server.URL + "/resource", "authorization_servers": []string{server.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize",
			"token_endpoint": server.URL + "/token", "registration_endpoint": server.URL + "/register",
			"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		registrations.Add(1)
		var metadata oauthex.ClientRegistrationMetadata
		_ = json.NewDecoder(r.Body).Decode(&metadata)
		writeTestJSON(w, map[string]any{
			"client_id": "dynamic-2", "redirect_uris": metadata.RedirectURIs,
			"token_endpoint_auth_method": "none", "grant_types": metadata.GrantTypes,
			"response_types": []string{"code"},
		})
	})
	server = httptest.NewServer(mux)
	defer server.Close()
	endpoint := server.URL + "/resource"

	tests := []struct {
		name              string
		newRedirect       string
		wantClientID      string
		wantRegistrations int32
	}{
		{
			// RFC 8252 loopback redirects vary by port between CLI logins.
			name: "loopback port change reuses stored client", newRedirect: "http://127.0.0.1:41234/callback",
			wantClientID: "dynamic-1", wantRegistrations: 0,
		},
		{
			// A serve callback is not registered for the stored loopback DCR
			// client; a compliant AS would reject it, so re-register instead.
			name: "web redirect re-registers", newRedirect: "https://app.example/ui/v1/mcp/oauth/callback",
			wantClientID: "dynamic-2", wantRegistrations: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registrations.Store(0)
			store := NewFileStore(t.TempDir() + "/mcp_oauth.json")
			_, err := store.Update(endpoint, func(*Session) (*Session, error) {
				return &Session{
					Endpoint: endpoint, Issuer: server.URL,
					Config: OAuth2Config{
						ClientID:    "dynamic-1",
						Endpoint:    oauth2.Endpoint{AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token"},
						RedirectURL: "http://127.0.0.1:39999/callback",
					},
					Token: &oauth2.Token{AccessToken: "expired", Expiry: time.Now().Add(-time.Hour)},
				}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			coordinator := NewCoordinator(store)
			flow, err := coordinator.Start(t.Context(), endpoint, Options{HTTPClient: server.Client()}, tt.newRedirect, false)
			if err != nil {
				t.Fatal(err)
			}
			defer coordinator.Cancel(endpoint, flow.ID)
			authorizeURL, err := url.Parse(flow.AuthorizationURL)
			if err != nil {
				t.Fatal(err)
			}
			if got := authorizeURL.Query().Get("client_id"); got != tt.wantClientID {
				t.Fatalf("client_id = %q, want %q", got, tt.wantClientID)
			}
			if got := authorizeURL.Query().Get("redirect_uri"); got != tt.newRedirect {
				t.Fatalf("redirect_uri = %q, want %q", got, tt.newRedirect)
			}
			if got := registrations.Load(); got != tt.wantRegistrations {
				t.Fatalf("registrations = %d, want %d", got, tt.wantRegistrations)
			}
		})
	}
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

type closeTrackingBody struct {
	closed bool
	reader *strings.Reader
	read   int
}

func (b *closeTrackingBody) Read(p []byte) (int, error) {
	if b.reader == nil {
		return 0, io.EOF
	}
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}
func (b *closeTrackingBody) Close() error { b.closed = true; return nil }

func TestBackgroundAuthorizeChallenge(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		challenge string
		wantAuth  bool
	}{
		{"unauthorized", http.StatusUnauthorized, `Bearer error="invalid_token"`, true},
		{"scope required", http.StatusForbidden, `Bearer error="insufficient_scope", scope="write"`, true},
		{"forbidden", http.StatusForbidden, `Bearer error="invalid_token"`, false},
		{"forbidden without challenge", http.StatusForbidden, "", false},
		{"first bearer error wins", http.StatusForbidden, `Bearer error="invalid_token", Bearer error="insufficient_scope"`, false},
		{"malformed unauthorized", http.StatusUnauthorized, `Bearer error="`, true},
		{"malformed forbidden", http.StatusForbidden, `Bearer error="`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.NotFound(w, r)
			}))
			defer server.Close()
			coordinator := NewCoordinator(NewFileStore(t.TempDir() + "/oauth.json"))
			handler, err := coordinator.Handler(server.URL+"/mcp", Options{HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			body := &closeTrackingBody{}
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Www-Authenticate": []string{tc.challenge}}, Body: body}
			req := httptest.NewRequest(http.MethodPost, server.URL+"/mcp", nil)
			err = handler.Authorize(t.Context(), req, resp)
			if tc.wantAuth && !errors.Is(err, ErrAuthenticationRequired) || !tc.wantAuth && err != nil {
				t.Errorf("Authorize error = %v, want auth-required = %v", err, tc.wantAuth)
			}
			if !body.closed {
				t.Error("Authorize did not close response body")
			}
			if got := requests.Load(); got != 0 {
				t.Errorf("background authorization made %d network requests", got)
			}
		})
	}
}

func TestBackgroundAuthorizeInvalidatesOnlyRejectedStoredToken(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		challenge string
		bearer    string
		refresh   string
		rotate    bool
		noGrant   bool
		wantState AuthState
	}{
		{name: "rejected token", status: http.StatusUnauthorized, bearer: "stored", wantState: AuthRequired},
		{name: "refreshable rejected token", status: http.StatusUnauthorized, bearer: "stored", refresh: "refresh", wantState: AuthExpired},
		{name: "scope step-up keeps grant", status: http.StatusForbidden, challenge: `Bearer error="insufficient_scope"`, bearer: "stored", wantState: AuthSignedIn},
		{name: "different bearer", status: http.StatusUnauthorized, bearer: "different", wantState: AuthSignedIn},
		{name: "concurrently rotated token", status: http.StatusUnauthorized, bearer: "stored", rotate: true, wantState: AuthSignedIn},
		{name: "no bearer", status: http.StatusUnauthorized, wantState: AuthSignedIn},
		{name: "ordinary forbidden", status: http.StatusForbidden, challenge: `Bearer error="invalid_token"`, bearer: "stored", wantState: AuthSignedIn},
		{name: "no grant", status: http.StatusUnauthorized, bearer: "stored", noGrant: true, wantState: AuthSignedOut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := "https://example.test/mcp"
			store := NewFileStore(t.TempDir() + "/oauth.json")
			if !tc.noGrant {
				_, err := store.Update(endpoint, func(*Session) (*Session, error) {
					return &Session{Endpoint: endpoint, Config: OAuth2Config{ClientID: "client"}, Token: &oauth2.Token{AccessToken: "stored", RefreshToken: tc.refresh, Expiry: time.Now().Add(time.Hour)}}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			coordinator := NewCoordinator(store)
			handler, err := coordinator.Handler(endpoint, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.rotate {
				_, err := store.Update(endpoint, func(current *Session) (*Session, error) { current.Token.AccessToken = "rotated"; return current, nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			before, _ := store.Load(endpoint)
			req := httptest.NewRequest(http.MethodPost, endpoint, nil)
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Www-Authenticate": []string{tc.challenge}}, Body: io.NopCloser(strings.NewReader(""))}
			err = handler.Authorize(t.Context(), req, resp)
			if tc.status == http.StatusUnauthorized || tc.challenge == `Bearer error="insufficient_scope"` {
				if !errors.Is(err, ErrAuthenticationRequired) {
					t.Fatalf("Authorize error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := coordinator.Status(endpoint).State; got != tc.wantState {
				t.Errorf("Status = %s, want %s", got, tc.wantState)
			}
			after, err := store.Load(endpoint)
			if tc.noGrant {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing grant changed: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantState == AuthSignedIn {
				if after.Version != before.Version || !after.Token.Expiry.Equal(before.Token.Expiry) || after.Token.AccessToken != before.Token.AccessToken {
					t.Error("unrelated grant was changed")
				}
			} else {
				if !after.Token.Expiry.Before(time.Now()) {
					t.Error("rejected access token is still usable")
				}
				if after.Token.AccessToken != before.Token.AccessToken || after.Token.RefreshToken != before.Token.RefreshToken {
					t.Error("invalidation changed token material")
				}
			}
		})
	}
}

func TestRejectedStoredTokenAllowsNonForcedStart(t *testing.T) {
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, server.URL+"/.well-known/oauth-protected-resource/mcp"))
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"resource": server.URL + "/mcp", "authorization_servers": []string{server.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"}})
	})
	server = httptest.NewServer(mux)
	defer server.Close()
	endpoint := server.URL + "/mcp"
	store := NewFileStore(t.TempDir() + "/oauth.json")
	_, err := store.Update(endpoint, func(*Session) (*Session, error) {
		return &Session{Endpoint: endpoint, Issuer: server.URL, Config: OAuth2Config{ClientID: "client", RedirectURL: "http://127.0.0.1/callback"}, Token: &oauth2.Token{AccessToken: "stored", Expiry: time.Now().Add(time.Hour)}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(store)
	handler, err := coordinator.Handler(endpoint, Options{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, endpoint, nil)
	req.Header.Set("Authorization", "Bearer stored")
	if err := handler.Authorize(t.Context(), req, &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}); !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatal(err)
	}
	flow, err := coordinator.Start(t.Context(), endpoint, Options{HTTPClient: server.Client()}, "http://127.0.0.1/callback", false)
	if err != nil {
		t.Fatalf("non-forced Start after rejection: %v", err)
	}
	defer coordinator.Cancel(endpoint, flow.ID)
	if flow.State != FlowPending || flow.AuthorizationURL == "" {
		t.Fatal("sign-in did not proceed")
	}
}

func TestBackgroundAuthorizeResponseBody(t *testing.T) {
	for _, name := range []string{"nil", "large"} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() != nil {
					t.Errorf("Authorize panicked for %s body", name)
				}
			}()
			coordinator := NewCoordinator(NewFileStore(t.TempDir() + "/oauth.json"))
			handler, err := coordinator.Handler("https://example.test/mcp", Options{})
			if err != nil {
				t.Fatal(err)
			}
			resp := &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header)}
			body := &closeTrackingBody{reader: strings.NewReader(strings.Repeat("x", 8192))}
			if name == "large" {
				resp.Body = body
			}
			if err := handler.Authorize(t.Context(), httptest.NewRequest(http.MethodPost, "https://example.test/mcp", nil), resp); !errors.Is(err, ErrAuthenticationRequired) {
				t.Fatal(err)
			}
			if name == "large" && (!body.closed || body.read != 4096) {
				t.Fatalf("response drained %d bytes, closed=%v", body.read, body.closed)
			}
		})
	}
}

func TestAuthStatusGrantRevisionTracksStoreUpdates(t *testing.T) {
	endpoint := "https://example.test/mcp"
	store := NewFileStore(t.TempDir() + "/oauth.json")
	_, err := store.Update(endpoint, func(*Session) (*Session, error) { return testSession(endpoint), nil })
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(store)
	before := coordinator.Status(endpoint)
	if before.GrantRevision == "" || before.GrantRevision != coordinator.Status(endpoint).GrantRevision {
		t.Fatal("grant revision is absent or unstable across reads")
	}
	_, err = store.Update(endpoint, func(current *Session) (*Session, error) {
		current.Config.Scopes = []string{"read", "write"}
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after := coordinator.Status(endpoint)
	if before.GrantRevision == after.GrantRevision || !before.ExpiresAt.Equal(after.ExpiresAt) {
		t.Fatal("grant update with unchanged expiry did not change revision")
	}
	data, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), after.GrantRevision) {
		t.Fatal("internal grant revision was serialized")
	}
	coordinator.flows["pending"] = &flowRecord{flow: Flow{State: FlowPending}}
	coordinator.byEndpoint[endpoint] = "pending"
	waiting := coordinator.Status(endpoint)
	if waiting.State != AuthWaiting || waiting.GrantRevision != after.GrantRevision {
		t.Fatal("pending sign-in obscured the existing grant revision")
	}
}
