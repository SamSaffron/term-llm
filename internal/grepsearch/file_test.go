package grepsearch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestScanCancellationStopsExpansionWithinFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matches.txt")
	if err := os.WriteFile(path, []byte("needle\nneedle\nneedle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := Scan(ctx, path, regexp.MustCompile("needle"), 150, func(lines []string, index int) error {
		calls++
		if index != 0 || len(lines) != 3 {
			t.Fatalf("unexpected source lines/index %d %d", len(lines), index)
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestScanPlainContextAndCapUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matches.txt")
	if err := os.WriteFile(path, []byte("before\nneedle\nafter\nneedle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := Scan(nil, path, regexp.MustCompile("needle"), 1, func(lines []string, index int) error {
		calls++
		if index != 1 || len(lines) != 4 || lines[0] != "before" || lines[2] != "after" {
			t.Fatalf("source=%v index=%d", lines, index)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("visited %d matches", calls)
	}
}
