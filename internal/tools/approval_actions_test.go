package tools

import "testing"

func TestApprovalGovernedClassifiesBuiltins(t *testing.T) {
	tests := []struct {
		name     string
		governed bool
	}{
		{ReadFileToolName, true},
		{WriteFileToolName, true},
		{EditFileToolName, true},
		{UnifiedDiffToolName, true},
		{ShellToolName, true},
		{GrepToolName, true},
		{GlobToolName, true},
		{ViewImageToolName, true},
		{ShowMediaToolName, true},
		{ShowImageToolName, true},
		{ImageGenerateToolName, true},
		{RunAgentScriptToolName, true},
		{SpawnAgentToolName, true},
		{QueueAgentToolName, true},
		{ManageWorkspaceToolName, true},
		{AskUserToolName, false},
		{UpdatePlanToolName, false},
		{WaitAgentToolName, false},
		{ContinueAgentToolName, false},
		{CancelAgentToolName, false},
		{ListAgentsToolName, false},
		{ActivateSkillToolName, false},
		{SearchSkillsToolName, false},
		{SessionDirectoryToolName, false},
		{GetGoalToolName, false},
	}
	for _, test := range tests {
		if got := ApprovalGoverned(test.name); got != test.governed {
			t.Errorf("ApprovalGoverned(%q) = %v, want %v", test.name, got, test.governed)
		}
	}
}

func TestApprovalGovernedTreatsUnknownToolsAsGoverned(t *testing.T) {
	// Custom agent tools and MCP tools execute outside the built-in checks.
	for _, name := range []string{"my_custom_tool", "mcp__github__search", "skill_script"} {
		if !ApprovalGoverned(name) {
			t.Errorf("ApprovalGoverned(%q) = false, want true for an unknown tool", name)
		}
	}
}

func TestAnyApprovalGoverned(t *testing.T) {
	if AnyApprovalGoverned(nil) {
		t.Fatal("no tools must not be governed")
	}
	if AnyApprovalGoverned([]string{AskUserToolName, ActivateSkillToolName, UpdatePlanToolName}) {
		t.Fatal("approval-free tool sets must not be governed")
	}
	if !AnyApprovalGoverned([]string{AskUserToolName, ShellToolName}) {
		t.Fatal("a shell tool is approval-governed")
	}
	if !AnyApprovalGoverned([]string{"custom_tool"}) {
		t.Fatal("an unknown custom tool is approval-governed")
	}
}
