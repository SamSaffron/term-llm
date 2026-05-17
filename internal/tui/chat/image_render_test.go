package chat

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/termimage"
	"github.com/samsaffron/term-llm/internal/ui"
)

func drainPendingImageRawForTest(t *testing.T, m *Model) string {
	t.Helper()
	cmd := m.drainPendingImageUploadCmd()
	if cmd == nil {
		t.Fatal("expected pending image raw command")
	}
	msg := cmd()
	raw, ok := msg.(tea.RawMsg)
	if !ok {
		t.Fatalf("image command message = %T, want tea.RawMsg", msg)
	}
	return fmt.Sprint(raw.Msg)
}

func settleVisibleKittyImagesForTest(t *testing.T, m *Model) string {
	t.Helper()
	m.viewCache.lastSetContentAt = time.Time{}
	_ = m.viewAltScreen()
	seq := m.TakePostFrameImageSequence()
	if !strings.Contains(seq, "\x1b_G") {
		t.Fatalf("Kitty image did not settle; post-frame=%q viewport=%q", seq, m.viewCache.lastViewportView)
	}
	return seq
}

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
	m.tracker.AddImageSegment(path)
	m.streaming = true
	m.bumpContentVersion()

	first := m.viewAltScreen()
	if strings.Contains(first, "\x1b_G") {
		t.Fatalf("alt-screen View content must not embed raw Kitty bytes; got %q", first)
	}
	if upload := strings.Join(m.pendingImageUploads, ""); upload != "" {
		t.Fatalf("post-frame compositor should not queue legacy image uploads; got %q", upload)
	}
	postFrame := m.TakePostFrameImageSequence()
	if !strings.Contains(postFrame, "\x1b_G") {
		t.Fatalf("first alt-screen render should queue Kitty bytes for post-frame composition; got %q", postFrame)
	}

	content := m.viewCache.lastContentStr
	if content == "" && len(m.contentLines) > 0 {
		content = strings.Join(m.contentLines, "\n")
	}
	if content == "" {
		t.Fatal("expected viewport content cache to be populated")
	}
	if strings.Contains(content, "\x1b_G") {
		t.Fatalf("viewport content must not contain raw Kitty APC bytes: %q", content)
	}
	if strings.Contains(m.viewport.View(), "\x1b_G") {
		t.Fatalf("rendered viewport must not contain raw Kitty APC bytes: %q", m.viewport.View())
	}
	if strings.Contains(content, "\U0010eeee") {
		t.Fatalf("backing viewport content should keep Kitty placeholders out of Bubble Tea cache: %q", content)
	}
	if strings.Contains(m.viewCache.lastViewportView, "\U0010eeee") {
		t.Fatalf("post-frame compositor should not inject Kitty placeholder cells into viewport text: %q", m.viewCache.lastViewportView)
	}
	if captions := strings.Count(content, "[Generated image: "+path+"]"); captions != 2 {
		t.Fatalf("viewport content should include one visible caption per image reference, got %d in %q", captions, content)
	}

	second := m.viewAltScreen()
	if strings.Contains(second, "\x1b_G") {
		t.Fatalf("unchanged image view should not contain raw upload bytes: %q", second)
	}
	if strings.Contains(m.viewCache.lastViewportView, "\U0010eeee") {
		t.Fatalf("post-frame compositor should keep viewport text placeholder-free: %q", m.viewCache.lastViewportView)
	}
}

func TestAltScreenImageUploadCmdUsesPostFrameComposition(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	path := writeChatTestPNG(t)
	m := newTestChatModel(true)
	m.width = 40
	m.height = 20

	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)

	artifact := m.renderViewportImageArtifact(path)
	if artifact.Display == "" {
		t.Fatalf("expected image reservation display for %s", path)
	}
	content, blocks := m.extractViewportImageBlocks(artifact.Display)
	m.viewportImageBlocks = blocks
	_ = m.renderAltScreenViewportLines(splitViewportContentLines(content))
	if cmd := m.drainPendingImageUploadCmd(); cmd != nil {
		t.Fatalf("post-frame image composition should not use legacy tea.Raw upload command, got %T", cmd)
	}
	seq := m.TakePostFrameImageSequence()
	if !strings.Contains(seq, "\x1b_G") {
		t.Fatalf("post-frame image sequence should contain Kitty APC bytes, got %q", seq)
	}
}

