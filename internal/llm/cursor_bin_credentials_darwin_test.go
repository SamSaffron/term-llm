//go:build darwin

package llm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func clearCursorExplicitCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("CURSOR_API_KEY", "")
	t.Setenv("AGENT_CLI_CREDENTIAL_STORE", "")
}

func writeTestLoginKeychain(t *testing.T, home, name string) string {
	t.Helper()
	keychain := filepath.Join(home, "Library", "Keychains", name)
	if err := os.MkdirAll(filepath.Dir(keychain), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keychain, []byte("test keychain"), 0o600); err != nil {
		t.Fatal(err)
	}
	return keychain
}

func TestCursorHomeBridgesMacOSKeychainIntoPrivateHome(t *testing.T) {
	clearCursorExplicitCredentials(t)
	for _, keychainName := range []string{"login.keychain-db", "login.keychain"} {
		t.Run(keychainName, func(t *testing.T) {
			realHome := t.TempDir()
			keychain := writeTestLoginKeychain(t, realHome, keychainName)
			t.Setenv("HOME", realHome)

			p := NewCursorBinProvider("auto-smart", nil)
			home, cleanup, err := p.homeForRequest(true)
			if err != nil {
				t.Fatalf("homeForRequest: %v", err)
			}

			privateHome := filepath.Join(home, cursorUserHomeDir)
			link := filepath.Join(privateHome, "Library", "Keychains", keychainName)
			target, err := os.Readlink(link)
			if err != nil {
				t.Fatalf("read keychain link: %v", err)
			}
			if target != keychain {
				t.Fatalf("keychain link = %q, want %q", target, keychain)
			}
			if got := envValue(p.buildCommandEnv(home), "HOME"); got != privateHome {
				t.Fatalf("HOME = %q, want private Cursor home %q", got, privateHome)
			}

			cleanup()
			if data, err := os.ReadFile(keychain); err != nil || string(data) != "test keychain" {
				t.Fatalf("real keychain after private-home cleanup: data=%q err=%v", data, err)
			}
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatalf("ephemeral Cursor home still exists after cleanup: %v", err)
			}
		})
	}
}

func TestCursorDurableHomeRetainsKeychainBridge(t *testing.T) {
	clearCursorExplicitCredentials(t)
	realHome := t.TempDir()
	keychain := writeTestLoginKeychain(t, realHome, "login.keychain-db")
	t.Setenv("HOME", realHome)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	p := NewCursorBinProvider("auto-smart", nil)
	for attempt := 0; attempt < 2; attempt++ {
		home, cleanup, err := p.homeForRequest(false)
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
		link := filepath.Join(home, cursorUserHomeDir, "Library", "Keychains", "login.keychain-db")
		if target, err := os.Readlink(link); err != nil || target != keychain {
			t.Fatalf("durable keychain link = %q, err=%v, want %q", target, err, keychain)
		}
	}
}

func TestPrepareCursorPlatformCredentialsIsIdempotentAndRepairsStaleLink(t *testing.T) {
	realHome := t.TempDir()
	keychain := writeTestLoginKeychain(t, realHome, "login.keychain-db")
	privateHome := t.TempDir()

	if err := prepareCursorPlatformCredentials(realHome, privateHome); err != nil {
		t.Fatal(err)
	}
	if err := prepareCursorPlatformCredentials(realHome, privateHome); err != nil {
		t.Fatalf("second preparation: %v", err)
	}

	link := filepath.Join(privateHome, "Library", "Keychains", "login.keychain-db")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(realHome, "old-login.keychain-db"), link); err != nil {
		t.Fatal(err)
	}
	if err := prepareCursorPlatformCredentials(realHome, privateHome); err != nil {
		t.Fatalf("repair stale link: %v", err)
	}
	if target, err := os.Readlink(link); err != nil || target != keychain {
		t.Fatalf("repaired keychain link = %q, err=%v, want %q", target, err, keychain)
	}
}

