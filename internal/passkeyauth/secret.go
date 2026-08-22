package passkeyauth

import (
	"fmt"
	"os"
	"runtime"
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
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("passkey secret file has insecure permissions %04o; use 0400 or 0600", info.Mode().Perm())
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
