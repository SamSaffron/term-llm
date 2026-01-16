package cmd

import (
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/testutil"
	"github.com/samsaffron/term-llm/internal/ui"
)

// TestApprovalScreen_ActualRender tests what ACTUALLY gets rendered during approval
func TestApprovalScreen_ActualRender(t *testing.T) {
	// Create the model in approval state
	model := newAskStreamModel()

	// Simulate receiving an approval request
	toolName := "view_image"
	toolInfo := "wow.png"
	description := "Allow read access to directory: /tmp/play/cats"

	// Add a pending tool segment (as askApprovalRequestMsg handler does)
	model.tracker.Segments = append(model.tracker.Segments, ui.Segment{
		Type:       ui.SegmentTool,
		ToolName:   toolName,
		ToolInfo:   toolInfo,
		ToolStatus: ui.ToolPending,
	})

	model.approvalDesc = description
	model.approvalToolInfo = toolInfo
	model.approvalForm = huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Key("confirm").
				Title(description).
				Affirmative("Yes").
				Negative("No").
				WithButtonAlignment(lipgloss.Left),
		),
	).WithShowHelp(false).WithShowErrors(false)

	// Initialize the form (required for View to work)
	model.approvalForm.Init()

	// NOW RENDER - this is exactly what the user sees
	rendered := model.View()
	plain := testutil.StripANSI(rendered)

	t.Logf("\n=== ACTUAL APPROVAL SCREEN ===\n%s\n=== END ===", plain)

	// Verify NO spinner characters appear (approval screen should not show spinner)
	spinnerChars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "⢿", "⣻", "⣽", "⣾", "⣷", "⣯", "⣟", "⡿"}
	for _, s := range spinnerChars {
		if strings.Contains(rendered, s) {
			t.Errorf("approval screen should NOT contain spinner %q", s)
		}
	}

	// Verify NO "Thinking" text (spinner is hidden during approval)
	if strings.Contains(plain, "Thinking") {
		t.Errorf("approval screen should NOT contain 'Thinking', got:\n%s", plain)
	}

	// Verify the approval question IS present
	if !strings.Contains(plain, "Allow read access to directory") {
		t.Errorf("approval screen SHOULD contain approval question, got:\n%s", plain)
	}

	if !strings.Contains(plain, "/tmp/play/cats") {
		t.Errorf("approval screen SHOULD contain directory path, got:\n%s", plain)
	}

	// Verify Yes/No buttons present
	if !strings.Contains(plain, "Yes") {
		t.Errorf("approval screen SHOULD contain 'Yes' button, got:\n%s", plain)
	}
	if !strings.Contains(plain, "No") {
		t.Errorf("approval screen SHOULD contain 'No' button, got:\n%s", plain)
	}
}

// TestAfterApproval_ToolSuccess tests what is rendered after a tool completes successfully
func TestAfterApproval_ToolSuccess(t *testing.T) {
	model := newAskStreamModel()

	// Simulate: tool completed successfully
	model.tracker.Segments = append(model.tracker.Segments, ui.Segment{
		Type:       ui.SegmentTool,
		ToolName:   "view_image",
		ToolInfo:   "(wow.png)",
		ToolStatus: ui.ToolSuccess,
	})
	model.thinking = true // Waiting for LLM response after tool

	// Render
	rendered := model.View()
	plain := testutil.StripANSI(rendered)

	t.Logf("\n=== AFTER TOOL SUCCESS ===\n%s\n=== END ===", plain)

	// Should show success indicator (green circle)
	if !strings.Contains(rendered, ui.SuccessCircle()) {
		t.Errorf("should show success circle, got:\n%s", rendered)
	}

	// Should show unified format: "view_image (wow.png)"
	if !strings.Contains(plain, "view_image (wow.png)") {
		t.Errorf("should show completed tool text 'view_image (wow.png)', got:\n%s", plain)
	}

	// Should show thinking spinner (waiting for LLM)
	if !strings.Contains(plain, "Thinking") {
		t.Errorf("should show 'Thinking' after tool completes, got:\n%s", plain)
	}
}

// TestAfterApproval_ToolError tests what is rendered after a tool fails
func TestAfterApproval_ToolError(t *testing.T) {
	model := newAskStreamModel()

	// Simulate: tool failed
	model.tracker.Segments = append(model.tracker.Segments, ui.Segment{
		Type:       ui.SegmentTool,
		ToolName:   "view_image",
		ToolInfo:   "(wow.png)",
		ToolStatus: ui.ToolError,
	})
	model.thinking = true

	// Render
	rendered := model.View()
	plain := testutil.StripANSI(rendered)

	t.Logf("\n=== AFTER TOOL ERROR ===\n%s\n=== END ===", plain)

	// Should show error indicator (red circle)
	if !strings.Contains(rendered, ui.ErrorCircle()) {
		t.Errorf("should show error circle, got:\n%s", rendered)
	}
}

