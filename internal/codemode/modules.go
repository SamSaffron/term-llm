// Package codemode runs bounded, fresh QuickJS cells without ambient host authority.
package codemode

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Module snapshot limits.
const (
	MaxSnapshotFiles     = 32       // maximum number of source files
	MaxSnapshotBytes     = 1 << 20  // 1 MiB total source bytes
	MaxSnapshotTypesSize = 64 << 10 // 64 KiB for declaration file
)

// SourceManifest describes the ES module files to include in a snapshot.
// It is derived from the skill's parsed CodeModeManifest at activation time
// and contains only file layout; tool requirements are handled by the caller.
type SourceManifest struct {
	// Module is the entry-point file, relative to root.
	// It must appear in Files.
	Module string

	// Files lists all declared source files, relative to root.
	Files []string

	// Types is an optional TypeScript declaration file (.d.ts), relative to root.
	// May be empty.
	Types string
}

// ModuleSnapshot is an immutable, admitted source snapshot for one skill.
// It implements Modules; the same value is safe for concurrent use.
type ModuleSnapshot struct {
	name    string            // skill name, used in skill:<name> specifiers
	entry   string            // normalized specifier for the entry point
	sources map[string]string // normalized specifier → source text
	types   string            // content of the .d.ts file (may be "")
	hash    [32]byte          // SHA-256 of all snapshotted bytes
}

// Snapshot reads all files declared in m from root and returns an immutable,
// content-addressed module resolver. It rejects path traversal, symlink
// escapes, duplicate/case-alias entries, unsupported schemes, and enforces
// byte/file/types size limits. The root directory is opened with os.OpenRoot
// to prevent symlink escapes after the initial open.
func Snapshot(skillName, root string, m SourceManifest) (*ModuleSnapshot, error) {
	if err := validateSnapshotInputs(skillName, m); err != nil {
		return nil, err
	}

	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open skill root %q: %w", root, err)
	}
	defer r.Close()

	sources := make(map[string]string, len(m.Files))
	h := sha256.New()
	var totalBytes int

	for _, f := range m.Files {
		content, err := readSourceFile(r, f)
		if err != nil {
			return nil, fmt.Errorf("snapshot %q: read %q: %w", skillName, f, err)
		}
		totalBytes += len(content)
		if totalBytes > MaxSnapshotBytes {
			return nil, fmt.Errorf("snapshot %q: total source size exceeds %d bytes", skillName, MaxSnapshotBytes)
		}
		key := normalizedKey(skillName, f)
		sources[key] = content
		fmt.Fprintf(h, "%s\x00%d\x00", f, len(content))
		h.Write([]byte(content))
	}

	var typesContent string
	if m.Types != "" {
		b, err := readDeclFile(r, m.Types)
		if err != nil {
			return nil, fmt.Errorf("snapshot %q: read types %q: %w", skillName, m.Types, err)
		}
		typesContent = string(b)
		fmt.Fprintf(h, "types\x00%d\x00", len(typesContent))
		h.Write(b)
	}

	var digest [32]byte
	copy(digest[:], h.Sum(nil))

	return &ModuleSnapshot{
		name:    skillName,
		entry:   normalizedKey(skillName, m.Module),
		sources: sources,
		types:   typesContent,
		hash:    digest,
	}, nil
}

// Normalize implements Modules. It resolves:
//   - "skill:<name>" → entry specifier
//   - "./<path>" relative to base → within-snapshot specifier
//
// It rejects cross-skill imports, parent traversal, node:/http:/https:/file:
// schemes, absolute paths, and specifiers not present in the snapshot.
func (s *ModuleSnapshot) Normalize(base, specifier string) (string, error) {
	if strings.HasPrefix(specifier, "skill:") {
		target := strings.TrimPrefix(specifier, "skill:")
		if target != s.name {
			return "", fmt.Errorf("cross-skill import not supported: %q", specifier)
		}
		return s.entry, nil
	}

	for _, scheme := range []string{"node:", "http:", "https:", "file:", "data:"} {
		if strings.HasPrefix(specifier, scheme) {
			return "", fmt.Errorf("import scheme not allowed: %q", specifier)
		}
	}

	if strings.HasPrefix(specifier, "/") || isAbsoluteWindowsPath(specifier) {
		return "", fmt.Errorf("absolute import path not allowed: %q", specifier)
	}

	if !strings.HasPrefix(specifier, "./") && !strings.HasPrefix(specifier, "../") {
		// bare specifier without a recognized scheme
		return "", fmt.Errorf("undeclared or bare import not allowed: %q", specifier)
	}

	if strings.HasPrefix(specifier, "../") {
		return "", fmt.Errorf("parent-directory import not allowed: %q", specifier)
	}

	// Relative import: resolve against base.
	resolved, err := resolveRelative(s.name, base, specifier)
	if err != nil {
		return "", err
	}
	if _, ok := s.sources[resolved]; !ok {
		return "", fmt.Errorf("import %q not in admitted snapshot", specifier)
	}
	return resolved, nil
}

// Source implements Modules. It returns the source text for a normalized
// specifier previously returned by Normalize.
func (s *ModuleSnapshot) Source(name string) (string, error) {
	src, ok := s.sources[name]
	if !ok {
		return "", fmt.Errorf("module %q not in admitted snapshot", name)
	}
	return src, nil
}

// SkillName returns the skill name this snapshot was created for.
func (s *ModuleSnapshot) SkillName() string { return s.name }

// EntrySpecifier returns the normalized specifier for the entry-point module.
func (s *ModuleSnapshot) EntrySpecifier() string { return s.entry }

// TypesContent returns the content of the TypeScript declaration file,
// or an empty string if no types file was included.
func (s *ModuleSnapshot) TypesContent() string { return s.types }

