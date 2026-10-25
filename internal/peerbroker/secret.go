package peerbroker

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func ReadSecretFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("peerbroker: secret file path is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("peerbroker: inspect secret file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 4096 {
		return "", fmt.Errorf("peerbroker: secret file must be private (0600), regular, and at most 4096 bytes")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("peerbroker: open secret file: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("peerbroker: stat secret file: %w", err)
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("peerbroker: secret file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", fmt.Errorf("peerbroker: read secret file: %w", err)
	}
	secret := strings.TrimSpace(string(data))
	if err := validateCredential(secret); err != nil {
		return "", fmt.Errorf("peerbroker: invalid secret file: %w", err)
	}
	return secret, nil
}
