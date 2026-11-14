package image

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/termimage"
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
	if result.Upload == "" || !strings.Contains(result.Upload, "a=t,t=d") {
		t.Fatalf("console Kitty wrapper should transmit upload bytes without direct display, got upload %q", result.Upload)
	}
	if !strings.Contains(result.Full, "\x1b_G") {
		t.Fatalf("expected delegated Kitty graphics command, got %q", result.Full)
	}
	if !strings.Contains(result.Full, "a=p,U=1") || !strings.Contains(result.Full, "\U0010eeee") {
		t.Fatalf("console image display should use Kitty Unicode placeholders for scrollback stability: %q", result.Full)
	}
	if result.Placeholder == "" || !strings.Contains(result.Placeholder, "\U0010eeee") {
		t.Fatalf("placeholder/display should contain Kitty placeholder cells: %q", result.Placeholder)
	}
}

func TestTerminalImageWriterAdvancesOneLineAfterKittyPlaceholders(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	path := writeTerminalTestPNG(t)
	var buf bytes.Buffer
	if err := RenderImageToWriter(&buf, path); err != nil {
		t.Fatalf("RenderImageToWriter() error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "a=p,U=1") || !strings.Contains(out, "\U0010eeee") {
		t.Fatalf("expected Kitty Unicode placeholder rendering, got %q", out)
	}
	if !strings.HasSuffix(out, "\r\n") {
		t.Fatalf("writer should finish on next line after placeholder grid, got %q", out)
	}
	if strings.Count(out, "\r\n") != 1 {
		t.Fatalf("placeholder grid consumes image rows; writer should add one CRLF, got %q", out)
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
	img := image.NewNRGBA(image.Rect(0, 0, 20, 40))
	for y := 0; y < 40; y++ {
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
