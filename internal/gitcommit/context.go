package gitcommit

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ScopeContext returns bounded, read-only context for whole-file scope planning.
// The caller must still revalidate the status token before applying a proposal.
func (r *Repository) ScopeContext(ctx context.Context, expected Fingerprint, statusToken string) (string, error) {
	state, err := r.Inspect(ctx)
	if err != nil {
		return "", err
	}
	if !fingerprintsEqual(state.Fingerprint, expected) || statusToken == "" || state.StatusToken != statusToken {
		return "", &Error{Kind: ErrStale, Message: "repository changes are stale for scope planning"}
	}
	status, err := r.git(ctx, nil, "status", "--short", "--untracked-files=all")
	if err != nil {
		return "", classifyGitError("read status", err)
	}
	cached, err := r.git(ctx, nil, "diff", "--cached", "--no-ext-diff", "--stat", "--patch", "--find-renames")
	if err != nil {
		return "", classifyGitError("read staged diff", err)
	}
	working, err := r.git(ctx, nil, "diff", "--no-ext-diff", "--stat", "--patch", "--find-renames")
	if err != nil {
		return "", classifyGitError("read working diff", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "STATUS\n%s\nSTAGED DIFF\n%s\nUNSTAGED DIFF\n%s", status, cached, working)
	if len(state.Untracked) > 0 {
		b.WriteString("\nUNTRACKED FILE CONTENT (bounded)\n")
		remaining := 1 << 20
		for _, change := range state.Untracked {
			if remaining <= 0 {
				b.WriteString("[untracked content limit reached]\n")
				break
			}
			full := filepath.Join(r.root, filepath.FromSlash(change.Path))
			file, openErr := os.Open(full)
			if openErr != nil {
				fmt.Fprintf(&b, "--- %s (unavailable: %v)\n", change.Path, openErr)
				continue
			}
			data, readErr := io.ReadAll(io.LimitReader(file, int64(remaining)+1))
			_ = file.Close()
			if readErr != nil {
				fmt.Fprintf(&b, "--- %s (unavailable: %v)\n", change.Path, readErr)
				continue
			}
			if len(data) > remaining {
				data = data[:remaining]
			}
			fmt.Fprintf(&b, "--- %s\n%s\n", change.Path, data)
			remaining -= len(data)
		}
	}
	return b.String(), nil
}

// DraftContext returns only finalized staged changes and recent subjects.
func (r *Repository) DraftContext(ctx context.Context, expected Fingerprint) (string, error) {
	state, err := r.Inspect(ctx)
	if err != nil {
		return "", err
	}
	if !fingerprintsEqual(state.Fingerprint, expected) {
		return "", &Error{Kind: ErrStale, Message: "staged changes are stale for message drafting"}
	}
	if len(state.Staged) == 0 {
		return "", &Error{Kind: ErrEmptyIndex, Message: "there are no staged changes to describe"}
	}
	status, err := r.git(ctx, nil, "status", "--short")
	if err != nil {
		return "", err
	}
	diff, err := r.git(ctx, nil, "diff", "--cached", "--no-ext-diff", "--stat", "--patch", "--find-renames")
	if err != nil {
		return "", err
	}
	logOut, _ := r.git(ctx, nil, "log", "-8", "--pretty=format:%s")
	return fmt.Sprintf("STAGED STATUS\n%s\nSTAGED DIFF (the only commit content)\n%s\nRECENT SUBJECTS (style context only)\n%s", status, diff, logOut), nil
}

// ValidateSelectionPaths validates model/user paths without mutating the index.
func ValidateSelectionPaths(state RepositoryState, paths []string) error {
	_, err := validateSelection(state, paths)
	return err
}
