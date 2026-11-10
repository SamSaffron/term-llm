//go:build !js

package codemode

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFiles writes path→content pairs under dir, creating parent directories as needed.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

// TestSnapshotBasic verifies that a well-formed manifest produces a correct snapshot.
func TestSnapshotBasic(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"js/index.mjs":   `export function hello() { return "hi"; }`,
		"js/helpers.mjs": `export const x = 1;`,
	})

	m := SourceManifest{
		Module: "js/index.mjs",
		Files:  []string{"js/index.mjs", "js/helpers.mjs"},
	}
	snap, err := Snapshot("my-skill", dir, m)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.SkillName() != "my-skill" {
		t.Errorf("SkillName = %q, want %q", snap.SkillName(), "my-skill")
	}
	wantEntry := "my-skill/js/index.mjs"
	if snap.EntrySpecifier() != wantEntry {
		t.Errorf("EntrySpecifier = %q, want %q", snap.EntrySpecifier(), wantEntry)
	}
	if snap.TypesContent() != "" {
		t.Errorf("TypesContent should be empty without types file")
	}
}

// TestSnapshotWithTypes verifies that a types file is included and accessible.
func TestSnapshotWithTypes(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"js/index.mjs":  `export function hello() {}`,
		"js/index.d.ts": `export declare function hello(): void;`,
	})

	snap, err := Snapshot("typed-skill", dir, SourceManifest{
		Module: "js/index.mjs",
		Files:  []string{"js/index.mjs"},
		Types:  "js/index.d.ts",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.TypesContent() == "" {
		t.Error("TypesContent should be non-empty")
	}
	if !strings.Contains(snap.TypesContent(), "declare function") {
		t.Errorf("unexpected TypesContent: %q", snap.TypesContent())
	}
}

// TestSnapshotImmutable verifies that modifying files after snapshot creation
// does not affect the snapshot.
func TestSnapshotImmutable(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.mjs": `export const v = 1;`,
	})
	snap, err := Snapshot("imm-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	origSrc, _ := snap.Source("imm-skill/index.mjs")

	// Overwrite the on-disk file.
	if err := os.WriteFile(filepath.Join(dir, "index.mjs"), []byte(`export const v = 999;`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Snapshot must still return original content.
	newSrc, _ := snap.Source("imm-skill/index.mjs")
	if newSrc != origSrc {
		t.Errorf("snapshot was not immutable: got %q after file change", newSrc)
	}
}

// TestSnapshotHashDeterministic verifies that two snapshots of the same files
// produce the same hash.
func TestSnapshotHashDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.mjs": `const a = 1;`,
		"b.mjs": `const b = 2;`,
	})
	m := SourceManifest{Module: "a.mjs", Files: []string{"a.mjs", "b.mjs"}}
	s1, err := Snapshot("det-skill", dir, m)
	if err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	s2, err := Snapshot("det-skill", dir, m)
	if err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if s1.Hash() != s2.Hash() {
		t.Error("hashes differ for identical files")
	}
}

// TestSnapshotHashChangesOnContent verifies that different content produces a
// different hash.
func TestSnapshotHashChangesOnContent(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeFiles(t, dir1, map[string]string{"a.mjs": `const x = 1;`})
	writeFiles(t, dir2, map[string]string{"a.mjs": `const x = 2;`})
	m := SourceManifest{Module: "a.mjs", Files: []string{"a.mjs"}}
	s1, _ := Snapshot("h-skill", dir1, m)
	s2, _ := Snapshot("h-skill", dir2, m)
	if s1.Hash() == s2.Hash() {
		t.Error("hashes should differ for different content")
	}
}

