package llm_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestEngineHarness_SimpleTextResponse(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.Provider.AddTextResponse("Hello! I'm a mock LLM.")

	output, err := h.Run(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("Hello")},
	})

	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	testutil.AssertContains(t, output, "Hello! I'm a mock LLM.")

	// Debug: print screen if env var is set
	if testutil.DebugScreensEnabled() {
		h.DumpScreen()
	}
}

func TestEngineHarness_MultiTurnConversation(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.Provider.AddTextResponse("Hello!")

	// First turn
	output1, err := h.Run(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("Hi there")},
	})
	if err != nil {
		t.Fatalf("Run() turn 1 error = %v", err)
	}
	testutil.AssertContains(t, output1, "Hello!")

	// For second turn, create a new harness (simulating a fresh conversation)
	h2 := testutil.NewEngineHarness()
	h2.Provider.AddTextResponse("I'm doing well, thanks!")

	// Second turn
	output2, err := h2.Run(context.Background(), llm.Request{
		Messages: []llm.Message{
			llm.UserText("Hi there"),
			llm.AssistantText("Hello!"),
			llm.UserText("How are you?"),
		},
	})
	if err != nil {
		t.Fatalf("Run() turn 2 error = %v", err)
	}
	testutil.AssertContains(t, output2, "I'm doing well")
}

func TestEngineHarness_ToolCall(t *testing.T) {
	h := testutil.NewEngineHarness()

	// Add a mock tool
	mockReadFile := h.AddMockTool("read_file", "package main\n\nfunc main() {}")

	// Turn 1: LLM requests tool call
	h.Provider.AddTurn(llm.MockTurn{
		ToolCalls: []llm.ToolCall{{
			ID:        "call_1",
			Name:      "read_file",
			Arguments: json.RawMessage(`{"path": "main.go"}`),
		}},
	})

	// Turn 2: LLM responds after seeing tool result
	h.Provider.AddTextResponse("The file defines the main entry point.")

	output, err := h.Run(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("What's in main.go?")},
		Tools:    h.Registry.AllSpecs(),
	})

	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Verify text output
	testutil.AssertContains(t, output, "The file defines the main entry point")

	// Verify tool was called (via mock tool's invocation tracking)
	if mockReadFile.InvocationCount() != 1 {
		t.Errorf("expected 1 tool invocation, got %d", mockReadFile.InvocationCount())
	}

	// Verify the arguments passed to the tool
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(mockReadFile.LastArgs(), &args); err != nil {
		t.Fatalf("failed to parse tool args: %v", err)
	}
	if args.Path != "main.go" {
		t.Errorf("expected path 'main.go', got %q", args.Path)
	}

	// Verify request history (turn 1: initial, turn 2: after tool result)
	if len(h.Provider.Requests) != 2 {
		t.Errorf("expected 2 requests, got %d", len(h.Provider.Requests))
	}
}

func TestEngineHarness_MultipleToolCalls(t *testing.T) {
	h := testutil.NewEngineHarness()

	// Add mock tools
	mockReadFile := h.AddMockTool("read_file", "file content")
	mockGrep := h.AddMockTool("grep", "found: match on line 5")

	// Turn 1: LLM requests multiple tool calls
	h.Provider.AddTurn(llm.MockTurn{
		ToolCalls: []llm.ToolCall{
			{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path": "main.go"}`)},
			{ID: "call_2", Name: "grep", Arguments: json.RawMessage(`{"pattern": "func"}`)},
		},
	})

	// Turn 2: LLM responds with summary
	h.Provider.AddTextResponse("Found the function you're looking for.")

	output, err := h.Run(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("Find the function")},
		Tools:    h.Registry.AllSpecs(),
	})

	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	testutil.AssertContains(t, output, "Found the function")

	// Verify both tools were called (via their invocation counts)
	if mockReadFile.InvocationCount() != 1 {
		t.Errorf("expected read_file to be called once, got %d", mockReadFile.InvocationCount())
	}
	if mockGrep.InvocationCount() != 1 {
		t.Errorf("expected grep to be called once, got %d", mockGrep.InvocationCount())
	}
}

