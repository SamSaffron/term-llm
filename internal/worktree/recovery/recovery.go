// Package recovery contains the user-facing worktree promotion recovery flow
// shared by interactive clients.
package recovery

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/worktree"
)

// Kind identifies an interactive worktree recovery flow.
type Kind string

const (
	KindConflict    Kind = "conflict"
	KindDirtyRoot   Kind = "dirty-root"
	KindRemoveInUse Kind = "remove-in-use"

	// UnavailableCallerReason explains the session-binding safety requirement.
	UnavailableCallerReason = "Assisted recovery must be started from a conversation using this worktree or its root checkout."
)

// Offer is the transport-independent confirmation shown before recovery.
type Offer struct {
	Kind              Kind     `json:"kind"`
	Title             string   `json:"title"`
	Question          string   `json:"question"`
	YesLabel          string   `json:"yes_label"`
	NoLabel           string   `json:"no_label"`
	Details           string   `json:"details,omitempty"`
	Conflicts         []string `json:"conflicts,omitempty"`
	Available         bool     `json:"available"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
	DeclineMessage    string   `json:"decline_message,omitempty"`
}

// OfferForMerge builds the same recovery confirmation for every client.
func OfferForMerge(kind Kind, res worktree.MergeResult, inUseCount int) Offer {
	offer := Offer{
		Kind:           kind,
		Title:          "Assisted Worktree Recovery",
		YesLabel:       "Yes — start assisted recovery",
		NoLabel:        "No — leave everything unchanged",
		Details:        offerDetails(kind, res),
		Available:      true,
		DeclineMessage: DeclinedMessage(kind, res),
	}
	switch kind {
	case KindConflict:
		offer.Question = "This worktree does not promote cleanly onto the current root branch. Would you like me to resolve it directly in the root checkout, leaving the result staged and uncommitted?"
		offer.Conflicts = append([]string(nil), res.Conflicts...)
	case KindDirtyRoot:
		offer.Question = "The root checkout is dirty. Would you like me to inspect the dirty root/worktree state and help sort it out before retrying?"
	case KindRemoveInUse:
		offer.Title = "Remove Promoted Worktree?"
		offer.Question = fmt.Sprintf("The promotion succeeded, but this worktree is used by %d other session(s). Remove it anyway?", inUseCount)
		offer.YesLabel = "Yes — remove it anyway"
		offer.NoLabel = "No — keep the worktree"
	default:
		offer.Question = "Would you like assisted worktree promotion recovery?"
	}
	return offer
}

func offerDetails(kind Kind, res worktree.MergeResult) string {
	var details strings.Builder
	if res.WorktreeDir != "" {
		fmt.Fprintf(&details, "Source: %s\n", res.WorktreeDir)
	}
	if res.RootDir != "" {
		fmt.Fprintf(&details, "Root: %s\n", res.RootDir)
	}
	if kind == KindConflict && len(res.Conflicts) > 0 {
		details.WriteString("Conflicts: ")
		limit := min(8, len(res.Conflicts))
		details.WriteString(strings.Join(res.Conflicts[:limit], ", "))
		if len(res.Conflicts) > limit {
			fmt.Fprintf(&details, " (+%d more)", len(res.Conflicts)-limit)
		}
		details.WriteByte('\n')
	}
	if kind == KindDirtyRoot && strings.TrimSpace(res.RootStatus) != "" {
		details.WriteString("Root status:\n")
		details.WriteString(strings.Join(statusLines(res.RootStatus, 8), "\n"))
		details.WriteByte('\n')
	}
	return strings.TrimSpace(details.String())
}

func statusLines(status string, limit int) []string {
	lines := strings.Split(strings.TrimSpace(status), "\n")
	if len(lines) > limit {
		remaining := len(lines) - limit
		lines = append(lines[:limit], fmt.Sprintf("… and %d more", remaining))
	}
	return lines
}

// DeclinedMessage describes the safe state after the user declines recovery.
func DeclinedMessage(kind Kind, res worktree.MergeResult) string {
	switch kind {
	case KindDirtyRoot:
		return "Okay — leaving the root checkout unchanged. Clean/commit/stash root changes, then retry `/worktree promote` when ready."
	case KindRemoveInUse:
		return MergeKeptMessage(res)
	default:
		return "Okay — leaving the root checkout clean. You can retry the promotion, promote to a branch instead, or ask for help later."
	}
}

// StartingMessage is shown while the source snapshot is applied to root.
func StartingMessage(res worktree.MergeResult) string {
	return fmt.Sprintf("Okay — switching to the root checkout and applying %s there. I will ask the LLM to resolve conflicts without committing or pushing.", displayName(res.WorktreeName))
}

// AssistedMergePrompt is the LLM prompt used after the snapshot is applied.
func AssistedMergePrompt(res worktree.AssistedMergeResult) string {
	var b strings.Builder
	b.WriteString("The user confirmed interactive recovery for a failed `/worktree promote`. The worktree snapshot has been applied directly to the current root checkout branch.\n\n")
	b.WriteString("Goal:\n")
	b.WriteString("- Resolve/apply all source worktree changes on the current root checkout branch.\n")
	b.WriteString("- Leave the complete result staged and uncommitted in the root checkout.\n")
	b.WriteString("- Do not commit, push, switch branches, discard user changes, or remove the original worktree.\n\n")
	b.WriteString("State:\n")
	fmt.Fprintf(&b, "- Root checkout: %s\n", res.RootDir)
	fmt.Fprintf(&b, "- Current root branch: %s\n", res.PreviousRootBranch)
	fmt.Fprintf(&b, "- Source worktree: %s (%s)\n", displayName(res.WorktreeName), res.WorktreeDir)
	fmt.Fprintf(&b, "- Worktree base SHA: %s\n", res.Base)
	fmt.Fprintf(&b, "- Root HEAD before recovery: %s\n", res.RootHead)
	fmt.Fprintf(&b, "- Worktree HEAD: %s\n", res.WorktreeHead)
	fmt.Fprintf(&b, "- Snapshot commit being applied: %s\n", res.SnapshotCommit)
	if len(res.Conflicts) > 0 {
		fmt.Fprintf(&b, "- Conflict files: %s\n", strings.Join(res.Conflicts, ", "))
	}
	if len(res.ChangedFiles) > 0 {
		fmt.Fprintf(&b, "- Changed files: %s\n", strings.Join(res.ChangedFiles, "; "))
	}
	if strings.TrimSpace(res.RootStatus) != "" {
		fmt.Fprintf(&b, "- Current root status:\n%s\n", res.RootStatus)
	}
	b.WriteString("Instructions:\n")
	b.WriteString("0. Use available shell/read/edit tools as needed; operate in the root checkout unless inspecting the source worktree.\n")
	b.WriteString("1. Start with `git status --short` in the root checkout and inspect conflicted files if any.\n")
	b.WriteString("2. Resolve conflict markers or apply equivalent edits that preserve all source worktree intent on top of the current root branch.\n")
	b.WriteString("3. Stage resolved files with `git add` as appropriate.\n")
	b.WriteString("4. If a cherry-pick state remains after staging, run `git cherry-pick --quit` (not `--continue`) so the result stays uncommitted.\n")
	b.WriteString("5. Compare the final staged diff with the source worktree and ensure no source changes were omitted.\n")
	b.WriteString("6. Finish by running `git status --short` and summarizing what changed plus next commands: `git status`, `git commit -m \"...\"`, and `git push`.\n")
	return b.String()
}

