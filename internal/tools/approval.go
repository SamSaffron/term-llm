package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
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

func (c *DirCache) Snapshot() (readDirs []string, writeDirs []string) {
	if c == nil {
		return nil, nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for dir, outcome := range c.readDirs {
		if outcome == ProceedAlways || outcome == ProceedAlwaysAndSave {
			readDirs = append(readDirs, dir)
		}
	}
	for dir, outcome := range c.writeDirs {
		if outcome == ProceedAlways || outcome == ProceedAlwaysAndSave {
			writeDirs = append(writeDirs, dir)
		}
	}
	sort.Strings(readDirs)
	sort.Strings(writeDirs)
	return readDirs, writeDirs
}

type shellCommandApproval struct {
	command string
	workDir string
}

// ShellApprovalCache caches exact shell commands and glob pattern approvals for
// the session.
type ShellApprovalCache struct {
	mu       sync.RWMutex
	patterns []string               // Glob patterns approved during this session
	commands []shellCommandApproval // Exact command/workdir pairs approved during this session
}

// NewShellApprovalCache creates a new ShellApprovalCache.
func NewShellApprovalCache() *ShellApprovalCache {
	return &ShellApprovalCache{
		patterns: []string{},
		commands: []shellCommandApproval{},
	}
}

// AddPattern adds a validated glob pattern to the session cache.
func (c *ShellApprovalCache) AddPattern(pattern string) error {
	if err := validateShellApprovalPattern(pattern); err != nil {
		return NewToolErrorf(ErrInvalidParams, "invalid shell pattern %q: %v", pattern, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Avoid duplicates
	for _, p := range c.patterns {
		if p == pattern {
			return nil
		}
	}
	c.patterns = append(c.patterns, pattern)
	return nil
}

// AddCommand adds an exact command/workdir pair to the session cache. Exact
// commands are kept separate from glob patterns so shell syntax and glob
// metacharacters are compared literally.
func (c *ShellApprovalCache) AddCommand(command, workDir string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return NewToolError(ErrInvalidParams, "shell command is required")
	}
	approval := shellCommandApproval{
		command: command,
		workDir: normalizeGuardianWorkDir(workDir),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, existing := range c.commands {
		if existing == approval {
			return nil
		}
	}
	c.commands = append(c.commands, approval)
	return nil
}

// IsCommandApproved reports whether command has an exact session approval in
// workDir.
func (c *ShellApprovalCache) IsCommandApproved(command, workDir string) bool {
	approval := shellCommandApproval{
		command: strings.TrimSpace(command),
		workDir: normalizeGuardianWorkDir(workDir),
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, existing := range c.commands {
		if existing == approval {
			return true
		}
	}
	return false
}

// GetPatterns returns all session-approved patterns.
func (c *ShellApprovalCache) GetPatterns() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]string, len(c.patterns))
	copy(result, c.patterns)
	return result
}

// getCommands returns all exact command approvals for the session.
func (c *ShellApprovalCache) getCommands() []shellCommandApproval {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]shellCommandApproval(nil), c.commands...)
}

// ApprovalMode controls how unmatched tool approval requests are handled.
type ApprovalMode int

const (
	ModePrompt ApprovalMode = iota
	ModeAuto
	ModeYolo
)

func (m ApprovalMode) String() string {
	switch m {
	case ModeAuto:
		return "auto"
	case ModeYolo:
		return "yolo"
	default:
		return "prompt"
	}
}

// TranscriptEntry is compact conversation evidence for policy review.
type TranscriptEntry struct {
	Role string
	Text string
}

// PolicyReviewRequest describes a shell action requiring guardian review.
type PolicyReviewRequest struct {
	Command         string
	WorkDir         string
	Transcript      []TranscriptEntry
	ApprovalContext string
	ScopeID         string
}

