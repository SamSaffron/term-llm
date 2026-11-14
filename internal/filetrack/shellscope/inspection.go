package shellscope

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/term-llm/term-llm/internal/tools/gitidentity"
)

const (
	// MaxGlobMatches caps discovered candidates per glob expansion.
	MaxGlobMatches         = 1000
	maxGitBatchHeaderBytes = 64 * 1024
	maxGitBatchDrainBytes  = 64 * 1024 * 1024
	// GitCommandTimeout bounds optional Git inspection, not tool execution.
	GitCommandTimeout   = 5 * time.Second
	gitCommandWaitDelay = 100 * time.Millisecond
)

// errShellGlobLimit terminates a glob walk once enough candidates are found.
var errShellGlobLimit = errors.New("shell glob match limit reached")

// NormalizePatterns trims inspection hints and drops empty entries.
func NormalizePatterns(patterns []string) []string {
	normalized := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			normalized = append(normalized, pattern)
		}
	}
	return normalized
}

// Candidate identifies one inspected path and whether it was explicitly named.
type Candidate struct {
	Path    string
	Literal bool
}

// ExpandPatterns resolves affected_paths entries (files or globs,
// relative to workDir or absolute) into absolute paths. Literal paths are
// included even when missing so creations can be detected. GlobWalk (rather
// than FilepathGlob) lets the walk stop at the match cap instead of collecting
// every match from a pathological pattern like "**" first. Its filesystem
// wrapper hides .git directories from traversal so administrative files cannot
// consume the candidate cap.
func ExpandPatterns(workDir string, patterns []string) []Candidate {
	var candidates []Candidate
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(workDir, pattern)
		}
		if !strings.ContainsAny(pattern, "*?[{") {
			path := filepath.Clean(pattern)
			if !HasGitAdminComponent(path) {
				candidates = append(candidates, Candidate{Path: path, Literal: true})
			}
			continue
		}
		if len(candidates) >= MaxGlobMatches {
			return candidates
		}

		base, rel := doublestar.SplitPattern(filepath.ToSlash(pattern))
		baseDir := filepath.Clean(filepath.FromSlash(base))
		if HasGitAdminComponent(baseDir) {
			continue
		}
		fsys := shellGlobFS{FS: os.DirFS(baseDir)}
		_ = doublestar.GlobWalk(fsys, rel, func(matchPath string, d fs.DirEntry) error {
			if d.IsDir() {
				if isGitAdminName(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			path := filepath.Clean(filepath.Join(baseDir, filepath.FromSlash(matchPath)))
			if !HasGitAdminComponent(path) {
				candidates = append(candidates, Candidate{Path: path})
			}
			if len(candidates) >= MaxGlobMatches {
				return errShellGlobLimit
			}
			return nil
		}, doublestar.WithNoFollow())
	}
	return candidates
}

type shellGlobFS struct {
	fs.FS
}

func (fsys shellGlobFS) Open(name string) (fs.File, error) {
	if HasGitAdminComponent(filepath.FromSlash(name)) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return fsys.FS.Open(name)
}

func (fsys shellGlobFS) Stat(name string) (fs.FileInfo, error) {
	if HasGitAdminComponent(filepath.FromSlash(name)) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return fs.Stat(fsys.FS, name)
}

func (fsys shellGlobFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if HasGitAdminComponent(filepath.FromSlash(name)) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	entries, err := fs.ReadDir(fsys.FS, name)
	if err != nil {
		return nil, err
	}
	filtered := entries[:0]
	for _, entry := range entries {
		if !isGitAdminName(entry.Name()) {
			filtered = append(filtered, entry)
		}
	}
	return filtered, nil
}

func isGitAdminName(name string) bool {
	// EqualFold is deliberately limited to the complete component, so .github
	// remains visible while case-insensitive filesystems cannot expose .GIT.
	return strings.EqualFold(name, ".git")
}

// HasGitAdminComponent excludes Git administrative components, including on
// case-insensitive filesystems, without hiding unrelated names such as .github.
func HasGitAdminComponent(path string) bool {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	path = strings.TrimPrefix(path, volume)
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if isGitAdminName(component) {
			return true
		}
	}
	return false
}

// RepoResolver discovers repository ownership within a single inspection phase.
// Its marker cache is phase-local; it must not be shared across commands.
type RepoResolver struct {
	markers map[string]bool
}

