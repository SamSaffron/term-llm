package presentation_test

import (
	"testing"

	"github.com/term-llm/term-llm/internal/codemode/presentation"
	"github.com/term-llm/term-llm/internal/llm/core"
)

// stubURL is an ImageURLFunc that returns a /images/<base64> stub URL.
func stubURL(mediaType, base64Data string) (string, bool) {
	if base64Data == "" {
		return "", false
	}
	return "/images/" + base64Data[:min(8, len(base64Data))], true
}

func failURL(_, _ string) (string, bool) { return "", false }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestContentPartsPayload_Nil(t *testing.T) {
	if got := presentation.ContentPartsPayload(nil, stubURL); got != nil {
		t.Fatalf("expected nil for nil parts, got %v", got)
	}
}

func TestContentPartsPayload_Empty(t *testing.T) {
	if got := presentation.ContentPartsPayload([]core.ToolContentPart{}, stubURL); got != nil {
		t.Fatalf("expected nil for empty parts, got %v", got)
	}
}

func TestContentPartsPayload_TextOnly(t *testing.T) {
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartText, Text: "hello"},
	}
	got := presentation.ContentPartsPayload(parts, stubURL)
	if len(got) != 1 {
		t.Fatalf("expected 1 part, got %d", len(got))
	}
	if got[0]["type"] != "text" || got[0]["text"] != "hello" {
		t.Errorf("unexpected part: %v", got[0])
	}
}

func TestContentPartsPayload_EmptyTextDropped(t *testing.T) {
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartText, Text: ""},
	}
	if got := presentation.ContentPartsPayload(parts, stubURL); got != nil {
		t.Fatalf("expected nil when only empty text, got %v", got)
	}
}

func TestContentPartsPayload_ImageWithURL(t *testing.T) {
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{
			MediaType: "image/png",
			Base64:    "ABCDEFGH",
			Width:     100,
			Height:    200,
		}},
	}
	got := presentation.ContentPartsPayload(parts, stubURL)
	if len(got) != 1 {
		t.Fatalf("expected 1 part, got %d", len(got))
	}
	p := got[0]
	if p["type"] != "image" {
		t.Errorf("expected type=image, got %v", p["type"])
	}
	url, ok := p["url"].(string)
	if !ok || url == "" {
		t.Errorf("expected non-empty url, got %v", p["url"])
	}
	if !hasPrefix(url, "/images/") {
		t.Errorf("expected route URL prefix, got %q", url)
	}
	if p["media_type"] != "image/png" {
		t.Errorf("expected media_type=image/png, got %v", p["media_type"])
	}
	if p["width"] != 100 {
		t.Errorf("expected width=100, got %v", p["width"])
	}
	if p["height"] != 200 {
		t.Errorf("expected height=200, got %v", p["height"])
	}
}

func TestContentPartsPayload_ImageURLFails(t *testing.T) {
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{
			MediaType: "image/png", Base64: "ABCDEF",
		}},
	}
	if got := presentation.ContentPartsPayload(parts, failURL); got != nil {
		t.Fatalf("expected nil when image URL fails, got %v", got)
	}
}

func TestContentPartsPayload_NilImageData(t *testing.T) {
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartImageData, ImageData: nil},
	}
	if got := presentation.ContentPartsPayload(parts, stubURL); got != nil {
		t.Fatalf("expected nil for nil ImageData, got %v", got)
	}
}

func TestContentPartsPayload_Interleaved(t *testing.T) {
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartText, Text: "before"},
		{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{
			MediaType: "image/jpeg", Base64: "XYZXYZXY",
		}},
		{Type: core.ToolContentPartText, Text: "after"},
	}
	got := presentation.ContentPartsPayload(parts, stubURL)
	if len(got) != 3 {
		t.Fatalf("expected 3 parts, got %d: %v", len(got), got)
	}
	if got[0]["type"] != "text" {
		t.Errorf("part 0 should be text, got %v", got[0]["type"])
	}
	if got[1]["type"] != "image" {
		t.Errorf("part 1 should be image, got %v", got[1]["type"])
	}
	if got[2]["type"] != "text" {
		t.Errorf("part 2 should be text, got %v", got[2]["type"])
	}
}

func TestContentPartsPayload_NoDimensionsWhenZero(t *testing.T) {
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{
			MediaType: "image/png", Base64: "ABCDEF", Width: 0, Height: 0,
		}},
	}
	got := presentation.ContentPartsPayload(parts, stubURL)
	if len(got) != 1 {
		t.Fatalf("expected 1 part, got %d", len(got))
	}
	if _, ok := got[0]["width"]; ok {
		t.Error("width should be absent when 0")
	}
	if _, ok := got[0]["height"]; ok {
		t.Error("height should be absent when 0")
	}
}

func TestContentPartsPayload_NoMediaTypeWhenEmpty(t *testing.T) {
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{
			MediaType: "", Base64: "ABCDEF",
		}},
	}
	got := presentation.ContentPartsPayload(parts, stubURL)
	if len(got) != 1 {
		t.Fatalf("expected 1 part, got %d", len(got))
	}
	if _, ok := got[0]["media_type"]; ok {
		t.Error("media_type should be absent when empty")
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
