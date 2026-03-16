// Package pathutil provides shared path manipulation utilities.
package pathutil

import (
	"fmt"
	"os"
	"os/user"
	"strings"
)

// Expand resolves shell-style tilde expansions in a path:
//
//   - "~"            → current user's home directory
//   - "~/subpath"    → current user's home directory + subpath
//   - "~username"    → named user's home directory
//   - "~username/subpath" → named user's home directory + subpath
//
// All other paths are returned unchanged.
// "." and ".." components are not resolved here; callers should pass the
// result through filepath.Abs or filepath.Clean as needed.
//
// Note: "~username" lookup uses the OS user database. An error is returned
// if the named user does not exist.
func Expand(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}

	// Split into username portion and the rest (everything from first "/" on).
	slash := strings.IndexByte(path, '/')
	var username, rest string
	if slash == -1 {
		username = path[1:]
		rest = ""
	} else {
		username = path[1:slash]
		rest = path[slash:] // retains the leading "/"
	}

	var homeDir string
	if username == "" {
		// bare ~ or ~/...
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: cannot determine home directory: %w", err)
		}
	} else {
		// ~username or ~username/...
		u, err := user.Lookup(username)
		if err != nil {
			return "", fmt.Errorf("expand ~%s: user not found: %w", username, err)
		}
		homeDir = u.HomeDir
	}

	return homeDir + rest, nil
}

// MustExpand is like Expand but returns the original path unchanged on error
// instead of surfacing the error. Useful in contexts where errors are
// non-fatal (e.g. serving default directories).
func MustExpand(path string) string {
	expanded, err := Expand(path)
	if err != nil {
		return path
	}
	return expanded
}
