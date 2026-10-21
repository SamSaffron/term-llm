//go:build unix

package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/grepsearch"
)

func TestIntentGoExpansionSkipsFIFOWithoutWriter(t *testing.T) {
	requireGoFallback(t)
	root := scoredFiles(t, .9)
	fifo := filepath.Join(root, "older.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	// Keep the regular match first, exactly like the reported max_results=1 repro.
	older := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(fifo, older, older); err != nil {
		t.Fatal(err)
	}
	// A broken implementation must not leave its blocking reader behind after
	// the assertion fails. This is non-blocking and opens no writer during the test.
	defer func() {
		if fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = syscall.Close(fd)
		}
	}()
	backend := newScoringFake()
	tool, factory := newIntentTool(t, backend, func(c *config.Config) { c.Tools.GrepRerank.TimeoutMS = 50 })
	fifoReturned := make(chan error, 1)
	tool.intentScan = func(ctx context.Context, path string, re *regexp.Regexp, max int, visit func([]string, int) error) error {
		err := grepsearch.Scan(ctx, path, re, max, visit)
		if path == fifo {
			fifoReturned <- err
		}
		return err
	}
	ordinary := execGrep(t, tool, root, map[string]any{"max_results": 1})
	started := time.Now()
	got := execGrep(t, tool, root, map[string]any{"intent": "writes", "max_results": 1})
	if elapsed := time.Since(started); elapsed > reviewedBudget+reviewedMargin {
		t.Fatalf("FIFO delayed intent grep: %v", elapsed)
	}
	select {
	case err := <-fifoReturned:
		if err == nil {
			t.Fatal("FIFO was scanned instead of skipped")
		}
	case <-time.After(reviewedMargin):
		t.Fatal("intent worker is still blocked opening FIFO without a writer")
	}
	if got != ordinary || strings.Contains(got, "intent rerank skipped") {
		t.Fatalf("wanted ordinary output without spending budget\n got %q\nwant %q", got, ordinary)
	}
	if factory.opens.Load() != 0 || backend.calls() != 0 {
		t.Fatal("single regular candidate called classifier")
	}
}