// Hash returns the SHA-256 digest of the snapshotted bytes.
func (s *ModuleSnapshot) Hash() [32]byte { return s.hash }

// normalizedKey constructs the canonical specifier for a file within a snapshot.
// Format: "<skillName>/<file>" where file uses forward slashes.
func normalizedKey(skillName, file string) string {
	return skillName + "/" + filepath.ToSlash(filepath.Clean(file))
}

// resolveRelative resolves a "./" relative specifier against a base specifier.
func resolveRelative(skillName, base, specifier string) (string, error) {
	if strings.ContainsAny(specifier, "\\:") {
		return "", fmt.Errorf("invalid relative specifier: %q", specifier)
	}
	for _, component := range strings.Split(specifier, "/") {
		if component == ".." {
			return "", fmt.Errorf("parent traversal not allowed: %q", specifier)
		}
	}
	// base is a normalized key: "<skillName>/<path>", or empty for root.
	var baseDir string
	if base == "" {
		baseDir = skillName
	} else {
		baseDir = skillName + "/" + filepath.ToSlash(filepath.Dir(strings.TrimPrefix(base, skillName+"/")))
	}

	joined := baseDir + "/" + strings.TrimPrefix(specifier, "./")
	clean := filepath.ToSlash(filepath.Clean(joined))

	prefix := skillName + "/"
	if !strings.HasPrefix(clean, prefix) {
		return "", fmt.Errorf("import escapes skill root: %q", specifier)
	}
	return clean, nil
}

// isAbsoluteWindowsPath reports whether p looks like an absolute Windows path
// (e.g. "C:\..." or "C:/..."), which must be rejected even on non-Windows hosts.
func isAbsoluteWindowsPath(p string) bool {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '/' || p[2] == '\\') {
		c := p[0]
		return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
	}
	return false
}

// validateSnapshotInputs performs manifest-level checks before any disk I/O.
func validateSnapshotInputs(skillName string, m SourceManifest) error {
	if skillName == "" {
		return fmt.Errorf("skill name must not be empty")
	}
	if strings.ContainsAny(skillName, "/\\:") {
		return fmt.Errorf("skill name must not contain path separators or colons: %q", skillName)
	}
	if m.Module == "" {
		return fmt.Errorf("manifest module must not be empty")
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("manifest files must not be empty")
	}
	if len(m.Files) > MaxSnapshotFiles {
		return fmt.Errorf("manifest files: at most %d files allowed, got %d", MaxSnapshotFiles, len(m.Files))
	}

	seen := make(map[string]bool, len(m.Files))
	entryInFiles := false
	for _, f := range m.Files {
		if err := validateSnapshotPath(f); err != nil {
			return fmt.Errorf("manifest files %q: %w", f, err)
		}
		lower := strings.ToLower(filepath.ToSlash(filepath.Clean(f)))
		if seen[lower] {
			return fmt.Errorf("manifest files: duplicate or case-alias entry %q", f)
		}
		seen[lower] = true
		if f == m.Module {
			entryInFiles = true
		}
	}
	if !entryInFiles {
		return fmt.Errorf("manifest module %q must be listed in files", m.Module)
	}

	if m.Types != "" {
		if err := validateSnapshotDeclPath(m.Types); err != nil {
			return fmt.Errorf("manifest types %q: %w", m.Types, err)
		}
	}
	return nil
}

// validateSnapshotPath checks a source file path for safety.
func validateSnapshotPath(p string) error {
	if p == "" {
		return fmt.Errorf("path must not be empty")
	}
	if strings.ContainsAny(p, "\\") {
		return fmt.Errorf("backslashes not allowed: %q", p)
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("absolute path not allowed: %q", p)
	}
	if strings.Contains(p, ":") {
		return fmt.Errorf("scheme specifier not allowed: %q", p)
	}
	for _, component := range strings.Split(p, "/") {
		if component == ".." {
			return fmt.Errorf("path traversal not allowed: %q", p)
		}
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if strings.HasPrefix(clean, "..") {
		return fmt.Errorf("path traversal not allowed: %q", p)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid path component in %q", p)
		}
	}
	ext := strings.ToLower(filepath.Ext(p))
	if ext != ".mjs" && ext != ".js" {
		return fmt.Errorf("unsupported extension %q (allowed: .mjs, .js): %q", ext, p)
	}
	return nil
}

// validateSnapshotDeclPath checks a types declaration path for safety.
func validateSnapshotDeclPath(p string) error {
	if p == "" {
		return fmt.Errorf("path must not be empty")
	}
	if strings.ContainsAny(p, "\\") {
		return fmt.Errorf("backslashes not allowed: %q", p)
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("absolute path not allowed: %q", p)
	}
	if strings.Contains(p, ":") {
		return fmt.Errorf("scheme specifier not allowed: %q", p)
	}
	for _, component := range strings.Split(p, "/") {
		if component == ".." {
			return fmt.Errorf("path traversal not allowed: %q", p)
		}
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if strings.HasPrefix(clean, "..") {
		return fmt.Errorf("path traversal not allowed: %q", p)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid path component in %q", p)
		}
	}
	if !strings.HasSuffix(strings.ToLower(p), ".d.ts") {
		return fmt.Errorf("types file must have .d.ts extension: %q", p)
	}
	return nil
}

// readSourceFile reads a declared source file through the rooted handle.
func readSourceFile(r *os.Root, p string) (string, error) {
	b, err := readSnapshotFile(r, p, MaxSnapshotBytes)
	return string(b), err
}
func readDeclFile(r *os.Root, p string) ([]byte, error) {
	return readSnapshotFile(r, p, MaxSnapshotTypesSize)
}
