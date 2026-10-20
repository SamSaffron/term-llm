package cmd

import (
	appchat "github.com/samsaffron/term-llm/internal/app/chat"
	"github.com/samsaffron/term-llm/internal/signal"
	"github.com/spf13/cobra"
)

var (
	chatDebug          bool
	chatSearch         bool
	chatNoSearch       bool
	chatProvider       string
	chatMCP            string
	chatMaxTurns       int
	chatNativeSearch   bool
	chatNoNativeSearch bool
	chatNoWebFetch     bool
	// Tool flags
	chatTools         string
	chatReadDirs      []string
	chatWriteDirs     []string
	chatShellAllow    []string
	chatSystemMessage string
	// Agent flag
	chatAgent string
	// Skills flag
	chatSkills string
	// Session resume flag
	chatResume string
	// Approval modes
	chatApproval     string
	chatYolo         bool
	chatAutoApproval bool
	// Auto-send mode (for benchmarking) - queue of messages to send
	chatAutoSend []string
	// Text mode (no markdown rendering)
	chatTextMode bool
)

var chatCmd = &cobra.Command{
	Use:   "chat [@agent]",
	Short: "Start an interactive chat session",
	Long: `Start an interactive TUI chat session with the LLM.

Examples:
  term-llm chat
  term-llm chat -s                        # with web search enabled
  term-llm chat --provider zen            # use specific provider
  term-llm chat --mcp playwright          # with MCP server(s) enabled

Agent examples (use @agent shortcut or --agent flag):
  term-llm chat @reviewer                 # code review session
  term-llm chat @editor                   # code editing session
  term-llm chat @web-researcher             # research session
  term-llm chat @agent-builder            # create custom agents
  term-llm chat --agent commit            # alternative syntax

Keyboard shortcuts:
  Enter        - Send message
  ! command    - Run directly in the session directory, then ask the model to respond
  Shift+Enter  - Insert newline
  Ctrl+/ Ctrl+H - Show help
  Ctrl+C       - Copy selection; cancel active response/tool/shell; press twice when idle to quit
  Ctrl+K       - Clear conversation
  Ctrl+N       - New session
  Ctrl+L       - Switch model
  Ctrl+R       - Cycle reasoning effort
  Ctrl+S       - Toggle web search
  Shift+Tab    - Cycle approval mode (prompt/auto/yolo)
  Ctrl+T       - MCP servers (tools)
  Ctrl+O       - Inspect conversation context
  Ctrl+E       - Expand/collapse tool and reasoning details
  Ctrl+P       - Command palette
  Ctrl+Y       - Copy selection, or latest assistant response
  PageUp/Down  - Scroll conversation
  Esc          - Cancel streaming / close modal / clear input

Slash commands:
  /help        - Show help
  /stats       - Show usage, cost, and context breakdown
  /clear       - Clear conversation
  /quit        - Exit chat
  /model       - Switch provider/model
  /effort      - Switch reasoning effort
  /search      - Toggle web search
  /fast        - Toggle ChatGPT fast mode
  /new         - Start a new session
  /save        - Save session with a name
  /copy        - Copy one assistant response as source Markdown
  /export      - Export conversation as markdown
  /thinking    - Toggle reasoning display
  /system      - Set custom system prompt
  /file        - Attach file(s) to next message
  /shell       - Open your shell or run a command in the session directory (--no-rc skips rc files)
  /dirs        - Manage approved directories
  /worktree    - Manage git worktrees for this session
  /mcp         - Manage MCP servers
  /skills      - List available skills
  /inspect     - View conversation/tool details
  /compact     - Compact conversation context
  /resume      - Browse and resume previous sessions
  /tree        - Browse paths or branch from an earlier message
  /reload      - Re-exec current binary and resume session
  /handover    - Hand conversation to another agent`,
	RunE:              runChat,
	ValidArgsFunction: AtAgentCompletion,
}

func init() {
	AddCommonFlags(chatCmd,
		CommonCoreFlags|CommonSearchFlags|CommonMaxTurns|CommonAgent|CommonSkills,
		CommonFlagBindings{
			Provider:        &chatProvider,
			Debug:           &chatDebug,
			Search:          &chatSearch,
			NoSearch:        &chatNoSearch,
			NativeSearch:    &chatNativeSearch,
			NoNativeSearch:  &chatNoNativeSearch,
			NoWebFetch:      &chatNoWebFetch,
			MCP:             &chatMCP,
			MaxTurns:        &chatMaxTurns,
			MaxTurnsDefault: 200,
			Tools:           &chatTools,
			ReadDirs:        &chatReadDirs,
			WriteDirs:       &chatWriteDirs,
			ShellAllow:      &chatShellAllow,
			SystemMessage:   &chatSystemMessage,
			Agent:           &chatAgent,
			Skills:          &chatSkills,
			Approval:        &chatApproval,
			Yolo:            &chatYolo,
			Auto:            &chatAutoApproval,
		})

	// Auto-send flag for benchmarking (repeatable for multiple messages)
	chatCmd.Flags().StringArrayVar(&chatAutoSend, "auto-send", nil, "Queue message(s) to send automatically and exit after all responses (repeatable)")

	// Text mode flag (no markdown rendering)
	chatCmd.Flags().BoolVar(&chatTextMode, "text", false, "Disable markdown rendering (plain text output)")

	// Session resume flag - NoOptDefVal allows --resume without a value
	chatCmd.Flags().StringVarP(&chatResume, "resume", "r", "", "Resume session (empty for most recent, or session ID)")
	chatCmd.Flags().Lookup("resume").NoOptDefVal = " " // space means "flag was passed without value"

	rootCmd.AddCommand(chatCmd)
}

func runChat(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext()
	defer stop()
	opts, deps := captureChatInvocation(cmd)
	return appchat.Run(ctx, args, opts, deps)
}
