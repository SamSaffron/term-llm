package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ApprovalCache provides session-scoped caching for tool+path decisions.
type ApprovalCache struct {
	mu    sync.RWMutex
	cache map[string]ConfirmOutcome
}

// NewApprovalCache creates a new ApprovalCache.
func NewApprovalCache() *ApprovalCache {
	return &ApprovalCache{
		cache: make(map[string]ConfirmOutcome),
	}
}

// cacheKey generates a unique key for a tool+path combination.
func cacheKey(toolName, path string) string {
	h := sha256.New()
	h.Write([]byte(toolName))
	h.Write([]byte{0}) // separator
	h.Write([]byte(path))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// Get retrieves a cached approval decision.
func (c *ApprovalCache) Get(toolName, path string) (ConfirmOutcome, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	outcome, ok := c.cache[cacheKey(toolName, path)]
	return outcome, ok
}

// Set stores an approval decision.
func (c *ApprovalCache) Set(toolName, path string, outcome ConfirmOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[cacheKey(toolName, path)] = outcome
}

// SetForDirectory stores an approval for all paths under a directory.
// This is used when user approves "always" for a directory.
func (c *ApprovalCache) SetForDirectory(toolName, dir string, outcome ConfirmOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Store with a special directory marker
	c.cache[cacheKey(toolName, "dir:"+dir)] = outcome
}

// GetForDirectory checks if there's an approval for a directory.
func (c *ApprovalCache) GetForDirectory(toolName, dir string) (ConfirmOutcome, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	outcome, ok := c.cache[cacheKey(toolName, "dir:"+dir)]
	return outcome, ok
}

// Clear removes all cached approvals.
func (c *ApprovalCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = make(map[string]ConfirmOutcome)
}

// DirCache provides tool-agnostic directory approval caching.
// When a directory is approved, all tools can access files within it.
type DirCache struct {
	mu   sync.RWMutex
	dirs map[string]ConfirmOutcome // absolute dir path -> outcome
}

// NewDirCache creates a new DirCache.
func NewDirCache() *DirCache {
	return &DirCache{
		dirs: make(map[string]ConfirmOutcome),
	}
}

// Get checks if a directory is approved.
func (c *DirCache) Get(dir string) (ConfirmOutcome, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	outcome, ok := c.dirs[dir]
	return outcome, ok
}

// Set stores a directory approval.
func (c *DirCache) Set(dir string, outcome ConfirmOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dirs[dir] = outcome
}

// IsPathInApprovedDir checks if a path is within any approved directory.
func (c *DirCache) IsPathInApprovedDir(path string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	for dir, outcome := range c.dirs {
		if outcome == ProceedAlways || outcome == ProceedAlwaysAndSave {
			if strings.HasPrefix(absPath, dir+string(filepath.Separator)) || absPath == dir {
				return true
			}
		}
	}
	return false
}

// ShellApprovalCache caches shell command pattern approvals for the session.
type ShellApprovalCache struct {
	mu       sync.RWMutex
	patterns []string // Patterns approved during this session
}

// NewShellApprovalCache creates a new ShellApprovalCache.
func NewShellApprovalCache() *ShellApprovalCache {
	return &ShellApprovalCache{
		patterns: []string{},
	}
}

// AddPattern adds a pattern to the session cache.
func (c *ShellApprovalCache) AddPattern(pattern string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Avoid duplicates
	for _, p := range c.patterns {
		if p == pattern {
			return
		}
	}
	c.patterns = append(c.patterns, pattern)
}

// GetPatterns returns all session-approved patterns.
func (c *ShellApprovalCache) GetPatterns() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]string, len(c.patterns))
	copy(result, c.patterns)
	return result
}

// ApprovalRequest represents a pending approval request.
type ApprovalRequest struct {
	ToolName    string
	Path        string   // For file tools
	Command     string   // For shell tool
	Description string   // Human-readable description
	Options     []string // Directory options for file tools
	ToolInfo    string   // Preview info for display (filename, URL, etc.)

	// Callbacks
	OnApprove func(choice string, saveToConfig bool) // choice is dir path or pattern
	OnDeny    func()
}

// ApprovalManager coordinates approval requests and caching.
type ApprovalManager struct {
	cache       *ApprovalCache
	dirCache    *DirCache // Tool-agnostic directory approvals
	shellCache  *ShellApprovalCache
	permissions *ToolPermissions

	// Callback for prompting user (set by TUI or CLI)
	PromptFunc func(req *ApprovalRequest) (ConfirmOutcome, string)
}

