package clibin

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAgyCopyCredentialsPreservesLegacyTokenSupport(t *testing.T) {
	realHome := t.TempDir()
	privateHome := t.TempDir()
	relative := filepath.Join(".gemini", "antigravity-cli", "antigravity-oauth-token")
	source, destination := filepath.Join(realHome, relative), filepath.Join(privateHome, relative)
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureAgyHomeLayout(privateHome); err != nil {
		t.Fatal(err)
	}
	p := NewAgyBinProvider("", nil)
	p.realHome, p.agyHome = realHome, privateHome
	for _, credential := range []string{"legacy-login", "refreshed-legacy-login"} {
		if err := os.WriteFile(source, []byte(credential), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := p.copyCredentials(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != credential {
			t.Fatalf("private token = %q, want %q", got, credential)
		}
		info, err := os.Stat(destination)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("private token permissions = %v, want 0600", info.Mode().Perm())
		}
	}
}

func TestAgyCopyCredentialsReportsLegacyCopyFailure(t *testing.T) {
	p := NewAgyBinProvider("", nil)
	p.realHome, p.agyHome = t.TempDir(), t.TempDir()
	source := filepath.Join(p.realHome, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("legacy-login"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A present legacy credential must be copied successfully, not silently
	// bypassed as if it were an unavailable native credential.
	if err := p.copyCredentials(); err == nil {
		t.Fatal("copyCredentials succeeded without the private token directory")
	}
}
