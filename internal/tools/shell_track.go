package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/llm"
)

// Bounds for shell file-change snapshots. They cap the cost of a single shell
// call, not correctness: files beyond the content-read budgets are still
// detected via stat and recorded metadata-only (truncated).
const (
	maxShellGlobMatches   = 1000             // candidate paths per glob expansion
	maxShellContentReads  = 200              // full-content snapshots per shell call
	maxShellSnapshotBytes = 64 * 1024 * 1024 // total content bytes held per shell call (pre + post)
	gitCommandTimeout     = 5 * time.Second
)

// shellSnapshotEntry captures one file's pre-exec state.
type shellSnapshotEntry struct {
	existed bool
	size    int64
	modTime time.Time
	content []byte // nil when not read (oversized or beyond read budget)
}

// shellSnapshot holds the pre-exec state used to detect file changes made by
// a shell command. Three layers feed it: declared affected_paths globs, the
// session's already-tracked paths, and git status when inside a repository.
type shellSnapshot struct {
	sessionID    string
	workDir      string
	patterns     []string
	files        map[string]*shellSnapshotEntry
	gitRoot      string
	gitStatus    map[string]string // absolute path -> porcelain XY status
	contentReads int
	contentBytes int64
	maxFileBytes int
}

// canReadContent reports whether a file of the given size may have its
// content captured under the per-file cap, the read-count cap, and the
// total-bytes budget. Pathological calls (many large files) degrade to
// stat-only entries and truncated metadata records instead of holding
// hundreds of megabytes in memory.
func (snap *shellSnapshot) canReadContent(size int64) bool {
	return size <= int64(snap.maxFileBytes) &&
		snap.contentReads < maxShellContentReads &&
		snap.contentBytes+size <= maxShellSnapshotBytes
}

// noteContentRead accounts one captured content buffer against the budgets.
func (snap *shellSnapshot) noteContentRead(content []byte) {
	snap.contentReads++
	snap.contentBytes += int64(len(content))
}

// preShellSnapshot records the relevant filesystem state before a shell
// command runs. Returns nil when tracking is inactive.
func preShellSnapshot(ctx context.Context, recorder FileChangeRecorder, workDir string, patterns []string) *shellSnapshot {
	if recorder == nil {
		return nil
	}
	sessionID := llm.SessionIDFromContext(ctx)
	if sessionID == "" {
		return nil
	}

	snap := &shellSnapshot{
		sessionID:    sessionID,
		workDir:      workDir,
		patterns:     patterns,
		files:        make(map[string]*shellSnapshotEntry),
		maxFileBytes: recorder.MaxFileBytes(),
	}

	if repo := DetectGitRepo(workDir); repo.IsRepo {
		snap.gitRoot = repo.Root
	}

	sessionPaths := recorder.SessionPaths(ctx, sessionID)
	// When the caller supplied bounded hints, trust that scope and avoid the
	// repo-wide git status fallback. Commands that omit hints still get the
	// broader best-effort dirty/untracked detection below.
	hasBoundedHints := len(patterns) > 0 || len(sessionPaths) > 0
	if snap.gitRoot != "" && !hasBoundedHints {
		snap.gitStatus = gitStatusPorcelain(ctx, snap.gitRoot)
	}

	candidates := expandShellPatterns(workDir, patterns)
	if snap.gitRoot != "" {
		// Affected-path entries can be intentionally broad (for example "**/*"),
		// and literal entries can still name generated files. In a git repository,
		// keep these hints aligned with Git's notion of source files: ignored
		// build/cache artifacts should not enter the tracked file-change set just
		// because an affected_paths entry happened to match them. Session-tracked
		// paths are appended below and intentionally bypass this filter, preserving
		// an explicit per-session escape hatch.
		candidates = filterGitIgnoredCandidates(ctx, snap.gitRoot, candidates)
	}
	if snap.gitStatus != nil {
		// Snapshot paths that were already dirty before the command. Otherwise a
		// command that edits a dirty tracked file, or an existing untracked file,
		// can leave porcelain status unchanged (e.g. " M" -> " M", "??" ->
		// "??") and would be invisible in postShellChanges.
		for path := range snap.gitStatus {
			candidates = append(candidates, path)
		}
	}
	candidates = append(candidates, sessionPaths...)
	for _, path := range candidates {
		if _, seen := snap.files[path]; seen {
			continue
		}
		snap.files[path] = snap.statAndMaybeRead(path)
	}
	return snap
}