// NewRepoResolver starts an independent repository discovery phase.
func NewRepoResolver() *RepoResolver {
	return &RepoResolver{markers: make(map[string]bool)}
}

// OwningRepo discovers the deepest repository containing path. The unbounded
// form is used only for the shell working directory itself.
func (resolver *RepoResolver) OwningRepo(path string) string {
	return resolver.owningRepoWithin(path, "", true)
}

// OwningRepoForCandidate bounds ancestor discovery to the command's relevant
// filesystem scope. Paths under workDir may inherit its owning repository and
// still discover deeper nested repos. Explicit absolute paths outside workDir
// may discover their own repo before the common ancestor, but are not assigned
// to an unrelated .git marker at that arbitrary common ancestor.
func (resolver *RepoResolver) OwningRepoForCandidate(path, workDir, workRepo string) string {
	absPath := absoluteCleanPath(path)
	absWorkDir := absoluteCleanPath(workDir)
	if PathWithinRoot(absPath, absWorkDir) {
		boundary := absWorkDir
		if workRepo != "" {
			boundary = absoluteCleanPath(workRepo)
		}
		return resolver.owningRepoWithin(absPath, boundary, true)
	}
	boundary := commonPathAncestor(absWorkDir, absPath)
	return resolver.owningRepoWithin(absPath, boundary, false)
}

