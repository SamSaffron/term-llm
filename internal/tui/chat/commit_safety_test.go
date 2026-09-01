package chat

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/gitcommit"
)

func TestCommitStagingCannotBeDismissedAndReviewCursorIsClamped(t *testing.T) {
	m, dir := commitTUIModel(t)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	repo, err := gitcommit.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := repo.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.commit = &CommitState{Phase: CommitStaging, Repo: repo, Status: state, Selected: map[string]bool{}}
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(*Model)
	if m.commit == nil || m.commit.Phase != CommitStaging {
		t.Fatal("Esc orphaned in-flight staging")
	}
	m.commit.Phase = CommitReviewing
	m.commit.Cursor = 99
	updated, _ = m.handleCommitKey(tea.KeyPressMsg{Code: ' ', Text: " "})
	m = updated.(*Model)
	if m.commit.Cursor != 0 {
		t.Fatalf("cursor was not clamped safely: cursor=%d", m.commit.Cursor)
	}
}

func TestCommitStaleFailureRequiresReviewAndEscCannotOrphanCommit(t *testing.T) {
	m, dir := commitTUIModel(t)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	repo, err := gitcommit.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := repo.Inspect(context.Background())
	staged, err := repo.Stage(context.Background(), gitcommit.StageRequest{Mode: gitcommit.StageAll, StatusToken: before.StatusToken}, before.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	editor := textarea.New()
	editor.SetValue("Preserved message")
	m.commit = &CommitState{Phase: CommitCommitting, Repo: repo, Status: staged, Message: editor, Generated: "Generated", Dirty: true}
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(*Model)
	if m.commit == nil || m.commit.Phase != CommitCommitting {
		t.Fatal("Esc orphaned an in-flight commit")
	}
	original := m.commit.Status.Fingerprint
	updated, _ = m.Update(commitDoneMsg{err: &gitcommit.Error{Kind: gitcommit.ErrStale, Message: "stale"}})
	m = updated.(*Model)
	if m.commit.Phase != CommitError || !m.commit.NeedsReview || !reflect.DeepEqual(m.commit.Status.Fingerprint, original) {
		t.Fatalf("stale failure was re-armed without review: %+v", m.commit)
	}
}
