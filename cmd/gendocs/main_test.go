package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/cmd"
	"github.com/samsaffron/term-llm/internal/docsgen"
)

func TestReferenceDocsUpToDate(t *testing.T) {
	dir := filepath.Join("..", "..", "docs-site", "content", "reference")
	if err := docsgen.Sync(dir, docsgen.Pages(cmd.ReferenceCommand()), true); err != nil {
		t.Fatal(err)
	}
}

func TestSyncReportsDriftWithoutWriting(t *testing.T) {
	page := docsgen.Page{Path: "page.md", Section: "example", Content: "current"}
	dir := t.TempDir()
	path := filepath.Join(dir, page.Path)
	stale := "before\n<!-- BEGIN GENERATED: example -->\nstale\n<!-- END GENERATED: example -->\nafter\n"
	if err := os.WriteFile(path, []byte(stale), 0600); err != nil {
		t.Fatal(err)
	}
	if err := docsgen.Sync(dir, []docsgen.Page{page}, true); err == nil || !strings.Contains(err.Error(), "run make docs") {
		t.Fatalf("expected actionable drift error, got %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != stale {
		t.Fatalf("check modified source: %q, %v", contents, err)
	}
	if err := docsgen.Sync(dir, []docsgen.Page{page}, false); err != nil {
		t.Fatal(err)
	}
	if err := docsgen.Sync(dir, []docsgen.Page{page}, true); err != nil {
		t.Fatalf("regenerated docs still drift: %v", err)
	}
}

func TestRenderRequiresOrderedMarkers(t *testing.T) {
	page := docsgen.Page{Path: "test.md", Section: "example", Content: "new\n"}
	for _, input := range []string{"missing", "<!-- END GENERATED: example -->\n<!-- BEGIN GENERATED: example -->"} {
		if _, err := docsgen.Render([]byte(input), page); err == nil {
			t.Fatalf("expected marker error for %q", input)
		}
	}
	original := "prose\n<!-- BEGIN GENERATED: example -->\nold\n<!-- END GENERATED: example -->\nmore prose\n"
	got, err := docsgen.Render([]byte(original), page)
	if err != nil || !strings.Contains(string(got), "prose\n<!-- BEGIN GENERATED: example -->\nnew\n<!-- END GENERATED: example -->\nmore prose") {
		t.Fatalf("Render = %s, %v", got, err)
	}
}
