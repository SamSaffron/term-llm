package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/session"
)

type cleanupTestStore struct {
	session.NoopStore
	summaries []session.SessionSummary
}

func (s *cleanupTestStore) List(_ context.Context, opts session.ListOptions) ([]session.SessionSummary, error) {
	var summaries []session.SessionSummary
	for _, summary := range s.summaries {
		if opts.Status != "" && summary.Status != opts.Status {
			continue
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

func TestMergeBackAndCleanup(t *testing.T) {
	tests := []struct {
		name       string
		store      func(string) session.Store
		excludeID  string
		wantRemove bool
		wantInUse  int
	}{
		{name: "nil store", wantRemove: true},
		{
			name: "other session keeps worktree",
			store: func(dir string) session.Store {
				return &cleanupTestStore{summaries: []session.SessionSummary{{ID: "other", Number: 7, Name: "other session", WorktreeDir: dir, Status: session.StatusActive}}}
			},
			wantInUse: 1,
		},
		{
			name: "completed session does not keep worktree",
			store: func(dir string) session.Store {
				return &cleanupTestStore{summaries: []session.SessionSummary{{ID: "completed", WorktreeDir: dir, Status: session.StatusComplete}}}
			},
			wantRemove: true,
		},
		{
			name: "current session is excluded",
			store: func(dir string) session.Store {
				return &cleanupTestStore{summaries: []session.SessionSummary{{ID: "current", WorktreeDir: dir, Status: session.StatusActive}}}
			},
			excludeID:  "current",
			wantRemove: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newGitRepoForWorktreeTest(t)
			wt, err := Create(context.Background(), repo, CreateOptions{Name: "cleanup-" + strings.ReplaceAll(tt.name, " ", "-")})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			dir := wt.Dir
			t.Cleanup(func() { _ = Remove(context.Background(), dir, RemoveOptions{Force: true}) })
			if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("cleanup test\n"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			var store session.Store
			if tt.store != nil {
				store = tt.store(dir)
			}
			res, cleanup, err := MergeBackAndCleanup(context.Background(), dir, MergeOptions{}, store, tt.excludeID)
			if err != nil {
				t.Fatalf("MergeBackAndCleanup: %v", err)
			}
			if !res.Applied {
				t.Fatalf("merge result = %+v, want applied", res)
			}
			if cleanup.Removed != tt.wantRemove || len(cleanup.InUse) != tt.wantInUse {
				t.Fatalf("cleanup = %+v, want removed=%v in-use=%d", cleanup, tt.wantRemove, tt.wantInUse)
			}
			_, statErr := os.Stat(dir)
			if tt.wantRemove && !os.IsNotExist(statErr) {
				t.Fatalf("worktree stat error = %v, want removed", statErr)
			}
			if !tt.wantRemove && statErr != nil {
				t.Fatalf("worktree should remain: %v", statErr)
			}
			status := runGitForWorktreeTest(t, repo, "status", "--porcelain")
			if !strings.Contains(status, "A  new.txt") {
				t.Fatalf("root status = %q, want staged new file", status)
			}
		})
	}
}

func TestMergeBackAndCleanupNoChangesRemovesWorktree(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "cleanup-no-changes"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res, cleanup, err := MergeBackAndCleanup(context.Background(), wt.Dir, MergeOptions{}, nil, "")
	if err != nil {
		t.Fatalf("MergeBackAndCleanup: %v", err)
	}
	if res.Applied || !cleanup.Removed {
		t.Fatalf("result=%+v cleanup=%+v, want no-op merge with removal", res, cleanup)
	}
	if _, err := os.Stat(wt.Dir); !os.IsNotExist(err) {
		t.Fatalf("worktree stat = %v, want removed", err)
	}
}

func TestMergeBackAndCleanupConflictKeepsWorktree(t *testing.T) {
	repo := newGitRepoForWorktreeTest(t)
	wt, err := Create(context.Background(), repo, CreateOptions{Name: "cleanup-conflict"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir := wt.Dir
	t.Cleanup(func() { _ = Remove(context.Background(), dir, RemoveOptions{Force: true}) })

	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("root changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitForWorktreeTest(t, repo, "add", "file.txt")
	runGitForWorktreeTest(t, repo, "commit", "-m", "root change")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("worktree changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, cleanup, err := MergeBackAndCleanup(context.Background(), dir, MergeOptions{}, nil, "")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if cleanup.Removed {
		t.Fatalf("cleanup = %+v, want preserved worktree", cleanup)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("worktree should remain after conflict: %v", err)
	}
}
