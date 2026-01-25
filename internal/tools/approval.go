package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/term"
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
	cache        *ApprovalCache
	dirCache     *DirCache // Tool-agnostic directory approvals
	shellCache   *ShellApprovalCache
	permissions  *ToolPermissions
	projectCache map[string]*ProjectApprovals // repo root -> approvals
	projectMu    sync.Mutex

	// promptMu serializes interactive approval prompts.
	// When tools execute in parallel, multiple may need approval simultaneously.
	// This mutex ensures only one prompt is shown at a time to avoid UI conflicts.
	promptMu sync.Mutex

	// YoloMode when true, auto-approves all tool executions without prompting.
	// Intended for CI/container environments where interactive approval isn't possible.
	YoloMode bool

	// Callback for prompting user (set by TUI or CLI)
	// Legacy callback - will be replaced by PromptUIFunc
	PromptFunc func(req *ApprovalRequest) (ConfirmOutcome, string)

	// New UI callback for improved approval prompts
	// Takes path/command, isWrite (for files), and returns ApprovalResult
	// If nil, falls back to PromptFunc
	PromptUIFunc func(path string, isWrite bool, isShell bool) (ApprovalResult, error)

	// Parent manager for inheriting session approvals and prompt function.
	// When set, this manager will check parent's caches and use parent's
	// PromptUIFunc if local is nil. This enables sub-agents to inherit
	// the parent session's approvals and prompting capability.
	parent *ApprovalManager
}

// NewApprovalManager creates a new ApprovalManager.
func NewApprovalManager(perms *ToolPermissions) *ApprovalManager {
	return &ApprovalManager{
		cache:        NewApprovalCache(),
		dirCache:     NewDirCache(),
		shellCache:   NewShellApprovalCache(),
		permissions:  perms,
		projectCache: make(map[string]*ProjectApprovals),
	}
}

// SetParent sets the parent ApprovalManager for inheritance.
// When set, this manager will check parent's session caches (dirCache, shellCache)
// and use parent's PromptUIFunc if local is nil.
// Also shares the parent's promptMu to serialize prompts across all sub-agents.
// Returns an error if setting parent would create a cycle.
func (m *ApprovalManager) SetParent(parent *ApprovalManager) error {
	// Check for self-reference
	if parent == m {
		return fmt.Errorf("cannot set approval manager as its own parent")
	}

	// Check for cycles by walking up the parent chain
	for p := parent; p != nil; p = p.parent {
		if p == m {
			return fmt.Errorf("cannot set parent: would create a cycle")
		}
	}

	m.parent = parent
	return nil
}

// PromptLock returns the mutex used to serialize prompts.
// When a parent is set, returns the parent's lock to ensure all
// sub-agents share the same serialization.
func (m *ApprovalManager) PromptLock() *sync.Mutex {
	if m.parent != nil {
		return m.parent.PromptLock()
	}
	return &m.promptMu
}

// SetYoloMode enables or disables yolo mode and prints a warning when enabled.
// Yolo mode auto-approves all tool executions without prompting.
func (m *ApprovalManager) SetYoloMode(enabled bool) {
	m.YoloMode = enabled
	if enabled && term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintf(os.Stderr, "⚠️  WARNING: Yolo mode enabled - all tool operations will be auto-approved without prompting.\n")
		fmt.Fprintf(os.Stderr, "   This includes shell commands and file modifications. Use only in trusted environments.\n")
	}
}

// getProjectApprovals returns or loads project approvals for the given path.
func (m *ApprovalManager) getProjectApprovals(path string) *ProjectApprovals {
	repoInfo := DetectGitRepo(path)
	if !repoInfo.IsRepo {
		return nil
	}

	m.projectMu.Lock()
	defer m.projectMu.Unlock()

	if pa, ok := m.projectCache[repoInfo.Root]; ok {
		return pa
	}

	pa, err := LoadProjectApprovals(repoInfo.Root)
	if err != nil {
		return nil
	}

	m.projectCache[repoInfo.Root] = pa
	return pa
}

// CheckPathApproval checks if a path is approved for the given tool.
// Approvals are directory-scoped and tool-agnostic - approving a directory
// for one tool allows all tools to access files within it.
// toolInfo is optional context for display (e.g., filename being accessed).
func (m *ApprovalManager) CheckPathApproval(toolName, path, toolInfo string, isWrite bool) (ConfirmOutcome, error) {
	// 0. Yolo mode - auto-approve everything
	if m.YoloMode {
		return ProceedOnce, nil
	}

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

	// 2. Check if path is in any approved directory (session cache, tool-agnostic)
	if m.dirCache.IsPathInApprovedDir(path) {
		return ProceedAlways, nil
	}

	// 2a. Check parent's session cache (inherited approvals)
	if m.parent != nil && m.parent.dirCache.IsPathInApprovedDir(path) {
		return ProceedAlways, nil
	}

	// 2b. Check parent's tool+path specific cache (inherited approvals)
	if m.parent != nil {
		if outcome, ok := m.parent.cache.Get(toolName, path); ok {
			return outcome, nil
		}
	}

	// 3. Check project-level approvals (persisted)
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}

	projectApprovals := m.getProjectApprovals(absPath)
	if projectApprovals != nil && projectApprovals.IsPathApproved(absPath, isWrite) {
		return ProceedAlways, nil
	}

	// 4. Need to prompt user - serialize prompts to avoid UI conflicts
	// Use shared lock (via PromptLock()) to prevent concurrent prompts across parent/child managers
	promptLock := m.PromptLock()
	promptLock.Lock()
	defer promptLock.Unlock()

	// Try new UI first (local, then parent), then fall back to legacy
	promptUIFunc := m.PromptUIFunc
	if promptUIFunc == nil && m.parent != nil {
		promptUIFunc = m.parent.PromptUIFunc
	}
	if promptUIFunc != nil {
		result, err := promptUIFunc(absPath, isWrite, false)
		if err != nil {
			return Cancel, err
		}
		return m.handleFileApprovalResult(result, absPath, isWrite, projectApprovals)
	}

	// Fall back to legacy PromptFunc (local, then parent)
	promptFunc := m.PromptFunc
	if promptFunc == nil && m.parent != nil {
		promptFunc = m.parent.PromptFunc
	}
	if promptFunc == nil {
		return Cancel, NewToolError(ErrPermissionDenied, "path not in allowlist and no TTY for approval")
	}

	dir := getDirectoryForApproval(path)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return Cancel, NewToolError(ErrPermissionDenied, "invalid path")
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

	outcome, _ := promptFunc(req)

	if outcome == ProceedAlways || outcome == ProceedAlwaysAndSave {
		m.dirCache.Set(absDir, outcome)
	}

	return outcome, nil
}

