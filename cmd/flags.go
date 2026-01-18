package cmd

import (
	"github.com/spf13/cobra"
)

// CommonFlags holds pointers to flag variables shared across commands.
// Each command creates its own instance with its own variables.
type CommonFlags struct {
	Provider       *string
	Debug          *bool
	MCP            *string
	Tools          *string
	ReadDirs       *[]string
	WriteDirs      *[]string
	ShellAllow     *[]string
	SystemMessage  *string
	Search         *bool
	NativeSearch   *bool
	NoNativeSearch *bool
	MaxTurns       *int
	Files          *[]string
	Agent          *string
}

// AddProviderFlag adds the --provider/-p flag with completion
func AddProviderFlag(cmd *cobra.Command, dest *string) {
	cmd.Flags().StringVarP(dest, "provider", "p", "", "Override provider, optionally with model (e.g., openai:gpt-4o)")
	if err := cmd.RegisterFlagCompletionFunc("provider", ProviderFlagCompletion); err != nil {
		panic("failed to register provider completion: " + err.Error())
	}
}

// AddDebugFlag adds the --debug/-d flag
func AddDebugFlag(cmd *cobra.Command, dest *bool) {
	cmd.Flags().BoolVarP(dest, "debug", "d", false, "Show debug information")
}

// AddSearchFlag adds the --search/-s flag
func AddSearchFlag(cmd *cobra.Command, dest *bool) {
	cmd.Flags().BoolVarP(dest, "search", "s", false, "Enable web search for current information")
}

// AddNativeSearchFlags adds --native-search and --no-native-search flags
func AddNativeSearchFlags(cmd *cobra.Command, native, noNative *bool) {
	cmd.Flags().BoolVar(native, "native-search", false, "Use provider's native search (override config)")
	cmd.Flags().BoolVar(noNative, "no-native-search", false, "Use external search tools instead of provider's native search")
}

// AddMCPFlag adds the --mcp flag with completion
func AddMCPFlag(cmd *cobra.Command, dest *string) {
	cmd.Flags().StringVar(dest, "mcp", "", "Enable MCP server(s), comma-separated (e.g., playwright,filesystem)")
	if err := cmd.RegisterFlagCompletionFunc("mcp", MCPFlagCompletion); err != nil {
		panic("failed to register mcp completion: " + err.Error())
	}
}

// AddMaxTurnsFlag adds the --max-turns flag
func AddMaxTurnsFlag(cmd *cobra.Command, dest *int, defaultValue int) {
	cmd.Flags().IntVar(dest, "max-turns", defaultValue, "Max agentic turns for tool execution")
}

// AddToolFlags adds tool-related flags (--tools, --read-dir, --write-dir, --shell-allow)
func AddToolFlags(cmd *cobra.Command, tools *string, readDirs, writeDirs, shellAllow *[]string) {
	cmd.Flags().StringVar(tools, "tools", "", "Enable local tools (comma-separated, or 'all'): read_file,write_file,edit_file,shell,grep,glob,view_image,show_image,image_generate,ask_user,spawn_agent")
	cmd.Flags().StringArrayVar(readDirs, "read-dir", nil, "Directories for read_file/grep/glob/view_image tools (repeatable)")
	cmd.Flags().StringArrayVar(writeDirs, "write-dir", nil, "Directories for write_file/edit_file tools (repeatable)")
	cmd.Flags().StringArrayVar(shellAllow, "shell-allow", nil, "Shell command patterns to allow (repeatable, glob syntax)")
}

// AddSystemMessageFlag adds the --system-message/-m flag
func AddSystemMessageFlag(cmd *cobra.Command, dest *string) {
	cmd.Flags().StringVarP(dest, "system-message", "m", "", "System message/instructions for the LLM (overrides config)")
}

// AddFileFlag adds the --file/-f flag
func AddFileFlag(cmd *cobra.Command, dest *[]string, description string) {
	cmd.Flags().StringArrayVarP(dest, "file", "f", nil, description)
}

// AddAgentFlag adds the --agent/-a flag with completion
func AddAgentFlag(cmd *cobra.Command, dest *string) {
	cmd.Flags().StringVarP(dest, "agent", "a", "", "Use an agent (named configuration bundle)")
	if err := cmd.RegisterFlagCompletionFunc("agent", AgentFlagCompletion); err != nil {
		panic("failed to register agent completion: " + err.Error())
	}
}

// AddYoloFlag adds the --yolo flag for auto-approving all tool operations
func AddYoloFlag(cmd *cobra.Command, dest *bool) {
	cmd.Flags().BoolVar(dest, "yolo", false, "Auto-approve all tool operations (for CI/container use, bypasses all prompts)")
}
