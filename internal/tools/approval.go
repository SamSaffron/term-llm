package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
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
// Read and write approvals are tracked separately: a read approval does
// not grant write access, but a write approval implies read access.
type DirCache struct {
	mu        sync.RWMutex
	readDirs  map[string]ConfirmOutcome // approved for read
	writeDirs map[string]ConfirmOutcome // approved for write
}

// NewDirCache creates a new DirCache.
func NewDirCache() *DirCache {
	return &DirCache{
		readDirs:  make(map[string]ConfirmOutcome),
		writeDirs: make(map[string]ConfirmOutcome),
	}
}

// Set stores a directory approval for the given access type.
func (c *DirCache) Set(dir string, outcome ConfirmOutcome, isWrite bool) {
	if resolved, err := canonicalApprovalPath(dir, isWrite); err == nil {
		dir = resolved
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if isWrite {
		c.writeDirs[dir] = outcome
	} else {
		c.readDirs[dir] = outcome
	}
}

// IsPathInApprovedDir checks if a path is within any approved directory
// for the given access type. Write access requires an explicit write
// approval; read access is satisfied by either a read or write approval.
func (c *DirCache) IsPathInApprovedDir(path string, isWrite bool) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	resolvedPath, err := canonicalApprovalPath(path, isWrite)
	if err != nil {
		return false
	}

	if isWrite {
		return matchApprovedPath(resolvedPath, c.writeDirs)
	}
	// Read: check both read and write approvals
	return matchApprovedPath(resolvedPath, c.readDirs) || matchApprovedPath(resolvedPath, c.writeDirs)
}

