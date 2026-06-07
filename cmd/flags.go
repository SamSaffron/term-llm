package cmd

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/spf13/cobra"
)

type flagKind int

const (
	flagKindBool flagKind = iota
	flagKindString
	flagKindStringArray
)

// CommonFlagSet identifies reusable groups of flags shared by LLM commands.
type CommonFlagSet uint64

const (
	CommonProvider CommonFlagSet = 1 << iota
	CommonDebug
	CommonSearch
	CommonNativeSearch
	CommonNoWebFetch
	CommonMCP
	CommonTools
	CommonSystemMessage
	CommonYolo
	CommonSkills
	// Tier 2 / non-position-0 flags. These can use the common registration
	// helper without becoming eligible for pre-command normalization.
	CommonMaxTurns
	CommonMaxOutputTokens
	CommonAgent
	CommonFiles
)

const (
	CommonCoreFlags   = CommonProvider | CommonDebug | CommonMCP | CommonTools | CommonSystemMessage | CommonYolo
	CommonSearchFlags = CommonSearch | CommonNativeSearch | CommonNoWebFetch
)

// CommonFlagBindings holds destinations and options for AddCommonFlags.
type CommonFlagBindings struct {
	Provider         *string
	Debug            *bool
	Search           *bool
	NativeSearch     *bool
	NoNativeSearch   *bool
	NoWebFetch       *bool
	MCP              *string
	Tools            *string
	ReadDirs         *[]string
	WriteDirs        *[]string
	ShellAllow       *[]string
	SystemMessage    *string
	Yolo             *bool
	Skills           *string
	MaxTurns         *int
	MaxTurnsDefault  int
	MaxOutputTokens  *int
	Agent            *string
	Files            *[]string
	FilesDescription string
}

// CommonFlags is kept as a backwards-compatible alias for common flag bindings.
type CommonFlags = CommonFlagBindings

type commonFlagMeta struct {
	Name       string
	Shorthand  string
	Kind       flagKind
	PreCommand bool
	Bit        CommonFlagSet
}

// PreCommand marks the intentionally limited Tier 1 flags accepted before a
// root command. Tier 2/context/control flags such as --file, --agent,
// --max-turns, --resume, --text, --json, and --fast remain command-local.
var commonFlagMetas = []commonFlagMeta{
	{Name: "provider", Shorthand: "p", Kind: flagKindString, PreCommand: true, Bit: CommonProvider},
	{Name: "debug", Shorthand: "d", Kind: flagKindBool, PreCommand: true, Bit: CommonDebug},
	{Name: "search", Shorthand: "s", Kind: flagKindBool, PreCommand: true, Bit: CommonSearch},
	{Name: "native-search", Kind: flagKindBool, PreCommand: true, Bit: CommonNativeSearch},
	{Name: "no-native-search", Kind: flagKindBool, PreCommand: true, Bit: CommonNativeSearch},
	{Name: "no-web-fetch", Kind: flagKindBool, PreCommand: true, Bit: CommonNoWebFetch},
	{Name: "mcp", Kind: flagKindString, PreCommand: true, Bit: CommonMCP},
	{Name: "tools", Kind: flagKindString, PreCommand: true, Bit: CommonTools},
	{Name: "read-dir", Kind: flagKindStringArray, PreCommand: true, Bit: CommonTools},
	{Name: "write-dir", Kind: flagKindStringArray, PreCommand: true, Bit: CommonTools},
	{Name: "shell-allow", Kind: flagKindStringArray, PreCommand: true, Bit: CommonTools},
	{Name: "system-message", Shorthand: "m", Kind: flagKindString, PreCommand: true, Bit: CommonSystemMessage},
	{Name: "yolo", Kind: flagKindBool, PreCommand: true, Bit: CommonYolo},
	{Name: "skills", Kind: flagKindString, PreCommand: true, Bit: CommonSkills},
	{Name: "max-turns", Kind: flagKindString, Bit: CommonMaxTurns},
	{Name: "max-output-tokens", Kind: flagKindString, Bit: CommonMaxOutputTokens},
	{Name: "agent", Shorthand: "a", Kind: flagKindString, Bit: CommonAgent},
	{Name: "file", Shorthand: "f", Kind: flagKindStringArray, Bit: CommonFiles},
}

