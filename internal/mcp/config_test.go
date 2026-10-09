package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigServerNamesSortedAlphabetically(t *testing.T) {
	cfg := &Config{Servers: map[string]ServerConfig{
		"zeta":  {Command: "zeta"},
		"alpha": {Command: "alpha"},
		"Beta":  {Command: "beta"},
		"gamma": {Command: "gamma"},
	}}

	got := cfg.ServerNames()
	want := []string{"Beta", "alpha", "gamma", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ServerNames() = %v, want %v", got, want)
	}
}

func TestManagerAvailableServersSortedAlphabetically(t *testing.T) {
	mgr := NewManager()
	mgr.config = &Config{Servers: map[string]ServerConfig{
		"zeta":  {Command: "zeta"},
		"alpha": {Command: "alpha"},
		"Beta":  {Command: "beta"},
		"gamma": {Command: "gamma"},
	}}

	got := mgr.AvailableServers()
	want := []string{"Beta", "alpha", "gamma", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableServers() = %v, want %v", got, want)
	}
}

func TestLoadConfigAlwaysLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"servers":{"github":{"command":"demo","always_load":["search_issues","get_pull_request"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Servers["github"].AlwaysLoad
	if !reflect.DeepEqual(got, []string{"search_issues", "get_pull_request"}) {
		t.Fatalf("always_load = %#v", got)
	}
}

func TestConfigOAuthRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	want := &OAuthConfig{
		ClientID: "public-client", ClientSecretEnv: "MCP_CLIENT_SECRET",
		Scopes: []string{"read", "write"}, ClientIDMetadataURL: "https://client.example/metadata.json",
	}
	cfg := &Config{Servers: map[string]ServerConfig{
		"remote": {Type: "http", URL: "https://mcp.example/mcp", OAuth: want},
	}}
	if err := cfg.SaveToPath(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfigFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Servers["remote"].OAuth; !reflect.DeepEqual(got, want) {
		t.Fatalf("OAuth after round trip = %#v, want %#v", got, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if stringsContainsTest(string(data), "client_secret\"") || stringsContainsTest(string(data), "access_token") {
		t.Fatalf("mcp.json contains credential material: %s", data)
	}
}

func stringsContainsTest(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

func TestOAuthScopesRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes []string
	}{
		{name: "nil"},
		{name: "empty", scopes: []string{}},
		{name: "populated", scopes: []string{"read", "write"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			cfg := &Config{Servers: map[string]ServerConfig{"remote": {URL: "https://example.test/mcp", OAuth: &OAuthConfig{Scopes: tc.scopes}}}}
			if err := cfg.SaveToPath(path); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				Servers map[string]struct {
					OAuth map[string]json.RawMessage `json:"oauth"`
				} `json:"servers"`
			}
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if _, present := saved.Servers["remote"].OAuth["scopes"]; present != (tc.scopes != nil) {
				t.Fatalf("saved scopes present = %v, want %v", present, tc.scopes != nil)
			}
			loaded, err := LoadConfigFromPath(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded.Servers["remote"].OAuth.Scopes; !reflect.DeepEqual(got, tc.scopes) {
				t.Fatalf("round-trip scopes = %#v, want %#v", got, tc.scopes)
			}
			options, err := oauthOptionsForServer(loaded.Servers["remote"])
			if err != nil {
				t.Fatal(err)
			}
			if options.ScopesConfigured != (tc.scopes != nil) || !reflect.DeepEqual(options.Scopes, tc.scopes) {
				t.Fatalf("OAuth options = %#v, want scopes %#v", options, tc.scopes)
			}
		})
	}
}

func TestUpdateConfigPreservesEmptyOAuthScopes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"servers":{"remote":{"url":"https://example.test/mcp","oauth":{"scopes":[]}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateConfigAtPath(path, func(cfg *Config) error {
		cfg.Servers["other"] = ServerConfig{Command: "example"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfigFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Servers["remote"].OAuth.Scopes; got == nil || len(got) != 0 {
		t.Fatalf("scopes after unrelated update = %#v, want []", got)
	}
}
