//go:build darwin

package llm

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	agyKeychainService = "gemini"
	agyKeychainAccount = "antigravity"
)

func detectAgyPlatformCredentials(home string) bool {
	cmd, err := agySecurityCredentialCommand(home)
	if err != nil {
		return false
	}
	return cmd.Run() == nil
}

var agyPlatformHasCredentials = detectAgyPlatformCredentials

func agySecurityCredentialCommand(home string) (*exec.Cmd, error) {
	security, err := exec.LookPath("security")
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(security, "find-generic-password", "-s", agyKeychainService, "-a", agyKeychainAccount)
	cmd.Env = envWithValue(os.Environ(), "HOME", home)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd, nil
}

func prepareAgyPlatformCredentials(realHome, privateHome string) (bool, error) {
	if !agyPlatformHasCredentials(realHome) {
		return false, nil
	}

	// macOS resolves the default login keychain relative to HOME. agy stores its
	// OAuth token there, so expose only that database inside agy's private HOME
	// rather than giving agy the real HOME (and its workspace/tool settings).
	src, err := agyLoginKeychainPath(realHome)
	if err != nil {
		return false, err
	}
	dir := filepath.Join(privateHome, "Library", "Keychains")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("create agy keychain directory: %w", err)
	}
	dst := filepath.Join(dir, filepath.Base(src))
	if target, err := os.Readlink(dst); err == nil {
		if filepath.Clean(target) != filepath.Clean(src) {
			return false, fmt.Errorf("link agy login keychain: %s already points to %s, want %s", dst, target, src)
		}
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect agy login keychain link: %w", err)
	}
	if err := os.Symlink(src, dst); err != nil {
		return false, fmt.Errorf("link agy login keychain: %w", err)
	}
	return true, nil
}

func agyLoginKeychainPath(home string) (string, error) {
	dir := filepath.Join(home, "Library", "Keychains")
	for _, name := range []string{"login.keychain-db", "login.keychain"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			return path, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("locate agy login keychain: %w", err)
		}
	}
	return "", fmt.Errorf("locate agy login keychain: neither login.keychain-db nor login.keychain exists in %s", dir)
}

func envWithValue(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
			continue
		}
		out = append(out, entry)
	}
	return append(out, prefix+value)
}
