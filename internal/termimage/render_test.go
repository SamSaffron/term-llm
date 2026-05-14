package termimage

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSelectProtocolByModeAndEnvironment(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		env  Environment
		want Protocol
	}{
		{
			name: "kitty viewport",
			req:  Request{Mode: ModeViewport, Protocol: ProtocolAuto},
			env:  Environment{KittyWindowID: "1", Term: "xterm-kitty"},
			want: ProtocolKitty,
		},
		{
			name: "ghostty viewport",
			req:  Request{Mode: ModeViewport, Protocol: ProtocolAuto},
			env:  Environment{TermProgram: "Ghostty"},
			want: ProtocolKitty,
		},
		{
			name: "iterm viewport falls back to ansi",
			req:  Request{Mode: ModeViewport, Protocol: ProtocolAuto},
			env:  Environment{TermProgram: "iTerm.app"},
			want: ProtocolANSI,
		},
		{
			name: "iterm scrollback uses iterm",
			req:  Request{Mode: ModeScrollback, Protocol: ProtocolAuto},
			env:  Environment{TermProgram: "iTerm.app"},
			want: ProtocolITerm,
		},
		{
			name: "forced ansi",
			req:  Request{Mode: ModeViewport, Protocol: ProtocolAuto},
			env:  Environment{KittyWindowID: "1", ForcedProtocol: "ansi"},
			want: ProtocolANSI,
		},
		{
			name: "forced viewport iterm falls back to ansi",
			req:  Request{Mode: ModeViewport, Protocol: ProtocolAuto},
			env:  Environment{ForcedProtocol: "iterm"},
			want: ProtocolANSI,
		},
		{
			name: "forced none",
			req:  Request{Mode: ModeViewport, Protocol: ProtocolAuto},
			env:  Environment{KittyWindowID: "1", ForcedProtocol: "none"},
			want: ProtocolNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Select(tt.req, tt.env)
			if got.Protocol != tt.want {
				t.Fatalf("Select() protocol = %s, want %s (strategy %s)", got.Protocol, tt.want, got.Name)
			}
		})
	}
}

func TestRenderKittyViewportSplitsUploadAndDisplay(t *testing.T) {
	ClearCache()
	atomic.StoreUint32(&imageIDCounter, 0)

	path := writeTestPNG(t, image.Rect(0, 0, 30, 20), func(x, y int) color.Color {
		return color.NRGBA{R: uint8(10 + x), G: uint8(20 + y), B: 200, A: 255}
	})

	result, err := RenderWithEnvironment(Request{
		Path:         path,
		Mode:         ModeViewport,
		Protocol:     ProtocolAuto,
		MaxCols:      3,
		MaxRows:      2,
		CellWidthPx:  10,
		CellHeightPx: 10,
	}, Environment{KittyWindowID: "1"})
	if err != nil {
		t.Fatalf("RenderWithEnvironment() error = %v", err)
	}
	if result.Protocol != ProtocolKitty {
		t.Fatalf("protocol = %s, want kitty", result.Protocol)
	}
	if result.Upload == "" {
		t.Fatal("expected non-empty out-of-band upload")
	}
	if result.Display == "" {
		t.Fatal("expected non-empty viewport-safe display")
	}
	if result.Full != result.Upload+result.Display {
		t.Fatal("Full should be Upload+Display")
	}
	if result.WidthCells != 3 || result.HeightCells != 2 {
		t.Fatalf("cells = %dx%d, want 3x2", result.WidthCells, result.HeightCells)
	}
	if !strings.Contains(result.Upload, "\x1b_G") {
		t.Fatalf("upload should contain Kitty APC commands: %q", result.Upload)
	}
	if !strings.Contains(result.Upload, "a=T,t=d,f=100") {
		t.Fatalf("upload should transmit PNG data separately: %q", result.Upload)
	}
	if !strings.Contains(result.Upload, "a=p,U=1") {
		t.Fatalf("upload should create Unicode-placeholder placement: %q", result.Upload)
	}
	if strings.Contains(result.Upload, "a=T,U=1") {
		t.Fatalf("upload should not use transmit-and-display placeholders: %q", result.Upload)
	}
	if strings.Contains(result.Display, "\x1b_G") {
		t.Fatalf("display must not contain raw Kitty APC upload bytes: %q", result.Display)
	}
	if !strings.Contains(result.Display, "\U0010eeee") {
		t.Fatalf("display should contain Kitty placeholder cells: %q", result.Display)
	}

	lines := strings.Split(result.Display, "\n")
	if len(lines) != result.HeightCells {
		t.Fatalf("display line count = %d, want %d", len(lines), result.HeightCells)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "\x1b[38;2;") {
			t.Fatalf("display line %d is not self-contained with foreground color: %q", i, line)
		}
		if !strings.HasSuffix(line, "\x1b[39m") {
			t.Fatalf("display line %d does not reset foreground color: %q", i, line)
		}
	}
}

