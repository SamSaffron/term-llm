package tools

import (
	codemodetool "github.com/samsaffron/term-llm/internal/codemode/tool"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// ToolDescriptor is a read-only snapshot of a built-in tool's public metadata.
// The schema is obtained with ToolSpec rather than shared through this value.
type ToolDescriptor struct {
	Name             string
	Description      string
	Policy           ToolKind
	DefaultEnabled   bool
	Aliases          []string
	FlagLabel        string
	CodeModeExposure core.CodeModeExposure
}

type builtinDescriptor struct {
	ToolDescriptor
	construct        func(*LocalToolRegistry) core.Tool
	pathCapable      bool
	guardianSelector bool
	requiresTool     string
	hubCredentials   bool
	nonLocal         bool
	hiddenCompletion bool
}

var builtinTools = []builtinDescriptor{
	{ToolDescriptor: ToolDescriptor{Name: CodeModeToolName, CodeModeExposure: core.CodeModeExposure{Reason: "script_lifecycle"}, Policy: KindExecute, DefaultEnabled: false}, hiddenCompletion: true, construct: func(*LocalToolRegistry) core.Tool { return codemodetool.New() }},
	{ToolDescriptor: ToolDescriptor{Name: CodeExecToolName, CodeModeExposure: core.CodeModeExposure{Reason: "script_lifecycle"}, Policy: KindExecute, DefaultEnabled: false, FlagLabel: "spawn_script (includes wait_script,cancel_script)"}, construct: func(*LocalToolRegistry) core.Tool { return codemodetool.NewExec() }},
	{ToolDescriptor: ToolDescriptor{Name: CodeWaitToolName, CodeModeExposure: core.CodeModeExposure{Reason: "script_lifecycle"}, Policy: KindExecute, DefaultEnabled: false}, requiresTool: CodeExecToolName, construct: func(*LocalToolRegistry) core.Tool { return codemodetool.NewWait() }},
	{ToolDescriptor: ToolDescriptor{Name: CodeCancelToolName, CodeModeExposure: core.CodeModeExposure{Reason: "script_lifecycle"}, Policy: KindExecute, DefaultEnabled: false}, requiresTool: CodeExecToolName, construct: func(*LocalToolRegistry) core.Tool { return codemodetool.NewCancel() }},
	{ToolDescriptor: ToolDescriptor{Name: ReadFileToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindRead, DefaultEnabled: true, FlagLabel: ReadFileToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewReadFileTool(r.approval, r.limits, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: WriteFileToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindEdit, DefaultEnabled: true, FlagLabel: WriteFileToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewWriteFileTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: EditFileToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindEdit, DefaultEnabled: true, FlagLabel: EditFileToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewEditFileTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: UnifiedDiffToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindEdit, DefaultEnabled: true}, construct: func(r *LocalToolRegistry) core.Tool { return NewUnifiedDiffTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: ShellToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindExecute, DefaultEnabled: true, FlagLabel: ShellToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewShellTool(r.approval, r.config, r.limits) }},
	{ToolDescriptor: ToolDescriptor{Name: GrepToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindSearch, DefaultEnabled: true, FlagLabel: GrepToolName}, construct: func(r *LocalToolRegistry) core.Tool {
		return NewGrepTool(r.approval, r.limits, r.config).WithReranker(r.grepReranker)
	}, pathCapable: true, guardianSelector: true},
	{ToolDescriptor: ToolDescriptor{Name: GlobToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindSearch, DefaultEnabled: true, FlagLabel: GlobToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewGlobTool(r.approval, r.config) }, pathCapable: true, guardianSelector: true},
	{ToolDescriptor: ToolDescriptor{Name: ViewImageToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindRead, DefaultEnabled: true, FlagLabel: ViewImageToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewViewImageTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: ShowMediaToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Aliases: []string{ShowImageToolName}, Policy: KindImage, DefaultEnabled: true, FlagLabel: ShowMediaToolName}, construct: func(r *LocalToolRegistry) core.Tool {
		t := NewShowMediaTool(r.approval, r.config)
		t.SetPublisher(r.mediaPublisher)
		return t
	}, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: ImageGenerateToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindImage, DefaultEnabled: true, FlagLabel: ImageGenerateToolName}, construct: func(r *LocalToolRegistry) core.Tool {
		return NewImageGenerateTool(r.approval, r.appConfig, r.config.ImageProvider, r.memoryStore, r.agent, r.sessionID, r.config)
	}, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: AskUserToolName, CodeModeExposure: core.CodeModeExposure{Reason: "foreground_interaction"}, Policy: KindInteractive, DefaultEnabled: true, FlagLabel: AskUserToolName}, construct: func(*LocalToolRegistry) core.Tool { return NewAskUserTool() }},
	{ToolDescriptor: ToolDescriptor{Name: SpawnAgentToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindAgent, DefaultEnabled: true, FlagLabel: "spawn_agent (includes wait_agent,continue_agent,cancel_agent,list_agents)"}, construct: func(r *LocalToolRegistry) core.Tool { return NewSpawnAgentTool(r.config.Spawn, 0) }},
	{ToolDescriptor: ToolDescriptor{Name: WaitAgentToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindAgent, DefaultEnabled: true}, requiresTool: SpawnAgentToolName, construct: agentControlConstructor(WaitAgentToolName)},
	{ToolDescriptor: ToolDescriptor{Name: ContinueAgentToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindAgent, DefaultEnabled: true}, requiresTool: SpawnAgentToolName, construct: agentControlConstructor(ContinueAgentToolName)},
	{ToolDescriptor: ToolDescriptor{Name: CancelAgentToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindAgent, DefaultEnabled: true}, requiresTool: SpawnAgentToolName, construct: agentControlConstructor(CancelAgentToolName)},
	{ToolDescriptor: ToolDescriptor{Name: ListAgentsToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindAgent, DefaultEnabled: true}, requiresTool: SpawnAgentToolName, construct: agentControlConstructor(ListAgentsToolName)},
	{ToolDescriptor: ToolDescriptor{Name: QueueAgentToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindAgent, DefaultEnabled: true, FlagLabel: QueueAgentToolName}, construct: func(r *LocalToolRegistry) core.Tool { return NewQueueAgentTool(r.approval, r.config) }},
	{ToolDescriptor: ToolDescriptor{Name: WaitForJobsToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindAgent, DefaultEnabled: true, FlagLabel: WaitForJobsToolName}, construct: func(*LocalToolRegistry) core.Tool { return NewWaitForJobsTool() }},
	{ToolDescriptor: ToolDescriptor{Name: RunAgentScriptToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindExecute, DefaultEnabled: true}, construct: func(r *LocalToolRegistry) core.Tool { return NewRunAgentScriptTool(r.config, r.limits) }},
	{ToolDescriptor: ToolDescriptor{Name: InitiateHandoverToolName, CodeModeExposure: core.CodeModeExposure{Reason: "runtime_handover"}, Policy: KindAgent, DefaultEnabled: true}, construct: func(*LocalToolRegistry) core.Tool { return NewInitiateHandoverTool() }},
	{ToolDescriptor: ToolDescriptor{Name: HubDelegateToolName, CodeModeExposure: core.CodeModeExposure{Reason: "hub_binding_unreviewed"}, Policy: KindAgent, DefaultEnabled: true}, hubCredentials: true, construct: func(*LocalToolRegistry) core.Tool { return NewHubDelegateTool() }},
	{ToolDescriptor: ToolDescriptor{Name: HubCheckDelegationToolName, CodeModeExposure: core.CodeModeExposure{Reason: "hub_binding_unreviewed"}, Policy: KindAgent, DefaultEnabled: true}, hubCredentials: true, construct: func(*LocalToolRegistry) core.Tool { return NewHubCheckDelegationTool() }},
	{ToolDescriptor: ToolDescriptor{Name: ManageWorkspaceToolName, CodeModeExposure: core.CodeModeExposure{Reason: "authority_mutation"}, Policy: KindSessionState}, construct: func(r *LocalToolRegistry) core.Tool { return NewManageWorkspaceTool(r.approval, r.config) }},
	{ToolDescriptor: ToolDescriptor{Name: UpdatePlanToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindSessionState}, construct: func(r *LocalToolRegistry) core.Tool { return r.newSessionStateTool(UpdatePlanToolName) }},
	{ToolDescriptor: ToolDescriptor{Name: LiveSettingsToolName, CodeModeExposure: core.CodeModeExposure{Reason: "runtime_mutation"}, Policy: KindSessionState}, construct: func(r *LocalToolRegistry) core.Tool { return r.newSessionStateTool(LiveSettingsToolName) }},
	{ToolDescriptor: ToolDescriptor{Name: UIGetSourceToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindRead}, construct: func(r *LocalToolRegistry) core.Tool { return &UIGetSourceTool{approval: r.approval, config: r.config} }},
	{ToolDescriptor: ToolDescriptor{Name: UIExtensionsToolName, CodeModeExposure: core.CodeModeExposure{Reason: "extension_lifecycle"}, Policy: KindSessionState}, construct: func(*LocalToolRegistry) core.Tool { return &UIExtensionsTool{} }},
	{ToolDescriptor: ToolDescriptor{Name: UIActivateToolName, CodeModeExposure: core.CodeModeExposure{Reason: "extension_lifecycle"}, Policy: KindSessionState}, construct: func(*LocalToolRegistry) core.Tool { return &UIExtensionsTool{activate: true} }},
	{ToolDescriptor: ToolDescriptor{Name: ShowImageToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindImage}, hiddenCompletion: true, construct: func(r *LocalToolRegistry) core.Tool { return NewShowImageTool(r.approval, r.config) }, pathCapable: true},
	{ToolDescriptor: ToolDescriptor{Name: ActivateSkillToolName, CodeModeExposure: core.CodeModeExposure{Reason: "instruction_mutation"}, Policy: KindSkill}, nonLocal: true, construct: func(r *LocalToolRegistry) core.Tool { return NewActivateSkillTool(nil, r.approval) }},
	{ToolDescriptor: ToolDescriptor{Name: SearchSkillsToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindSkill}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return NewSearchSkillsTool(nil) }},
	{ToolDescriptor: ToolDescriptor{Name: LiveNewSessionToolName, CodeModeExposure: core.CodeModeExposure{Reason: "session_ownership"}, Policy: KindSessionState}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return &LiveNewSessionTool{} }},
	{ToolDescriptor: ToolDescriptor{Name: LiveSwitchSessionToolName, CodeModeExposure: core.CodeModeExposure{Reason: "session_ownership"}, Policy: KindSessionState}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return &LiveSwitchSessionTool{} }},
	{ToolDescriptor: ToolDescriptor{Name: SessionDirectoryToolName, CodeModeExposure: core.CodeModeExposure{Reason: "live_control_directory"}, Policy: KindRead}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return &SessionDirectoryTool{} }},
	{ToolDescriptor: ToolDescriptor{Name: CreateGoalToolName, CodeModeExposure: core.CodeModeExposure{Reason: "host_tracked_mutation"}, Policy: KindSessionState}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return NewCreateGoalTool() }},
	{ToolDescriptor: ToolDescriptor{Name: UpdateGoalToolName, CodeModeExposure: core.CodeModeExposure{Reason: "host_tracked_mutation"}, Policy: KindSessionState}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return NewUpdateGoalTool() }},
	{ToolDescriptor: ToolDescriptor{Name: GetGoalToolName, CodeModeExposure: core.CodeModeExposure{Allowed: true}, Policy: KindRead}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return NewGetGoalTool(nil) }},
	{ToolDescriptor: ToolDescriptor{Name: UpdateProgressToolName, CodeModeExposure: core.CodeModeExposure{Reason: "host_tracked_mutation"}, Policy: KindSessionState}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return NewUpdateProgressTool() }},
	{ToolDescriptor: ToolDescriptor{Name: FinalizeProgressToolName, CodeModeExposure: core.CodeModeExposure{Reason: "finishing_tool"}, Policy: KindSessionState}, nonLocal: true, construct: func(*LocalToolRegistry) core.Tool { return NewFinalizeProgressTool() }},
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

const (
	// CodeModeToolName is the explicit legacy synchronous compatibility tool.
	CodeModeToolName   = codemodetool.Name
	CodeExecToolName   = codemodetool.ExecName
	CodeWaitToolName   = codemodetool.WaitName
	CodeCancelToolName = codemodetool.CancelName
)

// IsCodeModeTool includes execution and observation controls, not nested effects.
func IsCodeModeTool(name string) bool {
	return name == CodeModeToolName || codemodetool.IsControl(name)
}
