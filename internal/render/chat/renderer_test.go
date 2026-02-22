package chat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

// generateMessages creates a slice of test messages for benchmarking.
func generateMessages(count int) []session.Message {
	messages := make([]session.Message, count)
	for i := 0; i < count; i++ {
		if i%2 == 0 {
			// User message
			messages[i] = session.Message{
				ID:          int64(i + 1),
				Role:        llm.RoleUser,
				TextContent: fmt.Sprintf("This is user message %d with some content that might span multiple lines to simulate realistic message lengths.", i),
				Parts: []llm.Part{
					{Type: llm.PartText, Text: fmt.Sprintf("This is user message %d with some content that might span multiple lines to simulate realistic message lengths.", i)},
				},
				CreatedAt: time.Now(),
				Sequence:  i,
			}
		} else {
			// Assistant message
			text := fmt.Sprintf("This is assistant message %d. Here is a detailed response:\n\n1. First point\n2. Second point\n3. Third point\n\nHere's some code:\n```go\nfunc example() {\n    fmt.Println(\"Hello\")\n}\n```\n\nAnd a final thought.", i)
			messages[i] = session.Message{
				ID:          int64(i + 1),
				Role:        llm.RoleAssistant,
				TextContent: text,
				Parts: []llm.Part{
					{Type: llm.PartText, Text: text},
				},
				CreatedAt: time.Now(),
				Sequence:  i,
			}
		}
	}
	return messages
}

// simpleMarkdownRenderer is a no-op renderer for benchmarking.
func simpleMarkdownRenderer(content string, width int) string {
	// Simple word wrap simulation
	if len(content) < width {
		return content
	}
	// Just return content as-is for benchmarking (real glamour would be slower)
	return content
}

func TestRenderer_Render(t *testing.T) {
	renderer := NewRenderer(80, 24)
	renderer.SetMarkdownRenderer(simpleMarkdownRenderer)

	messages := generateMessages(10)
	state := RenderState{
		Messages: messages,
		Viewport: ViewportState{
			Height:       24,
			ScrollOffset: 0,
			AtBottom:     true,
		},
		Mode:   RenderModeAltScreen,
		Width:  80,
		Height: 24,
	}

	output := renderer.Render(state)

	// Should contain some content
	if output == "" {
		t.Error("Render() returned empty string for non-empty messages")
	}

	// Should contain user prompts
	if !strings.Contains(output, "❯") {
		t.Error("Render() output should contain user prompt markers")
	}
}

func TestRenderer_CacheHit(t *testing.T) {
	renderer := NewRenderer(80, 24)
	renderer.SetMarkdownRenderer(simpleMarkdownRenderer)

	messages := generateMessages(5)
	state := RenderState{
		Messages: messages,
		Viewport: ViewportState{Height: 24},
		Mode:     RenderModeAltScreen,
		Width:    80,
		Height:   24,
	}

	// First render - cache miss
	_ = renderer.Render(state)
	cacheSize1 := renderer.blockCache.Size()

	// Second render - should use cache
	_ = renderer.Render(state)
	cacheSize2 := renderer.blockCache.Size()

	// Cache size should be the same (no new entries)
	if cacheSize2 != cacheSize1 {
		t.Errorf("Cache size changed on second render: %d -> %d", cacheSize1, cacheSize2)
	}

	if cacheSize1 == 0 {
		t.Error("Cache should have entries after render")
	}
}

func TestRenderer_CacheInvalidateOnResize(t *testing.T) {
	renderer := NewRenderer(80, 24)
	renderer.SetMarkdownRenderer(simpleMarkdownRenderer)

	messages := generateMessages(5)
	state := RenderState{
		Messages: messages,
		Viewport: ViewportState{Height: 24},
		Mode:     RenderModeAltScreen,
		Width:    80,
		Height:   24,
	}

	// First render
	_ = renderer.Render(state)
	cacheSize1 := renderer.blockCache.Size()

	// Resize
	renderer.SetSize(100, 30)

	// Cache should be invalidated
	cacheSize2 := renderer.blockCache.Size()
	if cacheSize2 != 0 {
		t.Errorf("Cache should be empty after resize, got %d", cacheSize2)
	}

	// Render again with new size
	state.Width = 100
	state.Height = 30
	_ = renderer.Render(state)
	cacheSize3 := renderer.blockCache.Size()

	if cacheSize3 != cacheSize1 {
		t.Logf("Cache rebuilt after resize: %d entries", cacheSize3)
	}
}

