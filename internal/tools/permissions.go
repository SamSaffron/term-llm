package tools

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gobwas/glob"

	"github.com/samsaffron/term-llm/internal/appdata"
)

// ToolPermissions manages allowlists for tool access.
type ToolPermissions struct {
	mu sync.RWMutex

	ReadDirs       []string // Directories for read/grep/glob/view
	WriteDirs      []string // Directories for write/edit
	ShellAllow     []string // Shell command patterns (glob syntax)
	ScriptCommands []string // Exact script commands (auto-approved)

	// Compiled patterns for shell commands
	shellPatterns []glob.Glob
}

// NewToolPermissions creates a new ToolPermissions instance.
func NewToolPermissions() *ToolPermissions {
	return &ToolPermissions{
		ReadDirs:       []string{},
		WriteDirs:      []string{},
		ShellAllow:     []string{},
		ScriptCommands: []string{},
	}
}

// AddReadDir adds a directory to the read allowlist.
func (p *ToolPermissions) AddReadDir(dir string) error {
	abs, err := canonicalizePath(dir)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Avoid duplicates
	for _, existing := range p.ReadDirs {
		if existing == abs {
			return nil
		}
	}
	p.ReadDirs = append(p.ReadDirs, abs)
	return nil
}

// AddWriteDir adds a directory to the write allowlist.
func (p *ToolPermissions) AddWriteDir(dir string) error {
	abs, err := canonicalizePath(dir)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Avoid duplicates
	for _, existing := range p.WriteDirs {
		if existing == abs {
			return nil
		}
	}
	p.WriteDirs = append(p.WriteDirs, abs)
	return nil
}

// AddShellPattern adds a shell command pattern to the allowlist.
func (p *ToolPermissions) AddShellPattern(pattern string) error {
	g, err := glob.Compile(pattern)
	if err != nil {
		return NewToolErrorf(ErrInvalidParams, "invalid shell pattern %q: %v", pattern, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Avoid duplicates
	for _, existing := range p.ShellAllow {
		if existing == pattern {
			return nil
		}
	}
	p.ShellAllow = append(p.ShellAllow, pattern)
	p.shellPatterns = append(p.shellPatterns, g)
	return nil
}

// AddScriptCommand adds an exact script command to the allowlist.
func (p *ToolPermissions) AddScriptCommand(command string) {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Avoid duplicates
	for _, existing := range p.ScriptCommands {
		if existing == cmd {
			return
		}
	}
	p.ScriptCommands = append(p.ScriptCommands, cmd)
}

// CompileShellPatterns pre-compiles all shell patterns.
func (p *ToolPermissions) CompileShellPatterns() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.compileShellPatternsLocked()
}

func (p *ToolPermissions) compileShellPatternsLocked() error {
	p.shellPatterns = make([]glob.Glob, 0, len(p.ShellAllow))
	for _, pattern := range p.ShellAllow {
		g, err := glob.Compile(pattern)
		if err != nil {
			return NewToolErrorf(ErrInvalidParams, "invalid shell pattern %q: %v", pattern, err)
		}
		p.shellPatterns = append(p.shellPatterns, g)
	}
	return nil
}

// IsPathAllowedForRead checks if a path is allowed for read operations.
func (p *ToolPermissions) IsPathAllowedForRead(path string) (bool, error) {
	resolved, err := canonicalizePath(path)
	if err != nil {
		return false, err
	}
	if isHandoverPath(resolved) {
		return true, nil
	}
	p.mu.RLock()
	dirs := append([]string(nil), p.ReadDirs...)
	p.mu.RUnlock()
	return p.isPathInDirs(resolved, dirs), nil
}

// IsPathAllowedForWrite checks if a path is allowed for write operations.
func (p *ToolPermissions) IsPathAllowedForWrite(path string) (bool, error) {
	resolved, err := canonicalizePathForWrite(path)
	if err != nil {
		return false, err
	}
	if isHandoverPath(resolved) {
		return true, nil
	}
	p.mu.RLock()
	dirs := append([]string(nil), p.WriteDirs...)
	p.mu.RUnlock()
	return p.isPathInDirs(resolved, dirs), nil
}

// isHandoverPath reports whether resolvedPath is inside term-llm's own
// handover data directory. Handover plan documents are app-managed state
// (see session.GetHandoverDir); agents read and update their session's plan
// there constantly, so these paths are always allowed without explicit
// read/write dir grants or approval prompts.
func isHandoverPath(resolvedPath string) bool {
	dataDir, err := appdata.GetDataDir()
	if err != nil {
		return false
	}
	root := filepath.Join(dataDir, "handover")
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != "" {
		root = resolved
	}
	return strings.HasPrefix(resolvedPath, root+string(filepath.Separator))
}

// IsShellCommandAllowed checks if a shell command matches any allowlist pattern or script.
func (p *ToolPermissions) IsShellCommandAllowed(command string) bool {
	trimmedCmd := strings.TrimSpace(command)
	p.mu.RLock()
	scripts := append([]string(nil), p.ScriptCommands...)
	shellAllow := append([]string(nil), p.ShellAllow...)
	patterns := append([]glob.Glob(nil), p.shellPatterns...)
	needsCompile := len(patterns) == 0 && len(shellAllow) > 0
	p.mu.RUnlock()

	// Check exact script matches first
	for _, script := range scripts {
		if trimmedCmd == script {
			return true
		}
	}

	// Ensure patterns are compiled
	if needsCompile {
		p.mu.Lock()
		if len(p.shellPatterns) == 0 && len(p.ShellAllow) > 0 {
			_ = p.compileShellPatternsLocked()
		}
		patterns = append([]glob.Glob(nil), p.shellPatterns...)
		p.mu.Unlock()
	}

	for _, pattern := range patterns {
		if pattern.Match(command) {
			return true
		}
	}
	return false
}

func (p *ToolPermissions) Snapshot() (readDirs, writeDirs, shellAllow []string) {
	if p == nil {
		return nil, nil, nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.ReadDirs...), append([]string(nil), p.WriteDirs...), append([]string(nil), p.ShellAllow...)
}

// isPathInDirs checks if a resolved path is under any of the given directories.
func (p *ToolPermissions) isPathInDirs(resolvedPath string, dirs []string) bool {
	for _, dir := range dirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		// Resolve symlinks in the allowlist dir too
		resolvedDir, _ := filepath.EvalSymlinks(absDir)
		if resolvedDir == "" {
			resolvedDir = absDir
		}

		if strings.HasPrefix(resolvedPath, resolvedDir+string(filepath.Separator)) || resolvedPath == resolvedDir {
			return true
		}
	}
	return false
}

