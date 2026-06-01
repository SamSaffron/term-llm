// Package worktree manages git worktrees for term-llm sessions.
//
// A worktree is a second checkout of the same repository (sharing one .git)
// created with `git worktree add`, living out-of-tree under a managed root:
//
//	$XDG_DATA_HOME/term-llm/worktrees/<repo-hash>/<name>/
//
// Worktrees are created in a detached HEAD by default so that refs/heads/* stay
// clean and git's "a branch can only be checked out in one worktree" rule is
// sidestepped until the user explicitly promotes to a named branch.
//
// This package owns all git logic for the feature; callers (the TUI) drive it
// through the exported Create/List/Get/Promote/Remove/Diff API and never shell
// out to git themselves.
package worktree

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/appdata"
)

// Status describes where a worktree is in its lifecycle.
type Status string

const (
	StatusCreating Status = "creating"
	StatusReady    Status = "ready"
	StatusFailed   Status = "failed"
)

// Worktree describes a single git worktree.
type Worktree struct {
	Name       string // friendly slug, unique within the repo
	Dir        string // absolute path of the worktree
	Branch     string // named branch, or "" when detached
	Base       string // commit/branch it was created from (best-effort)
	Detached   bool   // true when HEAD is detached
	Status     Status // Creating | Ready | Failed
	DirtyFiles int    // count from `git status --porcelain`
	HeadSHA    string // current HEAD, short form
}

// CreateOptions configures Create.
type CreateOptions struct {
	Name        string               // optional; a slug is generated when empty
	Base        string               // base commit/branch; defaults to "HEAD"
	SetupScript string               // optional script run in the new worktree after creation
	ProgressFn  func(message string) // optional progress callback for a spinner
	Progress    chan<- Progress      // optional channel for progress events (TUI spinner)
}

// Progress is emitted by Create as it advances through the slow parts (worktree
// add, setup script) so a caller can drive a live spinner.
type Progress struct {
	Message string
}

