package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitRepoInfo contains information about a git repository.
type GitRepoInfo struct {
	IsRepo   bool   // Whether the path is inside a git repository
	Root     string // Absolute path to the repository root
	RepoName string // Basename of the repository (for display)
}

// DetectGitRepo detects if the given path is inside a git repository.
// Returns GitRepoInfo with IsRepo=false if not in a repo or if git is unavailable.
// The path can be a file or directory.
func DetectGitRepo(path string) GitRepoInfo {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return GitRepoInfo{}
	}

	// Determine the directory to run git from. If the path doesn't exist yet,
	// walk up to the nearest existing ancestor before invoking git.
	workDir := absPath
	for {
		if info, err := os.Stat(workDir); err == nil {
			if info.IsDir() {
				break
			}
			workDir = filepath.Dir(workDir)
			continue
		}

		parent := filepath.Dir(workDir)
		if parent == workDir {
			return GitRepoInfo{}
		}
		workDir = parent
	}

	// Run git rev-parse --show-toplevel to find repo root
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = workDir

	output, err := cmd.Output()
	if err != nil {
		// Not in a git repo or git not available
		return GitRepoInfo{}
	}

	root := strings.TrimSpace(string(output))
	if root == "" {
		return GitRepoInfo{}
	}

	return GitRepoInfo{
		IsRepo:   true,
		Root:     root,
		RepoName: filepath.Base(root),
	}
}

// GetGitRepoID returns a unique identifier for a git repository.
// The ID is a SHA256 hash of the absolute root path, suitable for use as a filename.
func GetGitRepoID(root string) string {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		absRoot = root
	}

	h := sha256.New()
	h.Write([]byte(absRoot))
	// Use first 16 bytes (32 hex chars) for reasonable uniqueness without excessive length
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// IsPathInRepo checks if the given path is under the specified repository root.
func IsPathInRepo(path, repoRoot string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return false
	}

	// Check if path starts with repo root
	return strings.HasPrefix(absPath, absRoot+string(filepath.Separator)) || absPath == absRoot
}

// GetRelativePath returns the path relative to the repo root, or the original path if not in repo.
func GetRelativePath(path, repoRoot string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return path
	}

	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return path
	}

	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return path
	}

	return rel
}