// canonicalizePath resolves a path to its absolute, symlink-resolved form.
func canonicalizePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", NewToolErrorf(ErrInvalidParams, "cannot resolve path: %v", err)
	}

	// EvalSymlinks resolves ALL symlinks in path
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", NewToolError(ErrFileNotFound, abs)
		}
		return "", NewToolErrorf(ErrInvalidParams, "cannot evaluate symlinks: %v", err)
	}

	return filepath.Clean(resolved), nil
}

// canonicalizePathForWrite resolves a path for write operations.
// If the file doesn't exist, it resolves the parent directory.
func canonicalizePathForWrite(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", NewToolErrorf(ErrInvalidParams, "cannot resolve path: %v", err)
	}

	// Try to resolve directly
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist - resolve parent
			return resolveParentSymlinks(abs)
		}
		return "", NewToolErrorf(ErrInvalidParams, "cannot evaluate symlinks: %v", err)
	}

	return filepath.Clean(resolved), nil
}

// resolveParentSymlinks resolves symlinks in the parent directory.
func resolveParentSymlinks(abs string) (string, error) {
	parent := filepath.Dir(abs)
	filename := filepath.Base(abs)

	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if os.IsNotExist(err) {
			// Parent doesn't exist either - this is fine for write operations
			// that will create directories
			return abs, nil
		}
		return "", NewToolErrorf(ErrInvalidParams, "cannot evaluate parent symlinks: %v", err)
	}

	return filepath.Join(resolvedParent, filename), nil
}

// ExtractCommandPrefix extracts a shell command prefix for policy learning.
func ExtractCommandPrefix(cmd string) string {
	tokens := tokenizeCommand(cmd)
	if len(tokens) == 0 {
		return ""
	}

	// Known multi-word commands
	multiWord := map[string]bool{
		"git":     true,
		"npm":     true,
		"go":      true,
		"docker":  true,
		"cargo":   true,
		"yarn":    true,
		"pnpm":    true,
		"make":    true,
		"kubectl": true,
	}

	if len(tokens) >= 2 && multiWord[tokens[0]] {
		return tokens[0] + " " + tokens[1] + " *"
	}
	return tokens[0] + " *"
}

// tokenizeCommand splits a command into tokens, handling quotes.
func tokenizeCommand(cmd string) []string {
	var tokens []string
	var current strings.Builder
	inQuote := false
	quoteChar := rune(0)

	for _, r := range cmd {
		switch {
		case r == '"' || r == '\'':
			if inQuote && r == quoteChar {
				inQuote = false
				quoteChar = 0
			} else if !inQuote {
				inQuote = true
				quoteChar = r
			} else {
				current.WriteRune(r)
			}
		case r == ' ' && !inQuote:
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}

	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	return tokens
}
