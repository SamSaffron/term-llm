package chat

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/termimage"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestAltScreenKittyImageUploadsStayOutOfViewportContent(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	path := writeChatTestPNG(t)
	m := newTestChatModel(true)
	m.width = 40
	m.height = 20
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	m.tracker = ui.NewToolTracker()
	m.tracker.AddImageSegment(path)
	m.tracker.AddImageSegment(path) // duplicate references should share one upload
	m.streaming = true
	m.bumpContentVersion()

	first := m.viewAltScreen()
	if strings.Contains(first, "\x1b_G") {
		t.Fatalf("alt-screen View content must not embed raw Kitty upload bytes; got %q", first)
	}
	upload := m.drainPendingImageUploads()
	if !strings.Contains(upload, "\x1b_G") {
		t.Fatalf("first alt-screen render should queue Kitty upload out-of-band; got %q", upload)
	}
	if got := strings.Count(upload, "a=T,t=d,f=100"); got != 1 {
		t.Fatalf("duplicate image references should be uploaded once, got %d transmit commands in %q", got, upload)
	}

	content := m.viewCache.lastContentStr
	if content == "" && len(m.contentLines) > 0 {
		content = strings.Join(m.contentLines, "\n")
	}
	if content == "" {
		t.Fatal("expected viewport content cache to be populated")
	}
	if strings.Contains(content, "\x1b_G") {
		t.Fatalf("viewport content must not contain raw Kitty APC upload bytes: %q", content)
	}
	if strings.Contains(m.viewport.View(), "\x1b_G") {
		t.Fatalf("rendered viewport must not contain raw Kitty APC upload bytes: %q", m.viewport.View())
	}
	if !strings.Contains(content, "\U0010eeee") {
		t.Fatalf("viewport content should retain Kitty placeholder cells for scrolling: %q", content)
	}
	if captions := strings.Count(content, "[Generated image: "+path+"]"); captions != 2 {
		t.Fatalf("viewport content should include one visible caption per image reference, got %d in %q", captions, content)
	}

	second := m.viewAltScreen()
	if strings.Contains(second, "\x1b_G") {
		t.Fatalf("unchanged image view should not contain raw upload bytes: %q", second)
	}
	if upload := m.drainPendingImageUploads(); upload != "" {
		t.Fatalf("unchanged image should not be re-uploaded on the next frame: %q", upload)
	}
}

func TestAltScreenImageUploadCmdUsesTeaRaw(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	path := writeChatTestPNG(t)
	m := newTestChatModel(true)
	m.width = 40
	m.height = 20

	artifact := m.renderViewportImageArtifact(path)
	if artifact.Display == "" {
		t.Fatalf("expected display placeholder for %s", path)
	}
	cmd := m.drainPendingImageUploadCmd()
	if cmd == nil {
		t.Fatal("expected pending image upload command")
	}
	msg := cmd()
	raw, ok := msg.(tea.RawMsg)
	if !ok {
		t.Fatalf("upload command message = %T, want tea.RawMsg", msg)
	}
	upload := fmt.Sprint(raw.Msg)
	if !strings.Contains(upload, "\x1b_G") {
		t.Fatalf("tea.Raw upload should contain Kitty APC bytes, got %q", upload)
	}
}

func TestAltScreenImageCleanupCmdDeletesKittyPlacements(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	m := newTestChatModel(true)

	cmd := m.terminalImageCleanupCmd()
	if cmd == nil {
		t.Fatal("expected Kitty cleanup command in alt-screen mode")
	}
	msg := cmd()
	raw, ok := msg.(tea.RawMsg)
	if !ok {
		t.Fatalf("cleanup command message = %T, want tea.RawMsg", msg)
	}
	cleanup := fmt.Sprint(raw.Msg)
	if !strings.Contains(cleanup, "a=d,d=A") {
		t.Fatalf("cleanup should delete visible Kitty placements, got %q", cleanup)
	}
}

func writeChatTestPNG(t *testing.T) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 20, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 20; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(50 + x), G: uint8(80 + y), B: 180, A: 255})
		}
	}

	f, err := os.CreateTemp(t.TempDir(), "chat-image-*.png")
	if err != nil {
		t.Fatalf("create temp image: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode temp image: %v", err)
	}
	return f.Name()
}
