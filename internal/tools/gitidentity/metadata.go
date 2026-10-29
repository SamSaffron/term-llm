package gitidentity

import (
	"os"
	"path/filepath"
	"strings"
)

// IsAdministrativePath reports whether a path names Git administrative metadata.
// It reserves .git entries and HEAD markers even before they exist, follows
// physical aliases, and recognizes bare/separate Git directories under arbitrary
// names. Reserving HEAD prevents assembling an implicit repository one file at a
// time, before any existing directory could be recognized as administrative.
// Discovery is uncached and never invokes Git or honors its environment/config.
// Unresolvable paths fail closed; this is a write boundary, not repo discovery.
func IsAdministrativePath(path string) bool {
	return isAdministrativePath(path, 0)
}

func isAdministrativePath(path string, links int) bool {
	name := administrativeComponent(filepath.Base(path))
	if strings.EqualFold(name, "HEAD") || containsGitComponent(path) || links >= 40 || isRepositoryStructurePath(path) {
		return true
	}
	// File writers may create a dangling link's target. Resolve that case too,
	// rather than either missing metadata or blocking ordinary sibling links.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return isAdministrativePath(resolved, links+1)
		} else if os.IsNotExist(err) {
			target, err := os.Readlink(path)
			if err != nil {
				return true
			}
			if !filepath.IsAbs(target) {
				target = unresolvedParent(path) + target
			}
			return isAdministrativePath(target, links+1)
		}
	}
	dir := physicalDirectory(path)
	if dir == "" || isRepositoryStructurePath(dir) {
		return true
	}
	for {
		if validGitDirectory(dir) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func administrativeComponent(name string) string {
	if filepath.Separator == '\\' {
		// Include Win32 aliases and NTFS default-stream syntax (HEAD::$DATA).
		name, _, _ = strings.Cut(name, ":")
		name = strings.TrimRight(name, " .")
	}
	return name
}

// A pre-existing HEAD may belong to an incomplete bare-repository fixture.
// Do not let an ordinary file write supply its missing objects/refs directory
// or a commondir redirect and activate config written before recognition.
func isRepositoryStructurePath(path string) bool {
	for path = filepath.Clean(path); ; {
		name := administrativeComponent(filepath.Base(path))
		parent := filepath.Dir(path)
		if strings.EqualFold(name, "objects") || strings.EqualFold(name, "refs") || strings.EqualFold(name, "commondir") {
			if info, err := os.Lstat(filepath.Join(parent, "HEAD")); err == nil && !info.IsDir() {
				return true
			}
		}
		if parent == path {
			return false
		}
		path = parent
	}
}