func TestAltScreenKittyPartialImageCanInjectAndUpload(t *testing.T) {
	m := newTestChatModel(true)
	m.viewportImageArtifacts = map[string]viewportImageArtifact{
		"t": {Key: "t", Upload: "\x1b_Ga=T;data\x1b\\", Rows: []string{"row0\U0010eeee", "row1\U0010eeee"}, WidthCells: 4, HeightCells: 2},
	}
	m.viewportImageBlocks = []viewportImageBlock{{Key: "t", StartLine: 0, WidthCells: 4, HeightCells: 2}}
	visible := []string{"    "}
	m.overlayVisibleViewportImages(visible, 1) // row zero is clipped
	if strings.Contains(strings.Join(visible, "\n"), "\U0010eeee") {
		t.Fatalf("partial Kitty image should not inject placeholders before upload flush: %q", visible)
	}
	if upload := strings.Join(m.pendingImageUploads, ""); !strings.Contains(upload, "a=T") {
		t.Fatalf("partial visible Kitty image should upload, got %q", upload)
	}
	m.uploadedImageKeys[m.viewportImageUploadKey("t")] = struct{}{}
	m.placedImageKeys[m.viewportImageUploadKey("t")+":place"] = struct{}{}
	m.pendingImageUploads = nil
	visible = []string{"    "}
	m.overlayVisibleViewportImages(visible, 1)
	if !strings.Contains(strings.Join(visible, "\n"), "\U0010eeee") {
		t.Fatalf("partial Kitty image should inject visible placeholder rows after upload/place flush: %q", visible)
	}
}

func TestAltScreenKittyUploadQueuedOnlyWhenImageVisible(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	path := writeChatTestPNG(t)
	m := newTestChatModel(true)
	m.width = 40
	m.height = 12
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)

	artifact := m.renderViewportImageArtifact(path)
	content := strings.Repeat("filler\n", 20) + artifact.Display
	content, blocks := m.extractViewportImageBlocks(content)
	m.viewportImageBlocks = blocks
	m.viewport.SetContent(content)
	m.viewport.SetYOffset(0)
	_ = m.renderAltScreenViewportLines(splitViewportContentLines(content))
	if seq := m.TakePostFrameImageSequence(); strings.Contains(seq, "a=T") {
		t.Fatalf("offscreen Kitty image should not render yet, got %q", seq)
	}

	m.viewport.SetYOffset(20)
	_ = m.renderAltScreenViewportLines(splitViewportContentLines(content))
	seq := m.TakePostFrameImageSequence()
	if !strings.Contains(seq, "a=T") {
		t.Fatalf("visible Kitty image should queue post-frame render, got %q", seq)
	}
}