// PolicyDecision is the guardian's allow/deny verdict.
type PolicyDecision struct {
	Allowed           bool
	RiskLevel         string
	UserAuthorization string
	Rationale         string
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

type guardianShellKey struct {
	Command string
	WorkDir string
}

// GuardianOutcome describes the result of an automatic guardian review.
type GuardianOutcome string

const (
	GuardianApproved GuardianOutcome = "approved"
	GuardianDenied   GuardianOutcome = "denied"
	GuardianWarning  GuardianOutcome = "warning"
	GuardianError    GuardianOutcome = "error"
)

// GuardianEvent is a guardian review annotation correlated with the shell tool
// invocation that caused the review.
type GuardianEvent struct {
	ToolCallID string
	Command    string
	WorkDir    string
	Message    string
	Outcome    GuardianOutcome
}

// ApprovalManager coordinates approval requests and caching.
type ApprovalManager struct {
	cache         *ApprovalCache
	dirCache      *DirCache // Tool-agnostic directory approvals
	shellCache    *ShellApprovalCache
	guardianMu    sync.RWMutex
	guardianExact map[guardianShellKey]struct{} // exact shell actions approved by guardian
	permissions   *ToolPermissions
	projectCache  map[string]*ProjectApprovals // repo root -> approvals
	projectMu     sync.Mutex

	toolAllowMu  sync.RWMutex
	toolReadDirs map[string][]string // per-tool read allowlist, e.g. routed view_image uploads

	// promptMu serializes interactive approval prompts.
	// When tools execute in parallel, multiple may need approval simultaneously.
	// This mutex ensures only one prompt is shown at a time to avoid UI conflicts.
	promptMu sync.Mutex

	// Approval mode. Yolo mode auto-approves all tool executions without prompting;
	// auto mode asks a policy reviewer for shell commands after deterministic
	// checks fail and before falling back to a human prompt.
	modeMu                   sync.RWMutex
	mode                     ApprovalMode
	YoloMode                 bool // Deprecated compatibility mirror for ModeYolo.
	autoHeadless             bool
	requireExplicitMutations bool

	guardianConsecutiveDenials  int
	guardianCircuitBreakerLimit int

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

	// PolicyReviewFunc is called in auto mode for shell commands that missed all
	// deterministic approvals. It must fail closed: errors never imply allow.
	PolicyReviewFunc func(ctx context.Context, req PolicyReviewRequest) (PolicyDecision, error)

	// GuardianEventFunc receives structured audit events for auto approvals/denials.
	GuardianEventFunc func(event GuardianEvent)

	// Parent manager for inheriting session approvals and prompt function.
	// When set, this manager will check parent's caches and use parent's
	// PromptUIFunc if local is nil. This enables sub-agents to inherit
	// the parent session's approvals and prompting capability.
	parent *ApprovalManager
}

// NewApprovalManager creates a new ApprovalManager.
func NewApprovalManager(perms *ToolPermissions) *ApprovalManager {
	return &ApprovalManager{
		cache:                       NewApprovalCache(),
		dirCache:                    NewDirCache(),
		shellCache:                  NewShellApprovalCache(),
		guardianExact:               make(map[guardianShellKey]struct{}),
		permissions:                 perms,
		projectCache:                make(map[string]*ProjectApprovals),
		toolReadDirs:                make(map[string][]string),
		guardianCircuitBreakerLimit: 3,
	}
}

// AddToolReadDir adds a per-tool read-only directory allowlist entry. Unlike
// ToolPermissions.ReadDirs, this does not grant access to other read tools.
func (m *ApprovalManager) AddToolReadDir(toolName, dir string) error {
	if m == nil {
		return nil
	}
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return fmt.Errorf("tool name is required")
	}
	abs, err := canonicalizePath(dir)
	if err != nil {
		return err
	}
	m.toolAllowMu.Lock()
	defer m.toolAllowMu.Unlock()
	for _, existing := range m.toolReadDirs[toolName] {
		if existing == abs {
			return nil
		}
	}
	m.toolReadDirs[toolName] = append(m.toolReadDirs[toolName], abs)
	return nil
}

