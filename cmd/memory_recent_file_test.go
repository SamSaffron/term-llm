package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
)

func TestWithLockedRecentFile_SerializesConcurrentAccess(t *testing.T) {
	recentPath := filepath.Join(t.TempDir(), "memory", "recent.md")

	firstAcquired := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondAcquired := make(chan struct{})
	errCh := make(chan error, 2)

	go func() {
		errCh <- withLockedRecentFile(recentPath, func() error {
			close(firstAcquired)
			<-releaseFirst
			return nil
		})
	}()

	select {
	case <-firstAcquired:
	case <-time.After(2 * time.Second):
		t.Fatal("first lock was not acquired")
	}

	go func() {
		errCh <- withLockedRecentFile(recentPath, func() error {
			close(secondAcquired)
			return nil
		})
	}()

	select {
	case <-secondAcquired:
		t.Fatal("second lock acquired before first lock released")
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseFirst)

	select {
	case <-secondAcquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second lock was not acquired after first lock released")
	}

	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("withLockedRecentFile: %v", err)
		}
	}
}

func TestMemoryPromoteRechecksCutoffAfterWaitingForRecentLock(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	agentDir := filepath.Join(tmp, "agent")
	if err := os.MkdirAll(filepath.Join(agentDir, "memory"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte("name: promote-test\ndescription: test\n"), 0o644); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}
	recentPath := filepath.Join(agentDir, "memory", "recent.md")
	if err := os.WriteFile(recentPath, []byte("old recent"), 0o644); err != nil {
		t.Fatalf("write recent.md: %v", err)
	}

	store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(tmp, "memory.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	oldCutoff := time.Now().Add(-2 * time.Hour).UTC()
	fragTime := time.Now().Add(-1 * time.Hour).UTC()
	if err := store.SetMeta(ctx, memoryPromoteMetaKey(agentDir), oldCutoff.Format(time.RFC3339)); err != nil {
		t.Fatalf("SetMeta old cutoff: %v", err)
	}
	if err := store.CreateFragment(ctx, &memorydb.Fragment{
		Agent:     agentDir,
		Path:      "fragments/new.md",
		Content:   "new fact",
		CreatedAt: fragTime,
		UpdatedAt: fragTime,
	}); err != nil {
		t.Fatalf("CreateFragment: %v", err)
	}

	firstAcquired := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstErr := make(chan error, 1)
	go func() {
		firstErr <- withLockedRecentFile(recentPath, func() error {
			close(firstAcquired)
			<-releaseFirst
			return nil
		})
	}()
	select {
	case <-firstAcquired:
	case <-time.After(2 * time.Second):
		t.Fatal("first lock was not acquired")
	}

	provider := llm.NewMockProvider("memory-promote-race")
	engine := llm.NewEngine(provider, nil)
	runDone := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		count, err := runMemoryPromoteFlow(ctx, &config.Config{}, engine, store, memoryPromoteOptions{
			Agent:        agentDir,
			QuietNothing: true,
		})
		runDone <- struct {
			count int
			err   error
		}{count: count, err: err}
	}()

	select {
	case res := <-runDone:
		t.Fatalf("promote flow completed before lock was released: count=%d err=%v", res.count, res.err)
	case <-time.After(150 * time.Millisecond):
	}

	advancedCutoff := time.Now().UTC()
	if err := store.SetMeta(ctx, memoryPromoteMetaKey(agentDir), advancedCutoff.Format(time.RFC3339)); err != nil {
		t.Fatalf("SetMeta advanced cutoff: %v", err)
	}
	close(releaseFirst)

	if err := <-firstErr; err != nil {
		t.Fatalf("first withLockedRecentFile: %v", err)
	}
	select {
	case res := <-runDone:
		if res.err != nil {
			t.Fatalf("runMemoryPromoteFlow: %v", res.err)
		}
		if res.count != 0 {
			t.Fatalf("promoted count = %d, want 0 after cutoff advanced while waiting for lock", res.count)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("promote flow did not complete after lock release")
	}
	if provider.CurrentTurn() != 0 {
		t.Fatalf("provider was called despite no fragments after rechecked cutoff")
	}
}

func TestWriteRecentFileAtomically_ReplacesContentWithoutLeakingTemps(t *testing.T) {
	dir := t.TempDir()
	recentPath := filepath.Join(dir, "recent.md")
	if err := os.WriteFile(recentPath, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed recent.md: %v", err)
	}

	if err := writeRecentFileAtomically(recentPath, "new content"); err != nil {
		t.Fatalf("writeRecentFileAtomically: %v", err)
	}

	data, err := os.ReadFile(recentPath)
	if err != nil {
		t.Fatalf("read recent.md: %v", err)
	}
	if string(data) != "new content" {
		t.Fatalf("got %q, want %q", string(data), "new content")
	}

	info, err := os.Stat(recentPath)
	if err != nil {
		t.Fatalf("stat recent.md: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode = %o, want 644", got)
	}

	leftovers, err := filepath.Glob(filepath.Join(dir, ".recent.md.*.tmp"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("unexpected temp files left behind: %v", leftovers)
	}
}
