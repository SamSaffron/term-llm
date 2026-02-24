package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/spf13/cobra"
)

func setupMemoryFragmentsTest(t *testing.T) string {
	t.Helper()
	oldAgent := memoryAgent
	oldDBPath := memoryDBPath
	oldAddContent := memoryFragmentsAddContent
	oldAddSource := memoryFragmentsAddSource
	oldUpdateContent := memoryFragmentsUpdateContent

	memoryAgent = "jarvis"
	memoryFragmentsAddContent = ""
	memoryFragmentsAddSource = "manual"
	memoryFragmentsUpdateContent = ""
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	memoryDBPath = dbPath

	t.Cleanup(func() {
		memoryAgent = oldAgent
		memoryDBPath = oldDBPath
		memoryFragmentsAddContent = oldAddContent
		memoryFragmentsAddSource = oldAddSource
		memoryFragmentsUpdateContent = oldUpdateContent
	})

	return dbPath
}

func openTestMemoryStore(t *testing.T, dbPath string) *memorydb.Store {
	t.Helper()
	store, err := memorydb.NewStore(memorydb.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return store
}

func setStdin(t *testing.T, content string) {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	if _, err := w.WriteString(content); err != nil {
		_ = w.Close()
		_ = r.Close()
		t.Fatalf("write stdin: %v", err)
	}
	if err := w.Close(); err != nil {
		_ = r.Close()
		t.Fatalf("close stdin writer: %v", err)
	}
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		_ = r.Close()
	})
}

func firstFragmentRowID(t *testing.T, dbPath string) int64 {
	t.Helper()
	store := openTestMemoryStore(t, dbPath)
	defer store.Close()

	frags, err := store.ListFragments(context.Background(), memorydb.ListOptions{Agent: "jarvis"})
	if err != nil {
		t.Fatalf("ListFragments() error = %v", err)
	}
	if len(frags) == 0 {
		t.Fatal("expected fragment list, got none")
	}
	return frags[0].RowID
}

func TestMemoryFragmentsAddCreatesFragment(t *testing.T) {
	dbPath := setupMemoryFragmentsTest(t)
	memoryFragmentsAddContent = "hello world"

	if err := runMemoryFragmentsAdd(&cobra.Command{}, []string{"fragments/notes/foo.md"}); err != nil {
		t.Fatalf("runMemoryFragmentsAdd() error = %v", err)
	}

	store := openTestMemoryStore(t, dbPath)
	defer store.Close()

	frag, err := store.GetFragment(context.Background(), "jarvis", "fragments/notes/foo.md")
	if err != nil {
		t.Fatalf("GetFragment() error = %v", err)
	}
	if frag == nil {
		t.Fatal("expected fragment, got nil")
	}
	if frag.Content != "hello world" {
		t.Fatalf("fragment content = %q, want %q", frag.Content, "hello world")
	}
}

