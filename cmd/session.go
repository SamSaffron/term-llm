package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
)

// SessionSettings holds the resolved settings for a session, merged from
// config defaults, agent settings, and CLI flags.
type SessionSettings struct {
	// Provider/model
	Provider string
	Model    string

	// Agent name (if any)
	AgentName string

	// Session ID (if any)
	SessionID string

	// Tool settings
	Tools        string
	ReadDirs     []string
	WriteDirs    []string
	ShellAllow   []string
	ShellAutoRun bool
	Scripts      []string

	// Agent directory (for run_agent_script and custom tools)
	AgentDir string

	// CustomTools holds script-backed custom tool definitions from agent.yaml
	CustomTools []agents.CustomToolDef

	// MCP servers (comma-separated)
	MCP string

	// System prompt (already expanded)
	SystemPrompt string

	// Behavior
	MaxTurns int
	Search   bool
}

// CLIFlags holds the CLI flag values that can override settings.
type CLIFlags struct {
	Provider      string
	Tools         string
	ReadDirs      []string
	WriteDirs     []string
	ShellAllow    []string
	MCP           string
	SystemMessage string
	MaxTurns      int
	MaxTurnsSet   bool // true if --max-turns was explicitly set
	Search        bool
	Files         []string // files passed via -f flag, used for agent template expansion (e.g., {{.Files}})
}

// LoadAgent loads and validates an agent by name or path.
// Returns nil if agentName is empty.
// If agentName contains a path separator, it is loaded directly from the filesystem.
// Otherwise, it is looked up in the agent registry.
func LoadAgent(agentName string, cfg *config.Config) (*agents.Agent, error) {
	if agentName == "" {
		return nil, nil
	}

	// If the value contains a path separator, load directly from filesystem
	if agents.IsAgentPath(agentName) {
		agent, err := agents.LoadFromPath(agentName)
		if err != nil {
			return nil, fmt.Errorf("load agent from path: %w", err)
		}
		if err := agent.Validate(); err != nil {
			return nil, fmt.Errorf("invalid agent at %s: %w", agentName, err)
		}
		// Apply preferences by agent name if configured
		if pref, ok := cfg.Agents.Preferences[agent.Name]; ok {
			agent.Merge(pref)
		}
		return agent, nil
	}

	registry, err := agents.NewRegistry(agents.RegistryConfig{
		UseBuiltin:  cfg.Agents.UseBuiltin,
		SearchPaths: cfg.Agents.SearchPaths,
	})
	if err != nil {
		return nil, fmt.Errorf("create agent registry: %w", err)
	}

	// Apply agent preferences from config
	registry.SetPreferences(cfg.Agents.Preferences)

	agent, err := registry.Get(agentName)
	if err != nil {
		return nil, fmt.Errorf("load agent: %w", err)
	}

	if err := agent.Validate(); err != nil {
		return nil, fmt.Errorf("invalid agent: %w", err)
	}

	return agent, nil
}

