package tools

import (
	"context"
	"path/filepath"

	"github.com/samsaffron/term-llm/internal/pathutil"
	"github.com/samsaffron/term-llm/internal/tools/gitidentity"
)

// canonicalFileApprovalPath enforces a write boundary before any workspace,
// allowlist, directory/project cache, extension capability, or Guardian grant.
// Generic file authority never authorizes Git administrative writes. Yolo is
// deliberately unconditional; reads retain their existing approval behavior.
func (m *ApprovalManager) canonicalFileApprovalPath(path string, isWrite bool) (string, error) {
	canonical, err := canonicalApprovalPath(path, isWrite)
	if err != nil {
		return "", err
	}
	if isWrite && !m.YoloEnabled() {
		if err := checkGitAdministrativeWrite(path); err != nil {
			return "", err
		}
		if canonical != path {
			if err := checkGitAdministrativeWrite(canonical); err != nil {
				return "", err
			}
		}
	}
	return canonical, nil
}

func checkGitAdministrativeWrite(path string) error {
	if gitidentity.IsAdministrativePath(path) {
		return NewToolErrorf(ErrPermissionDenied, "file tools cannot write Git administrative metadata; edit it manually or use a separately approved shell operation: %s", path)
	}
	return nil
}

// checkFileWriteApproval preserves the requested path as well as its canonical
// target. Otherwise a .git entry symlink to a gitfile stored under an ordinary
// name could lose its administrative spelling before reaching path approval.
func checkFileWriteApproval(ctx context.Context, manager *ApprovalManager, toolName, canonical, requested string, config *ToolConfig) (ConfirmOutcome, error) {
	if !manager.YoloEnabled() {
		expanded, err := pathutil.Expand(requested)
		if err != nil {
			return Cancel, NewToolErrorf(ErrInvalidParams, "cannot expand write path: %v", err)
		}
		baseDir := config.WorkingDir() // WorkingDir supports nil configs.
		if !filepath.IsAbs(expanded) && baseDir != "" {
			expanded = filepath.Join(baseDir, expanded)
		}
		if err := checkGitAdministrativeWrite(expanded); err != nil {
			return Cancel, err
		}
	}
	return manager.CheckPathApprovalWithContext(ctx, toolName, canonical, requested, true)
}
