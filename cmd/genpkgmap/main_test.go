package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageMapUpToDate(t *testing.T) {
	if err := syncMap(filepath.Join("..", ".."), true); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncMapWritesAndChecks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/m\n")
	writeFile(t, filepath.Join(root, "main.go"), "// Command m runs the example.\npackage main\n")
	writeFile(t, filepath.Join(root, "internal/a/a.go"), "// Package a does A things. More detail.\npackage a\n")
	writeFile(t, filepath.Join(root, "internal/a/a_test.go"), "package a\n")
	writeFile(t, filepath.Join(root, "internal/onlytests/x_test.go"), "package onlytests\n")
	writeFile(t, filepath.Join(root, "nested/go.mod"), "module example.com/nested\n")
	writeFile(t, filepath.Join(root, "nested/n.go"), "package n\n")
	writeFile(t, filepath.Join(root, "frontend/node_modules/x/x.go"), "package x\n")
	writeFile(t, filepath.Join(root, "internal/a/testdata/t.go"), "package t\n")
	writeFile(t, filepath.Join(root, "internal/a/_skip.go"), "// Package wrong is skipped.\npackage wrong\n")
	writeFile(t, filepath.Join(root, "internal/a/gen.go"), "//go:build ignore\n\n// Command gen is a standalone generator.\npackage main\n")
	writeFile(t, filepath.Join(root, "internal/onlygen/gen.go"), "//go:build ignore\n\npackage main\n")
	writeFile(t, filepath.Join(root, "internal/linux/l.go"), "//go:build linux\n\n// Package linux is platform specific.\npackage linux\n")
	stale := "# Map\n" + beginMarker + "\nstale\n" + endMarker + "\nafter\n"
	writeFile(t, filepath.Join(root, mapFile), stale)

	if err := syncMap(root, true); err == nil || !strings.Contains(err.Error(), "run make pkgmap") {
		t.Fatalf("expected drift error, got %v", err)
	}
	if err := syncMap(root, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, mapFile))
	if err != nil {
		t.Fatal(err)
	}
	want := "# Map\n" + beginMarker + "\n- `.` — runs the example.\n- `internal/a` — does A things.\n- `internal/linux` — is platform specific.\n" + endMarker + "\nafter\n"
	if string(got) != want {
		t.Fatalf("PACKAGES.md =\n%s\nwant\n%s", got, want)
	}
	if err := syncMap(root, true); err != nil {
		t.Fatalf("regenerated map still drifts: %v", err)
	}
}

func TestRenderRejectsUndocumentedPackages(t *testing.T) {
	_, err := render([]pkgDoc{{dir: "internal/a", name: "a", doc: "Package a is fine.\n"}, {dir: "internal/b", name: "b"}})
	if err == nil || !strings.Contains(err.Error(), "internal/b: add internal/b/doc.go with a `// Package b ...` comment") {
		t.Fatalf("expected actionable missing-doc error, got %v", err)
	}
}

func TestReplaceSectionRequiresOrderedMarkers(t *testing.T) {
	for _, input := range []string{"missing", endMarker + "\n" + beginMarker} {
		if _, err := replaceSection(input, "x"); err == nil {
			t.Fatalf("expected marker error for %q", input)
		}
	}
}
