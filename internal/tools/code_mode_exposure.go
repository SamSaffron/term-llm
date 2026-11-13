package tools

import "github.com/samsaffron/term-llm/internal/llm/core"

// builtinCodeModeExposure is used only by concrete builtin implementations, never
// to classify an arbitrary registered tool by its public name. Descriptors are
// the single source of builtin exposure policy; aliases have the same policy.
func builtinCodeModeExposure(name string) core.CodeModeExposure {
	if d := lookupBuiltin(name); d != nil {
		return d.CodeModeExposure
	}
	return core.CodeModeExposure{Reason: "undeclared_exposure"}
}

func (*ReadFileTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(ReadFileToolName)
}
func (*WriteFileTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(WriteFileToolName)
}
func (*EditFileTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(EditFileToolName)
}
func (*UnifiedDiffTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(UnifiedDiffToolName)
}
func (*ShellTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(ShellToolName)
}
func (*GrepTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(GrepToolName)
}
func (*GlobTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(GlobToolName)
}
func (*ViewImageTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(ViewImageToolName)
}
func (*ShowMediaTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(ShowMediaToolName)
}
func (*ShowImageTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(ShowImageToolName)
}
func (*ImageGenerateTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(ImageGenerateToolName)
}
func (*AskUserTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(AskUserToolName)
}
func (*SpawnAgentTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(SpawnAgentToolName)
}
func (*QueueAgentTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(QueueAgentToolName)
}
func (*WaitForJobsTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(WaitForJobsToolName)
}
func (*RunAgentScriptTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(RunAgentScriptToolName)
}
func (*InitiateHandoverTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(InitiateHandoverToolName)
}
func (*HubDelegateTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(HubDelegateToolName)
}
func (*HubCheckDelegationTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(HubCheckDelegationToolName)
}
func (*ManageWorkspaceTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(ManageWorkspaceToolName)
}
func (*UpdatePlanTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(UpdatePlanToolName)
}
func (*LiveSettingsTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(LiveSettingsToolName)
}
func (*UIGetSourceTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(UIGetSourceToolName)
}
func (*ActivateSkillTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(ActivateSkillToolName)
}
func (*SearchSkillsTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(SearchSkillsToolName)
}
func (*LiveNewSessionTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(LiveNewSessionToolName)
}
func (*LiveSwitchSessionTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(LiveSwitchSessionToolName)
}
func (*SessionDirectoryTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(SessionDirectoryToolName)
}
func (t *agentControlTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(t.name)
}
func (t *goalTool) CodeModeExposure() core.CodeModeExposure { return builtinCodeModeExposure(t.name) }
func (t *progressTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(t.name)
}
func (t *UIExtensionsTool) CodeModeExposure() core.CodeModeExposure {
	return builtinCodeModeExposure(t.Spec().Name)
}

// Custom and activated skill scripts already share this concrete implementation.
// Exposure does not bypass their source, trust, or approval checks.
func (*CustomScriptTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Allowed: true}
}

func (*SetOutputTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Reason: "finishing_tool"}
}