// postShellChanges diffs the filesystem against a pre-exec snapshot and
// records every detected change. Runs regardless of the command's exit code —
// partial writes are real changes.
func postShellChanges(ctx context.Context, recorder FileChangeRecorder, snap *shellSnapshot) []llm.FileChange {
	if snap == nil || recorder == nil {
		return nil
	}
	// The exec context may have timed out; recording should still proceed after
	// already-applied filesystem mutations, but keep a short timeout so tracking
	// can never hang the shell tool indefinitely.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fileRecordTimeout)
	defer cancel()

	type candidate struct {
		entry        *shellSnapshotEntry // nil when not statted pre-exec (git-derived)
		fromReexpand bool
	}
	candidates := make(map[string]candidate)

	for path, entry := range snap.files {
		candidates[path] = candidate{entry: entry}
	}
	// Re-expanding the same patterns catches files created by the command.
	// A path matched now but not pre-exec did not exist then.
	reexpanded := expandShellPatterns(snap.workDir, snap.patterns)
	if snap.gitRoot != "" {
		reexpanded = filterGitIgnoredCandidates(ctx, snap.gitRoot, reexpanded)
	}
	for _, path := range reexpanded {
		if _, seen := candidates[path]; !seen {
			candidates[path] = candidate{fromReexpand: true}
		}
	}

	// Only diff post-status against Git when a pre-status snapshot was captured;
	// otherwise every dirty path would look newly changed.
	var postStatus map[string]string
	if snap.gitRoot != "" && snap.gitStatus != nil {
		postStatus = gitStatusPorcelain(ctx, snap.gitRoot)
		for path := range gitChangedPaths(snap.gitStatus, postStatus) {
			if _, seen := candidates[path]; !seen {
				candidates[path] = candidate{}
			}
		}
	}

	var changes []llm.FileChange
	for path, cand := range candidates {
		rec := snap.buildChangeRecord(ctx, path, cand.entry, cand.fromReexpand)
		if rec == nil {
			continue
		}
		rec.ToolName = ShellToolName
		rec.ToolCallID = llm.CallIDFromContext(ctx)
		if fc := recorder.RecordChange(ctx, *rec); fc != nil {
			changes = append(changes, *fc)
		}
	}
	return changes
}

