package cmd

import (
	runtimeguardian "github.com/term-llm/term-llm/internal/app/runtime/guardian"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/tools"
	"github.com/term-llm/term-llm/internal/ui"
)

func applyResolvedApprovalMode(cfg *config.Config, approvalMgr *tools.ApprovalManager, resolved resolvedApprovalMode, opts approvalRuntimeOptions) error {
	return runtimeguardian.ApplyResolvedMode(cfg, approvalMgr, resolved, opts)
}

func addGuardianUsage(stats *ui.SessionStats, event tools.GuardianEvent) bool {
	return runtimeguardian.AddUsage(stats, event)
}
