package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func writeTestPNG(t *testing.T, path string) {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{G: 255, A: 255})
	img.Set(0, 1, color.RGBA{B: 255, A: 255})
	img.Set(1, 1, color.RGBA{R: 255, G: 255, B: 255, A: 255})

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create test png: %v", err)
	}
	defer f.Close()

	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
}

func TestViewImageToolExecute_ReturnsStructuredImageData(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.png")
	writeTestPNG(t, filePath)

	tool := NewViewImageTool(nil)
	args, err := json.Marshal(ViewImageArgs{FilePath: filePath})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if strings.Contains(out.Content, "[IMAGE_DATA:") {
		t.Fatalf("expected text output without IMAGE_DATA marker, got: %q", out.Content)
	}
	if len(out.ContentParts) != 2 {
		t.Fatalf("expected 2 content parts (text + image_data), got %d", len(out.ContentParts))
	}

	textPart := out.ContentParts[0]
	if textPart.Type != llm.ToolContentPartText {
		t.Fatalf("expected first content part type %q, got %q", llm.ToolContentPartText, textPart.Type)
	}
	if textPart.Text != out.Content {
		t.Fatalf("expected first content part text to match Content")
	}

	imagePart := out.ContentParts[1]
	if imagePart.Type != llm.ToolContentPartImageData {
		t.Fatalf("expected second content part type %q, got %q", llm.ToolContentPartImageData, imagePart.Type)
	}
	if imagePart.ImageData == nil {
		t.Fatal("expected second content part image_data to be non-nil")
	}
	if imagePart.ImageData.MediaType != "image/png" {
		t.Fatalf("expected media type image/png, got %q", imagePart.ImageData.MediaType)
	}
	if _, err := base64.StdEncoding.DecodeString(imagePart.ImageData.Base64); err != nil {
		t.Fatalf("image_data base64 should be valid: %v", err)
	}
}

func TestViewImageToolExecute_DetectsMimeWhenExtensionMismatched(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.jpg") // intentionally mismatched extension
	writeTestPNG(t, filePath)

	tool := NewViewImageTool(nil)
	args, err := json.Marshal(ViewImageArgs{FilePath: filePath})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(out.ContentParts) < 2 || out.ContentParts[1].ImageData == nil {
		t.Fatalf("expected image content part in output")
	}

	imagePart := out.ContentParts[1]
	if imagePart.ImageData.MediaType != "image/png" {
		t.Fatalf("expected media type image/png for PNG bytes, got %q", imagePart.ImageData.MediaType)
	}

	decoded, err := base64.StdEncoding.DecodeString(imagePart.ImageData.Base64)
	if err != nil {
		t.Fatalf("image_data base64 should be valid: %v", err)
	}
	if got := http.DetectContentType(decoded); got != "image/png" {
		t.Fatalf("expected decoded bytes to be image/png, got %q", got)
	}
}
