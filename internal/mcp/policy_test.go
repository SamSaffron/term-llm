package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func writeManagerPolicy(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, "term-llm")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestManagerPolicyAccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeManagerPolicy(t, home, "mcp: {servers: {restricted: {agents: [migration-data]}, denied: {agents: []}}}")
	for _, id := range []string{"", "migration-data", "other"} {
		t.Run(id, func(t *testing.T) {
			manager := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{"restricted": {}, "denied": {}, "open": {}}}, WithPolicyIdentity(id))
			defer manager.StopAll()
			for _, name := range []string{"restricted", "denied", "open"} {
				allowed := name == "open" || (name == "restricted" && id == "migration-data")
				err := manager.CheckAccess(name)
				if allowed {
					if err != nil {
						t.Fatal(err)
					}
					continue
				}
				var denied *PermissionError
				if !errors.As(err, &denied) {
					t.Fatalf("CheckAccess=%v, want PermissionError", err)
				}
				if err := manager.Enable(context.Background(), name); !errors.As(err, &denied) {
					t.Fatalf("Enable=%v", err)
				}
				if _, err := manager.CallCatalogTool(context.Background(), name, "x", "x", nil); !errors.As(err, &denied) {
					t.Fatalf("catalogue call=%v", err)
				}
			}
			want := []string{"open"}
			if id == "migration-data" {
				want = append(want, "restricted")
			}
			if got := manager.AvailableServers(); !reflect.DeepEqual(got, want) {
				t.Fatalf("available=%v want %v", got, want)
			}
			if len(manager.enabledServerNames()) != 0 {
				t.Fatal("denied servers started")
			}
		})
	}
}

func TestManagerPolicyReloadRevokesCatalogue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeManagerPolicy(t, home, "{}")
	manager := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{"greeter": {Command: os.Args[0], Env: map[string]string{runMCPManagerTestServerEnv: "1"}}}})
	defer manager.StopAll()
	if err := manager.Enable(context.Background(), "greeter"); err != nil {
		t.Fatal(err)
	}
	if status, err := waitForServerStatus(t, manager, "greeter", StatusReady, 3*time.Second); status != StatusReady || err != nil {
		t.Fatalf("status=%s error=%v", status, err)
	}
	if len(manager.CatalogueSnapshot().Tools) == 0 {
		t.Fatal("empty catalogue")
	}
	var removed bool
	manager.SetCatalogueChangeHandler(func(event CatalogueEvent) {
		if len(event.Snapshot.Tools) == 0 {
			removed = true
		}
	})
	writeManagerPolicy(t, home, "mcp: {servers: {greeter: {agents: [migration-data]}}}")
	var denied *PermissionError
	if _, err := manager.CallCatalogTool(context.Background(), "greeter", "greet", "greeter__greet", nil); !errors.As(err, &denied) {
		t.Fatalf("stale catalogue call=%v", err)
	}
	if len(manager.CatalogueSnapshot().Tools) != 0 || !removed {
		t.Fatal("revocation did not publish removal")
	}
	if status, _ := manager.ServerStatus("greeter"); status != StatusStopped {
		t.Fatalf("revoked status=%s", status)
	}
	writeManagerPolicy(t, home, "mcp: [invalid")
	if err := manager.CheckAccess("greeter"); !errors.As(err, &denied) {
		t.Fatalf("bad reload lost policy: %v", err)
	}
	writeManagerPolicy(t, home, "mcp: {servers: {greeter: {agent: [migration-data]}}}")
	if err := manager.CheckAccess("greeter"); !errors.As(err, &denied) {
		t.Fatalf("typo reload lost policy: %v", err)
	}
	writeManagerPolicy(t, home, "{}")
	if err := manager.CheckAccess("greeter"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerPolicyInitialFailureDeniesAll(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	writeManagerPolicy(t, home, "mcp: {servers: {greeter: {agent: []}}}")
	manager := NewManagerWithConfig(&Config{Servers: map[string]ServerConfig{"greeter": {}}})
	if err := manager.Enable(context.Background(), "greeter"); err == nil {
		t.Fatal("invalid initial policy granted access")
	}
	if len(manager.AvailableServers()) != 0 {
		t.Fatal("invalid policy listed servers")
	}
}

func TestMCPPolicyDoesNotChangeConnectionBytes(t *testing.T) {
	// This is the existing main-branch connection format, including field order,
	// indentation and no trailing newline. Policy must never enter this file.
	const original = `{
  "servers": {
    "Prod.DB": {
      "command": "server",
      "args": [
        "--readonly"
      ],
      "env": {
        "TOKEN": "example"
      },
      "always_load": [
        "query"
      ],
      "sampling": {
        "enabled": true
      }
    }
  }
}`
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveToPath(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("connection bytes changed:\n%s", got)
	}
}

func TestManagerPolicyRefreshBoundaries(t *testing.T) {
	for _, boundary := range []string{"load", "enable", "call", "refresh"} {
		t.Run(boundary, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", home)
			writeManagerPolicy(t, home, "{}")
			cfg := &Config{Servers: map[string]ServerConfig{"greeter": {Command: os.Args[0], Env: map[string]string{runMCPManagerTestServerEnv: "1"}}}}
			path, err := DefaultConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.SaveToPath(path); err != nil {
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
				t.Fatalf("%s: %v", status, err)
			}
			writeManagerPolicy(t, home, "mcp: {servers: {greeter: {agents: []}}}")
			switch boundary {
			case "load":
				if err := manager.LoadConfig(); err != nil {
					t.Fatal(err)
				}
			case "enable":
				var denied *PermissionError
				if err := manager.Enable(context.Background(), "greeter"); !errors.As(err, &denied) {
					t.Fatalf("Enable=%v", err)
				}
			case "call":
				_, _ = manager.CallCatalogTool(context.Background(), "greeter", "greet", "greeter__greet", nil)
			case "refresh":
				if err := manager.RefreshPolicy(); err != nil {
					t.Fatal(err)
				}
			}
			if status, _ := manager.ServerStatus("greeter"); status != StatusStopped {
				t.Fatalf("refresh did not stop server: %s", status)
			}
		})
	}
}