// ResolveSettings merges config, agent, and CLI flags into final settings.
// Priority: CLI > agent > config
func ResolveSettings(cfg *config.Config, agent *agents.Agent, cli CLIFlags, configProvider, configModel, configInstructions string, configMaxTurns, defaultMaxTurns int) (SessionSettings, error) {
	s := SessionSettings{}
	if agent != nil {
		s.AgentName = agent.Name
	}

	// Provider/model: CLI > agent > config
	s.Provider = configProvider
	s.Model = configModel
	if agent != nil {
		if agent.Provider != "" {
			s.Provider = agent.Provider
		}
		if agent.Model != "" {
			s.Model = agent.Model
		}
	}
	// CLI provider flag is handled separately via applyProviderOverridesWithAgent

	// Tools: CLI > agent
	if cli.Tools != "" {
		s.Tools = cli.Tools
	} else if agent != nil {
		if agent.HasEnabledList() {
			s.Tools = strings.Join(agent.Tools.Enabled, ",")
		} else if agent.HasDisabledList() {
			allTools := tools.AllToolNames()
			enabledTools := agent.GetEnabledTools(allTools)
			s.Tools = strings.Join(enabledTools, ",")
		}
	}

	// Read/Write/Shell dirs: CLI > agent
	if len(cli.ReadDirs) > 0 {
		s.ReadDirs = cli.ReadDirs
	} else if agent != nil {
		s.ReadDirs = agent.Read.Dirs
	}

	if len(cli.WriteDirs) > 0 {
		s.WriteDirs = cli.WriteDirs
	}
	// Note: agents don't have write dirs currently

	if len(cli.ShellAllow) > 0 {
		s.ShellAllow = cli.ShellAllow
	} else if agent != nil {
		s.ShellAllow = agent.Shell.Allow
	}

	// Shell auto-run and scripts from agent only
	if agent != nil {
		s.ShellAutoRun = agent.Shell.AutoRun
		// Extract script commands from map (sorted for determinism)
		keys := make([]string, 0, len(agent.Shell.Scripts))
		for k := range agent.Shell.Scripts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s.Scripts = append(s.Scripts, agent.Shell.Scripts[k])
		}
		s.AgentDir = agent.SourcePath
		s.CustomTools = agent.Tools.Custom
	}

	// MCP: CLI > agent
	if cli.MCP != "" {
		s.MCP = cli.MCP
	} else if agent != nil {
		mcpServers := agent.GetMCPServerNames()
		if len(mcpServers) > 0 {
			s.MCP = strings.Join(mcpServers, ",")
		}
	}

	// System prompt: CLI > agent > config
	if cli.SystemMessage != "" {
		templateCtx := agents.NewTemplateContextForTemplate(cli.SystemMessage).WithFiles(cli.Files)
		cwd, err := systemPromptCWDBaseDir()
		if err != nil {
			return s, err
		}
		expanded, err := expandSystemPromptWithIncludes(cli.SystemMessage, templateCtx, cwd)
		if err != nil {
			return s, fmt.Errorf("expand --system prompt: %w", err)
		}
		s.SystemPrompt = expanded
	} else if agent != nil && agent.SystemPrompt != "" {
		templateCtx, includeBaseDir, err := agentPromptTemplateContextAndBaseDir(agent, cli.Files)
		if err != nil {
			return s, fmt.Errorf("prepare agent system prompt context: %w", err)
		}
		expanded, err := expandSystemPromptWithIncludes(agent.SystemPrompt, templateCtx, includeBaseDir)
		if err != nil {
			return s, fmt.Errorf("expand agent system prompt: %w", err)
		}
		s.SystemPrompt = expanded

		// Append project instructions if agent requests them
		if agent.ShouldLoadProjectInstructions() {
			if projectInstructions := agents.DiscoverProjectInstructions(); projectInstructions != "" {
				s.SystemPrompt += "\n\n---\n\n" + projectInstructions
			}
		}
	} else {
		templateCtx := agents.NewTemplateContextForTemplate(configInstructions).WithFiles(cli.Files)
		cwd, err := systemPromptCWDBaseDir()
		if err != nil {
			return s, err
		}
		expanded, err := expandSystemPromptWithIncludes(configInstructions, templateCtx, cwd)
		if err != nil {
			return s, fmt.Errorf("expand config system prompt: %w", err)
		}
		s.SystemPrompt = expanded
	}

	// Max turns: CLI (if set) > agent > config > default
	if cli.MaxTurnsSet {
		s.MaxTurns = cli.MaxTurns
	} else if agent != nil && agent.MaxTurns > 0 {
		s.MaxTurns = agent.MaxTurns
	} else if configMaxTurns > 0 {
		s.MaxTurns = configMaxTurns
	} else {
		s.MaxTurns = defaultMaxTurns
	}

	// Search: CLI or agent enables it
	s.Search = cli.Search || (agent != nil && agent.Search)

	return s, nil
}

func expandSystemPromptWithIncludes(prompt string, templateCtx agents.TemplateContext, baseDir string) (string, error) {
	withIncludes, err := agents.ExpandFileIncludes(prompt, agents.IncludeOptions{
		BaseDir:       baseDir,
		MaxDepth:      agents.DefaultIncludeMaxDepth,
		AllowAbsolute: true,
	})
	if err != nil {
		return "", err
	}
	return agents.ExpandTemplate(withIncludes, templateCtx), nil
}

func systemPromptCWDBaseDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory for system prompt includes: %w", err)
	}
	return cwd, nil
}

func agentPromptTemplateContextAndBaseDir(agent *agents.Agent, files []string) (agents.TemplateContext, string, error) {
	templateCtx := agents.NewTemplateContextForTemplate(agent.SystemPrompt).WithFiles(files)

	if agent.Source == agents.SourceBuiltin {
		resourceDir, err := agents.ExtractBuiltinResources(agent.Name)
		if err != nil {
			return agents.TemplateContext{}, "", fmt.Errorf("extract builtin resources for %q: %w", agent.Name, err)
		}
		return templateCtx.WithResourceDir(resourceDir), resourceDir, nil
	}

	baseDir := strings.TrimSpace(agent.SourcePath)
	if baseDir == "" {
		cwd, err := systemPromptCWDBaseDir()
		if err != nil {
			return agents.TemplateContext{}, "", err
		}
		return templateCtx, cwd, nil
	}

	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return agents.TemplateContext{}, "", fmt.Errorf("resolve agent source path %q: %w", baseDir, err)
	}
	return templateCtx, absBase, nil
}