// TestSnapshotHashMatchesManual verifies the hash against a manually computed digest.
func TestSnapshotHashMatchesManual(t *testing.T) {
	dir := t.TempDir()
	content := `export const x = 42;`
	writeFiles(t, dir, map[string]string{"index.mjs": content})
	snap, err := Snapshot("manual-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00", "index.mjs", len(content))
	h.Write([]byte(content))
	var want [32]byte
	copy(want[:], h.Sum(nil))
	if snap.Hash() != want {
		t.Errorf("hash mismatch: got %x, want %x", snap.Hash(), want)
	}
}

// --- Normalize tests ---

// TestNormalizeSkillEntry verifies that "skill:<name>" resolves to the entry specifier.
func TestNormalizeSkillEntry(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": `export const x = 1;`})
	snap, _ := Snapshot("probe", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
	})
	got, err := snap.Normalize("", "skill:probe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "probe/index.mjs" {
		t.Errorf("Normalize = %q, want %q", got, "probe/index.mjs")
	}
}

// TestNormalizeRelativeFromRoot verifies relative resolution from an empty base.
func TestNormalizeRelativeFromRoot(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"js/index.mjs":   `import './helpers.mjs';`,
		"js/helpers.mjs": `export const h = 1;`,
	})
	snap, _ := Snapshot("rel-skill", dir, SourceManifest{
		Module: "js/index.mjs",
		Files:  []string{"js/index.mjs", "js/helpers.mjs"},
	})
	// From the entry file, resolve ./helpers.mjs
	got, err := snap.Normalize("rel-skill/js/index.mjs", "./helpers.mjs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "rel-skill/js/helpers.mjs" {
		t.Errorf("Normalize = %q, want %q", got, "rel-skill/js/helpers.mjs")
	}
}

// TestNormalizeRelativeNestedDir verifies resolution from a nested directory.
func TestNormalizeRelativeNestedDir(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"src/a/index.mjs": ``,
		"src/b/util.mjs":  ``,
	})
	snap, _ := Snapshot("nested", dir, SourceManifest{
		Module: "src/a/index.mjs",
		Files:  []string{"src/a/index.mjs", "src/b/util.mjs"},
	})
	// From src/a/index.mjs, ../b/util.mjs would be a parent traversal and must fail.
	_, err := snap.Normalize("nested/src/a/index.mjs", "../b/util.mjs")
	if err == nil {
		t.Fatal("expected error for ../b/util.mjs, got nil")
	}
}

// TestNormalizeCrossSkillRejected verifies that skill:<other> is rejected.
func TestNormalizeCrossSkillRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	snap, _ := Snapshot("my-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
	})
	_, err := snap.Normalize("", "skill:other-skill")
	if err == nil {
		t.Fatal("expected error for cross-skill import")
	}
	if !strings.Contains(err.Error(), "cross-skill") {
		t.Errorf("error should mention cross-skill: %v", err)
	}
}

// TestNormalizeRejectedSchemes verifies that node:, http:, https:, file: are all rejected.
func TestNormalizeRejectedSchemes(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	snap, _ := Snapshot("scheme-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
	})
	for _, bad := range []string{
		"node:fs",
		"http://example.com/mod.js",
		"https://cdn.example.com/mod.mjs",
		"file:///etc/passwd",
		"data:text/javascript,export default 1",
	} {
		_, err := snap.Normalize("", bad)
		if err == nil {
			t.Errorf("expected error for %q, got nil", bad)
		}
	}
}

// TestNormalizeAbsolutePathRejected verifies absolute paths are rejected.
func TestNormalizeAbsolutePathRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	snap, _ := Snapshot("abs-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
	})
	for _, bad := range []string{"/etc/passwd", "C:/Windows/file.mjs", "C:\\Windows\\file.mjs"} {
		_, err := snap.Normalize("", bad)
		if err == nil {
			t.Errorf("expected error for absolute path %q, got nil", bad)
		}
	}
}

// TestNormalizeParentTraversalRejected verifies ../ traversals are rejected.
func TestNormalizeParentTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	snap, _ := Snapshot("trav-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
	})
	_, err := snap.Normalize("trav-skill/index.mjs", "../secret.mjs")
	if err == nil {
		t.Fatal("expected error for ../secret.mjs")
	}
}

