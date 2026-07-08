// Package worktree manages git worktrees for term-llm sessions.
package worktree

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/samsaffron/term-llm/internal/appdata"
	"github.com/samsaffron/term-llm/internal/session"
)

// Status describes where a worktree is in its lifecycle.
type Status string

const (
	StatusCreating Status = "creating"
	StatusReady    Status = "ready"
	StatusFailed   Status = "failed"
)

// Worktree describes a managed git worktree.
type Worktree struct {
	Name        string    `json:"name"`
	Dir         string    `json:"dir"`
	RepoRoot    string    `json:"repo_root,omitempty"`
	Branch      string    `json:"branch,omitempty"`
	Base        string    `json:"base"` // full base SHA
	Detached    bool      `json:"detached"`
	Status      Status    `json:"status"`
	DirtyFiles  int       `json:"dirty_files"`
	HeadSHA     string    `json:"head_sha"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	LastBoundAt time.Time `json:"last_bound_at,omitempty"`
	Orphaned    bool      `json:"orphaned,omitempty"`
}

// Progress is emitted by long-running worktree operations.
type Progress struct {
	Message string `json:"message"`
}

// CreateOptions configures Create.
type CreateOptions struct {
	Name         string
	Base         string
	Branch       string
	SetupScript  string
	SetupTimeout time.Duration
	CopyFiles    []string
	ProgressFn   func(message string)
	Progress     chan<- Progress
}

// RemoveOptions configures Remove.
type RemoveOptions struct {
	Force        bool
	DeleteBranch bool
}

// MergeOptions configures MergeBack.
type MergeOptions struct {
	AllowDirty bool
	Commit     bool
	Message    string
}

// MergeResult describes the result of MergeBack.
type MergeResult struct {
	Applied        bool     `json:"applied"`
	Committed      bool     `json:"committed"`
	SnapshotCommit string   `json:"snapshot_commit,omitempty"`
	Conflicts      []string `json:"conflicts,omitempty"`
	Message        string   `json:"message,omitempty"`
}

// InUseSession describes a session bound to a worktree.
type InUseSession struct {
	ID     string `json:"id"`
	Number int64  `json:"number,omitempty"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
}

// metadata is persisted outside the worktree so the checkout stays clean.
type metadata struct {
	Name        string    `json:"name"`
	Dir         string    `json:"dir"`
	Base        string    `json:"base"`
	Branch      string    `json:"branch,omitempty"`
	RepoRoot    string    `json:"repo_root"`
	CreatedAt   time.Time `json:"created_at"`
	LastBoundAt time.Time `json:"last_bound_at,omitempty"`
}

var (
	ErrDirty    = errors.New("worktree has uncommitted changes")
	ErrExists   = errors.New("worktree already exists")
	ErrConflict = errors.New("merge back has conflicts")
)

func (o *CreateOptions) progress(msg string) {
	if o == nil {
		return
	}
	if o.ProgressFn != nil {
		o.ProgressFn(msg)
	}
	if o.Progress != nil {
		select {
		case o.Progress <- Progress{Message: msg}:
		default:
		}
	}
}

