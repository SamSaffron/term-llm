package serve

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
)

func newServeServerForMediaTest(t *testing.T) *serveServer {
	t.Helper()
	dir := t.TempDir()
	srv := &serveServer{cfgRef: &config.Config{}}
	srv.cfgRef.Image.OutputDir = dir
	return srv
}

// pngBytes is a minimal but valid 1×1 PNG.
var pngBytes = func() []byte {
	// 1×1 red PNG (67 bytes)
	const encoded = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8/5+hHgAHggJ/PchI6QAAAABJRU5ErkJggg=="
	b, _ := base64.StdEncoding.DecodeString(encoded)
	return b
}()

// TestToolContentPartsPayloadEmpty returns nil for empty input.
func TestToolContentPartsPayloadEmpty(t *testing.T) {
	srv := newServeServerForMediaTest(t)
	if got := srv.toolContentPartsPayload(nil); got != nil {
		t.Errorf("expected nil for empty parts, got %v", got)
	}
	if got := srv.toolContentPartsPayload([]core.ToolContentPart{}); got != nil {
		t.Errorf("expected nil for zero-len parts, got %v", got)
	}
}

// TestToolContentPartsPayloadTextOnly returns a text entry.
func TestToolContentPartsPayloadTextOnly(t *testing.T) {
	srv := newServeServerForMediaTest(t)
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartText, Text: "hello world"},
	}
	got := srv.toolContentPartsPayload(parts)
	if len(got) != 1 {
		t.Fatalf("expected 1 part, got %d", len(got))
	}
	if got[0]["type"] != "text" {
		t.Errorf("type = %v, want text", got[0]["type"])
	}
	if got[0]["text"] != "hello world" {
		t.Errorf("text = %v, want hello world", got[0]["text"])
	}
}

// TestToolContentPartsPayloadEmptyTextDropped drops empty text parts.
func TestToolContentPartsPayloadEmptyTextDropped(t *testing.T) {
	srv := newServeServerForMediaTest(t)
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartText, Text: ""},
	}
	if got := srv.toolContentPartsPayload(parts); got != nil {
		t.Errorf("expected nil when only empty text parts, got %v", got)
	}
}

// TestToolContentPartsPayloadImageMaterialized materializes a valid inline PNG
// and verifies the URL is a route URL (not a bare filesystem path).
func TestToolContentPartsPayloadImageMaterialized(t *testing.T) {
	srv := newServeServerForMediaTest(t)
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{
			MediaType: "image/png",
			Base64:    b64,
		}},
	}
	got := srv.toolContentPartsPayload(parts)
	if len(got) != 1 {
		t.Fatalf("expected 1 part, got %d", len(got))
	}
	if got[0]["type"] != "image" {
		t.Errorf("type = %v, want image", got[0]["type"])
	}
	url, ok := got[0]["url"].(string)
	if !ok || url == "" {
		t.Fatalf("url missing or empty: %v", got[0]["url"])
	}
	// Must be a route URL (/images/...) not a filesystem path (/tmp/...).
	if !strings.HasPrefix(url, "/") {
		t.Errorf("url must start with /: got %q", url)
	}
	wantPrefix := srv.cfg.imagesRoute()
	if !strings.HasPrefix(url, wantPrefix) {
		t.Errorf("url %q must start with images route %q (not a raw filesystem path)", url, wantPrefix)
	}
	if got[0]["media_type"] != "image/png" {
		t.Errorf("media_type = %v, want image/png", got[0]["media_type"])
	}
}

// TestToolContentPartsPayloadImageWithDimensions carries width/height.
func TestToolContentPartsPayloadImageWithDimensions(t *testing.T) {
	srv := newServeServerForMediaTest(t)
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{
			MediaType: "image/png",
			Base64:    b64,
			Width:     100,
			Height:    200,
		}},
	}
	got := srv.toolContentPartsPayload(parts)
	if len(got) != 1 {
		t.Fatalf("expected 1 part, got %d", len(got))
	}
	if got[0]["width"] != 100 {
		t.Errorf("width = %v, want 100", got[0]["width"])
	}
	if got[0]["height"] != 200 {
		t.Errorf("height = %v, want 200", got[0]["height"])
	}
}

// TestToolContentPartsPayloadOversizedImageDropped drops images that exceed the limit.
func TestToolContentPartsPayloadOversizedImageDropped(t *testing.T) {
	srv := newServeServerForMediaTest(t)
	tooLarge := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", maxMaterializedSessionImageBytes+1)))
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{
			MediaType: "image/png",
			Base64:    tooLarge,
		}},
	}
	if got := srv.toolContentPartsPayload(parts); got != nil {
		t.Errorf("expected nil for oversized image, got %v", got)
	}
}

// TestToolContentPartsPayloadNilImageDataDropped drops image parts with nil ImageData.
func TestToolContentPartsPayloadNilImageDataDropped(t *testing.T) {
	srv := newServeServerForMediaTest(t)
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartImageData, ImageData: nil},
	}
	if got := srv.toolContentPartsPayload(parts); got != nil {
		t.Errorf("expected nil for nil ImageData, got %v", got)
	}
}

// TestToolContentPartsPayloadOrderPreserved preserves text/image/text ordering.
func TestToolContentPartsPayloadOrderPreserved(t *testing.T) {
	srv := newServeServerForMediaTest(t)
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	parts := []core.ToolContentPart{
		{Type: core.ToolContentPartText, Text: "before"},
		{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{MediaType: "image/png", Base64: b64}},
		{Type: core.ToolContentPartText, Text: "after"},
	}
	got := srv.toolContentPartsPayload(parts)
	if len(got) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(got))
	}
	if got[0]["type"] != "text" {
		t.Errorf("[0] type = %v, want text", got[0]["type"])
	}
	if got[1]["type"] != "image" {
		t.Errorf("[1] type = %v, want image", got[1]["type"])
	}
	if got[2]["type"] != "text" {
		t.Errorf("[2] type = %v, want text", got[2]["type"])
	}
	if got[0]["text"] != "before" {
		t.Errorf("[0] text = %v, want before", got[0]["text"])
	}
	if got[2]["text"] != "after" {
		t.Errorf("[2] text = %v, want after", got[2]["text"])
	}
}
