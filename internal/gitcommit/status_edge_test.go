package gitcommit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStagedDeletionIsSupportedAndIntentToAddIsBlocked(t *testing.T) {
	r := repoTest(t)
	if err := os.Remove(filepath.Join(r.root, "base.txt")); err != nil {
		t.Fatal(err)
	}
	gitTest(t, r.root, "add", "-A")
	state, err := r.Inspect(context.Background())
	if err != nil {
		t.Fatalf("staged deletion rejected: %v", err)
	}
	if len(state.Staged) != 1 || state.Staged[0].Kind != ChangeDeleted {
		t.Fatalf("staged deletion state = %+v", state.Staged)
	}

	gitTest(t, r.root, "reset", "--hard", "-q", "HEAD")
	writeTest(t, r.root, "intent.txt", "intent\n")
	gitTest(t, r.root, "add", "-N", "intent.txt")
	if _, err := r.Inspect(context.Background()); !IsKind(err, ErrIntentToAdd) {
		t.Fatalf("intent-to-add error = %v", err)
	}
}