// buildChangeRecord compares one path's current state with its pre-exec state
// and returns the change to record, or nil when nothing changed (or change
// detection is impossible).
func (snap *shellSnapshot) buildChangeRecord(ctx context.Context, path string, prev *shellSnapshotEntry, fromReexpand bool) *filetrack.ChangeRecord {
	info, statErr := os.Stat(path)
	existsNow := statErr == nil && info.Mode().IsRegular()
	if statErr == nil && !info.Mode().IsRegular() {
		return nil // directories, sockets, etc.
	}

	rec := &filetrack.ChangeRecord{SessionID: snap.sessionID, Path: path}

	// Establish the "before" side.
	switch {
	case prev != nil && !prev.existed:
		rec.BeforeMissing = true
	case prev != nil && prev.content != nil:
		rec.Before = prev.content
	case prev != nil:
		// Statted pre-exec but content not read (oversized / beyond budget):
		// change detection falls back to size+mtime.
		if existsNow && info.Size() == prev.size && info.ModTime().Equal(prev.modTime) {
			return nil
		}
		rec.BeforeUnknown = true
		rec.BeforeSizeHint = prev.size
	case fromReexpand:
		// Newly matched by the same pre-exec glob set → did not exist before.
		rec.BeforeMissing = true
	default:
		// Git-derived candidate never statted pre-exec; classify via status.
		preLine, wasDirty := snap.gitStatus[path]
		switch {
		case !wasDirty:
			// Clean and tracked pre-exec: the index still holds the pre-exec
			// content. Recovered content counts against the snapshot budget
			// like any other read.
			if snap.contentBytes < maxShellSnapshotBytes && snap.contentReads < maxShellContentReads {
				if content, ok := gitShowIndex(ctx, snap.gitRoot, path, snap.maxFileBytes); ok {
					snap.noteContentRead(content)
					rec.Before = content
				} else {
					rec.BeforeUnknown = true
				}
			} else {
				rec.BeforeUnknown = true
			}
		case strings.HasPrefix(preLine, "??"):
			// Untracked pre-exec; content unrecoverable.
			rec.BeforeUnknown = true
		default:
			// Dirty tracked file pre-exec; worktree content unrecoverable.
			rec.BeforeUnknown = true
		}
	}

	// Establish the "after" side.
	if !existsNow {
		rec.AfterMissing = true
		if rec.BeforeMissing {
			return nil // never existed in either state
		}
		return rec
	}
	if !snap.canReadContent(info.Size()) {
		rec.AfterUnknown = true
		rec.AfterSizeHint = info.Size()
		return rec
	}
	content, err := os.ReadFile(path)
	if err != nil {
		rec.AfterUnknown = true
		rec.AfterSizeHint = info.Size()
		return rec
	}
	snap.noteContentRead(content)
	rec.After = content

	// Skip unchanged files (store would also drop them, but avoiding the
	// round trip keeps the common no-op case cheap).
	if rec.Before != nil && bytes.Equal(rec.Before, rec.After) {
		return nil
	}
	return rec
}

// statAndMaybeRead captures one file's current state, reading content when it
// fits the per-file cap and the read budget.
func (snap *shellSnapshot) statAndMaybeRead(path string) *shellSnapshotEntry {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return &shellSnapshotEntry{existed: false}
	}
	entry := &shellSnapshotEntry{existed: true, size: info.Size(), modTime: info.ModTime()}
	if !snap.canReadContent(info.Size()) {
		return entry
	}
	if content, err := os.ReadFile(path); err == nil {
		snap.noteContentRead(content)
		entry.content = content
	}
	return entry
}

// errShellGlobLimit terminates a glob walk once enough candidates are found.
var errShellGlobLimit = errors.New("shell glob match limit reached")

// expandShellPatterns resolves affected_paths entries (files or globs,
// relative to workDir or absolute) into absolute paths. Literal paths are
// included even when missing so creations can be detected. GlobWalk (rather
// than FilepathGlob) lets the walk stop at the match cap instead of
// collecting every match from a pathological pattern like "**" first.
func expandShellPatterns(workDir string, patterns []string) []string {
	var paths []string
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(workDir, pattern)
		}
		if !strings.ContainsAny(pattern, "*?[{") {
			paths = append(paths, filepath.Clean(pattern))
			continue
		}
		if len(paths) >= maxShellGlobMatches {
			return paths
		}

		base, rel := doublestar.SplitPattern(filepath.ToSlash(pattern))
		baseDir := filepath.FromSlash(base)
		_ = doublestar.GlobWalk(os.DirFS(baseDir), rel, func(matchPath string, d fs.DirEntry) error {
			if !d.IsDir() || matchPath == "." {
				paths = append(paths, filepath.Clean(filepath.Join(baseDir, filepath.FromSlash(matchPath))))
			}
			if len(paths) >= maxShellGlobMatches {
				return errShellGlobLimit
			}
			return nil
		}, doublestar.WithNoFollow())
	}
	return paths
}