func TestRenderer_RenderAltScreen_IncludesFullHistory(t *testing.T) {
	renderer := NewRenderer(80, 24)
	renderer.SetMarkdownRenderer(simpleMarkdownRenderer)

	messages := generateMessages(120)
	state := RenderState{
		Messages: messages,
		Viewport: ViewportState{
			Height:       24,
			ScrollOffset: 0,
			AtBottom:     true,
		},
		Mode:   RenderModeAltScreen,
		Width:  80,
		Height: 24,
	}

	output := renderer.Render(state)
	if !strings.Contains(output, "user message 0") {
		t.Fatalf("expected alt-screen render to include earliest history message")
	}
	if !strings.Contains(output, "assistant message 119") {
		t.Fatalf("expected alt-screen render to include latest history message")
	}
}

func TestRenderer_RenderInline_UsesScrollOffsetVirtualization(t *testing.T) {
	renderer := NewRenderer(80, 24)
	renderer.SetMarkdownRenderer(simpleMarkdownRenderer)

	messages := generateMessages(120)
	state := RenderState{
		Messages: messages,
		Viewport: ViewportState{
			Height:       24,
			ScrollOffset: 10,
			AtBottom:     false,
		},
		Mode:   RenderModeInline,
		Width:  80,
		Height: 24,
	}

	output := renderer.Render(state)
	if strings.Contains(output, "assistant message 119") {
		t.Fatalf("inline scroll offset should hide latest messages when scrolled up")
	}
	if !strings.Contains(output, "assistant message 109") {
		t.Fatalf("expected inline render to include visible scrolled history")
	}
}

func TestVirtualViewport_GetVisibleRange(t *testing.T) {
	vp := NewVirtualViewport(80, 24)

	tests := []struct {
		name         string
		msgCount     int
		scrollOffset int
		wantStart    int // Approximate - just check it's reasonable
		wantEnd      int // -1 means "check > 0 and fills viewport"
	}{
		{"empty", 0, 0, 0, 0},
		{"small list no scroll", 5, 0, 0, 5},
		// When scrolled up but messages fit in viewport, still show all
		{"small list scroll up 1", 5, 1, 0, 5},
		{"large list no scroll", 100, 0, -1, 100},
		{"large list scroll up", 100, 10, -1, 90},
		// Scroll to top should fill viewport, not show just 1 message
		{"large list scroll to top", 100, 99, 0, -1},
		{"large list max scroll", 100, 100, 0, -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := generateMessages(tt.msgCount)
			start, end := vp.GetVisibleRange(messages, tt.scrollOffset)

			if tt.wantStart >= 0 && start != tt.wantStart {
				t.Errorf("start = %d, want %d", start, tt.wantStart)
			}
			if tt.wantStart == -1 && start < 0 {
				t.Errorf("start = %d, want >= 0", start)
			}
			if tt.wantEnd >= 0 && end != tt.wantEnd {
				t.Errorf("end = %d, want %d", end, tt.wantEnd)
			}
			// -1 for wantEnd means "check that viewport is filled"
			if tt.wantEnd == -1 {
				// With height=24 and avg msg height ~6.5, we need ~7 messages
				// Check that end-start is reasonable (at least a few messages)
				if end-start < 3 {
					t.Errorf("end-start = %d, should fill viewport (at least 3)", end-start)
				}
			}

			// Sanity checks
			if start < 0 {
				t.Errorf("start = %d, should be >= 0", start)
			}
			if end > tt.msgCount {
				t.Errorf("end = %d, should be <= %d", end, tt.msgCount)
			}
			if start > end {
				t.Errorf("start (%d) > end (%d)", start, end)
			}
		})
	}
}

// BenchmarkRender500Messages benchmarks rendering 500 messages.
// Target: <16ms for 60fps rendering.
func BenchmarkRender500Messages(b *testing.B) {
	renderer := NewRenderer(80, 24)
	renderer.SetMarkdownRenderer(simpleMarkdownRenderer)

	messages := generateMessages(500)
	state := RenderState{
		Messages: messages,
		Viewport: ViewportState{
			Height:       24,
			ScrollOffset: 0,
			AtBottom:     true,
		},
		Mode:   RenderModeAltScreen,
		Width:  80,
		Height: 24,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		renderer.Render(state)
	}
}