// TestNormalizeBareSpecifierRejected verifies that bare module names are rejected.
func TestNormalizeBareSpecifierRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	snap, _ := Snapshot("bare-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
	})
	_, err := snap.Normalize("", "some-library")
	if err == nil {
		t.Fatal("expected error for bare specifier")
	}
}

// TestNormalizeUndeclaredFileRejected verifies that importing an undeclared file fails.
func TestNormalizeUndeclaredFileRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.mjs":     ``,
		"untracked.mjs": ``,
	})
	snap, _ := Snapshot("decl-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"}, // untracked.mjs not declared
	})
	_, err := snap.Normalize("decl-skill/index.mjs", "./untracked.mjs")
	if err == nil {
		t.Fatal("expected error for undeclared file")
	}
}

// --- Source tests ---

// TestSourceReturnsContent verifies Source returns correct content.
func TestSourceReturnsContent(t *testing.T) {
	dir := t.TempDir()
	content := `export const greeting = "hello";`
	writeFiles(t, dir, map[string]string{"greet.mjs": content})
	snap, _ := Snapshot("src-skill", dir, SourceManifest{
		Module: "greet.mjs",
		Files:  []string{"greet.mjs"},
	})
	got, err := snap.Source("src-skill/greet.mjs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != content {
		t.Errorf("Source = %q, want %q", got, content)
	}
}

// TestSourceUnknownKeyErrors verifies Source returns error for unknown keys.
func TestSourceUnknownKeyErrors(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	snap, _ := Snapshot("unk-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
	})
	_, err := snap.Source("other-skill/index.mjs")
	if err == nil {
		t.Fatal("expected error for unknown module key")
	}
}

// --- Snapshot validation error tests ---

// TestSnapshotEmptySkillName verifies that an empty skill name is rejected.
func TestSnapshotEmptySkillName(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	_, err := Snapshot("", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
	})
	if err == nil {
		t.Fatal("expected error for empty skill name")
	}
}

// TestSnapshotSkillNameWithSlash verifies that skill names with slashes are rejected.
func TestSnapshotSkillNameWithSlash(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	_, err := Snapshot("my/skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
	})
	if err == nil {
		t.Fatal("expected error for skill name with slash")
	}
}

// TestSnapshotEmptyModule verifies that an empty module path is rejected.
func TestSnapshotEmptyModule(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	_, err := Snapshot("my-skill", dir, SourceManifest{
		Module: "",
		Files:  []string{"index.mjs"},
	})
	if err == nil {
		t.Fatal("expected error for empty module")
	}
}

// TestSnapshotModuleNotInFiles verifies rejection when module is not in files.
func TestSnapshotModuleNotInFiles(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.mjs":  ``,
		"helper.mjs": ``,
	})
	_, err := Snapshot("my-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"helper.mjs"}, // index.mjs omitted
	})
	if err == nil {
		t.Fatal("expected error when module not in files")
	}
}

// TestSnapshotTooManyFiles verifies the 32-file limit is enforced.
func TestSnapshotTooManyFiles(t *testing.T) {
	dir := t.TempDir()
	files := make([]string, MaxSnapshotFiles+1)
	fsfiles := make(map[string]string, MaxSnapshotFiles+1)
	for i := range files {
		name := fmt.Sprintf("f%03d.mjs", i)
		files[i] = name
		fsfiles[name] = `export const x = 1;`
	}
	writeFiles(t, dir, fsfiles)
	_, err := Snapshot("big-skill", dir, SourceManifest{
		Module: files[0],
		Files:  files,
	})
	if err == nil {
		t.Fatalf("expected error for >%d files", MaxSnapshotFiles)
	}
}

