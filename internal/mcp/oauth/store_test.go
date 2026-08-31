package oauth

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func testSession(endpoint string) *Session {
	return &Session{
		Endpoint: endpoint,
		Issuer:   "https://auth.example",
		Config: OAuth2Config{
			ClientID: "client", Endpoint: oauth2.Endpoint{AuthURL: "https://auth.example/authorize", TokenURL: "https://auth.example/token"},
		},
		Token: &oauth2.Token{AccessToken: "access", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)},
	}
}

func TestCanonicalEndpoint(t *testing.T) {
	got, err := CanonicalEndpoint("HTTPS://EXAMPLE.COM:443/mcp?tenant=one#fragment")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://example.com/mcp?tenant=one"; got != want {
		t.Fatalf("CanonicalEndpoint = %q, want %q", got, want)
	}
}

func TestFileStorePermissionsAndStaleWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "mcp_oauth.json")
	store := NewFileStore(path)
	endpoint := "https://mcp.example/mcp"
	saved, err := store.Update(endpoint, func(*Session) (*Session, error) { return testSession(endpoint), nil })
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 1 {
		t.Fatalf("version = %d, want 1", saved.Version)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("mode = %o, want 600", got)
		}
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if got := dirInfo.Mode().Perm(); got != 0o700 {
			t.Fatalf("directory mode = %o, want 700", got)
		}
	}
	_, err = store.Update(endpoint, func(current *Session) (*Session, error) {
		current.Version--
		return current, nil
	})
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("stale update error = %v, want ErrStaleVersion", err)
	}
	loaded, err := store.Load(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != 1 || loaded.Token.RefreshToken != "refresh" {
		t.Fatalf("stale write changed stored session: %+v", loaded)
	}
}

func TestFileStoreRejectsSymlinkAndCorruption(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(`{"version":1,"entries":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "mcp_oauth.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	store := NewFileStore(link)
	if _, err := store.Load("https://mcp.example/mcp"); err == nil || !stringsContains(err.Error(), "regular file") {
		t.Fatalf("symlink Load error = %v, want regular file rejection", err)
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte(`{"version":1,"entries":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(corrupt).Load("https://mcp.example/mcp"); err == nil || !stringsContains(err.Error(), "parse MCP OAuth store") {
		t.Fatalf("corrupt Load error = %v", err)
	}
}

func stringsContains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
