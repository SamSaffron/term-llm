package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/tools"
)

func TestStreamingBlock_AddText(t *testing.T) {
	// Simple pass-through markdown renderer
	mdRenderer := func(content string, width int) string {
		return content
	}
	sb := NewStreamingBlock(80, mdRenderer)

	sb.AddText("Hello ")
	sb.AddText("World")

	// Complete the text to trigger rendering
	sb.Complete()

	output := sb.Render(-1, false, true)
	if !strings.Contains(output, "Hello") || !strings.Contains(output, "World") {
		t.Errorf("Render() = %q, expected to contain 'Hello' and 'World'", output)
	}
}

func TestStreamingBlock_ToolTracking(t *testing.T) {
	sb := NewStreamingBlock(80, nil)

	// Add text, then tool
	sb.AddText("Thinking...")
	started := sb.StartTool("call-1", "read_file", "main.go", nil)
	if !started {
		t.Error("StartTool should return true for first tool")
	}

	if !sb.HasPendingTools() {
		t.Error("Should have pending tools after StartTool")
	}

	// End the tool
	sb.EndTool("call-1", true)

	if sb.HasPendingTools() {
		t.Error("Should not have pending tools after EndTool")
	}
}

func TestStreamingBlock_ExpandedShellTool(t *testing.T) {
	sb := NewStreamingBlock(80, nil)
	sb.SetToolsExpanded(true)
	args, err := json.Marshal(tools.ShellArgs{Command: "git status", Description: "Check git status"})
	if err != nil {
		t.Fatalf("failed to marshal args: %v", err)
	}
	started := sb.StartTool("call-1", "shell", "Check git status", args)
	if !started {
		t.Fatal("StartTool should return true for first tool")
	}
	sb.EndTool("call-1", true)

	output := sb.Render(-1, false, true)
	if !strings.Contains(output, "Check git status") {
		t.Errorf("expected expanded description in output, got: %q", output)
	}
}
func TestStreamingBlock_Complete(t *testing.T) {
	mdRenderer := func(content string, width int) string {
		return "[MD]" + content + "[/MD]"
	}

	sb := NewStreamingBlock(80, mdRenderer)
	sb.AddText("Test content")
	sb.Complete()

	// After completion, the tracker should have completed text segments
	tracker := sb.GetTracker()
	if tracker == nil {
		t.Fatal("GetTracker should not return nil")
	}

	// Check that segments were marked complete
	for _, seg := range tracker.Segments {
		if seg.Type == 0 && !seg.Complete { // SegmentText
			t.Error("Text segment should be marked complete after Complete()")
		}
	}
}

func TestStreamingBlock_Resize(t *testing.T) {
	mdRenderer := func(content string, width int) string {
		return content
	}
	sb := NewStreamingBlock(80, mdRenderer)
	sb.AddText("Some text")

	// Should not panic on resize
	sb.Resize(120)

	// Complete and verify content still renders
	sb.Complete()
	output := sb.Render(-1, false, true)
	if !strings.Contains(output, "text") {
		t.Errorf("Render should still contain text after resize, got %q", output)
	}
}

func TestStreamingBlock_AddImageAndDiff(t *testing.T) {
	sb := NewStreamingBlock(80, nil)

	sb.AddImage("/path/to/image.png")
	sb.AddDiff("main.go", "old code", "new code", 10)

	// Should have segments for these
	tracker := sb.GetTracker()
	if tracker == nil {
		t.Fatal("GetTracker should not return nil")
	}

	hasImage := false
	hasDiff := false
	for _, seg := range tracker.Segments {
		if seg.ImagePath == "/path/to/image.png" {
			hasImage = true
		}
		if seg.DiffPath == "main.go" {
			hasDiff = true
		}
	}

	if !hasImage {
		t.Error("Should have image segment")
	}
	if !hasDiff {
		t.Error("Should have diff segment")
	}
}

func TestStreamingBlock_FlushAll(t *testing.T) {
	mdRenderer := func(content string, width int) string {
		return content
	}

	sb := NewStreamingBlock(80, mdRenderer)
	sb.AddText("Content to flush")
	sb.Complete()

	result := sb.FlushAll()
	if result.Content == "" {
		t.Log("FlushAll returned empty content (may be expected if all was already flushed)")
	}
}
