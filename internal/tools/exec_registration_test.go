package tools

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

func TestExecRegistersLifecycleControls(t *testing.T) {
	manager, err := NewToolManager(&ToolConfig{Enabled: []string{CodeExecToolName}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.ApprovalMgr.Close()
	for _, name := range []string{CodeExecToolName, CodeWaitToolName, CodeCancelToolName} {
		tool, ok := manager.Registry.Get(name)
		if !ok || tool.Spec().Name != name || !manager.Registry.IsEnabled(name) {
			t.Fatalf("exec must enable and register %s", name)
		}
		if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
			t.Fatalf("%s must fail closed without an engine-owned controller", name)
		}
		if slices.Contains(StandardToolNames(), name) {
			t.Fatalf("opt-in code mode widened the global implicit set: %s", name)
		}
	}
	if _, ok := manager.Registry.Get(CodeModeToolName); ok {
		t.Fatal("exec should not advertise the legacy synchronous code_mode tool")
	}
	if !ValidToolName(CodeModeToolName) || slices.Contains(ValidToolNames(), CodeModeToolName) {
		t.Fatal("legacy code_mode must remain explicitly accepted without being suggested")
	}
}

func TestExecControlsRequireExecutionCapability(t *testing.T) {
	for _, name := range []string{CodeWaitToolName, CodeCancelToolName} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewToolManager(&ToolConfig{Enabled: []string{name}}, nil); err == nil {
				t.Fatalf("%s cannot be enabled without exec", name)
			}
		})
	}
}