func (m *ApprovalManager) isPathAllowedForToolRead(toolName, path string) bool {
	if m == nil || strings.TrimSpace(toolName) == "" {
		return false
	}
	resolved, err := canonicalizePath(path)
	if err != nil {
		return false
	}
	m.toolAllowMu.RLock()
	dirs := append([]string(nil), m.toolReadDirs[toolName]...)
	m.toolAllowMu.RUnlock()
	for _, dir := range dirs {
		if strings.HasPrefix(resolved, dir+string(filepath.Separator)) || resolved == dir {
			return true
		}
	}
	if m.parent != nil {
		return m.parent.isPathAllowedForToolRead(toolName, path)
	}
	return false
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

func (m *ApprovalManager) root() *ApprovalManager {
	if m == nil {
		return nil
	}
	for m.parent != nil {
		m = m.parent
	}
	return m
}

func (m *ApprovalManager) lookupPromptUIFunc() func(path string, isWrite bool, isShell bool, workDir string) (ApprovalResult, error) {
	for cur := m; cur != nil; cur = cur.parent {
		if cur.PromptUIFunc != nil {
			return cur.PromptUIFunc
		}
	}
	return nil
}

func (m *ApprovalManager) lookupPromptFunc() func(req *ApprovalRequest) (ConfirmOutcome, string) {
	for cur := m; cur != nil; cur = cur.parent {
		if cur.PromptFunc != nil {
			return cur.PromptFunc
		}
	}
	return nil
}

func (m *ApprovalManager) lookupPolicyReviewFunc() func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
	for cur := m; cur != nil; cur = cur.parent {
		if cur.PolicyReviewFunc != nil {
			return cur.PolicyReviewFunc
		}
	}
	return nil
}

func (m *ApprovalManager) lookupGuardianEventFunc() func(event GuardianEvent) {
	for cur := m; cur != nil; cur = cur.parent {
		if cur.GuardianEventFunc != nil {
			return cur.GuardianEventFunc
		}
	}
	return nil
}

// GuardianReviewerAvailable reports whether this manager or an ancestor has a
// policy reviewer installed for auto approval mode.
func (m *ApprovalManager) GuardianReviewerAvailable() bool {
	return m.lookupPolicyReviewFunc() != nil
}

// SetApprovalMode sets the manager's approval mode.
func (m *ApprovalManager) SetApprovalMode(mode ApprovalMode) {
	if m == nil {
		return
	}
	m.modeMu.Lock()
	old := m.mode
	m.mode = mode
	m.YoloMode = mode == ModeYolo
	m.modeMu.Unlock()
	if old == ModeAuto && mode != ModeAuto {
		m.clearGuardianExactShell()
	}
}

// SetRequireExplicitMutations makes every shell execution and write operation
// require a fresh human decision, bypassing yolo, allowlists, and remembered
// approvals. Side conversations use this because they share the parent's
// workspace and can otherwise race it.
func (m *ApprovalManager) SetRequireExplicitMutations(required bool) {
	if m == nil {
		return
	}
	m.modeMu.Lock()
	m.requireExplicitMutations = required
	m.modeMu.Unlock()
}

func (m *ApprovalManager) requiresExplicitMutations() bool {
	if m == nil {
		return false
	}
	m.modeMu.RLock()
	required := m.requireExplicitMutations
	m.modeMu.RUnlock()
	return required
}

// ApprovalMode returns this manager's effective mode, inheriting from parents.
func (m *ApprovalManager) ApprovalMode() ApprovalMode {
	if m == nil {
		return ModePrompt
	}
	m.modeMu.RLock()
	mode := m.mode
	m.modeMu.RUnlock()
	if mode != ModePrompt {
		return mode
	}
	if m.parent != nil {
		return m.parent.ApprovalMode()
	}
	return ModePrompt
}

// SetYoloMode enables or disables yolo mode.
// Yolo mode auto-approves all tool executions without prompting.
func (m *ApprovalManager) SetYoloMode(enabled bool) {
	if enabled {
		m.SetApprovalMode(ModeYolo)
		return
	}
	m.SetApprovalMode(ModePrompt)
}

// SetAutoMode enables or disables guardian auto mode.
func (m *ApprovalManager) SetAutoMode(enabled bool) {
	if enabled {
		m.SetApprovalMode(ModeAuto)
		return
	}
	m.SetApprovalMode(ModePrompt)
}

// SetAutoHeadless configures whether reviewer failures deny instead of prompting.
func (m *ApprovalManager) SetAutoHeadless(headless bool) {
	if m == nil {
		return
	}
	m.modeMu.Lock()
	m.autoHeadless = headless
	m.modeMu.Unlock()
}

func (m *ApprovalManager) AutoHeadless() bool {
	if m == nil {
		return false
	}
	m.modeMu.RLock()
	headless := m.autoHeadless
	m.modeMu.RUnlock()
	if headless {
		return true
	}
	if m.parent != nil {
		return m.parent.AutoHeadless()
	}
	return false
}

