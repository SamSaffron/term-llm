package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDebugProviderName(t *testing.T) {
	tests := []struct {
		variant string
		want    string
	}{
		{"", "debug"},
		{"normal", "debug"},
		{"fast", "debug:fast"},
		{"slow", "debug:slow"},
		{"realtime", "debug:realtime"},
		{"burst", "debug:burst"},
		{"unknown", "debug:unknown"}, // Unknown variants still get named
	}

	for _, tt := range tests {
		t.Run(tt.variant, func(t *testing.T) {
			p := NewDebugProvider(tt.variant)
			if got := p.Name(); got != tt.want {
				t.Errorf("Name() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDebugProviderCredential(t *testing.T) {
	p := NewDebugProvider("")
	if got := p.Credential(); got != "none" {
		t.Errorf("Credential() = %q, want %q", got, "none")
	}
}

func TestDebugProviderCapabilities(t *testing.T) {
	p := NewDebugProvider("")
	caps := p.Capabilities()
	if caps.NativeWebSearch {
		t.Error("expected NativeWebSearch to be false")
	}
	if caps.NativeWebFetch {
		t.Error("expected NativeWebFetch to be false")
	}
}

func TestDebugProviderStream(t *testing.T) {
	p := NewDebugProvider("fast") // Use fast for quicker tests
	ctx := context.Background()

	stream, err := p.Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var fullText strings.Builder
	var gotUsage bool

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		switch event.Type {
		case EventTextDelta:
			fullText.WriteString(event.Text)
		case EventUsage:
			gotUsage = true
			if event.Use.OutputTokens == 0 {
				t.Error("expected non-zero output tokens in usage")
			}
		case EventError:
			t.Fatalf("unexpected error event: %v", event.Err)
		}
	}

	text := fullText.String()

	// Verify markdown elements are present
	elements := []string{
		"# Debug Provider Output",
		"## Code Blocks",
		"```go",
		"```python",
		"```bash",
		"- First item",
		"1. First numbered item",
		"| Feature |",
		"> This is a simple blockquote",
		"**bold text**",
		"*italic text*",
	}

	for _, elem := range elements {
		if !strings.Contains(text, elem) {
			t.Errorf("stream output missing expected element: %q", elem)
		}
	}

	if !gotUsage {
		t.Error("stream did not emit usage event")
	}
}

func TestDebugProviderStreamCancellation(t *testing.T) {
	p := NewDebugProvider("slow") // Use slow to ensure we can cancel mid-stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := p.Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	// Read a few events then cancel
	eventCount := 0
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// After cancel, we expect context.Canceled
			if err != context.Canceled {
				t.Errorf("expected context.Canceled error, got: %v", err)
			}
			return
		}
		if event.Type == EventTextDelta {
			eventCount++
			if eventCount >= 3 {
				cancel()
				// Continue receiving to see the cancellation error
			}
		}
	}
}

func TestDebugPresets(t *testing.T) {
	presets := GetDebugPresets()

	expectedPresets := []string{"fast", "normal", "slow", "realtime", "burst"}
	for _, name := range expectedPresets {
		preset, ok := presets[name]
		if !ok {
			t.Errorf("missing preset: %s", name)
			continue
		}
		if preset.ChunkSize <= 0 {
			t.Errorf("preset %s has invalid ChunkSize: %d", name, preset.ChunkSize)
		}
		if preset.Delay < 0 {
			t.Errorf("preset %s has invalid Delay: %v", name, preset.Delay)
		}
	}

	// Verify specific preset values from the plan
	if p := presets["fast"]; p.ChunkSize != 50 || p.Delay != 5*time.Millisecond {
		t.Errorf("fast preset mismatch: got %+v", p)
	}
	if p := presets["normal"]; p.ChunkSize != 20 || p.Delay != 20*time.Millisecond {
		t.Errorf("normal preset mismatch: got %+v", p)
	}
	if p := presets["slow"]; p.ChunkSize != 10 || p.Delay != 50*time.Millisecond {
		t.Errorf("slow preset mismatch: got %+v", p)
	}
	if p := presets["realtime"]; p.ChunkSize != 5 || p.Delay != 30*time.Millisecond {
		t.Errorf("realtime preset mismatch: got %+v", p)
	}
	if p := presets["burst"]; p.ChunkSize != 200 || p.Delay != 100*time.Millisecond {
		t.Errorf("burst preset mismatch: got %+v", p)
	}
}

func TestDebugProviderUnknownVariant(t *testing.T) {
	// Unknown variants should fall back to normal preset
	p := NewDebugProvider("nonexistent")

	// Should use normal preset values
	if p.preset.ChunkSize != 20 {
		t.Errorf("expected ChunkSize 20 for unknown variant, got %d", p.preset.ChunkSize)
	}
	if p.preset.Delay != 20*time.Millisecond {
		t.Errorf("expected Delay 20ms for unknown variant, got %v", p.preset.Delay)
	}
}

func TestParseSequenceDSL(t *testing.T) {
	tools := []ToolSpec{
		{Name: "read_file"},
		{Name: "glob"},
		{Name: "shell"},
	}

	tests := []struct {
		name        string
		prompt      string
		wantActions int
		wantText    int
		wantTools   int
	}{
		{
			name:        "empty prompt",
			prompt:      "",
			wantActions: 0,
		},
		{
			name:        "no comma - not DSL",
			prompt:      "read README.md",
			wantActions: 0,
		},
		{
			name:        "markdown with tool",
			prompt:      "markdown,read README.md",
			wantActions: 2,
			wantText:    1,
			wantTools:   1,
		},
		{
			name:        "markdown with custom length",
			prompt:      "markdown*100,read README.md",
			wantActions: 2,
			wantText:    1,
			wantTools:   1,
		},
		{
			name:        "multiple tools",
			prompt:      "markdown,read README.md,glob **/*.go,markdown*200",
			wantActions: 4,
			wantText:    2,
			wantTools:   2,
		},
		{
			name:        "empty segments skipped",
			prompt:      "markdown,,read README.md,",
			wantActions: 2,
			wantText:    1,
			wantTools:   1,
		},
		{
			name:        "whitespace trimmed",
			prompt:      " markdown , read README.md ",
			wantActions: 2,
			wantText:    1,
			wantTools:   1,
		},
		{
			name:        "invalid command skipped",
			prompt:      "markdown,invalid,read README.md",
			wantActions: 2,
			wantText:    1,
			wantTools:   1,
		},
		{
			name:        "missing tool skipped",
			prompt:      "markdown,write foo.txt bar",
			wantActions: 1,
			wantText:    1,
			wantTools:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions := parseSequenceDSL(tt.prompt, tools)
			if len(actions) != tt.wantActions {
				t.Errorf("got %d actions, want %d", len(actions), tt.wantActions)
			}

			var textCount, toolCount int
			for _, a := range actions {
				switch a.Type {
				case actionText:
					textCount++
				case actionTool:
					toolCount++
				}
			}

			if textCount != tt.wantText {
				t.Errorf("got %d text actions, want %d", textCount, tt.wantText)
			}
			if toolCount != tt.wantTools {
				t.Errorf("got %d tool actions, want %d", toolCount, tt.wantTools)
			}
		})
	}
}

func TestParseSequenceDSLMarkdownLength(t *testing.T) {
	tools := []ToolSpec{{Name: "read_file"}}

	tests := []struct {
		name       string
		prompt     string
		wantLength int
	}{
		{
			name:       "default markdown length",
			prompt:     "markdown,read README.md",
			wantLength: 50,
		},
		{
			name:       "custom markdown length 100",
			prompt:     "markdown*100,read README.md",
			wantLength: 100,
		},
		{
			name:       "custom markdown length 200",
			prompt:     "markdown*200,read README.md",
			wantLength: 200,
		},
		{
			name:       "large length capped at debugMarkdown length",
			prompt:     "markdown*99999,read README.md",
			wantLength: len(debugMarkdown),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions := parseSequenceDSL(tt.prompt, tools)
			if len(actions) < 1 || actions[0].Type != actionText {
				t.Fatal("expected first action to be text")
			}
			if len(actions[0].Text) != tt.wantLength {
				t.Errorf("got text length %d, want %d", len(actions[0].Text), tt.wantLength)
			}
		})
	}
}

