package cmd

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/app/ask"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/tools"
)

// These integration tests use the production upload hooks installed by
// ask_compat.go's init, not the fake hooks used by internal/app/ask's unit tests.
func testAskConfig() config.AskConfig {
	return config.AskConfig{StdinInlineMaxBytes: 10, StdinMaxBytes: 1024}
}

func tinyAskPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{B: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPrepareAskInputClassifiesTinyPNGAndPreservesOriginal(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	raw := tinyAskPNG(t)
	got, err := ask.PrepareInput("describe", nil, bytes.NewReader(raw), true, testAskConfig())
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	t.Cleanup(func() { ask.CleanupPreparedInput(got) })
	if len(got.Message.Parts) != 2 || got.Message.Parts[0].Type != core.PartImage {
		t.Fatalf("message parts = %#v", got.Message.Parts)
	}
	part := got.Message.Parts[0]
	if filepath.Ext(part.ImagePath) != ".png" {
		t.Fatalf("image path = %q, want .png", part.ImagePath)
	}
	if part.ImageData == nil || part.ImageData.MediaType != "image/png" || part.ImageData.Width != 2 || part.ImageData.Height != 1 {
		t.Fatalf("image metadata = %#v", part.ImageData)
	}
	decoded, err := base64.StdEncoding.DecodeString(part.ImageData.Base64)
	if err != nil || !bytes.Equal(decoded, raw) {
		t.Fatalf("inline image differs from original: err=%v", err)
	}
	saved, err := os.ReadFile(part.ImagePath)
	if err != nil || !bytes.Equal(saved, raw) {
		t.Fatalf("saved original differs: err=%v", err)
	}
	settings := SessionSettings{}
	settings.AutoTools = ask.AddAutoTools(settings.AutoTools, []core.Message{got.Message})
	if strings.Join(settings.AutoTools, ",") != tools.ReadFileToolName+","+tools.ViewImageToolName {
		t.Fatalf("AutoTools = %v", settings.AutoTools)
	}
}

func TestPrepareAskInputResizesLargeImageWithProviderDimensions(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	img := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	var state uint32 = 1
	for i := range img.Pix {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		img.Pix[i] = byte(state)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	if encoded.Len() <= maxLLMImageBytes {
		t.Fatalf("test PNG is only %d bytes", encoded.Len())
	}
	cfg := testAskConfig()
	cfg.StdinMaxBytes = int64(encoded.Len() + 1)

	got, err := ask.PrepareInput("describe", nil, bytes.NewReader(encoded.Bytes()), true, cfg)
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	t.Cleanup(func() { ask.CleanupPreparedInput(got) })
	if len(got.Message.Parts) != 2 || got.Message.Parts[0].Type != core.PartImage {
		t.Fatalf("message parts = %#v, want image then question", got.Message.Parts)
	}
	part := got.Message.Parts[0]
	if part.ImageData == nil {
		t.Fatal("missing provider image metadata")
	}
	if part.ImageData.MediaType != "image/jpeg" {
		t.Fatalf("provider image MIME = %q, want resized JPEG", part.ImageData.MediaType)
	}
	providerBytes, err := base64.StdEncoding.DecodeString(part.ImageData.Base64)
	if err != nil {
		t.Fatal(err)
	}
	if len(providerBytes) > maxLLMImageBytes {
		t.Fatalf("provider image is %d bytes, want at most %d", len(providerBytes), maxLLMImageBytes)
	}
	width, height := imageDisplayDimensions(providerBytes)
	if width <= 0 || height <= 0 || part.ImageData.Width != width || part.ImageData.Height != height || width >= 1024 || height >= 1024 {
		t.Fatalf("metadata dimensions = %dx%d, provider bytes = %dx%d", part.ImageData.Width, part.ImageData.Height, width, height)
	}
	saved, err := os.ReadFile(part.ImagePath)
	if err != nil || !bytes.Equal(saved, encoded.Bytes()) {
		t.Fatalf("saved original differs: err=%v", err)
	}
}

func TestPrepareAskInputDetectsBinaryMIMEAndExtension(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	raw := []byte("%PDF-1.7\n%\x00binary")
	got, err := ask.PrepareInput("inspect", nil, bytes.NewReader(raw), true, testAskConfig())
	if err != nil {
		t.Fatalf("prepareAskInput: %v", err)
	}
	t.Cleanup(func() { ask.CleanupPreparedInput(got) })
	part := got.Message.Parts[0]
	if part.Type != core.PartFile || part.FileData == nil {
		t.Fatalf("part = %#v", part)
	}
	if part.FileData.MediaType != "application/pdf" || part.FileData.Filename != "stdin.pdf" || filepath.Ext(part.FilePath) != ".pdf" {
		t.Fatalf("metadata/path = %#v %q", part.FileData, part.FilePath)
	}
}
