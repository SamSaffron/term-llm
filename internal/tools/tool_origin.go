package tools

import "github.com/term-llm/term-llm/internal/llm/core"

// Builtin origin is declared on concrete local implementations. Remote tools
// cannot inherit it by choosing a builtin-looking public name.
func builtinToolOrigin(name string) core.ToolOrigin {
	return core.ToolOrigin{Class: core.ToolOriginBuiltin, ID: name}
}

func (*ReadFileTool) ToolOrigin() core.ToolOrigin    { return builtinToolOrigin(ReadFileToolName) }
func (*WriteFileTool) ToolOrigin() core.ToolOrigin   { return builtinToolOrigin(WriteFileToolName) }
func (*EditFileTool) ToolOrigin() core.ToolOrigin    { return builtinToolOrigin(EditFileToolName) }
func (*UnifiedDiffTool) ToolOrigin() core.ToolOrigin { return builtinToolOrigin(UnifiedDiffToolName) }
func (*ShellTool) ToolOrigin() core.ToolOrigin       { return builtinToolOrigin(ShellToolName) }
func (*GrepTool) ToolOrigin() core.ToolOrigin        { return builtinToolOrigin(GrepToolName) }
func (*GlobTool) ToolOrigin() core.ToolOrigin        { return builtinToolOrigin(GlobToolName) }
func (*ViewImageTool) ToolOrigin() core.ToolOrigin   { return builtinToolOrigin(ViewImageToolName) }
func (*ShowMediaTool) ToolOrigin() core.ToolOrigin   { return builtinToolOrigin(ShowMediaToolName) }
func (*ShowImageTool) ToolOrigin() core.ToolOrigin   { return builtinToolOrigin(ShowImageToolName) }
func (*ImageGenerateTool) ToolOrigin() core.ToolOrigin {
	return builtinToolOrigin(ImageGenerateToolName)
}
func (*AskUserTool) ToolOrigin() core.ToolOrigin     { return builtinToolOrigin(AskUserToolName) }
func (*SpawnAgentTool) ToolOrigin() core.ToolOrigin  { return builtinToolOrigin(SpawnAgentToolName) }
func (*QueueAgentTool) ToolOrigin() core.ToolOrigin  { return builtinToolOrigin(QueueAgentToolName) }
func (*WaitForJobsTool) ToolOrigin() core.ToolOrigin { return builtinToolOrigin(WaitForJobsToolName) }
func (*RunAgentScriptTool) ToolOrigin() core.ToolOrigin {
	return builtinToolOrigin(RunAgentScriptToolName)
}
func (*InitiateHandoverTool) ToolOrigin() core.ToolOrigin {
	return builtinToolOrigin(InitiateHandoverToolName)
}
func (*HubDelegateTool) ToolOrigin() core.ToolOrigin { return builtinToolOrigin(HubDelegateToolName) }
func (*HubCheckDelegationTool) ToolOrigin() core.ToolOrigin {
	return builtinToolOrigin(HubCheckDelegationToolName)
}
func (*ManageWorkspaceTool) ToolOrigin() core.ToolOrigin {
	return builtinToolOrigin(ManageWorkspaceToolName)
}
func (*UpdatePlanTool) ToolOrigin() core.ToolOrigin   { return builtinToolOrigin(UpdatePlanToolName) }
func (*LiveSettingsTool) ToolOrigin() core.ToolOrigin { return builtinToolOrigin(LiveSettingsToolName) }
func (*UIGetSourceTool) ToolOrigin() core.ToolOrigin  { return builtinToolOrigin(UIGetSourceToolName) }
func (*ActivateSkillTool) ToolOrigin() core.ToolOrigin {
	return builtinToolOrigin(ActivateSkillToolName)
}
func (*SearchSkillsTool) ToolOrigin() core.ToolOrigin { return builtinToolOrigin(SearchSkillsToolName) }
func (*LiveNewSessionTool) ToolOrigin() core.ToolOrigin {
	return builtinToolOrigin(LiveNewSessionToolName)
}
func (*LiveSwitchSessionTool) ToolOrigin() core.ToolOrigin {
	return builtinToolOrigin(LiveSwitchSessionToolName)
}
func (*SessionDirectoryTool) ToolOrigin() core.ToolOrigin {
	return builtinToolOrigin(SessionDirectoryToolName)
}
func (t *agentControlTool) ToolOrigin() core.ToolOrigin { return builtinToolOrigin(t.name) }
func (t *goalTool) ToolOrigin() core.ToolOrigin         { return builtinToolOrigin(t.name) }
func (t *progressTool) ToolOrigin() core.ToolOrigin     { return builtinToolOrigin(t.name) }
func (t *UIExtensionsTool) ToolOrigin() core.ToolOrigin { return builtinToolOrigin(t.Spec().Name) }
func (t *SetOutputTool) ToolOrigin() core.ToolOrigin    { return builtinToolOrigin(t.name) }
