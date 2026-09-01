package gitcommit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func FingerprintsEqual(a, b Fingerprint) bool {
	return fingerprintsEqual(a, b)
}

func fingerprintsEqual(a, b Fingerprint) bool {
	return a.CheckoutID == b.CheckoutID && a.HeadState == b.HeadState && a.HeadOID == b.HeadOID && a.IndexTree == b.IndexTree && a.Operation.Kind == b.Operation.Kind && a.Operation.Digest == b.Operation.Digest && stringSlicesEqual(a.Operation.HeadOIDs, b.Operation.HeadOIDs)
}
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (r *Repository) Stage(ctx context.Context, request StageRequest, expected Fingerprint) (RepositoryState, error) {
	release, err := r.acquire(ctx)
	if err != nil {
		return RepositoryState{}, err
	}
	defer release()
	before, err := r.inspectUnlocked(ctx)
	if err != nil {
		return before, err
	}
	if !fingerprintsEqual(before.Fingerprint, expected) {
		return before, &Error{Kind: ErrStale, Message: "the checkout, HEAD, operation state, or index changed; refresh before staging"}
	}
	switch request.Mode {
	case StageAll:
		if request.StatusToken == "" || request.StatusToken != before.StatusToken {
			return before, &Error{Kind: ErrStale, Message: "files changed before all changes could be staged"}
		}
		if _, err = r.git(ctx, nil, "add", "-A"); err != nil {
			return r.refreshAfterError(ctx, classifyGitError("stage all changes", err))
		}
	case StageExactSelection:
		if !before.SelectionAvailable || before.StatusToken == "" {
			return before, &Error{Kind: ErrInvalidSelection, Message: "exact file selection is unavailable for this repository state"}
		}
		if request.StatusToken == "" || request.StatusToken != before.StatusToken {
			return before, &Error{Kind: ErrStale, Message: "files changed while the selection was being reviewed"}
		}
		paths, validationErr := validateSelection(before, request.Paths)
		if validationErr != nil {
			return before, validationErr
		}
		if err = r.stageExact(ctx, before, paths); err != nil {
			return r.refreshAfterError(ctx, err)
		}
	default:
		return before, &Error{Kind: ErrInvalidSelection, Message: fmt.Sprintf("unsupported staging mode %q", request.Mode)}
	}
	after, err := r.inspectUnlocked(ctx)
	if err != nil {
		return after, err
	}
	if len(after.Staged) == 0 {
		return after, &Error{Kind: ErrEmptyIndex, Message: "there are no staged changes to commit"}
	}
	return after, nil
}

func (r *Repository) refreshAfterError(ctx context.Context, operationErr error) (RepositoryState, error) {
	state, inspectErr := r.inspectUnlocked(context.WithoutCancel(ctx))
	if inspectErr != nil {
		return state, errors.Join(operationErr, inspectErr)
	}
	return state, operationErr
}

