package cmd

import (
	runtimeguardian "github.com/samsaffron/term-llm/internal/app/runtime/guardian"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func preflightHeadlessApproval(cfg *config.Config, resolved resolvedApprovalMode) error {
	return runtimeguardian.PreflightHeadlessApproval(cfg, resolved)
}

func applyResolvedApprovalMode(cfg *config.Config, approvalMgr *tools.ApprovalManager, resolved resolvedApprovalMode, opts approvalRuntimeOptions) error {
	return runtimeguardian.ApplyResolvedMode(cfg, approvalMgr, resolved, opts)
}

func addGuardianUsage(stats *ui.SessionStats, event tools.GuardianEvent) bool {
	return runtimeguardian.AddUsage(stats, event)
}
