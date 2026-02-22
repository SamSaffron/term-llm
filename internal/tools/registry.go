package tools

import (
	"fmt"
	"os"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/skills"
)

// LocalToolRegistry manages local tools and their registration with the engine.
type LocalToolRegistry struct {
	config      *ToolConfig
	permissions *ToolPermissions
	approval    *ApprovalManager
	limits      OutputLimits
	appConfig   *config.Config

	memoryStore ImageRecorder
	agent       string
	sessionID   string

	// Registered tools
	tools map[string]llm.Tool
}

// NewLocalToolRegistry creates a new registry from configuration.
// The approvalMgr parameter is used for interactive permission prompts.
func NewLocalToolRegistry(toolConfig *ToolConfig, appConfig *config.Config, approvalMgr *ApprovalManager) (*LocalToolRegistry, error) {
	// Build permissions from config
	perms, err := toolConfig.BuildPermissions()
	if err != nil {
		return nil, err
	}

	// If no approval manager provided, create one (for backwards compatibility)
	if approvalMgr == nil {
		approvalMgr = NewApprovalManager(perms)
	}

	r := &LocalToolRegistry{
		config:      toolConfig,
		permissions: perms,
		approval:    approvalMgr,
		limits:      DefaultOutputLimits(),
		appConfig:   appConfig,
		tools:       make(map[string]llm.Tool),
	}

	// Register enabled tools
	if err := r.registerEnabledTools(); err != nil {
		return nil, err
	}

	return r, nil
}

// SetImageRecorder wires an image recorder for image generation tracking.
func (r *LocalToolRegistry) SetImageRecorder(recorder ImageRecorder, agent, sessionID string) {
	r.memoryStore = recorder
	r.agent = agent
	r.sessionID = sessionID
}

// registerEnabledTools registers all tools that are enabled in config.
func (r *LocalToolRegistry) registerEnabledTools() error {
	for _, specName := range r.config.Enabled {
		if err := r.registerTool(specName); err != nil {
			return err
		}
	}
	return nil
}

// registerTool registers a single tool by spec name.
func (r *LocalToolRegistry) registerTool(specName string) error {
	if !ValidToolName(specName) {
		return NewToolErrorf(ErrInvalidParams, "unknown tool: %s", specName)
	}

	var tool llm.Tool

	switch specName {
	case ReadFileToolName:
		tool = NewReadFileTool(r.approval, r.limits)
	case WriteFileToolName:
		tool = NewWriteFileTool(r.approval)
	case EditFileToolName:
		tool = NewEditFileTool(r.approval)
	case UnifiedDiffToolName:
		tool = NewUnifiedDiffTool(r.approval)
	case ShellToolName:
		tool = NewShellTool(r.approval, r.config, r.limits)
	case GrepToolName:
		tool = NewGrepTool(r.approval, r.limits)
	case GlobToolName:
		tool = NewGlobTool(r.approval)
	case ViewImageToolName:
		tool = NewViewImageTool(r.approval)
	case ShowImageToolName:
		tool = NewShowImageTool(r.approval)
	case ImageGenerateToolName:
		tool = NewImageGenerateTool(r.approval, r.appConfig, r.config.ImageProvider, r.memoryStore, r.agent, r.sessionID)
	case AskUserToolName:
		tool = NewAskUserTool()
	case SpawnAgentToolName:
		// SpawnAgentTool requires a runner to be set later via SetRunner
		tool = NewSpawnAgentTool(r.config.Spawn, 0)
	case RunAgentScriptToolName:
		tool = NewRunAgentScriptTool(r.config, r.limits)
	default:
		return NewToolErrorf(ErrInvalidParams, "unimplemented tool: %s", specName)
	}

	r.tools[specName] = tool
	return nil
}

// RegisterWithEngine registers all enabled tools with the LLM engine.
func (r *LocalToolRegistry) RegisterWithEngine(engine *llm.Engine) {
	for _, tool := range r.tools {
		engine.Tools().Register(tool)
	}
}

// GetSpecs returns tool specs for all enabled tools.
func (r *LocalToolRegistry) GetSpecs() []llm.ToolSpec {
	specs := make([]llm.ToolSpec, 0, len(r.tools))
	for _, tool := range r.tools {
		specs = append(specs, tool.Spec())
	}
	return specs
}

// Get returns a tool by spec name.
func (r *LocalToolRegistry) Get(specName string) (llm.Tool, bool) {
	tool, ok := r.tools[specName]
	return tool, ok
}

// IsEnabled checks if a tool is enabled.
func (r *LocalToolRegistry) IsEnabled(specName string) bool {
	return r.config.IsToolEnabled(specName)
}

// Permissions returns the underlying permissions manager.
func (r *LocalToolRegistry) Permissions() *ToolPermissions {
	return r.permissions
}

// SetLimits updates the output limits.
func (r *LocalToolRegistry) SetLimits(limits OutputLimits) {
	r.limits = limits
	// Re-register tools that use limits
	for _, specName := range r.config.Enabled {
		switch specName {
		case ReadFileToolName:
			r.tools[specName] = NewReadFileTool(r.approval, r.limits)
		case ShellToolName:
			r.tools[specName] = NewShellTool(r.approval, r.config, r.limits)
		case GrepToolName:
			r.tools[specName] = NewGrepTool(r.approval, r.limits)
		case RunAgentScriptToolName:
			r.tools[specName] = NewRunAgentScriptTool(r.config, r.limits)
		}
	}
}