func matchApprovedPath(absPath string, dirs map[string]ConfirmOutcome) bool {
	for dir, outcome := range dirs {
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

	// IgnoreProjectApprovals when true, skips persisted project-level approvals
	// (e.g., read_approved/write_approved from prior CLI sessions).
	// Used in serve mode so the web UI user is always prompted.
	IgnoreProjectApprovals bool

	// DebugApproval when true, logs approval decision details to stderr.
	DebugApproval bool

	// Callback for prompting user (set by TUI or CLI)
	// Legacy callback - will be replaced by PromptUIFunc
	PromptFunc func(req *ApprovalRequest) (ConfirmOutcome, string)

	// New UI callback for improved approval prompts.
	// Takes path/command, isWrite (for files), isShell, and workDir
	// (non-empty for shell commands to show where the command will run).
	// If nil, falls back to PromptFunc.
	PromptUIFunc func(path string, isWrite bool, isShell bool, workDir string) (ApprovalResult, error)

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

// SetYoloMode enables or disables yolo mode.
// Yolo mode auto-approves all tool executions without prompting.
func (m *ApprovalManager) SetYoloMode(enabled bool) {
	m.YoloMode = enabled
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

// checkPathApprovalNoPrompt runs the non-interactive approval checks.
// Returns (outcome, true, nil) when a decision is made, or (Cancel, false, nil)
// when prompting is still required.
func (m *ApprovalManager) checkPathApprovalNoPrompt(toolName, path, absPath string, isWrite bool) (ConfirmOutcome, bool, error) {
	// 1. Check pre-approved allowlist first (--read-dir / --write-dir flags)
	var allowed bool
	var err error

	if isWrite {
		allowed, err = m.permissions.IsPathAllowedForWrite(path)
	} else {
		allowed, err = m.permissions.IsPathAllowedForRead(path)
	}

	if err != nil {
		if m.DebugApproval {
			log.Printf("[approval]   allowlist check error for %q: %v", path, err)
		}
		return Cancel, true, err
	}

	if allowed {
		if m.DebugApproval {
			log.Printf("[approval]   allowlist approved %q (isWrite=%v)", path, isWrite)
		}
		return ProceedOnce, true, nil
	}

	// 2. Check if path is in any approved directory (session cache, tool-agnostic)
	if m.dirCache.IsPathInApprovedDir(path, isWrite) {
		if m.DebugApproval {
			log.Printf("[approval]   dirCache approved %q (isWrite=%v)", path, isWrite)
		}
		return ProceedAlways, true, nil
	}

	// 2a. Check parent's session cache (inherited approvals)
	if m.parent != nil && m.parent.dirCache.IsPathInApprovedDir(path, isWrite) {
		if m.DebugApproval {
			log.Printf("[approval]   parent dirCache approved %q (isWrite=%v)", path, isWrite)
		}
		return ProceedAlways, true, nil
	}

	// 2b. Check parent's tool+path specific cache (inherited approvals)
	if m.parent != nil {
		if outcome, ok := m.parent.cache.Get(toolName, path); ok {
			if m.DebugApproval {
				log.Printf("[approval]   parent cache approved %q: %v", path, outcome)
			}
			return outcome, true, nil
		}
	}

	// 3. Check project-level approvals (persisted)
	if !m.IgnoreProjectApprovals {
		if absPath == "" {
			absPath = path
			if resolved, err := filepath.Abs(path); err == nil {
				absPath = resolved
			}
		}

		projectApprovals := m.getProjectApprovals(absPath)
		if projectApprovals != nil && projectApprovals.IsPathApproved(absPath, isWrite) {
			if m.DebugApproval {
				log.Printf("[approval]   project approvals approved %q (isWrite=%v)", absPath, isWrite)
			}
			return ProceedAlways, true, nil
		}
	}

	return Cancel, false, nil
}

// checkShellApprovalNoPrompt runs the non-interactive shell approval checks.
// workDir is the directory where the command will execute (empty = cwd).
// Returns (outcome, true) when a decision is made, or (Cancel, false) when
// prompting is still required.
func (m *ApprovalManager) checkShellApprovalNoPrompt(command, workDir string) (ConfirmOutcome, bool) {
	// Check pre-approved patterns
	if m.permissions.IsShellCommandAllowed(command) {
		return ProceedOnce, true
	}

	// Check session-approved patterns
	for _, pattern := range m.shellCache.GetPatterns() {
		if matchPattern(pattern, command) {
			return ProceedAlways, true
		}
	}

	// Check parent's session-approved patterns (inherited approvals)
	if m.parent != nil {
		for _, pattern := range m.parent.shellCache.GetPatterns() {
			if matchPattern(pattern, command) {
				return ProceedAlways, true
			}
		}
	}

	// Check project-level approvals (persisted) — use the command's
	// working directory so approvals attach to the correct repo.
	if !m.IgnoreProjectApprovals {
		dir := workDir
		if dir == "" {
			dir, _ = os.Getwd()
		}
		projectApprovals := m.getProjectApprovals(dir)
		if projectApprovals != nil && projectApprovals.IsShellPatternApproved(command) {
			return ProceedAlways, true
		}
	}

	return Cancel, false
}

// CheckPathApproval checks if a path is approved for the given tool.
// Approvals are directory-scoped and tool-agnostic - approving a directory
// for one tool allows all tools to access files within it.
// toolInfo is optional context for display (e.g., filename being accessed).
func (m *ApprovalManager) CheckPathApproval(toolName, path, toolInfo string, isWrite bool) (ConfirmOutcome, error) {
	// 0. Yolo mode - auto-approve everything
	if m.YoloMode {
		if m.DebugApproval {
			log.Printf("[approval] CheckPathApproval tool=%s path=%q isWrite=%v → yolo auto-approve", toolName, path, isWrite)
		}
		return ProceedOnce, nil
	}

	absPath, err := canonicalApprovalPath(path, isWrite)
	if err != nil {
		if m.DebugApproval {
			log.Printf("[approval] CheckPathApproval tool=%s path=%q → canonicalize error: %v", toolName, path, err)
		}
		return Cancel, err
	}
	originalPath := path
	if resolved, err := filepath.Abs(path); err == nil {
		originalPath = resolved
	}

	outcome, ok, err := m.checkPathApprovalNoPrompt(toolName, absPath, absPath, isWrite)
	if err != nil {
		if m.DebugApproval {
			log.Printf("[approval] CheckPathApproval tool=%s path=%q → no-prompt error: %v", toolName, absPath, err)
		}
		return Cancel, err
	}
	if ok {
		if m.DebugApproval {
			log.Printf("[approval] CheckPathApproval tool=%s path=%q → no-prompt decided: %v", toolName, absPath, outcome)
		}
		return outcome, nil
	}
	if originalPath != absPath {
		return Cancel, NewToolErrorf(ErrSymlinkEscape, "path %s resolves to %s which is outside approved directories", originalPath, absPath)
	}

	// 4. Need to prompt user - serialize prompts to avoid UI conflicts
	// Use shared lock (via PromptLock()) to prevent concurrent prompts across parent/child managers
	promptLock := m.PromptLock()
	promptLock.Lock()
	defer promptLock.Unlock()

	// Recheck now that we hold the prompt lock to avoid duplicate prompts
	outcome, ok, err = m.checkPathApprovalNoPrompt(toolName, absPath, absPath, isWrite)
	if err != nil {
		if m.DebugApproval {
			log.Printf("[approval] CheckPathApproval tool=%s path=%q → recheck error: %v", toolName, absPath, err)
		}
		return Cancel, err
	}
	if ok {
		if m.DebugApproval {
			log.Printf("[approval] CheckPathApproval tool=%s path=%q → recheck decided: %v", toolName, absPath, outcome)
		}
		return outcome, nil
	}
	if originalPath != absPath {
		return Cancel, NewToolErrorf(ErrSymlinkEscape, "path %s resolves to %s which is outside approved directories", originalPath, absPath)
	}

	projectApprovals := m.getProjectApprovals(absPath)

	// Try new UI first (local, then parent), then fall back to legacy
	promptUIFunc := m.PromptUIFunc
	if promptUIFunc == nil && m.parent != nil {
		promptUIFunc = m.parent.PromptUIFunc
	}
	if promptUIFunc != nil {
		if m.DebugApproval {
			log.Printf("[approval] CheckPathApproval tool=%s path=%q → calling PromptUIFunc", toolName, absPath)
		}
		result, err := promptUIFunc(absPath, isWrite, false, "")
		if err != nil {
			if m.DebugApproval {
				log.Printf("[approval] CheckPathApproval tool=%s path=%q → PromptUIFunc error: %v", toolName, absPath, err)
			}
			return Cancel, err
		}
		if m.DebugApproval {
			log.Printf("[approval] CheckPathApproval tool=%s path=%q → PromptUIFunc result: choice=%v cancelled=%v", toolName, absPath, result.Choice, result.Cancelled)
		}
		return m.handleFileApprovalResult(result, absPath, isWrite, projectApprovals)
	}

	if m.DebugApproval {
		log.Printf("[approval] CheckPathApproval tool=%s path=%q → no PromptUIFunc or PromptFunc set, denying", toolName, absPath)
	}

	// Fall back to legacy PromptFunc (local, then parent)
	promptFunc := m.PromptFunc
	if promptFunc == nil && m.parent != nil {
		promptFunc = m.parent.PromptFunc
	}
	if promptFunc == nil {
		return Cancel, NewToolError(ErrPermissionDenied, "path not in allowlist and no TTY for approval")
	}

	dir := getDirectoryForApproval(absPath)
	absDir, err := canonicalApprovalPath(dir, isWrite)
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

	outcome, _ = promptFunc(req)

	if outcome == ProceedAlways || outcome == ProceedAlwaysAndSave {
		m.dirCache.Set(absDir, outcome, isWrite)
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
		// Session-only file approval - cache the exact path so repeated
		// accesses to the same file don't re-prompt within this session.
		absFile, err := canonicalApprovalPath(path, isWrite)
		if err != nil {
			absFile = path
		}
		m.dirCache.Set(absFile, ProceedAlways, isWrite)
		return ProceedAlways, nil

	case ApprovalChoiceDirectory:
		// Session-only directory approval
		absDir, err := canonicalApprovalPath(result.Path, isWrite)
		if err != nil {
			absDir = result.Path
		}
		m.dirCache.Set(absDir, ProceedAlways, isWrite)
		return ProceedAlways, nil

	case ApprovalChoiceRepoRead:
		// Approve read for entire repo (persisted)
		if projectApprovals != nil {
			if err := projectApprovals.ApproveRead(); err != nil {
				if m.DebugApproval {
					log.Printf("[approval] failed to persist read approval: %v", err)
				}
			}
		}
		// Also add to session cache for fast lookups (read only)
		if result.Path != "" {
			m.dirCache.Set(result.Path, ProceedAlways, false)
		}
		return ProceedAlways, nil

	case ApprovalChoiceRepoWrite:
		// Approve write for entire repo (persisted)
		if projectApprovals != nil {
			if err := projectApprovals.ApproveWrite(); err != nil {
				if m.DebugApproval {
					log.Printf("[approval] failed to persist write approval: %v", err)
				}
			}
		}
		// Also add to session cache (write)
		if result.Path != "" {
			m.dirCache.Set(result.Path, ProceedAlways, true)
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
// workDir is the directory where the command will execute (may be empty for cwd).
func (m *ApprovalManager) CheckShellApproval(command, workDir string) (ConfirmOutcome, error) {
	// Yolo mode - auto-approve everything
	if m.YoloMode {
		if m.DebugApproval {
			log.Printf("[approval] CheckShellApproval cmd=%q → yolo auto-approve", command)
		}
		return ProceedOnce, nil
	}

	if outcome, ok := m.checkShellApprovalNoPrompt(command, workDir); ok {
		if m.DebugApproval {
			log.Printf("[approval] CheckShellApproval cmd=%q → no-prompt decided: %v", command, outcome)
		}
		return outcome, nil
	}

	// Need to prompt - serialize prompts to avoid UI conflicts
	// Use shared lock (via PromptLock()) to prevent concurrent prompts across parent/child managers
	promptLock := m.PromptLock()
	promptLock.Lock()
	defer promptLock.Unlock()

	// Recheck now that we hold the prompt lock to avoid duplicate prompts
	if outcome, ok := m.checkShellApprovalNoPrompt(command, workDir); ok {
		if m.DebugApproval {
			log.Printf("[approval] CheckShellApproval cmd=%q → recheck decided: %v", command, outcome)
		}
		return outcome, nil
	}

	// Use the command's working directory for project approval lookup
	// so remembered approvals attach to the correct repo.
	dir := workDir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	projectApprovals := m.getProjectApprovals(dir)

	// Try new UI first (local, then parent), then fall back to legacy
	promptUIFunc := m.PromptUIFunc
	if promptUIFunc == nil && m.parent != nil {
		promptUIFunc = m.parent.PromptUIFunc
	}
	if promptUIFunc != nil {
		if m.DebugApproval {
			log.Printf("[approval] CheckShellApproval cmd=%q → calling PromptUIFunc", command)
		}
		result, err := promptUIFunc(command, false, true, workDir)
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
				if m.DebugApproval {
					log.Printf("[approval] failed to persist shell pattern %q: %v", pattern, err)
				}
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
	if resolved, err := canonicalApprovalPath(path, false); err == nil {
		path = resolved
	}
	m.cache.Set(toolName, path, outcome)
}

// ApproveDirectory adds a directory approval to the session cache.
func (m *ApprovalManager) ApproveDirectory(toolName, dir string, outcome ConfirmOutcome) {
	if resolved, err := canonicalApprovalPath(dir, false); err == nil {
		dir = resolved
	}
	m.cache.SetForDirectory(toolName, dir, outcome)
}

// safePipeTargets lists commands that only read/filter/format stdin and are
// safe to auto-approve as pipe targets without separate pattern matching.
var safePipeTargets = map[string]bool{
	"head": true, "tail": true, "grep": true, "egrep": true, "fgrep": true,
	"sort": true, "uniq": true, "wc": true, "cat": true, "less": true,
	"more": true, "cut": true, "tr": true, "column": true, "fmt": true,
	"fold": true, "nl": true, "paste": true, "rev": true, "tac": true,
	"jq": true, "yq": true,
}

// isSafePipeTarget checks if a command string is a safe pipe target
// by extracting its first word and checking against the safe set.
func isSafePipeTarget(command string) bool {
	words, err := splitShellWords(command)
	if err != nil || len(words) == 0 {
		return false
	}
	return safePipeTargets[filepath.Base(words[0])]
}

// matchPattern checks if a command matches a glob pattern.
// For compound commands, it uses two-level decomposition:
//  1. Split on sequential operators (&&, ||, ;) — each segment must match.
//  2. Within each segment, split on pipes (|) — the first command must match
//     the pattern, and remaining pipe targets must match OR be safe pipe targets.
func matchPattern(pattern, command string) bool {
	if matchPatternSingle(pattern, command) {
		return true
	}
	if !hasUnsafeShellSyntax(command) || hasUnsafeShellSyntax(pattern) {
		return false
	}
	// Level 1: split on sequential operators (&&, ||, ;).
	seqParts := splitSequentialCommands(command)
	if len(seqParts) < 1 {
		return false
	}
	for _, seqPart := range seqParts {
		if !matchPipeChain(pattern, seqPart) {
			return false
		}
	}
	return true
}

// matchPipeChain checks if a pipe chain matches a pattern.
// The first command must match the pattern. Subsequent pipe targets
// must either match the pattern or be safe pipe targets.
func matchPipeChain(pattern, pipeChain string) bool {
	pipeParts := splitPipeCommands(pipeChain)
	if len(pipeParts) == 0 {
		return false
	}
	if !matchPatternSingle(pattern, pipeParts[0]) {
		return false
	}
	for _, pipePart := range pipeParts[1:] {
		if !matchPatternSingle(pattern, pipePart) && !isSafePipeTarget(pipePart) {
			return false
		}
	}
	return true
}

// matchPatternSingle checks if a single command (no shell operators) matches
// a glob pattern. Does not attempt decomposition.
func matchPatternSingle(pattern, command string) bool {
	if pattern == "" {
		return false
	}
	if pattern == command {
		return true
	}
	if hasUnsafeShellSyntax(pattern) || hasUnsafeShellSyntax(command) {
		return false
	}

	patternParts, err := splitShellWords(pattern)
	if err != nil {
		return false
	}
	commandParts, err := splitShellWords(command)
	if err != nil {
		return false
	}

	if len(patternParts) == 0 {
		return false
	}

	wildcard := patternParts[len(patternParts)-1] == "*"
	checkParts := patternParts
	if wildcard {
		checkParts = patternParts[:len(patternParts)-1]
		if len(commandParts) < len(checkParts) {
			return false
		}
	} else if len(patternParts) != len(commandParts) {
		return false
	}

	for i, part := range checkParts {
		if i >= len(commandParts) || !matchShellPattern(part, commandParts[i]) {
			return false
		}
	}

	return wildcard || len(commandParts) == len(checkParts)
}

// splitShellCommands splits a shell command string on unquoted operators
// (&&, ||, ;, |) into individual sub-command strings. Tracks quoting state
// (single quotes, double quotes, backslash escapes) to avoid splitting inside
// quoted arguments.
func splitShellCommands(input string) []string {
	var commands []string
	var current strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	runes := []rune(input)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case inSingle:
			current.WriteRune(r)
			if r == '\'' {
				inSingle = false
			}
		case inDouble:
			current.WriteRune(r)
			switch r {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
			}
		default:
			switch {
			case r == '\'':
				inSingle = true
				current.WriteRune(r)
			case r == '"':
				inDouble = true
				current.WriteRune(r)
			case r == '\\':
				escaped = true
				current.WriteRune(r)
			case r == ';':
				if s := strings.TrimSpace(current.String()); s != "" {
					commands = append(commands, s)
				}
				current.Reset()
			case r == '&' && i+1 < len(runes) && runes[i+1] == '&':
				if s := strings.TrimSpace(current.String()); s != "" {
					commands = append(commands, s)
				}
				current.Reset()
				i++ // skip second &
			case r == '|' && i+1 < len(runes) && runes[i+1] == '|':
				if s := strings.TrimSpace(current.String()); s != "" {
					commands = append(commands, s)
				}
				current.Reset()
				i++ // skip second |
			case r == '|':
				if s := strings.TrimSpace(current.String()); s != "" {
					commands = append(commands, s)
				}
				current.Reset()
			default:
				current.WriteRune(r)
			}
		}
	}
	if s := strings.TrimSpace(current.String()); s != "" {
		commands = append(commands, s)
	}
	return commands
}

// splitSequentialCommands splits on sequential operators (&&, ||, ;) but
// NOT on pipes (|). This preserves pipe chains as single units.
func splitSequentialCommands(input string) []string {
	var commands []string
	var current strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	runes := []rune(input)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case inSingle:
			current.WriteRune(r)
			if r == '\'' {
				inSingle = false
			}
		case inDouble:
			current.WriteRune(r)
			switch r {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
			}
		default:
			switch {
			case r == '\'':
				inSingle = true
				current.WriteRune(r)
			case r == '"':
				inDouble = true
				current.WriteRune(r)
			case r == '\\':
				escaped = true
				current.WriteRune(r)
			case r == ';':
				if s := strings.TrimSpace(current.String()); s != "" {
					commands = append(commands, s)
				}
				current.Reset()
			case r == '&' && i+1 < len(runes) && runes[i+1] == '&':
				if s := strings.TrimSpace(current.String()); s != "" {
					commands = append(commands, s)
				}
				current.Reset()
				i++ // skip second &
			case r == '|' && i+1 < len(runes) && runes[i+1] == '|':
				if s := strings.TrimSpace(current.String()); s != "" {
					commands = append(commands, s)
				}
				current.Reset()
				i++ // skip second |
			default:
				current.WriteRune(r)
			}
		}
	}
	if s := strings.TrimSpace(current.String()); s != "" {
		commands = append(commands, s)
	}
	return commands
}

// splitPipeCommands splits on single pipe (|) but NOT on ||, &&, or ;.
func splitPipeCommands(input string) []string {
	var commands []string
	var current strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	runes := []rune(input)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case inSingle:
			current.WriteRune(r)
			if r == '\'' {
				inSingle = false
			}
		case inDouble:
			current.WriteRune(r)
			switch r {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
			}
		default:
			switch {
			case r == '\'':
				inSingle = true
				current.WriteRune(r)
			case r == '"':
				inDouble = true
				current.WriteRune(r)
			case r == '\\':
				escaped = true
				current.WriteRune(r)
			case r == '|' && i+1 < len(runes) && runes[i+1] == '|':
				// || is a sequential operator, preserve it
				current.WriteRune(r)
				current.WriteRune(runes[i+1])
				i++ // skip second |
			case r == '|':
				if s := strings.TrimSpace(current.String()); s != "" {
					commands = append(commands, s)
				}
				current.Reset()
			default:
				current.WriteRune(r)
			}
		}
	}
	if s := strings.TrimSpace(current.String()); s != "" {
		commands = append(commands, s)
	}
	return commands
}

func canonicalApprovalPath(path string, isWrite bool) (string, error) {
	if isWrite {
		return canonicalizePathForWrite(path)
	}
	return canonicalizePath(path)
}
