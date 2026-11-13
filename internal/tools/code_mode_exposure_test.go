package tools

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestBuiltinCodeModeExposureInventory(t *testing.T) {
	// This explicit inventory is independent of permission categories. Adding a
	// descriptor must make a deliberate exposure decision and update this test.
	allowed := []string{
		"read_file", "write_file", "edit_file", "unified_diff", "shell", "grep", "glob",
		"view_image", "show_media", "show_image", "image_generate", "spawn_agent",
		"wait_agent", "continue_agent", "cancel_agent", "list_agents", "queue_agent",
		"wait_for_jobs", "run_agent_script", "update_plan", "ui_get_source", "search_skills", "get_goal",
	}
	direct := []string{
		"code_mode", "spawn_script", "wait_script", "cancel_script", "ask_user", "initiate_handover",
		"hub_delegate", "hub_check_delegation", "manage_workspace", "live_settings", "ui_extensions",
		"ui_activate_extensions", "activate_skill", "live_new_session", "live_switch_session", "session_directory",
		"create_goal", "update_goal", "update_progress", "finalize_progress",
	}
	want := make(map[string]bool, len(allowed)+len(direct))
	for _, name := range allowed {
		want[name] = true
	}
	for _, name := range direct {
		want[name] = false
	}
	if len(want) != len(builtinTools) {
		t.Fatalf("inventory has %d tools, descriptors have %d", len(want), len(builtinTools))
	}
	for _, d := range builtinTools {
		t.Run(d.Name, func(t *testing.T) {
			expected, ok := want[d.Name]
			if !ok {
				t.Fatal("tool lacks explicit exposure inventory entry")
			}
			if d.CodeModeExposure.Allowed != expected || (!expected && d.CodeModeExposure.Reason == "") {
				t.Fatalf("descriptor exposure = %+v, want allowed=%v with explicit direct reason", d.CodeModeExposure, expected)
			}
			tool := d.construct(&LocalToolRegistry{config: &ToolConfig{}, tools: map[string]core.Tool{}})
			provider, ok := tool.(core.CodeModeExposureProvider)
			if !ok {
				t.Fatalf("%T lacks instance exposure", tool)
			}
			if got := provider.CodeModeExposure(); got != d.CodeModeExposure {
				t.Fatalf("instance exposure = %+v, descriptor = %+v", got, d.CodeModeExposure)
			}
			for _, alias := range d.Aliases {
				if got := builtinCodeModeExposure(alias); got != d.CodeModeExposure {
					t.Fatalf("alias %s exposure = %+v, canonical = %+v", alias, got, d.CodeModeExposure)
				}
			}
		})
	}
}

func TestNonDescriptorScriptExposure(t *testing.T) {
	custom := &CustomScriptTool{}
	if !custom.CodeModeExposure().Allowed {
		t.Fatal("custom/activated skill script excluded")
	}
	// The configurable output name is immaterial to finishing semantics.
	output := &SetOutputTool{}
	if exposure := output.CodeModeExposure(); exposure.Allowed || exposure.Reason != "finishing_tool" {
		t.Fatalf("set_output exposure = %+v", exposure)
	}
	if exposure := builtinCodeModeExposure("future_builtin"); exposure.Allowed || exposure.Reason == "" {
		t.Fatalf("unknown builtin exposure = %+v", exposure)
	}
}
