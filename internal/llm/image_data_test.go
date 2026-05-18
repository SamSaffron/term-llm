package llm

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestPartImageDataFallsBackToImagePath(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "sample.png")
	data := []byte("png-bytes")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	mimeType, base64Data, ok := partImageData(Part{
		Type:      PartImage,
		ImagePath: path,
		ImageData: &ToolImageData{MediaType: "image/png"},
	})
	if !ok {
		t.Fatal("partImageData returned ok=false")
	}
	if mimeType != "image/png" {
		t.Fatalf("mimeType = %q, want image/png", mimeType)
	}
	if base64Data != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("base64Data = %q, want encoded file bytes", base64Data)
	}
}

func TestToolResultImageDataFallsBackToImagePath(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "sample.png")
	data := []byte("png-bytes")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	mimeType, base64Data, ok := toolResultImageData(ToolContentPart{
		Type:      ToolContentPartImageData,
		ImagePath: path,
		ImageData: &ToolImageData{MediaType: "image/png"},
	})
	if !ok {
		t.Fatal("toolResultImageData returned ok=false")
	}
	if mimeType != "image/png" {
		t.Fatalf("mimeType = %q, want image/png", mimeType)
	}
	if base64Data != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("base64Data = %q, want encoded file bytes", base64Data)
	}
}