func TestDebugProviderMixedStream(t *testing.T) {
	p := NewDebugProvider("fast")
	ctx := context.Background()

	tools := []ToolSpec{
		{Name: "read_file"},
		{Name: "glob"},
	}

	stream, err := p.Stream(ctx, Request{
		Tools: tools,
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "markdown*20,read README.md,glob **/*.go,markdown*30"}},
		}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var events []Event
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		events = append(events, event)
	}

	// Count event types
	var textEvents, toolEvents, usageEvents int
	var toolNames []string

	for _, e := range events {
		switch e.Type {
		case EventTextDelta:
			textEvents++
		case EventToolCall:
			toolEvents++
			toolNames = append(toolNames, e.Tool.Name)
		case EventUsage:
			usageEvents++
		}
	}

	// We expect some text events (streaming markdown)
	if textEvents == 0 {
		t.Error("expected text events from markdown segments")
	}

	// We expect 2 tool calls
	if toolEvents != 2 {
		t.Errorf("got %d tool events, want 2", toolEvents)
	}

	// Verify tool names
	if len(toolNames) != 2 {
		t.Errorf("got %d tool names, want 2", len(toolNames))
	} else {
		if toolNames[0] != "read_file" {
			t.Errorf("first tool = %q, want read_file", toolNames[0])
		}
		if toolNames[1] != "glob" {
			t.Errorf("second tool = %q, want glob", toolNames[1])
		}
	}

	// Should have exactly 1 usage event
	if usageEvents != 1 {
		t.Errorf("got %d usage events, want 1", usageEvents)
	}
}