// IsGitRepo reports whether dir is inside a git working tree.
func IsGitRepo(dir string) bool {
	out, err := runGit(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// MainRepoRoot returns the canonical main checkout root for any worktree in the repo.
func MainRepoRoot(dir string) (string, error) { return canonicalRepoRoot(dir) }

// ManagedRoot returns the bucket for managed worktrees for repoRoot.
func ManagedRoot(repoRoot string) (string, error) {
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		return "", err
	}
	dataDir = canonicalPathWithExistingAncestor(dataDir)
	canonical, err := canonicalRepoRoot(repoRoot)
	if err != nil {
		return "", err
	}
	base := slug(filepath.Base(canonical))
	if base == "" {
		base = "repo"
	}
	return filepath.Join(dataDir, "worktrees", fmt.Sprintf("%s-%s", base, repoHash8(canonical))), nil
}

// ManagedRootBase returns the global worktree data directory.
func ManagedRootBase() (string, error) {
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(canonicalPathWithExistingAncestor(dataDir), "worktrees"), nil
}

// Create creates a managed worktree. Detached HEAD is used unless Branch is set.
func Create(ctx context.Context, repoRoot string, opts CreateOptions) (*Worktree, error) {
	if !IsGitRepo(repoRoot) {
		return nil, fmt.Errorf("worktree: %q is not a git repository", repoRoot)
	}
	mainRoot, err := canonicalRepoRoot(repoRoot)
	if err != nil {
		return nil, err
	}
	root, err := ManagedRoot(mainRoot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(metaDir(root), 0o755); err != nil {
		return nil, fmt.Errorf("worktree: create metadata dir: %w", err)
	}

	baseRef := strings.TrimSpace(opts.Base)
	if baseRef == "" {
		baseRef = "HEAD"
	}
	baseSHA, err := revParseFull(mainRoot, baseRef)
	if err != nil {
		return nil, fmt.Errorf("worktree: resolve base %q: %w", baseRef, err)
	}

	name, dir, err := resolveName(root, opts.Name)
	if err != nil {
		return nil, err
	}
	branch := strings.TrimSpace(opts.Branch)

	opts.progress(fmt.Sprintf("Creating worktree %s…", name))
	args := []string{"worktree", "add"}
	if branch != "" {
		args = append(args, "-b", branch)
	} else {
		args = append(args, "--detach")
	}
	args = append(args, dir, baseSHA)
	if out, err := runGitCtx(ctx, mainRoot, args...); err != nil {
		return nil, fmt.Errorf("worktree: git worktree add: %w: %s", err, strings.TrimSpace(out))
	}

	cleanup := func() {
		_, _ = runGit(mainRoot, "worktree", "remove", "--force", dir)
		_, _ = runGit(mainRoot, "worktree", "prune")
		_ = os.RemoveAll(dir)
		_ = os.Remove(metaPath(root, name))
	}

	m := metadata{Name: name, Dir: dir, Base: baseSHA, Branch: branch, RepoRoot: mainRoot, CreatedAt: time.Now()}
	if err := writeMetadata(root, m); err != nil {
		cleanup()
		return nil, err
	}

	if len(opts.CopyFiles) > 0 {
		opts.progress("Copying configured files…")
		if err := copyFiles(mainRoot, dir, opts.CopyFiles); err != nil {
			cleanup()
			return nil, err
		}
	}

	if _, err := os.Stat(filepath.Join(mainRoot, ".gitmodules")); err == nil {
		opts.progress("Initializing submodules…")
		if out, err := runGitCtx(ctx, dir, "submodule", "update", "--init", "--recursive"); err != nil {
			cleanup()
			return nil, fmt.Errorf("worktree: submodule update: %w: %s", err, strings.TrimSpace(out))
		}
	}

	if script := strings.TrimSpace(opts.SetupScript); script != "" {
		timeout := opts.SetupTimeout
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		setupCtx, cancel := context.WithTimeout(ctx, timeout)
		opts.progress("Running setup script…")
		out, err := runScript(setupCtx, dir, script)
		cancel()
		if strings.TrimSpace(out) != "" {
			opts.progress(lastLines(out, 5))
		}
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("worktree: setup script failed: %w\n%s", err, strings.TrimSpace(out))
		}
	}

	wt, err := Get(dir)
	if err != nil {
		cleanup()
		return nil, err
	}
	wt.Name = name
	wt.Base = baseSHA
	wt.Branch = branch
	wt.CreatedAt = m.CreatedAt
	wt.Status = StatusReady
	opts.progress("Ready")
	return wt, nil
}

// List returns managed worktrees for the repository, excluding the main checkout.
func List(repoRoot string) ([]Worktree, error) {
	if !IsGitRepo(repoRoot) {
		return nil, fmt.Errorf("worktree: %q is not a git repository", repoRoot)
	}
	mainRoot, err := canonicalRepoRoot(repoRoot)
	if err != nil {
		return nil, err
	}
	root, err := ManagedRoot(mainRoot)
	if err != nil {
		return nil, err
	}
	return listForRoot(mainRoot, root)
}

// ListAll scans all managed buckets using metadata. Orphaned buckets are marked.
func ListAll() ([]Worktree, error) {
	base, err := ManagedRootBase()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var all []Worktree
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		bucket := filepath.Join(base, entry.Name())
		metas := readAllMetadata(bucket)
		if len(metas) == 0 {
			continue
		}
		repoRoot := metas[0].RepoRoot
		if IsGitRepo(repoRoot) {
			items, err := listForRoot(repoRoot, bucket)
			if err == nil {
				all = append(all, items...)
				continue
			}
		}
		for _, m := range metas {
			all = append(all, Worktree{Name: m.Name, Dir: m.Dir, RepoRoot: m.RepoRoot, Base: m.Base, Branch: m.Branch, CreatedAt: m.CreatedAt, LastBoundAt: m.LastBoundAt, Orphaned: true, Status: StatusFailed})
		}
	}
	sortWorktrees(all)
	return all, nil
}

