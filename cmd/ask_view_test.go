package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/testutil"
	"github.com/samsaffron/term-llm/internal/ui"
)

// TestApprovalScreen_ActualRender tests what ACTUALLY gets rendered during approval
func TestApprovalScreen_ActualRender(t *testing.T) {
	// Create the model in approval state - exactly as askStreamModel does
	model := newAskStreamModel()

	// Simulate receiving an approval request (same as askApprovalRequestMsg handler)
	toolName := "view_image"
	toolInfo := "wow.png"
	description := "Allow read access to directory: /tmp/play/cats"

	// This is what the fixed code does:
	if toolName != "" {
		phase := ui.FormatToolPhase(toolName, toolInfo)
		model.toolPhase = phase.Active
	}
	model.approvalDesc = description
	model.approvalForm = huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Key("confirm").
				Title(description). // Just the description, NOT tool phase
				Affirmative("Yes").
				Negative("No").
				WithButtonAlignment(lipgloss.Left),
		),
	).WithShowHelp(false).WithShowErrors(false)

	// Initialize the form (required for View to work)
	model.approvalForm.Init()

	// NOW RENDER - this is exactly what the user sees
	rendered := model.View()

	// Print what we actually see
	t.Logf("\n=== ACTUAL APPROVAL SCREEN ===\n%s\n=== END ===", rendered)

	// Show raw bytes with escape sequences visible
	t.Logf("\n=== RAW BYTES (escape sequences visible) ===\n%q\n=== END ===", rendered)

	// Show each byte with control codes marked
	var rawBytes strings.Builder
	for i, b := range []byte(rendered) {
		if b < 32 || b == 127 {
			rawBytes.WriteString(fmt.Sprintf("[%02x]", b))
		} else if b == 0x1b {
			rawBytes.WriteString("[ESC]")
		} else {
			rawBytes.WriteByte(b)
		}
		if i > 0 && i%60 == 0 {
			rawBytes.WriteString("\n")
		}
	}
	t.Logf("\n=== BYTES WITH CONTROL CODES MARKED ===\n%s\n=== END ===", rawBytes.String())

	// Strip ANSI for assertions
	plain := testutil.StripANSI(rendered)
	t.Logf("\n=== PLAIN TEXT (no ANSI) ===\n%s\n=== END ===", plain)

	// Verify NO spinner characters
	spinnerChars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "⢿", "⣻", "⣽", "⣾", "⣷", "⣯", "⣟", "⡿"}
	for _, s := range spinnerChars {
		if strings.Contains(rendered, s) {
			t.Errorf("approval screen should NOT contain spinner %q", s)
		}
	}

	// Verify NO elapsed time
	if strings.Contains(plain, "s (esc") || strings.Contains(plain, "...") {
		// Check for patterns like "2.2s" or "Viewing..."
		if strings.Contains(plain, "Viewing wow.png...") {
			t.Errorf("approval screen should NOT contain tool phase with spinner dots")
		}
	}

	// Verify NO tool phase text "Viewing wow.png" appears
	if strings.Contains(plain, "Viewing wow.png") {
		t.Errorf("approval screen should NOT contain tool phase 'Viewing wow.png', got:\n%s", plain)
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

// TestAfterApproval_BeforeToolReturns tests what is rendered AFTER approval but BEFORE tool completes
func TestAfterApproval_BeforeToolReturns(t *testing.T) {
	// State: approval was given, tool is now executing
	// - approvalForm is nil (dismissed)
	// - toolPhase is set (tool is running)
	// - loading is false (we've moved past initial thinking)
	// - rendered might be empty or have prior content

	model := newAskStreamModel()

	// Simulate: approval was given, now tool is executing
	model.approvalForm = nil                  // approval form dismissed
	model.toolPhase = "Viewing wow.png"       // tool is executing
	model.loading = false                     // past initial loading
	model.rendered = ""                       // no content yet (or could have some)

	// Render - this is exactly what the user sees after clicking Yes
	rendered := model.View()

	t.Logf("\n=== AFTER APPROVAL, BEFORE TOOL RETURNS (no prior content) ===\n%s\n=== END ===", rendered)

	// Show raw bytes with escape sequences visible
	t.Logf("\n=== RAW BYTES (escape sequences visible) ===\n%q\n=== END ===", rendered)

	// Show each byte
	var rawBytes strings.Builder
	for i, b := range []byte(rendered) {
		if b < 32 || b == 127 {
			rawBytes.WriteString(fmt.Sprintf("[%02x]", b))
		} else if b == 0x1b {
			rawBytes.WriteString("[ESC]")
		} else {
			rawBytes.WriteByte(b)
		}
		if i > 0 && i%60 == 0 {
			rawBytes.WriteString("\n")
		}
	}
	t.Logf("\n=== BYTES WITH CONTROL CODES MARKED ===\n%s\n=== END ===", rawBytes.String())

	plain := testutil.StripANSI(rendered)
	t.Logf("\n=== PLAIN TEXT ===\n%s\n=== END ===", plain)

	// Should show the tool phase with spinner
	if !strings.Contains(plain, "Viewing wow.png") {
		t.Errorf("should show tool phase 'Viewing wow.png', got:\n%s", plain)
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
		t.Errorf("should have spinner character, got:\n%s", rendered)
	}

	// Should have elapsed time pattern
	if !strings.Contains(plain, "s") {
		t.Errorf("should show elapsed time, got:\n%s", plain)
	}

	// Should have cancel hint
	if !strings.Contains(plain, "(esc to cancel)") {
		t.Errorf("should show '(esc to cancel)', got:\n%s", plain)
	}

	// Now test with some prior rendered content (e.g., LLM said something first)
	model2 := newAskStreamModel()
	model2.approvalForm = nil
	model2.toolPhase = "Viewing wow.png"
	model2.loading = false
	model2.rendered = "I'll view that image for you.\n"

	rendered2 := model2.View()
	t.Logf("\n=== AFTER APPROVAL, BEFORE TOOL RETURNS (with prior content) ===\n%s\n=== END ===", rendered2)

	plain2 := testutil.StripANSI(rendered2)

	// Should have both the prior content AND the spinner
	if !strings.Contains(plain2, "I'll view that image for you") {
		t.Errorf("should show prior content, got:\n%s", plain2)
	}
	if !strings.Contains(plain2, "Viewing wow.png") {
		t.Errorf("should show tool phase, got:\n%s", plain2)
	}
}

// TestImmediatelyAfterApproval_ExactState tests the exact model state right after clicking Yes
func TestImmediatelyAfterApproval_ExactState(t *testing.T) {
	// Simulate the exact state after approval form completion
	// This is what Update() sets after user clicks Yes (lines 517-548 in ask.go)
	model := newAskStreamModel()

	// These are set BEFORE approval form shows (in askApprovalRequestMsg handler):
	toolPhase := "Viewing wow.png"
	model.toolPhase = toolPhase

	// These are set AFTER approval (in the StateCompleted handler):
	// Lines 520-538: content is written and rendered
	model.content.WriteString("- ")
	model.content.WriteString(toolPhase)
	model.content.WriteString(" ✓\n")
	model.rendered = model.render()

	// Lines 544-546: form is dismissed
	model.approvalForm = nil
	model.approvalDesc = ""

	// Line 547: returns m.spinner.Tick (loading is NOT changed, stays false)
	model.loading = false

	// NOW RENDER - this is what the user sees RIGHT AFTER clicking Yes
	rendered := model.View()

	t.Logf("\n=== IMMEDIATELY AFTER APPROVAL (exact model state) ===\n%s\n=== END ===", rendered)
	t.Logf("\n=== RAW BYTES ===\n%q\n=== END ===", rendered)

	// Show each byte with control codes marked
	var rawBytes strings.Builder
	for i, b := range []byte(rendered) {
		if b < 32 || b == 127 {
			rawBytes.WriteString(fmt.Sprintf("[%02x]", b))
		} else if b == 0x1b {
			rawBytes.WriteString("[ESC]")
		} else {
			rawBytes.WriteByte(b)
		}
		if i > 0 && i%60 == 0 {
			rawBytes.WriteString("\n")
		}
	}
	t.Logf("\n=== BYTES WITH CONTROL CODES MARKED ===\n%s\n=== END ===", rawBytes.String())

	plain := testutil.StripANSI(rendered)
	t.Logf("\n=== PLAIN TEXT ===\n%s\n=== END ===", plain)

	t.Logf("\n=== MODEL STATE ===")
	t.Logf("approvalForm: %v", model.approvalForm)
	t.Logf("toolPhase: %q", model.toolPhase)
	t.Logf("loading: %v", model.loading)
	t.Logf("rendered length: %d", len(model.rendered))
	t.Logf("content length: %d", model.content.Len())
	t.Logf("=== END MODEL STATE ===")

	// Should NOT be empty
	if rendered == "" {
		t.Errorf("screen should NOT be empty after approval!")
	}

	// Should show the approved message
	if !strings.Contains(plain, "✓") {
		t.Errorf("should show '✓', got:\n%s", plain)
	}

	// Should show spinner with tool phase
	if !strings.Contains(plain, "Viewing wow.png") {
		t.Errorf("should show tool phase 'Viewing wow.png', got:\n%s", plain)
	}
}

// TestScreenTransition_ApprovalToToolExecution compares the two screen states
func TestScreenTransition_ApprovalToToolExecution(t *testing.T) {
	// State 1: Approval screen
	model1 := newAskStreamModel()
	toolName := "view_image"
	toolInfo := "wow.png"
	description := "Allow read access to directory: /tmp/play/cats"

	if toolName != "" {
		phase := ui.FormatToolPhase(toolName, toolInfo)
		model1.toolPhase = phase.Active
	}
	model1.approvalDesc = description
	model1.approvalForm = huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Key("confirm").
				Title(description).
				Affirmative("Yes").
				Negative("No").
				WithButtonAlignment(lipgloss.Left),
		),
	).WithShowHelp(false).WithShowErrors(false)
	model1.approvalForm.Init()

	approvalView := model1.View()
	approvalLines := strings.Split(approvalView, "\n")

	// State 2: After approval, tool executing
	model2 := newAskStreamModel()
	model2.toolPhase = "Viewing wow.png"
	model2.content.WriteString("- Viewing wow.png ✓\n")
	model2.rendered = model2.render()
	model2.approvalForm = nil
	model2.loading = false

	postApprovalView := model2.View()
	postApprovalLines := strings.Split(postApprovalView, "\n")

	t.Logf("\n=== STATE 1: APPROVAL SCREEN ===")
	t.Logf("Line count: %d", len(approvalLines))
	for i, line := range approvalLines {
		plainLine := testutil.StripANSI(line)
		t.Logf("Line %d (%d chars, %d plain): %q", i, len(line), len(plainLine), plainLine)
	}

	t.Logf("\n=== STATE 2: AFTER APPROVAL ===")
	t.Logf("Line count: %d", len(postApprovalLines))
	for i, line := range postApprovalLines {
		plainLine := testutil.StripANSI(line)
		t.Logf("Line %d (%d chars, %d plain): %q", i, len(line), len(plainLine), plainLine)
	}

	t.Logf("\n=== LINE COUNT COMPARISON ===")
	t.Logf("Approval screen: %d lines", len(approvalLines))
	t.Logf("Post-approval:   %d lines", len(postApprovalLines))

	if len(approvalLines) > len(postApprovalLines) {
		t.Logf("WARNING: Post-approval has FEWER lines than approval screen!")
		t.Logf("This could cause visual artifacts if bubbletea doesn't clear properly")
	}
}

