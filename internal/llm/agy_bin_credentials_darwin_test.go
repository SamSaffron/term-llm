//go:build darwin

package llm

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgyCopyCredentialsBridgesMacOSKeychainIntoPrivateHome(t *testing.T) {
	for _, keychainName := range []string{"login.keychain-db", "login.keychain"} {
		t.Run(keychainName, func(t *testing.T) {
			realHome := t.TempDir()
			privateHome := t.TempDir()
			keychain := filepath.Join(realHome, "Library", "Keychains", keychainName)
			if err := os.MkdirAll(filepath.Dir(keychain), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keychain, []byte("test keychain"), 0o600); err != nil {
				t.Fatal(err)
			}
			installFakeAgySecurity(t, realHome)

			p := NewAgyBinProvider("", nil)
			p.realHome = realHome
			p.agyHome = privateHome
			if err := p.copyCredentials(); err != nil {
				t.Fatalf("copyCredentials: %v", err)
			}
			// Preparing the same private home again must accept the existing,
			// correctly targeted link.
			if err := p.copyCredentials(); err != nil {
				t.Fatalf("second copyCredentials: %v", err)
			}

			link := filepath.Join(privateHome, "Library", "Keychains", keychainName)
			target, err := os.Readlink(link)
			if err != nil {
				t.Fatalf("read keychain link: %v", err)
			}
			if target != keychain {
				t.Fatalf("keychain link = %q, want %q", target, keychain)
			}
		})
	}
}

func TestPrepareAgyPlatformCredentialsRejectsWrongExistingLink(t *testing.T) {
	realHome := t.TempDir()
	privateHome := t.TempDir()
	keychain := filepath.Join(realHome, "Library", "Keychains", "login.keychain-db")
	if err := os.MkdirAll(filepath.Dir(keychain), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keychain, []byte("test keychain"), 0o600); err != nil {
		t.Fatal(err)
	}
	installFakeAgySecurity(t, realHome)

	linkDir := filepath.Join(privateHome, "Library", "Keychains")
	if err := os.MkdirAll(linkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "login.keychain-db")
	if err := os.Symlink(filepath.Join(realHome, "wrong.keychain-db"), link); err != nil {
		t.Fatal(err)
	}

	_, err := prepareAgyPlatformCredentials(realHome, privateHome)
	if err == nil || !strings.Contains(err.Error(), "already points to") {
		t.Fatalf("prepareAgyPlatformCredentials error = %v, want wrong-link error", err)
	}
}

func TestAgyCopyCredentialsWithoutKeychainReturnsUserFacingError(t *testing.T) {
	p := NewAgyBinProvider("", nil)
	p.realHome = t.TempDir()
	p.agyHome = t.TempDir()
	t.Setenv("PATH", t.TempDir())

	err := p.copyCredentials()
	var userErr *UserFacingProviderError
	if !errors.As(err, &userErr) {
		t.Fatalf("copyCredentials error = %T %v, want UserFacingProviderError", err, err)
	}
	if userErr.Summary != "agy is not logged in" {
		t.Fatalf("summary = %q", userErr.Summary)
	}
}

func TestAgyBinHasCredentialsRecognizesMacOSKeychain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	installFakeAgySecurity(t, home)

	if !AgyBinHasCredentials() {
		t.Fatal("AgyBinHasCredentials returned false for an available agy binary and keychain login")
	}
}

func installFakeAgySecurity(t *testing.T, expectedHome string) {
	t.Helper()
	binDir := t.TempDir()
	for _, name := range []string{"agy", "security"} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, nil, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	previousCheck := agyPlatformHasCredentials
	agyPlatformHasCredentials = func(home string) bool { return home == expectedHome }
	t.Cleanup(func() { agyPlatformHasCredentials = previousCheck })
	t.Setenv("PATH", binDir)
}

func TestAgySecurityCredentialCommandUsesRequestedHome(t *testing.T) {
	binDir := t.TempDir()
	security := filepath.Join(binDir, "security")
	if err := os.WriteFile(security, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("HOME", "/unexpected/home")

	cmd, err := agySecurityCredentialCommand("/real/home")
	if err != nil {
		t.Fatalf("agySecurityCredentialCommand: %v", err)
	}
	if got := envValue(cmd.Env, "HOME"); got != "/real/home" {
		t.Fatalf("security HOME = %q, want /real/home", got)
	}
	wantArgs := []string{"find-generic-password", "-s", agyKeychainService, "-a", agyKeychainAccount}
	if got := cmd.Args[1:]; strings.Join(got, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("security args = %q, want %q", got, wantArgs)
	}
}