// YoloEnabled reports whether this manager or any parent manager is in yolo mode.
func (m *ApprovalManager) YoloEnabled() bool {
	return m.ApprovalMode() == ModeYolo
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

	// 1a. Check per-tool read allowlist (used for routed view_image uploads)
	if !isWrite && m.isPathAllowedForToolRead(toolName, path) {
		if m.DebugApproval {
			log.Printf("[approval]   tool read allowlist approved %q for %s", path, toolName)
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

func (m *ApprovalManager) addGuardianExactShell(command, workDir string) {
	root := m.root()
	if root == nil {
		return
	}
	key, ok := guardianExactKey(command, workDir)
	if !ok {
		return
	}
	root.guardianMu.Lock()
	root.guardianExact[key] = struct{}{}
	root.guardianMu.Unlock()
}

func (m *ApprovalManager) isGuardianExactShellApproved(command, workDir string) bool {
	root := m.root()
	if root == nil || m.ApprovalMode() != ModeAuto {
		return false
	}
	key, ok := guardianExactKey(command, workDir)
	if !ok {
		return false
	}
	root.guardianMu.RLock()
	_, ok = root.guardianExact[key]
	root.guardianMu.RUnlock()
	return ok
}

func (m *ApprovalManager) clearGuardianExactShell() {
	root := m.root()
	if root == nil {
		return
	}
	root.guardianMu.Lock()
	root.guardianExact = make(map[guardianShellKey]struct{})
	root.guardianMu.Unlock()
}

func guardianExactKey(command, workDir string) (guardianShellKey, bool) {
	command = strings.TrimSpace(command)
	if command == "" {
		return guardianShellKey{}, false
	}
	return guardianShellKey{Command: command, WorkDir: normalizeGuardianWorkDir(workDir)}, true
}

func normalizeGuardianWorkDir(workDir string) string {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			return cwd
		}
		return ""
	}
	if abs, err := filepath.Abs(workDir); err == nil {
		return abs
	}
	return workDir
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

	// Check guardian-approved exact commands. These intentionally use string
	// equality rather than shell glob/pattern matching so a guardian approval can
	// never widen itself into a broader deterministic allowlist.
	if m.isGuardianExactShellApproved(command, workDir) {
		return ProceedAlways, true
	}

	// Check session-approved exact commands and patterns.
	if m.shellCache.IsCommandApproved(command, workDir) {
		return ProceedAlways, true
	}
	if matchAnyShellPattern(m.shellCache.GetPatterns(), command) {
		return ProceedAlways, true
	}

	// Check parent's session-approved commands and patterns (inherited approvals).
	if m.parent != nil {
		if m.parent.shellCache.IsCommandApproved(command, workDir) {
			return ProceedAlways, true
		}
		if matchAnyShellPattern(m.parent.shellCache.GetPatterns(), command) {
			return ProceedAlways, true
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
	forceExplicit := isWrite && m.requiresExplicitMutations()
	// 0. Yolo mode - auto-approve everything unless this runtime mechanically
	// requires a fresh decision for shared-state mutations.
	if !forceExplicit && m.YoloEnabled() {
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

	if !forceExplicit {
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
	}
	if originalPath != absPath {
		return Cancel, NewToolErrorf(ErrSymlinkEscape, "path %s resolves to %s which is outside approved directories", originalPath, absPath)
	}

	// 4. Need to prompt user - serialize prompts to avoid UI conflicts
	// Use shared lock (via PromptLock()) to prevent concurrent prompts across parent/child managers
	promptLock := m.PromptLock()
	promptLock.Lock()
	defer promptLock.Unlock()

	// Recheck yolo now that we hold the prompt lock; the user may have toggled it
	// while this request was waiting behind another prompt.
	if !forceExplicit && m.YoloEnabled() {
		if m.DebugApproval {
			log.Printf("[approval] CheckPathApproval tool=%s path=%q isWrite=%v → yolo auto-approve after lock", toolName, path, isWrite)
		}
		return ProceedOnce, nil
	}

	// Recheck now that we hold the prompt lock to avoid duplicate prompts.
	if !forceExplicit {
		outcome, ok, err := m.checkPathApprovalNoPrompt(toolName, absPath, absPath, isWrite)
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
	}
	if originalPath != absPath {
		return Cancel, NewToolErrorf(ErrSymlinkEscape, "path %s resolves to %s which is outside approved directories", originalPath, absPath)
	}

	projectApprovals := m.getProjectApprovals(absPath)

	// Try new UI first (local, then ancestors), then fall back to legacy
	promptUIFunc := m.lookupPromptUIFunc()
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

	// Fall back to legacy PromptFunc (local, then ancestors)
	promptFunc := m.lookupPromptFunc()
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

	outcome, _ := promptFunc(req)

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
	return m.checkShellApprovalWithContext(context.Background(), command, workDir, nil)
}

// CheckShellApprovalWithContext checks shell approval with optional transcript evidence.
// It keeps the existing eager transcript API for callers that have already
// materialized approval evidence.
func (m *ApprovalManager) CheckShellApprovalWithContext(ctx context.Context, command, workDir string, transcript []TranscriptEntry) (ConfirmOutcome, error) {
	return m.checkShellApprovalWithContext(ctx, command, workDir, func() []TranscriptEntry { return transcript })
}

// checkShellApprovalWithContext checks shell approval with lazily-supplied
// transcript evidence. The supplier is only evaluated when an auto guardian
// reviewer actually needs the transcript.
func (m *ApprovalManager) checkShellApprovalWithContext(ctx context.Context, command, workDir string, transcript func() []TranscriptEntry) (ConfirmOutcome, error) {
	forceExplicit := m.requiresExplicitMutations()
	// Yolo mode - auto-approve everything unless shared-state safety requires a prompt.
	if !forceExplicit && m.YoloEnabled() {
		if m.DebugApproval {
			log.Printf("[approval] CheckShellApproval cmd=%q → yolo auto-approve", command)
		}
		return ProceedOnce, nil
	}

	if !forceExplicit {
		if outcome, ok := m.checkShellApprovalNoPrompt(command, workDir); ok {
			if m.DebugApproval {
				log.Printf("[approval] CheckShellApproval cmd=%q → no-prompt decided: %v", command, outcome)
			}
			return outcome, nil
		}
	}

	guardianAttemptedBeforeLock := false
	if !forceExplicit && m.ApprovalMode() == ModeAuto {
		guardianAttemptedBeforeLock = true
		if outcome, decided, err := m.checkShellGuardianApproval(ctx, command, workDir, transcript); decided || err != nil {
			return outcome, err
		}
	}

	// Need to prompt - serialize prompts to avoid UI conflicts
	// Use shared lock (via PromptLock()) to prevent concurrent prompts across parent/child managers
	promptLock := m.PromptLock()
	promptLock.Lock()
	defer promptLock.Unlock()

	// Recheck yolo now that we hold the prompt lock; the user may have toggled it
	// while this request was waiting behind another prompt.
	if !forceExplicit && m.YoloEnabled() {
		if m.DebugApproval {
			log.Printf("[approval] CheckShellApproval cmd=%q → yolo auto-approve after lock", command)
		}
		return ProceedOnce, nil
	}

	// Recheck now that we hold the prompt lock to avoid duplicate prompts.
	if !forceExplicit {
		if outcome, ok := m.checkShellApprovalNoPrompt(command, workDir); ok {
			if m.DebugApproval {
				log.Printf("[approval] CheckShellApproval cmd=%q → recheck decided: %v", command, outcome)
			}
			return outcome, nil
		}
	}

	if !forceExplicit && m.ApprovalMode() == ModeAuto && !guardianAttemptedBeforeLock {
		if outcome, decided, err := m.checkShellGuardianApproval(ctx, command, workDir, transcript); decided || err != nil {
			return outcome, err
		}
	}

	// Use the command's working directory for project approval lookup
	// so remembered approvals attach to the correct repo.
	dir := workDir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	projectApprovals := m.getProjectApprovals(dir)

	// Try new UI first (local, then ancestors), then fall back to legacy
	promptUIFunc := m.lookupPromptUIFunc()
	if promptUIFunc != nil {
		if m.DebugApproval {
			log.Printf("[approval] CheckShellApproval cmd=%q → calling PromptUIFunc", command)
		}
		result, err := promptUIFunc(command, false, true, workDir)
		if err != nil {
			return Cancel, err
		}
		return m.handleShellApprovalResult(result, command, workDir, projectApprovals)
	}

	// Fall back to legacy PromptFunc (local, then ancestors)
	promptFunc := m.lookupPromptFunc()
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
		// Cache the command or pattern for future use. A malformed remembered
		// pattern still honors the user's approval for this invocation.
		if pattern != "" {
			if err := m.shellCache.AddPattern(pattern); err != nil {
				log.Printf("[approval] failed to remember shell pattern %q; using one-time approval: %v", pattern, err)
				outcome = ProceedOnce
			}
		} else if err := m.shellCache.AddCommand(command, workDir); err != nil {
			return Cancel, fmt.Errorf("remember exact shell command: %w", err)
		}
	}
	if outcome == ProceedOnce || outcome == ProceedAlways || outcome == ProceedAlwaysAndSave {
		m.resetGuardianDenials()
	}

	return outcome, nil
}

func (m *ApprovalManager) guardianApprovalContext(command, workDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "shell_command=%q\n", strings.TrimSpace(command))
	fmt.Fprintf(&b, "workdir=%q\n", normalizeGuardianWorkDir(workDir))
	b.WriteString("These approvals are deterministic permissions already granted to term-llm tools. They are strong authorization evidence when the shell action is a narrow equivalent of those first-party tool operations. They do not authorize unrelated shell side effects, network transfer, process control, or credential disclosure.\n")
	m.appendApprovalContextFromChain(&b)
	return b.String()
}

func (m *ApprovalManager) appendApprovalContextFromChain(b *strings.Builder) {
	seenRead := map[string]struct{}{}
	seenWrite := map[string]struct{}{}
	seenShell := map[string]struct{}{}
	for cur := m; cur != nil; cur = cur.parent {
		if cur.permissions != nil {
			readDirs, writeDirs, shellAllow := cur.permissions.Snapshot()
			for _, dir := range readDirs {
				addApprovalContextLine(b, seenRead, "configured_read_dir", dir)
			}
			for _, dir := range writeDirs {
				addApprovalContextLine(b, seenWrite, "configured_write_dir", dir)
			}
			for _, pattern := range shellAllow {
				addApprovalContextLine(b, seenShell, "configured_shell_allow", pattern)
			}
		}
		readDirs, writeDirs := cur.dirCache.Snapshot()
		for _, dir := range readDirs {
			addApprovalContextLine(b, seenRead, "session_read_dir", dir)
		}
		for _, dir := range writeDirs {
			addApprovalContextLine(b, seenWrite, "session_write_dir", dir)
		}
		for _, pattern := range cur.shellCache.GetPatterns() {
			addApprovalContextLine(b, seenShell, "session_shell_pattern", pattern)
		}
		for _, approval := range cur.shellCache.getCommands() {
			addShellCommandApprovalContextLine(b, seenShell, approval)
		}
	}
}

func addShellCommandApprovalContextLine(b *strings.Builder, seen map[string]struct{}, approval shellCommandApproval) {
	key := "session_shell_command\x00" + approval.command + "\x00" + approval.workDir
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	fmt.Fprintf(b, "session_shell_command=%q workdir=%q\n", approval.command, approval.workDir)
}

func addApprovalContextLine(b *strings.Builder, seen map[string]struct{}, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	key := label + "\x00" + value
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	fmt.Fprintf(b, "%s=%q\n", label, value)
}

func (m *ApprovalManager) checkShellGuardianApproval(ctx context.Context, command, workDir string, transcript func() []TranscriptEntry) (ConfirmOutcome, bool, error) {
	reviewFunc := m.lookupPolicyReviewFunc()
	if reviewFunc == nil {
		m.emitGuardianEvent(m.guardianEvent(ctx, command, workDir, GuardianWarning, "guardian: auto mode unavailable (no reviewer configured); prompting for approval"))
		if m.AutoHeadless() {
			return Cancel, true, NewToolError(ErrPermissionDenied, "auto mode enabled but no guardian reviewer is configured")
		}
		return Cancel, false, nil
	}
	approvalContext := m.guardianApprovalContext(command, workDir)
	var entries []TranscriptEntry
	if transcript != nil {
		entries = transcript()
	}
	decision, err := reviewFunc(ctx, PolicyReviewRequest{Command: command, WorkDir: normalizeGuardianWorkDir(workDir), Transcript: entries, ApprovalContext: approvalContext, ScopeID: llm.SessionIDFromContext(ctx)})
	if err != nil {
		m.emitGuardianEvent(m.guardianEvent(ctx, command, workDir, GuardianError, fmt.Sprintf("guardian: review failed (%v)", err)))
		if m.AutoHeadless() {
			return Cancel, true, NewToolErrorf(ErrPermissionDenied, "guardian review failed: %v", err)
		}
		return Cancel, false, nil
	}
	if decision.Allowed && guardianAllowContradictsPolicy(decision) {
		rationale := strings.TrimSpace(decision.Rationale)
		if rationale == "" {
			rationale = "guardian allow contradicted policy risk/authorization fields"
		}
		m.emitGuardianEvent(m.guardianEvent(ctx, command, workDir, GuardianDenied, "guardian: denied: "+rationale))
		m.recordGuardianDenial()
		if !m.AutoHeadless() {
			return Cancel, false, nil
		}
		return Cancel, true, NewToolError(ErrPermissionDenied, rationale+". Do not attempt to achieve this outcome via workarounds.")
	}
	if decision.Allowed {
		m.resetGuardianDenials()
		m.addGuardianExactShell(command, workDir)
		m.emitGuardianEvent(m.guardianEvent(ctx, command, workDir, GuardianApproved, "guardian: "+formatGuardianApproval(decision)))
		return ProceedAlways, true, nil
	}
	rationale := strings.TrimSpace(decision.Rationale)
	if rationale == "" {
		rationale = "action was not approved by guardian policy"
	}
	m.emitGuardianEvent(m.guardianEvent(ctx, command, workDir, GuardianDenied, "guardian: denied: "+rationale))
	m.recordGuardianDenial()
	if !m.AutoHeadless() {
		// In interactive auto mode, guardian denials are rare and high-signal.
		// Escalate to the human instead of silently denying so the user can make
		// an explicit override decision with the rationale visible in the stream.
		return Cancel, false, nil
	}
	return Cancel, true, NewToolError(ErrPermissionDenied, rationale+". Do not attempt to achieve this outcome via workarounds.")
}

func guardianAllowContradictsPolicy(decision PolicyDecision) bool {
	risk := strings.ToLower(strings.TrimSpace(decision.RiskLevel))
	auth := strings.ToLower(strings.TrimSpace(decision.UserAuthorization))
	return risk == "high" || risk == "critical" || auth == "low" || auth == "unknown" || auth == "none"
}

func formatGuardianApproval(decision PolicyDecision) string {
	risk := strings.TrimSpace(decision.RiskLevel)
	if risk == "" {
		risk = "reviewed"
	}
	auth := humanGuardianAuthorization(decision.UserAuthorization)
	if auth == "" {
		return fmt.Sprintf("approved (%s risk)", risk)
	}
	return fmt.Sprintf("approved (%s risk; %s)", risk, auth)
}

func humanGuardianAuthorization(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "high":
		return "clearly user-authorized"
	case "medium":
		return "reasonably user-authorized"
	case "low":
		return "weakly user-authorized"
	case "unknown", "none":
		return "authorization unclear"
	default:
		return "authorization unclear"
	}
}

func (m *ApprovalManager) guardianEvent(ctx context.Context, command, workDir string, outcome GuardianOutcome, message string) GuardianEvent {
	return GuardianEvent{
		ToolCallID: llm.CallIDFromContext(ctx),
		Command:    command,
		WorkDir:    normalizeGuardianWorkDir(workDir),
		Message:    message,
		Outcome:    outcome,
	}
}

func (m *ApprovalManager) emitGuardianEvent(event GuardianEvent) {
	fn := m.lookupGuardianEventFunc()
	if fn != nil {
		fn(event)
	}
}

func (m *ApprovalManager) resetGuardianDenials() {
	root := m.root()
	if root == nil {
		return
	}
	root.guardianMu.Lock()
	root.guardianConsecutiveDenials = 0
	root.guardianMu.Unlock()
}

func (m *ApprovalManager) recordGuardianDenial() {
	root := m.root()
	if root == nil {
		return
	}
	root.guardianMu.Lock()
	root.guardianConsecutiveDenials++
	tripped := root.guardianCircuitBreakerLimit > 0 && root.guardianConsecutiveDenials >= root.guardianCircuitBreakerLimit
	root.guardianMu.Unlock()
	if tripped {
		root.clearGuardianExactShell()
		root.SetApprovalMode(ModePrompt)
		root.emitGuardianEvent(GuardianEvent{Message: "guardian: circuit breaker tripped after repeated denials; auto mode disabled", Outcome: GuardianWarning})
	}
}

// handleShellApprovalResult processes the result from the shell approval UI.
func (m *ApprovalManager) handleShellApprovalResult(result ApprovalResult, command, workDir string, projectApprovals *ProjectApprovals) (ConfirmOutcome, error) {
	if result.Cancelled {
		return Cancel, nil
	}

	switch result.Choice {
	case ApprovalChoiceDeny:
		return Cancel, nil

	case ApprovalChoiceOnce:
		m.resetGuardianDenials()
		return ProceedOnce, nil

	case ApprovalChoiceCommand:
		// Session-only exact command approval.
		if err := m.shellCache.AddCommand(command, workDir); err != nil {
			return Cancel, fmt.Errorf("remember exact shell command: %w", err)
		}
		m.resetGuardianDenials()
		return ProceedAlways, nil

	case ApprovalChoicePattern:
		// Approve pattern in repo (persisted if in repo, session otherwise)
		pattern := result.Pattern
		if pattern == "" {
			pattern = GenerateShellPattern(command)
		}
		if err := validateShellApprovalPattern(pattern); err != nil {
			log.Printf("[approval] invalid shell pattern %q; using one-time approval: %v", pattern, err)
			m.resetGuardianDenials()
			return ProceedOnce, nil
		}
		// Cache before persistence so a durable success can never be followed by a
		// failed session insertion. Validation failures degrade to this invocation.
		if err := m.shellCache.AddPattern(pattern); err != nil {
			log.Printf("[approval] failed to remember shell pattern %q; using one-time approval: %v", pattern, err)
			m.resetGuardianDenials()
			return ProceedOnce, nil
		}

		var persistErr error
		if result.SaveToRepo && projectApprovals != nil {
			persistErr = projectApprovals.ApproveShellPattern(pattern)
		}
		if persistErr != nil {
			log.Printf("[approval] failed to persist shell pattern %q; using session-only approval: %v", pattern, persistErr)
		}
		m.resetGuardianDenials()
		return ProceedAlways, nil

	default:
		return Cancel, nil
	}
}

// ApproveShellPattern adds a pattern to the session cache.
func (m *ApprovalManager) ApproveShellPattern(pattern string) error {
	return m.shellCache.AddPattern(pattern)
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

// matchAnyShellPattern returns true if the command is covered by the set of
// patterns. A command is covered if any single pattern matches the whole
// command, or — for compound commands — every segment (sequential and piped)
// is covered by some pattern. Pipe targets (i.e. pipe parts after the first)
// may additionally be a safe pipe built-in like grep/head/jq.
//
// Examples (with `gh *`, `echo *`, `python *` approved):
//
//	gh pr view 1 && echo hi && gh pr diff 1      → approved
//	gh pr diff 1 | python summarize.py           → approved
//	gh pr view 1 && rm -rf /tmp                  → denied
func matchAnyShellPattern(patterns []string, command string) bool {
	validPatterns := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if validateShellApprovalPattern(pattern) == nil {
			validPatterns = append(validPatterns, pattern)
		}
	}

	for _, pattern := range validPatterns {
		if matchPatternValidated(pattern, command) {
			return true
		}
	}
	if !hasUnsafeShellSyntax(command) {
		return false
	}
	seqParts := splitSequentialCommands(command)
	if len(seqParts) == 0 {
		return false
	}
	for _, seqPart := range seqParts {
		pipeParts := splitPipeCommands(seqPart)
		if len(pipeParts) == 0 {
			return false
		}
		for i, pipePart := range pipeParts {
			if matchAnyPatternSingle(validPatterns, pipePart) {
				continue
			}
			if i > 0 && isSafePipeTarget(pipePart) {
				continue
			}
			return false
		}
	}
	return true
}

// matchAnyPatternSingle returns true if any pattern matches the single
// command (no shell operators) via matchPatternSingle.
func matchAnyPatternSingle(patterns []string, command string) bool {
	for _, pattern := range patterns {
		if matchPatternSingle(pattern, command) {
			return true
		}
	}
	return false
}

// matchPattern checks if a command matches a glob pattern.
// For compound commands, it uses two-level decomposition:
//  1. Split on sequential operators (&&, ||, ;) — each segment must match.
//  2. Within each segment, split on pipes (|) — the first command must match
//     the pattern, and remaining pipe targets must match OR be safe pipe targets.
func matchPattern(pattern, command string) bool {
	if validateShellApprovalPattern(pattern) != nil {
		return false
	}
	return matchPatternValidated(pattern, command)
}

// matchPatternValidated matches a pattern that has already passed
// validateShellApprovalPattern.
func matchPatternValidated(pattern, command string) bool {
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
