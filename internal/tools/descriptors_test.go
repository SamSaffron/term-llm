package tools

import (
	"slices"
	"testing"
)

func TestBuiltinDescriptorsComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, descriptor := range BuiltinToolDescriptors() {
		if descriptor.Name == "" || seen[descriptor.Name] {
			t.Fatalf("missing or duplicate tool name: %q", descriptor.Name)
		}
		seen[descriptor.Name] = true
		d := lookupBuiltin(descriptor.Name)
		if d == nil || d.construct == nil || descriptor.Policy == "" || descriptor.Description == "" {
			t.Errorf("incomplete descriptor: %s", descriptor.Name)
			continue
		}
		spec, ok := ToolSpec(descriptor.Name)
		if !ok || spec.Name != descriptor.Name || spec.Description == "" || spec.Schema == nil {
			t.Errorf("invalid spec for %s: %#v", descriptor.Name, spec)
		}
		if !d.nonLocal && !ValidToolName(descriptor.Name) {
			t.Errorf("local tool %s does not resolve", descriptor.Name)
		}
	}
	for _, name := range ValidToolNames() {
		if !seen[name] {
			t.Errorf("valid name %s lacks descriptor", name)
		}
	}
	for _, name := range StandardToolNames() {
		if !seen[name] {
			t.Errorf("default name %s lacks descriptor", name)
		}
	}
	for _, d := range builtinTools {
		if d.FlagLabel != "" && !ValidToolName(d.Name) {
			t.Errorf("CLI help tool %s does not resolve", d.Name)
		}
	}

}

func TestBuiltinDescriptorLegacyLists(t *testing.T) {
	restore := resetHubDelegationForTest()
	defer restore()
	want := []string{ReadFileToolName, WriteFileToolName, EditFileToolName, UnifiedDiffToolName, ShellToolName, GrepToolName, GlobToolName, ViewImageToolName, ShowMediaToolName, ImageGenerateToolName, AskUserToolName, SpawnAgentToolName, WaitAgentToolName, ContinueAgentToolName, CancelAgentToolName, ListAgentsToolName, QueueAgentToolName, WaitForJobsToolName, RunAgentScriptToolName, InitiateHandoverToolName}
	if got := StandardToolNames(); !slices.Equal(got, want) {
		t.Fatalf("implicit tools = %v, want %v", got, want)
	}
	valid := append(append([]string(nil), want...), HubDelegateToolName, HubCheckDelegationToolName, ManageWorkspaceToolName, UpdatePlanToolName, LiveSettingsToolName, UIGetSourceToolName, UIExtensionsToolName, UIActivateToolName)
	if got := ValidToolNames(); !slices.Equal(got, valid) {
		t.Fatalf("completion tools = %v, want %v", got, valid)
	}
	const help = "Enable local tools (comma-separated, or 'all'): read_file,write_file,edit_file,shell,grep,glob,view_image,show_media,image_generate,ask_user,spawn_agent (includes wait_agent,continue_agent,cancel_agent,list_agents),queue_agent,wait_for_jobs"
	if got := ToolFlagHelp(); got != help {
		t.Fatalf("tools flag help = %q, want %q", got, help)
	}
}

func TestBuiltinDescriptorSnapshotAndLegacyName(t *testing.T) {
	descriptors := BuiltinToolDescriptors()
	for i := range descriptors {
		if descriptors[i].Name == ShowMediaToolName {
			descriptors[i].Aliases[0] = "changed"
			break
		}
	}
	if got := LegacyDisabledName(ShowMediaToolName); got != ShowImageToolName {
		t.Fatalf("legacy name = %q", got)
	}
	if got := GetToolKind(ShowMediaToolName); got != KindImage {
		t.Fatalf("policy = %q", got)
	}
}
