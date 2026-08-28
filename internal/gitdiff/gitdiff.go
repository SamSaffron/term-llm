// Package gitdiff exposes structured staged and working-tree changes for the
// web Changes panel.
package gitdiff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/filetrack"
)

// Scope selects which Git states are compared.
type Scope string

const (
	ScopeUncommitted Scope = "uncommitted"
	ScopeUnstaged    Scope = "unstaged"
	ScopeStaged      Scope = "staged"
)

const (
	maxPathOutput         = 4 << 20
	maxFileBytes          = filetrack.DefaultMaxFileBytes
	maxChangedFiles       = 1000
	maxUntrackedListBytes = 32 << 20
)

// Repo is a Git checkout used for scoped diff queries.
type Repo struct {
	root string
}

// Open discovers the checkout containing dir.
func Open(ctx context.Context, dir string) (*Repo, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("empty repository directory")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("discover git repository: %w", err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return nil, errors.New("git repository has no worktree root")
	}
	return &Repo{root: filepath.Clean(root)}, nil
}

// List returns per-file metadata for scope.
func (r *Repo) List(ctx context.Context, scope Scope) ([]filetrack.CumulativeChange, error) {
	if scope == ScopeUncommitted {
		if _, err := gitOutput(ctx, 256, "-C", r.root, "rev-parse", "--verify", "HEAD"); err != nil {
			// Preserve support for unborn repositories, where there is no single
			// HEAD-to-worktree diff to query.
			return r.listByContent(ctx, scope)
		}
	}
	args := []string{"-C", r.root, "diff", "--numstat", "-z", "--no-renames"}
	switch scope {
	case ScopeUncommitted:
		args = append(args, "HEAD")
	case ScopeStaged:
		args = append(args, "--cached")
	case ScopeUnstaged:
	default:
		return nil, fmt.Errorf("unsupported git diff scope %q", scope)
	}
	args = append(args, "--")
	out, err := gitOutput(ctx, maxPathOutput, args...)
	if err != nil {
		return nil, err
	}
	statusArgs := append([]string(nil), args...)
	for i := range statusArgs {
		if statusArgs[i] == "--numstat" {
			statusArgs[i] = "--name-status"
			break
		}
	}
	statusOut, err := gitOutput(ctx, maxPathOutput, statusArgs...)
	if err != nil {
		return nil, err
	}
	statuses, err := parseNameStatus(statusOut)
	if err != nil {
		return nil, err
	}
	changes, err := r.parseNumstat(out, statuses)
	if err != nil {
		return nil, err
	}
	if len(changes) > maxChangedFiles {
		return nil, fmt.Errorf("git diff contains more than %d changed files", maxChangedFiles)
	}
	if scope != ScopeStaged {
		untracked, err := gitOutput(ctx, maxPathOutput, "-C", r.root, "ls-files", "--others", "--exclude-standard", "-z", "--")
		if err != nil {
			return nil, err
		}
		untrackedPaths := make([][]byte, 0)
		for _, raw := range bytes.Split(untracked, []byte{0}) {
			if len(raw) > 0 {
				untrackedPaths = append(untrackedPaths, raw)
			}
		}
		if len(changes)+len(untrackedPaths) > maxChangedFiles {
			return nil, fmt.Errorf("git diff contains more than %d changed files", maxChangedFiles)
		}
		remainingBytes := int64(maxUntrackedListBytes)
		for _, raw := range untrackedPaths {
			rel := filepath.ToSlash(string(raw))
			path := filepath.Join(r.root, filepath.FromSlash(rel))
			change := filetrack.CumulativeChange{Path: path, Kind: filetrack.KindCreate, ContentAvailable: true}
			info, statErr := os.Lstat(path)
			if statErr != nil {
				return nil, fmt.Errorf("stat untracked file: %w", statErr)
			}
			if info.Mode().IsRegular() && (info.Size() > int64(maxFileBytes) || info.Size() > remainingBytes) {
				change.Truncated = true
				change.ContentAvailable = false
				changes = append(changes, change)
				continue
			}
			content, _, readErr := r.worktreeFile(rel)
			if readErr != nil {
				return nil, readErr
			}
			remainingBytes -= int64(len(content))
			if len(content) > maxFileBytes || bytes.IndexByte(content, 0) >= 0 {
				change.Truncated = true
				change.ContentAvailable = false
			} else {
				change.Adds, _ = filetrack.CountAddsDels(nil, content)
			}
			changes = append(changes, change)
		}
	}
	if len(changes) > maxChangedFiles {
		return nil, fmt.Errorf("git diff contains more than %d changed files", maxChangedFiles)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

func parseNameStatus(out []byte) (map[string]string, error) {
	fields := bytes.Split(out, []byte{0})
	statuses := make(map[string]string, len(fields)/2)
	for i := 0; i < len(fields); {
		if len(fields[i]) == 0 {
			i++
			continue
		}
		if i+1 >= len(fields) || len(fields[i+1]) == 0 {
			return nil, errors.New("parse git name-status output")
		}
		kind := filetrack.KindModify
		switch fields[i][0] {
		case 'A':
			kind = filetrack.KindCreate
		case 'D':
			kind = filetrack.KindDelete
		}
		statuses[filepath.ToSlash(string(fields[i+1]))] = kind
		i += 2
	}
	return statuses, nil
}

func (r *Repo) parseNumstat(out []byte, statuses map[string]string) ([]filetrack.CumulativeChange, error) {
	changes := make([]filetrack.CumulativeChange, 0)
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := bytes.SplitN(record, []byte{'\t'}, 3)
		if len(parts) != 3 || len(parts[2]) == 0 {
			return nil, errors.New("parse git numstat output")
		}
		rel := filepath.ToSlash(string(parts[2]))
		kind := statuses[rel]
		if kind == "" {
			kind = filetrack.KindModify
		}
		change := filetrack.CumulativeChange{Path: filepath.Join(r.root, filepath.FromSlash(rel)), Kind: kind, ContentAvailable: true}
		if string(parts[0]) == "-" || string(parts[1]) == "-" {
			change.Truncated = true
			change.ContentAvailable = false
		} else {
			if _, err := fmt.Sscan(string(parts[0]), &change.Adds); err != nil {
				return nil, fmt.Errorf("parse git additions: %w", err)
			}
			if _, err := fmt.Sscan(string(parts[1]), &change.Dels); err != nil {
				return nil, fmt.Errorf("parse git deletions: %w", err)
			}
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func (r *Repo) listByContent(ctx context.Context, scope Scope) ([]filetrack.CumulativeChange, error) {
	paths, err := r.paths(ctx, scope)
	if err != nil {
		return nil, err
	}
	if len(paths) > maxChangedFiles {
		return nil, fmt.Errorf("git diff contains more than %d changed files", maxChangedFiles)
	}
	changes := make([]filetrack.CumulativeChange, 0, len(paths))
	for _, path := range paths {
		content, err := r.file(ctx, scope, path)
		if err != nil {
			return nil, err
		}
		if content == nil {
			continue
		}
		change := filetrack.CumulativeChange{Path: content.Path, Kind: content.Kind, Truncated: content.Truncated, ContentAvailable: !content.Truncated}
		if !content.Truncated {
			change.Adds, change.Dels = filetrack.CountAddsDels(content.Before, content.After)
		}
		changes = append(changes, change)
	}
	return changes, nil
}

// File returns retained text for one changed path, or nil when the path is not
// changed in scope. Binary and oversized files are returned as truncated.
func (r *Repo) File(ctx context.Context, scope Scope, path string) (*filetrack.FileDiffContent, error) {
	rel, err := r.relativePath(path)
	if err != nil {
		return nil, err
	}
	paths, err := r.paths(ctx, scope)
	if err != nil {
		return nil, err
	}
	i := sort.SearchStrings(paths, rel)
	if i >= len(paths) || paths[i] != rel {
		return nil, nil
	}
	return r.file(ctx, scope, rel)
}

func (r *Repo) file(ctx context.Context, scope Scope, rel string) (*filetrack.FileDiffContent, error) {
	var before, after []byte
	var beforeMissing, afterMissing bool
	var err error

	switch scope {
	case ScopeUncommitted:
		before, beforeMissing, err = r.gitBlob(ctx, "HEAD:./"+filepath.ToSlash(rel))
		if err == nil {
			after, afterMissing, err = r.worktreeFile(rel)
		}
	case ScopeUnstaged:
		before, beforeMissing, err = r.gitBlob(ctx, ":./"+filepath.ToSlash(rel))
		if err == nil {
			after, afterMissing, err = r.worktreeFile(rel)
		}
	case ScopeStaged:
		before, beforeMissing, err = r.gitBlob(ctx, "HEAD:./"+filepath.ToSlash(rel))
		if err == nil {
			after, afterMissing, err = r.gitBlob(ctx, ":./"+filepath.ToSlash(rel))
		}
	default:
		return nil, fmt.Errorf("unsupported git diff scope %q", scope)
	}
	if err != nil {
		return nil, err
	}
	if beforeMissing && afterMissing {
		return nil, nil
	}
	kind := filetrack.KindModify
	if beforeMissing {
		kind = filetrack.KindCreate
	} else if afterMissing {
		kind = filetrack.KindDelete
	}
	truncated := len(before) > maxFileBytes || len(after) > maxFileBytes || bytes.IndexByte(before, 0) >= 0 || bytes.IndexByte(after, 0) >= 0
	if truncated {
		before, after = nil, nil
	}
	return &filetrack.FileDiffContent{
		Path:      filepath.Join(r.root, filepath.FromSlash(rel)),
		Kind:      kind,
		Before:    before,
		After:     after,
		Truncated: truncated,
	}, nil
}

func (r *Repo) paths(ctx context.Context, scope Scope) ([]string, error) {
	set := make(map[string]struct{})
	addDiff := func(cached bool) error {
		args := []string{"-C", r.root, "diff", "--name-only", "-z", "--no-renames"}
		if cached {
			args = append(args, "--cached")
		}
		args = append(args, "--")
		out, err := gitOutput(ctx, maxPathOutput, args...)
		if err != nil {
			return err
		}
		addNULPaths(set, out)
		return nil
	}
	addUntracked := func() error {
		out, err := gitOutput(ctx, maxPathOutput, "-C", r.root, "ls-files", "--others", "--exclude-standard", "-z", "--")
		if err != nil {
			return err
		}
		addNULPaths(set, out)
		return nil
	}

	switch scope {
	case ScopeUncommitted:
		if err := addDiff(true); err != nil {
			return nil, err
		}
		if err := addDiff(false); err != nil {
			return nil, err
		}
		if err := addUntracked(); err != nil {
			return nil, err
		}
	case ScopeUnstaged:
		if err := addDiff(false); err != nil {
			return nil, err
		}
		if err := addUntracked(); err != nil {
			return nil, err
		}
	case ScopeStaged:
		if err := addDiff(true); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported git diff scope %q", scope)
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		if path != "" {
			paths = append(paths, filepath.ToSlash(path))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func addNULPaths(set map[string]struct{}, data []byte) {
	for _, raw := range bytes.Split(data, []byte{0}) {
		if len(raw) > 0 {
			set[filepath.ToSlash(string(raw))] = struct{}{}
		}
	}
}

func (r *Repo) relativePath(path string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return "", errors.New("empty file path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.root, path)
	}
	rel, err := filepath.Rel(r.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("file path is outside repository")
	}
	return filepath.ToSlash(rel), nil
}

func (r *Repo) worktreeFile(rel string) ([]byte, bool, error) {
	path := filepath.Join(r.root, filepath.FromSlash(rel))
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("stat worktree file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return nil, false, fmt.Errorf("read worktree symlink: %w", err)
		}
		return []byte(target), false, nil
	}
	if !info.Mode().IsRegular() {
		return nil, true, nil
	}
	if info.Size() > maxFileBytes {
		return make([]byte, maxFileBytes+1), false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read worktree file: %w", err)
	}
	return data, false, nil
}

func (r *Repo) gitBlob(ctx context.Context, spec string) ([]byte, bool, error) {
	out, err := gitOutput(ctx, maxFileBytes+1, "-C", r.root, "show", spec)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if errors.Is(err, errGitOutputTruncated) {
			return make([]byte, maxFileBytes+1), false, nil
		}
		var commandErr *gitCommandError
		if errors.As(err, &commandErr) {
			exists, probeErr := r.blobSpecExists(ctx, spec)
			if probeErr == nil && !exists {
				return nil, true, nil
			}
		}
		return nil, false, err
	}
	return out, false, nil
}

func (r *Repo) blobSpecExists(ctx context.Context, spec string) (bool, error) {
	var args []string
	switch {
	case strings.HasPrefix(spec, "HEAD:./"):
		path := strings.TrimPrefix(spec, "HEAD:./")
		if _, err := gitOutput(ctx, 256, "-C", r.root, "rev-parse", "--verify", "HEAD"); err != nil {
			return false, nil
		}
		args = []string{"-C", r.root, "ls-tree", "-z", "--name-only", "HEAD", "--", path}
	case strings.HasPrefix(spec, ":./"):
		path := strings.TrimPrefix(spec, ":./")
		args = []string{"-C", r.root, "ls-files", "--error-unmatch", "--", path}
	default:
		return false, fmt.Errorf("unsupported blob spec %q", spec)
	}
	out, err := gitOutput(ctx, maxPathOutput, args...)
	if err != nil {
		var commandErr *gitCommandError
		if errors.As(err, &commandErr) {
			return false, nil
		}
		return false, err
	}
	return len(bytes.Trim(out, "\x00\r\n")) > 0, nil
}

var errGitOutputTruncated = errors.New("git output exceeded limit")

type gitCommandError struct {
	op     string
	stderr string
	err    error
}

func (e *gitCommandError) Error() string {
	if e.stderr != "" {
		return fmt.Sprintf("git %s: %s: %v", e.op, e.stderr, e.err)
	}
	return fmt.Sprintf("git %s: %v", e.op, e.err)
}

func (e *gitCommandError) Unwrap() error { return e.err }

func gitOperation(args []string) string {
	if len(args) >= 3 && args[0] == "-C" {
		return args[2]
	}
	if len(args) > 0 {
		return args[0]
	}
	return "command"
}

func gitOutput(ctx context.Context, limit int, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	var out bytes.Buffer
	writer := &limitedWriter{w: &out, remaining: limit}
	cmd.Stdout = writer
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, &gitCommandError{op: gitOperation(args), stderr: strings.TrimSpace(stderr.String()), err: err}
	}
	if writer.truncated {
		return out.Bytes(), errGitOutputTruncated
	}
	return out.Bytes(), nil
}

type limitedWriter struct {
	w         *bytes.Buffer
	remaining int
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	original := len(p)
	if len(p) > w.remaining {
		w.truncated = true
	}
	if w.remaining > 0 {
		n := len(p)
		if n > w.remaining {
			n = w.remaining
		}
		_, _ = w.w.Write(p[:n])
		w.remaining -= n
	}
	return original, nil
}
