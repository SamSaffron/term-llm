package passkeyauth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReadPrivateSecretFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("abcdefghijklmnopqrstuv"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPrivateSecretFile(path)
	if err != nil || string(got) != "abcdefghijklmnopqrstuv" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPrivateSecretFile(path); err == nil {
			t.Fatal("accepted insecure secret file")
		}
		target := filepath.Join(t.TempDir(), "target")
		_ = os.WriteFile(target, []byte("abcdefghijklmnopqrstuv"), 0o600)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err == nil {
			if _, err := ReadPrivateSecretFile(link); err == nil {
				t.Fatal("accepted symlink secret")
			}
		}
	}
}
func TestGenerateBootstrapSecret(t *testing.T) {
	secret, display, err := GenerateBootstrapSecret(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 39 || string(secret) != display {
		t.Fatalf("secret=%q display=%q", secret, display)
	}
	if err := ValidateHostSecret(secret); err != nil {
		t.Fatal(err)
	}
}
