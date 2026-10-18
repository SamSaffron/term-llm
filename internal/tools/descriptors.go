package tools

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

// ToolSurface identifies a runtime in which a built-in is offered. Availability
// still depends on the runtime's configuration and capabilities.
type ToolSurface string

const (
	SurfaceCLI      ToolSurface = "cli"
	SurfaceChat     ToolSurface = "tui_chat"
	SurfaceServe    ToolSurface = "serve"
	SurfaceTelegram ToolSurface = "telegram"
	SurfaceJobs     ToolSurface = "jobs"
	SurfaceLive     ToolSurface = "live_voice"
)

// ToolDescriptor is a read-only snapshot of a built-in tool's public metadata.
// The schema is obtained with ToolSpec rather than shared through this value.
type ToolDescriptor struct {
	Name           string
	Description    string
	Policy         ToolKind
	Modes          []ToolSurface
	DefaultEnabled bool
	Aliases        []string
	FlagLabel      string
	RenderHint     string
}

type builtinDescriptor struct {
	ToolDescriptor
	construct        func(*LocalToolRegistry) llm.Tool
	pathCapable      bool
	guardianSelector bool
	requiresSpawn    bool
	hubCredentials   bool
	nonLocal         bool
	hiddenCompletion bool
}

var commonSurfaces = []ToolSurface{SurfaceCLI, SurfaceChat, SurfaceServe, SurfaceTelegram, SurfaceJobs, SurfaceLive}
var liveSurfaces = []ToolSurface{SurfaceLive}
var builtinTools = []builtinDescriptor{
	{ToolDescriptor: ToolDescriptor{Name: ReadFileToolName, Policy: KindRead, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: ReadFileToolName}, construct: func(r *LocalToolRegistry) llm.Tool { return NewReadFileTool(r.approval, r.limits, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: WriteFileToolName, Policy: KindEdit, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: WriteFileToolName}, construct: func(r *LocalToolRegistry) llm.Tool { return NewWriteFileTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: EditFileToolName, Policy: KindEdit, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: EditFileToolName}, construct: func(r *LocalToolRegistry) llm.Tool { return NewEditFileTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: UnifiedDiffToolName, Policy: KindEdit, Modes: commonSurfaces, DefaultEnabled: true}, construct: func(r *LocalToolRegistry) llm.Tool { return NewUnifiedDiffTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: ShellToolName, Policy: KindExecute, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: ShellToolName}, construct: func(r *LocalToolRegistry) llm.Tool { return NewShellTool(r.approval, r.config, r.limits) }},
	{ToolDescriptor: ToolDescriptor{Name: GrepToolName, Policy: KindSearch, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: GrepToolName}, construct: func(r *LocalToolRegistry) llm.Tool { return NewGrepTool(r.approval, r.limits, r.config) }, pathCapable: true, guardianSelector: true},
	{ToolDescriptor: ToolDescriptor{Name: GlobToolName, Policy: KindSearch, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: GlobToolName}, construct: func(r *LocalToolRegistry) llm.Tool { return NewGlobTool(r.approval, r.config) }, pathCapable: true, guardianSelector: true},
	{ToolDescriptor: ToolDescriptor{Name: ViewImageToolName, Policy: KindRead, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: ViewImageToolName}, construct: func(r *LocalToolRegistry) llm.Tool { return NewViewImageTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: ShowMediaToolName, Aliases: []string{ShowImageToolName}, Policy: KindImage, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: ShowMediaToolName, RenderHint: "media"}, construct: func(r *LocalToolRegistry) llm.Tool {
		t := NewShowMediaTool(r.approval, r.config)
		t.SetPublisher(r.mediaPublisher)
		return t
	}, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: ImageGenerateToolName, Policy: KindImage, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: ImageGenerateToolName}, construct: func(r *LocalToolRegistry) llm.Tool {
		return NewImageGenerateTool(r.approval, r.appConfig, r.config.ImageProvider, r.memoryStore, r.agent, r.sessionID, r.config)
	}, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: AskUserToolName, Policy: KindInteractive, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: AskUserToolName}, construct: func(*LocalToolRegistry) llm.Tool { return NewAskUserTool() }},
	{ToolDescriptor: ToolDescriptor{Name: SpawnAgentToolName, Policy: KindAgent, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: "spawn_agent (includes wait_agent,continue_agent,cancel_agent,list_agents)"}, construct: func(r *LocalToolRegistry) llm.Tool { return NewSpawnAgentTool(r.config.Spawn, 0) }},
	{ToolDescriptor: ToolDescriptor{Name: WaitAgentToolName, Policy: KindAgent, Modes: commonSurfaces, DefaultEnabled: true}, requiresSpawn: true, construct: agentControlConstructor(WaitAgentToolName)},
	{ToolDescriptor: ToolDescriptor{Name: ContinueAgentToolName, Policy: KindAgent, Modes: commonSurfaces, DefaultEnabled: true}, requiresSpawn: true, construct: agentControlConstructor(ContinueAgentToolName)},
	{ToolDescriptor: ToolDescriptor{Name: CancelAgentToolName, Policy: KindAgent, Modes: commonSurfaces, DefaultEnabled: true}, requiresSpawn: true, construct: agentControlConstructor(CancelAgentToolName)},
	{ToolDescriptor: ToolDescriptor{Name: ListAgentsToolName, Policy: KindAgent, Modes: commonSurfaces, DefaultEnabled: true}, requiresSpawn: true, construct: agentControlConstructor(ListAgentsToolName)},
	{ToolDescriptor: ToolDescriptor{Name: QueueAgentToolName, Policy: KindAgent, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: QueueAgentToolName}, construct: func(r *LocalToolRegistry) llm.Tool { return NewQueueAgentTool(r.config) }},
	{ToolDescriptor: ToolDescriptor{Name: WaitForJobsToolName, Policy: KindAgent, Modes: commonSurfaces, DefaultEnabled: true, FlagLabel: WaitForJobsToolName}, construct: func(*LocalToolRegistry) llm.Tool { return NewWaitForJobsTool() }},
	{ToolDescriptor: ToolDescriptor{Name: RunAgentScriptToolName, Policy: KindExecute, Modes: commonSurfaces, DefaultEnabled: true}, construct: func(r *LocalToolRegistry) llm.Tool { return NewRunAgentScriptTool(r.config, r.limits) }},
	{ToolDescriptor: ToolDescriptor{Name: InitiateHandoverToolName, Policy: KindAgent, Modes: commonSurfaces, DefaultEnabled: true}, construct: func(*LocalToolRegistry) llm.Tool { return NewInitiateHandoverTool() }},
	{ToolDescriptor: ToolDescriptor{Name: HubDelegateToolName, Policy: KindAgent, Modes: commonSurfaces, DefaultEnabled: true}, hubCredentials: true, construct: func(*LocalToolRegistry) llm.Tool { return NewHubDelegateTool() }},
	{ToolDescriptor: ToolDescriptor{Name: HubCheckDelegationToolName, Policy: KindAgent, Modes: commonSurfaces, DefaultEnabled: true}, hubCredentials: true, construct: func(*LocalToolRegistry) llm.Tool { return NewHubCheckDelegationTool() }},
	{ToolDescriptor: ToolDescriptor{Name: ManageWorkspaceToolName, Policy: KindSessionState, Modes: commonSurfaces}, construct: func(r *LocalToolRegistry) llm.Tool { return NewManageWorkspaceTool(r.approval, r.config) }},
	{ToolDescriptor: ToolDescriptor{Name: UpdatePlanToolName, Policy: KindSessionState, Modes: commonSurfaces}, construct: func(r *LocalToolRegistry) llm.Tool { return r.newSessionStateTool(UpdatePlanToolName) }},
	{ToolDescriptor: ToolDescriptor{Name: LiveSettingsToolName, Policy: KindSessionState, Modes: liveSurfaces}, construct: func(r *LocalToolRegistry) llm.Tool { return r.newSessionStateTool(LiveSettingsToolName) }},
	{ToolDescriptor: ToolDescriptor{Name: UIGetSourceToolName, Policy: KindRead, Modes: []ToolSurface{SurfaceChat, SurfaceServe}}, construct: func(r *LocalToolRegistry) llm.Tool { return &UIGetSourceTool{approval: r.approval, config: r.config} }},
	{ToolDescriptor: ToolDescriptor{Name: UIExtensionsToolName, Policy: KindSessionState, Modes: []ToolSurface{SurfaceChat, SurfaceServe}}, construct: func(*LocalToolRegistry) llm.Tool { return &UIExtensionsTool{} }},
	{ToolDescriptor: ToolDescriptor{Name: UIActivateToolName, Policy: KindSessionState, Modes: []ToolSurface{SurfaceChat, SurfaceServe}}, construct: func(*LocalToolRegistry) llm.Tool { return &UIExtensionsTool{activate: true} }},
	{ToolDescriptor: ToolDescriptor{Name: ShowImageToolName, Policy: KindImage, Modes: commonSurfaces}, hiddenCompletion: true, construct: func(r *LocalToolRegistry) llm.Tool { return NewShowImageTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: ActivateSkillToolName, Policy: KindSkill, Modes: commonSurfaces}, nonLocal: true, construct: func(r *LocalToolRegistry) llm.Tool { return NewActivateSkillTool(nil, r.approval) }},
	{ToolDescriptor: ToolDescriptor{Name: SearchSkillsToolName, Policy: KindSkill, Modes: commonSurfaces}, nonLocal: true, construct: func(*LocalToolRegistry) llm.Tool { return NewSearchSkillsTool(nil) }},
	{ToolDescriptor: ToolDescriptor{Name: LiveNewSessionToolName, Policy: KindSessionState, Modes: liveSurfaces}, nonLocal: true, construct: func(*LocalToolRegistry) llm.Tool { return &LiveNewSessionTool{} }},
	{ToolDescriptor: ToolDescriptor{Name: LiveSwitchSessionToolName, Policy: KindSessionState, Modes: liveSurfaces}, nonLocal: true, construct: func(*LocalToolRegistry) llm.Tool { return &LiveSwitchSessionTool{} }},
	{ToolDescriptor: ToolDescriptor{Name: SessionDirectoryToolName, Policy: KindRead, Modes: []ToolSurface{SurfaceServe, SurfaceLive}}, nonLocal: true, construct: func(*LocalToolRegistry) llm.Tool { return &SessionDirectoryTool{} }},
	{ToolDescriptor: ToolDescriptor{Name: CreateGoalToolName, Policy: KindSessionState, Modes: commonSurfaces}, nonLocal: true, construct: func(*LocalToolRegistry) llm.Tool { return NewCreateGoalTool() }},
	{ToolDescriptor: ToolDescriptor{Name: UpdateGoalToolName, Policy: KindSessionState, Modes: commonSurfaces}, nonLocal: true, construct: func(*LocalToolRegistry) llm.Tool { return NewUpdateGoalTool() }},
	{ToolDescriptor: ToolDescriptor{Name: GetGoalToolName, Policy: KindRead, Modes: commonSurfaces}, nonLocal: true, construct: func(*LocalToolRegistry) llm.Tool { return NewGetGoalTool(nil) }},
	{ToolDescriptor: ToolDescriptor{Name: UpdateProgressToolName, Policy: KindSessionState, Modes: []ToolSurface{SurfaceCLI, SurfaceJobs}}, nonLocal: true, construct: func(*LocalToolRegistry) llm.Tool { return NewUpdateProgressTool() }},
	{ToolDescriptor: ToolDescriptor{Name: FinalizeProgressToolName, Policy: KindSessionState, Modes: []ToolSurface{SurfaceCLI, SurfaceJobs}}, nonLocal: true, construct: func(*LocalToolRegistry) llm.Tool { return NewFinalizeProgressTool() }},
}