// TestRealFlow_EmptyContentBeforeApproval simulates the real flow where LLM
// immediately calls a tool without any text first
func TestRealFlow_EmptyContentBeforeApproval(t *testing.T) {
	// Real flow:
	// 1. User runs: term-llm ask --tools view 'describe abc.png'
	// 2. LLM immediately calls view_image (no text response first)
	// 3. Approval form shows
	// 4. User clicks Yes
	// 5. What does the screen show?

	model := newAskStreamModel()

	// Simulate the initial state when LLM requests a tool
	// At this point, no text content exists
	t.Logf("Initial state - content.Len(): %d", model.content.Len())
	t.Logf("Initial state - loading: %v", model.loading) // should be true

	// askToolStartMsg arrives - sets phase (not toolPhase!) because loading is still true
	// See lines 631-635 in ask.go:
	// if !m.loading { m.toolPhase = newPhase } else { m.phase = newPhase }
	model.phase = "Viewing wow.png" // since loading is true, phase is set, not toolPhase
	// loading STAYS true - no content has streamed yet

	// askApprovalRequestMsg arrives - this DOES set toolPhase
	model.toolPhase = "Viewing wow.png"
	// loading STILL stays true!

	t.Logf("Before approval - loading: %v, phase: %q, toolPhase: %q",
		model.loading, model.phase, model.toolPhase)

	// After approval completes (StateCompleted), the code does:
	// Line 520: if m.content.Len() > 0 { m.content.WriteString("\n\n") }
	// Line 523-527: m.content.WriteString("- "); m.content.WriteString(m.toolPhase); ...
	// IMPORTANTLY: loading is NOT changed!

	// Simulate approval completion:
	if model.content.Len() > 0 {
		model.content.WriteString("\n\n")
	}
	model.content.WriteString("- ")
	model.content.WriteString(model.toolPhase)
	model.content.WriteString(" ✓\n")

	t.Logf("After approval - content: %q", model.content.String())
	t.Logf("After approval - content.Len(): %d", model.content.Len())

	// Now render
	model.rendered = model.render()
	t.Logf("rendered length: %d", len(model.rendered))
	t.Logf("rendered content: %q", testutil.StripANSI(model.rendered))

	// Dismiss approval form
	model.approvalForm = nil
	// NOTE: loading is STILL true! This is the real state!

	t.Logf("After approval - loading: %v (STILL TRUE!)", model.loading)
	t.Logf("Spinner view: %q", model.spinner.View())

	// Now View()
	rendered := model.View()
	plain := testutil.StripANSI(rendered)

	t.Logf("\n=== WHAT USER SEES AFTER APPROVAL (real flow) ===\n%s\n=== END ===", plain)
	t.Logf("View() returned %d chars, %d plain chars", len(rendered), len(plain))

	// Should NOT be empty
	if rendered == "" {
		t.Errorf("screen should NOT be empty!")
	}

	// Should show the approval result
	if !strings.Contains(plain, "✓") {
		t.Errorf("should show '✓', got:\n%s", plain)
	}

	// Should show spinner
	if !strings.Contains(plain, "Viewing wow.png") {
		t.Errorf("should show spinner with tool phase, got:\n%s", plain)
	}
}

