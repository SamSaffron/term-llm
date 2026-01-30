package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// debugPreset defines streaming rate configuration.
type debugPreset struct {
	ChunkSize int
	Delay     time.Duration
}

// presets maps variant names to their streaming configurations.
var presets = map[string]debugPreset{
	"fast":     {ChunkSize: 50, Delay: 5 * time.Millisecond},
	"normal":   {ChunkSize: 20, Delay: 20 * time.Millisecond},
	"slow":     {ChunkSize: 10, Delay: 50 * time.Millisecond},
	"realtime": {ChunkSize: 5, Delay: 30 * time.Millisecond},
	"burst":    {ChunkSize: 200, Delay: 100 * time.Millisecond},
}

// debugMarkdown contains rich markdown content for performance testing.
const debugMarkdown = `# Debug Provider Output

This is a **debug stream** for testing the TUI rendering performance. It includes various markdown elements to stress-test the renderer.

## Code Blocks

Here's some Go code:

` + "```go" + `
package main

import (
  "fmt"
  "time"
)

func main() {
	// Stream simulation
	for i := 0; i < 100; i++ {
		fmt.Printf("Chunk %d\n", i)
		time.Sleep(10 * time.Millisecond)
	}
}
` + "```" + `

And some Python:

` + "```python" + `
import asyncio

async def stream_data():
    """Async generator for streaming data."""
    for i in range(100):
        yield f"chunk_{i}"
        await asyncio.sleep(0.01)

async def main():
    async for chunk in stream_data():
        print(chunk)
` + "```" + `

Shell commands:

` + "```bash" + `
#!/bin/bash
for i in {1..10}; do
    echo "Processing item $i"
    sleep 0.1
done | tee output.log
` + "```" + `

## Lists

### Unordered Lists

- First item with some text
- Second item with **bold** and *italic* text
- Third item with ` + "`inline code`" + `
  - Nested item one
  - Nested item two
    - Deeply nested item
- Fourth item with a [link](https://example.com)

### Ordered Lists

1. First numbered item
2. Second numbered item with ~~strikethrough~~
3. Third numbered item
   1. Nested numbered one
   2. Nested numbered two
4. Fourth numbered item

## Tables

| Feature | Status | Priority | Notes |
|---------|--------|----------|-------|
| Streaming | ✅ Done | High | Works well |
| Markdown | ✅ Done | High | Full support |
| Code blocks | ✅ Done | Medium | Syntax highlighting |
| Tables | ✅ Done | Low | Basic support |

Another table with longer content:

| Provider | Model | Capabilities | Rate Limits |
|----------|-------|--------------|-------------|
| Anthropic | claude-3-opus | Tool calls, vision, streaming | 4000 req/min |
| OpenAI | gpt-4-turbo | Tool calls, vision, streaming | 500 req/min |
| Gemini | gemini-pro | Tool calls, streaming | 60 req/min |

## Blockquotes

> This is a simple blockquote.
> It can span multiple lines.

> **Note:** This is an important note with formatting.
>
> It contains multiple paragraphs.
>
> > And nested blockquotes too!
> > With their own content.

## Inline Formatting

This paragraph contains **bold text**, *italic text*, ` + "`inline code`" + `, and a [hyperlink](https://github.com). You can also have ***bold italic*** and ~~strikethrough~~ text.

Here's a longer paragraph to test word wrapping behavior. The quick brown fox jumps over the lazy dog. Pack my box with five dozen liquor jugs. How vexingly quick daft zebras jump! The five boxing wizards jump quickly. Sphinx of black quartz, judge my vow. Two driven jocks help fax my big quiz.

## Headers at All Levels

# Header 1
## Header 2
### Header 3
#### Header 4
##### Header 5
###### Header 6

## Additional Content

Lorem ipsum dolor sit amet, consectetur adipiscing elit. Sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat.

---

Final section with mixed content:

1. **Step one**: Initialize the system with ` + "`init()`" + `
2. **Step two**: Configure the settings
   - Set ` + "`timeout=30`" + `
   - Enable ` + "`debug=true`" + `
3. **Step three**: Run the main loop
4. **Step four**: Cleanup and exit

> **Summary:** This debug output contains headers, code blocks, lists, tables, blockquotes, and various inline formatting elements to thoroughly test markdown rendering performance.
`

// DebugProvider streams rich markdown content for performance testing.
type DebugProvider struct {
	variant string
	preset  debugPreset
}

// NewDebugProvider creates a debug provider with the specified variant.
// Valid variants: fast, normal, slow, realtime, burst
// Empty string defaults to "normal".
func NewDebugProvider(variant string) *DebugProvider {
	if variant == "" {
		variant = "normal"
	}
	preset, ok := presets[variant]
	if !ok {
		preset = presets["normal"]
	}
	return &DebugProvider{
		variant: variant,
		preset:  preset,
	}
}

