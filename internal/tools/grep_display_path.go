package tools

import (
	"path/filepath"
	"strings"
)

// grepDisplayBase returns the canonical session working directory that grep
// output paths are shown relative to, or "" to keep absolute paths.
//
// Relative paths save context tokens on every header, and they round-trip:
// read_file, edit_file, write_file, glob and friends resolve relative paths
// against the same WorkingDir (resolveToolPathWithConfig). Match paths come
// from the canonicalized search root, so the base must be canonical too.
func (t *GrepTool) grepDisplayBase() string {
	if t == nil || t.config == nil {
		return ""
	}
	wd := strings.TrimSpace(t.config.WorkingDir())
	if wd == "" {
		return ""
	}
	canonical, err := canonicalizePath(wd)
	if err != nil {
		return ""
	}
	return canonical
}

// grepDisplayPath shows path relative to base when it lies inside base, and
// leaves it untouched otherwise. Paths outside base stay absolute rather than
// becoming ../ chains, which are longer and easier to misresolve.
func grepDisplayPath(base, path string) string {
	if base == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return path
	}
	return rel
}

// formatGrepDisplay formats matches with display paths relative to base.
// When any path was shortened, the output records the base once: grep
// results persist in session history, and the working directory can change
// later (worktree switches, SetBaseDir), so a bare relative path must never
// be the only record of which file matched.
//
// The copy is for formatting only: enrichment, mtime sorting, file reads and
// classifier items keep the absolute paths.
func formatGrepDisplay(base string, matches []GrepMatch, format func([]GrepMatch) string) string {
	if base == "" || len(matches) == 0 {
		return format(matches)
	}
	display := make([]GrepMatch, len(matches))
	changed := false
	for i, m := range matches {
		if rel := grepDisplayPath(base, m.FilePath); rel != m.FilePath {
			m.FilePath = rel
			changed = true
		}
		display[i] = m
	}
	if !changed {
		return format(matches)
	}
	return "[paths relative to: " + base + "]\n" + format(display)
}
