package cmd

import (
	"fmt"
	"io"

	runapproval "github.com/samsaffron/term-llm/internal/app/runtime/approval"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
)

var cliApprovalModeNames = []string{"prompt", "auto", "yolo"}

// These aliases keep cmd callers source-compatible while implementations move to runapproval.
type approvalSurface = runapproval.Surface

const (
	approvalSurfaceChat     = runapproval.SurfaceChat
	approvalSurfaceAsk      = runapproval.SurfaceAsk
	approvalSurfaceEdit     = runapproval.SurfaceEdit
	approvalSurfaceExec     = runapproval.SurfaceExec
	approvalSurfaceLoop     = runapproval.SurfaceLoop
	approvalSurfaceServe    = runapproval.SurfaceServe
	approvalSurfaceServeMCP = runapproval.SurfaceServeMCP
)

type approvalModeSource = runapproval.ModeSource

const (
	approvalModeSourceCLI            = runapproval.ModeSourceCLI
	approvalModeSourceSession        = runapproval.ModeSourceSession
	approvalModeSourceLegacySession  = runapproval.ModeSourceLegacySession
	approvalModeSourceSurfaceConfig  = runapproval.ModeSourceSurfaceConfig
	approvalModeSourceGlobalConfig   = runapproval.ModeSourceGlobalConfig
	approvalModeSourceBuiltinDefault = runapproval.ModeSourceBuiltinDefault
)

type resolvedApprovalMode = runapproval.ResolvedMode
type approvalRuntimeOptions = runapproval.RuntimeOptions
type approvalModeResolutionInput = runapproval.ResolutionInput

func resolveApprovalMode(input approvalModeResolutionInput) (resolvedApprovalMode, error) {
	return runapproval.Resolve(input)
}
func parseApprovalMode(value string, allowYolo bool) (tools.ApprovalMode, bool) {
	return runapproval.ParseMode(value, allowYolo)
}
func parseConfiguredApprovalMode(path, value string) (tools.ApprovalMode, error) {
	return runapproval.ParseConfiguredMode(path, value)
}
func parseCLIApprovalMode(value string) (tools.ApprovalMode, error) {
	return runapproval.ParseCLIMode(value)
}
func effectiveApprovalConfigValue(key string, cfg *config.Config) (string, bool, error) {
	return runapproval.EffectiveConfigValue(key, cfg)
}
func reportApprovalMode(w io.Writer, enabled bool, resolved resolvedApprovalMode, mgr *tools.ApprovalManager) {
	runapproval.Report(w, enabled, resolved, mgr)
}

// approvalModeFromCommand returns only explicitly supplied CLI state. Cobra's
// Changed state is authoritative; compatibility aliases are never inferred from
// their bound boolean values alone.
func approvalModeFromCommand(cmd *cobra.Command, approval string, auto, yolo bool) (*tools.ApprovalMode, error) {
	changedApproval := cmd != nil && cmd.Flags().Changed("approval")
	changedAuto := cmd != nil && cmd.Flags().Changed("auto")
	changedYolo := cmd != nil && cmd.Flags().Changed("yolo")
	changed := 0
	for _, value := range []bool{changedApproval, changedAuto, changedYolo} {
		if value {
			changed++
		}
	}
	if changed > 1 {
		return nil, fmt.Errorf("--approval, --auto, and --yolo are mutually exclusive")
	}
	if changedApproval {
		mode, err := parseCLIApprovalMode(approval)
		return &mode, err
	}
	if changedAuto {
		if !auto {
			return nil, fmt.Errorf("--auto=false is not supported; use --approval prompt")
		}
		mode := tools.ModeAuto
		return &mode, nil
	}
	if changedYolo {
		if !yolo {
			return nil, fmt.Errorf("--yolo=false is not supported; use --approval prompt")
		}
		mode := tools.ModeYolo
		return &mode, nil
	}
	return nil, nil
}

func resolveCommandApprovalMode(cmd *cobra.Command, surface approvalSurface, cfg *config.Config, persisted *session.SessionApprovalMode, approval string, auto, yolo bool) (resolvedApprovalMode, error) {
	cli, err := approvalModeFromCommand(cmd, approval, auto, yolo)
	if err != nil {
		return resolvedApprovalMode{}, err
	}
	return runapproval.Resolve(runapproval.ResolutionInput{Surface: surface, Config: cfg, CLI: cli, Session: persisted})
}