// SetupToolManager creates and configures a ToolManager from settings.
// Returns nil if no tools are enabled.
func (s *SessionSettings) SetupToolManager(cfg *config.Config, engine *llm.Engine) (*tools.ToolManager, error) {
	if s.Tools == "" {
		return nil, nil
	}

	toolConfig := buildToolConfig(s.Tools, s.ReadDirs, s.WriteDirs, s.ShellAllow, cfg)
	toolConfig.AgentDir = s.AgentDir
	if s.ShellAutoRun {
		toolConfig.ShellAutoRun = true
	}
	if len(s.Scripts) > 0 {
		toolConfig.ScriptCommands = append(toolConfig.ScriptCommands, s.Scripts...)
	}

	if errs := toolConfig.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("invalid tool config: %v", errs[0])
	}

	toolMgr, err := tools.NewToolManager(&toolConfig, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize tools: %w", err)
	}
	wireImageRecorder(toolMgr.Registry, s.AgentName, s.SessionID)

	// Register any custom script-backed tools declared in agent.yaml
	if len(s.CustomTools) > 0 {
		if err := toolMgr.Registry.RegisterCustomTools(s.CustomTools, s.AgentDir); err != nil {
			return nil, fmt.Errorf("custom tools: %w", err)
		}
	}

	toolMgr.SetupEngine(engine)
	return toolMgr, nil
}

// WireSpawnAgentRunner sets up the spawn_agent runner if the tool is enabled.
// This should be called after SetupToolManager.
// The toolMgr's ApprovalMgr is passed to sub-agents so they inherit session approvals
// and can use the parent's prompt function for interactive approvals.
func WireSpawnAgentRunner(cfg *config.Config, toolMgr *tools.ToolManager, yoloMode bool) error {
	return WireSpawnAgentRunnerWithStore(cfg, toolMgr, yoloMode, nil, "")
}

// WireSpawnAgentRunnerWithStore sets up the spawn_agent runner with session tracking.
// store is used to save subagent turns, parentSessionID links child sessions to parent.
func WireSpawnAgentRunnerWithStore(cfg *config.Config, toolMgr *tools.ToolManager, yoloMode bool, store session.Store, parentSessionID string) error {
	if toolMgr == nil {
		return nil
	}
	spawnTool := toolMgr.GetSpawnAgentTool()
	if spawnTool == nil {
		return nil
	}
	runner, err := NewSpawnAgentRunnerWithStore(cfg, yoloMode, toolMgr.ApprovalMgr, store, parentSessionID)
	if err != nil {
		return fmt.Errorf("setup spawn_agent: %w", err)
	}
	spawnTool.SetRunner(runner)
	return nil
}

func sessionStoreConfig(cfg *config.Config) session.Config {
	path := strings.TrimSpace(cfg.Sessions.Path)
	if cliPath := strings.TrimSpace(sessionDBPath); cliPath != "" {
		path = cliPath
	}
	return session.Config{
		Enabled:    cfg.Sessions.Enabled && !noSession,
		MaxAgeDays: cfg.Sessions.MaxAgeDays,
		MaxCount:   cfg.Sessions.MaxCount,
		Path:       path,
	}
}

// InitSessionStore creates a session store if enabled in config.
// Returns the store (may be nil if disabled) and a cleanup function.
// The cleanup function is always safe to call (handles nil store).
// Warnings are written to errWriter.
func InitSessionStore(cfg *config.Config, errWriter io.Writer) (session.Store, func()) {
	storeCfg := sessionStoreConfig(cfg)
	if !storeCfg.Enabled {
		return nil, func() {}
	}

	store, err := session.NewStore(storeCfg)
	if err != nil {
		// Check if this is a schema error that can be recovered by reset
		if isSchemaError(err) {
			dbPath, _ := session.ResolveDBPath(storeCfg.Path)
			fmt.Fprintf(errWriter, "Session database has schema errors: %v\n\n", err)
			fmt.Fprintf(errWriter, "Would you like to reset the sessions database? [y/N]: ")

			if promptForReset() {
				if resetErr := resetSessionDatabase(storeCfg.Path); resetErr != nil {
					fmt.Fprintf(errWriter, "warning: failed to reset database: %v\n", resetErr)
					return nil, func() {}
				}
				fmt.Fprintln(errWriter, "Session database reset. Creating new store...")
				// Retry store creation
				store, err = session.NewStore(storeCfg)
				if err != nil {
					fmt.Fprintf(errWriter, "warning: session store still unavailable after reset: %v\n", err)
					return nil, func() {}
				}
			} else {
				fmt.Fprintf(errWriter, "\nTo fix manually, run: term-llm sessions reset\n")
				fmt.Fprintf(errWriter, "Or delete: %s\n", dbPath)
				return nil, func() {}
			}
		} else {
			fmt.Fprintf(errWriter, "warning: session store unavailable: %v\n", err)
			return nil, func() {}
		}
	}

	// Wrap store with logging to surface persistence errors
	store = session.NewLoggingStore(store, func(format string, args ...any) {
		fmt.Fprintf(errWriter, "warning: "+format+"\n", args...)
	})

	cleanup := func() {
		if store != nil {
			store.Close()
		}
	}

	return store, cleanup
}