// TestApprovalScreen_WhatUserReported tests the bug the user reported
func TestApprovalScreen_WhatUserReported(t *testing.T) {
	// The user reported seeing:
	// ⣾  Viewing wow.png... 2.2s
	// ┃ Viewing wow.png
	// ┃ Allow read access to directory: /tmp/play/cats
	// ┃
	// ┃   Yes     No
	//
	// This test ensures we DON'T see that anymore

	model := newAskStreamModel()

	toolName := "view_image"
	toolInfo := "wow.png"
	description := "Allow read access to directory: /tmp/play/cats"

	if toolName != "" {
		phase := ui.FormatToolPhase(toolName, toolInfo)
		model.toolPhase = phase.Active
	}
	model.approvalDesc = description
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
	model.approvalForm.Init()

	rendered := model.View()
	plain := testutil.StripANSI(rendered)

	// The buggy output had "Viewing wow.png" appearing TWICE:
	// 1. In the spinner line: "⣾  Viewing wow.png... 2.2s"
	// 2. In the form title: "┃ Viewing wow.png"
	//
	// After fix, it should appear ZERO times

	count := strings.Count(plain, "Viewing wow.png")
	if count > 0 {
		t.Errorf("'Viewing wow.png' should appear 0 times in approval screen, found %d times:\n%s", count, plain)
	}

	// Should NOT have spinner line with time
	if strings.Contains(plain, "... ") && strings.Contains(plain, "s") {
		// Pattern like "... 2.2s" indicates spinner line
		t.Errorf("approval screen should not have spinner timing line, got:\n%s", plain)
	}
}