// metadata is persisted alongside the managed root (outside the worktree dir so
// it never dirties the checkout) to recover Name/Base after creation.
type metadata struct {
	Name      string    `json:"name"`
	Dir       string    `json:"dir"`
	Base      string    `json:"base"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrDirty is returned by Remove when the worktree has uncommitted changes and
// force was not requested.
var ErrDirty = errors.New("worktree has uncommitted changes")

// ErrExists is returned by Create when an explicitly named worktree already
// exists.
var ErrExists = errors.New("worktree already exists")

func (o *CreateOptions) progress(msg string) {
	if o == nil {
		return
	}
	if o.ProgressFn != nil {
		o.ProgressFn(msg)
	}
	emit(o.Progress, msg)
}

// emit sends a progress event without blocking; events are dropped if no
// receiver is ready so Create never stalls on a slow/absent consumer.
func emit(ch chan<- Progress, msg string) {
	if ch == nil {
		return
	}
	select {
	case ch <- Progress{Message: msg}:
	default:
	}
}

// MainRepoRoot returns the main checkout root for any worktree of the repo
// (the directory whose .git is the shared common dir). Useful for rebinding a
// session back to the root checkout after removing its worktree.
func MainRepoRoot(dir string) (string, error) {
	return canonicalRepoRoot(dir)
}

// IsGitRepo reports whether dir is inside a git working tree.
func IsGitRepo(dir string) bool {
	out, err := runGit(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// ManagedRoot returns the directory under which this repo's worktrees live:
// $XDG_DATA_HOME/term-llm/worktrees/<repo-hash>/. The repo identity is derived
// from the shared .git directory, so all worktrees of the same repo agree.
func ManagedRoot(repoRoot string) (string, error) {
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		return "", err
	}
	canonical, err := canonicalRepoRoot(repoRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "worktrees", repoHash(canonical)), nil
}

// Create runs `git worktree add` (detached) under the managed root, then the
// optional setup script. It is long-running and reports progress via
// opts.ProgressFn. On failure it cleans up so no half-created directory is left.
func Create(ctx context.Context, repoRoot string, opts CreateOptions) (*Worktree, error) {
	if !IsGitRepo(repoRoot) {
		return nil, fmt.Errorf("worktree: %q is not a git repository", repoRoot)
	}

	root, err := ManagedRoot(repoRoot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("worktree: create managed root: %w", err)
	}

	base := strings.TrimSpace(opts.Base)
	if base == "" {
		base = "HEAD"
	}
	// Resolve to a concrete commit so the worktree is pinned even if the branch
	// later moves, and so Base is meaningful for diffs.
	baseSHA, err := runGit(repoRoot, "rev-parse", base)
	if err != nil {
		return nil, fmt.Errorf("worktree: resolve base %q: %w", base, err)
	}
	baseSHA = strings.TrimSpace(baseSHA)

	name, dir, err := resolveName(root, opts.Name)
	if err != nil {
		return nil, err
	}

	opts.progress(fmt.Sprintf("Creating worktree %s…", name))
	if out, err := runGitCtx(ctx, repoRoot, "worktree", "add", "--detach", dir, baseSHA); err != nil {
		return nil, fmt.Errorf("worktree: git worktree add: %w: %s", err, strings.TrimSpace(out))
	}

	// From here on, clean up the worktree if anything fails.
	cleanup := func() {
		_, _ = runGit(repoRoot, "worktree", "remove", "--force", dir)
		_, _ = runGit(repoRoot, "worktree", "prune")
		_ = os.RemoveAll(dir)
	}

	writeMetadata(root, metadata{Name: name, Dir: dir, Base: baseSHA, CreatedAt: time.Now()})

	if script := strings.TrimSpace(opts.SetupScript); script != "" {
		opts.progress("Running setup script…")
		if out, err := runScript(ctx, dir, script); err != nil {
			cleanup()
			removeMetadata(root, name)
			return nil, fmt.Errorf("worktree: setup script failed: %w\n%s", err, strings.TrimSpace(out))
		}
	}

	wt, err := Get(dir)
	if err != nil {
		cleanup()
		removeMetadata(root, name)
		return nil, err
	}
	wt.Name = name
	wt.Base = shortSHA(baseSHA)
	wt.Status = StatusReady
	opts.progress("Ready")
	return wt, nil
}

// List returns the repo's worktrees, excluding the main checkout. Names and
// bases are recovered from metadata when available, otherwise derived.
func List(repoRoot string) ([]Worktree, error) {
	if !IsGitRepo(repoRoot) {
		return nil, fmt.Errorf("worktree: %q is not a git repository", repoRoot)
	}
	mainRoot, err := canonicalRepoRoot(repoRoot)
	if err != nil {
		return nil, err
	}
	out, err := runGit(repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("worktree: list: %w", err)
	}
	root, _ := ManagedRoot(repoRoot)
	meta := readAllMetadata(root)

	var result []Worktree
	for _, rec := range parseWorktreeList(out) {
		if pathsEqual(rec.Dir, mainRoot) {
			continue // skip the main checkout; the TUI shows it as a synthetic row
		}
		wt := Worktree{
			Name:     filepath.Base(rec.Dir),
			Dir:      rec.Dir,
			Branch:   rec.Branch,
			Detached: rec.Detached,
			HeadSHA:  shortSHA(rec.HeadSHA),
			Status:   StatusReady,
		}
		if m, ok := meta[rec.Dir]; ok {
			wt.Name = m.Name
			wt.Base = shortSHA(m.Base)
		}
		wt.DirtyFiles = dirtyCount(rec.Dir)
		result = append(result, wt)
	}
	return result, nil
}

// Get inspects a single worktree directory.
func Get(dir string) (*Worktree, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if !IsGitRepo(abs) {
		return nil, fmt.Errorf("worktree: %q is not a git worktree", abs)
	}
	top, err := runGit(abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("worktree: resolve toplevel: %w", err)
	}
	abs = strings.TrimSpace(top)

	wt := &Worktree{
		Name:       filepath.Base(abs),
		Dir:        abs,
		Status:     StatusReady,
		DirtyFiles: dirtyCount(abs),
	}
	if head, err := runGit(abs, "rev-parse", "--short", "HEAD"); err == nil {
		wt.HeadSHA = strings.TrimSpace(head)
	}
	// symbolic-ref succeeds (with a branch name) only when HEAD is attached.
	if branch, err := runGit(abs, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		wt.Branch = strings.TrimSpace(branch)
		wt.Detached = false
	} else {
		wt.Detached = true
	}

	// Recover Base/Name from metadata when present.
	if mainRoot, err := canonicalRepoRoot(abs); err == nil {
		if dataDir, err := appdata.GetDataDir(); err == nil {
			root := filepath.Join(dataDir, "worktrees", repoHash(mainRoot))
			if m, ok := readMetadataByDir(root, abs); ok {
				wt.Name = m.Name
				wt.Base = shortSHA(m.Base)
			}
		}
	}
	return wt, nil
}

// Promote converts a detached-HEAD worktree to a named branch at the current
// HEAD.
func Promote(dir, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return errors.New("worktree: branch name required")
	}
	if out, err := runGit(dir, "switch", "-c", branch); err != nil {
		return fmt.Errorf("worktree: promote to %q: %w: %s", branch, err, strings.TrimSpace(out))
	}
	return nil
}

// Remove deletes a worktree via `git worktree remove`. It refuses on a dirty
// worktree unless force is set. When force is set and the worktree is on a
// named branch, that branch is deleted too.
func Remove(dir string, force bool) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	wt, err := Get(abs)
	if err != nil {
		return err
	}
	if wt.DirtyFiles > 0 && !force {
		return ErrDirty
	}
	mainRoot, err := canonicalRepoRoot(abs)
	if err != nil {
		return err
	}

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, abs)
	if out, err := runGit(mainRoot, args...); err != nil {
		return fmt.Errorf("worktree: remove: %w: %s", err, strings.TrimSpace(out))
	}
	_, _ = runGit(mainRoot, "worktree", "prune")
	_ = os.RemoveAll(abs)

	if force && wt.Branch != "" {
		_, _ = runGit(mainRoot, "branch", "-D", wt.Branch)
	}

	if root, err := ManagedRoot(mainRoot); err == nil {
		removeMetadataByDir(root, abs)
	}
	return nil
}

// Diff returns the diff of the worktree against its base commit (everything
// done in the worktree), falling back to a diff against HEAD when the base is
// unknown.
func Diff(dir string) (string, error) {
	wt, err := Get(dir)
	if err != nil {
		return "", err
	}
	target := "HEAD"
	if wt.Base != "" {
		// wt.Base is short; git accepts short SHAs.
		target = wt.Base
	}
	out, err := runGit(wt.Dir, "--no-pager", "diff", target)
	if err != nil {
		return "", fmt.Errorf("worktree: diff: %w", err)
	}
	return out, nil
}

// --- git plumbing helpers -------------------------------------------------

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runGitCtx(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runScript(ctx context.Context, dir, script string) (string, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM_LLM_WORKTREE_DIR="+dir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// canonicalRepoRoot returns the main worktree's root for any worktree of the
// repo, so all worktrees share one identity. It resolves the shared .git
// (common) directory and returns its parent.
func canonicalRepoRoot(dir string) (string, error) {
	out, err := runGit(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		// Older git may not support --path-format; fall back.
		out, err = runGit(dir, "rev-parse", "--git-common-dir")
		if err != nil {
			return "", fmt.Errorf("worktree: resolve git dir: %w", err)
		}
	}
	common := strings.TrimSpace(out)
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	common, err = filepath.Abs(common)
	if err != nil {
		return "", err
	}
	// common is typically <root>/.git
	root := filepath.Dir(common)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return root, nil
}

func dirtyCount(dir string) int {
	out, err := runGit(dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return 0
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

type worktreeRec struct {
	Dir      string
	HeadSHA  string
	Branch   string
	Detached bool
}

// parseWorktreeList parses `git worktree list --porcelain` output.
func parseWorktreeList(out string) []worktreeRec {
	var recs []worktreeRec
	var cur *worktreeRec
	flush := func() {
		if cur != nil {
			recs = append(recs, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &worktreeRec{Dir: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			continue
		case strings.HasPrefix(line, "HEAD "):
			cur.HeadSHA = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimPrefix(line, "branch ")
			cur.Branch = strings.TrimPrefix(ref, "refs/heads/")
		case line == "detached":
			cur.Detached = true
		case line == "":
			flush()
		}
	}
	flush()
	return recs
}

func shortSHA(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func repoHash(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	h := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(h[:8])
}

func pathsEqual(a, b string) bool {
	ca, err := filepath.Abs(a)
	if err != nil {
		ca = a
	}
	if r, err := filepath.EvalSymlinks(ca); err == nil {
		ca = r
	}
	cb, err := filepath.Abs(b)
	if err != nil {
		cb = b
	}
	if r, err := filepath.EvalSymlinks(cb); err == nil {
		cb = r
	}
	return filepath.Clean(ca) == filepath.Clean(cb)
}

// resolveName picks a worktree name and its target directory under root. An
// explicit name that already exists is an error; a generated slug is retried
// until it is free.
func resolveName(root, requested string) (name, dir string, err error) {
	requested = sanitizeName(requested)
	if requested != "" {
		dir = filepath.Join(root, requested)
		if _, statErr := os.Stat(dir); statErr == nil {
			return "", "", fmt.Errorf("worktree: %w: %q", ErrExists, requested)
		}
		return requested, dir, nil
	}
	for i := 0; i < 20; i++ {
		candidate := Slug()
		dir = filepath.Join(root, candidate)
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			return candidate, dir, nil
		}
	}
	return "", "", errors.New("worktree: could not allocate a unique name")
}

// sanitizeName makes a user-supplied name safe to use as a directory segment.
func sanitizeName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return ""
	}
	var b strings.Builder
	prevDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '_' || r == ' ' || r == '/':
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// --- metadata persistence (best-effort) -----------------------------------

func metaDir(root string) string { return filepath.Join(root, ".meta") }

func writeMetadata(root string, m metadata) {
	dir := metaDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, m.Name+".json"), data, 0o644)
}

func removeMetadata(root, name string) {
	_ = os.Remove(filepath.Join(metaDir(root), name+".json"))
}

func readAllMetadata(root string) map[string]metadata {
	result := map[string]metadata{}
	entries, err := os.ReadDir(metaDir(root))
	if err != nil {
		return result
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(metaDir(root), e.Name()))
		if err != nil {
			continue
		}
		var m metadata
		if json.Unmarshal(data, &m) == nil && m.Dir != "" {
			result[m.Dir] = m
		}
	}
	return result
}

func readMetadataByDir(root, dir string) (metadata, bool) {
	for d, m := range readAllMetadata(root) {
		if pathsEqual(d, dir) {
			return m, true
		}
	}
	return metadata{}, false
}

func removeMetadataByDir(root, dir string) {
	for _, m := range readAllMetadata(root) {
		if pathsEqual(m.Dir, dir) {
			removeMetadata(root, m.Name)
		}
	}
}

// --- slug generation ------------------------------------------------------

var slugAdjectives = []string{
	"amber", "azure", "bold", "brave", "bright", "calm", "clear", "cobalt",
	"coral", "crimson", "crisp", "dawn", "deep", "dusk", "eager", "ember",
	"fair", "fleet", "fond", "gentle", "glad", "gold", "green", "hardy",
	"hazel", "ivory", "jade", "keen", "lance", "lively", "lunar", "mellow",
	"merry", "mild", "neon", "noble", "olive", "polar", "proud", "quiet",
	"rapid", "royal", "ruby", "sage", "sleek", "snowy", "solar", "spry",
	"still", "swift", "teal", "tidy", "vivid", "warm", "wise", "zesty",
}

var slugNouns = []string{
	"anchor", "arbor", "aspen", "badge", "birch", "bloom", "bolt", "brook",
	"canyon", "cedar", "cliff", "comet", "coral", "cove", "crane", "creek",
	"crest", "delta", "drift", "ember", "fern", "field", "fjord", "flare",
	"glade", "glen", "grove", "harbor", "haven", "heron", "hollow", "inlet",
	"isle", "knoll", "lake", "ledge", "maple", "marsh", "meadow", "mesa",
	"oasis", "orbit", "otter", "peak", "pine", "pond", "reef", "ridge",
	"river", "shoal", "shore", "slate", "spire", "summit", "thorn", "vale",
}

// Slug returns an adjective-noun pair like "neon-canyon".
func Slug() string {
	return slugAdjectives[randIndex(len(slugAdjectives))] + "-" + slugNouns[randIndex(len(slugNouns))]
}

func randIndex(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}
