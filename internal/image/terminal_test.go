package image

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/termimage"
)

func TestTerminalImageWrapperUsesTermimageKittyImplementation(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	path := writeTerminalTestPNG(t)
	result, err := RenderImageToString(path)
	if err != nil {
		t.Fatalf("RenderImageToString() error = %v", err)
	}
	if result.Full == "" {
		t.Fatal("expected Kitty terminal image output")
	}
	if result.Upload == "" || !strings.Contains(result.Upload, "a=t") {
		t.Fatalf("direct terminal wrapper should expose reusable Kitty upload bytes, got upload %q", result.Upload)
	}
	if !strings.Contains(result.Full, "\x1b_G") {
		t.Fatalf("expected delegated Kitty graphics command, got %q", result.Full)
	}
	if strings.Contains(result.Full, "a=T,U=1") || strings.Contains(result.Full, "a=p,U=1") || strings.Contains(result.Full, "\U0010eeee") {
		t.Fatalf("standard image display should use direct Kitty rendering, not Unicode placeholders: %q", result.Full)
	}
	if result.Placeholder != result.Full {
		t.Fatalf("direct terminal wrapper should expose full output as placeholder/display")
	}
}

func TestDetectCapabilityUsesTermimageTmuxPolicy(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "")
	t.Setenv("TERM", "xterm-kitty")
	t.Setenv("KITTY_WINDOW_ID", "1")
	t.Setenv("TMUX", "/tmp/tmux")
	if got := DetectCapability(); got != CapNone {
		t.Fatalf("DetectCapability() inside tmux = %s, want none without forcing", got)
	}

	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	if got := DetectCapability(); got != CapKitty {
		t.Fatalf("DetectCapability() forced inside tmux = %s, want kitty", got)
	}
}

func writeTerminalTestPNG(t *testing.T) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 20, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 20; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(10 + x), G: uint8(20 + y), B: 180, A: 255})
		}
	}
	f, err := os.CreateTemp(t.TempDir(), "terminal-image-*.png")
	if err != nil {
		t.Fatalf("create temp image: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode temp image: %v", err)
	}
	return f.Name()
}
