package ui

import "testing"

func TestRenderMarkdownWithError_ZeroWidth_DoesNotError(t *testing.T) {
	_, err := RenderMarkdownWithError("# title", 0)
	if err != nil {
		t.Fatalf("RenderMarkdownWithError must not fail for zero width: %v", err)
	}
}
