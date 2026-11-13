package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

type identityTestServer struct {
	*httptest.Server
	registrations atomic.Int32
	tokens        atomic.Int32
	metadataReads atomic.Int32
	resourceReads atomic.Int32
	verifier      string
}

func newIdentityTestServer(t *testing.T, change func(map[string]any, int32), tokenError bool) *identityTestServer {
	t.Helper()
	s := new(identityTestServer)
	mux := http.NewServeMux()
	mux.HandleFunc("/resource", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, s.URL+"/prm"))
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/prm", func(w http.ResponseWriter, r *http.Request) {
		s.resourceReads.Add(1)
		writeTestJSON(w, map[string]any{"resource": s.URL + "/resource", "authorization_servers": []string{s.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		metadata := map[string]any{
			"issuer": s.URL, "authorization_endpoint": s.URL + "/authorize", "token_endpoint": s.URL + "/token",
			"registration_endpoint": s.URL + "/register", "response_types_supported": []string{"code"},
			"code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{"read", "write"},
			"token_endpoint_auth_methods_supported": []string{"none"}, "client_id_metadata_document_supported": true,
			"authorization_response_iss_parameter_supported": true,
		}
		count := s.metadataReads.Add(1)
		if change != nil {
			change(metadata, count)
		}
		writeTestJSON(w, metadata)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		s.registrations.Add(1)
		var metadata oauthex.ClientRegistrationMetadata
		if err := json.NewDecoder(r.Body).Decode(&metadata); err != nil {
			t.Errorf("decode DCR: %v", err)
		}
		writeTestJSON(w, map[string]any{"client_id": "dynamic-client", "redirect_uris": metadata.RedirectURIs, "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		s.tokens.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		if r.Form.Get("grant_type") == "refresh_token" && (r.Header.Get("Authorization") != "" || r.Form.Get("client_id") != builtinClientIDMetadataURL) {
			w.WriteHeader(http.StatusUnauthorized)
			writeTestJSON(w, map[string]any{"error": "invalid_client"})
			return
		}
		s.verifier = r.Form.Get("code_verifier")
		if tokenError {
			// SDK CIMD uses oauth2's auto auth-style probe (up to two token
			// requests), but both requests must retain the same selected identity.
			clientID := r.Form.Get("client_id")
			if clientID == "" {
				clientID, _, _ = r.BasicAuth()
				clientID, _ = url.QueryUnescape(clientID)
			}
			if clientID != builtinClientIDMetadataURL {
				t.Errorf("token exchange downgraded identity to %q", clientID)
			}
			w.WriteHeader(http.StatusBadRequest)
			writeTestJSON(w, map[string]any{"error": "invalid_client", "error_description": "untrusted token endpoint description"})
			return
		}
		writeTestJSON(w, map[string]any{"access_token": "access", "refresh_token": "refresh", "token_type": "Bearer", "expires_in": 3600})
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestNativeCallback(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:1/callback", "http://127.0.0.1:65535/callback", "http://127.0.0.1:01234/callback"} {
		if !nativeCallback(raw) {
			t.Errorf("rejected native callback %q", raw)
		}
	}
	for _, raw := range []string{
		"http://127.0.0.1/callback", "http://127.0.0.1:/callback", "http://127.0.0.1:0/callback",
		"http://127.0.0.1:65536/callback", "http://127.0.0.1:-1/callback", "http://127.0.0.1:+123/callback",
		"http://127.0.0.1:abc/callback", "http://localhost:123/callback", "http://[::1]:123/callback",
		"http://127.0.0.2:123/callback", "https://127.0.0.1:123/callback", "http://example.com:123/callback",
		"http://127.0.0.1:123/other", "http://127.0.0.1:123/callback/", "http://127.0.0.1:123/%63allback",
		"http://127.0.0.1:123/callback?", "http://127.0.0.1:123/callback?x=1", "http://127.0.0.1:123/callback#",
		"http://127.0.0.1:123/callback#fragment", "http://user@127.0.0.1:123/callback",
	} {
		if nativeCallback(raw) {
			t.Errorf("accepted unsupported callback %q", raw)
		}
	}
}

func TestInteractiveIdentityPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name           string
		options        Options
		storedID       string
		storedRedirect string
		redirect       string
		wantID         string
	}{
		{name: "built-in", wantID: builtinClientIDMetadataURL},
		{name: "explicit ID", options: Options{ClientID: "explicit-client"}, wantID: "explicit-client"},
		{name: "explicit metadata overrides explicit ID", options: Options{ClientID: "explicit-client", ClientIDMetadataURL: "https://example.com/client.json"}, wantID: "https://example.com/client.json"},
		{name: "stored ID with changed port", storedID: "stored-client", storedRedirect: "http://127.0.0.1:1234/callback", wantID: "stored-client"},
		{name: "explicit beats stored", options: Options{ClientID: "explicit-client"}, storedID: "stored-client", storedRedirect: "http://127.0.0.1:1234/callback", wantID: "explicit-client"},
		{name: "explicit metadata beats stored", options: Options{ClientIDMetadataURL: "https://example.com/client.json"}, storedID: "stored-client", storedRedirect: "http://127.0.0.1:1234/callback", wantID: "https://example.com/client.json"},
		{name: "incompatible stored redirect", storedID: "stored-client", storedRedirect: "http://127.0.0.1:1234/other", wantID: builtinClientIDMetadataURL},
		{name: "stored built-in with changed port", storedID: builtinClientIDMetadataURL, storedRedirect: "http://127.0.0.1:1234/callback", wantID: builtinClientIDMetadataURL},
		{name: "explicit metadata supports other callbacks", options: Options{ClientIDMetadataURL: "https://example.com/client.json"}, redirect: "https://client.example/callback", wantID: "https://example.com/client.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newIdentityTestServer(t, nil, false)
			store := NewFileStore(t.TempDir() + "/oauth.json")
			if tc.storedID != "" {
				_, err := store.Update(s.URL+"/resource", func(*Session) (*Session, error) {
					return &Session{Endpoint: s.URL + "/resource", Issuer: s.URL, Token: &oauth2.Token{}, Config: OAuth2Config{ClientID: tc.storedID, RedirectURL: tc.storedRedirect}}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			coordinator := NewCoordinator(store)
			tc.options.HTTPClient = s.Client()
			if tc.redirect == "" {
				tc.redirect = "http://127.0.0.1:4321/callback"
			}
			flow, err := coordinator.Start(t.Context(), s.URL+"/resource", tc.options, tc.redirect, true)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { coordinator.Cancel(s.URL+"/resource", flow.ID) })
			u, err := url.Parse(flow.AuthorizationURL)
			if err != nil {
				t.Fatal(err)
			}
			if got := u.Query().Get("client_id"); got != tc.wantID {
				t.Errorf("client_id = %q, want %q", got, tc.wantID)
			}
			if s.registrations.Load() != 0 {
				t.Error("selected identity performed DCR")
			}
			if s.resourceReads.Load() != 1 || s.metadataReads.Load() != 1 {
				t.Errorf("discovery was not reused: PRM=%d AS=%d", s.resourceReads.Load(), s.metadataReads.Load())
			}
			if got := strings.Fields(u.Query().Get("scope")); !containsString(got, "read") || !containsString(got, "write") {
				t.Errorf("discovered scopes = %v", got)
			}
		})
	}
}

func TestBuiltinIdentityCallbackAndTokenErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		issuer     string
		tokenError bool
		wantState  FlowState
		wantTokens int32
	}{
		{name: "valid", issuer: "server", wantState: FlowSucceeded, wantTokens: 1},
		{name: "missing issuer", wantState: FlowFailed},
		{name: "wrong issuer", issuer: "https://wrong.example", wantState: FlowFailed},
		{name: "rejected token does not downgrade", issuer: "server", tokenError: true, wantState: FlowFailed, wantTokens: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newIdentityTestServer(t, nil, tc.tokenError)
			store := NewFileStore(t.TempDir() + "/oauth.json")
			coordinator := NewCoordinator(store)
			flow, err := coordinator.Start(t.Context(), s.URL+"/resource", Options{HTTPClient: s.Client()}, "http://127.0.0.1:4321/callback", true)
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(flow.AuthorizationURL)
			query := u.Query()
			if query.Get("state") == "" || query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
				t.Fatal("missing PKCE/state")
			}
			issuer := tc.issuer
			if issuer == "server" {
				issuer = s.URL
			}
			if _, ok := coordinator.CompleteCallback(query.Get("state"), "code", issuer, ""); !ok {
				t.Fatal("callback not accepted")
			}
			completed, err := coordinator.Wait(t.Context(), flow.ID)
			if err != nil || completed.State != tc.wantState {
				t.Fatalf("completed = %+v, err = %v", completed, err)
			}
			if s.tokens.Load() != tc.wantTokens || s.registrations.Load() != 0 {
				t.Errorf("token calls=%d, DCR=%d", s.tokens.Load(), s.registrations.Load())
			}
			if tc.wantState == FlowSucceeded {
				sum := sha256.Sum256([]byte(s.verifier))
				if s.verifier == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != query.Get("code_challenge") {
					t.Error("token exchange did not use the authorization PKCE verifier")
				}
				saved, err := store.Load(s.URL + "/resource")
				if err != nil || saved.Config.ClientID != builtinClientIDMetadataURL || saved.Issuer != s.URL {
					t.Fatalf("saved session = %+v, err = %v", saved, err)
				}
				if saved.Config.Endpoint.AuthStyle != oauth2.AuthStyleInParams {
					t.Fatal("public identity did not persist token auth method none")
				}
				// Each reconstructed config must refresh without a failed Basic
				// authentication probe, including after a process restart.
				for i := 0; i < 2; i++ {
					before := s.tokens.Load()
					if _, err := refreshGrant(t.Context(), saved.Config.oauth2Config(), saved.Token, saved.Endpoint); err != nil {
						t.Fatal(err)
					}
					if s.tokens.Load() != before+1 {
						t.Fatal("public-client refresh probed an unsupported token auth method")
					}
				}
			} else if strings.Contains(completed.Error, "untrusted") {
				t.Error("token description leaked")
			}
		})
	}
}

func TestBuiltinIdentityDocumentParity(t *testing.T) {
	data, err := os.ReadFile("../../../docs-site/static/oauth/client.json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"client_id": builtinClientIDMetadataURL, "client_name": "term-llm", "client_uri": "https://term-llm.com/",
		"application_type": "native", "redirect_uris": []any{"http://127.0.0.1/callback"},
		"grant_types": []any{"authorization_code", "refresh_token"}, "response_types": []any{"code"}, "token_endpoint_auth_method": "none",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hosted document differs from native public-client identity:\ngot:  %s\nwant: %+v", data, want)
	}
}