func TestMemoryFragmentsAddConflict(t *testing.T) {
	dbPath := setupMemoryFragmentsTest(t)
	memoryFragmentsAddContent = "first"

	if err := runMemoryFragmentsAdd(&cobra.Command{}, []string{"fragments/notes/foo.md"}); err != nil {
		t.Fatalf("runMemoryFragmentsAdd() error = %v", err)
	}

	memoryFragmentsAddContent = "second"
	err := runMemoryFragmentsAdd(&cobra.Command{}, []string{"fragments/notes/foo.md"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "fragment already exists: fragments/notes/foo.md — use 'update' instead" {
		t.Fatalf("error = %q", err.Error())
	}

	store := openTestMemoryStore(t, dbPath)
	defer store.Close()

	frag, err := store.GetFragment(context.Background(), "jarvis", "fragments/notes/foo.md")
	if err != nil {
		t.Fatalf("GetFragment() error = %v", err)
	}
	if frag == nil {
		t.Fatal("expected fragment, got nil")
	}
	if frag.Content != "first" {
		t.Fatalf("fragment content = %q, want %q", frag.Content, "first")
	}
}

func TestMemoryFragmentsUpdateModifiesContent(t *testing.T) {
	dbPath := setupMemoryFragmentsTest(t)
	memoryFragmentsAddContent = "original"

	if err := runMemoryFragmentsAdd(&cobra.Command{}, []string{"fragments/notes/foo.md"}); err != nil {
		t.Fatalf("runMemoryFragmentsAdd() error = %v", err)
	}

	memoryFragmentsUpdateContent = "updated"
	if err := runMemoryFragmentsUpdate(&cobra.Command{}, []string{"fragments/notes/foo.md"}); err != nil {
		t.Fatalf("runMemoryFragmentsUpdate() error = %v", err)
	}

	store := openTestMemoryStore(t, dbPath)
	defer store.Close()

	frag, err := store.GetFragment(context.Background(), "jarvis", "fragments/notes/foo.md")
	if err != nil {
		t.Fatalf("GetFragment() error = %v", err)
	}
	if frag == nil {
		t.Fatal("expected fragment, got nil")
	}
	if frag.Content != "updated" {
		t.Fatalf("fragment content = %q, want %q", frag.Content, "updated")
	}
}

func TestMemoryFragmentsUpdateMissing(t *testing.T) {
	setupMemoryFragmentsTest(t)
	memoryFragmentsUpdateContent = "updated"

	err := runMemoryFragmentsUpdate(&cobra.Command{}, []string{"fragments/notes/missing.md"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "fragment not found: fragments/notes/missing.md — use 'add' to create it" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestUpdateFragmentByNumericID(t *testing.T) {
	dbPath := setupMemoryFragmentsTest(t)
	memoryFragmentsAddContent = "original"

	if err := runMemoryFragmentsAdd(&cobra.Command{}, []string{"fragments/notes/foo.md"}); err != nil {
		t.Fatalf("runMemoryFragmentsAdd() error = %v", err)
	}

	rowID := firstFragmentRowID(t, dbPath)

	memoryFragmentsUpdateContent = "updated"
	if err := runMemoryFragmentsUpdate(&cobra.Command{}, []string{fmt.Sprintf("%d", rowID)}); err != nil {
		t.Fatalf("runMemoryFragmentsUpdate() error = %v", err)
	}

	store := openTestMemoryStore(t, dbPath)
	defer store.Close()

	frag, err := store.GetFragment(context.Background(), "jarvis", "fragments/notes/foo.md")
	if err != nil {
		t.Fatalf("GetFragment() error = %v", err)
	}
	if frag == nil {
		t.Fatal("expected fragment, got nil")
	}
	if frag.Content != "updated" {
		t.Fatalf("fragment content = %q, want %q", frag.Content, "updated")
	}
}

func TestUpdateFragmentNotFound(t *testing.T) {
	setupMemoryFragmentsTest(t)
	memoryFragmentsUpdateContent = "updated"

	err := runMemoryFragmentsUpdate(&cobra.Command{}, []string{"9999"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "fragment not found or content unchanged: rowid 9999" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestDeleteFragmentByNumericID(t *testing.T) {
	dbPath := setupMemoryFragmentsTest(t)
	memoryFragmentsAddContent = "original"

	if err := runMemoryFragmentsAdd(&cobra.Command{}, []string{"fragments/notes/foo.md"}); err != nil {
		t.Fatalf("runMemoryFragmentsAdd() error = %v", err)
	}

	rowID := firstFragmentRowID(t, dbPath)

	if err := runMemoryFragmentsDelete(&cobra.Command{}, []string{fmt.Sprintf("%d", rowID)}); err != nil {
		t.Fatalf("runMemoryFragmentsDelete() error = %v", err)
	}

	store := openTestMemoryStore(t, dbPath)
	defer store.Close()

	frag, err := store.GetFragment(context.Background(), "jarvis", "fragments/notes/foo.md")
	if err != nil {
		t.Fatalf("GetFragment() error = %v", err)
	}
	if frag != nil {
		t.Fatalf("expected fragment to be deleted")
	}
}

func TestDeleteFragmentNotFound(t *testing.T) {
	setupMemoryFragmentsTest(t)

	err := runMemoryFragmentsDelete(&cobra.Command{}, []string{"9999"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "fragment not found: rowid 9999" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestMemoryFragmentsDeleteRemovesFragment(t *testing.T) {
	dbPath := setupMemoryFragmentsTest(t)
	memoryFragmentsAddContent = "original"

	if err := runMemoryFragmentsAdd(&cobra.Command{}, []string{"fragments/notes/foo.md"}); err != nil {
		t.Fatalf("runMemoryFragmentsAdd() error = %v", err)
	}

	if err := runMemoryFragmentsDelete(&cobra.Command{}, []string{"fragments/notes/foo.md"}); err != nil {
		t.Fatalf("runMemoryFragmentsDelete() error = %v", err)
	}

	store := openTestMemoryStore(t, dbPath)
	defer store.Close()

	frag, err := store.GetFragment(context.Background(), "jarvis", "fragments/notes/foo.md")
	if err != nil {
		t.Fatalf("GetFragment() error = %v", err)
	}
	if frag != nil {
		t.Fatalf("expected fragment to be deleted")
	}
}

func TestMemoryFragmentsDeleteMissing(t *testing.T) {
	setupMemoryFragmentsTest(t)

	err := runMemoryFragmentsDelete(&cobra.Command{}, []string{"fragments/notes/missing.md"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "fragment not found: fragments/notes/missing.md" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestMemoryFragmentsAddReadsStdin(t *testing.T) {
	dbPath := setupMemoryFragmentsTest(t)
	memoryFragmentsAddContent = ""
	setStdin(t, "stdin add")

	if err := runMemoryFragmentsAdd(&cobra.Command{}, []string{"fragments/notes/stdin.md"}); err != nil {
		t.Fatalf("runMemoryFragmentsAdd() error = %v", err)
	}

	store := openTestMemoryStore(t, dbPath)
	defer store.Close()

	frag, err := store.GetFragment(context.Background(), "jarvis", "fragments/notes/stdin.md")
	if err != nil {
		t.Fatalf("GetFragment() error = %v", err)
	}
	if frag == nil {
		t.Fatal("expected fragment, got nil")
	}
	if frag.Content != "stdin add" {
		t.Fatalf("fragment content = %q, want %q", frag.Content, "stdin add")
	}
}

func TestMemoryFragmentsUpdateReadsStdin(t *testing.T) {
	dbPath := setupMemoryFragmentsTest(t)
	memoryFragmentsAddContent = "original"

	if err := runMemoryFragmentsAdd(&cobra.Command{}, []string{"fragments/notes/stdin-update.md"}); err != nil {
		t.Fatalf("runMemoryFragmentsAdd() error = %v", err)
	}

	memoryFragmentsUpdateContent = ""
	setStdin(t, "stdin update")
	if err := runMemoryFragmentsUpdate(&cobra.Command{}, []string{"fragments/notes/stdin-update.md"}); err != nil {
		t.Fatalf("runMemoryFragmentsUpdate() error = %v", err)
	}

	store := openTestMemoryStore(t, dbPath)
	defer store.Close()

	frag, err := store.GetFragment(context.Background(), "jarvis", "fragments/notes/stdin-update.md")
	if err != nil {
		t.Fatalf("GetFragment() error = %v", err)
	}
	if frag == nil {
		t.Fatal("expected fragment, got nil")
	}
	if frag.Content != "stdin update" {
		t.Fatalf("fragment content = %q, want %q", frag.Content, "stdin update")
	}
}