// handleFileApprovalResult processes the result from the approval UI.
func (m *ApprovalManager) handleFileApprovalResult(result ApprovalResult, path string, isWrite bool, projectApprovals *ProjectApprovals) (ConfirmOutcome, error) {
	if result.Cancelled {
		return Cancel, nil
	}

	switch result.Choice {
	case ApprovalChoiceDeny:
		return Cancel, nil

	case ApprovalChoiceOnce:
		return ProceedOnce, nil

	case ApprovalChoiceFile:
		// Session-only file approval - just proceed once
		return ProceedOnce, nil

	case ApprovalChoiceDirectory:
		// Session-only directory approval
		absDir, err := filepath.Abs(result.Path)
		if err != nil {
			absDir = result.Path
		}
		m.dirCache.Set(absDir, ProceedAlways)
		return ProceedAlways, nil

	case ApprovalChoiceRepoRead:
		// Approve read for entire repo (persisted)
		if projectApprovals != nil {
			if err := projectApprovals.ApproveRead(); err != nil {
				// Log error but don't fail - still allow access for this session
			}
		}
		// Also add to session cache for fast lookups
		if result.Path != "" {
			m.dirCache.Set(result.Path, ProceedAlways)
		}
		return ProceedAlways, nil

	case ApprovalChoiceRepoWrite:
		// Approve write for entire repo (persisted)
		if projectApprovals != nil {
			if err := projectApprovals.ApproveWrite(); err != nil {
				// Log error but don't fail
			}
		}
		// Also add to session cache
		if result.Path != "" {
			m.dirCache.Set(result.Path, ProceedAlways)
		}
		return ProceedAlways, nil

	default:
		return Cancel, nil
	}
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
	// Yolo mode - auto-approve everything
	if m.YoloMode {
		return ProceedOnce, nil
	}

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

	// Check parent's session-approved patterns (inherited approvals)
	if m.parent != nil {
		for _, pattern := range m.parent.shellCache.GetPatterns() {
			if matchPattern(pattern, command) {
				return ProceedAlways, nil
			}
		}
	}

	// Check project-level approvals (persisted)
	cwd, _ := os.Getwd()
	projectApprovals := m.getProjectApprovals(cwd)
	if projectApprovals != nil && projectApprovals.IsShellPatternApproved(command) {
		return ProceedAlways, nil
	}

	// Need to prompt - serialize prompts to avoid UI conflicts
	// Use shared lock (via PromptLock()) to prevent concurrent prompts across parent/child managers
	promptLock := m.PromptLock()
	promptLock.Lock()
	defer promptLock.Unlock()

	// Try new UI first (local, then parent), then fall back to legacy
	promptUIFunc := m.PromptUIFunc
	if promptUIFunc == nil && m.parent != nil {
		promptUIFunc = m.parent.PromptUIFunc
	}
	if promptUIFunc != nil {
		result, err := promptUIFunc(command, false, true)
		if err != nil {
			return Cancel, err
		}
		return m.handleShellApprovalResult(result, command, projectApprovals)
	}

	// Fall back to legacy PromptFunc (local, then parent)
	promptFunc := m.PromptFunc
	if promptFunc == nil && m.parent != nil {
		promptFunc = m.parent.PromptFunc
	}
	if promptFunc == nil {
		return Cancel, NewToolError(ErrPermissionDenied, "command not in allowlist and no TTY for approval")
	}

	req := &ApprovalRequest{
		ToolName:    ShellToolName,
		Command:     command,
		Description: fmt.Sprintf("Allow shell command: %s", command),
		ToolInfo:    command,
	}

	outcome, pattern := promptFunc(req)

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

// handleShellApprovalResult processes the result from the shell approval UI.
func (m *ApprovalManager) handleShellApprovalResult(result ApprovalResult, command string, projectApprovals *ProjectApprovals) (ConfirmOutcome, error) {
	if result.Cancelled {
		return Cancel, nil
	}

	switch result.Choice {
	case ApprovalChoiceDeny:
		return Cancel, nil

	case ApprovalChoiceOnce:
		return ProceedOnce, nil

	case ApprovalChoiceCommand:
		// Session-only command approval
		m.shellCache.AddPattern(command)
		return ProceedAlways, nil

	case ApprovalChoicePattern:
		// Approve pattern in repo (persisted if in repo, session otherwise)
		pattern := result.Pattern
		if pattern == "" {
			pattern = GenerateShellPattern(command)
		}

		if result.SaveToRepo && projectApprovals != nil {
			if err := projectApprovals.ApproveShellPattern(pattern); err != nil {
				// Log error but don't fail
			}
		}
		// Also add to session cache for fast lookups
		m.shellCache.AddPattern(pattern)
		return ProceedAlways, nil

	default:
		return Cancel, nil
	}
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
