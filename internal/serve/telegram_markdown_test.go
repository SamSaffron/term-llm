package serve

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestMdToTelegramHTML(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains []string
		absent   []string
	}{
		{
			name:     "bold",
			input:    "This is **bold** text",
			contains: []string{"<b>bold</b>"},
			absent:   []string{"**"},
		},
		{
			name:     "italic",
			input:    "This is _italic_ text",
			contains: []string{"<i>italic</i>"},
			absent:   []string{"_italic_"},
		},
		{
			name:     "inline code",
			input:    "Use `fmt.Println` to print",
			contains: []string{"<code>fmt.Println</code>"},
			absent:   []string{"`"},
		},
		{
			name:     "code block",
			input:    "```go\nfmt.Println(\"hello\")\n```",
			contains: []string{"<pre>", "</pre>", "fmt.Println"},
			absent:   []string{"```"},
		},
		{
			name:     "strikethrough",
			input:    "~~deleted~~",
			contains: []string{"<s>deleted</s>"},
			absent:   []string{"~~"},
		},
		{
			name:     "link",
			input:    "[Click here](https://example.com)",
			contains: []string{`<a href="https://example.com">Click here</a>`},
		},
		{
			name:     "heading becomes bold",
			input:    "# Big Title",
			contains: []string{"<b>Big Title</b>"},
			absent:   []string{"<h1>"},
		},
		{
			name:     "unordered list",
			input:    "- Item 1\n- Item 2\n- Item 3",
			contains: []string{"• Item 1", "• Item 2", "• Item 3"},
			absent:   []string{"<ul>", "<li>"},
		},
		{
			name:     "ordered list",
			input:    "1. First\n2. Second\n3. Third",
			contains: []string{"1. First", "2. Second", "3. Third"},
			absent:   []string{"<ol>", "<li>"},
		},
		{
			name:     "blockquote",
			input:    "> A quoted passage",
			contains: []string{"<blockquote>", "A quoted passage", "</blockquote>"},
		},
		{
			name:     "no double asterisks in output",
			input:    "**hello** and __world__",
			contains: []string{"<b>hello</b>"},
			absent:   []string{"**", "__"},
		},
		{
			name:  "empty input",
			input: "",
		},
		{
			name:     "plain text passes through",
			input:    "Just plain text here",
			contains: []string{"Just plain text here"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mdToTelegramHTML(tc.input)
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("expected %q to contain %q\ngot: %s", tc.input, want, got)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(got, unwanted) {
					t.Errorf("expected %q NOT to contain %q\ngot: %s", tc.input, unwanted, got)
				}
			}
		})
	}
}

func TestTelegramProseChunkDoesNotSplitMediaReference(t *testing.T) {
	const reference = "0123456789abcdef0123456789abcdef"
	token := "![Chart](term-llm-media://" + reference + ")"
	input := strings.Repeat("a", telegramMaxMessageLen-8) + token
	first, split := telegramProseChunk(input)
	if strings.Contains(first, "term-llm-media://") || split != telegramMaxMessageLen-8 {
		t.Fatalf("first chunk split=%d content tail=%q", split, first[max(0, len(first)-32):])
	}
	second, _ := telegramProseChunk(input[split:])
	if second != token || telegramMediaMarkdownPattern.ReplaceAllString(second, "$1") != "Chart" {
		t.Fatalf("second chunk = %q", second)
	}
}

func TestReferencedTelegramMediaFollowsAssistantOrder(t *testing.T) {
	const first = "0123456789abcdef0123456789abcdef"
	const second = "fedcba9876543210fedcba9876543210"
	media := []llm.MediaArtifact{
		{Reference: first, Name: "first.png"},
		{Reference: second, Name: "second.mp4"},
		{Reference: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "unused.png"},
	}
	text := "![Video](term-llm-media://" + second + ") then ![Image](term-llm-media://" + first + ") and ![again](term-llm-media://" + second + ")"
	got := referencedTelegramMedia(text, media)
	if len(got) != 2 || got[0].Reference != second || got[1].Reference != first {
		t.Fatalf("referencedTelegramMedia() = %#v", got)
	}
	if prose := telegramMediaMarkdownPattern.ReplaceAllString(text, "$1"); strings.Contains(prose, "term-llm-media://") || !strings.Contains(prose, "Video then Image") {
		t.Fatalf("telegram prose = %q", prose)
	}
}