var commandCommonFlagSets = map[string]CommonFlagSet{}

func (s CommonFlagSet) has(bit CommonFlagSet) bool {
	return s&bit != 0
}

// AddCommonFlags adds a set of reusable flags to cmd and records the command's
// flag capabilities for pre-command flag normalization.
func AddCommonFlags(cmd *cobra.Command, set CommonFlagSet, b CommonFlagBindings) {
	commandCommonFlagSets[cmd.Name()] |= set

	if set.has(CommonProvider) {
		requireStringFlagBinding("provider", b.Provider)
		AddProviderFlag(cmd, b.Provider)
	}
	if set.has(CommonDebug) {
		requireBoolFlagBinding("debug", b.Debug)
		AddDebugFlag(cmd, b.Debug)
	}
	if set.has(CommonSearch) {
		requireBoolFlagBinding("search", b.Search)
		AddSearchFlag(cmd, b.Search)
	}
	if set.has(CommonNativeSearch) {
		requireBoolFlagBinding("native-search", b.NativeSearch)
		requireBoolFlagBinding("no-native-search", b.NoNativeSearch)
		AddNativeSearchFlags(cmd, b.NativeSearch, b.NoNativeSearch)
	}
	if set.has(CommonNoWebFetch) {
		requireBoolFlagBinding("no-web-fetch", b.NoWebFetch)
		AddNoWebFetchFlag(cmd, b.NoWebFetch)
	}
	if set.has(CommonMCP) {
		requireStringFlagBinding("mcp", b.MCP)
		AddMCPFlag(cmd, b.MCP)
	}
	if set.has(CommonMaxTurns) {
		requireIntFlagBinding("max-turns", b.MaxTurns)
		AddMaxTurnsFlag(cmd, b.MaxTurns, b.MaxTurnsDefault)
	}
	if set.has(CommonMaxOutputTokens) {
		requireIntFlagBinding("max-output-tokens", b.MaxOutputTokens)
		AddMaxOutputTokensFlag(cmd, b.MaxOutputTokens)
	}
	if set.has(CommonTools) {
		requireStringFlagBinding("tools", b.Tools)
		requireStringSliceFlagBinding("read-dir", b.ReadDirs)
		requireStringSliceFlagBinding("write-dir", b.WriteDirs)
		requireStringSliceFlagBinding("shell-allow", b.ShellAllow)
		AddToolFlags(cmd, b.Tools, b.ReadDirs, b.WriteDirs, b.ShellAllow)
	}
	if set.has(CommonSystemMessage) {
		requireStringFlagBinding("system-message", b.SystemMessage)
		AddSystemMessageFlag(cmd, b.SystemMessage)
	}
	if set.has(CommonFiles) {
		requireStringSliceFlagBinding("file", b.Files)
		AddFileFlag(cmd, b.Files, b.FilesDescription)
	}
	if set.has(CommonAgent) {
		requireStringFlagBinding("agent", b.Agent)
		AddAgentFlag(cmd, b.Agent)
	}
	if set.has(CommonYolo) {
		requireBoolFlagBinding("yolo", b.Yolo)
		AddYoloFlag(cmd, b.Yolo)
	}
	if set.has(CommonSkills) {
		requireStringFlagBinding("skills", b.Skills)
		AddSkillsFlag(cmd, b.Skills)
	}
}

func requireStringFlagBinding(name string, ptr *string) {
	if ptr == nil {
		panic("missing string flag binding for " + name)
	}
}

func requireBoolFlagBinding(name string, ptr *bool) {
	if ptr == nil {
		panic("missing bool flag binding for " + name)
	}
}

func requireIntFlagBinding(name string, ptr *int) {
	if ptr == nil {
		panic("missing int flag binding for " + name)
	}
}

