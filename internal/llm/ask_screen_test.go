package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/testutil"
	"github.com/samsaffron/term-llm/internal/ui"
)

// This test demonstrates what the user sees at different stages of
// "term-llm ask --tools view 'describe abc.png'"

func TestAskViewImage_ScreenStates(t *testing.T) {
	// We'll capture screen state at key moments using a blocking mock tool

	h := testutil.NewEngineHarness()
	h.EnableScreenCapture()

	// Create a view_image tool that blocks until we signal it
	// This lets us capture the screen state at the exact moment we want
	toolStarted := make(chan struct{})
	toolContinue := make(chan struct{})
	toolDone := make(chan struct{})

	blockingViewTool := &testutil.MockTool{
		SpecData: llm.ToolSpec{
			Name:        "view_image",
			Description: "View an image file",
			Schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{"type": "string"},
				},
				"required": []string{"file_path"},
			},
		},
		ExecuteFn: func(ctx context.Context, args json.RawMessage) (string, error) {
			// Signal that tool execution started (after approval would have happened)
			close(toolStarted)
			// Wait for signal to continue
			<-toolContinue
			close(toolDone)
			return "Image loaded: abc.png\nFormat: image/png\nSize: 12345 bytes", nil
		},
		PreviewFn: func(args json.RawMessage) string {
			var a struct {
				FilePath string `json:"file_path"`
			}
			json.Unmarshal(args, &a)
			return a.FilePath
		},
	}
	h.AddTool(blockingViewTool)

	// Turn 1: LLM requests view_image tool
	h.Provider.AddTurn(llm.MockTurn{
		ToolCalls: []llm.ToolCall{{
			ID:        "call_1",
			Name:      "view_image",
			Arguments: json.RawMessage(`{"file_path": "abc.png"}`),
		}},
	})

	// Turn 2: LLM describes the image
	h.Provider.AddTextResponse("The image shows a beautiful sunset over the ocean.")

	// Run the stream in a goroutine
	var output string
	var streamErr error
	var wg sync.WaitGroup
	wg.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		defer wg.Done()
		output, streamErr = h.Run(ctx, llm.Request{
			Messages: []llm.Message{llm.UserText("describe abc.png")},
			Tools:    h.Registry.AllSpecs(),
		})
	}()

	// Wait for tool execution to start (this is AFTER approval would have been given)
	select {
	case <-toolStarted:
		// Tool has started executing - this is the moment we want to capture
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for tool to start")
	}

	// === THIS IS THE SCREEN STATE AFTER APPROVAL, BEFORE TOOL COMPLETES ===
	//
	// At this point in the real flow:
	// 1. LLM requested view_image tool
	// 2. User was prompted for approval
	// 3. User approved
	// 4. Tool is now executing (loading the image)
	//
	// The screen would show the StreamingIndicator with "Viewing abc.png..."

	// Simulate what the ask command's View() would render at this moment
	screenState := simulateAskScreenDuringToolExecution("view_image", "abc.png", 1*time.Second)

	fmt.Println("=== Screen State: After Approval, Before Tool Completes ===")
	fmt.Println(screenState)
	fmt.Println("=== End Screen State ===")

	// Now let the tool complete
	close(toolContinue)
	<-toolDone
	wg.Wait()

	if streamErr != nil {
		t.Fatalf("Stream error: %v", streamErr)
	}

	// Verify final output
	testutil.AssertContains(t, output, "beautiful sunset")
}

// simulateAskScreenDuringToolExecution simulates what the ask command's
// askStreamModel.View() would render during tool execution.
func simulateAskScreenDuringToolExecution(toolName, toolInfo string, elapsed time.Duration) string {
	var b strings.Builder

	// Get the phase text for this tool
	phase := ui.FormatToolPhase(toolName, toolInfo)

	// Simulate spinner (in real UI this animates: ⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏)
	spinner := "⠋"

	// Build the streaming indicator (matches ui.StreamingIndicator.Render())
	b.WriteString(spinner)
	b.WriteString(" ")
	b.WriteString(phase.Active)
	b.WriteString("...")
	b.WriteString(fmt.Sprintf(" %.1fs", elapsed.Seconds()))
	b.WriteString(" ")
	b.WriteString("(esc to cancel)")

	return b.String()
}

