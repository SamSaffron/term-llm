package recovery

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/worktree"
)

func TestConflictOfferAndPromptAreComplete(t *testing.T) {
	merge := worktree.MergeResult{
		WorktreeName: "feature",
		WorktreeDir:  "/tmp/feature",
		RootDir:      "/repo",
		Conflicts:    []string{"file.txt"},
	}
	offer := OfferForMerge(KindConflict, merge, 0)
	if offer.Kind != KindConflict || !offer.Available || offer.Title == "" || offer.Question == "" || offer.YesLabel == "" || offer.NoLabel == "" {
		t.Fatalf("offer = %+v", offer)
	}
	for _, want := range []string{"/tmp/feature", "/repo", "file.txt"} {
		if !strings.Contains(offer.Details, want) {
			t.Fatalf("offer details missing %q: %s", want, offer.Details)
		}
	}

	prompt := AssistedMergePrompt(worktree.AssistedMergeResult{
		WorktreeName:       "feature",
		WorktreeDir:        "/tmp/feature",
		RootDir:            "/repo",
		PreviousRootBranch: "main",
		Base:               "base",
		RootHead:           "root",
		WorktreeHead:       "head",
		SnapshotCommit:     "snapshot",
		Conflicts:          []string{"file.txt"},
		ChangedFiles:       []string{"M file.txt"},
	})
	for _, want := range []string{"failed `/worktree promote`", "/repo", "/tmp/feature", "file.txt", "staged and uncommitted", "Do not commit"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestOfferLabelsCoverEveryTUIRecoveryKind(t *testing.T) {
	dirty := OfferForMerge(KindDirtyRoot, worktree.MergeResult{RootStatus: " M root.txt"}, 0)
	if !strings.Contains(dirty.Question, "root checkout is dirty") || !strings.Contains(dirty.Details, "M root.txt") {
		t.Fatalf("dirty-root offer = %+v", dirty)
	}
	remove := OfferForMerge(KindRemoveInUse, worktree.MergeResult{}, 2)
	if remove.Title != "Remove Promoted Worktree?" || remove.YesLabel != "Yes — remove it anyway" || !strings.Contains(remove.Question, "2 other session(s)") {
		t.Fatalf("remove-in-use offer = %+v", remove)
	}
}

func TestNothingToApplyUsesExistingTwelveCharacterSHA(t *testing.T) {
	message := AssistedMergeNothingToApplyMessage(worktree.AssistedMergeResult{SnapshotCommit: "1234567890abcdef"})
	if !strings.Contains(message, "1234567890ab") {
		t.Fatalf("message = %q", message)
	}
}