// gitIgnoredCandidates returns the subset of paths ignored by Git. It uses
// git-check-ignore rather than parsing .gitignore files so repo-level excludes,
// nested .gitignore files, and global excludes all behave exactly as Git does.
// Paths outside the repo are treated as not ignored.
func gitIgnoredCandidates(ctx context.Context, root string, paths []string) map[string]bool {
	ignored := make(map[string]bool)
	if root == "" || len(paths) == 0 {
		return ignored
	}

	canonicalRoot := canonicalPathForGitRel(root)
	relToAbs := make(map[string]string, len(paths))
	var input strings.Builder
	for _, path := range paths {
		cleanPath := filepath.Clean(path)
		rel := GetRelativePath(cleanPath, root)
		if rel == cleanPath || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			canonicalPath := canonicalPathForGitRel(cleanPath)
			rel = GetRelativePath(canonicalPath, canonicalRoot)
			if rel == canonicalPath || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
				continue
			}
		}
		rel = filepath.ToSlash(rel)
		if _, seen := relToAbs[rel]; seen {
			continue
		}
		relToAbs[rel] = cleanPath
		input.WriteString(rel)
		input.WriteByte('\x00')
	}
	if input.Len() == 0 {
		return ignored
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", root, "check-ignore", "-z", "--stdin")
	cmd.Stdin = strings.NewReader(input.String())
	out, err := cmd.Output()
	if err != nil {
		// Exit status 1 means no paths matched. Other failures (for example a
		// transient git error) also degrade to an empty ignore set; file tracking is
		// best-effort and should never fail the shell tool.
		return ignored
	}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		if abs, ok := relToAbs[filepath.ToSlash(rel)]; ok {
			ignored[abs] = true
		}
	}
	return ignored
}

func canonicalPathForGitRel(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.Clean(abs)

	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}

	var missing []string
	probe := abs
	for {
		parent := filepath.Dir(probe)
		if parent == probe {
			return abs
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved)
		}
	}
}

func filterGitIgnoredCandidates(ctx context.Context, root string, paths []string) []string {
	ignored := gitIgnoredCandidates(ctx, root, paths)
	if len(ignored) == 0 {
		return paths
	}
	filtered := make([]string, 0, len(paths))
	for _, path := range paths {
		if ignored[filepath.Clean(path)] {
			continue
		}
		filtered = append(filtered, path)
	}
	return filtered
}

// gitStatusPorcelain returns the repo's dirty paths (absolute) mapped to their
// porcelain XY status. Returns nil on any failure — git tracking is optional.
func gitStatusPorcelain(ctx context.Context, root string) map[string]string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain", "-z", "--untracked-files=all")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	status := make(map[string]string)
	tokens := strings.Split(string(out), "\x00")
	for i := 0; i < len(tokens); i++ {
		entry := tokens[i]
		if len(entry) < 4 {
			continue
		}
		xy := entry[:2]
		rel := entry[3:]
		// Renames/copies carry the original path in the next token.
		if xy[0] == 'R' || xy[0] == 'C' {
			i++
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		status[abs] = xy
	}
	return status
}

// gitChangedPaths returns paths whose porcelain status differs between two
// snapshots (including paths present in only one of them).
func gitChangedPaths(pre, post map[string]string) map[string]struct{} {
	changed := make(map[string]struct{})
	for path, line := range post {
		if pre[path] != line {
			changed[path] = struct{}{}
		}
	}
	for path := range pre {
		if _, ok := post[path]; !ok {
			changed[path] = struct{}{}
		}
	}
	return changed
}

// gitShowIndex returns the index content for a path inside a repo. Used to
// recover the before-content of files that were clean when the shell command
// started. Returns ok=false when the file is untracked, too large, or git fails.
func gitShowIndex(ctx context.Context, root, absPath string, maxBytes int) ([]byte, bool) {
	if maxBytes < 0 {
		return nil, false
	}
	rel := GetRelativePath(absPath, root)
	if rel == absPath || strings.HasPrefix(rel, "..") {
		return nil, false
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", root, "show", ":"+filepath.ToSlash(rel))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false
	}
	if err := cmd.Start(); err != nil {
		return nil, false
	}

	out, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxBytes)+1))
	if readErr != nil {
		cancel()
		_ = cmd.Wait()
		return nil, false
	}
	if len(out) > maxBytes {
		cancel()
		_ = cmd.Wait()
		return nil, false
	}
	if err := cmd.Wait(); err != nil {
		return nil, false
	}
	return out, true
}
