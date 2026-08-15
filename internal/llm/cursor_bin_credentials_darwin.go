//go:build darwin

package llm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// prepareCursorPlatformCredentials makes Cursor's login-keychain credentials
// available while preserving the private HOME used to isolate Cursor's user
// settings, skills, and project state.
func prepareCursorPlatformCredentials(realHome, privateHome string) error {
	if realHome == "" {
		return nil
	}
	// Cursor initializes its default credential manager even when CURSOR_API_KEY
	// is set, so the login keychain must remain visible whenever the default
	// store is selected.
	src, err := macOSLoginKeychainPath(realHome)
	if errors.Is(err, errMacOSLoginKeychainNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	dst := filepath.Join(privateHome, "Library", "Keychains", filepath.Base(src))
	return linkCursorCredential(src, dst, "login keychain")
}

// Cursor's file credential store ignores CURSOR_CONFIG_DIR on macOS and reads
// ~/.cursor/auth.json, so expose that one file inside the isolated HOME.
func prepareCursorFileCredentials(realHome, privateHome string) error {
	if realHome == "" {
		return nil
	}
	src := filepath.Join(realHome, ".cursor", "auth.json")
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("locate cursor-bin auth file: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("locate cursor-bin auth file: %s is a directory", src)
	}
	return linkCursorCredential(src, filepath.Join(privateHome, ".cursor", "auth.json"), "auth file")
}

func linkCursorCredential(src, dst, name string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("create cursor-bin credential directory: %w", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		target, err := os.Readlink(dst)
		if err == nil {
			if filepath.Clean(target) == filepath.Clean(src) {
				return nil
			}
			// The private home belongs to term-llm. Replace a stale credential
			// link after a home or credential migration.
			if err := os.Remove(dst); err != nil {
				return fmt.Errorf("replace stale cursor-bin %s link: %w", name, err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect cursor-bin %s link: %w", name, err)
		}
		if err := os.Symlink(src, dst); err == nil {
			return nil
		} else if !os.IsExist(err) {
			return fmt.Errorf("link cursor-bin %s: %w", name, err)
		}
	}
	return fmt.Errorf("link cursor-bin %s: %s changed concurrently", name, dst)
}