func listForRoot(mainRoot, bucket string) ([]Worktree, error) {
	out, err := runGit(mainRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("worktree: list: %w", err)
	}
	metasByDir := metadataByDir(bucket)
	seen := map[string]bool{}
	var result []Worktree
	for _, rec := range parsePorcelainWorktrees(out) {
		dir := rec["worktree"]
		if dir == "" {
			continue
		}
		if samePath(dir, mainRoot) {
			continue
		}
		if !strings.HasPrefix(filepath.Clean(dir), filepath.Clean(bucket)+string(filepath.Separator)) {
			continue
		}
		wt, err := describeWorktree(dir)
		if err != nil {
			continue
		}
		if m, ok := metasByDir[filepath.Clean(dir)]; ok {
			wt.Name = m.Name
			wt.Base = m.Base
			wt.Branch = firstNonEmpty(wt.Branch, m.Branch)
			wt.RepoRoot = m.RepoRoot
			wt.CreatedAt = m.CreatedAt
			wt.LastBoundAt = m.LastBoundAt
		} else {
			wt.Name = slug(filepath.Base(dir))
			if wt.Name == "" {
				wt.Name = filepath.Base(dir)
			}
			m := metadata{Name: wt.Name, Dir: dir, Base: wt.Base, Branch: wt.Branch, RepoRoot: mainRoot, CreatedAt: time.Now()}
			_ = writeMetadata(bucket, m)
		}
		seen[filepath.Clean(dir)] = true
		result = append(result, *wt)
	}
	// Drop stale metadata whose directory is no longer in git's worktree list.
	for dir, m := range metasByDir {
		if !seen[dir] {
			_ = os.Remove(metaPath(bucket, m.Name))
		}
	}
	_, _ = runGit(mainRoot, "worktree", "prune")
	sortWorktrees(result)
	return result, nil
}

// Get describes a worktree by directory.
func Get(dir string) (*Worktree, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if !IsGitRepo(abs) {
		return nil, fmt.Errorf("worktree: %q is not a git repository", dir)
	}
	wt, err := describeWorktree(abs)
	if err != nil {
		return nil, err
	}
	mainRoot, _ := canonicalRepoRoot(abs)
	if root, err := ManagedRoot(mainRoot); err == nil {
		if m, ok := metadataByDir(root)[filepath.Clean(abs)]; ok {
			wt.Name = m.Name
			wt.Base = m.Base
			wt.Branch = firstNonEmpty(wt.Branch, m.Branch)
			wt.RepoRoot = m.RepoRoot
			wt.CreatedAt = m.CreatedAt
			wt.LastBoundAt = m.LastBoundAt
		}
	}
	return wt, nil
}

// TouchLastBound records that a session bound this worktree now.
func TouchLastBound(dir string) error {
	wt, err := Get(dir)
	if err != nil {
		return err
	}
	root, err := ManagedRoot(wt.RepoRoot)
	if err != nil {
		return err
	}
	m := metadata{Name: wt.Name, Dir: wt.Dir, Base: wt.Base, Branch: wt.Branch, RepoRoot: wt.RepoRoot, CreatedAt: wt.CreatedAt, LastBoundAt: time.Now()}
	return writeMetadata(root, m)
}

