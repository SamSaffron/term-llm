package mcp

import (
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
