package cmd

import (
	"io"
	"os"

	"github.com/samsaffron/term-llm/internal/agents"
	appchat "github.com/samsaffron/term-llm/internal/app/chat"
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/process"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func captureChatInvocation(cmd *cobra.Command) (appchat.Options, appchat.Dependencies) {
	opts := appchat.Options{
		Debug:          chatDebug,
		Search:         chatSearch,
		NoSearch:       chatNoSearch,
		Provider:       chatProvider,
		MCP:            chatMCP,
		MaxTurns:       chatMaxTurns,
		NativeSearch:   chatNativeSearch,
		NoNativeSearch: chatNoNativeSearch,
		NoWebFetch:     chatNoWebFetch,
		Tools:          chatTools,
		ReadDirs:       append([]string(nil), chatReadDirs...),
		WriteDirs:      append([]string(nil), chatWriteDirs...),
		ShellAllow:     append([]string(nil), chatShellAllow...),
		SystemMessage:  chatSystemMessage,
		Agent:          chatAgent,
		Skills:         chatSkills,
		Resume:         chatResume,
		Approval:       chatApproval,
		Yolo:           chatYolo,
		AutoApproval:   chatAutoApproval,
		AutoSend:       append([]string(nil), chatAutoSend...),
		TextMode:       chatTextMode,
		DebugRaw:       debugRaw,
	}
	stderr, stdout := io.Writer(io.Discard), io.Writer(io.Discard)
	if cmd != nil {
		opts.Changed = appchat.FlagState{Resume: cmd.Flags().Changed("resume"), Tools: cmd.Flags().Changed("tools"), System: cmd.Flags().Changed("system"), MaxTurns: cmd.Flags().Changed("max-turns"), Search: cmd.Flags().Changed("search")}
		stderr, stdout = cmd.ErrOrStderr(), cmd.OutOrStdout()
	}
	cli, cliErr := approvalModeFromCommand(cmd, opts.Approval, opts.AutoApproval, opts.Yolo)
	interactive, outputInteractive := chatLifecycleInteractive, chatOutputInteractive
	deps := appchat.Dependencies{
		Stdout: stdout, Stderr: stderr, SessionStore: sessionStoreOverrides(), MemoryPaths: memoryPaths(),
		LoadConfig: loadConfigWithSetup, CreateDebugLogger: createDebugLogger, StartDiagnostics: startInteractiveDiagnostics,
		Interactive:       func() bool { return interactive(os.Stdin, os.Stdout) },
		OutputInteractive: func() bool { return outputInteractive(os.Stdout) },
		StdoutIsTerminal:  func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }, OpenTTY: chatOpenTTY,
		RestoreReloadState: process.RestoreState, ExecReload: execReload,
		ResolveApproval: func(cfg *config.Config, persisted *session.SessionApprovalMode, carried *tools.ApprovalMode) (resolvedApprovalMode, error) {
			if cliErr != nil {
				return resolvedApprovalMode{}, cliErr
			}
			selected := cli
			if selected == nil && carried != nil {
				value := *carried
				selected = &value
			}
			return resolveApprovalMode(approvalModeResolutionInput{Surface: approvalSurfaceChat, Config: cfg, CLI: selected, Session: persisted})
		},
	}
	return opts, deps
}
func chatInvocationForCommand(cmd *cobra.Command) *chatInvocation {
	opts, deps := captureChatInvocation(cmd)
	return &chatInvocation{opts: opts, deps: deps}
}
func chatOwnsTerminalHost() bool     { return chatInvocationForCommand(nil).ownsTerminalHost() }
func chatRendererOwnsTerminal() bool { return chatInvocationForCommand(nil).rendererOwnsTerminal() }
func buildChatProgramInput(autoSend bool) (chatProgramInput, error) {
	return chatInvocationForCommand(nil).buildChatProgramInput(autoSend)
}
func resolveChatHandoverSystemPromptWithConfig(cmd *cobra.Command, cfg *config.Config, target *agents.Agent, provider, model string, paths appmemory.Paths) (string, error) {
	return chatInvocationForCommand(cmd).resolveChatHandoverSystemPromptWithConfig(cfg, target, provider, model, paths)
}
func resolveChatRuntimeSystemContextWithConfig(cmd *cobra.Command, cfg *config.Config, target *agents.Agent, provider, model, dir, instructions, system string, paths appmemory.Paths) (chat.RuntimeSystemContext, error) {
	return chatInvocationForCommand(cmd).resolveChatRuntimeSystemContextWithConfig(cfg, target, provider, model, dir, instructions, system, paths)
}