// BenchmarkRender500MessagesCached benchmarks with warm cache.
func BenchmarkRender500MessagesCached(b *testing.B) {
	renderer := NewRenderer(80, 24)
	renderer.SetMarkdownRenderer(simpleMarkdownRenderer)

	messages := generateMessages(500)
	state := RenderState{
		Messages: messages,
		Viewport: ViewportState{
			Height:       24,
			ScrollOffset: 0,
			AtBottom:     true,
		},
		Mode:   RenderModeAltScreen,
		Width:  80,
		Height: 24,
	}

	// Warm up cache
	renderer.Render(state)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		renderer.Render(state)
	}
}

// TestMessageBlockRenderer_DiffsOnHydration tests that edit_file diffs are rendered
// when loading messages from the session store (hydration).
func TestMessageBlockRenderer_DiffsOnHydration(t *testing.T) {
	// Helper to create a __DIFF__: marker
	makeDiffMarker := func(file, old, new string, line int) string {
		data := ui.DiffData{File: file, Old: old, New: new, Line: line}
		jsonData, _ := json.Marshal(data)
		return "__DIFF__:" + base64.StdEncoding.EncodeToString(jsonData)
	}

	toolCallID := "call-123"
	messages := []session.Message{
		// Message 0: Assistant message with edit_file tool call
		{
			ID:   1,
			Role: llm.RoleAssistant,
			Parts: []llm.Part{
				{Type: llm.PartText, Text: "I'll update the file."},
				{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:        toolCallID,
						Name:      "edit_file",
						Arguments: []byte(`{"file_path":"test.go","old_string":"x = 1","new_string":"x = 2"}`),
					},
				},
			},
			CreatedAt: time.Now(),
		},
		// Message 1: Tool result message containing diff marker (Role=RoleTool, not RoleUser)
		{
			ID:   2,
			Role: llm.RoleTool,
			Parts: []llm.Part{
				{
					Type: llm.PartToolResult,
					ToolResult: &llm.ToolResult{
						ID:      toolCallID,
						Name:    "edit_file",
						Content: "Edit applied successfully.\n" + makeDiffMarker("test.go", "x = 1", "x = 2", 10),
					},
				},
			},
			CreatedAt: time.Now(),
		},
	}

	// Use the context-aware renderer with full message list
	rb := NewMessageBlockRendererWithContext(80, simpleMarkdownRenderer, messages, 0, false)
	block := rb.Render(&messages[0])

	// The rendered output should contain the diff
	// The diff renderer outputs the file path and +/- lines
	if !strings.Contains(block.Rendered, "test.go") {
		t.Errorf("Expected rendered block to contain file path 'test.go', got:\n%s", block.Rendered)
	}

	// Should contain the edit_file tool indication
	if !strings.Contains(block.Rendered, "edit_file") {
		t.Errorf("Expected rendered block to contain tool name 'edit_file', got:\n%s", block.Rendered)
	}
}

// TestMessageBlockRenderer_NoDiffsWithoutContext tests that diffs are NOT rendered
// when using the basic renderer without message context.
func TestMessageBlockRenderer_NoDiffsWithoutContext(t *testing.T) {
	toolCallID := "call-456"
	msg := session.Message{
		ID:   1,
		Role: llm.RoleAssistant,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "I'll update the file."},
			{
				Type: llm.PartToolCall,
				ToolCall: &llm.ToolCall{
					ID:        toolCallID,
					Name:      "edit_file",
					Arguments: []byte(`{"file_path":"test.go"}`),
				},
			},
		},
		CreatedAt: time.Now(),
	}

	// Use basic renderer without context (no messages slice)
	rb := NewMessageBlockRenderer(80, simpleMarkdownRenderer, false)
	block := rb.Render(&msg)

	// Should still have the tool call rendered
	if !strings.Contains(block.Rendered, "edit_file") {
		t.Errorf("Expected rendered block to contain tool name 'edit_file', got:\n%s", block.Rendered)
	}

	// The diff should NOT be rendered since we don't have access to the tool result
	// (The diff content is in the next message which we don't have access to)
}