// owningRepoWithin discovers repositories from path up to boundary. Both .git
// directories and worktree/submodule .git files establish ownership.
func (resolver *RepoResolver) owningRepoWithin(path, boundary string, includeBoundary bool) string {
	abs := absoluteCleanPath(path)
	if boundary != "" {
		boundary = absoluteCleanPath(boundary)
		if !PathWithinRoot(abs, boundary) {
			return ""
		}
	}
	start := abs
	if info, err := os.Stat(start); err != nil || !info.IsDir() {
		start = filepath.Dir(start)
	}

	for dir := filepath.Clean(start); ; dir = filepath.Dir(dir) {
		atBoundary := boundary != "" && dir == boundary
		if atBoundary && !includeBoundary {
			return ""
		}
		marker, checked := resolver.markers[dir]
		if !checked {
			_, err := os.Lstat(filepath.Join(dir, ".git"))
			marker = err == nil
			resolver.markers[dir] = marker
		}
		if marker {
			return dir
		}
		if atBoundary {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
	}
}

func absoluteCleanPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

// PathWithinRoot checks lexical containment at complete path-component boundaries.
func PathWithinRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func commonPathAncestor(first, second string) string {
	for dir := filepath.Clean(first); ; dir = filepath.Dir(dir) {
		if PathWithinRoot(second, dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
	}
}

// boundedGitCommand applies GitCommandTimeout without detaching from a shorter
// parent deadline. WaitDelay prevents descendants that inherited stdout/stderr
// pipes from keeping a canceled best-effort Git probe alive indefinitely.
func boundedGitCommand(ctx context.Context, root string, args ...string) (*exec.Cmd, context.CancelFunc) {
	commandCtx, cancel := context.WithTimeout(ctx, GitCommandTimeout)
	cmd := exec.CommandContext(commandCtx, "git", append([]string{"-C", root}, args...)...)
	cmd.WaitDelay = gitCommandWaitDelay
	return cmd, cancel
}

// GitIgnoredCandidates returns the subset of paths ignored by Git. It uses
// git-check-ignore rather than parsing .gitignore files so repo-level excludes,
// nested .gitignore files, and global excludes all behave exactly as Git does.
// Paths outside the repo are treated as not ignored.
func GitIgnoredCandidates(ctx context.Context, root string, paths []string) map[string]bool {
	ignored := make(map[string]bool)
	if root == "" || len(paths) == 0 {
		return ignored
	}

	canonicalRoot := CanonicalPathForGitRel(root)
	relToAbs := make(map[string]string, len(paths))
	var input strings.Builder
	for _, path := range paths {
		cleanPath := filepath.Clean(path)
		rel := gitidentity.GetRelativePath(cleanPath, root)
		if rel == cleanPath || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			canonicalPath := CanonicalPathForGitRel(cleanPath)
			rel = gitidentity.GetRelativePath(canonicalPath, canonicalRoot)
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

	cmd, cancel := boundedGitCommand(ctx, root, "check-ignore", "-z", "--stdin")
	defer cancel()
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

// CanonicalPathForGitRel resolves the nearest existing ancestor, preserving
// missing trailing components for Git ignore checks on not-yet-created paths.
func CanonicalPathForGitRel(path string) string {
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

// GitStatusPorcelain returns the repo's dirty paths (absolute) mapped to their
// porcelain XY status. Returns nil on any failure — git tracking is optional.
func GitStatusPorcelain(ctx context.Context, root string) map[string]string {
	cmd, cancel := boundedGitCommand(ctx, root, "status", "--porcelain", "-z", "--untracked-files=all")
	defer cancel()
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
		abs := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
		if !HasGitAdminComponent(abs) {
			status[abs] = xy
		}
		// Renames/copies carry the original path in the next token. Include
		// both sides so broad discovery can retain the dirty deletion as well as
		// the destination.
		if (xy[0] == 'R' || xy[0] == 'C') && i+1 < len(tokens) {
			i++
			original := filepath.Clean(filepath.Join(root, filepath.FromSlash(tokens[i])))
			if !HasGitAdminComponent(original) {
				status[original] = xy
			}
		}
	}
	return status
}

// GitShowIndexBatch returns bounded index content for paths inside one repo.
// One cat-file process serves the whole batch. Bounded oversized and non-blob
// objects are drained and skipped so later blobs remain recoverable; malformed
// or unreasonable responses abort the remaining best-effort batch.
func GitShowIndexBatch(ctx context.Context, root string, absPaths []string, maxFileBytes, maxReads int, maxTotalBytes int64) map[string][]byte {
	contentByPath := make(map[string][]byte)
	if root == "" || maxFileBytes < 0 || maxReads <= 0 || maxTotalBytes < 0 || len(absPaths) == 0 {
		return contentByPath
	}

	type query struct {
		path string
		spec string
	}
	queries := make([]query, 0, len(absPaths))
	for _, absPath := range absPaths {
		rel := gitidentity.GetRelativePath(absPath, root)
		if rel == absPath || strings.HasPrefix(rel, "..") || strings.ContainsAny(rel, "\r\n") {
			continue
		}
		queries = append(queries, query{path: absPath, spec: ":" + filepath.ToSlash(rel)})
	}
	if len(queries) == 0 {
		return contentByPath
	}

	cmd, cancel := boundedGitCommand(ctx, root, "cat-file", "--batch")
	defer cancel()
	var input strings.Builder
	for _, query := range queries {
		input.WriteString(query.spec)
		input.WriteByte('\n')
	}
	cmd.Stdin = strings.NewReader(input.String())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return contentByPath
	}
	if err := cmd.Start(); err != nil {
		return contentByPath
	}

	reader := bufio.NewReaderSize(stdout, maxGitBatchHeaderBytes)
	var totalBytes, drainedBytes int64
	for _, query := range queries {
		if len(contentByPath) >= maxReads {
			cancel()
			break
		}
		headerBytes, err := reader.ReadSlice('\n')
		if err != nil {
			cancel() // includes ErrBufferFull for an unreasonably long header
			break
		}
		header := strings.TrimSuffix(string(headerBytes), "\n")
		if strings.HasSuffix(header, " missing") {
			continue
		}
		fields := strings.Fields(header)
		if len(fields) != 3 {
			cancel()
			break
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			cancel()
			break
		}

		retain := fields[1] == "blob" &&
			size <= int64(maxFileBytes) &&
			size <= maxTotalBytes-totalBytes
		if !retain {
			if size > maxGitBatchDrainBytes-drainedBytes {
				// A malformed or unreasonably large object cannot be drained
				// within the batch's bounded I/O policy, so abort the remainder.
				cancel()
				break
			}
			if !drainGitBatchObject(reader, size) {
				cancel()
				break
			}
			drainedBytes += size
			continue
		}

		content := make([]byte, size)
		if _, err := io.ReadFull(reader, content); err != nil {
			cancel()
			break
		}
		separator, err := reader.ReadByte()
		if err != nil || separator != '\n' {
			cancel()
			break
		}
		contentByPath[query.path] = content
		totalBytes += size
	}
	_ = cmd.Wait()
	return contentByPath
}

func drainGitBatchObject(reader *bufio.Reader, size int64) bool {
	if _, err := io.CopyN(io.Discard, reader, size); err != nil {
		return false
	}
	separator, err := reader.ReadByte()
	return err == nil && separator == '\n'
}