func TestEngineHarness_ToolError(t *testing.T) {
	h := testutil.NewEngineHarness()

	// Add a tool that returns an error
	h.AddTool(testutil.NewMockToolWithSchema(
		"fail_tool",
		"A tool that always fails",
		map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		func(ctx context.Context, args json.RawMessage) (string, error) {
			return "", context.DeadlineExceeded
		},
	))

	// Turn 1: LLM requests the failing tool
	h.Provider.AddTurn(llm.MockTurn{
		ToolCalls: []llm.ToolCall{
			{ID: "call_1", Name: "fail_tool", Arguments: json.RawMessage(`{}`)},
		},
	})

	// Turn 2: LLM gracefully handles the error
	h.Provider.AddTextResponse("I encountered an error trying to do that.")

	output, err := h.Run(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("Do the thing")},
		Tools:    h.Registry.AllSpecs(),
	})

	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	testutil.AssertContains(t, output, "error")
}

func TestEngineHarness_ScreenCapture(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.EnableScreenCapture()

	h.Provider.AddTextResponse("This is a test response with multiple words.")

	_, err := h.Run(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("Test")},
	})

	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Verify frames were captured
	if h.Screen.FrameCount() == 0 {
		t.Error("expected at least one frame to be captured")
	}

	// Verify final screen contains the response
	finalScreen := h.Screen.FinalScreenPlain()
	testutil.AssertContains(t, finalScreen, "test response")

	// Debug output if enabled
	if testutil.DebugScreensEnabled() {
		h.Screen.RenderAllFrames()
	}

	// Save frames if enabled
	if testutil.SaveFramesEnabled() {
		if err := h.SaveFrames("testdata/debug/screen_capture_test"); err != nil {
			t.Errorf("SaveFrames() error = %v", err)
		}
	}
}

func TestEngineHarness_StreamingChunks(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.EnableScreenCapture()

	// This text will be chunked by MockProvider
	longText := "This is a longer response that will be split into multiple chunks during streaming to simulate realistic LLM behavior."
	h.Provider.AddTextResponse(longText)

	output, err := h.Run(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("Say something long")},
	})

	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Verify complete text was received
	if output != longText {
		t.Errorf("output = %q, want %q", output, longText)
	}

	// With screen capture enabled, we should have multiple frames
	// (one for each text chunk)
	if h.Screen.FrameCount() < 2 {
		t.Logf("Frame count: %d (expected multiple frames for chunked output)", h.Screen.FrameCount())
	}
}

func TestEngineHarness_ContextCancellation(t *testing.T) {
	h := testutil.NewEngineHarness()

	// Add a response with delay
	h.Provider.AddTurn(llm.MockTurn{
		Text:  "This response has a delay",
		Delay: 5 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel immediately
	cancel()

	_, err := h.Run(ctx, llm.Request{
		Messages: []llm.Message{llm.UserText("Test")},
	})

	if err == nil {
		t.Error("expected context cancellation error")
	}
}

func TestAssertions_StripANSI(t *testing.T) {
	// Test ANSI stripping
	input := "\x1b[31mRed text\x1b[0m and \x1b[32mgreen text\x1b[0m"
	expected := "Red text and green text"
	got := testutil.StripANSI(input)

	if got != expected {
		t.Errorf("StripANSI() = %q, want %q", got, expected)
	}
}

func TestScreenCapture_SaveAndLoad(t *testing.T) {
	sc := testutil.NewScreenCapture()
	sc.Enable()
	sc.Capture("Frame 1", "Thinking")
	sc.Capture("Frame 2", "Responding")
	sc.Capture("Frame 3", "Done")

	// Verify frame count
	if sc.FrameCount() != 3 {
		t.Errorf("FrameCount() = %d, want 3", sc.FrameCount())
	}

	// Verify last frame
	last := sc.LastFrame()
	if last.Plain != "Frame 3" {
		t.Errorf("LastFrame().Plain = %q, want %q", last.Plain, "Frame 3")
	}
	if last.Phase != "Done" {
		t.Errorf("LastFrame().Phase = %q, want %q", last.Phase, "Done")
	}

	// Test save (only if debug is enabled)
	if testutil.SaveFramesEnabled() {
		dir := "testdata/debug/save_test"
		if err := sc.SaveFrames(dir); err != nil {
			t.Errorf("SaveFrames() error = %v", err)
		}
		// Clean up
		os.RemoveAll(dir)
	}
}