// NewApprovalManager creates a new ApprovalManager.
func NewApprovalManager(perms *ToolPermissions) *ApprovalManager {
	return &ApprovalManager{
		cache:       NewApprovalCache(),
		dirCache:    NewDirCache(),
		shellCache:  NewShellApprovalCache(),
		permissions: perms,
	}
}

// CheckPathApproval checks if a path is approved for the given tool.
// Approvals are directory-scoped and tool-agnostic - approving a directory
// for one tool allows all tools to access files within it.
// toolInfo is optional context for display (e.g., filename being accessed).
func (m *ApprovalManager) CheckPathApproval(toolName, path, toolInfo string, isWrite bool) (ConfirmOutcome, error) {
	// 1. Check pre-approved allowlist first (--read-dir / --write-dir flags)
	var allowed bool
	var err error

	if isWrite {
		allowed, err = m.permissions.IsPathAllowedForWrite(path)
	} else {
		allowed, err = m.permissions.IsPathAllowedForRead(path)
	}

	if err != nil {
		return Cancel, err
	}

	if allowed {
		return ProceedOnce, nil
	}

	// 2. Check if path is in any approved directory (tool-agnostic)
	if m.dirCache.IsPathInApprovedDir(path) {
		return ProceedAlways, nil
	}

	// 3. Determine which directory to approve
	dir := getDirectoryForApproval(path)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return Cancel, NewToolError(ErrPermissionDenied, "invalid path")
	}

	// 4. Prompt user for directory approval
	if m.PromptFunc == nil {
		return Cancel, NewToolError(ErrPermissionDenied, "path not in allowlist and no TTY for approval")
	}

	actionType := "read"
	if isWrite {
		actionType = "write"
	}

	req := &ApprovalRequest{
		ToolName:    toolName,
		Path:        absDir,
		Description: fmt.Sprintf("Allow %s access to directory: %s", actionType, absDir),
		ToolInfo:    toolInfo,
	}

	outcome, _ := m.PromptFunc(req)

	if outcome == ProceedAlways || outcome == ProceedAlwaysAndSave {
		m.dirCache.Set(absDir, outcome)
	}

	return outcome, nil
}

// getDirectoryForApproval determines which directory to ask approval for.
func getDirectoryForApproval(path string) string {
	// If it's a directory, use it directly
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		return path
	}

	// Otherwise, use the parent directory
	return filepath.Dir(path)
}

// CheckShellApproval checks if a shell command is approved.
func (m *ApprovalManager) CheckShellApproval(command string) (ConfirmOutcome, error) {
	// Check pre-approved patterns
	if m.permissions.IsShellCommandAllowed(command) {
		return ProceedOnce, nil
	}

	// Check session-approved patterns
	for _, pattern := range m.shellCache.GetPatterns() {
		if matchPattern(pattern, command) {
			return ProceedAlways, nil
		}
	}

	// Need to prompt
	if m.PromptFunc == nil {
		return Cancel, NewToolError(ErrPermissionDenied, "command not in allowlist and no TTY for approval")
	}

	req := &ApprovalRequest{
		ToolName:    ShellToolName,
		Command:     command,
		Description: fmt.Sprintf("Allow shell command: %s", command),
		ToolInfo:    command,
	}

	outcome, pattern := m.PromptFunc(req)

	if outcome == ProceedAlways || outcome == ProceedAlwaysAndSave {
		// Cache the command or pattern for future use
		if pattern != "" {
			m.shellCache.AddPattern(pattern)
		} else {
			m.shellCache.AddPattern(command)
		}
	}

	return outcome, nil
}

// truncateForDisplay shortens a string for display purposes.
func truncateForDisplay(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// ApproveShellPattern adds a pattern to the session cache.
func (m *ApprovalManager) ApproveShellPattern(pattern string) {
	m.shellCache.AddPattern(pattern)
}

// ApprovePath adds a path/directory approval to the session cache.
func (m *ApprovalManager) ApprovePath(toolName, path string, outcome ConfirmOutcome) {
	m.cache.Set(toolName, path, outcome)
}

// ApproveDirectory adds a directory approval to the session cache.
func (m *ApprovalManager) ApproveDirectory(toolName, dir string, outcome ConfirmOutcome) {
	m.cache.SetForDirectory(toolName, dir, outcome)
}

// matchPattern checks if a command matches a glob pattern.
func matchPattern(pattern, command string) bool {
	// Simple glob matching for shell patterns
	// Patterns like "git *" or "npm test"
	if len(pattern) == 0 {
		return false
	}

	// Handle trailing wildcard
	if pattern[len(pattern)-1] == '*' {
		prefix := pattern[:len(pattern)-1]
		return len(command) >= len(prefix) && command[:len(prefix)] == prefix
	}

	return pattern == command
}