// TestSnapshotTotalSizeLimit verifies that 1 MiB total source is rejected.
func TestSnapshotTotalSizeLimit(t *testing.T) {
	dir := t.TempDir()
	// Two files each slightly over 512 KiB to exceed 1 MiB total.
	halfMiB := strings.Repeat("x", (MaxSnapshotBytes/2)+1)
	writeFiles(t, dir, map[string]string{
		"a.mjs": "// " + halfMiB,
		"b.mjs": "// " + halfMiB,
	})
	_, err := Snapshot("big-skill", dir, SourceManifest{
		Module: "a.mjs",
		Files:  []string{"a.mjs", "b.mjs"},
	})
	if err == nil {
		t.Fatal("expected error for total size exceeding 1 MiB")
	}
}

// TestSnapshotTypesSizeLimit verifies that the 64 KiB types limit is enforced.
func TestSnapshotTypesSizeLimit(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.mjs":  `export const x = 1;`,
		"index.d.ts": "// " + strings.Repeat("x", MaxSnapshotTypesSize+1),
	})
	_, err := Snapshot("types-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
		Types:  "index.d.ts",
	})
	if err == nil {
		t.Fatal("expected error for types file exceeding 64 KiB")
	}
}

// TestSnapshotDuplicateFileRejected verifies that duplicate entries are rejected.
func TestSnapshotDuplicateFileRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": `export const x = 1;`})
	_, err := Snapshot("dup-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs", "index.mjs"},
	})
	if err == nil {
		t.Fatal("expected error for duplicate file entry")
	}
}

// TestSnapshotCaseAliasRejected verifies that case-alias duplicates are rejected.
func TestSnapshotCaseAliasRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"Index.mjs": `export const x = 1;`,
		"index.mjs": `export const x = 2;`,
	})
	_, err := Snapshot("case-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs", "Index.mjs"},
	})
	if err == nil {
		t.Fatal("expected error for case-alias file entry")
	}
}

// TestSnapshotTraversalInFilesRejected verifies that "../" in file paths is rejected.
func TestSnapshotTraversalInFilesRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	_, err := Snapshot("trav-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs", "../secret.mjs"},
	})
	if err == nil {
		t.Fatal("expected error for traversal in files")
	}
}

// TestSnapshotAbsolutePathInFilesRejected verifies absolute paths in files are rejected.
func TestSnapshotAbsolutePathInFilesRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	_, err := Snapshot("abs-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs", "/etc/passwd"},
	})
	if err == nil {
		t.Fatal("expected error for absolute path in files")
	}
}

// TestSnapshotSchemeInFilesRejected verifies scheme specifiers in files are rejected.
func TestSnapshotSchemeInFilesRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	_, err := Snapshot("scheme-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs", "http://example.com/mod.mjs"},
	})
	if err == nil {
		t.Fatal("expected error for scheme in files")
	}
}

// TestSnapshotUnsupportedExtensionRejected verifies that .ts, .tsx etc. are rejected.
func TestSnapshotUnsupportedExtensionRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.mjs": ``,
		"helper.ts": ``,
	})
	_, err := Snapshot("ext-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs", "helper.ts"},
	})
	if err == nil {
		t.Fatal("expected error for .ts file")
	}
}

// TestSnapshotMissingFileErrors verifies that a declared but missing file is an error.
func TestSnapshotMissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	_, err := Snapshot("miss-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs", "missing.mjs"},
	})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestSnapshotSymlinkEscapeRejected verifies that symlinks pointing outside the
// root directory are rejected.
func TestSnapshotSymlinkEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	secret := t.TempDir()
	if err := os.WriteFile(filepath.Join(secret, "secret.txt"), []byte("sensitive"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	// Create a symlink inside dir pointing outside dir.
	if err := os.Symlink(filepath.Join(secret, "secret.txt"), filepath.Join(dir, "escape.mjs")); err != nil {
		t.Skip("symlink creation failed (may require elevated privileges):", err)
	}
	_, err := Snapshot("sym-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs", "escape.mjs"},
	})
	if err == nil {
		t.Fatal("expected error for symlink escape")
	}
}

