package tools

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

const approvalRepoIDPrefixLen = 16

// ProjectApprovals stores per-project approval decisions.
// Approvals are persisted to ~/.config/term-llm/projects/<repo-slug>/<context-slug>-<short-hash>.yaml
type ProjectApprovals struct {
	RepoRoot      string    `yaml:"repo_root"`
	RepoName      string    `yaml:"repo_name"`
	UpdatedAt     time.Time `yaml:"updated_at"`
	ReadApproved  bool      `yaml:"read_approved"`  // Whole repo read access
	WriteApproved bool      `yaml:"write_approved"` // Whole repo write access
	ApprovedPaths []string  `yaml:"approved_paths"` // Individual approved paths (relative to repo)
	ShellPatterns []string  `yaml:"shell_patterns"` // Approved shell command patterns

	// Runtime fields (not persisted)
	filePath string     `yaml:"-"` // Path to the YAML file
	mu       sync.Mutex `yaml:"-"` // Protects concurrent access
}

// getProjectsDir returns the directory for storing project approvals.
func getProjectsDir() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "term-llm", "projects"), nil
}

func projectApprovalsFilePath(repoRoot string) (string, error) {
	projectsDir, err := getProjectsDir()
	if err != nil {
		return "", err
	}

	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		absRoot = repoRoot
	}
	absRoot = filepath.Clean(absRoot)

	repoSlug := slugifyApprovalPathPart(filepath.Base(absRoot))
	if repoSlug == "" {
		repoSlug = "repo"
	}

	contextSlug := slugifyApprovalPathPart(filepath.Base(filepath.Dir(absRoot)))
	if contextSlug == "" {
		contextSlug = "root"
	}

	repoID := GetGitRepoID(absRoot)
	shortID := repoID
	if len(shortID) > approvalRepoIDPrefixLen {
		shortID = shortID[:approvalRepoIDPrefixLen]
	}

	return filepath.Join(projectsDir, repoSlug, contextSlug+"-"+shortID+".yaml"), nil
}

func slugifyApprovalPathPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == '.' || unicode.IsSpace(r):
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// LoadProjectApprovals loads or creates approval data for a git repository.
// Returns nil if the repo root is empty or invalid.
func LoadProjectApprovals(repoRoot string) (*ProjectApprovals, error) {
	if repoRoot == "" {
		return nil, nil
	}

	filePath, err := projectApprovalsFilePath(repoRoot)
	if err != nil {
		return nil, err
	}

	pa := &ProjectApprovals{
		RepoRoot:      repoRoot,
		RepoName:      filepath.Base(repoRoot),
		filePath:      filePath,
		ApprovedPaths: []string{},
		ShellPatterns: []string{},
	}

	// Try to load existing file
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// No existing file, return empty approvals
			return pa, nil
		}
		return nil, err
	}

	if err := yaml.Unmarshal(data, pa); err != nil {
		// Corrupted file, start fresh
		return &ProjectApprovals{
			RepoRoot:      repoRoot,
			RepoName:      filepath.Base(repoRoot),
			filePath:      filePath,
			ApprovedPaths: []string{},
			ShellPatterns: []string{},
		}, nil
	}

	// Ensure runtime fields are set
	pa.filePath = filePath
	if pa.ApprovedPaths == nil {
		pa.ApprovedPaths = []string{}
	}
	if pa.ShellPatterns == nil {
		pa.ShellPatterns = []string{}
	}

	return pa, nil
}

// Save persists the approval data to disk.
func (p *ProjectApprovals) Save() error {
	if p == nil || p.filePath == "" {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return p.saveLocked()
}

func (p *ProjectApprovals) saveLocked() error {
	if p == nil || p.filePath == "" {
		return nil
	}

	p.UpdatedAt = time.Now()

	// Ensure directory exists
	dir := filepath.Dir(p.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := yaml.Marshal(p)
	if err != nil {
		return err
	}

	return os.WriteFile(p.filePath, data, 0600)
}

// IsReadApproved checks if read access is approved for the entire repo.
func (p *ProjectApprovals) IsReadApproved() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ReadApproved
}

// IsWriteApproved checks if write access is approved for the entire repo.
func (p *ProjectApprovals) IsWriteApproved() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.WriteApproved
}

// ApproveRead approves read access for the entire repo.
func (p *ProjectApprovals) ApproveRead() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ReadApproved = true
	return p.saveLocked()
}

// ApproveWrite approves write access for the entire repo.
func (p *ProjectApprovals) ApproveWrite() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.WriteApproved = true
	return p.saveLocked()
}

// IsPathApproved checks if a specific path is approved.
// Path should be absolute; it will be checked against repo root and approved paths.
func (p *ProjectApprovals) IsPathApproved(path string, isWrite bool) bool {
	if p == nil {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Check whole-repo approval
	if isWrite {
		if p.WriteApproved {
			return true
		}
	} else {
		if p.ReadApproved {
			return true
		}
	}

	resolvedPath, err := canonicalApprovalPath(path, isWrite)
	if err != nil {
		return false
	}

	// Check if path is in approved paths list
	relPath := GetRelativePath(resolvedPath, p.RepoRoot)
	for _, approved := range p.ApprovedPaths {
		// Check exact match or if path is under approved directory
		if relPath == approved || strings.HasPrefix(relPath, approved+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

// ApprovePath adds a specific path to the approved list.
// Path should be absolute; it will be stored relative to the repo root.
func (p *ProjectApprovals) ApprovePath(path string) error {
	if p == nil {
		return nil
	}

	resolvedPath, err := canonicalApprovalPath(path, true)
	if err != nil {
		return err
	}
	relPath := GetRelativePath(resolvedPath, p.RepoRoot)

	p.mu.Lock()
	defer p.mu.Unlock()
	// Check if already approved
	for _, existing := range p.ApprovedPaths {
		if existing == relPath {
			return nil
		}
	}
	p.ApprovedPaths = append(p.ApprovedPaths, relPath)
	return p.saveLocked()
}

// IsShellPatternApproved checks if a command matches any approved shell
// pattern. For compound commands, acceptance requires every segment to be
// covered by some approved pattern — see matchAnyShellPattern for details.
func (p *ProjectApprovals) IsShellPatternApproved(command string) bool {
	if p == nil {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return matchAnyShellPattern(p.ShellPatterns, command)
}

// ApproveShellPattern adds a shell command pattern to the approved list.
func (p *ProjectApprovals) ApproveShellPattern(pattern string) error {
	if p == nil {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Check if already approved
	for _, existing := range p.ShellPatterns {
		if existing == pattern {
			return nil
		}
	}
	p.ShellPatterns = append(p.ShellPatterns, pattern)
	return p.saveLocked()
}

// GenerateShellPattern creates a glob pattern from a command.
// For example: "go test ./..." -> "go test *"
func GenerateShellPattern(command string) string {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return command
	}

	// Keep the command name, replace args with wildcard
	if len(parts) == 1 {
		return parts[0]
	}

	// For common commands, keep the first argument
	switch parts[0] {
	case "go", "npm", "yarn", "pnpm", "cargo", "make", "git":
		if len(parts) >= 2 {
			return parts[0] + " " + parts[1] + " *"
		}
	case "python", "python3", "node", "ruby", "perl":
		// Script execution - keep just the interpreter
		return parts[0] + " *"
	}

	// Default: command + wildcard
	return parts[0] + " *"
}
