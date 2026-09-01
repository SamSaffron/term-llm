package tools

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestFilterToolSpecsForApprovalModeOmitsManageWorkspaceOnlyInYolo(t *testing.T) {
	specs := []llm.ToolSpec{
		{Name: ReadFileToolName},
		{Name: ManageWorkspaceToolName},
		{Name: ShellToolName},
	}
	approval := NewApprovalManager(NewToolPermissions())

	if got := FilterToolSpecsForApprovalMode(specs, approval); len(got) != len(specs) {
		t.Fatalf("prompt specs = %v, want all %v", toolSpecNames(got), toolSpecNames(specs))
	}

	approval.SetApprovalMode(ModeYolo)
	got := FilterToolSpecsForApprovalMode(specs, approval)
	if want := []string{ReadFileToolName, ShellToolName}; !equalToolSpecNames(got, want) {
		t.Fatalf("yolo specs = %v, want %v", toolSpecNames(got), want)
	}
	if specs[1].Name != ManageWorkspaceToolName {
		t.Fatalf("filter mutated input specs: %v", toolSpecNames(specs))
	}

	approval.SetApprovalMode(ModePrompt)
	if got := FilterToolSpecsForApprovalMode(specs, approval); len(got) != len(specs) {
		t.Fatalf("restored prompt specs = %v, want all %v", toolSpecNames(got), toolSpecNames(specs))
	}
}

func toolSpecNames(specs []llm.ToolSpec) []string {
	names := make([]string, len(specs))
	for i, spec := range specs {
		names[i] = spec.Name
	}
	return names
}

func equalToolSpecNames(specs []llm.ToolSpec, want []string) bool {
	got := toolSpecNames(specs)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
