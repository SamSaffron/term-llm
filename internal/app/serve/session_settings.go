package serve

import (
	"github.com/samsaffron/term-llm/internal/agents"
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/samsaffron/term-llm/internal/tools"
)

// SessionSettings holds the resolved settings for a session, merged from
// config defaults, agent settings, and CLI flags.
type SessionSettings struct {
	// Provider/model
	Provider string
	Model    string

	// Agent name (if any)
	AgentName    string
	PlanGuidance bool   // Built-in developer's callable-only update_plan guidance
	Workspace    string // Agent workspace policy: auto/default or none

	// Session ID (if any)
	SessionID string

	// Tool settings
	Tools        string
	AutoTools    []string // Transient tools required by structured user attachments; never persisted as configured tools.
	ReadDirs     []string
	WriteDirs    []string
	ShellAllow   []string
	ShellAutoRun bool
	Scripts      []string
	Spawn        tools.SpawnConfig

	// BaseDir is the unified per-session working directory for all tools. It is
	// threaded through ToolConfig and never applied with os.Chdir.
	BaseDir string
	// PrimaryWorkspace is non-empty only when a local launch or explicit
	// session/request binding proposes a canonical primary workspace. It grants
	// no file authority until the human confirms it on first access.
	PrimaryWorkspace string

	// RequireExplicitWorkingDir makes unbound remote runtimes reject relative
	// tool paths and default process execution instead of using daemon CWD.
	RequireExplicitWorkingDir bool

	// Agent directory (for run_agent_script and custom tools)
	AgentDir string

	// ShellWorkingDir roots the shell tool's command execution (exec.Cmd.Dir) at
	// a per-run directory without a process-wide os.Chdir. Empty preserves the
	// process-CWD default for local/legacy callers; unbound remote runtimes reject it.
	ShellWorkingDir string

	// CustomTools holds script-backed custom tool definitions from agent.yaml
	CustomTools []agents.CustomToolDef

	// MCP servers (comma-separated)
	MCP string

	// System prompt (already expanded)
	SystemPrompt string

	// Behavior
	MaxTurns        int
	MaxOutputTokens int // 0 = use provider default
	Search          bool
	TimeGrounding   bool

	// Memory / insights. MemoryPaths is invocation-local and never serialized.
	InsightsExpansion bool
	InsightsMaxTokens int             // 0 → default 500
	MemoryPaths       appmemory.Paths `json:"-"`
}

// CLIFlags holds the CLI flag values that can override settings.
type CLIFlags struct {
	Provider         string
	Tools            string
	ToolsSet         bool
	SystemMessageSet bool
	Inputs           *SessionInputSelection
	ReadDirs         []string
	WriteDirs        []string
	ShellAllow       []string
	MCP              string
	SystemMessage    string
	MaxTurns         int
	MaxTurnsSet      bool // true if --max-turns was explicitly set
	MaxOutputTokens  int  // 0 = use provider default
	Search           bool
	NoSearch         bool
	Files            []string // files passed via -f flag, used for agent template expansion (e.g., {{.Files}})
	Platform         string   // runtime surface for template expansion (e.g., chat, console, web, telegram, jobs)
	MemoryPaths      appmemory.Paths

	// ActiveProvider and ActiveModel name the provider/model the caller has
	// resolved for this run after CLI, request, agent, and config overrides.
	// When ActiveProvider is set the pair wins over the agent's preference for
	// prompt templating ({{provider}}, {{model}}) and model-gated user
	// AGENTS.md blocks, so the prompt describes the model that actually runs.
	ActiveProvider string
	ActiveModel    string
}