// AddReadDir adds a directory to the read allowlist at runtime.
func (r *LocalToolRegistry) AddReadDir(dir string) error {
	return r.permissions.AddReadDir(dir)
}

// AddWriteDir adds a directory to the write allowlist at runtime.
func (r *LocalToolRegistry) AddWriteDir(dir string) error {
	return r.permissions.AddWriteDir(dir)
}

// AddShellPattern adds a shell pattern to the allowlist at runtime.
func (r *LocalToolRegistry) AddShellPattern(pattern string) error {
	return r.permissions.AddShellPattern(pattern)
}

// ToolManager provides a high-level interface for tool management in commands.
type ToolManager struct {
	Registry    *LocalToolRegistry
	ApprovalMgr *ApprovalManager
}

// NewToolManager creates a new tool manager from config.
func NewToolManager(toolConfig *ToolConfig, appConfig *config.Config) (*ToolManager, error) {
	// Build permissions first to create ApprovalManager
	perms, err := toolConfig.BuildPermissions()
	if err != nil {
		return nil, err
	}

	// Create approval manager first so it can be shared with tools
	approvalMgr := NewApprovalManager(perms)

	// Create registry, passing the approval manager
	registry, err := NewLocalToolRegistry(toolConfig, appConfig, approvalMgr)
	if err != nil {
		return nil, err
	}

	return &ToolManager{
		Registry:    registry,
		ApprovalMgr: approvalMgr,
	}, nil
}

// SetupEngine registers tools with the engine.
func (m *ToolManager) SetupEngine(engine *llm.Engine) {
	m.Registry.RegisterWithEngine(engine)
}

// GetSpecs returns all tool specs for the request.
func (m *ToolManager) GetSpecs() []llm.ToolSpec {
	return m.Registry.GetSpecs()
}

// GetSpawnAgentTool returns the spawn_agent tool if enabled, for runner configuration.
func (m *ToolManager) GetSpawnAgentTool() *SpawnAgentTool {
	return m.Registry.GetSpawnAgentTool()
}

// GetSpawnAgentTool returns the spawn_agent tool if enabled.
func (r *LocalToolRegistry) GetSpawnAgentTool() *SpawnAgentTool {
	tool, ok := r.tools[SpawnAgentToolName]
	if !ok {
		return nil
	}
	if spawnTool, ok := tool.(*SpawnAgentTool); ok {
		return spawnTool
	}
	return nil
}

// RegisterOutputTool creates and registers a SetOutputTool with the given configuration.
// Returns the tool so the caller can retrieve the captured value later.
func (r *LocalToolRegistry) RegisterOutputTool(name, param, desc string) *SetOutputTool {
	tool := NewSetOutputTool(name, param, desc)
	r.tools[name] = tool
	return tool
}

// GetOutputTool returns the output tool by name if it exists and is a SetOutputTool.
func (r *LocalToolRegistry) GetOutputTool(name string) *SetOutputTool {
	tool, ok := r.tools[name]
	if !ok {
		return nil
	}
	if outputTool, ok := tool.(*SetOutputTool); ok {
		return outputTool
	}
	return nil
}

// RegisterSkillTool registers the activate_skill tool with the given registry.
// This must be called after the skills registry is created.
func (r *LocalToolRegistry) RegisterSkillTool(skillRegistry *skills.Registry) *ActivateSkillTool {
	tool := NewActivateSkillTool(skillRegistry, r.approval)
	r.tools[ActivateSkillToolName] = tool
	return tool
}

// RegisterSkillTools registers script-backed tools declared in a skill's frontmatter.
// skillDir is the skill's SourcePath (absolute directory containing SKILL.md).
// Tools are resolved relative to skillDir and executed from there.
// Duplicate registrations (same name) overwrite the previous entry — activating
// a skill twice is idempotent. Name collisions with built-in tools are rejected.
func (r *LocalToolRegistry) RegisterSkillTools(defs []skills.SkillToolDef, skillDir string) error {
	// Convert SkillToolDef → agents.CustomToolDef
	agentDefs := make([]agents.CustomToolDef, 0, len(defs))
	for _, d := range defs {
		if d.Name == "" {
			return fmt.Errorf("skill tool: name is required")
		}
		if d.Description == "" {
			return fmt.Errorf("skill tool %q: description is required", d.Name)
		}
		if d.Script == "" {
			return fmt.Errorf("skill tool %q: script is required", d.Name)
		}
		agentDefs = append(agentDefs, agents.CustomToolDef{
			Name:           d.Name,
			Description:    d.Description,
			Script:         d.Script,
			Input:          d.Input,
			TimeoutSeconds: d.TimeoutSeconds,
			Env:            d.Env,
			Call:           d.Call,
		})
	}

	// Warn if the skill dir doesn't exist (non-fatal, matches existing behaviour)
	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: skill directory not found: %s\n", skillDir)
	}

	return r.RegisterCustomTools(agentDefs, skillDir)
}

// GetSkillTool returns the activate_skill tool if registered.
func (r *LocalToolRegistry) GetSkillTool() *ActivateSkillTool {
	tool, ok := r.tools[ActivateSkillToolName]
	if !ok {
		return nil
	}
	if skillTool, ok := tool.(*ActivateSkillTool); ok {
		return skillTool
	}
	return nil
}