// TestMessageBlockRenderer_NonEditFileToolNoDiff tests that non-edit_file tools don't try to render diffs.
func TestMessageBlockRenderer_NonEditFileToolNoDiff(t *testing.T) {
	// Helper to create a __DIFF__: marker (even though shell shouldn't produce diffs)
	makeDiffMarker := func(file, old, new string, line int) string {
		data := ui.DiffData{File: file, Old: old, New: new, Line: line}
		jsonData, _ := json.Marshal(data)
		return "__DIFF__:" + base64.StdEncoding.EncodeToString(jsonData)
	}

	toolCallID := "call-789"
	messages := []session.Message{
		{
			ID:   1,
			Role: llm.RoleAssistant,
			Parts: []llm.Part{
				{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:        toolCallID,
						Name:      "shell", // Not edit_file
						Arguments: []byte(`{"command":"ls -la"}`),
					},
				},
			},
			CreatedAt: time.Now(),
		},
		{
			ID:   2,
			Role: llm.RoleTool,
			Parts: []llm.Part{
				{
					Type: llm.PartToolResult,
					ToolResult: &llm.ToolResult{
						ID:      toolCallID,
						Name:    "shell",
						Content: "file1.txt\nfile2.txt\n" + makeDiffMarker("fake.go", "a", "b", 1),
					},
				},
			},
			CreatedAt: time.Now(),
		},
	}

	rb := NewMessageBlockRendererWithContext(80, simpleMarkdownRenderer, messages, 0, false)
	block := rb.Render(&messages[0])

	// Should NOT contain diff output for non-edit_file tools
	// The diff marker should be in the tool result but not rendered
	if strings.Contains(block.Rendered, "fake.go") {
		t.Errorf("Expected rendered block to NOT contain diff for non-edit_file tool, got:\n%s", block.Rendered)
	}
}

// TestMessageBlockRenderer_DiffsFromDisplayField tests that diffs render from
// the Display field (new path) when it contains the __DIFF__ markers.
func TestMessageBlockRenderer_DiffsFromDisplayField(t *testing.T) {
	makeDiffMarker := func(file, old, new string, line int) string {
		data := ui.DiffData{File: file, Old: old, New: new, Line: line}
		jsonData, _ := json.Marshal(data)
		return "__DIFF__:" + base64.StdEncoding.EncodeToString(jsonData)
	}

	toolCallID := "call-display"
	rawOutput := "Edited test.go: replaced 1 lines with 1 lines.\n" + makeDiffMarker("test.go", "x = 1", "x = 2", 10)
	messages := []session.Message{
		{
			ID:   1,
			Role: llm.RoleAssistant,
			Parts: []llm.Part{
				{Type: llm.PartText, Text: "Updating file."},
				{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:        toolCallID,
						Name:      "edit_file",
						Arguments: []byte(`{"file_path":"test.go","old_string":"x = 1","new_string":"x = 2"}`),
					},
				},
			},
			CreatedAt: time.Now(),
		},
		{
			ID:   2,
			Role: llm.RoleTool,
			Parts: []llm.Part{
				{
					Type: llm.PartToolResult,
					ToolResult: &llm.ToolResult{
						ID:      toolCallID,
						Name:    "edit_file",
						Content: "Edited test.go: replaced 1 lines with 1 lines.", // Clean (no markers)
						Display: rawOutput,                                        // Full output with markers
					},
				},
			},
			CreatedAt: time.Now(),
		},
	}

	rb := NewMessageBlockRendererWithContext(80, simpleMarkdownRenderer, messages, 0, false)
	block := rb.Render(&messages[0])

	if !strings.Contains(block.Rendered, "test.go") {
		t.Errorf("Expected diff to render from Display field, got:\n%s", block.Rendered)
	}
}

// TestMessageBlockRenderer_BackwardCompatNilDisplay tests that old sessions
// without the Display field still render diffs from Content.
func TestMessageBlockRenderer_BackwardCompatNilDisplay(t *testing.T) {
	makeDiffMarker := func(file, old, new string, line int) string {
		data := ui.DiffData{File: file, Old: old, New: new, Line: line}
		jsonData, _ := json.Marshal(data)
		return "__DIFF__:" + base64.StdEncoding.EncodeToString(jsonData)
	}

	toolCallID := "call-compat"
	messages := []session.Message{
		{
			ID:   1,
			Role: llm.RoleAssistant,
			Parts: []llm.Part{
				{Type: llm.PartText, Text: "Updating file."},
				{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:        toolCallID,
						Name:      "edit_file",
						Arguments: []byte(`{"file_path":"compat.go","old_string":"a","new_string":"b"}`),
					},
				},
			},
			CreatedAt: time.Now(),
		},
		{
			ID:   2,
			Role: llm.RoleTool,
			Parts: []llm.Part{
				{
					Type: llm.PartToolResult,
					ToolResult: &llm.ToolResult{
						ID:      toolCallID,
						Name:    "edit_file",
						Content: "Edit applied.\n" + makeDiffMarker("compat.go", "a", "b", 1),
						// Display intentionally empty — simulating old session
					},
				},
			},
			CreatedAt: time.Now(),
		},
	}

	rb := NewMessageBlockRendererWithContext(80, simpleMarkdownRenderer, messages, 0, false)
	block := rb.Render(&messages[0])

	if !strings.Contains(block.Rendered, "compat.go") {
		t.Errorf("Expected diff to render from Content (backward compat), got:\n%s", block.Rendered)
	}
}

