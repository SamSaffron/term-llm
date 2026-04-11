package streaming

import (
	"bytes"
	"testing"
)

// TestDirectParity verifies streaming output exactly matches direct rendering.
func TestDirectParity(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"simple heading", "# Hello\n"},
		{"heading and paragraph", "# Hello\n\nWorld\n"},
		{"two paragraphs", "Hello\n\nWorld\n"},
		{"heading paragraph list", "# Title\n\nParagraph\n\n- Item 1\n- Item 2\n\nDone.\n"},
		{"code block", "```go\nfmt.Println(\"hi\")\n```\n"},
		{"mixed content", "# Heading\n\nThis is a paragraph.\n\n- Item 1\n- Item 2\n\n```\ncode\n```\n\nDone.\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			direct := newTestMarkdownRenderer(testRenderWidth)
			directOut, err := direct.Render([]byte(tt.input))
			if err != nil {
				t.Fatalf("direct render failed: %v", err)
			}

			var buf bytes.Buffer
			sr, err := NewRenderer(&buf, newTestMarkdownRenderer(testRenderWidth))
			if err != nil {
				t.Fatalf("Failed to create streaming renderer: %v", err)
			}
			sr.Write([]byte(tt.input))
			sr.Close()

			if buf.String() != string(directOut) {
				t.Errorf("Parity failed\nInput: %q\nDirect len: %d, newlines: %d\nStreaming len: %d, newlines: %d\nDirect: %q\nStreaming: %q",
					tt.input,
					len(directOut), bytes.Count(directOut, []byte("\n")),
					buf.Len(), bytes.Count(buf.Bytes(), []byte("\n")),
					directOut,
					buf.String())
			}
		})
	}
}

// TestDirectParityChunked verifies streaming output matches direct rendering even when chunked.
func TestDirectParityChunked(t *testing.T) {
	input := "# Hello\n\nWorld\n"

	direct := newTestMarkdownRenderer(testRenderWidth)
	directOut, err := direct.Render([]byte(input))
	if err != nil {
		t.Fatalf("direct render failed: %v", err)
	}

	var buf bytes.Buffer
	sr, err := NewRenderer(&buf, newTestMarkdownRenderer(testRenderWidth))
	if err != nil {
		t.Fatalf("failed to create renderer: %v", err)
	}
	for i := 0; i < len(input); i++ {
		sr.Write([]byte{input[i]})
	}
	sr.Close()

	if buf.String() != string(directOut) {
		t.Errorf("Chunked parity failed\nDirect: %q\nStreaming: %q", directOut, buf.String())
	}
}