func TestRenderANSIHalfBlockCompositesTransparency(t *testing.T) {
	ClearCache()

	path := writeTestPNG(t, image.Rect(0, 0, 2, 4), func(x, y int) color.Color {
		return color.NRGBA{R: 0, G: 0, B: 0, A: 0}
	})

	result, err := RenderWithEnvironment(Request{
		Path:       path,
		Mode:       ModeViewport,
		Protocol:   ProtocolANSI,
		MaxCols:    2,
		MaxRows:    2,
		Background: color.NRGBA{R: 10, G: 20, B: 30, A: 255},
		// Use 1x2 pixel cells so the 2x4 source maps to 2x2 terminal cells.
		CellWidthPx:  1,
		CellHeightPx: 2,
	}, Environment{})
	if err != nil {
		t.Fatalf("RenderWithEnvironment() error = %v", err)
	}
	if result.Protocol != ProtocolANSI {
		t.Fatalf("protocol = %s, want ansi", result.Protocol)
	}
	if result.WidthCells != 2 || result.HeightCells != 2 {
		t.Fatalf("cells = %dx%d, want 2x2", result.WidthCells, result.HeightCells)
	}
	if result.Upload != "" {
		t.Fatalf("ANSI renderer should not produce upload bytes: %q", result.Upload)
	}
	if strings.Contains(result.Display, "\x1b_G") || strings.Contains(result.Display, "\x1b]") {
		t.Fatalf("ANSI display should contain only normal text/SGR escapes: %q", result.Display)
	}
	if got := strings.Count(result.Display, "▀"); got != 4 {
		t.Fatalf("half-block count = %d, want 4 in %q", got, result.Display)
	}
	lines := strings.Split(result.Display, "\n")
	if len(lines) != result.HeightCells {
		t.Fatalf("display line count = %d, want %d", len(lines), result.HeightCells)
	}
	for i, line := range lines {
		if !strings.HasSuffix(line, "\x1b[0m") {
			t.Fatalf("ANSI line %d should reset SGR for independent viewport clipping: %q", i, line)
		}
	}
	if !strings.Contains(result.Display, "\x1b[38;2;10;20;30m") || !strings.Contains(result.Display, "\x1b[48;2;10;20;30m") {
		t.Fatalf("transparent pixels should composite against configured background, got %q", result.Display)
	}
	if strings.Contains(result.Display, "38;2;0;0;0m") || strings.Contains(result.Display, "48;2;0;0;0m") {
		t.Fatalf("transparent pixels should not fall back to black, got %q", result.Display)
	}
}

func writeTestPNG(t *testing.T, rect image.Rectangle, pixel func(x, y int) color.Color) string {
	t.Helper()
	img := image.NewNRGBA(rect)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			img.Set(x, y, pixel(x, y))
		}
	}

	f, err := os.CreateTemp(t.TempDir(), "image-*.png")
	if err != nil {
		t.Fatalf("create temp image: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode temp image: %v", err)
	}
	return f.Name()
}
