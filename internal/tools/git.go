package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/tools/gitidentity"
)

// GitRepoInfo contains information about a git repository.
type GitRepoInfo struct {
	IsRepo   bool   // Whether the path is inside a git repository
	Root     string // Absolute path to the repository root
	RepoName string // Basename of the repository (for display)
}

// DetectGitRepo discovers the physical checkout containing path, which may be a
// file, directory, or not-yet-created path. Git configuration and environment
// cannot redirect this approval identity to another checkout.
func DetectGitRepo(path string) GitRepoInfo {
	root := gitidentity.Root(path)
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
	if info := DetectGitRepo(absRoot); info.IsRepo && info.Root != "" {
		absRoot = info.Root
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