func agentControlConstructor(name string) func(*LocalToolRegistry) llm.Tool {
	return func(r *LocalToolRegistry) llm.Tool {
		spawn, _ := r.tools[SpawnAgentToolName].(*SpawnAgentTool)
		return &agentControlTool{name: name, spawn: spawn}
	}
}

var builtinsByName = func() map[string]*builtinDescriptor {
	byName := make(map[string]*builtinDescriptor, len(builtinTools))
	for i := range builtinTools {
		byName[builtinTools[i].Name] = &builtinTools[i]
	}
	return byName
}()

func lookupBuiltin(name string) *builtinDescriptor { return builtinsByName[name] }

// ToolAvailableOn reports whether a built-in declares support for this surface.
// It does not override explicit configuration or runtime capability checks.
func ToolAvailableOn(name string, surface ToolSurface) bool {
	if d := lookupBuiltin(name); d != nil {
		for _, mode := range d.Modes {
			if mode == surface {
				return true
			}
		}
	}
	return false
}

// BuiltinToolDescriptors enumerates built-in metadata in stable registration order.
// The returned descriptors and slices can be modified without affecting the catalog.
func BuiltinToolDescriptors() []ToolDescriptor {
	result := make([]ToolDescriptor, 0, len(builtinTools))
	for _, d := range builtinTools {
		copy := d.ToolDescriptor
		copy.Modes = append([]ToolSurface(nil), d.Modes...)
		copy.Aliases = append([]string(nil), d.Aliases...)
		copy.Description = d.construct(&LocalToolRegistry{config: &ToolConfig{}, tools: map[string]llm.Tool{}}).Spec().Description
		result = append(result, copy)
	}
	return result
}

// ToolSpec returns the current schema and description for a built-in name.
func ToolSpec(name string) (llm.ToolSpec, bool) {
	d := lookupBuiltin(name)
	if d == nil {
		return llm.ToolSpec{}, false
	}
	return d.construct(&LocalToolRegistry{config: &ToolConfig{}, tools: map[string]llm.Tool{}}).Spec(), true
}

// LegacyDisabledName maps implicit tool replacements to their older deny-list name.
func LegacyDisabledName(name string) string {
	if d := lookupBuiltin(name); d != nil && len(d.Aliases) > 0 {
		return d.Aliases[0]
	}
	return ""
}

// ToolFlagHelp preserves the CLI's concise, ordered tool examples.
func ToolFlagHelp() string {
	var labels []string
	for _, d := range builtinTools {
		if d.FlagLabel != "" {
			labels = append(labels, d.FlagLabel)
		}
	}
	return "Enable local tools (comma-separated, or 'all'): " + strings.Join(labels, ",")
}

// ToolRenderHint identifies special presentation for a built-in, if any.
func ToolRenderHint(name string) string {
	if d := lookupBuiltin(name); d != nil {
		return d.RenderHint
	}
	return ""
}
