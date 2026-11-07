package passkeyauth

import (
	"fmt"
	"os"

	"github.com/samsaffron/term-llm/internal/fileprivacy"
)

// ReadPrivateSecretFile reads an operator-managed bootstrap/recovery secret
// without following symlinks or accepting broadly accessible Unix files.
func ReadPrivateSecretFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect passkey secret file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("passkey secret file must be a regular non-symlink file")
	}
	if err := fileprivacy.CheckFile(info); err != nil {
		return nil, fmt.Errorf("passkey secret file has insecure permissions; use 0400 or 0600: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read passkey secret file: %w", err)
	}
	if err := ValidateHostSecret(data); err != nil {
		return nil, err
	}
	return canonicalSecret(data), nil
}
