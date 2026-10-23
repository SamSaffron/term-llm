package peerbroker

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// ReadCapabilityFile reads a bounded private regular file, rejecting symlinks
// and checking the opened descriptor still identifies the inspected file.
func ReadCapabilityFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("a private capability file is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 4096 {
		return "", fmt.Errorf("capability file must be a private regular file (0600), at most 4096 bytes")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("capability file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 || len(token) > 128 {
		return "", fmt.Errorf("capability must be 32-128 printable non-space ASCII characters")
	}
	for _, b := range []byte(token) {
		if b < 0x21 || b > 0x7e {
			return "", fmt.Errorf("capability must be 32-128 printable non-space ASCII characters")
		}
	}
	return token, nil
}