func TestDebugProviderMixedStreamToolArguments(t *testing.T) {
	p := NewDebugProvider("fast")
	ctx := context.Background()

	tools := []ToolSpec{
		{Name: "read_file"},
		{Name: "shell"},
	}

	stream, err := p.Stream(ctx, Request{
		Tools: tools,
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "markdown,read README.md,shell ls -la"}},
		}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var toolCalls []*ToolCall
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if event.Type == EventToolCall {
			toolCalls = append(toolCalls, event.Tool)
		}
	}

	if len(toolCalls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(toolCalls))
	}

	// Check read_file args
	var readArgs map[string]string
	if err := json.Unmarshal(toolCalls[0].Arguments, &readArgs); err != nil {
		t.Fatalf("failed to unmarshal read_file args: %v", err)
	}
	if readArgs["file_path"] != "README.md" {
		t.Errorf("read_file file_path = %q, want README.md", readArgs["file_path"])
	}

	// Check shell args
	var shellArgs map[string]string
	if err := json.Unmarshal(toolCalls[1].Arguments, &shellArgs); err != nil {
		t.Fatalf("failed to unmarshal shell args: %v", err)
	}
	if shellArgs["command"] != "ls -la" {
		t.Errorf("shell command = %q, want 'ls -la'", shellArgs["command"])
	}
}

func TestDebugProviderDSLEdgeCases(t *testing.T) {
	p := NewDebugProvider("fast")
	ctx := context.Background()

	tests := []struct {
		name         string
		prompt       string
		tools        []ToolSpec
		wantToolCall bool
		wantText     bool
	}{
		{
			name:         "only markdown in DSL",
			prompt:       "markdown,markdown*100",
			tools:        []ToolSpec{{Name: "read_file"}},
			wantToolCall: false,
			wantText:     true,
		},
		{
			name:         "only tools in DSL",
			prompt:       "read README.md,read AGENTS.md",
			tools:        []ToolSpec{{Name: "read_file"}},
			wantToolCall: true,
			wantText:     false,
		},
		{
			name:         "all invalid - falls back to markdown",
			prompt:       "invalid,nope,bad",
			tools:        []ToolSpec{{Name: "read_file"}},
			wantToolCall: false,
			wantText:     true, // Falls back to default markdown streaming
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream, err := p.Stream(ctx, Request{
				Tools: tt.tools,
				Messages: []Message{{
					Role:  RoleUser,
					Parts: []Part{{Type: PartText, Text: tt.prompt}},
				}},
			})
			if err != nil {
				t.Fatalf("Stream() error = %v", err)
			}
			defer stream.Close()

			var gotText, gotToolCall bool
			for {
				event, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("stream error: %v", err)
				}
				switch event.Type {
				case EventTextDelta:
					gotText = true
				case EventToolCall:
					gotToolCall = true
				}
			}

			if gotText != tt.wantText {
				t.Errorf("gotText = %v, want %v", gotText, tt.wantText)
			}
			if gotToolCall != tt.wantToolCall {
				t.Errorf("gotToolCall = %v, want %v", gotToolCall, tt.wantToolCall)
			}
		})
	}
}

