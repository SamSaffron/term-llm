package oauth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

func TestBuiltinIdentityRequiresPublicMetadataAndIssuerSupport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*oauthex.AuthServerMeta)
	}{
		{"no metadata", nil},
		{"no CIMD", func(m *oauthex.AuthServerMeta) { m.ClientIDMetadataDocumentSupported = false }},
		{"no auth methods", func(m *oauthex.AuthServerMeta) { m.TokenEndpointAuthMethodsSupported = nil }},
		{"confidential only", func(m *oauthex.AuthServerMeta) { m.TokenEndpointAuthMethodsSupported = []string{"client_secret_basic"} }},
		{"case sensitive auth method", func(m *oauthex.AuthServerMeta) { m.TokenEndpointAuthMethodsSupported = []string{"NONE"} }},
		{"no issuer", func(m *oauthex.AuthServerMeta) { m.Issuer = "" }},
		{"no issuer response support", func(m *oauthex.AuthServerMeta) { m.AuthorizationResponseIssParameterSupported = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &oauthex.AuthServerMeta{Issuer: "https://as.example", ClientIDMetadataDocumentSupported: true, AuthorizationResponseIssParameterSupported: true, TokenEndpointAuthMethodsSupported: []string{"none"}}
			if !supportsBuiltinIdentity("http://127.0.0.1:1234/callback", m) {
				t.Fatal("valid public-client metadata rejected")
			}
			if tc.change == nil {
				m = nil
			} else {
				tc.change(m)
			}
			if supportsBuiltinIdentity("http://127.0.0.1:1234/callback", m) {
				t.Fatal("unsupported metadata accepted")
			}
		})
	}
}

