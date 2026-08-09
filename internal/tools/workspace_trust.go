package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/filelock"
	"gopkg.in/yaml.v3"
)

const rememberedWorkspacesVersion = 1

type workspaceTrustStore interface {
	IsTrusted(context.Context, string) (bool, error)
	Remember(workspace string) error
}

type rememberedWorkspace struct {
	Path       string    `yaml:"path"`
	ApprovedAt time.Time `yaml:"approved_at"`
}

type rememberedWorkspaceFile struct {
	Version    int                   `yaml:"version"`
	Workspaces []rememberedWorkspace `yaml:"workspaces"`
}

type fileWorkspaceTrustStore struct {
	path string
}

var rememberedWorkspacesMu sync.Mutex

func defaultWorkspaceTrustStore() workspaceTrustStore {
	return &fileWorkspaceTrustStore{}
}

// registeredWorktrees returns canonical worktree roots in Git's authoritative
// order. Git documents the first porcelain entry as the main worktree. The
// command runs from an already-approved path and ignores ambient variables that
// could redirect repository discovery.
func registeredWorktrees(ctx context.Context, approvedWorkspace string) (string, map[string]struct{}, error) {
	approved, err := canonicalWorkspaceDirectory(approvedWorkspace)
	if err != nil {
		return "", nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	execCtx, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(execCtx, "git", "-C", approved, "worktree", "list", "--porcelain", "-z")
	cmd.Env = scrubGitRepositoryEnvironment(os.Environ())
	output, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("list registered Git worktrees: %w", err)
	}

	paths := make(map[string]struct{})
	mainRoot := ""
	seenWorktree := false
	for _, field := range strings.Split(string(output), "\x00") {
		if !strings.HasPrefix(field, "worktree ") {
			continue
		}
		path, canonicalErr := canonicalWorkspaceDirectory(strings.TrimPrefix(field, "worktree "))
		if !seenWorktree {
			seenWorktree = true
			if canonicalErr != nil {
				return "", nil, fmt.Errorf("canonicalize Git main worktree: %w", canonicalErr)
			}
			mainRoot = path
		}
		if canonicalErr != nil {
			continue
		}
		paths[path] = struct{}{}
	}
	if mainRoot == "" {
		return "", nil, fmt.Errorf("list registered Git worktrees: main worktree was not reported")
	}
	if _, registered := paths[approved]; !registered {
		return "", nil, fmt.Errorf("approved workspace %s is not a registered Git worktree", approved)
	}
	return mainRoot, paths, nil
}

func scrubGitRepositoryEnvironment(env []string) []string {
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		key := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			key = entry[:index]
		}
		if strings.EqualFold(key, "GIT_DIR") || strings.EqualFold(key, "GIT_COMMON_DIR") || strings.EqualFold(key, "GIT_WORK_TREE") {
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

// inheritsRegisteredMainWorktreeApproval verifies candidate from the approved
// repository's worktree registry. A direct approval inherits only when it was
// granted to the main worktree; inherited provenance can continue across
// sibling worktrees and back to main.
func inheritsRegisteredMainWorktreeApproval(ctx context.Context, approved, candidate, provenance string) bool {
	mainRoot, worktrees, err := registeredWorktrees(ctx, approved)
	if err != nil {
		return false
	}
	if provenance == primaryWorkspaceProvenanceConfirmed && approved != mainRoot {
		return false
	}
	if provenance != primaryWorkspaceProvenanceConfirmed && provenance != primaryWorkspaceProvenanceMainInherited {
		return false
	}
	_, registered := worktrees[candidate]
	return registered
}

func rememberedWorkspacesPath() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "term-llm", "remembered-workspaces.yaml"), nil
}

func (s *fileWorkspaceTrustStore) filePath() (string, error) {
	if s != nil && s.path != "" {
		return s.path, nil
	}
	return rememberedWorkspacesPath()
}

func (s *fileWorkspaceTrustStore) IsTrusted(ctx context.Context, workspace string) (bool, error) {
	canonical, err := canonicalWorkspaceDirectory(workspace)
	if err != nil {
		return false, fmt.Errorf("canonicalize workspace trust lookup: %w", err)
	}
	rememberedWorkspacesMu.Lock()
	ledger, _, err := s.loadLocked()
	rememberedWorkspacesMu.Unlock()
	if err != nil {
		return false, err
	}
	for _, remembered := range ledger.Workspaces {
		if remembered.Path == canonical {
			return true, nil
		}
	}
	for _, remembered := range ledger.Workspaces {
		mainRoot, worktrees, gitErr := registeredWorktrees(ctx, remembered.Path)
		if gitErr != nil || remembered.Path != mainRoot {
			continue
		}
		if _, registered := worktrees[canonical]; registered {
			return true, nil
		}
	}
	return false, nil
}

func (s *fileWorkspaceTrustStore) Remember(workspace string) error {
	canonical, err := canonicalWorkspaceDirectory(workspace)
	if err != nil {
		return fmt.Errorf("canonicalize remembered workspace: %w", err)
	}
	rememberedWorkspacesMu.Lock()
	defer rememberedWorkspacesMu.Unlock()

	path, err := s.filePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create remembered workspace directory: %w", err)
	}
	unlock, err := filelock.Lock(path + ".lock")
	if err != nil {
		return fmt.Errorf("lock remembered workspaces: %w", err)
	}
	defer func() { _ = unlock() }()

	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("secure remembered workspace permissions: %w", err)
	}
	ledger, _, err := s.loadLocked()
	if err != nil {
		return err
	}
	for _, remembered := range ledger.Workspaces {
		if remembered.Path == canonical {
			return nil
		}
	}
	ledger.Workspaces = append(ledger.Workspaces, rememberedWorkspace{
		Path:       canonical,
		ApprovedAt: time.Now(),
	})
	data, err := yaml.Marshal(ledger)
	if err != nil {
		return fmt.Errorf("marshal remembered workspaces: %w", err)
	}
	if err := config.WriteFileAtomically(path, data, 0o600); err != nil {
		return fmt.Errorf("write remembered workspaces: %w", err)
	}
	return nil
}

func (s *fileWorkspaceTrustStore) loadLocked() (rememberedWorkspaceFile, string, error) {
	path, err := s.filePath()
	if err != nil {
		return rememberedWorkspaceFile{}, "", err
	}
	ledger := rememberedWorkspaceFile{Version: rememberedWorkspacesVersion, Workspaces: []rememberedWorkspace{}}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ledger, path, nil
		}
		return rememberedWorkspaceFile{}, "", fmt.Errorf("stat remembered workspaces: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return rememberedWorkspaceFile{}, "", fmt.Errorf("remembered workspaces file %s has insecure permissions %o; want 0600", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return rememberedWorkspaceFile{}, "", fmt.Errorf("read remembered workspaces: %w", err)
	}
	if err := yaml.Unmarshal(data, &ledger); err != nil {
		return rememberedWorkspaceFile{}, "", fmt.Errorf("parse remembered workspaces: %w", err)
	}
	if ledger.Version != rememberedWorkspacesVersion {
		return rememberedWorkspaceFile{}, "", fmt.Errorf("unsupported remembered workspaces version %d", ledger.Version)
	}
	return ledger, path, nil
}