func requireStringSliceFlagBinding(name string, ptr *[]string) {
	if ptr == nil {
		panic("missing string slice flag binding for " + name)
	}
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

// AddNoWebFetchFlag adds --no-web-fetch to disable external read_url injection.
func AddNoWebFetchFlag(cmd *cobra.Command, dest *bool) {
	cmd.Flags().BoolVar(dest, "no-web-fetch", false, "Disable external URL fetching (read_url) when search is enabled")
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

// AddMaxOutputTokensFlag adds the --max-output-tokens flag
func AddMaxOutputTokensFlag(cmd *cobra.Command, dest *int) {
	cmd.Flags().IntVar(dest, "max-output-tokens", 0, "Maximum output tokens (0 = provider default)")
}

// AddToolFlags adds tool-related flags (--tools, --read-dir, --write-dir, --shell-allow)
func AddToolFlags(cmd *cobra.Command, tools *string, readDirs, writeDirs, shellAllow *[]string) {
	cmd.Flags().StringVar(tools, "tools", "", "Enable local tools (comma-separated, or 'all'): read_file,write_file,edit_file,shell,grep,glob,view_image,show_image,image_generate,ask_user,spawn_agent,queue_agent,wait_for_agent")
	cmd.Flags().StringArrayVar(readDirs, "read-dir", nil, "Directories for read_file/grep/glob/view_image tools (repeatable)")
	cmd.Flags().StringArrayVar(writeDirs, "write-dir", nil, "Directories for write_file/edit_file tools (repeatable)")
	cmd.Flags().StringArrayVar(shellAllow, "shell-allow", nil, "Shell command patterns to allow (repeatable, glob syntax)")
	if err := cmd.RegisterFlagCompletionFunc("tools", ToolsFlagCompletion); err != nil {
		panic("failed to register tools completion: " + err.Error())
	}
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
	cmd.Flags().StringVarP(dest, "agent", "a", "", "Use an agent (name or path to agent directory)")
	if err := cmd.RegisterFlagCompletionFunc("agent", AgentFlagCompletionWithPaths); err != nil {
		panic("failed to register agent completion: " + err.Error())
	}
}

// AddYoloFlag adds the --yolo flag for auto-approving all tool operations
func AddYoloFlag(cmd *cobra.Command, dest *bool) {
	cmd.Flags().BoolVar(dest, "yolo", false, "Auto-approve all tool operations (for CI/container use, bypasses all prompts)")
}

// AddSkillsFlag adds the --skills flag with completion
// Values: "all" or "*" to enable all, "none" to disable, skill names (comma-separated), or skill,+ for explicit + auto
func AddSkillsFlag(cmd *cobra.Command, dest *string) {
	cmd.Flags().StringVar(dest, "skills", "", "Skills mode: 'all'/'*' to enable all, 'none' to disable, names (comma-separated), or 'name,+' for explicit + auto")
	if err := cmd.RegisterFlagCompletionFunc("skills", SkillsFlagCompletion); err != nil {
		panic("failed to register skills completion: " + err.Error())
	}
}

// SkillsFlagCompletion provides shell completion for the --skills flag.
func SkillsFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Return special values first
	completions := []string{"all", "none"}

	// Add skill names from registry
	names, directive := SkillFlagCompletion(cmd, args, toComplete)
	completions = append(completions, names...)

	return completions, directive
}

// applySkillsFlag applies the --skills flag value to modify skills config.
// Returns the modified config (or original if flag is empty).
//
// Flag values:
//   - "" (empty): use config defaults
//   - "none": disable skills entirely
//   - "all" or "*": enable all skills with auto mode
//   - "skill1,skill2": explicit skills only (disables auto-discovery for injection)
//   - "skill1,+": explicit skills + auto-discovery
func applySkillsFlag(cfg *config.SkillsConfig, flag string) *config.SkillsConfig {
	if flag == "" {
		return cfg
	}

	// Create a copy to avoid modifying the original
	result := *cfg

	switch strings.TrimSpace(flag) {
	case "none":
		result.Enabled = false
		return &result
	case "all", "*":
		// Enable all skills with auto mode
		result.Enabled = true
		result.AutoInvoke = true
		return &result
	}

	// Parse comma-separated skill names
	var skills []string
	hasPlus := false

	for _, part := range strings.Split(flag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "+" {
			hasPlus = true
		} else {
			skills = append(skills, part)
		}
	}

	// Enable skills with specified list
	if len(skills) > 0 {
		result.Enabled = true
		result.AlwaysEnabled = skills
		// When explicit skills are specified, auto-invoke is disabled
		// unless "+" is included to also include auto-discovered skills
		result.AutoInvoke = hasPlus
	}

	return &result
}