// Diff returns a unified diff from the recorded base and includes untracked files.
func Diff(dir string) (string, error) {
	wt, err := Get(dir)
	if err != nil {
		return "", err
	}
	base := strings.TrimSpace(wt.Base)
	if base == "" {
		base, _ = mergeBase(dir, "HEAD")
	}
	if base == "" {
		base = "HEAD"
	}
	var b strings.Builder
	out, err := runGit(dir, "diff", "--binary", base, "--")
	if err != nil {
		// git diff returns 0 for differences; any error is real but include output.
		if strings.TrimSpace(out) != "" {
			b.WriteString(out)
		}
		return b.String(), err
	}
	b.WriteString(out)
	untracked, err := runGit(dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return b.String(), nil
	}
	for _, rel := range strings.Split(untracked, "\x00") {
		if rel == "" {
			continue
		}
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
		noIndexOut, _ := runGitAllowExit(dir, []string{"diff", "--no-index", "--", os.DevNull, rel}, map[int]bool{0: true, 1: true})
		noIndexOut = strings.ReplaceAll(noIndexOut, "--- "+os.DevNull, "--- /dev/null")
		b.WriteString(noIndexOut)
		if !strings.HasSuffix(noIndexOut, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

// Promote creates a branch at the worktree HEAD and checks it out.
func Promote(ctx context.Context, dir, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Errorf("branch is required")
	}
	if out, err := runGitCtx(ctx, dir, "branch", branch, "HEAD"); err != nil {
		return fmt.Errorf("worktree: create branch: %w: %s", err, strings.TrimSpace(out))
	}
	if out, err := runGitCtx(ctx, dir, "checkout", branch); err != nil {
		return fmt.Errorf("worktree: checkout branch: %w: %s", err, strings.TrimSpace(out))
	}
	_ = TouchLastBound(dir)
	return nil
}

// Remove removes a managed worktree and prunes git's worktree metadata.
func Remove(ctx context.Context, dir string, opts RemoveOptions) error {
	wt, err := Get(dir)
	if err != nil {
		return err
	}
	if wt.DirtyFiles > 0 && !opts.Force {
		return ErrDirty
	}
	root := wt.RepoRoot
	args := []string{"worktree", "remove"}
	if opts.Force {
		args = append(args, "--force")
	}
	args = append(args, wt.Dir)
	if out, err := runGitCtx(ctx, root, args...); err != nil {
		return fmt.Errorf("worktree: remove: %w: %s", err, strings.TrimSpace(out))
	}
	if opts.DeleteBranch && wt.Branch != "" {
		_, _ = runGit(root, "branch", "-D", wt.Branch)
	}
	_, _ = runGit(root, "worktree", "prune")
	if bucket, err := ManagedRoot(root); err == nil {
		_ = os.Remove(metaPath(bucket, wt.Name))
	}
	_ = os.RemoveAll(wt.Dir)
	return nil
}

// MergeBack snapshots the worktree and cherry-picks it into the root checkout staged and uncommitted by default.
func MergeBack(ctx context.Context, dir string, opts MergeOptions) (MergeResult, error) {
	wt, err := Get(dir)
	if err != nil {
		return MergeResult{}, err
	}
	root := wt.RepoRoot
	if !opts.AllowDirty {
		if dirtyCount(root) > 0 {
			return MergeResult{}, fmt.Errorf("root checkout has uncommitted changes")
		}
	}
	base := strings.TrimSpace(wt.Base)
	if base == "" {
		base = "HEAD"
	}
	msg := strings.TrimSpace(opts.Message)
	if msg == "" {
		msg = fmt.Sprintf("Merge term-llm worktree %s", wt.Name)
	}
	snapshot, err := snapshotCommit(ctx, wt.Dir, base, msg)
	if err != nil {
		return MergeResult{}, err
	}
	out, err := runGitCtx(ctx, root, "cherry-pick", "-n", snapshot)
	if err != nil {
		conflicts := conflictFiles(root)
		_, _ = runGit(root, "reset", "--merge")
		res := MergeResult{Applied: false, SnapshotCommit: snapshot, Conflicts: conflicts, Message: strings.TrimSpace(out)}
		return res, ErrConflict
	}
	res := MergeResult{Applied: true, SnapshotCommit: snapshot}
	if opts.Commit {
		if out, err := runGitCtx(ctx, root, "commit", "-m", msg); err != nil {
			return res, fmt.Errorf("worktree: commit merged changes: %w: %s", err, strings.TrimSpace(out))
		}
		res.Committed = true
	}
	return res, nil
}

// InUse returns non-archived sessions currently bound to dir when the store exposes worktree summaries.
func InUse(ctx context.Context, store session.Store, dir string) ([]InUseSession, error) {
	if store == nil {
		return nil, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	list, err := store.List(ctx, session.ListOptions{Archived: false, Limit: 10000})
	if err != nil {
		return nil, err
	}
	var out []InUseSession
	for _, s := range list {
		if s.WorktreeDir == "" {
			continue
		}
		if samePath(s.WorktreeDir, abs) {
			out = append(out, InUseSession{ID: s.ID, Number: s.Number, Name: s.Name, Status: string(s.Status)})
		}
	}
	return out, nil
}

func describeWorktree(dir string) (*Worktree, error) {
	abs, _ := filepath.Abs(dir)
	root, err := canonicalRepoRoot(abs)
	if err != nil {
		return nil, err
	}
	head, _ := revParseFull(abs, "HEAD")
	branchOut, _ := runGit(abs, "symbolic-ref", "--short", "-q", "HEAD")
	branch := strings.TrimSpace(branchOut)
	base, _ := mergeBase(abs, "HEAD")
	return &Worktree{
		Name:       slug(filepath.Base(abs)),
		Dir:        abs,
		RepoRoot:   root,
		Branch:     branch,
		Base:       base,
		Detached:   branch == "",
		Status:     StatusReady,
		DirtyFiles: dirtyCount(abs),
		HeadSHA:    head,
	}, nil
}

func snapshotCommit(ctx context.Context, dir, base, message string) (string, error) {
	tmp, err := os.CreateTemp("", "term-llm-index-*")
	if err != nil {
		return "", err
	}
	idx := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(idx)
	defer os.Remove(idx)
	env := []string{"GIT_INDEX_FILE=" + idx}
	if out, err := runGitCtxEnv(ctx, dir, env, "read-tree", "HEAD"); err != nil {
		return "", fmt.Errorf("worktree: snapshot read-tree: %w: %s", err, strings.TrimSpace(out))
	}
	if out, err := runGitCtxEnv(ctx, dir, env, "add", "-A"); err != nil {
		return "", fmt.Errorf("worktree: snapshot add: %w: %s", err, strings.TrimSpace(out))
	}
	treeOut, err := runGitCtxEnv(ctx, dir, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("worktree: snapshot write-tree: %w: %s", err, strings.TrimSpace(treeOut))
	}
	tree := strings.TrimSpace(treeOut)
	commitOut, err := runGitCtx(ctx, dir, "commit-tree", tree, "-p", base, "-m", message)
	if err != nil {
		return "", fmt.Errorf("worktree: snapshot commit-tree: %w: %s", err, strings.TrimSpace(commitOut))
	}
	return strings.TrimSpace(commitOut), nil
}

func conflictFiles(root string) []string {
	out, err := runGit(root, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

func dirtyCount(dir string) int {
	out, err := runGit(dir, "status", "--porcelain")
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func mergeBase(dir, ref string) (string, error) {
	root, err := canonicalRepoRoot(dir)
	if err != nil {
		return "", err
	}
	rootHead, err := revParseFull(root, "HEAD")
	if err != nil {
		return "", err
	}
	out, err := runGit(dir, "merge-base", ref, rootHead)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func revParseFull(dir, ref string) (string, error) {
	out, err := runGit(dir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func canonicalRepoRoot(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("empty repo dir")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	out, err := runGit(abs, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("worktree: resolve git common dir: %w", err)
	}
	common := strings.TrimSpace(out)
	if common == "" {
		return "", fmt.Errorf("worktree: empty git common dir")
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(abs, common)
	}
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		common = resolved
	} else if absCommon, err := filepath.Abs(common); err == nil {
		common = absCommon
	}
	common = filepath.Clean(common)
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common), nil
	}
	return filepath.Dir(common), nil
}

func canonicalPathWithExistingAncestor(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	cur := abs
	var tail []string
	for {
		if info, err := os.Stat(cur); err == nil && info.IsDir() {
			if resolved, err := filepath.EvalSymlinks(cur); err == nil {
				for i := len(tail) - 1; i >= 0; i-- {
					resolved = filepath.Join(resolved, tail[i])
				}
				return filepath.Clean(resolved)
			}
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
	return filepath.Clean(abs)
}

func repoHash8(root string) string {
	h := sha256.Sum256([]byte(filepath.Clean(root)))
	return hex.EncodeToString(h[:])[:8]
}

func resolveName(root, requested string) (string, string, error) {
	name := slug(requested)
	if name == "" {
		name = randomName()
	}
	dir := filepath.Join(root, name)
	if _, err := os.Stat(dir); err == nil {
		if requested != "" {
			return "", "", ErrExists
		}
		for i := 2; i < 1000; i++ {
			candidate := fmt.Sprintf("%s-%d", name, i)
			dir = filepath.Join(root, candidate)
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				return candidate, dir, nil
			}
		}
		return "", "", ErrExists
	}
	return name, dir, nil
}

func randomName() string {
	adjectives := []string{"bright", "calm", "clever", "quiet", "swift", "tidy"}
	nouns := []string{"branch", "patch", "tree", "delta", "leaf", "forge"}
	ai := randomInt(len(adjectives))
	ni := randomInt(len(nouns))
	return adjectives[ai] + "-" + nouns[ni]
}

func randomInt(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return int(time.Now().UnixNano() % int64(n))
	}
	return int(v.Int64())
}

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == '.' || unicode.IsSpace(r):
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func metaDir(root string) string        { return filepath.Join(root, ".meta") }
func metaPath(root, name string) string { return filepath.Join(metaDir(root), slug(name)+".json") }

func writeMetadata(root string, m metadata) error {
	if err := os.MkdirAll(metaDir(root), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath(root, m.Name), data, 0o644)
}

func readAllMetadata(root string) []metadata {
	entries, err := os.ReadDir(metaDir(root))
	if err != nil {
		return nil
	}
	var metas []metadata
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(metaDir(root), entry.Name()))
		if err != nil {
			continue
		}
		var m metadata
		if json.Unmarshal(data, &m) == nil && m.Dir != "" {
			metas = append(metas, m)
		}
	}
	return metas
}

func metadataByDir(root string) map[string]metadata {
	out := map[string]metadata{}
	for _, m := range readAllMetadata(root) {
		out[filepath.Clean(m.Dir)] = m
	}
	return out
}

func parsePorcelainWorktrees(out string) []map[string]string {
	var records []map[string]string
	cur := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if len(cur) > 0 {
				records = append(records, cur)
				cur = map[string]string{}
			}
			continue
		}
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			cur[line] = "true"
			continue
		}
		cur[key] = val
	}
	if len(cur) > 0 {
		records = append(records, cur)
	}
	return records
}

func sortWorktrees(items []Worktree) {
	sort.Slice(items, func(i, j int) bool {
		li, lj := items[i].LastBoundAt, items[j].LastBoundAt
		if !li.Equal(lj) {
			if li.IsZero() {
				return false
			}
			if lj.IsZero() {
				return true
			}
			return li.After(lj)
		}
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
}

func copyFiles(srcRoot, dstRoot string, patterns []string) error {
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(srcRoot, pattern))
		if err != nil {
			return err
		}
		for _, src := range matches {
			info, err := os.Stat(src)
			if err != nil || info.IsDir() {
				continue
			}
			rel, err := filepath.Rel(srcRoot, src)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			dst := filepath.Join(dstRoot, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := copyFile(src, dst, info.Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func runScript(ctx context.Context, dir, script string) (string, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", script)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func runGit(dir string, args ...string) (string, error) {
	return runGitCtx(context.Background(), dir, args...)
}

func runGitCtx(ctx context.Context, dir string, args ...string) (string, error) {
	return runGitCtxEnv(ctx, dir, nil, args...)
}

func runGitCtxEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runGitAllowExit(dir string, args []string, allowed map[int]bool) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), nil
	}
	if ee, ok := err.(*exec.ExitError); ok && allowed[ee.ExitCode()] {
		return string(out), nil
	}
	return string(out), err
}

func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	if ra, err := filepath.EvalSymlinks(aa); err == nil {
		aa = ra
	}
	if rb, err := filepath.EvalSymlinks(bb); err == nil {
		bb = rb
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