// isSchemaError checks if an error indicates a database schema problem
// that could be fixed by resetting the database.
func isSchemaError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	schemaPatterns := []string{
		"no such column",
		"no such table",
		"SQL logic error",
		"database disk image is malformed",
		"file is not a database",
		"create base schema",
		"initialize schema",
	}
	for _, pattern := range schemaPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}
	return false
}

// promptForReset asks the user if they want to reset the session database.
// Returns true if user confirms with 'y' or 'yes'.
func promptForReset() bool {
	// Try to open /dev/tty directly for interactive input
	// This works even when stdin is redirected
	tty, err := os.Open("/dev/tty")
	if err != nil {
		// Fall back to stdin
		tty = os.Stdin
	} else {
		defer tty.Close()
	}

	reader := bufio.NewReader(tty)
	response, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "yes" || response == "y"
}

// resetSessionDatabase deletes the session database files.
func resetSessionDatabase(pathOverride string) error {
	dbPath, err := session.ResolveDBPath(pathOverride)
	if err != nil {
		return fmt.Errorf("get db path: %w", err)
	}
	if dbPath == ":memory:" {
		return nil
	}

	filesToDelete := []string{
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
	}

	for _, f := range filesToDelete {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("delete %s: %w", f, err)
		}
	}

	return nil
}

// SetupSkills initializes the skills system if enabled.
// Returns the setup (may be nil) and logs warnings to errWriter on errors.
func SetupSkills(cfg *config.SkillsConfig, skillsFlag string, errWriter io.Writer) *skills.Setup {
	skillsCfg := applySkillsFlag(cfg, skillsFlag)
	if !skillsCfg.Enabled {
		return nil
	}

	setup, err := skills.NewSetup(skillsCfg)
	if err != nil {
		fmt.Fprintf(errWriter, "warning: skills initialization failed: %v\n", err)
		return nil
	}
	return setup
}

// RegisterSkillToolWithEngine registers activate_skill on the engine when skills are available.
// This is independent of local tool manager setup so skills work in both agent-only and tools modes.
func RegisterSkillToolWithEngine(engine *llm.Engine, toolMgr *tools.ToolManager, skillsSetup *skills.Setup) {
	if skillsSetup == nil || skillsSetup.Registry == nil {
		return
	}

	var skillTool *tools.ActivateSkillTool
	if toolMgr != nil {
		skillTool = toolMgr.Registry.RegisterSkillTool(skillsSetup.Registry)
	} else {
		skillTool = tools.NewActivateSkillTool(skillsSetup.Registry, nil)
	}
	if skillTool == nil {
		return
	}

	skillTool.SetOnActivated(func(allowedTools []string) {
		engine.SetAllowedTools(allowedTools)
	})
	if toolMgr != nil {
		skillTool.SetOnToolsActivated(func(defs []skills.SkillToolDef, skillDir string) {
			if err := toolMgr.Registry.RegisterSkillTools(defs, skillDir); err != nil {
				fmt.Fprintf(os.Stderr, "warning: skill tools registration failed: %v\n", err)
				return
			}
			// Register the newly added tools with the engine so the LLM can call them.
			// AddDynamicTool queues the spec for injection into the active agentic loop
			// so the LLM sees the tools immediately on the very next turn.
			for _, def := range defs {
				if tool, ok := toolMgr.Registry.Get(def.Name); ok {
					engine.AddDynamicTool(tool)
				}
			}
		})
	}
	engine.Tools().Register(skillTool)
}

// InjectSkillsMetadata appends <available_skills> metadata to instructions when available.
func InjectSkillsMetadata(instructions string, skillsSetup *skills.Setup) string {
	if skillsSetup == nil || !skillsSetup.HasSkillsXML() || skills.CheckAgentsMdForSkills() {
		return instructions
	}
	if instructions != "" {
		return instructions + "\n\n" + skillsSetup.XML
	}
	return skillsSetup.XML
}
