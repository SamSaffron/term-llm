package ui

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
	"github.com/samsaffron/term-llm/internal/tools"
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

func TestBuildSubagentPreview_UpdatePlanChecklist(t *testing.T) {
	args := json.RawMessage(`{"explanation":"moving to tests","plan":[{"step":"Inspect","status":"completed"},{"step":"Test","status":"in_progress"}]}`)
	progress := &SubagentProgress{CompletedTools: []ToolSegment{{Name: "update_plan", Args: args, Done: true, Success: true}}}

	collapsed := strings.Join(BuildSubagentPreview(progress, 4), "\n")
	if !strings.Contains(collapsed, "Plan updated · 1/2 completed · Test") || strings.Contains(collapsed, "update_plan") {
		t.Fatalf("collapsed plan preview = %q", collapsed)
	}
	expanded := strings.Join(BuildSubagentPreview(progress, 0), "\n")
	for _, want := range []string{"Plan updated — moving to tests", "✓ Inspect", "→ Test"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("expanded plan preview missing %q: %q", want, expanded)
		}
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

func TestBuildSubagentPreview_MaxCallsLimitsCompleted(t *testing.T) {
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
		t.Fatalf("expected 2 calls (maxCalls limit), got %d lines", len(result))
	}
	// Should show the most recent completed tools
	if !strings.Contains(result[0], "file4.go") {
		t.Errorf("expected file4.go in first line: %s", result[0])
	}
	if !strings.Contains(result[1], "file5.go") {
		t.Errorf("expected file5.go in second line: %s", result[1])
	}
}

func TestBuildSubagentPreview_LimitsRecentToolCallsAndKeepsGuardianGrouped(t *testing.T) {
	guardian := func(message string) *tools.GuardianEvent {
		return &tools.GuardianEvent{Message: message, Outcome: tools.GuardianApproved}
	}
	p := &SubagentProgress{
		CompletedTools: []ToolSegment{
			{Name: "read_file", Info: "file1.go", Success: true, Done: true},
			{Name: "read_file", Info: "file2.go", Guardian: guardian("guardian: approved file2"), Success: true, Done: true},
			{Name: "read_file", Info: "file3.go", Success: true, Done: true},
			{Name: "read_file", Info: "file4.go", Success: true, Done: true},
			{Name: "read_file", Info: "file5.go", Success: true, Done: true},
			{Name: "read_file", Info: "file6.go", Guardian: guardian("guardian: approved file6"), Success: true, Done: true},
		},
	}

	result := BuildSubagentPreview(p, 5)
	joined := strings.Join(result, "\n")
	if strings.Contains(joined, "file1.go") {
		t.Fatalf("oldest call should be omitted: %q", joined)
	}
	for _, want := range []string{"file2.go", "file3.go", "file4.go", "file5.go", "file6.go"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("latest five calls should include %q: %q", want, joined)
		}
	}
	if !strings.Contains(joined, "file2.go\n  Guardian: approved file2") ||
		!strings.Contains(joined, "file6.go\n  Guardian: approved file6") {
		t.Fatalf("guardian annotations should remain attached to selected calls: %q", joined)
	}
}

func TestBuildSubagentPreview_ActiveCallsShareRecentCallWindow(t *testing.T) {
	p := &SubagentProgress{
		CompletedTools: []ToolSegment{
			{Name: "read_file", Info: "completed1", Success: true, Done: true},
			{Name: "read_file", Info: "completed2", Success: true, Done: true},
			{Name: "read_file", Info: "completed3", Success: true, Done: true},
			{Name: "read_file", Info: "completed4", Success: true, Done: true},
		},
		ActiveTools: []ToolSegment{
			{Name: "grep", Info: "active1"},
			{Name: "shell", Info: "active2"},
		},
	}

	result := strings.Join(BuildSubagentPreview(p, 5), "\n")
	if strings.Contains(result, "completed1") {
		t.Fatalf("oldest completed call should be omitted: %q", result)
	}
	for _, want := range []string{"completed2", "completed3", "completed4", "active1", "active2"} {
		if !strings.Contains(result, want) {
			t.Fatalf("recent call window should include %q: %q", want, result)
		}
	}
}

