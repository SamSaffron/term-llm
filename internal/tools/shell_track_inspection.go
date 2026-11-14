package tools

import (
	"context"

	"github.com/term-llm/term-llm/internal/filetrack/shellscope"
)

// These adapters preserve the shell tracker's private representation and its
// focused regression fixtures. The inspection package never decides attribution,
// permissions, or output claims; those remain with the shell tool.
type shellExpandedCandidate struct {
	path   string
	source shellCandidateSource
}

func normalizeShellPatterns(patterns []string) []string {
	return shellscope.NormalizePatterns(patterns)
}

func expandShellPatterns(workDir string, patterns []string) []shellExpandedCandidate {
	var candidates []shellExpandedCandidate
	for _, candidate := range shellscope.ExpandPatterns(workDir, patterns) {
		source := shellSourceGlob
		if candidate.Literal {
			source = shellSourceLiteral
		}
		candidates = append(candidates, shellExpandedCandidate{path: candidate.Path, source: source})
	}
	return candidates
}

func hasGitAdminComponent(path string) bool { return shellscope.HasGitAdminComponent(path) }
func pathWithinRoot(path, root string) bool { return shellscope.PathWithinRoot(path, root) }

type shellRepoResolver struct{ *shellscope.RepoResolver }

func newShellRepoResolver() *shellRepoResolver {
	return &shellRepoResolver{shellscope.NewRepoResolver()}
}

func (resolver *shellRepoResolver) owningRepo(path string) string {
	return resolver.OwningRepo(path)
}

func (resolver *shellRepoResolver) owningRepoForCandidate(path, workDir, workRepo string) string {
	return resolver.OwningRepoForCandidate(path, workDir, workRepo)
}

func gitIgnoredCandidates(ctx context.Context, root string, paths []string) map[string]bool {
	return shellscope.GitIgnoredCandidates(ctx, root, paths)
}

func canonicalPathForGitRel(path string) string { return shellscope.CanonicalPathForGitRel(path) }

func gitStatusPorcelain(ctx context.Context, root string) map[string]string {
	return shellscope.GitStatusPorcelain(ctx, root)
}

func gitShowIndexBatch(ctx context.Context, root string, absPaths []string, maxFileBytes, maxReads int, maxTotalBytes int64) map[string][]byte {
	return shellscope.GitShowIndexBatch(ctx, root, absPaths, maxFileBytes, maxReads, maxTotalBytes)
}
