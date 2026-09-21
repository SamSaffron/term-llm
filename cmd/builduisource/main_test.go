package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/uisource"
)

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectSkipsHubAndTestSources(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "frontend/src/app.tsx", "export const app = 1;\n")
	writeFile(t, root, "frontend/src/styles/app.css", ".app{}\n")
	writeFile(t, root, "frontend/src/domain/markdown.ts", "export const md = 1;\n")
	writeFile(t, root, "frontend/src/domain/markdown.test.ts", "test('x', () => {});\n")
	writeFile(t, root, "frontend/src/hub/dashboard.tsx", "export const hub = 1;\n")
	writeFile(t, root, "frontend/src/test/helpers.ts", "export const helper = 1;\n")
	writeFile(t, root, "frontend/src/app.snap", "not source\n")
	writeFile(t, root, "frontend/README.md", "readme\n")

	files, err := collect(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"frontend/README.md",
		"frontend/src/app.tsx",
		"frontend/src/domain/markdown.ts",
		"frontend/src/styles/app.css",
	}
	if got := sortedKeys(files); len(got) != len(want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
	for _, rel := range want {
		if _, ok := files[rel]; !ok {
			t.Fatalf("missing %s in %v", rel, sortedKeys(files))
		}
	}
}

func TestBuildIsDecodableAndDeterministic(t *testing.T) {
	files := map[string][]byte{
		"frontend/README.md":               []byte("readme\n"),
		"frontend/src/components/App.tsx":  []byte("export const app = 1;\n"),
		"frontend/src/styles/features.css": []byte(".feature{}\n"),
	}
	archive, err := build(files, "v1.2.3", "abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	again, err := build(files, "v1.2.3", "abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	if string(archive) != string(again) {
		t.Fatal("archive bytes changed for identical inputs")
	}

	ref, err := uisource.Decode(archive)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Version != "v1.2.3" || ref.AssetVersion != "abcdef123456" {
		t.Fatalf("manifest = %q/%q", ref.Version, ref.AssetVersion)
	}
	for rel, want := range files {
		if string(ref.Files[rel]) != string(want) {
			t.Fatalf("archive content for %s = %q", rel, ref.Files[rel])
		}
	}

	var decoded manifest
	if err := json.Unmarshal(ref.Files["source-manifest.json"], &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Files) != len(files) {
		t.Fatalf("manifest inventory = %d entries, want %d", len(decoded.Files), len(files))
	}
	entry := decoded.Files["frontend/README.md"]
	if entry.Size != len("readme\n") || len(entry.SHA256) != 64 {
		t.Fatalf("manifest entry = %+v", entry)
	}
	if decoded.SourceDigest == "" {
		t.Fatal("manifest missing source digest")
	}
}

func TestGroupFormatsThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 397674: "397,674", 1234567: "1,234,567"} {
		if got := group(n); got != want {
			t.Fatalf("group(%d) = %q, want %q", n, got, want)
		}
	}
}
