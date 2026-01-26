package ui

import (
	"strings"
	"testing"
)

func TestBuildSubagentPreview_Nil(t *testing.T) {
	result := BuildSubagentPreview(nil, 4)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestBuildSubagentPreview_EmptyProgress(t *testing.T) {
	p := &SubagentProgress{}
	result := BuildSubagentPreview(p, 4)
	if len(result) != 0 {
		t.Errorf("expected empty slice, got %v", result)
	}
}

func TestBuildSubagentPreview_ActiveTools(t *testing.T) {
	p := &SubagentProgress{
		ActiveTools: []ToolSegment{
			{Name: "Read", Info: "file.go"},
			{Name: "Grep", Info: "pattern"},
		},
	}
	result := BuildSubagentPreview(p, 4)
	if len(result) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(result))
	}
	// Active tools should have working circle
	if !strings.Contains(result[0], WorkingCircle()) {
		t.Errorf("expected working circle in active tool line: %s", result[0])
	}
	if !strings.Contains(result[0], "Read") || !strings.Contains(result[0], "file.go") {
		t.Errorf("expected tool name and info in line: %s", result[0])
	}
	if !strings.Contains(result[1], "Grep") || !strings.Contains(result[1], "pattern") {
		t.Errorf("expected tool name and info in line: %s", result[1])
	}
}

func TestBuildSubagentPreview_CompletedToolsSuccess(t *testing.T) {
	p := &SubagentProgress{
		CompletedTools: []ToolSegment{
			{Name: "Read", Info: "file.go", Success: true, Done: true},
		},
	}
	result := BuildSubagentPreview(p, 4)
	if len(result) != 1 {
		t.Fatalf("expected 1 line, got %d", len(result))
	}
	// Completed successful tools should have success circle
	if !strings.Contains(result[0], SuccessCircle()) {
		t.Errorf("expected success circle in completed tool line: %s", result[0])
	}
	if !strings.Contains(result[0], "Read") || !strings.Contains(result[0], "file.go") {
		t.Errorf("expected tool name and info in line: %s", result[0])
	}
}

func TestBuildSubagentPreview_CompletedToolsError(t *testing.T) {
	p := &SubagentProgress{
		CompletedTools: []ToolSegment{
			{Name: "Bash", Info: "rm -rf", Success: false, Done: true},
		},
	}
	result := BuildSubagentPreview(p, 4)
	if len(result) != 1 {
		t.Fatalf("expected 1 line, got %d", len(result))
	}
	// Failed tools should have error circle
	if !strings.Contains(result[0], ErrorCircle()) {
		t.Errorf("expected error circle in failed tool line: %s", result[0])
	}
	if !strings.Contains(result[0], "Bash") {
		t.Errorf("expected tool name in line: %s", result[0])
	}
}

func TestBuildSubagentPreview_ChronologicalOrder(t *testing.T) {
	p := &SubagentProgress{
		ActiveTools: []ToolSegment{
			{Name: "Grep", Info: "searching"},
		},
		CompletedTools: []ToolSegment{
			{Name: "Read", Info: "file1.go", Success: true, Done: true},
			{Name: "Read", Info: "file2.go", Success: true, Done: true},
			{Name: "Read", Info: "file3.go", Success: true, Done: true},
		},
	}
	result := BuildSubagentPreview(p, 4)
	if len(result) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(result))
	}
	// Completed tools should come first in chronological order
	for i := 0; i < 3; i++ {
		if !strings.Contains(result[i], SuccessCircle()) {
			t.Errorf("expected success circle in completed tool line %d: %s", i, result[i])
		}
	}
	if !strings.Contains(result[0], "file1.go") {
		t.Errorf("expected file1.go in first line: %s", result[0])
	}
	if !strings.Contains(result[1], "file2.go") {
		t.Errorf("expected file2.go in second line: %s", result[1])
	}
	if !strings.Contains(result[2], "file3.go") {
		t.Errorf("expected file3.go in third line: %s", result[2])
	}
	// Active tool should come last (most recent)
	if !strings.Contains(result[3], WorkingCircle()) {
		t.Errorf("expected active tool last with working circle: %s", result[3])
	}
	if !strings.Contains(result[3], "Grep") {
		t.Errorf("expected Grep in last line: %s", result[3])
	}
}

func TestBuildSubagentPreview_MaxLinesLimitsCompleted(t *testing.T) {
	p := &SubagentProgress{
		CompletedTools: []ToolSegment{
			{Name: "Read", Info: "file1.go", Success: true, Done: true},
			{Name: "Read", Info: "file2.go", Success: true, Done: true},
			{Name: "Read", Info: "file3.go", Success: true, Done: true},
			{Name: "Read", Info: "file4.go", Success: true, Done: true},
			{Name: "Read", Info: "file5.go", Success: true, Done: true},
		},
	}
	result := BuildSubagentPreview(p, 2)
	if len(result) != 2 {
		t.Fatalf("expected 2 lines (maxLines limit), got %d", len(result))
	}
	// Should show the most recent completed tools
	if !strings.Contains(result[0], "file4.go") {
		t.Errorf("expected file4.go in first line: %s", result[0])
	}
	if !strings.Contains(result[1], "file5.go") {
		t.Errorf("expected file5.go in second line: %s", result[1])
	}
}

func TestBuildSubagentPreview_TextOnlyWhenNoTools(t *testing.T) {
	p := &SubagentProgress{
		previewLines: []string{"line1", "line2"},
	}
	result := BuildSubagentPreview(p, 4)
	if len(result) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(result))
	}
	if result[0] != "line1" || result[1] != "line2" {
		t.Errorf("expected text lines, got %v", result)
	}
}

func TestBuildSubagentPreview_TextNotShownWhenToolsPresent(t *testing.T) {
	p := &SubagentProgress{
		CompletedTools: []ToolSegment{
			{Name: "Read", Info: "file.go", Success: true, Done: true},
		},
		previewLines: []string{"some text", "more text"},
	}
	result := BuildSubagentPreview(p, 4)
	if len(result) != 1 {
		t.Fatalf("expected 1 line (tool only), got %d", len(result))
	}
	// Should show tool, not text
	if !strings.Contains(result[0], "Read") {
		t.Errorf("expected tool in result, got %v", result)
	}
	if strings.Contains(result[0], "some text") {
		t.Errorf("text should not appear when tools are present: %v", result)
	}
}

func TestBuildSubagentPreview_MixedSuccessAndError(t *testing.T) {
	p := &SubagentProgress{
		CompletedTools: []ToolSegment{
			{Name: "Read", Success: true, Done: true},
			{Name: "Bash", Success: false, Done: true},
			{Name: "Edit", Success: true, Done: true},
		},
	}
	result := BuildSubagentPreview(p, 4)
	if len(result) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(result))
	}
	// Check circles match success status
	if !strings.Contains(result[0], SuccessCircle()) {
		t.Errorf("Read should have success circle: %s", result[0])
	}
	if !strings.Contains(result[1], ErrorCircle()) {
		t.Errorf("Bash should have error circle: %s", result[1])
	}
	if !strings.Contains(result[2], SuccessCircle()) {
		t.Errorf("Edit should have success circle: %s", result[2])
	}
}
