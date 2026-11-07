//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveFileWindowsPathForms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(path, []byte("  secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	forms := []string{"file://" + path, "file:///" + filepath.ToSlash(path)}
	for _, form := range forms {
		got, err := ResolveValue(form)
		if err != nil {
			t.Fatalf("ResolveValue(%q): %v", form, err)
		}
		if got != "secret-token" {
			t.Fatalf("ResolveValue(%q) = %q", form, got)
		}
	}
}

func TestResolveFileWindowsFragment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(path, []byte(`{"access_token":"tok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveValue("file://" + path + "#access_token")
	if err != nil {
		t.Fatalf("ResolveValue: %v", err)
	}
	if got != "tok" {
		t.Fatalf("fragment value = %q", got)
	}
}
