package ui

import "testing"

func TestFindSafeBoundary(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{
			name: "too short",
			text: "hello",
			want: -1,
		},
		{
			name: "no paragraph boundary",
			text: "this is a single line of text without any paragraph breaks at all here",
			want: -1,
		},
		{
			name: "simple paragraph boundary",
			text: "First paragraph here.\n\nSecond paragraph starts",
			want: 23, // After "First paragraph here.\n\n"
		},
		{
			name: "multiple paragraphs - returns last safe",
			text: "First para.\n\nSecond para.\n\nThird paragraph starts here",
			want: 27, // After "First para.\n\nSecond para.\n\n" (11+2+12+2=27)
		},
		{
			name: "paragraph inside code block is not safe",
			text: "Before code.\n\n```\ncode here\n\nmore code\n```\n\nAfter",
			want: 44, // After the closing ``` and \n\n
		},
		{
			name: "unclosed code block",
			text: "Before code.\n\n```\ncode here\n\nmore code without closing",
			want: 14, // Before the code block starts
		},
		{
			name: "unclosed bold marker in same paragraph",
			text: "First paragraph with **unclosed bold and no break yet",
			want: -1, // No safe boundary - unclosed ** and no paragraph break
		},
		{
			name: "balanced markers",
			text: "Some **bold** and *italic* text.\n\nMore text here",
			want: 34, // After the first paragraph
		},
		{
			name: "code span prevents false positive",
			text: "Some `code with **stars**` here.\n\nMore text",
			want: 34,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindSafeBoundary(tt.text)
			if got != tt.want {
				t.Errorf("FindSafeBoundary() = %d, want %d", got, tt.want)
				if got >= 0 && got <= len(tt.text) {
					t.Errorf("  prefix: %q", tt.text[:got])
				}
				if tt.want >= 0 && tt.want <= len(tt.text) {
					t.Errorf("  expected prefix: %q", tt.text[:tt.want])
				}
			}
		})
	}
}

func TestIsInCodeBlock(t *testing.T) {
	cases := map[string]struct {
		text string
		pos  int
		want bool
	}{
		"no code blocks":             {text: "just regular text", pos: 10},
		"before code block":          {text: "text\n```\ncode\n```\nafter", pos: 3},
		"inside code block":          {text: "text\n```\ncode\n```\nafter", pos: 10, want: true},
		"after closed code block":    {text: "text\n```\ncode\n```\nafter", pos: 20},
		"inside unclosed code block": {text: "text\n```\ncode continues", pos: 15, want: true},
		"indented code fence":        {text: "text\n  ```\ncode\n  ```\nafter", pos: 12, want: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isInCodeBlock(tc.text, tc.pos); got != tc.want {
				t.Errorf("isInCodeBlock(%q, %d) = %v, want %v", tc.text, tc.pos, got, tc.want)
			}
		})
	}
}

func TestAreInlineMarkersBalanced(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "no markers",
			text: "plain text here",
			want: true,
		},
		{
			name: "balanced bold",
			text: "some **bold** text",
			want: true,
		},
		{
			name: "unclosed bold",
			text: "some **bold text",
			want: false,
		},
		{
			name: "balanced italic asterisk",
			text: "some *italic* text",
			want: true,
		},
		{
			name: "unclosed italic asterisk",
			text: "some *italic text",
			want: false,
		},
		{
			name: "balanced code span",
			text: "some `code` here",
			want: true,
		},
		{
			name: "unclosed code span",
			text: "some `code here",
			want: false,
		},
		{
			name: "double backtick code span",
			text: "some ``code with ` backtick`` here",
			want: true,
		},
		{
			name: "unclosed double backtick",
			text: "some ``code here",
			want: false,
		},
		{
			name: "balanced strikethrough",
			text: "some ~~strikethrough~~ text",
			want: true,
		},
		{
			name: "unclosed strikethrough",
			text: "some ~~strikethrough text",
			want: false,
		},
		{
			name: "mixed balanced",
			text: "**bold** and *italic* and `code`",
			want: true,
		},
		{
			name: "nested balanced",
			text: "**bold with *italic* inside**",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := areInlineMarkersBalanced(tt.text)
			if got != tt.want {
				t.Errorf("areInlineMarkersBalanced(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestCountCodeFences(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{
			name: "no fences",
			text: "plain text",
			want: 0,
		},
		{
			name: "one fence",
			text: "text\n```\ncode",
			want: 1,
		},
		{
			name: "two fences (closed block)",
			text: "text\n```\ncode\n```\nafter",
			want: 2,
		},
		{
			name: "fence with language",
			text: "```go\ncode\n```",
			want: 2,
		},
		{
			name: "inline backticks not counted",
			text: "some `inline` code",
			want: 0,
		},
		{
			name: "indented fence",
			text: "  ```\ncode\n  ```",
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countCodeFences(tt.text)
			if got != tt.want {
				t.Errorf("countCodeFences(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}