func TestMessageBlockRenderer_ToolOnlyAssistantMessage_IsCompact(t *testing.T) {
	msg := session.Message{
		ID:   1,
		Role: llm.RoleAssistant,
		Parts: []llm.Part{
			{
				Type: llm.PartToolCall,
				ToolCall: &llm.ToolCall{
					ID:        "call-1",
					Name:      "web_search",
					Arguments: []byte(`{"query":"first"}`),
				},
			},
			{
				Type: llm.PartToolCall,
				ToolCall: &llm.ToolCall{
					ID:        "call-2",
					Name:      "web_search",
					Arguments: []byte(`{"query":"second"}`),
				},
			},
		},
		CreatedAt: time.Now(),
	}

	rb := NewMessageBlockRenderer(80, simpleMarkdownRenderer, false)
	block := rb.Render(&msg)
	plain := ui.StripANSI(block.Rendered)

	first := strings.Index(plain, "web_search")
	if first == -1 {
		t.Fatalf("expected first web_search tool line in output, got %q", plain)
	}
	rest := plain[first+len("web_search"):]
	secondOffset := strings.Index(rest, "web_search")
	if secondOffset == -1 {
		t.Fatalf("expected second web_search tool line in output, got %q", plain)
	}
	between := rest[:secondOffset]
	if got := strings.Count(between, "\n"); got != 1 {
		t.Fatalf("expected exactly 1 newline between tool lines, got %d; between=%q output=%q", got, between, plain)
	}
}

func TestRenderer_ConsecutiveToolOnlyAssistantMessages_NoBlankLineBetweenTools(t *testing.T) {
	renderer := NewRenderer(80, 24)
	renderer.SetMarkdownRenderer(simpleMarkdownRenderer)

	messages := []session.Message{
		{
			ID:   1,
			Role: llm.RoleAssistant,
			Parts: []llm.Part{
				{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:        "call-1",
						Name:      "web_search",
						Arguments: []byte(`{"query":"first"}`),
					},
				},
			},
			CreatedAt: time.Now(),
			Sequence:  0,
		},
		{
			ID:   2,
			Role: llm.RoleAssistant,
			Parts: []llm.Part{
				{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:        "call-2",
						Name:      "web_search",
						Arguments: []byte(`{"query":"second"}`),
					},
				},
			},
			CreatedAt: time.Now(),
			Sequence:  1,
		},
	}

	state := RenderState{
		Messages: messages,
		Viewport: ViewportState{
			Height:       24,
			ScrollOffset: 0,
			AtBottom:     true,
		},
		Mode:   RenderModeAltScreen,
		Width:  80,
		Height: 24,
	}

	output := ui.StripANSI(renderer.Render(state))
	first := strings.Index(output, "web_search")
	if first == -1 {
		t.Fatalf("expected first tool line in output, got %q", output)
	}
	rest := output[first+len("web_search"):]
	secondOffset := strings.Index(rest, "web_search")
	if secondOffset == -1 {
		t.Fatalf("expected second tool line in output, got %q", output)
	}
	between := rest[:secondOffset]
	if got := strings.Count(between, "\n"); got != 1 {
		t.Fatalf("expected exactly 1 newline between consecutive tool-only assistant messages, got %d; between=%q output=%q", got, between, output)
	}
}

// BenchmarkRender500MessagesScrolling benchmarks with scroll position changes.
func BenchmarkRender500MessagesScrolling(b *testing.B) {
	renderer := NewRenderer(80, 24)
	renderer.SetMarkdownRenderer(simpleMarkdownRenderer)

	messages := generateMessages(500)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Simulate scrolling by changing offset each iteration
		state := RenderState{
			Messages: messages,
			Viewport: ViewportState{
				Height:       24,
				ScrollOffset: i % 100,
				AtBottom:     i%100 == 0,
			},
			Mode:   RenderModeAltScreen,
			Width:  80,
			Height: 24,
		}
		renderer.Render(state)
	}
}
