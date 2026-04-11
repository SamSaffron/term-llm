package streaming

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"testing"
)

// SpecExample represents a single example from the CommonMark spec.
type SpecExample struct {
	Markdown  string `json:"markdown"`
	HTML      string `json:"html"`
	Example   int    `json:"example"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Section   string `json:"section"`
}

func loadSpec(tb testing.TB) []SpecExample {
	tb.Helper()

	data, err := os.ReadFile("testdata/spec.json")
	if err != nil {
		tb.Skipf("CommonMark spec not found: %v", err)
	}

	var examples []SpecExample
	if err := json.Unmarshal(data, &examples); err != nil {
		tb.Fatalf("Failed to parse spec: %v", err)
	}

	return examples
}

// TestCommonMarkSpecChunkingInvariant verifies that streaming renderer
// produces the same output regardless of how the input is chunked.
func TestCommonMarkSpecChunkingInvariant(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CommonMark spec tests in short mode")
	}
	examples := loadSpec(t)

	for _, ex := range examples {
		t.Run(fmt.Sprintf("%s/%d", ex.Section, ex.Example), func(t *testing.T) {
			// Render all at once
			var fullBuf bytes.Buffer
			fullSR, err := NewRenderer(&fullBuf, newTestMarkdownRenderer(testRenderWidth))
			if err != nil {
				t.Fatalf("Failed to create renderer: %v", err)
			}
			fullSR.Write([]byte(ex.Markdown))
			fullSR.Close()
			fullOutput := fullBuf.String()

			// Render byte by byte
			var byteBuf bytes.Buffer
			byteSR, _ := NewRenderer(&byteBuf, newTestMarkdownRenderer(testRenderWidth))
			for i := 0; i < len(ex.Markdown); i++ {
				byteSR.Write([]byte{ex.Markdown[i]})
			}
			byteSR.Close()
			byteOutput := byteBuf.String()

			if fullOutput != byteOutput {
				t.Errorf("Example %d (%s): chunking invariant violated\nInput: %q\nFull: %q\nByte-by-byte: %q",
					ex.Example, ex.Section, ex.Markdown, fullOutput, byteOutput)
			}
		})
	}
}

// TestCommonMarkSpecRandomChunks runs spec examples with random chunk sizes.
func TestCommonMarkSpecRandomChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CommonMark spec tests in short mode")
	}
	examples := loadSpec(t)

	// Test a subset for speed
	for i := 0; i < len(examples) && i < 100; i++ {
		ex := examples[i]
		t.Run(ex.Section+"/random", func(t *testing.T) {
			// Render all at once for reference
			var fullBuf bytes.Buffer
			fullSR, _ := NewRenderer(&fullBuf, newTestMarkdownRenderer(testRenderWidth))
			fullSR.Write([]byte(ex.Markdown))
			fullSR.Close()
			fullOutput := fullBuf.String()

			// Render with random chunks
			for trial := 0; trial < 5; trial++ {
				var randBuf bytes.Buffer
				randSR, _ := NewRenderer(&randBuf, newTestMarkdownRenderer(testRenderWidth))

				pos := 0
				for pos < len(ex.Markdown) {
					chunkSize := rand.Intn(10) + 1
					if pos+chunkSize > len(ex.Markdown) {
						chunkSize = len(ex.Markdown) - pos
					}
					randSR.Write([]byte(ex.Markdown[pos : pos+chunkSize]))
					pos += chunkSize
				}
				randSR.Close()

				if randBuf.String() != fullOutput {
					t.Errorf("Example %d trial %d: random chunking failed\nInput: %q",
						ex.Example, trial, ex.Markdown)
					break
				}
			}
		})
	}
}

// TestCommonMarkSpecSections tests specific sections that are well-supported.
func TestCommonMarkSpecSections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CommonMark spec tests in short mode")
	}
	examples := loadSpec(t)

	// Sections that should work well with streaming
	supportedSections := map[string]bool{
		"ATX headings":                 true,
		"Setext headings":              true,
		"Thematic breaks":              true,
		"Fenced code blocks":           true,
		"Paragraphs":                   true,
		"Block quotes":                 true,
		"Lists":                        true,
		"Backslash escapes":            true,
		"Code spans":                   true,
		"Emphasis and strong emphasis": true,
	}

	for _, ex := range examples {
		if !supportedSections[ex.Section] {
			continue
		}

		t.Run(fmt.Sprintf("%s/%d", ex.Section, ex.Example), func(t *testing.T) {
			var buf bytes.Buffer
			sr, _ := NewRenderer(&buf, newTestMarkdownRenderer(testRenderWidth))
			sr.Write([]byte(ex.Markdown))
			sr.Close()

			// Just verify it doesn't panic and produces output
			if buf.Len() == 0 && len(ex.Markdown) > 0 {
				t.Errorf("Example %d (%s): no output for non-empty input %q",
					ex.Example, ex.Section, ex.Markdown)
			}
		})
	}
}

// TestCommonMarkSpecDirectParity verifies the streaming renderer matches direct
// rendering with the same terminal markdown renderer.
func TestCommonMarkSpecDirectParity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CommonMark spec tests in short mode")
	}
	examples := loadSpec(t)

	passed := 0
	failed := 0

	for _, ex := range examples {
		t.Run(fmt.Sprintf("%s/%d", ex.Section, ex.Example), func(t *testing.T) {
			directRenderer := newTestMarkdownRenderer(testRenderWidth)
			directOut, err := directRenderer.Render([]byte(ex.Markdown))
			if err != nil {
				t.Fatalf("direct render failed: %v", err)
			}

			var buf bytes.Buffer
			sr, err := NewRenderer(&buf, newTestMarkdownRenderer(testRenderWidth))
			if err != nil {
				t.Fatalf("failed to create streaming renderer: %v", err)
			}
			sr.Write([]byte(ex.Markdown))
			sr.Close()

			if buf.String() != string(directOut) {
				failed++
				t.Logf("Example %d (%s): direct parity differs\nInput: %q", ex.Example, ex.Section, ex.Markdown)
			} else {
				passed++
			}
		})
	}

	total := passed + failed
	if total > 0 {
		passRate := float64(passed) / float64(total) * 100
		t.Logf("CommonMark spec direct parity: %d/%d (%.1f%%)", passed, total, passRate)
		if passRate < 95 {
			t.Errorf("Parity rate %.1f%% is below threshold of 95%%", passRate)
		}
	}
}

// BenchmarkStreamingVsDirect compares streaming against direct document rendering.
func BenchmarkStreamingVsDirect(b *testing.B) {
	examples := loadSpec(b)
	if len(examples) == 0 {
		b.Skip("No spec examples loaded")
	}

	var combined bytes.Buffer
	for i := 0; i < 20 && i < len(examples); i++ {
		combined.WriteString(examples[i].Markdown)
		combined.WriteString("\n")
	}
	input := combined.Bytes()

	b.Run("Direct", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			renderer := newTestMarkdownRenderer(testRenderWidth)
			_, _ = renderer.Render(input)
		}
	})

	b.Run("Streaming", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var buf bytes.Buffer
			sr, _ := NewRenderer(&buf, newTestMarkdownRenderer(testRenderWidth))
			sr.Write(input)
			sr.Close()
		}
	})

	b.Run("StreamingChunked", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var buf bytes.Buffer
			sr, _ := NewRenderer(&buf, newTestMarkdownRenderer(testRenderWidth))
			for pos := 0; pos < len(input); pos += 50 {
				end := pos + 50
				if end > len(input) {
					end = len(input)
				}
				sr.Write(input[pos:end])
			}
			sr.Close()
		}
	})
}