// DirtyRootPrompt is the LLM prompt used when dirty root state blocked promotion.
func DirtyRootPrompt(res worktree.MergeResult) string {
	var b strings.Builder
	b.WriteString("The user confirmed interactive recovery for a `/worktree promote` that was blocked because the root checkout is dirty. You have permission to inspect and help sort this out safely.\n\n")
	b.WriteString("Goal:\n")
	b.WriteString("- Determine why the root checkout is dirty and recommend or perform safe steps to preserve those changes before retrying the worktree promotion.\n")
	b.WriteString("- Do not discard, overwrite, commit, push, or stash changes unless you clearly explain the action first and it is safe. If uncertain, ask the user.\n\n")
	fmt.Fprintf(&b, "Source worktree: %s (%s)\n", displayName(res.WorktreeName), res.WorktreeDir)
	fmt.Fprintf(&b, "Destination root: %s\n", res.RootDir)
	fmt.Fprintf(&b, "Base SHA: %s\n", res.Base)
	fmt.Fprintf(&b, "Root HEAD: %s\n", res.RootHead)
	fmt.Fprintf(&b, "Worktree HEAD: %s\n", res.WorktreeHead)
	if strings.TrimSpace(res.RootStatus) != "" {
		fmt.Fprintf(&b, "Root status that blocked promotion:\n%s\n", res.RootStatus)
	}
	b.WriteString("\nSuggested first commands:\n")
	b.WriteString("- Use available shell/read/edit tools as needed; operate in the root checkout unless inspecting the source worktree.\n")
	b.WriteString("- `git status --short` in the root checkout\n")
	b.WriteString("- inspect relevant diffs before deciding whether to commit, stash, or ask the user\n")
	b.WriteString("- once root is clean, retry `/worktree promote` or use `/worktree promote --branch` if safer\n")
	return b.String()
}