// Name returns the provider name with variant.
func (d *DebugProvider) Name() string {
	if d.variant == "" || d.variant == "normal" {
		return "debug"
	}
	return "debug:" + d.variant
}

// Credential returns "none" since debug provider needs no authentication.
func (d *DebugProvider) Credential() string {
	return "none"
}

// Capabilities returns the provider capabilities.
func (d *DebugProvider) Capabilities() Capabilities {
	return Capabilities{ToolCalls: true}
}

// Stream starts streaming content based on the request.
// If tools are provided and the prompt matches a command pattern, emits a tool call.
// If tool results are present, emits completion text.
// Otherwise, streams the debug markdown content.
func (d *DebugProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, ch chan<- Event) error {
		// If tool results are present, stream completion text
		if hasToolResults(req.Messages) {
			return d.streamCompletionText(ctx, ch)
		}

		// Parse prompt for tool command(s)
		prompt := getLastUserPrompt(req.Messages)

		// Check for sleep prefix (e.g., "sleep 5 read README.md")
		prompt = parseSleepPrefix(ctx, prompt)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Try DSL sequence first (comma-separated actions)
		if actions := parseSequenceDSL(prompt, req.Tools); len(actions) > 0 {
			return d.streamActionSequence(ctx, ch, actions)
		}

		// Try single command
		if calls := parseCommand(prompt, req.Tools); len(calls) > 0 {
			for _, call := range calls {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case ch <- Event{Type: EventToolCall, Tool: call}:
				}
			}
			// Emit usage
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- Event{Type: EventUsage, Use: &Usage{
				InputTokens:  len(prompt) / 4,
				OutputTokens: 10 * len(calls),
			}}:
			}
			return nil
		}

		// Fall back to debug markdown
		return d.streamDebugMarkdown(ctx, ch)
	}), nil
}

// streamDebugMarkdown streams the standard debug markdown content.
func (d *DebugProvider) streamDebugMarkdown(ctx context.Context, ch chan<- Event) error {
	text := debugMarkdown
	chunkSize := d.preset.ChunkSize
	delay := d.preset.Delay

	for len(text) > 0 {
		// Calculate chunk boundary
		end := chunkSize
		if end > len(text) {
			end = len(text)
		}

		chunk := text[:end]
		text = text[end:]

		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- Event{Type: EventTextDelta, Text: chunk}:
		}

		if delay > 0 && len(text) > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}

	// Emit usage stats
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ch <- Event{Type: EventUsage, Use: &Usage{
		InputTokens:  10,
		OutputTokens: len(debugMarkdown) / 4, // Approximate tokens
	}}:
	}

	return nil
}

// streamCompletionText streams a simple completion message after tool execution.
func (d *DebugProvider) streamCompletionText(ctx context.Context, ch chan<- Event) error {
	text := "Debug: Tool execution completed successfully."
	chunkSize := d.preset.ChunkSize
	delay := d.preset.Delay

	for len(text) > 0 {
		end := chunkSize
		if end > len(text) {
			end = len(text)
		}

		chunk := text[:end]
		text = text[end:]

		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- Event{Type: EventTextDelta, Text: chunk}:
		}

		if delay > 0 && len(text) > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}

	// Emit usage
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ch <- Event{Type: EventUsage, Use: &Usage{
		InputTokens:  50,
		OutputTokens: 10,
	}}:
	}

	return nil
}

// streamActionSequence streams a sequence of interleaved text and tool calls.
func (d *DebugProvider) streamActionSequence(ctx context.Context, ch chan<- Event, actions []debugAction) error {
	chunkSize := d.preset.ChunkSize
	delay := d.preset.Delay
	var totalTextLen, toolCount int

	for _, action := range actions {
		switch action.Type {
		case actionText:
			totalTextLen += len(action.Text)
			text := action.Text
			for len(text) > 0 {
				end := chunkSize
				if end > len(text) {
					end = len(text)
				}
				chunk := text[:end]
				text = text[end:]

				select {
				case <-ctx.Done():
					return ctx.Err()
				case ch <- Event{Type: EventTextDelta, Text: chunk}:
				}

				if delay > 0 && len(text) > 0 {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(delay):
					}
				}
			}

		case actionTool:
			toolCount++
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- Event{Type: EventToolCall, Tool: action.ToolCall}:
			}
		}
	}

	// Emit usage stats
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ch <- Event{Type: EventUsage, Use: &Usage{
		InputTokens:  10,
		OutputTokens: totalTextLen/4 + toolCount*10,
	}}:
	}

	return nil
}

// GetDebugPresets returns a copy of available presets for testing.
func GetDebugPresets() map[string]debugPreset {
	result := make(map[string]debugPreset)
	for k, v := range presets {
		result[k] = v
	}
	return result
}

