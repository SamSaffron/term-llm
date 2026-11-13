package oauth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"
)

func TestStoredBuiltinIdentityRetainsIssuerPin(t *testing.T) {
	for _, tc := range []struct {
		name          string
		options       Options
		changed       bool
		trailingSlash bool
		noIssSupport  bool
		redirect      string
		wantID        string
	}{
		{name: "same issuer", wantID: builtinClientIDMetadataURL},
		{name: "same issuer with trailing slash", trailingSlash: true, wantID: builtinClientIDMetadataURL},
		{name: "changed issuer", changed: true},
		{name: "explicit ID still overrides stored identity", changed: true, options: Options{ClientID: "explicit-client"}, wantID: "explicit-client"},
		{name: "explicit metadata still overrides stored identity", changed: true, options: Options{ClientIDMetadataURL: "https://client.example/client.json"}, wantID: "https://client.example/client.json"},
		{name: "changed issuer cannot fall back to DCR", changed: true, noIssSupport: true},
		{name: "changed issuer with ineligible callback", changed: true, redirect: "http://127.0.0.1:4321/other"},
		{name: "same issuer may select DCR before identity chosen", noIssSupport: true, wantID: "dynamic-client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newIdentityTestServer(t, func(m map[string]any, _ int32) {
				if tc.noIssSupport {
					m["authorization_response_iss_parameter_supported"] = false
				}
			}, false)
			issuer := s.URL
			if tc.trailingSlash {
				issuer += "/"
			}
			if tc.changed {
				issuer = "https://previous-as.example"
			}
			store := NewFileStore(t.TempDir() + "/oauth.json")
			before, err := store.Update(s.URL+"/resource", func(*Session) (*Session, error) {
				return &Session{
					Endpoint: s.URL + "/resource", Issuer: issuer, Token: &oauth2.Token{},
					Config: OAuth2Config{ClientID: builtinClientIDMetadataURL, RedirectURL: "http://127.0.0.1:1234/callback"},
				}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			redirect := tc.redirect
			if redirect == "" {
				redirect = "http://127.0.0.1:4321/callback"
			}
			coordinator := NewCoordinator(store)
			tc.options.HTTPClient = s.Client()
			flow, err := coordinator.Start(t.Context(), s.URL+"/resource", tc.options, redirect, true)
			if tc.wantID == "" {
				if err == nil || !strings.Contains(err.Error(), "stored OAuth issuer") || flow == nil || flow.State != FlowFailed || flow.AuthorizationURL != "" {
					if flow != nil {
						coordinator.Cancel(s.URL+"/resource", flow.ID)
					}
					t.Fatalf("changed AS issuer was not rejected: flow=%+v err=%v", flow, err)
				}
				if s.registrations.Load() != 0 || s.tokens.Load() != 0 {
					t.Fatalf("issuer mismatch attempted DCR=%d or token calls=%d", s.registrations.Load(), s.tokens.Load())
				}
				after, err := store.Load(s.URL + "/resource")
				if err != nil || after.Version != before.Version || after.Issuer != before.Issuer {
					t.Fatalf("issuer mismatch changed stored grant: after=%+v err=%v", after, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer coordinator.Cancel(s.URL+"/resource", flow.ID)
			u, _ := url.Parse(flow.AuthorizationURL)
			if got := u.Query().Get("client_id"); got != tc.wantID {
				t.Errorf("client_id = %q, want %q", got, tc.wantID)
			}
		})
	}
}

func TestStoredBuiltinIssuerCannotBypassPinWithLegacyDiscovery(t *testing.T) {
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
	store := NewFileStore(t.TempDir() + "/oauth.json")
	_, err := store.Update(server.URL+"/resource", func(*Session) (*Session, error) {
		return &Session{Endpoint: server.URL + "/resource", Issuer: "https://previous-as.example", Token: &oauth2.Token{}, Config: OAuth2Config{ClientID: builtinClientIDMetadataURL, RedirectURL: "http://127.0.0.1:1234/callback"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(store)
	flow, err := coordinator.Start(t.Context(), server.URL+"/resource", Options{HTTPClient: server.Client()}, "http://127.0.0.1:4321/callback", true)
	if flow != nil {
		defer coordinator.Cancel(server.URL+"/resource", flow.ID)
	}
	if err == nil || !strings.Contains(err.Error(), "cannot verify stored OAuth issuer") || registrations.Load() != 0 {
		t.Fatalf("legacy discovery bypassed issuer pin: flow=%+v err=%v DCR=%d", flow, err, registrations.Load())
	}
}
