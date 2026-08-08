package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileWorkspaceTrustStorePersistsExactWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "remembered-workspaces.yaml")
	store := &fileWorkspaceTrustStore{path: path}
	workspace := t.TempDir()
	other := t.TempDir()

	trusted, err := store.IsTrusted(workspace)
	if err != nil || trusted {
		t.Fatalf("initial trust = %v, %v", trusted, err)
	}
	if err := store.Remember(workspace); err != nil {
		t.Fatal(err)
	}
	trusted, err = (&fileWorkspaceTrustStore{path: path}).IsTrusted(workspace)
	if err != nil || !trusted {
		t.Fatalf("reloaded trust = %v, %v", trusted, err)
	}
	trusted, err = store.IsTrusted(other)
	if err != nil || trusted {
		t.Fatalf("unrelated workspace trust = %v, %v", trusted, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger permissions = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), workspace) != 1 {
		t.Fatalf("ledger did not contain one exact workspace record: %s", data)
	}
	if err := store.Remember(workspace); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), workspace) != 1 {
		t.Fatalf("duplicate remember changed ledger: %s", data)
	}
}

func TestFileWorkspaceTrustStoreRejectsInsecurePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remembered-workspaces.yaml")
	workspace := t.TempDir()
	data := []byte("version: 1\nworkspaces:\n  - path: " + workspace + "\n    approved_at: 2026-08-08T00:00:00Z\n")
	if err := os.WriteFile(path, data, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	store := &fileWorkspaceTrustStore{path: path}
	if trusted, err := store.IsTrusted(workspace); err == nil || trusted {
		t.Fatalf("insecure ledger trust = %v, %v", trusted, err)
	}
}

func TestFileWorkspaceTrustStoreRejectsMalformedLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remembered-workspaces.yaml")
	if err := os.WriteFile(path, []byte("version: [not-valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &fileWorkspaceTrustStore{path: path}
	if trusted, err := store.IsTrusted(t.TempDir()); err == nil || trusted {
		t.Fatalf("malformed ledger trust = %v, %v", trusted, err)
	}
}
