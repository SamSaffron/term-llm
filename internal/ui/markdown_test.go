package ui

import "testing"

func TestRenderMarkdownWithError_ZeroWidth_DoesNotError(t *testing.T) {
	_, err := RenderMarkdownWithError("# title", 0)
	if err != nil {
		t.Fatalf("RenderMarkdownWithError must not fail for zero width: %v", err)
	}
}

func TestRenderMarkdownWithOptionsError_ZeroWidth_DoesNotError(t *testing.T) {
	_, err := RenderMarkdownWithOptionsError("# title", 0, MarkdownRenderOptions{
		WrapOffset:         2,
		NormalizeTabs:      true,
		NormalizeNewlines:  false,
		EnsureTrailingLine: true,
	})
	if err != nil {
		t.Fatalf("RenderMarkdownWithOptionsError must not fail for zero width: %v", err)
	}
}
