package llm

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestTruncateToolOutputStructuredContent(t *testing.T) {
	image := &core.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}
	original := core.ToolOutput{
		Content: strings.Repeat("unbounded fallback", 20),
		ContentParts: []core.ToolContentPart{
			{Type: core.ToolContentPartText, Text: "abcdefghij"},
			{Type: core.ToolContentPartImageData, ImageData: image},
			{Type: core.ToolContentPartText, Text: "klmnopqrst"},
		},
	}

	got := core.TruncateToolOutput(original, 10)
	if len(got.ContentParts) != 3 {
		t.Fatalf("ContentParts length = %d, want 3", len(got.ContentParts))
	}
	if got.ContentParts[0].Type != core.ToolContentPartText ||
		got.ContentParts[1].Type != core.ToolContentPartImageData ||
		got.ContentParts[2].Type != core.ToolContentPartText {
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
	got := core.TruncateToolOutput(core.ToolOutput{
		Content: strings.Repeat("x", 100),
		ContentParts: []core.ToolContentPart{{
			Type:      core.ToolContentPartImageData,
			ImageData: &core.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="},
		}},
	}, 20)

	if len(got.ContentParts) != 1 || got.ContentParts[0].Type != core.ToolContentPartImageData {
		t.Fatalf("image-only parts changed: %#v", got.ContentParts)
	}
	if !strings.Contains(got.Content, "80 chars truncated") {
		t.Fatalf("image-only text fallback was not truncated: %q", got.Content)
	}
}