func TestDebugProviderEditCommand(t *testing.T) {
	p := NewDebugProvider("fast")
	ctx := context.Background()

	tools := []ToolSpec{
		{Name: "edit_file"},
	}

	// Use a .go file that exists in this package directory
	testFile := "debug_provider.go"

	stream, err := p.Stream(ctx, Request{
		Tools: tools,
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "edit " + testFile}},
		}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var toolCalls []*ToolCall
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if event.Type == EventToolCall {
			toolCalls = append(toolCalls, event.Tool)
		}
	}

	// Should get 3 edit_file tool calls by default
	if len(toolCalls) != 3 {
		t.Fatalf("got %d tool calls, want 3", len(toolCalls))
	}

	// Check all calls are edit_file with valid content
	for i, call := range toolCalls {
		if call.Name != "edit_file" {
			t.Errorf("tool call %d: got name %q, want %q", i, call.Name, "edit_file")
		}

		var args map[string]string
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			t.Errorf("tool call %d: failed to parse args: %v", i, err)
			continue
		}

		if args["file_path"] != testFile {
			t.Errorf("tool call %d: got file_path %q, want %q", i, args["file_path"], testFile)
		}
		if args["old_text"] == "" {
			t.Errorf("tool call %d: old_text is empty", i)
		}
		if args["new_text"] == "" {
			t.Errorf("tool call %d: new_text is empty", i)
		}
		// new_text should be old_text + " // edited by debug"
		if !strings.Contains(args["new_text"], "// edited by debug") {
			t.Errorf("tool call %d: new_text %q should contain '// edited by debug'", i, args["new_text"])
		}
		if !strings.HasPrefix(args["new_text"], args["old_text"]) {
			t.Errorf("tool call %d: new_text should start with old_text", i)
		}
	}
}

func TestDebugProviderEditCommandFallback(t *testing.T) {
	// Test fallback behavior when file doesn't exist
	p := NewDebugProvider("fast")
	ctx := context.Background()

	tools := []ToolSpec{
		{Name: "edit_file"},
	}

	stream, err := p.Stream(ctx, Request{
		Tools: tools,
		Messages: []Message{{
			Role:  RoleUser,
			Parts: []Part{{Type: PartText, Text: "edit nonexistent_file.txt"}},
		}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var toolCalls []*ToolCall
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if event.Type == EventToolCall {
			toolCalls = append(toolCalls, event.Tool)
		}
	}

	// Should get 3 fallback edit_file tool calls
	if len(toolCalls) != 3 {
		t.Fatalf("got %d tool calls, want 3", len(toolCalls))
	}

	// Check fallback placeholder content
	for i, call := range toolCalls {
		if call.Name != "edit_file" {
			t.Errorf("tool call %d: got name %q, want %q", i, call.Name, "edit_file")
		}

		var args map[string]string
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			t.Errorf("tool call %d: failed to parse args: %v", i, err)
			continue
		}

		expectedOld := "[DEBUG_OLD_" + string(rune('1'+i)) + "]"
		expectedNew := "[DEBUG_NEW_" + string(rune('1'+i)) + "]"
		if args["old_text"] != expectedOld {
			t.Errorf("tool call %d: got old_text %q, want %q", i, args["old_text"], expectedOld)
		}
		if args["new_text"] != expectedNew {
			t.Errorf("tool call %d: got new_text %q, want %q", i, args["new_text"], expectedNew)
		}
	}
}

func TestGenerateWriteContent(t *testing.T) {
	tests := []struct {
		n         int
		wantLines int
	}{
		{1, 1},
		{5, 5},
		{40, 40},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("n=%d", tt.n), func(t *testing.T) {
			content := generateWriteContent(tt.n)
			lines := strings.Split(content, "\n")
			// Last element is empty because content ends with \n
			nonEmpty := 0
			for _, l := range lines {
				if l != "" {
					nonEmpty++
				}
			}
			if nonEmpty != tt.wantLines {
				t.Errorf("got %d non-empty lines, want %d", nonEmpty, tt.wantLines)
			}
			// Check format of first line
			if !strings.Contains(content, "// line 1: auto-generated debug content") {
				t.Error("content missing expected format")
			}
		})
	}
}