func TestInteractiveIdentityFallbackAndUnsupportedMethods(t *testing.T) {
	for _, tc := range []struct {
		name          string
		key           string
		value         any
		redirect      string
		noDCR         bool
		storedBuiltin bool
		wantID        string
	}{
		{name: "CIMD only", noDCR: true, wantID: builtinClientIDMetadataURL},
		{name: "no CIMD falls back to DCR", key: "client_id_metadata_document_supported", value: false, wantID: "dynamic-client"},
		{name: "no iss falls back to DCR", key: "authorization_response_iss_parameter_supported", value: false, wantID: "dynamic-client"},
		{name: "no public token auth falls back to DCR", key: "token_endpoint_auth_methods_supported", value: []string{"client_secret_basic"}, wantID: "dynamic-client"},
		{name: "unsupported callback retains DCR", redirect: "http://localhost:4321/callback", wantID: "dynamic-client"},
		{name: "unsupported callback without DCR", redirect: "http://localhost:4321/callback", noDCR: true},
		{name: "no CIMD or DCR", key: "client_id_metadata_document_supported", value: false, noDCR: true},
		{name: "no iss or DCR", key: "authorization_response_iss_parameter_supported", value: false, noDCR: true},
		{name: "no auth methods or DCR", key: "token_endpoint_auth_methods_supported", value: nil, noDCR: true},
		{name: "stored built-in loses issuer support", key: "authorization_response_iss_parameter_supported", value: false, noDCR: true, storedBuiltin: true},
		{name: "stored built-in loses public auth", key: "token_endpoint_auth_methods_supported", value: []string{"client_secret_basic"}, noDCR: true, storedBuiltin: true},
		{name: "stored built-in changes to unsupported callback", redirect: "http://127.0.0.1:4321/other", noDCR: true, storedBuiltin: true},
		{name: "stored built-in falls back to DCR", key: "authorization_response_iss_parameter_supported", value: false, storedBuiltin: true, wantID: "dynamic-client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newIdentityTestServer(t, func(m map[string]any, _ int32) {
				if tc.key != "" {
					m[tc.key] = tc.value
				}
				if tc.noDCR {
					delete(m, "registration_endpoint")
				}
			}, false)
			store := NewFileStore(t.TempDir() + "/oauth.json")
			if tc.storedBuiltin {
				_, err := store.Update(s.URL+"/resource", func(*Session) (*Session, error) {
					return &Session{Endpoint: s.URL + "/resource", Issuer: s.URL, Token: &oauth2.Token{}, Config: OAuth2Config{ClientID: builtinClientIDMetadataURL, RedirectURL: "http://127.0.0.1:1234/callback"}}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.redirect == "" {
				tc.redirect = "http://127.0.0.1:4321/callback"
			}
			coordinator := NewCoordinator(store)
			flow, err := coordinator.Start(t.Context(), s.URL+"/resource", Options{HTTPClient: s.Client()}, tc.redirect, true)
			if tc.wantID == "" {
				if err == nil || !strings.Contains(err.Error(), "configure a client ID or client metadata URL") {
					t.Fatalf("expected actionable unsupported identity error, got flow=%+v err=%v", flow, err)
				}
				if s.registrations.Load() != 0 || s.tokens.Load() != 0 {
					t.Fatal("unsupported identity attempted DCR or token exchange")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { coordinator.Cancel(s.URL+"/resource", flow.ID) })
			u, _ := url.Parse(flow.AuthorizationURL)
			if got := u.Query().Get("client_id"); got != tc.wantID {
				t.Errorf("client_id = %q, want %q", got, tc.wantID)
			}
			wantDCR := int32(0)
			if tc.wantID == "dynamic-client" {
				wantDCR = 1
			}
			if got := s.registrations.Load(); got != wantDCR {
				t.Errorf("registrations = %d, want %d", got, wantDCR)
			}
		})
	}
}

func TestInteractiveDiscoverySnapshotKeepsIssuerRequirement(t *testing.T) {
	s := newIdentityTestServer(t, func(m map[string]any, n int32) {
		if n > 1 {
			m["authorization_response_iss_parameter_supported"] = false
		}
	}, false)
	coordinator := NewCoordinator(NewFileStore(t.TempDir() + "/oauth.json"))
	flow, err := coordinator.Start(t.Context(), s.URL+"/resource", Options{HTTPClient: s.Client(), Scopes: []string{"configured"}}, "http://127.0.0.1:4321/callback", true)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(flow.AuthorizationURL)
	coordinator.CompleteCallback(u.Query().Get("state"), "code", "", "")
	completed, err := coordinator.Wait(t.Context(), flow.ID)
	if err != nil || completed.State != FlowFailed || s.tokens.Load() != 0 || s.metadataReads.Load() != 1 {
		t.Fatalf("changed metadata bypassed issuer validation: flow=%+v err=%v tokens=%d AS reads=%d", completed, err, s.tokens.Load(), s.metadataReads.Load())
	}
}

func TestInteractiveDiscoveryUsesSDKFirstAuthorizationServer(t *testing.T) {
	var secondReads atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondReads.Add(1)
		t.Error("must not select a different authorization server for its CIMD support")
		http.NotFound(w, r)
	}))
	defer second.Close()
	first := newIdentityTestServer(t, func(m map[string]any, _ int32) {
		m["client_id_metadata_document_supported"] = false
		delete(m, "registration_endpoint")
	}, false)
	var resource *httptest.Server
	resource = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prm" {
			writeTestJSON(w, map[string]any{"resource": resource.URL + "/resource", "authorization_servers": []string{first.URL, second.URL}})
			return
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic resource_metadata=%q, Bearer`, resource.URL+"/prm"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer resource.Close()
	coordinator := NewCoordinator(NewFileStore(t.TempDir() + "/oauth.json"))
	_, err := coordinator.Start(t.Context(), resource.URL+"/resource", Options{HTTPClient: resource.Client()}, "http://127.0.0.1:4321/callback", true)
	if err == nil || !strings.Contains(err.Error(), "configure a client ID") || secondReads.Load() != 0 {
		t.Fatalf("first-AS selection failed: err=%v second reads=%d", err, secondReads.Load())
	}
}

func TestInteractiveLegacyDCRFallback(t *testing.T) {
	var registrations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/register" {
			registrations.Add(1)
			writeTestJSON(w, map[string]any{"client_id": "legacy-client", "token_endpoint_auth_method": "none"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	coordinator := NewCoordinator(NewFileStore(t.TempDir() + "/oauth.json"))
	flow, err := coordinator.Start(t.Context(), server.URL+"/resource", Options{HTTPClient: server.Client()}, "http://127.0.0.1:4321/callback", true)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Cancel(server.URL+"/resource", flow.ID)
	u, _ := url.Parse(flow.AuthorizationURL)
	if u.Query().Get("client_id") != "legacy-client" || registrations.Load() != 1 {
		t.Fatalf("legacy fallback changed: URL=%s registrations=%d", flow.AuthorizationURL, registrations.Load())
	}
}

func TestInteractiveDiscoveryPinsResourceMetadata(t *testing.T) {
	first := newIdentityTestServer(t, nil, false)
	var secondReads atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondReads.Add(1)
		http.NotFound(w, r)
	}))
	defer second.Close()
	var reads atomic.Int32
	var resource *httptest.Server
	resource = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prm" {
			issuer := first.URL
			if reads.Add(1) > 1 {
				issuer = second.URL
			}
			writeTestJSON(w, map[string]any{"resource": resource.URL + "/resource", "authorization_servers": []string{issuer}})
			return
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, resource.URL+"/prm"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer resource.Close()
	coordinator := NewCoordinator(NewFileStore(t.TempDir() + "/oauth.json"))
	flow, err := coordinator.Start(t.Context(), resource.URL+"/resource", Options{HTTPClient: resource.Client()}, "http://127.0.0.1:4321/callback", true)
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Cancel(resource.URL+"/resource", flow.ID)
	u, _ := url.Parse(flow.AuthorizationURL)
	if !strings.HasPrefix(flow.AuthorizationURL, first.URL+"/authorize?") || u.Query().Get("client_id") != builtinClientIDMetadataURL || reads.Load() != 1 || secondReads.Load() != 0 {
		t.Fatalf("SDK used a different PRM snapshot: URL=%s PRM reads=%d second AS reads=%d", flow.AuthorizationURL, reads.Load(), secondReads.Load())
	}
}

func TestInteractiveInvalidMetadataDoesNotAttemptRegistration(t *testing.T) {
	for _, key := range []string{"issuer", "code_challenge_methods_supported"} {
		t.Run(key, func(t *testing.T) {
			s := newIdentityTestServer(t, func(m map[string]any, _ int32) { delete(m, key) }, false)
			coordinator := NewCoordinator(NewFileStore(t.TempDir() + "/oauth.json"))
			_, err := coordinator.Start(t.Context(), s.URL+"/resource", Options{HTTPClient: s.Client()}, "http://127.0.0.1:4321/callback", true)
			if err == nil || s.registrations.Load() != 0 || s.tokens.Load() != 0 {
				t.Fatalf("invalid metadata caused fallback: err=%v DCR=%d token calls=%d", err, s.registrations.Load(), s.tokens.Load())
			}
		})
	}
}