// AssistedMergeRootDirtyMessage explains a recovery preflight failure.
func AssistedMergeRootDirtyMessage(res worktree.AssistedMergeResult) string {
	var b strings.Builder
	b.WriteString("Assisted recovery could not start because the root checkout became dirty.\n\n")
	fmt.Fprintf(&b, "Root checkout: %s\n", res.RootDir)
	fmt.Fprintf(&b, "Source worktree: %s (%s)\n", displayName(res.WorktreeName), res.WorktreeDir)
	appendStatusSection(&b, "Root status", res.RootStatus, 30)
	b.WriteString("\nThe root checkout was not changed. Clean, commit, or stash root changes, then retry the promotion.\n")
	return b.String()
}

// AssistedMergeNothingToApplyMessage explains a no-op assisted recovery.
func AssistedMergeNothingToApplyMessage(res worktree.AssistedMergeResult) string {
	var b strings.Builder
	b.WriteString("Assisted recovery did not need to start: there are no worktree changes to apply.\n\n")
	fmt.Fprintf(&b, "Root checkout: %s\n", res.RootDir)
	fmt.Fprintf(&b, "Source worktree: %s (%s)\n", displayName(res.WorktreeName), res.WorktreeDir)
	if res.SnapshotCommit != "" {
		fmt.Fprintf(&b, "Snapshot checked: %s\n", shortSHA(res.SnapshotCommit))
	}
	b.WriteString("The root checkout was not changed.\n")
	return b.String()
}

// MergeKeptMessage describes a successful promotion whose source was retained.
func MergeKeptMessage(res worktree.MergeResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Promoted worktree %s → root checkout\n\n", displayName(res.WorktreeName))
	fmt.Fprintf(&b, "Source: %s (%s)\n", displayName(res.WorktreeName), res.WorktreeDir)
	fmt.Fprintf(&b, "Destination: %s\n", res.RootDir)
	if res.SnapshotCommit != "" {
		fmt.Fprintf(&b, "Snapshot: %s\n", shortSHA(res.SnapshotCommit))
	}
	if res.Committed {
		b.WriteString("Result: changes were committed on the root checkout.\n")
	} else if res.Applied {
		b.WriteString("Result: changes are staged and uncommitted on the root checkout.\n")
	} else {
		b.WriteString("Result: no worktree changes needed to be applied.\n")
	}
	b.WriteString("Cleanup: kept the source worktree because removal was declined.\n")
	b.WriteString("Current session: still bound to the source worktree. `/shell` still opens the worktree until you run `/worktree root`.\n")
	appendLinesSection(&b, "Changed files", res.ChangedFiles, 20)
	b.WriteString("\nNext:\n")
	b.WriteString("  /worktree root\n")
	b.WriteString("  /shell\n")
	b.WriteString("  git status\n")
	if !res.Committed && res.Applied {
		b.WriteString("  git commit -m \"...\"\n")
	}
	name := res.WorktreeDir
	if strings.TrimSpace(res.WorktreeName) != "" {
		name = res.WorktreeName
	}
	b.WriteString("  /worktree rm " + name + " --force   # when you are done\n")
	return b.String()
}

func displayName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "worktree"
	}
	return name
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func appendLinesSection(b *strings.Builder, title string, lines []string, maxLines int) {
	if len(lines) == 0 {
		return
	}
	if maxLines <= 0 || maxLines > len(lines) {
		maxLines = len(lines)
	}
	fmt.Fprintf(b, "\n%s:\n", title)
	for _, line := range lines[:maxLines] {
		fmt.Fprintf(b, "- %s\n", line)
	}
	if len(lines) > maxLines {
		fmt.Fprintf(b, "- … and %d more\n", len(lines)-maxLines)
	}
}

func appendStatusSection(b *strings.Builder, title, status string, maxLines int) {
	var lines []string
	for _, line := range strings.Split(status, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		fmt.Fprintf(b, "\n%s: clean\n", title)
		return
	}
	appendLinesSection(b, title, lines, maxLines)
}
