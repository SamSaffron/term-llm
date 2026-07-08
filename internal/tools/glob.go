package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/samsaffron/term-llm/internal/llm"
)

// GlobTool implements the glob tool.
type GlobTool struct {
	approval *ApprovalManager
	config   *ToolConfig
}

// NewGlobTool creates a new GlobTool.
func NewGlobTool(approval *ApprovalManager, configs ...*ToolConfig) *GlobTool {
	return &GlobTool{
		approval: approval,
		config:   optionalToolConfig(configs),
	}
}

// GlobArgs are the arguments for glob.
type GlobArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

// FileEntry represents a file in glob results.
type FileEntry struct {
	FilePath  string    `json:"file_path"`
	IsDir     bool      `json:"is_dir"`
	SizeBytes int64     `json:"size_bytes"`
	ModTime   time.Time `json:"mod_time"`
}

const maxGlobResults = 200

var errGlobResultLimit = errors.New("glob result limit reached")

func (t *GlobTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        GlobToolName,
		Description: "Find files by glob pattern (supports ** for recursive matching). Returns file metadata sorted by modification time.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"pattern": map[string]interface{}{
					"type":        "string",
					"description": "Glob pattern supporting ** for recursive matching, e.g., '**/*.go' or 'src/**/*.ts'",
				},
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Base directory for the search (defaults to current directory)",
				},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},
	}
}

func (t *GlobTool) Preview(args json.RawMessage) string {
	var a GlobArgs
	if err := json.Unmarshal(args, &a); err != nil || a.Pattern == "" {
		return ""
	}
	if a.Path != "" {
		return fmt.Sprintf("%s in %s", a.Pattern, a.Path)
	}
	return a.Pattern
}

func (t *GlobTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	warning := WarnUnknownParams(args, []string{"pattern", "path"})
	textOutput := func(message string) llm.ToolOutput {
		return llm.TextOutput(warning + message)
	}

	var a GlobArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.Pattern == "" {
		return textOutput(formatToolError(NewToolError(ErrInvalidParams, "pattern is required"))), nil
	}

	// Set defaults
	basePath := a.Path
	if basePath == "" {
		if t.config != nil {
			basePath = t.config.WorkingDir()
		} else {
			var err error
			basePath, err = os.Getwd()
			if err != nil {
				return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot get working directory: %v", err))), nil
			}
		}
	}

	absBasePath, err := resolveToolPathWithConfig(basePath, false, t.config)
	if err != nil {
		if toolErr, ok := err.(*ToolError); ok {
			return textOutput(formatToolError(toolErr)), nil
		}
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot resolve path: %v", err))), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(GlobToolName, absBasePath, a.Pattern, false)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return textOutput(formatToolError(toolErr)), nil
			}
			return textOutput(formatToolError(NewToolError(ErrPermissionDenied, err.Error()))), nil
		}
		if outcome == Cancel {
			return textOutput(formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", basePath))), nil
		}
	}

	// Let doublestar drive the traversal from the pattern instead of walking the
	// whole tree and matching every path. This keeps exact and prefix-heavy globs
	// (for example "go.mod" or "cmd/*.go") proportional to the matched subtree.
	var entries []FileEntry
	truncated := false
	pattern := filepath.ToSlash(a.Pattern)
	if err := validateGlobPattern(pattern); err != nil {
		return textOutput(formatToolError(NewToolErrorf(ErrInvalidParams, "invalid pattern: %v", err))), nil
	}
	fsys := globContextFS{ctx: ctx, root: absBasePath, fsys: os.DirFS(absBasePath)}

	err = doublestar.GlobWalk(fsys, pattern, func(matchPath string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Preserve the historical tool behaviour of ignoring hidden files and
		// directories even when the pattern names them explicitly. WithNoHidden
		// handles wildcard traversal; this callback handles exact dot-paths.
		if hasHiddenPathSegment(matchPath) {
			if d.IsDir() {
				return doublestar.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		fullPath := absBasePath
		if matchPath != "." {
			fullPath = filepath.Join(absBasePath, filepath.FromSlash(matchPath))
		}
		entries = append(entries, FileEntry{
			FilePath:  fullPath,
			IsDir:     d.IsDir(),
			SizeBytes: info.Size(),
			ModTime:   info.ModTime(),
		})

		if len(entries) >= maxGlobResults {
			truncated = true
			return errGlobResultLimit
		}

		return nil
	}, doublestar.WithNoHidden(), doublestar.WithNoFollow(), doublestar.WithFailOnIOErrors())

	if errors.Is(err, errGlobResultLimit) {
		err = nil
	}
	if err != nil {
		return textOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "walk error: %v", err))), nil
	}

	// Sort by modification time (newest first)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ModTime.After(entries[j].ModTime)
	})

	if len(entries) == 0 {
		return textOutput("No files matched the pattern."), nil
	}

	return textOutput(formatGlobResults(entries, truncated)), nil
}