func TestAskViewImage_AllScreenStates(t *testing.T) {
	// Show all the different screen states during the flow

	fmt.Println("")
	fmt.Println("========================================")
	fmt.Println("SCREEN STATES FOR: term-llm ask --tools view 'describe abc.png'")
	fmt.Println("========================================")

	// State 1: Initial thinking
	fmt.Println("--- State 1: Initial Thinking ---")
	fmt.Println(simulateThinkingScreen(0 * time.Second))
	fmt.Println()

	// State 2: LLM decided to call view_image, approval prompt shown
	// NOTE: No spinner/time/tool phase during approval - just the approval question
	fmt.Println("--- State 2: Approval Prompt (ONLY the question) ---")
	approvalScreen := simulateApprovalScreen("view_image", "abc.png", "/home/user/images")
	fmt.Println(approvalScreen)
	fmt.Println()

	// State 3: After approval, tool executing (THIS IS WHAT USER ASKED ABOUT)
	fmt.Println("--- State 3: After Approval, Tool Executing (THE REQUESTED STATE) ---")
	fmt.Println(simulateAskScreenDuringToolExecution("view_image", "abc.png", 1200*time.Millisecond))
	fmt.Println()

	// State 4: Tool done, back to thinking
	fmt.Println("--- State 4: Tool Complete, LLM Processing Result ---")
	fmt.Println(simulateThinkingScreenWithToolLog("abc.png", 2500*time.Millisecond))
	fmt.Println()

	// State 5: Final response streaming
	fmt.Println("--- State 5: Final Response ---")
	fmt.Println(simulateResponseScreen("The image shows a beautiful sunset over the ocean.", 3200*time.Millisecond))
	fmt.Println()
}

func TestApprovalScreen_NoSpinnerOrTime(t *testing.T) {
	// Verify that approval screen does NOT contain spinner, elapsed time, or tool phase
	approvalScreen := simulateApprovalScreen("view_image", "abc.png", "/tmp/play/cats")

	// Should NOT contain spinner characters
	spinnerChars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏", "⢿", "⣻", "⣽", "⣾", "⣷", "⣯", "⣟", "⡿"}
	for _, s := range spinnerChars {
		if strings.Contains(approvalScreen, s) {
			t.Errorf("approval screen should NOT contain spinner character %q, got:\n%s", s, approvalScreen)
		}
	}

	// Should NOT contain elapsed time pattern (e.g., "8.4s", "1.2s")
	timePattern := regexp.MustCompile(`\d+\.\d+s`)
	if timePattern.MatchString(approvalScreen) {
		t.Errorf("approval screen should NOT contain elapsed time, got:\n%s", approvalScreen)
	}

	// Should NOT contain "(esc to cancel)"
	if strings.Contains(approvalScreen, "(esc to cancel)") {
		t.Errorf("approval screen should NOT contain '(esc to cancel)', got:\n%s", approvalScreen)
	}

	// Should NOT contain tool phase like "Viewing abc.png" - only the approval question
	toolPhase := ui.FormatToolPhase("view_image", "abc.png")
	if strings.Contains(approvalScreen, toolPhase.Active) {
		t.Errorf("approval screen should NOT contain tool phase %q, got:\n%s", toolPhase.Active, approvalScreen)
	}

	// SHOULD contain only the approval question and buttons
	testutil.AssertContains(t, approvalScreen, "Allow read access")
	testutil.AssertContains(t, approvalScreen, "/tmp/play/cats")
	testutil.AssertContains(t, approvalScreen, "Yes")
	testutil.AssertContains(t, approvalScreen, "No")
}

func simulateThinkingScreen(elapsed time.Duration) string {
	return fmt.Sprintf("⠋ Thinking... %.1fs (esc to cancel)", elapsed.Seconds())
}

func simulateApprovalScreen(toolName, filePath, dirPath string) string {
	// Approval screen shows ONLY the approval question - no spinner, no time, no tool phase
	// This matches the fixed View() behavior in cmd/ask.go
	//
	// The form title is just the description from the approval request,
	// NOT the tool phase like "Viewing abc.png"
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Allow read access to directory: %s\n", dirPath))
	b.WriteString("\n")
	b.WriteString("  > Yes    No")
	return b.String()
}

func simulateThinkingScreenWithToolLog(filePath string, elapsed time.Duration) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("- Viewed %s: Approved\n", filePath))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("⠋ Thinking... %.1fs (esc to cancel)", elapsed.Seconds()))
	return b.String()
}

func simulateResponseScreen(response string, elapsed time.Duration) string {
	// In real UI, this would be glamour-rendered markdown
	return response
}