func TestAltScreenImageCleanupCmdDeletesKittyPlacementsAfterKittyUpload(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	m := newTestChatModel(true)
	m.uploadedImageKeys["kitty-image"] = struct{}{}

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

func TestAltScreenImageCleanupCmdSkippedWithoutKittyActivity(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	m := newTestChatModel(true)
	if cmd := m.terminalImageCleanupCmd(); cmd != nil {
		t.Fatalf("cleanup without Kitty image activity should be nil, got %T", cmd)
	}
}

func TestAltScreenImageResizeQueuesCleanupAndReupload(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	path := writeChatTestPNG(t)
	m := newTestChatModel(true)
	m.width = 40
	m.height = 20
	m.tracker = ui.NewToolTracker()
	m.tracker.AddImageSegment(path)
	m.streaming = true
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	m.bumpContentVersion()

	_ = m.viewAltScreen()
	settleVisibleKittyImagesForTest(t, m)

	m.applyWindowSize(tea.WindowSizeMsg{Width: 24, Height: 20})
	_ = m.viewAltScreen()
	content := m.viewCache.lastContentStr
	if content == "" && len(m.contentLines) > 0 {
		content = strings.Join(m.contentLines, "\n")
	}
	if strings.Contains(content, "\U0010eeee") {
		t.Fatalf("resize frame should suppress placeholders until cleanup/reupload is flushed: %q", content)
	}
	upload := strings.Join(m.pendingImageUploads, "")
	if !strings.Contains(upload, "a=d") {
		t.Fatalf("resize should queue Kitty cleanup before reupload, got %q", upload)
	}
	if strings.Contains(upload, "a=T") {
		t.Fatalf("resize cleanup frame must not reupload while old placeholders may still be on screen: %q", upload)
	}
	cmd := m.drainPendingImageUploadCmd()
	if cmd == nil {
		t.Fatal("expected resize cleanup command")
	}
	_ = cmd()
	_ = m.finishImageCleanupFlush()

	m.viewCache.lastSetContentAt = time.Time{}
	_ = m.viewAltScreen()
	seq := m.TakePostFrameImageSequence()
	if !strings.Contains(seq, "a=T") {
		t.Fatalf("post-cleanup frame should queue Kitty post-frame render for new dimensions, got %q", seq)
	}
	content = m.viewCache.lastContentStr
	if content == "" && len(m.contentLines) > 0 {
		content = strings.Join(m.contentLines, "\n")
	}
	if strings.Contains(content, "\U0010eeee") {
		t.Fatalf("post-cleanup backing content should still keep placeholders out of cache: %q", content)
	}
	if strings.Contains(m.viewCache.lastViewportView, "\U0010eeee") {
		t.Fatalf("post-cleanup upload frame should still wait for upload/place flush before placeholders: %q", m.viewCache.lastViewportView)
	}
}

func TestAltScreenResizeKeepsImageBlocksWhileSuppressingPlaceholders(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	path := writeChatTestPNG(t)
	m := newTestChatModel(true)
	m.width = 40
	m.height = 20
	m.tracker = ui.NewToolTracker()
	m.tracker.AddImageSegment(path)
	m.streaming = true
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	m.bumpContentVersion()

	_ = m.viewAltScreen()
	settleVisibleKittyImagesForTest(t, m)

	m.applyWindowSize(tea.WindowSizeMsg{Width: 24, Height: 20})
	_ = m.viewAltScreen()
	if len(m.viewportImageBlocks) == 0 {
		t.Fatal("resize suppression frame should retain image reservation blocks for post-cleanup repaint")
	}
	if strings.Contains(m.viewCache.lastViewportView, "\U0010eeee") {
		t.Fatalf("resize suppression frame should not inject placeholders: %q", m.viewCache.lastViewportView)
	}
	cleanupCmd := m.drainPendingImageUploadCmd()
	if cleanupCmd == nil {
		t.Fatal("expected resize cleanup command")
	}
	_ = cleanupCmd()
	_ = m.finishImageCleanupFlush()
	_ = m.viewAltScreen()
	if strings.Contains(m.viewCache.lastViewportView, "\U0010eeee") {
		t.Fatalf("post-cleanup repaint should wait for upload/place flush: %q", m.viewCache.lastViewportView)
	}
	settleVisibleKittyImagesForTest(t, m)
}

func TestAltScreenImageHeightResizeQueuesCleanupAndReupload(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	path := writeChatTestPNG(t)
	m := newTestChatModel(true)
	m.width = 40
	m.height = 20
	m.tracker = ui.NewToolTracker()
	m.tracker.AddImageSegment(path)
	m.streaming = true
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	m.bumpContentVersion()

	_ = m.viewAltScreen()
	settleVisibleKittyImagesForTest(t, m)

	oldGeneration := m.imageGeneration
	m.applyWindowSize(tea.WindowSizeMsg{Width: 40, Height: 12})
	if m.imageGeneration == oldGeneration {
		t.Fatalf("height resize should bump image generation")
	}
	_ = m.viewAltScreen()
	upload := strings.Join(m.pendingImageUploads, "")
	if !strings.Contains(upload, "a=d") {
		t.Fatalf("height resize should queue Kitty cleanup, got %q", upload)
	}
	if strings.Contains(upload, "a=T") {
		t.Fatalf("height resize cleanup frame must not reupload immediately, got %q", upload)
	}
}

func TestAltScreenNewImageDoesNotCleanupExistingImages(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	pathA := writeChatTestPNG(t)
	pathB := writeChatTestPNG(t)
	m := newTestChatModel(true)
	m.width = 40
	m.height = 20
	m.tracker = ui.NewToolTracker()
	m.tracker.AddImageSegment(pathA)
	m.streaming = true
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	m.bumpContentVersion()

	_ = m.viewAltScreen()
	settleVisibleKittyImagesForTest(t, m)

	m.tracker.AddImageSegment(pathB)
	m.viewCache.lastSetContentAt = time.Time{}
	m.bumpContentVersion()
	_ = m.viewAltScreen()
	seq := m.TakePostFrameImageSequence()
	if strings.Contains(seq, "a=d,d=A") {
		t.Fatalf("adding a later image must not globally cleanup/delete already-visible images: %q", seq)
	}
	if got := strings.Count(seq, "a=T"); got == 0 {
		t.Fatalf("later image should queue post-frame Kitty render, got %d in %q", got, seq)
	}
}

func TestAltScreenImageMaxRowsFitsViewport(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 40
	m.height = 16
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	if got, max := m.imageMaxRows(), m.viewport.Height()-4; got > max {
		t.Fatalf("imageMaxRows() = %d, want <= viewport reserve limit %d", got, max)
	}
	if got := m.imageMaxRows(); got < 1 {
		t.Fatalf("imageMaxRows() = %d, want at least 1", got)
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
