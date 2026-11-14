package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	mcpoauth "github.com/term-llm/term-llm/internal/mcp/oauth"
)

func TestManagerPolicyGettersUseCacheWithoutRevocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeManagerPolicy(t, home, "{}")
	manager := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{"greeter": {Command: os.Args[0], Env: map[string]string{runMCPManagerTestServerEnv: "1"}}}})
	defer manager.StopAll()
	if err := manager.Enable(context.Background(), "greeter"); err != nil {
		t.Fatal(err)
	}
	if status, err := waitForServerStatus(t, manager, "greeter", StatusReady, 3*time.Second); status != StatusReady || err != nil {
		t.Fatalf("ready=%s %v", status, err)
	}
	snapshot := manager.CatalogueSnapshot()
	tools := manager.AllTools()
	writeManagerPolicy(t, home, "mcp: {servers: {greeter: {agents: []}}}")
	for i := 0; i < 3; i++ {
		if !reflect.DeepEqual(manager.AvailableServers(), []string{"greeter"}) || !reflect.DeepEqual(manager.EnabledServers(), []string{"greeter"}) {
			t.Fatal("hot listing refreshed policy")
		}
		if manager.CatalogueSnapshot() != snapshot || !reflect.DeepEqual(manager.AllTools(), tools) {
			t.Fatal("hot catalogue refreshed policy")
		}
		if status, _ := manager.ServerStatus("greeter"); status != StatusReady {
			t.Fatal("hot getter stopped server")
		}
	}
	if err := manager.RefreshPolicy(); err != nil {
		t.Fatal(err)
	}
	if len(manager.AvailableServers()) != 0 || len(manager.EnabledServers()) != 0 || len(manager.CatalogueSnapshot().Tools) != 0 || len(manager.AllTools()) != 0 {
		t.Fatal("explicit refresh did not revoke")
	}
}

func TestManagerPolicyRefreshCachesFileMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeManagerPolicy(t, home, "mcp: {servers: {db: {agents: [a]}}}")
	manager := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{"db": {}}}, WithPolicyIdentity("a"))
	path := filepath.Join(home, "term-llm", "config.yaml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// A same-size edit with its mtime restored is intentionally invisible to
	// the cheap metadata cache until metadata changes (there is no watcher).
	writeManagerPolicy(t, home, "mcp: {servers: {db: {agents: [b]}}}")
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := manager.CheckAccess("db"); err != nil {
		t.Fatalf("unchanged metadata reparsed: %v", err)
	}
	changed := info.ModTime().Add(time.Second)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	var denied *PermissionError
	if err := manager.CheckAccess("db"); !errors.As(err, &denied) {
		t.Fatalf("mtime change ignored: %v", err)
	}
	// A config path change is significant even with identical size and mtime.
	other := filepath.Join(home, "term-llm", "config.yml")
	if err := os.Rename(path, other); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("mcp: {servers: {db: {agents: [a]}}}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, changed, changed); err != nil {
		t.Fatal(err)
	}
	if err := manager.CheckAccess("db"); err != nil {
		t.Fatalf("path change ignored: %v", err)
	}
}

func TestManagerPolicyCorruptReloadDoesNotReenableRevokedWrappers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeManagerPolicy(t, home, "{}")
	connections := &Config{Servers: map[string]ServerConfig{"greeter": {Command: os.Args[0], Env: map[string]string{runMCPManagerTestServerEnv: "1"}}}}
	path, err := DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := connections.SaveToPath(path); err != nil {
		t.Fatal(err)
	}
	manager := NewManager()
	defer manager.StopAll()
	if err := manager.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Enable(context.Background(), "greeter"); err != nil {
		t.Fatal(err)
	}
	if status, err := waitForServerStatus(t, manager, "greeter", StatusReady, 3*time.Second); status != StatusReady || err != nil {
		t.Fatalf("ready=%s %v", status, err)
	}
	catalogue := manager.CatalogueSnapshot().Tools
	if len(catalogue) == 0 {
		t.Fatal("no tool to retain")
	}
	wrappers := []*MCPTool{NewCatalogMCPTool(manager, catalogue[0]), NewMCPTool(manager, manager.AllTools()[0])}
	writeManagerPolicy(t, home, "mcp: {servers: {greeter: {agents: []}}}")
	if err := manager.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	writeManagerPolicy(t, home, "mcp: [corrupt")
	for i := 0; i < 2; i++ {
		if err := manager.LoadConfig(); err != nil {
			t.Fatalf("last-good reload: %v", err)
		}
		var denied *PermissionError
		if err := manager.Enable(context.Background(), "greeter"); !errors.As(err, &denied) {
			t.Fatalf("corrupt reload enabled revoked server: %v", err)
		}
		for _, wrapper := range wrappers {
			if _, err := wrapper.Execute(context.Background(), json.RawMessage(`{}`)); !errors.As(err, &denied) {
				t.Fatalf("stale wrapper bypassed policy: %v", err)
			}
		}
		if status, _ := manager.ServerStatus("greeter"); status != StatusStopped {
			t.Fatalf("revoked server restarted: %s", status)
		}
		if len(manager.CatalogueSnapshot().Tools) != 0 || len(manager.EnabledServers()) != 0 {
			t.Fatal("corrupt reload resurrected catalogue")
		}
	}
}

