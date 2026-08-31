package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
