// Package gitidentity validates checkout membership before sharing a Git approval identity.
package gitidentity

import (
	"os"
	"path/filepath"
	"strings"
)

// Root discovers an approval identity from the physical filesystem, without
// invoking Git or honoring core.worktree, GIT_DIR, or other Git overrides.
// Ordinary checkouts, separate Git directories, and submodules use their own
// checkout path. Only registered linked worktrees share the main checkout path.
// There is deliberately no identity cache: even an exact-path cache would miss
// newly created nested repositories, replaced .git entries and symlink changes.
func Root(path string) string {
	// Protect the entry itself and not-yet-created metadata paths, too.
	if containsGitComponent(path) {
		return ""
	}
	dir := physicalDirectory(path)
	for dir != "" {
		// Bare repositories and separate Git directories can be embedded in
		// a checkout under any name. Never inherit its project grants there.
		if validGitDirectory(dir) {
			return ""
		}
		entry := filepath.Join(dir, ".git")
		info, err := os.Lstat(entry)
		if err == nil {
			return checkoutRoot(dir, entry, info)
		}
		if !os.IsNotExist(err) {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// physicalDirectory resolves existing paths before taking their parent (a file
// symlink can target a different checkout). Missing paths use their nearest
// existing ancestor, but dangling symlinks and inaccessible paths fail closed.
func physicalDirectory(path string) string {
	if !filepath.IsAbs(path) {
		// Windows drive-relative and root-relative paths cannot be safely
		// resolved by prefixing cwd. Do not alias them to ordinary relatives.
		if filepath.VolumeName(path) != "" || (len(path) > 0 && os.IsPathSeparator(path[0])) {
			return ""
		}
		cwd, err := os.Getwd()
		if err != nil {
			return ""
		}
		// Abs/Join would clean symlink/.. before physical resolution.
		path = cwd + string(filepath.Separator) + path
	}
	for {
		info, err := os.Lstat(path)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(path)
			// Check before dropping a file's name: a symlink can resolve to a
			// registered worktree's .git file, not just a .git directory.
			if err != nil || containsGitComponent(resolved) {
				return ""
			}
			if info.Mode()&os.ModeSymlink != 0 {
				info, err = os.Stat(resolved)
				if err != nil {
					return ""
				}
			}
			if !info.IsDir() {
				resolved = filepath.Dir(resolved)
			}
			return resolved
		}
		if !os.IsNotExist(err) {
			return ""
		}
		path = unresolvedParent(path)
		if path == "" {
			return ""
		}
	}
}

// containsGitComponent also rejects alternate casing on case-insensitive
// filesystems. This is an approval boundary, not Git's repository parser.
func containsGitComponent(path string) bool {
	for component := range strings.SplitSeq(filepath.ToSlash(path), "/") {
		if filepath.Separator == '\\' {
			component, _, _ = strings.Cut(component, ":")
			component = strings.TrimRight(component, " .")
		}
		if strings.EqualFold(component, ".git") {
			return true
		}
	}
	return false
}

// unresolvedParent removes the last component without cleaning symlink/.. .
// Its trailing separator preserves volume roots on Windows.
func unresolvedParent(path string) string {
	end := len(path)
	for end > 0 && os.IsPathSeparator(path[end-1]) {
		end--
	}
	for end > 0 && !os.IsPathSeparator(path[end-1]) {
		end--
	}
	return path[:end]
}

func checkoutRoot(root, entry string, info os.FileInfo) string {
	gitDir := entry
	if !info.IsDir() {
		// Never follow a .git symlink: it is not a checkout registration.
		if !info.Mode().IsRegular() {
			return ""
		}
		contents, ok := pathFile(entry)
		if !ok || !strings.HasPrefix(contents, "gitdir: ") {
			return ""
		}
		gitDir = resolvePath(root, strings.TrimPrefix(contents, "gitdir: "))
		if gitDir == "" {
			return ""
		}
	}

	commonFile := filepath.Join(gitDir, "commondir")
	if _, err := os.Lstat(commonFile); os.IsNotExist(err) {
		// An arbitrary .git file may point at a separate Git directory, a
		// submodule admin directory, or even somebody else's main .git. It
		// never grants that other checkout's identity.
		if validGitDirectory(gitDir) {
			return root
		}
		return ""
	}
	commonPath, ok := pathFile(commonFile)
	if !ok || !info.Mode().IsRegular() {
		return ""
	}
	common := resolvePath(gitDir, commonPath)
	if common == "" || filepath.Base(common) != ".git" || !validGitDirectory(common) {
		return ""
	}
	// Registration must physically live inside the main checkout's registry;
	// a commondir declaration alone is not evidence of membership.
	if filepath.Dir(gitDir) != filepath.Join(common, "worktrees") {
		return ""
	}
	backlink, ok := pathFile(filepath.Join(gitDir, "gitdir"))
	if !ok || filepath.Base(backlink) != ".git" {
		return ""
	}
	// Resolve only the parent. A symlink substituted for the checkout's .git
	// file must not become equivalent to the actual registered .git entry.
	if resolvePath(gitDir, unresolvedParent(backlink)) != root {
		return ""
	}
	return filepath.Dir(common)
}

func resolvePath(base, path string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = base + string(filepath.Separator) + path
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return resolved
}

// pathFile reads only small regular metadata files, not FIFOs/devices or
// symlinks. Strip line endings, not path whitespace (spaces are valid in paths).
func pathFile(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"), true
}

func validGitDirectory(path string) bool {
	head, err := os.Stat(filepath.Join(path, "HEAD"))
	if err != nil || !head.Mode().IsRegular() {
		return false
	}
	for _, name := range []string{"objects", "refs"} {
		info, err := os.Stat(filepath.Join(path, name))
		if err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}
