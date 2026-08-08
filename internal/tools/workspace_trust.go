package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/filelock"
	"gopkg.in/yaml.v3"
)

const rememberedWorkspacesVersion = 1

type workspaceTrustStore interface {
	IsTrusted(workspace string) (bool, error)
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

func (s *fileWorkspaceTrustStore) IsTrusted(workspace string) (bool, error) {
	canonical, err := canonicalWorkspaceDirectory(workspace)
	if err != nil {
		return false, fmt.Errorf("canonicalize workspace trust lookup: %w", err)
	}
	rememberedWorkspacesMu.Lock()
	defer rememberedWorkspacesMu.Unlock()

	ledger, _, err := s.loadLocked()
	if err != nil {
		return false, err
	}
	for _, remembered := range ledger.Workspaces {
		if remembered.Path == canonical {
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
