package mentions

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	vcsDirs             = map[string]struct{}{`.git`: {}, `.hg`: {}, `.svn`: {}}
	errNotGitWorktree   = errors.New("not in a git worktree")
	discoverGitForBuild = discoverGit
)

// Build discovers project files and synthesized parent directories.
func Build(ctx context.Context, root string, opts BuildOptions) (*Snapshot, error) {
	if opts.MaxCandidates <= 0 || opts.MaxPathBytes <= 0 {
		opts = DefaultBuildOptions()
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve mention root: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("stat mention root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("mention root is not a directory: %s", absRoot)
	}

	collector := newCandidateCollector(opts)
	source := "git"
	gitErr := discoverGitForBuild(ctx, absRoot, collector)
	if errors.Is(gitErr, errNotGitWorktree) {
		source = "walk"
		if err := discoverWalk(ctx, absRoot, collector); err != nil {
			return nil, err
		}
	} else if gitErr != nil {
		return nil, gitErr
	}

	paths := make([]string, 0, len(collector.entries))
	for path := range collector.entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	snapshot := &Snapshot{Root: absRoot, Source: source, BuiltAt: time.Now(), Truncated: collector.truncated}
	for _, path := range paths {
		candidate := makeCandidate(path, collector.entries[path])
		snapshot.Candidates = append(snapshot.Candidates, candidate)
		snapshot.PathBytes += candidateAccountedBytes(candidate)
	}
	return snapshot, nil
}

type candidateCollector struct {
	entries   map[string]Kind
	opts      BuildOptions
	pathBytes int64
	truncated bool
}

func newCandidateCollector(opts BuildOptions) *candidateCollector {
	return &candidateCollector{entries: make(map[string]Kind), opts: opts}
}

func (c *candidateCollector) addWithParents(path string, kind Kind) bool {
	clean, ok := cleanSafeRelativePath(path)
	if !ok {
		return true
	}
	var parents []string
	for parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(clean))); parent != "." && parent != ""; parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(parent))) {
		parents = append(parents, parent)
	}
	for i := len(parents) - 1; i >= 0; i-- {
		if !c.add(parents[i], KindDirectory) {
			return false
		}
	}
	return c.add(clean, kind)
}

func (c *candidateCollector) add(path string, kind Kind) bool {
	if existing, ok := c.entries[path]; ok {
		if existing == KindDirectory && kind == KindFile {
			c.entries[path] = kind
		}
		return true
	}
	candidate := makeCandidate(path, kind)
	accounted := candidateAccountedBytes(candidate)
	if len(c.entries) >= c.opts.MaxCandidates || c.pathBytes+accounted > c.opts.MaxPathBytes {
		c.truncated = true
		return false
	}
	c.entries[path] = kind
	c.pathBytes += accounted
	return true
}

func candidateAccountedBytes(candidate Candidate) int64 {
	return int64(len(candidate.Path)+len(candidate.LowerPath)) + 64
}

func discoverGit(ctx context.Context, root string, collector *candidateCollector) error {
	hasMarker := gitWorktreeMarkerExists(root)
	probe := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	probe.Dir = root
	out, err := probe.Output()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if hasMarker {
			return fmt.Errorf("probe git worktree: %w", err)
		}
		return errNotGitWorktree
	}
	if strings.TrimSpace(string(out)) != "true" {
		if hasMarker {
			return errors.New("git metadata found but root is not an accessible worktree")
		}
		return errNotGitWorktree
	}

	cmd := exec.CommandContext(ctx, "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--deduplicate", "--", ".")
	cmd.Dir = root
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open git ls-files output: %w", err)
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("start git ls-files: %w", err)
	}

	reader := bufio.NewReader(stdout)
	for {
		part, readErr := reader.ReadString(0)
		if len(part) > 0 {
			rel := strings.TrimSuffix(part, "\x00")
			if clean, ok := cleanSafeRelativePath(filepath.ToSlash(rel)); ok {
				path := filepath.Join(root, filepath.FromSlash(clean))
				if fi, statErr := os.Lstat(path); statErr == nil {
					switch {
					case fi.Mode().IsRegular():
						collector.addWithParents(clean, KindFile)
					case fi.IsDir(): // gitlinks/submodules are directory-only in V1
						collector.addWithParents(strings.TrimSuffix(clean, "/"), KindDirectory)
					}
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = cmd.Wait()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read git ls-files: %w", readErr)
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("git ls-files: %w", err)
	}
	return nil
}

func gitWorktreeMarkerExists(root string) bool {
	for dir := filepath.Clean(root); ; dir = filepath.Dir(dir) {
		_, err := os.Lstat(filepath.Join(dir, ".git"))
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
	}
}

func discoverWalk(ctx context.Context, root string, collector *candidateCollector) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if _, skip := vcsDirs[entry.Name()]; skip {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if !collector.addWithParents(filepath.ToSlash(rel), KindFile) {
			return fs.SkipAll
		}
		return nil
	})
}

func cleanSafeRelativePath(path string) (string, bool) {
	if path == "" || filepath.IsAbs(path) || !utf8.ValidString(path) {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	for _, r := range clean {
		if r == 0 || r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return clean, true
}

func makeCandidate(path string, kind Kind) Candidate {
	lower := lowerASCII(path)
	base := strings.LastIndexByte(path, '/') + 1
	candidate := Candidate{Path: path, LowerPath: lower, BaseOffset: uint32(base), Kind: kind}
	for _, b := range []byte(lower) {
		candidate.ASCII[b>>6] |= 1 << (b & 63)
	}
	return candidate
}

func lowerASCII(value string) string {
	buf := []byte(value)
	for i, b := range buf {
		if b >= 'A' && b <= 'Z' {
			buf[i] = b + ('a' - 'A')
		}
	}
	return string(buf)
}