func TestBuildSubagentPreview_NonPositiveLimitIsUnbounded(t *testing.T) {
	p := &SubagentProgress{CompletedTools: []ToolSegment{
		{Name: "read_file", Info: "file1.go", Success: true, Done: true},
		{Name: "read_file", Info: "file2.go", Success: true, Done: true},
		{Name: "read_file", Info: "file3.go", Success: true, Done: true},
	}}

	for _, maxCalls := range []int{0, -1} {
		if got := BuildSubagentPreview(p, maxCalls); len(got) != 3 {
			t.Fatalf("maxCalls=%d returned %d calls, want all 3: %#v", maxCalls, len(got), got)
		}
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

func TestSubagentTextPreviewBoundsLongLogicalLines(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		suffix string
	}{
		{name: "ASCII", prefix: strings.Repeat("a", maxSubagentPreviewLineBytes), suffix: "LATEST"},
		{name: "multibyte UTF-8", prefix: strings.Repeat("界", maxSubagentPreviewLineBytes), suffix: "最新"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &SubagentProgress{}
			p.updatePreviewLines(tt.prefix+tt.suffix, defaultSubagentTextPreviewLines)

			if len(p.previewLines) != 1 {
				t.Fatalf("preview lines = %d, want 1", len(p.previewLines))
			}
			if len(p.previewLines[0]) > maxSubagentPreviewLineBytes {
				t.Fatalf("preview retained %d bytes, want <= %d", len(p.previewLines[0]), maxSubagentPreviewLineBytes)
			}
			if !strings.HasSuffix(p.previewLines[0], tt.suffix) {
				t.Fatalf("bounded preview should preserve newest text, got %q", p.previewLines[0])
			}
			if !utf8.ValidString(p.previewLines[0]) {
				t.Fatalf("bounded preview is not valid UTF-8: %q", p.previewLines[0])
			}
		})
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

func TestUpdateSegmentFromSubagentProgress_CanSkipUnchangedPreviewRebuild(t *testing.T) {
	tracker := NewToolTracker()
	tracker.HandleToolStart("spawn", "spawn_agent", "reviewer", nil)
	p := &SubagentProgress{CompletedTools: []ToolSegment{{Name: "read_file", Info: "file1", Success: true, Done: true}}}
	UpdateSegmentFromSubagentProgress(tracker, "spawn", p)
	seg := FindSegmentByCallID(tracker, "spawn")
	if seg == nil {
		t.Fatal("spawn_agent segment missing")
	}
	collapsedBefore := slices.Clone(seg.SubagentPreview)
	expandedBefore := slices.Clone(seg.SubagentExpandedPreview)

	p.CompletedTools = append(p.CompletedTools, ToolSegment{Name: "read_file", Info: "file2", Success: true, Done: true})
	updateSegmentFromSubagentProgress(tracker, "spawn", p, false)

	if !slices.Equal(seg.SubagentPreview, collapsedBefore) || !slices.Equal(seg.SubagentExpandedPreview, expandedBefore) {
		t.Fatalf("preview refresh disabled but snapshots changed: collapsed=%#v expanded=%#v", seg.SubagentPreview, seg.SubagentExpandedPreview)
	}
}

func TestUpdateSegmentFromSubagentProgress_MarksTextOnlyPreview(t *testing.T) {
	tracker := NewToolTracker()
	tracker.HandleToolStart("spawn", "spawn_agent", "reviewer", nil)
	p := &SubagentProgress{previewLines: []string{"verbose text"}}

	UpdateSegmentFromSubagentProgress(tracker, "spawn", p)
	seg := FindSegmentByCallID(tracker, "spawn")
	if seg == nil || !seg.SubagentPreviewTextOnly {
		t.Fatalf("text fallback should be marked for bounded rendering: %#v", seg)
	}

	p.ActiveTools = append(p.ActiveTools, ToolSegment{Name: "read_file", Info: "file.go"})
	UpdateSegmentFromSubagentProgress(tracker, "spawn", p)
	if seg.SubagentPreviewTextOnly {
		t.Fatal("tool-call preview should not use text-only visual truncation")
	}
}

func TestUpdateSegmentFromSubagentProgress_FullPreviewChangeBumpsVersion(t *testing.T) {
	tracker := NewToolTracker()
	tracker.HandleToolStart("spawn", "spawn_agent", "reviewer", nil)
	p := &SubagentProgress{}
	for i := 1; i <= 6; i++ {
		p.CompletedTools = append(p.CompletedTools, ToolSegment{
			CallID: "nested-" + strconv.Itoa(i), Name: "read_file", Info: "file" + strconv.Itoa(i), Success: true, Done: true,
		})
	}
	UpdateSegmentFromSubagentProgress(tracker, "spawn", p)
	seg := FindSegmentByCallID(tracker, "spawn")
	if seg == nil {
		t.Fatal("spawn_agent segment missing")
	}
	collapsedBefore := slices.Clone(seg.SubagentPreview)
	versionBefore := tracker.Version

	p.CompletedTools[0].Guardian = &tools.GuardianEvent{Message: "guardian: approved oldest", Outcome: tools.GuardianApproved}
	UpdateSegmentFromSubagentProgress(tracker, "spawn", p)

	if !slices.Equal(seg.SubagentPreview, collapsedBefore) {
		t.Fatalf("guardian on omitted call should not change collapsed preview: before=%#v after=%#v", collapsedBefore, seg.SubagentPreview)
	}
	if tracker.Version <= versionBefore {
		t.Fatalf("expanded-preview-only update should bump tracker version: before=%d after=%d", versionBefore, tracker.Version)
	}
	if !strings.Contains(strings.Join(seg.SubagentExpandedPreview, "\n"), "Guardian: approved oldest") {
		t.Fatalf("complete preview did not retain older guardian update: %#v", seg.SubagentExpandedPreview)
	}
	collapsed := StripANSI(RenderSegments([]*Segment{seg}, 80, -1, nil, false, false))
	expanded := StripANSI(RenderSegments([]*Segment{seg}, 80, -1, nil, false, true))
	if strings.Contains(collapsed, "Guardian: approved oldest") {
		t.Fatalf("collapsed render should hide guardian with its omitted call: %q", collapsed)
	}
	if !strings.Contains(expanded, "Guardian: approved oldest") {
		t.Fatalf("expanded render should reveal guardian with its older call: %q", expanded)
	}
}

func TestRenderSubagentPromptLines_WrapsAndTruncatesCollapsed(t *testing.T) {
	prompt := "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen"
	result := renderSubagentPromptLines(prompt, 16, false)
	if len(result) != 4 {
		t.Fatalf("expected 4 wrapped prompt lines, got %d: %#v", len(result), result)
	}
	if strings.Contains(strings.Join(result, "\n"), "Question:") {
		t.Fatalf("prompt lines should not include Question: label: %#v", result)
	}
	if !strings.HasSuffix(result[3], "...") {
		t.Fatalf("expected truncated last prompt line to end with ellipsis: %#v", result)
	}
}

func TestRenderSubagentPromptLines_ExpandedShowsAllLines(t *testing.T) {
	prompt := "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen"
	collapsed := renderSubagentPromptLines(prompt, 16, false)
	expanded := renderSubagentPromptLines(prompt, 16, true)
	if len(expanded) <= len(collapsed) {
		t.Fatalf("expanded prompt lines = %#v, collapsed = %#v", expanded, collapsed)
	}
	if strings.HasSuffix(expanded[len(expanded)-1], "...") {
		t.Fatalf("expanded prompt should not be ellipsized: %#v", expanded)
	}
}

func TestRenderSubagentPromptLines_UnicodeWidth(t *testing.T) {
	prompt := "調査して 調査して 調査して 調査して 調査して 調査して 調査して 調査して 調査して"
	width := 20
	contentWidth := width - runewidth.StringWidth(subagentPromptPrefix)
	result := renderSubagentPromptLines(prompt, width, false)
	if len(result) != 4 {
		t.Fatalf("expected 4 wrapped prompt lines, got %d: %#v", len(result), result)
	}
	for i, line := range result {
		if got := runewidth.StringWidth(line); got > contentWidth {
			t.Fatalf("line %d display width = %d, want <= %d: %q", i, got, contentWidth, line)
		}
	}
	if !strings.HasSuffix(result[3], "...") {
		t.Fatalf("expected truncated last prompt line to end with ellipsis: %#v", result)
	}
}

func TestHandleSubagentProgress_ExtractsPromptFromSpawnArgs(t *testing.T) {
	args, err := json.Marshal(tools.SpawnAgentArgs{
		AgentName: "codebase",
		Prompt:    "Find where subagent progress is rendered",
	})
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewToolTracker()
	tracker.Segments = append(tracker.Segments, Segment{
		Type:       SegmentTool,
		ToolCallID: "call-1",
		ToolName:   "spawn_agent",
		ToolInfo:   "@codebase: Find where subagent progress is rendered",
		ToolArgs:   args,
		ToolStatus: ToolPending,
	})
	subagents := NewSubagentTracker()

	HandleSubagentProgress(tracker, subagents, "call-1", tools.SubagentEvent{Type: tools.SubagentEventInit})

	seg := FindSegmentByCallID(tracker, "call-1")
	if seg == nil {
		t.Fatal("segment missing")
	}
	if p := subagents.Get("call-1"); p == nil || p.AgentName != "codebase" {
		t.Fatalf("agent label must use structured name, not prompt-bearing ToolInfo: %+v", p)
	}
	if seg.SubagentPrompt != "Find where subagent progress is rendered" {
		t.Fatalf("SubagentPrompt = %q", seg.SubagentPrompt)
	}
}
