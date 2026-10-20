package cmd

import (
	"io"
	"os"

	tea "charm.land/bubbletea/v2"
	appchat "github.com/samsaffron/term-llm/internal/app/chat"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/process"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/terminalpolicy"
	"github.com/samsaffron/term-llm/internal/tools"
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
	if cliErr == nil && cli != nil {
		// Canonicalize aliases too: subsequent runtime builds receive the
		// invocation's mutable requested policy, not the launch-time booleans.
		opts.Approval = cli.String()
	}
	interactive, outputInteractive := terminalpolicy.Interactive, terminalpolicy.OutputInteractive
	deps := appchat.Dependencies{
		Stdout: stdout, Stderr: stderr, SessionStore: sessionStoreOverrides(), MemoryPaths: memoryPaths(),
		LoadConfig: loadConfigWithSetup, CreateDebugLogger: createDebugLogger, StartDiagnostics: startInteractiveDiagnostics,
		Interactive:       func() bool { return interactive(os.Stdin, os.Stdout) },
		OutputInteractive: func() bool { return outputInteractive(os.Stdout) },
		StdoutIsTerminal:  func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }, OpenTTY: tea.OpenTTY,
		RestoreReloadState: process.RestoreState, ExecReload: execReload,
		ResolveApproval: func(cfg *config.Config, persisted *session.SessionApprovalMode, carried *tools.ApprovalMode, requested string) (resolvedApprovalMode, error) {
			if cliErr != nil {
				return resolvedApprovalMode{}, cliErr
			}
			// Capture explicitness, not immutable policy. runChatOnce updates
			// requested after an operator toggle; session switches still keep
			// explicit invocation policy ahead of any old handover carry.
			var selected *tools.ApprovalMode
			if cli != nil {
				value, err := parseCLIApprovalMode(requested)
				if err != nil {
					return resolvedApprovalMode{}, err
				}
				selected = &value
			} else if carried != nil {
				selected = carried
			}
			return resolveApprovalMode(approvalModeResolutionInput{Surface: approvalSurfaceChat, Config: cfg, CLI: selected, Session: persisted})
		},
	}
	return opts, deps
}