// parseDebugVariant extracts the variant from a model string like "fast" or "".
func parseDebugVariant(model string) string {
	return strings.TrimSpace(model)
}

// debugActionType distinguishes text streaming from tool calls in DSL sequences.
type debugActionType int

const (
	actionText debugActionType = iota
	actionTool
)

// debugAction represents a single action in a DSL sequence.
type debugAction struct {
	Type     debugActionType
	Text     string    // for actionText: the text to stream
	ToolCall *ToolCall // for actionTool: the tool call to emit
}

// markdownLengthRegex matches "markdown" or "markdown*N" where N is the length.
var markdownLengthRegex = regexp.MustCompile(`^markdown(?:\*(\d+))?$`)

// parseSequenceDSL parses a comma-separated DSL prompt into a sequence of actions.
// Example: "markdown*50,read README.md,glob **/*.go,markdown*200"
// Returns nil if the prompt doesn't look like a DSL sequence.
func parseSequenceDSL(prompt string, tools []ToolSpec) []debugAction {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" || !strings.Contains(prompt, ",") {
		return nil
	}

	// NOTE: Simple comma split - tool arguments containing commas will be split unexpectedly.
	// This is a known limitation; escape sequences are not supported.
	segments := strings.Split(prompt, ",")
	var actions []debugAction

	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}

		// Check for markdown segment (with optional length)
		if match := markdownLengthRegex.FindStringSubmatch(strings.ToLower(seg)); match != nil {
			length := 50 // default length
			if match[1] != "" {
				if n, err := strconv.Atoi(match[1]); err == nil && n > 0 {
					length = n
					if length > len(debugMarkdown) {
						length = len(debugMarkdown)
					}
				}
			}
			actions = append(actions, debugAction{
				Type: actionText,
				Text: debugMarkdown[:length],
			})
			continue
		}

		// Try to parse as a tool command
		if calls := parseCommand(seg, tools); len(calls) > 0 {
			for _, call := range calls {
				actions = append(actions, debugAction{
					Type:     actionTool,
					ToolCall: call,
				})
			}
		}
		// Invalid segments are silently skipped
	}

	// Only return if we have at least one action
	if len(actions) == 0 {
		return nil
	}

	return actions
}

// debugCallID generates unique tool call IDs for the debug provider.
var debugCallID atomic.Uint64

func nextDebugCallID() string {
	return fmt.Sprintf("debug-call-%d", debugCallID.Add(1))
}

// hasToolResults checks if any message contains tool results.
func hasToolResults(msgs []Message) bool {
	for _, msg := range msgs {
		if msg.Role == RoleTool {
			return true
		}
		for _, part := range msg.Parts {
			if part.Type == PartToolResult {
				return true
			}
		}
	}
	return false
}

// getLastUserPrompt extracts the text from the last user message.
func getLastUserPrompt(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleUser {
			for _, part := range msgs[i].Parts {
				if part.Type == PartText && part.Text != "" {
					return part.Text
				}
			}
		}
	}
	return ""
}

// sleepRegex matches a "sleep N" prefix (e.g., "sleep 5 read README.md").
var sleepRegex = regexp.MustCompile(`^sleep\s+(\d+)\s+`)

// parseSleepPrefix checks for a "sleep N" prefix, sleeps if present, and returns the remaining prompt.
func parseSleepPrefix(ctx context.Context, prompt string) string {
	if match := sleepRegex.FindStringSubmatch(prompt); match != nil {
		if secs, err := strconv.Atoi(match[1]); err == nil && secs > 0 && secs <= 60 {
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(time.Duration(secs) * time.Second):
			}
			return strings.TrimSpace(prompt[len(match[0]):])
		}
	}
	return prompt
}

// multiplierRegex matches a trailing "xN" multiplier (e.g., "x3", "x5").
var multiplierRegex = regexp.MustCompile(`\s+x(\d+)$`)

// generateSyntheticEdits reads a file and creates N random edits.
// Returns edit pairs (old_text, new_text) that will match the file content.
func generateSyntheticEdits(filePath string, count int) [][2]string {
	content, err := os.ReadFile(filePath)
	if err != nil {
		// Fallback to placeholder edits if file can't be read
		edits := make([][2]string, count)
		for i := range edits {
			edits[i] = [2]string{
				fmt.Sprintf("[DEBUG_OLD_%d]", i+1),
				fmt.Sprintf("[DEBUG_NEW_%d]", i+1),
			}
		}
		return edits
	}

	lines := strings.Split(string(content), "\n")
	if len(lines) == 0 {
		return nil
	}

	edits := make([][2]string, 0, count)
	// Pick evenly spaced lines to avoid overlapping edits
	step := max(1, len(lines)/count)

	for i := 0; i < count && i*step < len(lines); i++ {
		lineIdx := i * step
		line := lines[lineIdx]
		if strings.TrimSpace(line) == "" {
			continue // skip blank lines
		}
		// Simple edit: append " // edited by debug" to the line
		edits = append(edits, [2]string{
			line,
			line + " // edited by debug",
		})
	}
	return edits
}

