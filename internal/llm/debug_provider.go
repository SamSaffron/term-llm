package llm

import (
	"context"
	"strings"
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

// Capabilities returns the provider capabilities (no special capabilities).
func (d *DebugProvider) Capabilities() Capabilities {
	return Capabilities{}
}

// Stream starts streaming the debug markdown content.
func (d *DebugProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, ch chan<- Event) error {
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
	}), nil
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
