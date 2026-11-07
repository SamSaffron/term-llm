package peerbroker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSecretFileAcceptsPrivateFileAfterOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	secret := strings.Repeat("s", MinCredentialLen)
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSecretFile(path)
	if err != nil {
		t.Fatalf("ReadSecretFile: %v", err)
	}
	if got != secret {
		t.Fatalf("secret = %q", got)
	}
}