func TestManagerPolicyOAuthLoginDoesNotRequireAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeManagerPolicy(t, home, "mcp: {servers: {protected: {agents: []}}}")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/resource":
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/resource"`, server.URL))
			w.WriteHeader(http.StatusUnauthorized)
		case "/.well-known/oauth-protected-resource/resource":
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": server.URL + "/resource", "authorization_servers": []string{server.URL}})
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manager := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{"protected": {URL: server.URL + "/resource", OAuth: &OAuthConfig{ClientID: "public-client"}}}})
	manager.oauthCoordinator = mcpoauth.NewCoordinator(mcpoauth.NewFileStore(filepath.Join(t.TempDir(), "oauth.json")))
	if manager.CheckAccess("protected") == nil {
		t.Fatal("fixture must deny tool access")
	}
	flow, err := manager.StartOAuth(t.Context(), "protected", OAuthStartOptions{RedirectURL: "http://127.0.0.1/callback", SkipReconnect: true})
	if err != nil {
		t.Fatalf("restricted login failed: %v", err)
	}
	if flow == nil || !strings.HasPrefix(flow.AuthorizationURL, server.URL+"/authorize") {
		t.Fatalf("flow=%+v", flow)
	}
	defer manager.oauthCoordinator.Cancel(server.URL+"/resource", flow.ID)
	if len(manager.EnabledServers()) != 0 {
		t.Fatal("login started restricted server")
	}
}

func TestManagerPolicyRetainsRestrictionsDuringEditorSaveWindow(t *testing.T) {
	for _, window := range []string{"missing", "empty", "whitespace"} {
		t.Run(window, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			writeManagerPolicy(t, home, "mcp: {servers: {db: {agents: []}}}")
			manager := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{"db": {}}})
			path := filepath.Join(home, "term-llm", "config.yaml")
			if window == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				body := ""
				if window == "whitespace" {
					body = " \n\t\n"
				}
				writeManagerPolicy(t, home, body)
			}
			for i := 0; i < 2; i++ {
				var denied *PermissionError
				if err := manager.CheckAccess("db"); !errors.As(err, &denied) {
					t.Fatalf("editor window opened policy: %v", err)
				}
				if len(manager.AvailableServers()) != 0 {
					t.Fatal("editor window listed restricted server")
				}
			}
			writeManagerPolicy(t, home, "default_provider: debug\n")
			if err := manager.CheckAccess("db"); err != nil {
				t.Fatalf("valid unrestricted file did not loosen: %v", err)
			}
		})
	}
}

func TestManagerPolicyNonMappingEditorDocumentsRetainRestrictions(t *testing.T) {
	for _, body := range []string{"# comment only\n", "null\n", "---\n", "---\n# comment\n"} {
		t.Run(body, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			writeManagerPolicy(t, home, "mcp: {servers: {db: {agents: []}}}")
			manager := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{"db": {}}})
			writeManagerPolicy(t, home, body)
			var denied *PermissionError
			if err := manager.CheckAccess("db"); !errors.As(err, &denied) {
				t.Fatalf("non-mapping document removed policy: %v", err)
			}
			writeManagerPolicy(t, home, "{}")
			if err := manager.CheckAccess("db"); err != nil {
				t.Fatalf("explicit empty mapping did not loosen policy: %v", err)
			}
		})
	}
}

func TestManagerPolicyFailedLoadIsRetriedWithUnchangedStamp(t *testing.T) {
	for _, priorGood := range []bool{false, true} {
		t.Run(fmt.Sprint(priorGood), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			const allowed = "mcp: {servers: {db: {agents: [a]}}}"
			const revoked = "mcp: {servers: {db: {agents: [b]}}}"
			initial := strings.Repeat("[", len(allowed))
			if priorGood {
				initial = allowed
			}
			writeManagerPolicy(t, home, initial)
			manager := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{"db": {}}}, WithPolicyIdentity("a"))
			path := filepath.Join(home, "term-llm", "config.yaml")
			if priorGood {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				writeManagerPolicy(t, home, strings.Repeat("[", len(allowed)))
				changed := info.ModTime().Add(time.Second)
				if err := os.Chtimes(path, changed, changed); err != nil {
					t.Fatal(err)
				}
				if err := manager.CheckAccess("db"); err != nil {
					t.Fatalf("failed reload should retain old allow policy: %v", err)
				}
			}
			failed, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			writeManagerPolicy(t, home, revoked)
			if err := os.Chtimes(path, failed.ModTime(), failed.ModTime()); err != nil {
				t.Fatal(err)
			}
			var denied *PermissionError
			if err := manager.CheckAccess("db"); !errors.As(err, &denied) {
				t.Fatalf("failed stamp prevented retry: %v", err)
			}
		})
	}
}