// parseCommand parses the prompt into tool calls if it matches a known pattern.
// Supports a trailing "xN" suffix to generate N parallel tool calls (e.g., "read README.md x3").
// Returns nil if no match or if the required tool is not available.
func parseCommand(prompt string, tools []ToolSpec) []*ToolCall {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil
	}

	// Check for multiplier suffix (e.g., "x3")
	multiplier := 1
	if match := multiplierRegex.FindStringSubmatch(prompt); match != nil {
		if n, err := strconv.Atoi(match[1]); err == nil && n > 0 && n <= 20 {
			multiplier = n
			prompt = strings.TrimSpace(prompt[:len(prompt)-len(match[0])])
		}
	}

	// Build a set of available tool names
	toolSet := make(map[string]bool)
	for _, t := range tools {
		toolSet[t.Name] = true
	}

	parts := strings.Fields(prompt)
	if len(parts) == 0 {
		return nil
	}

	cmd := strings.ToLower(parts[0])
	args := parts[1:]

	// makeCall creates a single tool call with the given name and arguments
	makeCall := func(name string, argsMap map[string]string) *ToolCall {
		argsJSON, _ := json.Marshal(argsMap)
		return &ToolCall{
			ID:        nextDebugCallID(),
			Name:      name,
			Arguments: argsJSON,
		}
	}

	// Generate the base tool call arguments based on command
	var toolName string
	var argsMap map[string]string

	switch cmd {
	case "read":
		if !toolSet["read_file"] || len(args) < 1 {
			return nil
		}
		// Support comma-separated files: "read README.md,AGENTS.md,foo.go"
		files := strings.Split(args[0], ",")
		if len(files) > 1 {
			calls := make([]*ToolCall, 0, len(files)*multiplier)
			for _, file := range files {
				file = strings.TrimSpace(file)
				if file == "" {
					continue
				}
				for j := 0; j < multiplier; j++ {
					calls = append(calls, makeCall("read_file", map[string]string{"file_path": file}))
				}
			}
			return calls
		}
		toolName = "read_file"
		argsMap = map[string]string{"file_path": args[0]}

	case "write":
		if !toolSet["write_file"] || len(args) < 2 {
			return nil
		}
		toolName = "write_file"
		argsMap = map[string]string{
			"file_path": args[0],
			"content":   strings.Join(args[1:], " "),
		}

	case "grep":
		if !toolSet["grep"] || len(args) < 1 {
			return nil
		}
		toolName = "grep"
		argsMap = map[string]string{"pattern": args[0]}
		if len(args) >= 2 {
			argsMap["path"] = args[1]
		}

	case "glob":
		if !toolSet["glob"] || len(args) < 1 {
			return nil
		}
		toolName = "glob"
		argsMap = map[string]string{"pattern": args[0]}

	case "shell":
		if !toolSet["shell"] || len(args) < 1 {
			return nil
		}
		toolName = "shell"
		argsMap = map[string]string{"command": strings.Join(args, " ")}

	case "edit":
		if !toolSet["edit_file"] || len(args) < 1 {
			return nil
		}
		filePath := args[0]
		editCount := 3 // default
		if multiplier > 1 {
			editCount = multiplier
			multiplier = 1 // don't multiply the edits themselves
		}

		edits := generateSyntheticEdits(filePath, editCount)
		if len(edits) == 0 {
			return nil
		}

		calls := make([]*ToolCall, len(edits))
		for i, e := range edits {
			argsJSON, _ := json.Marshal(map[string]string{
				"file_path": filePath,
				"old_text":  e[0],
				"new_text":  e[1],
			})
			calls[i] = &ToolCall{
				ID:        nextDebugCallID(),
				Name:      "edit_file",
				Arguments: argsJSON,
			}
		}
		return calls

	case "ask":
		if !toolSet["ask_user"] {
			return nil
		}
		argsJSON, _ := json.Marshal(map[string]interface{}{
			"questions": []map[string]interface{}{{
				"header":   "Test",
				"question": "Debug provider test question?",
				"options": []map[string]string{
					{"label": "Option A", "description": "First option"},
					{"label": "Option B", "description": "Second option"},
				},
			}},
		})
		calls := make([]*ToolCall, multiplier)
		for i := range calls {
			calls[i] = &ToolCall{
				ID:        nextDebugCallID(),
				Name:      "ask_user",
				Arguments: argsJSON,
			}
		}
		return calls

	default:
		return nil
	}

	// Generate multiplier number of calls
	calls := make([]*ToolCall, multiplier)
	for i := range calls {
		calls[i] = makeCall(toolName, argsMap)
	}
	return calls
}
