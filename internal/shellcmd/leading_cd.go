package shellcmd

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// ExpandTilde resolves a tilde prefix in a path.
// "~" or "~/sub" uses os.UserHomeDir; "~user" or "~user/sub" uses user.Lookup.
// Returns ("", false) if expansion fails.
func ExpandTilde(path string) (string, bool) {
	if path == "" || path[0] != '~' {
		return path, true
	}

	// Split into tilde component and optional rest after first separator.
	rest := ""
	slash := strings.IndexAny(path, string([]byte{'/', filepath.Separator}))
	tildePrefix := path
	if slash > 0 {
		tildePrefix = path[:slash]
		rest = path[slash:] // keeps the leading separator
	}

	var home string
	if tildePrefix == "~" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		home = h
	} else {
		username := tildePrefix[1:]
		u, err := user.Lookup(username)
		if err != nil {
			return "", false
		}
		home = u.HomeDir
	}
	return home + rest, true
}

// ExtractLeadingCd strips a leading "cd <dir> && " from a shell command and
// folds the directory into workDir. If the pattern is not matched or the path
// cannot be resolved, the original command and workDir are returned unchanged.
//
// Conservative by design: only rewrites plain literal paths whose meaning can
// be modelled exactly. No escape-sequence handling inside quotes, and only the
// "&&" separator is recognised (not ";"). Tilde expansion is only performed on
// unquoted paths (in POSIX shell, cd "~/x" does NOT expand the tilde).
func ExtractLeadingCd(command, workDir string) (string, string) {
	trimmed := strings.TrimSpace(command)
	if !strings.HasPrefix(trimmed, "cd ") && !strings.HasPrefix(trimmed, "cd\t") {
		return command, workDir
	}

	after := strings.TrimLeft(trimmed[2:], " \t") // skip "cd" + whitespace

	// Parse the path — track whether it was quoted so we can avoid
	// expanding shell constructs that quoting would suppress.
	var path, rest string
	quoted := false
	switch {
	case strings.HasPrefix(after, "'"):
		end := strings.Index(after[1:], "'")
		if end < 0 {
			return command, workDir
		}
		path = after[1 : end+1]
		rest = strings.TrimLeft(after[end+2:], " \t")
		quoted = true

	case strings.HasPrefix(after, "\""):
		end := strings.Index(after[1:], "\"")
		if end < 0 {
			return command, workDir
		}
		path = after[1 : end+1]
		rest = strings.TrimLeft(after[end+2:], " \t")
		quoted = true

	default:
		// Unquoted: path extends to next whitespace.
		idx := strings.IndexAny(after, " \t")
		if idx < 0 {
			return command, workDir // bare "cd path" with no continuation
		}
		path = after[:idx]
		rest = strings.TrimLeft(after[idx:], " \t")
	}

	// Must be followed by "&&" and a non-empty command.
	if !strings.HasPrefix(rest, "&&") {
		return command, workDir
	}
	remaining := strings.TrimLeft(rest[2:], " \t")
	if remaining == "" {
		return command, workDir
	}

	// Bail on shell-special cd operands we cannot model.
	if path == "-" || path == "~+" || path == "~-" {
		return command, workDir
	}

	// Bail on env-var, backtick, or backslash-escape expansion.
	if strings.ContainsAny(path, "$`\\") {
		return command, workDir
	}

	// Tilde expansion — only for unquoted paths. In POSIX shell,
	// cd "~/foo" and cd '~' treat ~ as a literal character.
	if !quoted && strings.HasPrefix(path, "~") {
		expanded, ok := ExpandTilde(path)
		if !ok {
			return command, workDir
		}
		path = expanded
	} else if strings.HasPrefix(path, "~") {
		// Quoted tilde — can't resolve without shell, bail.
		return command, workDir
	}

	// Resolve relative paths against workDir (or cwd).
	if !filepath.IsAbs(path) {
		base := workDir
		if base == "" {
			var err error
			base, err = os.Getwd()
			if err != nil {
				return command, workDir
			}
		}
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)

	return remaining, path
}