func TestPrepareCursorPlatformCredentialsAllowsMissingKeychain(t *testing.T) {
	privateHome := t.TempDir()
	if err := prepareCursorPlatformCredentials(t.TempDir(), privateHome); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(privateHome, "Library", "Keychains")); !os.IsNotExist(err) {
		t.Fatalf("keychain directory created without a login keychain: %v", err)
	}

	_, err := macOSLoginKeychainPath(t.TempDir())
	if !errors.Is(err, errMacOSLoginKeychainNotFound) {
		t.Fatalf("macOSLoginKeychainPath error = %v, want not-found sentinel", err)
	}

	malformedHome := t.TempDir()
	malformed := filepath.Join(malformedHome, "Library", "Keychains", "login.keychain-db")
	if err := os.MkdirAll(malformed, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = macOSLoginKeychainPath(malformedHome)
	if err == nil || errors.Is(err, errMacOSLoginKeychainNotFound) {
		t.Fatalf("malformed keychain error = %v, want operational error", err)
	}
}

func TestCursorHomeBridgesKeychainWhenAPIKeyIsSet(t *testing.T) {
	realHome := t.TempDir()
	keychain := writeTestLoginKeychain(t, realHome, "login.keychain-db")
	t.Setenv("HOME", realHome)

	p := NewCursorBinProvider("auto-smart", map[string]string{"CURSOR_API_KEY": "test-key"})
	home, cleanup, err := p.homeForRequest(true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	link := filepath.Join(home, cursorUserHomeDir, "Library", "Keychains", "login.keychain-db")
	if target, err := os.Readlink(link); err != nil || target != keychain {
		t.Fatalf("API-key keychain link = %q, err=%v, want %q", target, err, keychain)
	}
}

func TestCursorHomeBridgesFileCredentialStore(t *testing.T) {
	realHome := t.TempDir()
	authFile := filepath.Join(realHome, ".cursor", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authFile, []byte(`{"accessToken":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", realHome)

	p := NewCursorBinProvider("auto-smart", map[string]string{"AGENT_CLI_CREDENTIAL_STORE": "file"})
	home, cleanup, err := p.homeForRequest(true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	link := filepath.Join(home, cursorUserHomeDir, ".cursor", "auth.json")
	if target, err := os.Readlink(link); err != nil || target != authFile {
		t.Fatalf("file-store auth link = %q, err=%v, want %q", target, err, authFile)
	}
	if _, err := os.Stat(filepath.Join(home, cursorUserHomeDir, "Library", "Keychains")); !os.IsNotExist(err) {
		t.Fatalf("keychain bridge created for file store: %v", err)
	}
}

func TestCursorHomeSkipsCredentialsForMemoryStore(t *testing.T) {
	realHome := t.TempDir()
	writeTestLoginKeychain(t, realHome, "login.keychain-db")
	t.Setenv("HOME", realHome)

	p := NewCursorBinProvider("auto-smart", map[string]string{"AGENT_CLI_CREDENTIAL_STORE": "memory"})
	home, cleanup, err := p.homeForRequest(true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	path := filepath.Join(home, cursorUserHomeDir, "Library", "Keychains")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("keychain bridge created for memory store: %v", err)
	}
}

func TestCursorBinListModelsPreparesMacOSKeychain(t *testing.T) {
	clearCursorExplicitCredentials(t)
	realHome := t.TempDir()
	keychain := writeTestLoginKeychain(t, realHome, "login.keychain-db")
	t.Setenv("HOME", realHome)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	previous := cursorModelsCommandOutput
	cursorModelsCommandOutput = func(_ context.Context, p *CursorBinProvider, home string) ([]byte, error) {
		link := filepath.Join(home, cursorUserHomeDir, "Library", "Keychains", "login.keychain-db")
		if target, err := os.Readlink(link); err != nil || target != keychain {
			t.Fatalf("model-list keychain link = %q, err=%v, want %q", target, err, keychain)
		}
		return []byte("auto - Auto (default)\n"), nil
	}
	t.Cleanup(func() { cursorModelsCommandOutput = previous })

	if _, err := NewCursorBinProvider("", nil).ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
}