// TestSnapshotTypesTraversalRejected verifies that ../traversal in types path is rejected.
func TestSnapshotTypesTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	_, err := Snapshot("types-trav", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
		Types:  "../secret.d.ts",
	})
	if err == nil {
		t.Fatal("expected error for types traversal")
	}
}

// TestSnapshotTypesWrongExtensionRejected verifies that types must be .d.ts.
func TestSnapshotTypesWrongExtensionRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.mjs": ``,
		"index.ts":  ``,
	})
	_, err := Snapshot("types-ext", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs"},
		Types:  "index.ts",
	})
	if err == nil {
		t.Fatal("expected error for .ts types (must be .d.ts)")
	}
}

// TestSnapshotNonRegularFileRejected verifies that directories listed as files fail.
func TestSnapshotNonRegularFileRejected(t *testing.T) {
	dir := t.TempDir()
	// Create a subdirectory named like a file.
	if err := os.MkdirAll(filepath.Join(dir, "subdir.mjs"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"index.mjs": ``})
	_, err := Snapshot("dir-skill", dir, SourceManifest{
		Module: "index.mjs",
		Files:  []string{"index.mjs", "subdir.mjs"},
	})
	if err == nil {
		t.Fatal("expected error for directory listed as source file")
	}
}

// --- NormalizeRelative round-trip through Normalize + Source ---

// TestNormalizeSourceRoundTrip verifies that Normalize followed by Source returns
// the original file content.
func TestNormalizeSourceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	helperSrc := `export const help = true;`
	writeFiles(t, dir, map[string]string{
		"main.mjs":   `import './helper.mjs';`,
		"helper.mjs": helperSrc,
	})
	snap, err := Snapshot("rt-skill", dir, SourceManifest{
		Module: "main.mjs",
		Files:  []string{"main.mjs", "helper.mjs"},
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Simulate dynamic import('./helper.mjs') from main.mjs.
	norm, err := snap.Normalize("rt-skill/main.mjs", "./helper.mjs")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	got, err := snap.Source(norm)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	if got != helperSrc {
		t.Errorf("Source = %q, want %q", got, helperSrc)
	}
}

// TestMultipleSnapshotsConcurrent verifies that multiple snapshots coexist
// without interfering with each other.
func TestMultipleSnapshotsConcurrent(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	writeFiles(t, dir1, map[string]string{"a.mjs": `const a = "skill1";`})
	writeFiles(t, dir2, map[string]string{"a.mjs": `const a = "skill2";`})
	s1, err := Snapshot("skill1", dir1, SourceManifest{Module: "a.mjs", Files: []string{"a.mjs"}})
	if err != nil {
		t.Fatalf("skill1 snapshot: %v", err)
	}
	s2, err := Snapshot("skill2", dir2, SourceManifest{Module: "a.mjs", Files: []string{"a.mjs"}})
	if err != nil {
		t.Fatalf("skill2 snapshot: %v", err)
	}
	src1, _ := s1.Source("skill1/a.mjs")
	src2, _ := s2.Source("skill2/a.mjs")
	if src1 == src2 {
		t.Error("different skills should have different content")
	}
	// Cross-skill source access should fail.
	_, err = s1.Source("skill2/a.mjs")
	if err == nil {
		t.Error("s1.Source should not access skill2 content")
	}
}

func TestSnapshotRejectsEmbeddedParentComponents(t *testing.T) {
	root := t.TempDir()
	for _, file := range []string{"js/../index.mjs", "js/nested/../../index.mjs"} {
		if _, err := Snapshot("probe", root, SourceManifest{Module: file, Files: []string{file}}); err == nil {
			t.Errorf("accepted %s", file)
		}
	}
}
