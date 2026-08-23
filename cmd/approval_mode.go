package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
)

type approvalSurface string

const (
	approvalSurfaceChat     approvalSurface = "chat"
	approvalSurfaceAsk      approvalSurface = "ask"
	approvalSurfaceEdit     approvalSurface = "edit"
	approvalSurfaceExec     approvalSurface = "exec"
	approvalSurfaceLoop     approvalSurface = "loop"
	approvalSurfaceServe    approvalSurface = "serve"
	approvalSurfaceServeMCP approvalSurface = "serve_mcp"
)

type approvalModeSource string

const (
	approvalModeSourceCLI            approvalModeSource = "cli"
	approvalModeSourceSession        approvalModeSource = "session"
	approvalModeSourceLegacySession  approvalModeSource = "legacy_session"
	approvalModeSourceSurfaceConfig  approvalModeSource = "surface_config"
	approvalModeSourceGlobalConfig   approvalModeSource = "global_config"
	approvalModeSourceBuiltinDefault approvalModeSource = "builtin_default"
)

type resolvedApprovalMode struct {
	Mode   tools.ApprovalMode
	Source approvalModeSource
}

type approvalRuntimeOptions struct {
	Headless         bool
	PrepareCallbacks bool
	WarningWriter    io.Writer
}

type approvalModeResolutionInput struct {
	Surface approvalSurface
	Config  *config.Config
	CLI     *tools.ApprovalMode
	Session *session.SessionApprovalMode
}

func resolveApprovalMode(input approvalModeResolutionInput) (resolvedApprovalMode, error) {
	if input.CLI != nil {
		switch *input.CLI {
		case tools.ModePrompt, tools.ModeAuto, tools.ModeYolo:
			return resolvedApprovalMode{Mode: *input.CLI, Source: approvalModeSourceCLI}, nil
		default:
			return resolvedApprovalMode{}, fmt.Errorf("invalid CLI approval mode %d", *input.CLI)
		}
	}

	if input.Session != nil {
		switch strings.ToLower(strings.TrimSpace(string(*input.Session))) {
		case string(session.ApprovalModePrompt):
			return resolvedApprovalMode{Mode: tools.ModePrompt, Source: approvalModeSourceSession}, nil
		case string(session.ApprovalModeAuto):
			return resolvedApprovalMode{Mode: tools.ModeAuto, Source: approvalModeSourceSession}, nil
		default:
			// Empty values are legacy sessions created before approval policy was
			// persisted. Stored yolo and malformed values are also deliberately
			// downgraded on cold resume.
			return resolvedApprovalMode{Mode: tools.ModePrompt, Source: approvalModeSourceLegacySession}, nil
		}
	}

	value, path, err := surfaceApprovalConfig(input.Config, input.Surface)
	if err != nil {
		return resolvedApprovalMode{}, err
	}
	if strings.TrimSpace(value) != "" {
		mode, err := parseConfiguredApprovalMode(path, value)
		if err != nil {
			return resolvedApprovalMode{}, err
		}
		return resolvedApprovalMode{Mode: mode, Source: approvalModeSourceSurfaceConfig}, nil
	}
	if input.Config != nil && strings.TrimSpace(input.Config.Approval.DefaultMode) != "" {
		mode, err := parseConfiguredApprovalMode("approval.default_mode", input.Config.Approval.DefaultMode)
		if err != nil {
			return resolvedApprovalMode{}, err
		}
		return resolvedApprovalMode{Mode: mode, Source: approvalModeSourceGlobalConfig}, nil
	}

	mode := tools.ModePrompt
	if input.Surface == approvalSurfaceChat || input.Surface == approvalSurfaceAsk || input.Surface == approvalSurfaceServe {
		mode = tools.ModeAuto
	}
	return resolvedApprovalMode{Mode: mode, Source: approvalModeSourceBuiltinDefault}, nil
}

