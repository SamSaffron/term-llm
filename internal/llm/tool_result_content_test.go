package llm

import (
	"strings"
	"testing"
)

func TestTruncateToolOutputStructuredContent(t *testing.T) {
	image := &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}
	original := ToolOutput{
		Content: strings.Repeat("unbounded fallback", 20),
		ContentParts: []ToolContentPart{
			{Type: ToolContentPartText, Text: "abcdefghij"},
			{Type: ToolContentPartImageData, ImageData: image},
			{Type: ToolContentPartText, Text: "klmnopqrst"},
		},
	}

	got := truncateToolOutput(original, 10)
	if len(got.ContentParts) != 3 {
		t.Fatalf("ContentParts length = %d, want 3", len(got.ContentParts))
	}
	if got.ContentParts[0].Type != ToolContentPartText ||
		got.ContentParts[1].Type != ToolContentPartImageData ||
		got.ContentParts[2].Type != ToolContentPartText {
		t.Fatalf("ContentParts order changed: %#v", got.ContentParts)
	}
	if got.ContentParts[1].ImageData == nil || got.ContentParts[1].ImageData.Base64 != image.Base64 {
		t.Fatalf("image was not preserved: %#v", got.ContentParts[1])
	}
	if got.ContentParts[1].ImageData == image {
		t.Fatal("image metadata was not cloned")
	}
	if !strings.HasPrefix(got.ContentParts[0].Text, "abcde") || !strings.Contains(got.ContentParts[0].Text, "10 chars truncated") {
		t.Fatalf("first text part = %q, want retained head and marker", got.ContentParts[0].Text)
	}
	if got.ContentParts[2].Text != "pqrst" {
		t.Fatalf("last text part = %q, want retained tail", got.ContentParts[2].Text)
	}
	if got.Content != got.ContentParts[0].Text+got.ContentParts[2].Text {
		t.Fatalf("Content = %q, want flattened bounded structured text", got.Content)
	}
	if strings.Contains(got.Content, "unbounded fallback") {
		t.Fatalf("Content retained stale unbounded fallback: %q", got.Content)
	}

	if original.ContentParts[0].Text != "abcdefghij" || original.ContentParts[2].Text != "klmnopqrst" {
		t.Fatalf("original output was mutated: %#v", original.ContentParts)
	}
}

func TestTruncateToolOutputImageOnlyFallback(t *testing.T) {
	got := truncateToolOutput(ToolOutput{
		Content: strings.Repeat("x", 100),
		ContentParts: []ToolContentPart{{
			Type:      ToolContentPartImageData,
			ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="},
		}},
	}, 20)

	if len(got.ContentParts) != 1 || got.ContentParts[0].Type != ToolContentPartImageData {
		t.Fatalf("image-only parts changed: %#v", got.ContentParts)
	}
	if !strings.Contains(got.Content, "80 chars truncated") {
		t.Fatalf("image-only text fallback was not truncated: %q", got.Content)
	}
}

func TestEngineApplyCompactionToolResultLimitCoversContentParts(t *testing.T) {
	engine := NewEngine(nil, nil)
	config := DefaultCompactionConfig()
	config.MaxToolResultChars = 6
	engine.SetCompaction(1000, config)

	got := engine.applyToolOutputTruncation(ToolOutput{
		ContentParts: []ToolContentPart{
			{Type: ToolContentPartText, Text: "abcdef"},
			{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
			{Type: ToolContentPartText, Text: "ghijkl"},
		},
	})

	if !strings.Contains(got.Content, "6 chars truncated") {
		t.Fatalf("structured output bypassed compaction tool-result limit: %q", got.Content)
	}
	if got.ContentParts[1].Type != ToolContentPartImageData {
		t.Fatalf("mixed content order changed: %#v", got.ContentParts)
	}
}

func TestEngineApplyToolOutputTruncationCoversContentParts(t *testing.T) {
	engine := NewEngine(nil, nil)
	engine.SetMaxToolOutputChars(8)

	got := engine.applyToolOutputTruncation(ToolOutput{
		Content: "stale and unbounded",
		ContentParts: []ToolContentPart{
			{Type: ToolContentPartText, Text: "123456"},
			{Type: ToolContentPartImageData, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
			{Type: ToolContentPartText, Text: "789012"},
		},
	})

	if !strings.Contains(got.Content, "4 chars truncated") {
		t.Fatalf("structured output bypassed engine truncation: %q", got.Content)
	}
	if got.ContentParts[1].Type != ToolContentPartImageData {
		t.Fatalf("mixed content order changed: %#v", got.ContentParts)
	}
	if got.Content != got.ContentParts[0].Text+got.ContentParts[2].Text {
		t.Fatalf("Content and ContentParts diverged: content=%q parts=%#v", got.Content, got.ContentParts)
	}
}
