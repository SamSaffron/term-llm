package tools

import "github.com/samsaffron/term-llm/internal/tools/gitidentity"

// GitRepoInfo contains information about a git repository.
type GitRepoInfo = gitidentity.GitRepoInfo

// DetectGitRepo discovers the physical checkout containing path, which may be a
// file, directory, or not-yet-created path. Git configuration and environment
// cannot redirect this approval identity to another checkout.
func DetectGitRepo(path string) GitRepoInfo {
	return gitidentity.DetectGitRepo(path)
}

// GetGitRepoID returns a unique identifier for a git repository.
// The ID is a SHA256 hash of the absolute root path, suitable for use as a filename.
func GetGitRepoID(root string) string {
	return gitidentity.GetGitRepoID(root)
}

// IsPathInRepo checks if the given path is under the specified repository root.
func IsPathInRepo(path, repoRoot string) bool {
	return gitidentity.IsPathInRepo(path, repoRoot)
}

// GetRelativePath returns the path relative to the repo root, or the original path if not in repo.
func GetRelativePath(path, repoRoot string) string {
	return gitidentity.GetRelativePath(path, repoRoot)
}