func surfaceApprovalConfig(cfg *config.Config, surface approvalSurface) (value, path string, err error) {
	pathFor := func(path string, value func(*config.Config) string) (string, string, error) {
		if cfg == nil {
			return "", path, nil
		}
		return value(cfg), path, nil
	}
	switch surface {
	case approvalSurfaceChat:
		return pathFor("chat.approval_mode", func(c *config.Config) string { return c.Chat.ApprovalMode })
	case approvalSurfaceAsk:
		return pathFor("ask.approval_mode", func(c *config.Config) string { return c.Ask.ApprovalMode })
	case approvalSurfaceEdit:
		return pathFor("edit.approval_mode", func(c *config.Config) string { return c.Edit.ApprovalMode })
	case approvalSurfaceExec:
		return pathFor("exec.approval_mode", func(c *config.Config) string { return c.Exec.ApprovalMode })
	case approvalSurfaceLoop:
		return pathFor("loop.approval_mode", func(c *config.Config) string { return c.Loop.ApprovalMode })
	case approvalSurfaceServe:
		return pathFor("serve.approval_mode", func(c *config.Config) string { return c.Serve.ApprovalMode })
	case approvalSurfaceServeMCP:
		return pathFor("serve.mcp.approval_mode", func(c *config.Config) string { return c.Serve.MCP.ApprovalMode })
	default:
		return "", "", fmt.Errorf("unknown approval surface %q", surface)
	}
}

var cliApprovalModeNames = []string{"prompt", "auto", "yolo"}

func parseApprovalMode(value string, allowYolo bool) (tools.ApprovalMode, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "prompt":
		return tools.ModePrompt, true
	case "auto":
		return tools.ModeAuto, true
	case "yolo":
		return tools.ModeYolo, allowYolo
	default:
		return tools.ModePrompt, false
	}
}

func parseConfiguredApprovalMode(path, value string) (tools.ApprovalMode, error) {
	if mode, ok := parseApprovalMode(value, false); ok {
		return mode, nil
	}
	return tools.ModePrompt, fmt.Errorf("invalid %s %q: expected prompt or auto", path, value)
}

func parseCLIApprovalMode(value string) (tools.ApprovalMode, error) {
	if mode, ok := parseApprovalMode(value, true); ok {
		return mode, nil
	}
	return tools.ModePrompt, fmt.Errorf("invalid approval mode %q: must be one of %s", value, strings.Join(cliApprovalModeNames, ", "))
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
	return resolveApprovalMode(approvalModeResolutionInput{Surface: surface, Config: cfg, CLI: cli, Session: persisted})
}

func approvalSurfaceForConfigKey(key string) (approvalSurface, bool) {
	switch key {
	case "chat.approval_mode":
		return approvalSurfaceChat, true
	case "ask.approval_mode":
		return approvalSurfaceAsk, true
	case "edit.approval_mode":
		return approvalSurfaceEdit, true
	case "exec.approval_mode":
		return approvalSurfaceExec, true
	case "loop.approval_mode":
		return approvalSurfaceLoop, true
	case "serve.approval_mode":
		return approvalSurfaceServe, true
	case "serve.mcp.approval_mode":
		return approvalSurfaceServeMCP, true
	default:
		return "", false
	}
}

func effectiveApprovalConfigValue(key string, cfg *config.Config) (string, bool, error) {
	surface, ok := approvalSurfaceForConfigKey(key)
	if !ok {
		return "", false, nil
	}
	resolved, err := resolveApprovalMode(approvalModeResolutionInput{Surface: surface, Config: cfg})
	if err != nil {
		return "", true, err
	}
	return fmt.Sprintf("%s (%s)", resolved.Mode, resolved.Source), true, nil
}

func reportApprovalMode(w io.Writer, enabled bool, resolved resolvedApprovalMode, mgr *tools.ApprovalManager) {
	if !enabled || w == nil {
		return
	}
	actual := resolved.Mode
	if mgr != nil {
		actual = mgr.ApprovalMode()
	}
	fmt.Fprintf(w, "approval: requested=%s source=%s actual=%s\n", resolved.Mode, resolved.Source, actual)
}

func chatApprovalCarryForRelaunch(modeChanged bool, handoverAutoSend string, mode tools.ApprovalMode) *tools.ApprovalMode {
	if !modeChanged || strings.TrimSpace(handoverAutoSend) == "" {
		return nil
	}
	carried := mode
	return &carried
}

func approvalModeToSession(value tools.ApprovalMode) session.SessionApprovalMode {
	switch value {
	case tools.ModeAuto:
		return session.ApprovalModeAuto
	case tools.ModeYolo:
		return session.ApprovalModeYolo
	default:
		return session.ApprovalModePrompt
	}
}

func approvalModeForColdPersistence(value tools.ApprovalMode) session.SessionApprovalMode {
	if value == tools.ModeYolo {
		return session.ApprovalModePrompt
	}
	return approvalModeToSession(value)
}
