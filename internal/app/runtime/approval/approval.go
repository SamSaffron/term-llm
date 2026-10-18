// Package approval resolves command and session approval policy without depending on cmd.
package approval

import (
	"fmt"
	"io"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type Surface string

const (
	SurfaceChat     Surface = "chat"
	SurfaceAsk      Surface = "ask"
	SurfaceEdit     Surface = "edit"
	SurfaceExec     Surface = "exec"
	SurfaceLoop     Surface = "loop"
	SurfaceServe    Surface = "serve"
	SurfaceServeMCP Surface = "serve_mcp"
)

type ModeSource string

const (
	ModeSourceCLI            ModeSource = "cli"
	ModeSourceSession        ModeSource = "session"
	ModeSourceLegacySession  ModeSource = "legacy_session"
	ModeSourceSurfaceConfig  ModeSource = "surface_config"
	ModeSourceGlobalConfig   ModeSource = "global_config"
	ModeSourceBuiltinDefault ModeSource = "builtin_default"
)

type ResolvedMode struct {
	Mode   tools.ApprovalMode
	Source ModeSource
}

type RuntimeOptions struct {
	Headless         bool
	PrepareCallbacks bool
	WarningWriter    io.Writer
}

type ResolutionInput struct {
	Surface Surface
	Config  *config.Config
	CLI     *tools.ApprovalMode
	Session *session.SessionApprovalMode
}

func Resolve(input ResolutionInput) (ResolvedMode, error) {
	if input.CLI != nil {
		switch *input.CLI {
		case tools.ModePrompt, tools.ModeAuto, tools.ModeYolo:
			return ResolvedMode{Mode: *input.CLI, Source: ModeSourceCLI}, nil
		default:
			return ResolvedMode{}, fmt.Errorf("invalid CLI approval mode %d", *input.CLI)
		}
	}

	if input.Session != nil {
		switch strings.ToLower(strings.TrimSpace(string(*input.Session))) {
		case string(session.ApprovalModePrompt):
			return ResolvedMode{Mode: tools.ModePrompt, Source: ModeSourceSession}, nil
		case string(session.ApprovalModeAuto):
			return ResolvedMode{Mode: tools.ModeAuto, Source: ModeSourceSession}, nil
		default:
			// Empty values are legacy sessions created before approval policy was
			// persisted. Stored yolo and malformed values are also deliberately
			// downgraded on cold resume.
			return ResolvedMode{Mode: tools.ModePrompt, Source: ModeSourceLegacySession}, nil
		}
	}

	value, path, err := surfaceConfig(input.Config, input.Surface)
	if err != nil {
		return ResolvedMode{}, err
	}
	if strings.TrimSpace(value) != "" {
		mode, err := ParseConfiguredMode(path, value)
		if err != nil {
			return ResolvedMode{}, err
		}
		return ResolvedMode{Mode: mode, Source: ModeSourceSurfaceConfig}, nil
	}
	if input.Config != nil && strings.TrimSpace(input.Config.Approval.DefaultMode) != "" {
		mode, err := ParseConfiguredMode("approval.default_mode", input.Config.Approval.DefaultMode)
		if err != nil {
			return ResolvedMode{}, err
		}
		return ResolvedMode{Mode: mode, Source: ModeSourceGlobalConfig}, nil
	}

	mode := tools.ModePrompt
	if input.Surface == SurfaceChat || input.Surface == SurfaceAsk || input.Surface == SurfaceServe {
		mode = tools.ModeAuto
	}
	return ResolvedMode{Mode: mode, Source: ModeSourceBuiltinDefault}, nil
}

func surfaceConfig(cfg *config.Config, surface Surface) (value, path string, err error) {
	pathFor := func(path string, value func(*config.Config) string) (string, string, error) {
		if cfg == nil {
			return "", path, nil
		}
		return value(cfg), path, nil
	}
	switch surface {
	case SurfaceChat:
		return pathFor("chat.approval_mode", func(c *config.Config) string { return c.Chat.ApprovalMode })
	case SurfaceAsk:
		return pathFor("ask.approval_mode", func(c *config.Config) string { return c.Ask.ApprovalMode })
	case SurfaceEdit:
		return pathFor("edit.approval_mode", func(c *config.Config) string { return c.Edit.ApprovalMode })
	case SurfaceExec:
		return pathFor("exec.approval_mode", func(c *config.Config) string { return c.Exec.ApprovalMode })
	case SurfaceLoop:
		return pathFor("loop.approval_mode", func(c *config.Config) string { return c.Loop.ApprovalMode })
	case SurfaceServe:
		return pathFor("serve.approval_mode", func(c *config.Config) string { return c.Serve.ApprovalMode })
	case SurfaceServeMCP:
		return pathFor("serve.mcp.approval_mode", func(c *config.Config) string { return c.Serve.MCP.ApprovalMode })
	default:
		return "", "", fmt.Errorf("unknown approval surface %q", surface)
	}
}

var cliModeNames = []string{"prompt", "auto", "yolo"}

func ParseMode(value string, allowYolo bool) (tools.ApprovalMode, bool) {
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

func ParseConfiguredMode(path, value string) (tools.ApprovalMode, error) {
	if mode, ok := ParseMode(value, false); ok {
		return mode, nil
	}
	return tools.ModePrompt, fmt.Errorf("invalid %s %q: expected prompt or auto", path, value)
}

func ParseCLIMode(value string) (tools.ApprovalMode, error) {
	if mode, ok := ParseMode(value, true); ok {
		return mode, nil
	}
	return tools.ModePrompt, fmt.Errorf("invalid approval mode %q: must be one of %s", value, strings.Join(cliModeNames, ", "))
}

func surfaceForConfigKey(key string) (Surface, bool) {
	switch key {
	case "chat.approval_mode":
		return SurfaceChat, true
	case "ask.approval_mode":
		return SurfaceAsk, true
	case "edit.approval_mode":
		return SurfaceEdit, true
	case "exec.approval_mode":
		return SurfaceExec, true
	case "loop.approval_mode":
		return SurfaceLoop, true
	case "serve.approval_mode":
		return SurfaceServe, true
	case "serve.mcp.approval_mode":
		return SurfaceServeMCP, true
	default:
		return "", false
	}
}

func EffectiveConfigValue(key string, cfg *config.Config) (string, bool, error) {
	surface, ok := surfaceForConfigKey(key)
	if !ok {
		return "", false, nil
	}
	resolved, err := Resolve(ResolutionInput{Surface: surface, Config: cfg})
	if err != nil {
		return "", true, err
	}
	return fmt.Sprintf("%s (%s)", resolved.Mode, resolved.Source), true, nil
}

func Report(w io.Writer, enabled bool, resolved ResolvedMode, mgr *tools.ApprovalManager) {
	if !enabled || w == nil {
		return
	}
	actual := resolved.Mode
	if mgr != nil {
		actual = mgr.ApprovalMode()
	}
	fmt.Fprintf(w, "approval: requested=%s source=%s actual=%s\n", resolved.Mode, resolved.Source, actual)
}

func ChatCarryForRelaunch(modeChanged bool, handoverAutoSend string, mode tools.ApprovalMode) *tools.ApprovalMode {
	if !modeChanged || strings.TrimSpace(handoverAutoSend) == "" {
		return nil
	}
	carried := mode
	return &carried
}

func ToSession(value tools.ApprovalMode) session.SessionApprovalMode {
	switch value {
	case tools.ModeAuto:
		return session.ApprovalModeAuto
	case tools.ModeYolo:
		return session.ApprovalModeYolo
	default:
		return session.ApprovalModePrompt
	}
}

func ForColdPersistence(value tools.ApprovalMode) session.SessionApprovalMode {
	if value == tools.ModeYolo {
		return session.ApprovalModePrompt
	}
	return ToSession(value)
}
