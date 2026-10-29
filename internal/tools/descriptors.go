package tools

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// ToolDescriptor is a read-only snapshot of a built-in tool's public metadata.
// The schema is obtained with ToolSpec rather than shared through this value.
type ToolDescriptor struct {
	Name           string
	Description    string
	Policy         ToolKind
	DefaultEnabled bool
	Aliases        []string
	FlagLabel      string
}

type builtinDescriptor struct {
	ToolDescriptor
	construct        func(*LocalToolRegistry) core.Tool
	pathCapable      bool
	guardianSelector bool
	requiresSpawn    bool
	hubCredentials   bool
	nonLocal         bool
	hiddenCompletion bool
}

var builtinTools = []builtinDescriptor{
	{ToolDescriptor: ToolDescriptor{Name: ReadFileToolName, Policy: KindRead, DefaultEnabled: true, FlagLabel: ReadFileToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewReadFileTool(r.approval, r.limits, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: WriteFileToolName, Policy: KindEdit, DefaultEnabled: true, FlagLabel: WriteFileToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewWriteFileTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: EditFileToolName, Policy: KindEdit, DefaultEnabled: true, FlagLabel: EditFileToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewEditFileTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: UnifiedDiffToolName, Policy: KindEdit, DefaultEnabled: true}, construct: func(r *LocalToolRegistry) core.Tool { return NewUnifiedDiffTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: ShellToolName, Policy: KindExecute, DefaultEnabled: true, FlagLabel: ShellToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewShellTool(r.approval, r.config, r.limits) }},
	{ToolDescriptor: ToolDescriptor{Name: GrepToolName, Policy: KindSearch, DefaultEnabled: true, FlagLabel: GrepToolName}, construct: func(r *LocalToolRegistry) core.Tool {
		return NewGrepTool(r.approval, r.limits, r.config).WithReranker(r.grepReranker)
	}, pathCapable: true, guardianSelector: true},
	{ToolDescriptor: ToolDescriptor{Name: GlobToolName, Policy: KindSearch, DefaultEnabled: true, FlagLabel: GlobToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewGlobTool(r.approval, r.config) }, pathCapable: true, guardianSelector: true},
	{ToolDescriptor: ToolDescriptor{Name: ViewImageToolName, Policy: KindRead, DefaultEnabled: true, FlagLabel: ViewImageToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewViewImageTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: ShowMediaToolName, Aliases: []string{ShowImageToolName}, Policy: KindImage, DefaultEnabled: true, FlagLabel: ShowMediaToolName}, construct: func(r *LocalToolRegistry) core.Tool {
		t := NewShowMediaTool(r.approval, r.config)
		t.SetPublisher(r.mediaPublisher)
		return t
	}, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: ImageGenerateToolName, Policy: KindImage, DefaultEnabled: true, FlagLabel: ImageGenerateToolName}, construct: func(r *LocalToolRegistry) core.Tool {
		return NewImageGenerateTool(r.approval, r.appConfig, r.config.ImageProvider, r.memoryStore, r.agent, r.sessionID, r.config)
	}, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: AskUserToolName, Policy: KindInteractive, DefaultEnabled: true, FlagLabel: AskUserToolName}, construct: func(*LocalToolRegistry) core.Tool { return NewAskUserTool() }},
	{ToolDescriptor: ToolDescriptor{Name: SpawnAgentToolName, Policy: KindAgent, DefaultEnabled: true, FlagLabel: "spawn_agent (includes wait_agent,continue_agent,cancel_agent,list_agents)"}, construct: func(r *LocalToolRegistry) core.Tool { return NewSpawnAgentTool(r.config.Spawn, 0) }},
	{ToolDescriptor: ToolDescriptor{Name: WaitAgentToolName, Policy: KindAgent, DefaultEnabled: true}, requiresSpawn: true, construct: agentControlConstructor(WaitAgentToolName)},
	{ToolDescriptor: ToolDescriptor{Name: ContinueAgentToolName, Policy: KindAgent, DefaultEnabled: true}, requiresSpawn: true, construct: agentControlConstructor(ContinueAgentToolName)},
	{ToolDescriptor: ToolDescriptor{Name: CancelAgentToolName, Policy: KindAgent, DefaultEnabled: true}, requiresSpawn: true, construct: agentControlConstructor(CancelAgentToolName)},
	{ToolDescriptor: ToolDescriptor{Name: ListAgentsToolName, Policy: KindAgent, DefaultEnabled: true}, requiresSpawn: true, construct: agentControlConstructor(ListAgentsToolName)},
	{ToolDescriptor: ToolDescriptor{Name: QueueAgentToolName, Policy: KindAgent, DefaultEnabled: true, FlagLabel: QueueAgentToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewQueueAgentTool(r.approval, r.config) }},
	{ToolDescriptor: ToolDescriptor{Name: WaitForJobsToolName, Policy: KindAgent, DefaultEnabled: true, FlagLabel: WaitForJobsToolName}, construct: func(*LocalToolRegistry) core.Tool { return NewWaitForJobsTool() }},
	{ToolDescriptor: ToolDescriptor{Name: RunAgentScriptToolName, Policy: KindExecute, DefaultEnabled: true}, construct: func(r *LocalToolRegistry) core.Tool { return NewRunAgentScriptTool(r.config, r.limits) }},
	{ToolDescriptor: ToolDescriptor{Name: InitiateHandoverToolName, Policy: KindAgent, DefaultEnabled: true}, construct: func(*LocalToolRegistry) core.Tool { return NewInitiateHandoverTool() }},
	{ToolDescriptor: ToolDescriptor{Name: HubDelegateToolName, Policy: KindAgent, DefaultEnabled: true}, hubCredentials: true, construct: func(*LocalToolRegistry) core.Tool { return NewHubDelegateTool() }},
	{ToolDescriptor: ToolDescriptor{Name: HubCheckDelegationToolName, Policy: KindAgent, DefaultEnabled: true}, hubCredentials: true, construct: func(*LocalToolRegistry) core.Tool { return NewHubCheckDelegationTool() }},
	{ToolDescriptor: ToolDescriptor{Name: ManageWorkspaceToolName, Policy: KindSessionState}, construct: func(r *LocalToolRegistry) core.Tool { return NewManageWorkspaceTool(r.approval, r.config) }},
	{ToolDescriptor: ToolDescriptor{Name: UpdatePlanToolName, Policy: KindSessionState}, construct: func(r *LocalToolRegistry) core.Tool { return r.newSessionStateTool(UpdatePlanToolName) }},
	{ToolDescriptor: ToolDescriptor{Name: LiveSettingsToolName, Policy: KindSessionState}, construct: func(r *LocalToolRegistry) core.Tool { return r.newSessionStateTool(LiveSettingsToolName) }},
	{ToolDescriptor: ToolDescriptor{Name: UIGetSourceToolName, Policy: KindRead}, construct: func(r *LocalToolRegistry) core.Tool { return &UIGetSourceTool{approval: r.approval, config: r.config} }},
	{ToolDescriptor: ToolDescriptor{Name: UIExtensionsToolName, Policy: KindSessionState}, construct: func(*LocalToolRegistry) core.Tool { return &UIExtensionsTool{} }},
	{ToolDescriptor: ToolDescriptor{Name: UIActivateToolName, Policy: KindSessionState}, construct: func(*LocalToolRegistry) core.Tool { return &UIExtensionsTool{activate: true} }},
	{ToolDescriptor: ToolDescriptor{Name: ShowImageToolName, Policy: KindImage}, hiddenCompletion: true, construct: func(r *LocalToolRegistry) core.Tool { return NewShowImageTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: ActivateSkillToolName, Policy: KindSkill}, nonLocal: true, construct: func(r *LocalToolRegistry) core.Tool { return NewActivateSkillTool(nil, r.approval) }},
	{ToolDescriptor: ToolDescriptor{Name: SearchSkillsToolName, Policy: KindSkill}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return NewSearchSkillsTool(nil) }},
	{ToolDescriptor: ToolDescriptor{Name: LiveNewSessionToolName, Policy: KindSessionState}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return &LiveNewSessionTool{} }},
	{ToolDescriptor: ToolDescriptor{Name: LiveSwitchSessionToolName, Policy: KindSessionState}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return &LiveSwitchSessionTool{} }},
	{ToolDescriptor: ToolDescriptor{Name: SessionDirectoryToolName, Policy: KindRead}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return &SessionDirectoryTool{} }},
	{ToolDescriptor: ToolDescriptor{Name: CreateGoalToolName, Policy: KindSessionState}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return NewCreateGoalTool() }},
	{ToolDescriptor: ToolDescriptor{Name: UpdateGoalToolName, Policy: KindSessionState}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return NewUpdateGoalTool() }},
	{ToolDescriptor: ToolDescriptor{Name: GetGoalToolName, Policy: KindRead}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return NewGetGoalTool(nil) }},
	{ToolDescriptor: ToolDescriptor{Name: UpdateProgressToolName, Policy: KindSessionState}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return NewUpdateProgressTool() }},
	{ToolDescriptor: ToolDescriptor{Name: FinalizeProgressToolName, Policy: KindSessionState}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return NewFinalizeProgressTool() }},
}

func agentControlConstructor(name string) func(*LocalToolRegistry) core.Tool {
	return func(r *LocalToolRegistry) core.Tool {
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

// BuiltinToolDescriptors enumerates built-in metadata in stable registration order.
// The returned descriptors and slices can be modified without affecting the catalog.
func BuiltinToolDescriptors() []ToolDescriptor {
	result := make([]ToolDescriptor, 0, len(builtinTools))
	for _, d := range builtinTools {
		copy := d.ToolDescriptor
		copy.Aliases = append([]string(nil), d.Aliases...)
		copy.Description = d.construct(&LocalToolRegistry{config: &ToolConfig{}, tools: map[string]core.Tool{}}).Spec().Description
		result = append(result, copy)
	}
	return result
}

// ToolSpec returns the current schema and description for a built-in name.
func ToolSpec(name string) (core.ToolSpec, bool) {
	d := lookupBuiltin(name)
	if d == nil {
		return core.ToolSpec{}, false
	}
	return d.construct(&LocalToolRegistry{config: &ToolConfig{}, tools: map[string]core.Tool{}}).Spec(), true
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