// TestPendingTool_ShowsWaveAnimation tests that pending tools show the animation
func TestPendingTool_ShowsWaveAnimation(t *testing.T) {
	model := newAskStreamModel()

	// Simulate: tool is pending
	model.tracker.Segments = append(model.tracker.Segments, ui.Segment{
		Type:       ui.SegmentTool,
		ToolName:   "view_image",
		ToolInfo:   "(wow.png)",
		ToolStatus: ui.ToolPending,
	})
	model.thinking = false    // Not thinking, tool is running
	model.tracker.WavePos = 5 // Mid-wave animation

	// Render
	rendered := model.View()
	plain := testutil.StripANSI(rendered)

	t.Logf("\n=== PENDING TOOL ===\n%s\n=== END ===", plain)

	// Should show pending indicator (gray circle)
	if !strings.Contains(rendered, ui.PendingCircle()) {
		t.Errorf("should show pending circle, got:\n%s", rendered)
	}

	// Should NOT show "Thinking" (spinner only for LLM wait)
	if strings.Contains(plain, "Thinking") {
		t.Errorf("should NOT show 'Thinking' while tool is pending, got:\n%s", plain)
	}
}

// TestThinkingState tests that thinking spinner only shows when waiting for LLM
func TestThinkingState(t *testing.T) {
	model := newAskStreamModel()

	// Initial state: thinking
	model.thinking = true

	rendered := model.View()
	plain := testutil.StripANSI(rendered)

	t.Logf("\n=== THINKING STATE ===\n%s\n=== END ===", plain)

	// Should show "Thinking"
	if !strings.Contains(plain, "Thinking") {
		t.Errorf("should show 'Thinking' when waiting for LLM, got:\n%s", plain)
	}

	// Should have spinner
	hasSpinner := false
	spinnerChars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "⢿", "⣻", "⣽", "⣾", "⣷", "⣯", "⣟", "⡿"}
	for _, s := range spinnerChars {
		if strings.Contains(rendered, s) {
			hasSpinner = true
			break
		}
	}
	if !hasSpinner {
		t.Errorf("should have spinner character when thinking, got:\n%s", rendered)
	}
}

// TestTextStreaming tests that text content is rendered correctly
func TestTextStreaming(t *testing.T) {
	model := newAskStreamModel()

	// Simulate: text has been streamed
	model.tracker.Segments = append(model.tracker.Segments, ui.Segment{
		Type:     ui.SegmentText,
		Text:     "Hello, this is a test response.",
		Complete: true,
		Rendered: "Hello, this is a test response.",
	})
	model.thinking = false

	rendered := model.View()
	plain := testutil.StripANSI(rendered)

	t.Logf("\n=== TEXT STREAMING ===\n%s\n=== END ===", plain)

	// Should show the text content
	if !strings.Contains(plain, "Hello, this is a test response") {
		t.Errorf("should show text content, got:\n%s", plain)
	}

	// Should NOT show "Thinking" (not waiting for LLM)
	if strings.Contains(plain, "Thinking") {
		t.Errorf("should NOT show 'Thinking' when displaying text, got:\n%s", plain)
	}
}

// TestMultipleSegments tests proper spacing between segments
func TestMultipleSegments(t *testing.T) {
	model := newAskStreamModel()

	// Simulate: text -> tool -> text
	model.tracker.Segments = []ui.Segment{
		{
			Type:     ui.SegmentText,
			Text:     "Let me check that file.",
			Complete: true,
			Rendered: "Let me check that file.",
		},
		{
			Type:       ui.SegmentTool,
			ToolName:   "read_file",
			ToolInfo:   "(test.go)",
			ToolStatus: ui.ToolSuccess,
		},
		{
			Type:     ui.SegmentText,
			Text:     "Here is what I found.",
			Complete: true,
			Rendered: "Here is what I found.",
		},
	}
	model.thinking = false

	rendered := model.View()
	plain := testutil.StripANSI(rendered)

	t.Logf("\n=== MULTIPLE SEGMENTS ===\n%s\n=== END ===", plain)

	// Should show all content
	if !strings.Contains(plain, "Let me check that file") {
		t.Errorf("should show first text segment, got:\n%s", plain)
	}
	if !strings.Contains(plain, "read_file (test.go)") {
		t.Errorf("should show tool segment, got:\n%s", plain)
	}
	if !strings.Contains(plain, "Here is what I found") {
		t.Errorf("should show second text segment, got:\n%s", plain)
	}
}