func validateSelection(state RepositoryState, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return nil, &Error{Kind: ErrInvalidSelection, Message: "choose at least one changed file"}
	}
	allowed := map[string]Change{}
	for _, list := range [][]Change{state.Staged, state.Unstaged, state.Untracked} {
		for _, ch := range list {
			allowed[ch.Path] = ch
		}
	}
	seen := map[string]struct{}{}
	expanded := map[string]struct{}{}
	for _, p := range requested {
		if !safePath(p) {
			return nil, &Error{Kind: ErrInvalidSelection, Message: fmt.Sprintf("invalid selected path %q", p)}
		}
		if _, dup := seen[p]; dup {
			return nil, &Error{Kind: ErrInvalidSelection, Message: fmt.Sprintf("duplicate selected path %q", p)}
		}
		seen[p] = struct{}{}
		ch, ok := allowed[p]
		if !ok {
			return nil, &Error{Kind: ErrInvalidSelection, Message: fmt.Sprintf("selected path %q is no longer changed", p)}
		}
		expanded[p] = struct{}{}
		if ch.OldPath != "" {
			expanded[ch.OldPath] = struct{}{}
		}
	}
	paths := make([]string, 0, len(expanded))
	for p := range expanded {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

func (r *Repository) stageExact(ctx context.Context, before RepositoryState, paths []string) error {
	tempDir, err := os.MkdirTemp("", "term-llm-index-*")
	if err != nil {
		return fmt.Errorf("create temporary index directory: %w", err)
	}
	if chmodErr := os.Chmod(tempDir, 0700); chmodErr != nil {
		_ = os.RemoveAll(tempDir)
		return fmt.Errorf("secure temporary index directory: %w", chmodErr)
	}
	defer os.RemoveAll(tempDir)
	temp := filepath.Join(tempDir, "index")
	env := map[string]string{"GIT_INDEX_FILE": temp}
	if before.Unborn {
		_, err = r.git(ctx, env, "read-tree", "--empty")
	} else {
		_, err = r.git(ctx, env, "read-tree", before.HeadOID)
	}
	if err != nil {
		return classifyGitError("initialize exact selection", err)
	}
	args := []string{"add", "-A", "--"}
	args = append(args, paths...)
	if _, err = r.git(ctx, env, args...); err != nil {
		return classifyGitError("stage selected files", err)
	}
	treeOut, err := r.git(ctx, env, "write-tree")
	if err != nil {
		return classifyGitError("build selected index", err)
	}
	tree := strings.TrimSpace(string(treeOut))
	// Revalidate immediately before the live-index Git command. read-tree itself
	// uses index.lock, so an external Git process either wins first (and this
	// revalidation catches it) or receives Git's normal lock failure.
	current, err := r.inspectUnlocked(ctx)
	if err != nil {
		return err
	}
	if !fingerprintsEqual(current.Fingerprint, before.Fingerprint) || current.StatusToken != before.StatusToken {
		return &Error{Kind: ErrStale, Message: "files or the index changed while staging the selection"}
	}
	if _, err = r.git(ctx, nil, "read-tree", tree); err != nil {
		return classifyGitError("apply selected index", err)
	}
	return nil
}

func (r *Repository) Commit(ctx context.Context, message string, expected Fingerprint) (CommitResult, error) {
	if strings.TrimSpace(message) == "" || strings.TrimSpace(strings.SplitN(message, "\n", 2)[0]) == "" {
		return CommitResult{}, &Error{Kind: ErrCommit, Message: "a non-empty commit subject is required"}
	}
	if len(message) > 128<<10 {
		return CommitResult{}, &Error{Kind: ErrOutputLimit, Message: "commit message exceeds the 128 KiB limit"}
	}
	release, err := r.acquire(ctx)
	if err != nil {
		return CommitResult{}, err
	}
	defer release()
	before, err := r.inspectUnlocked(ctx)
	if err != nil {
		return CommitResult{}, err
	}
	if !fingerprintsEqual(before.Fingerprint, expected) {
		return CommitResult{}, &Error{Kind: ErrStale, Message: "the reviewed checkout, HEAD, operation state, or staged content changed"}
	}
	if len(before.Staged) == 0 {
		return CommitResult{}, &Error{Kind: ErrEmptyIndex, Message: "there are no staged changes to commit"}
	}
	path, err := tempMessage(message)
	if err != nil {
		return CommitResult{}, fmt.Errorf("write temporary commit message: %w", err)
	}
	defer removeFile(path)
	stdout, stderr, runErr := r.runCommit(ctx, path)
	verifyCtx := context.Background()
	result, verifyErr := r.verifyCommit(verifyCtx, before.Fingerprint, stdout, stderr)
	if verifyErr == nil {
		return result, nil
	}
	if runErr != nil {
		// A failed process with unchanged HEAD is a classified, recoverable failure.
		if IsKind(verifyErr, ErrUncertain) {
			return result, verifyErr
		}
		runErr = &commandError{args: []string{"commit"}, stdout: stdout, stderr: stderr, err: runErr}
		return result, classifyGitError("create commit", runErr)
	}
	return result, verifyErr
}

func (r *Repository) verifyCommit(ctx context.Context, expected Fingerprint, stdout, stderr string) (CommitResult, error) {
	result := CommitResult{BeforeHead: expected.HeadOID, Stdout: bounded(stdout), Stderr: bounded(stderr)}
	headOut, err := r.git(ctx, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		if expected.HeadState == HeadUnborn {
			return result, &Error{Kind: ErrCommit, Message: "Git did not create a commit", Cause: err}
		}
		return result, &Error{Kind: ErrUncertain, Message: "could not determine whether Git created the commit", Cause: err}
	}
	head := strings.TrimSpace(string(headOut))
	if head == expected.HeadOID {
		return result, &Error{Kind: ErrCommit, Message: "Git did not create a commit"}
	}
	lineOut, err := r.git(ctx, nil, "rev-list", "--parents", "-n", "1", head)
	if err != nil {
		return result, &Error{Kind: ErrUncertain, Message: "the commit outcome could not be verified", Cause: err}
	}
	parts := strings.Fields(string(lineOut))
	validParent := (expected.HeadState == HeadUnborn && len(parts) == 1) || (expected.HeadState == HeadBorn && len(parts) == 2 && parts[1] == expected.HeadOID)
	if !validParent {
		result.OutcomeUncertain = true
		return result, &Error{Kind: ErrUncertain, Message: "HEAD moved, but not to the reviewed ordinary commit"}
	}
	treeOut, err := r.git(ctx, nil, "show", "-s", "--format=%T", head)
	if err != nil {
		return result, &Error{Kind: ErrUncertain, Message: "read resulting commit tree", Cause: err}
	}
	messageOut, err := r.git(ctx, nil, "show", "-s", "--format=%B", head)
	if err != nil {
		return result, &Error{Kind: ErrUncertain, Message: "read resulting commit message", Cause: err}
	}
	shortOut, _ := r.git(ctx, nil, "rev-parse", "--short", head)
	actualMessage := strings.TrimRight(string(messageOut), "\r\n")
	subject := actualMessage
	if i := strings.IndexByte(subject, '\n'); i >= 0 {
		subject = subject[:i]
	}
	result.HeadOID = head
	result.TreeOID = strings.TrimSpace(string(treeOut))
	result.ShortOID = strings.TrimSpace(string(shortOut))
	result.Subject = subject
	result.Message = actualMessage
	result.TreeChanged = result.TreeOID != expected.IndexTree
	return result, nil
}

func bounded(s string) string {
	if len(s) > maxCommitOutput {
		return s[:maxCommitOutput]
	}
	return s
}