func TestParseCommandWriteLines(t *testing.T) {
	tools := []ToolSpec{{Name: "write_file"}}

	tests := []struct {
		name      string
		prompt    string
		wantCalls int
		wantPath  string
		wantLines int // 0 means don't check content lines
	}{
		{
			name:      "write*10 default path",
			prompt:    "write*10",
			wantCalls: 1,
			wantPath:  "debug-output.txt",
			wantLines: 10,
		},
		{
			name:      "write*40 custom path",
			prompt:    "write*40 myfile.go",
			wantCalls: 1,
			wantPath:  "myfile.go",
			wantLines: 40,
		},
		{
			name:      "write*5 with multiplier",
			prompt:    "write*5 x3",
			wantCalls: 3,
			wantPath:  "debug-output.txt",
			wantLines: 5,
		},
		{
			name:      "plain write still works",
			prompt:    "write foo.txt hello world",
			wantCalls: 1,
			wantPath:  "foo.txt",
			wantLines: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := parseCommand(tt.prompt, tools)
			if len(calls) != tt.wantCalls {
				t.Fatalf("got %d calls, want %d", len(calls), tt.wantCalls)
			}

			for _, call := range calls {
				if call.Name != "write_file" {
					t.Errorf("got name %q, want write_file", call.Name)
				}

				var args map[string]string
				if err := json.Unmarshal(call.Arguments, &args); err != nil {
					t.Fatalf("failed to unmarshal args: %v", err)
				}

				if args["file_path"] != tt.wantPath {
					t.Errorf("got file_path %q, want %q", args["file_path"], tt.wantPath)
				}

				if tt.wantLines > 0 {
					lines := strings.Split(args["content"], "\n")
					nonEmpty := 0
					for _, l := range lines {
						if l != "" {
							nonEmpty++
						}
					}
					if nonEmpty != tt.wantLines {
						t.Errorf("got %d content lines, want %d", nonEmpty, tt.wantLines)
					}
				}
			}
		})
	}
}

func TestParseSequenceDSLWithWrite(t *testing.T) {
	tools := []ToolSpec{
		{Name: "write_file"},
		{Name: "read_file"},
	}

	actions := parseSequenceDSL("markdown*50,write*40,markdown*100", tools)
	if len(actions) != 3 {
		t.Fatalf("got %d actions, want 3", len(actions))
	}

	// First: markdown text
	if actions[0].Type != actionText {
		t.Errorf("action 0: got type %d, want actionText", actions[0].Type)
	}
	if len(actions[0].Text) != 50 {
		t.Errorf("action 0: got text length %d, want 50", len(actions[0].Text))
	}

	// Second: write tool call
	if actions[1].Type != actionTool {
		t.Errorf("action 1: got type %d, want actionTool", actions[1].Type)
	}
	if actions[1].ToolCall.Name != "write_file" {
		t.Errorf("action 1: got tool name %q, want write_file", actions[1].ToolCall.Name)
	}

	// Third: markdown text
	if actions[2].Type != actionText {
		t.Errorf("action 2: got type %d, want actionText", actions[2].Type)
	}
	if len(actions[2].Text) != 100 {
		t.Errorf("action 2: got text length %d, want 100", len(actions[2].Text))
	}
}
