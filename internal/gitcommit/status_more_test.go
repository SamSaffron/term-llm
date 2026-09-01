package gitcommit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPorcelainRenameAndPartialStaging(t *testing.T) {
	r := repoTest(t)
	newPath := "renamed file.txt"
	if err := os.Rename(filepath.Join(r.root, "base.txt"), filepath.Join(r.root, newPath)); err != nil {
		t.Fatal(err)
	}
	gitTest(t, r.root, "add", "-A")
	state, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Staged) != 1 || state.Staged[0].Kind != ChangeRenamed || state.Staged[0].OldPath != "base.txt" || state.Staged[0].Path != newPath {
		t.Fatalf("rename state = %+v", state.Staged)
	}
	writeTest(t, r.root, newPath, "staged\n")
	gitTest(t, r.root, "add", "--", newPath)
	writeTest(t, r.root, newPath, "working\n")
	state, err = r.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	partialStaged := false
	for _, change := range state.Staged {
		if change.Path == newPath && change.PartiallyStaged {
			partialStaged = true
		}
	}
	if !partialStaged || len(state.Unstaged) != 1 || !state.Unstaged[0].PartiallyStaged {
		t.Fatalf("partial state staged=%+v unstaged=%+v", state.Staged, state.Unstaged)
	}
}

func TestLinkedWorktreeIdentityAndOperationState(t *testing.T) {
	main := repoTest(t)
	linked := filepath.Join(t.TempDir(), "linked")
	gitTest(t, main.root, "worktree", "add", "-q", "-b", "linked-test", linked)
	linkedRepo, err := Open(context.Background(), linked)
	if err != nil {
		t.Fatal(err)
	}
	if linkedRepo.CheckoutRoot() == main.CheckoutRoot() || linkedRepo.CheckoutID() == main.CheckoutID() {
		t.Fatalf("linked checkout identity collapsed to main: main=%s linked=%s", main.CheckoutRoot(), linkedRepo.CheckoutRoot())
	}
	writeTest(t, linkedRepo.gitDir, "MERGE_HEAD", main.checkoutID+"\n")
	if _, err := linkedRepo.Inspect(context.Background()); !IsKind(err, ErrUnsupportedOperation) {
		t.Fatalf("linked worktree operation error = %v", err)
	}
}