func validateGlobPattern(pattern string) error {
	if pattern == "." {
		return nil
	}
	if strings.Contains(pattern, "..") {
		return fmt.Errorf("must not contain .. path traversal")
	}
	if !fs.ValidPath(pattern) {
		return fmt.Errorf("must be a relative pattern without . or .. path segments")
	}
	return nil
}

type globContextFS struct {
	ctx  context.Context
	root string
	fsys fs.FS
}

func (f globContextFS) Open(name string) (fs.File, error) {
	if _, err := f.lstatPath(name, false); err != nil {
		return nil, err
	}
	file, err := f.fsys.Open(name)
	if err != nil {
		if ctxErr := f.ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fs.ErrNotExist
	}
	return globContextFile{ctx: f.ctx, File: file}, nil
}

func (f globContextFS) Stat(name string) (fs.FileInfo, error) {
	return f.lstatPath(name, true)
}

func (f globContextFS) ReadDir(name string) ([]fs.DirEntry, error) {
	info, err := f.lstatPath(name, false)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fs.ErrNotExist
	}

	var (
		entries []fs.DirEntry
		readErr error
	)
	if readDirFS, ok := f.fsys.(fs.ReadDirFS); ok {
		entries, readErr = readDirFS.ReadDir(name)
	} else {
		entries, readErr = fs.ReadDir(f.fsys, name)
	}
	if readErr != nil {
		if ctxErr := f.ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, nil
	}
	return entries, nil
}

func (f globContextFS) lstatPath(name string, allowLeafSymlink bool) (fs.FileInfo, error) {
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	if name != "" && name != "." && !fs.ValidPath(name) {
		return nil, fs.ErrNotExist
	}

	current := f.root
	parts := []string{"."}
	if name != "" && name != "." {
		parts = strings.Split(filepath.ToSlash(name), "/")
	}

	var info fs.FileInfo
	for i, part := range parts {
		if part == "" || part == "." {
			current = f.root
		} else if part == ".." {
			return nil, fs.ErrNotExist
		} else {
			current = filepath.Join(current, filepath.FromSlash(part))
		}

		var err error
		info, err = os.Lstat(current)
		if err != nil {
			if ctxErr := f.ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			return nil, fs.ErrNotExist
		}
		if info.Mode()&os.ModeSymlink != 0 && !(allowLeafSymlink && i == len(parts)-1) {
			return nil, fs.ErrNotExist
		}
	}
	return info, nil
}

type globContextFile struct {
	ctx context.Context
	fs.File
}

func (f globContextFile) Read(p []byte) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.File.Read(p)
}

func (f globContextFile) Stat() (fs.FileInfo, error) {
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	return f.File.Stat()
}

func (f globContextFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	readDirFile, ok := f.File.(fs.ReadDirFile)
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Err: errors.ErrUnsupported}
	}
	return readDirFile.ReadDir(n)
}

func hasHiddenPathSegment(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part != "" && part != "." && strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// formatGlobResults formats glob results for the LLM.
func formatGlobResults(entries []FileEntry, truncated bool) string {
	var sb strings.Builder

	for _, e := range entries {
		typeIndicator := "f"
		if e.IsDir {
			typeIndicator = "d"
		}

		// Format size
		size := formatSize(e.SizeBytes)

		// Format time
		timeStr := e.ModTime.Format("2006-01-02 15:04")

		sb.WriteString(fmt.Sprintf("[%s] %s  %s  %s\n", typeIndicator, size, timeStr, e.FilePath))
	}

	if truncated {
		sb.WriteString(fmt.Sprintf("\n[Results truncated at %d files]", maxGlobResults))
	}

	return strings.TrimSuffix(sb.String(), "\n")
}

// formatSize formats a byte count as human-readable.
func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%4dB", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%4.0f%c", float64(bytes)/float64(div), "KMGTPE"[exp])
}
