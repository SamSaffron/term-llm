package chat

import (
	"errors"
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

func postFramePayloadForTest(m *Model) string {
	payload, _ := m.postFrameImagePayloadForView()
	return string(payload)
}

func acknowledgePostFrameForTest(t *testing.T, m *Model) {
	t.Helper()
	_, receipt := m.postFrameImagePayloadForView()
	if receipt == nil {
		t.Fatal("expected post-frame receipt")
	}
	if cmd := m.handlePostFrameImageResult(receipt, nil); cmd != nil {
		t.Fatal("successful post-frame acknowledgement returned a command")
	}
}

func TestAltScreenKittyImagePayloadIsAttachedToComposedView(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	path := writeChatTestPNG(t)
	m := newTestChatModel(true)
	m.width = 40
	m.height = 20
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	m.tracker = ui.NewToolTracker()
	m.tracker.AddImageSegment(path)
	m.streaming = true
	m.bumpContentVersion()

	view := m.View()
	if !strings.Contains(string(view.PostFrame), "a=t") || !strings.Contains(string(view.PostFrame), "a=p") {
		t.Fatalf("View.PostFrame should contain the exact Kitty upload and placement, got %q", view.PostFrame)
	}
	if strings.Contains(view.Content, "\x1b_G") {
		t.Fatalf("View content must not contain Kitty APC bytes: %q", view.Content)
	}
}

func TestPostFrameImagePayloadSuppressedForExternalProcess(t *testing.T) {
	m := newTestChatModel(true)
	m.postFrameImageSeq = "\x1b_Gpending-image\x1b\\"

	m.setShellTerminalHandoff(true)
	view := m.View()
	if len(view.PostFrame) != 0 {
		t.Fatalf("shell handoff View.PostFrame = %q, want empty", view.PostFrame)
	}
	if view.AltScreen {
		t.Fatal("shell handoff View kept alternate screen enabled")
	}

	m.setShellTerminalHandoff(false)
	m.postFrameImagePrefixSeq = "after-shell"
	view = m.View()
	if !strings.Contains(string(view.PostFrame), "after-shell") {
		t.Fatalf("restored View.PostFrame = %q, want recomposed payload", view.PostFrame)
	}
}

func TestReplacementPostFrameUploadIsSelfContained(t *testing.T) {
	m := newTestChatModel(true)
	image := postFrameImageState{
		ImageID:     42,
		PlacementID: 7,
		WidthCells:  2,
		HeightCells: 1,
		ScreenRow:   3,
		Upload:      "kitty-upload",
	}
	compose := func() string {
		m.beginPostFrameImageComposition()
		m.postFrameImageMu.Lock()
		m.postFrameCurrentImages["image"] = image
		m.postFrameImageMu.Unlock()
		m.finishPostFrameImageComposition()
		return postFramePayloadForTest(m)
	}

	first := compose()
	second := compose()
	if !strings.Contains(first, image.Upload) || !strings.Contains(second, image.Upload) {
		t.Fatalf("replacement-capable payloads must each contain the upload; first=%q second=%q", first, second)
	}
	if !strings.Contains(first, "a=p") || !strings.Contains(second, "a=p") {
		t.Fatalf("replacement-capable payloads must each contain placement; first=%q second=%q", first, second)
	}
}

func TestDroppedPostFrameAcknowledgementRetransmitsBoundedTransitionThenConverges(t *testing.T) {
	m := newTestChatModel(true)
	image := postFrameImageState{
		ImageID:     42,
		PlacementID: 7,
		WidthCells:  2,
		HeightCells: 1,
		ScreenRow:   3,
		Upload:      "kitty-upload",
	}
	compose := func() (string, *postFrameImageReceipt) {
		m.beginPostFrameImageComposition()
		m.postFrameImageMu.Lock()
		m.postFrameCurrentImages["image"] = image
		m.postFrameImageMu.Unlock()
		m.finishPostFrameImageComposition()
		payload, receipt := m.postFrameImagePayloadForView()
		return payload, receipt
	}

	first, firstReceipt := compose()
	if firstReceipt == nil {
		t.Fatal("first image transition had no receipt")
	}
	// Model Bubble Tea's bounded acknowledgement queue being full: the newest
	// receipt is dropped and therefore never reaches handlePostFrameImageResult.
	resultQueue := make(chan *postFrameImageReceipt, 1)
	resultQueue <- &postFrameImageReceipt{}
	select {
	case resultQueue <- firstReceipt:
		t.Fatal("full acknowledgement queue unexpectedly accepted receipt")
	default:
	}

	second, acceptedReceipt := compose()
	if second != first {
		t.Fatalf("unacknowledged transition changed or accumulated across retransmit\nfirst:  %q\nsecond: %q", first, second)
	}
	if strings.Count(second, image.Upload) != 1 || strings.Count(second, "a=p") != 1 {
		t.Fatalf("retransmit bandwidth grew beyond one complete transition: %q", second)
	}
	if acceptedReceipt == nil {
		t.Fatal("retransmitted transition had no receipt")
	}
	if cmd := m.handlePostFrameImageResult(acceptedReceipt, nil); cmd != nil {
		t.Fatal("accepted acknowledgement returned a command")
	}

	third, receipt := compose()
	if third != "" || receipt != nil {
		t.Fatalf("acknowledged image transition did not converge to empty payload: payload=%q receipt=%#v", third, receipt)
	}
}

func TestLegacyPostFrameUploadPreservesCursor(t *testing.T) {
	m := newTestChatModel(true)
	m.pendingImageUploads = []string{"legacy-iterm-or-sixel-upload"}
	m.beginPostFrameImageComposition()
	m.finishPostFrameImageComposition()
	if got, want := postFramePayloadForTest(m), "\x1b[slegacy-iterm-or-sixel-upload\x1b[u"; got != want {
		t.Fatalf("legacy post-frame payload = %q, want cursor-preserving %q", got, want)
	}
}

func TestChatViewportImagePlacementHasAbsoluteTerminalOrigin(t *testing.T) {
	m := newTestChatModel(true)
	if got := m.viewport.Style.GetVerticalFrameSize(); got != 0 {
		t.Fatalf("chat viewport vertical frame size = %d, want zero for absolute image rows", got)
	}
	image := postFrameImageState{ImageID: 42, PlacementID: 7, WidthCells: 2, HeightCells: 1, ScreenRow: 3, Upload: "kitty-upload"}
	m.beginPostFrameImageComposition()
	m.postFrameCurrentImages["image"] = image
	m.finishPostFrameImageComposition()
	if payload := postFramePayloadForTest(m); !strings.Contains(payload, "\x1b[4;1H") {
		t.Fatalf("zero-based viewport screen row was not placed at absolute terminal row 4: %q", payload)
	}
}

func settleVisibleKittyImagesForTest(t *testing.T, m *Model) string {
	t.Helper()
	m.viewCache.lastSetContentAt = time.Time{}
	seq := string(m.View().PostFrame)
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

	firstView := m.View()
	first := firstView.Content
	if strings.Contains(first, "\x1b_G") {
		t.Fatalf("alt-screen View content must not embed raw Kitty bytes; got %q", first)
	}
	if upload := strings.Join(m.pendingImageUploads, ""); upload != "" {
		t.Fatalf("post-frame compositor should not queue legacy image uploads; got %q", upload)
	}
	postFrame := string(firstView.PostFrame)
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

	secondView := m.View()
	if strings.Contains(secondView.Content, "\x1b_G") {
		t.Fatalf("unchanged image view should not contain raw upload bytes: %q", secondView.Content)
	}
	if !strings.Contains(string(secondView.PostFrame), "a=p") {
		t.Fatalf("unchanged image view should carry a stable placement: %q", secondView.PostFrame)
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
	m.beginPostFrameImageComposition()
	_ = m.renderAltScreenViewportLines(splitViewportContentLines(content))
	m.finishPostFrameImageComposition()
	seq := postFramePayloadForTest(m)
	if !strings.Contains(seq, "\x1b_G") {
		t.Fatalf("post-frame image sequence should contain Kitty APC bytes, got %q", seq)
	}
}

func TestAltScreenKittyPartialImageCanInjectAndUpload(t *testing.T) {
	m := newTestChatModel(true)
	m.viewportImageArtifacts = map[string]viewportImageArtifact{
		"t": {Key: "t", Upload: "\x1b_Ga=t;data\x1b\\", Rows: []string{"row0\U0010eeee", "row1\U0010eeee"}, WidthCells: 4, HeightCells: 2},
	}
	m.viewportImageBlocks = []viewportImageBlock{{Key: "t", StartLine: 0, WidthCells: 4, HeightCells: 2}}
	visible := []string{"    "}
	m.overlayVisibleViewportImages(visible, 1) // row zero is clipped
	if !strings.Contains(strings.Join(visible, "\n"), "\U0010eeee") {
		t.Fatalf("partial Kitty image should inject placeholders in its renderer frame: %q", visible)
	}
	if upload := strings.Join(m.pendingImageUploads, ""); !strings.Contains(upload, "a=t") {
		t.Fatalf("partial visible Kitty image should attach its upload to PostFrame, got %q", upload)
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
	m.beginPostFrameImageComposition()
	_ = m.renderAltScreenViewportLines(splitViewportContentLines(content))
	m.finishPostFrameImageComposition()
	if seq := postFramePayloadForTest(m); strings.Contains(seq, "a=t") || strings.Contains(seq, "a=p") {
		t.Fatalf("offscreen Kitty image should not render yet, got %q", seq)
	}

	m.viewport.SetYOffset(20)
	m.beginPostFrameImageComposition()
	_ = m.renderAltScreenViewportLines(splitViewportContentLines(content))
	m.finishPostFrameImageComposition()
	seq := postFramePayloadForTest(m)
	if !strings.Contains(seq, "a=t") || !strings.Contains(seq, "a=p") {
		t.Fatalf("visible Kitty image should queue post-frame upload and placement, got %q", seq)
	}
}

func TestAltScreenPostFrameKeepsStaleDeletesAcrossReplacement(t *testing.T) {
	m := newTestChatModel(true)
	m.postFrameKnownImages = map[string]postFrameImageState{
		"old": {ImageID: 42, PlacementID: 77, WidthCells: 4, HeightCells: 2},
	}

	m.beginPostFrameImageComposition()
	m.finishPostFrameImageComposition()
	// A second View can replace the first before a renderer flush. Conservative
	// known-placement state keeps the delete in every replacement payload.
	m.beginPostFrameImageComposition()
	m.finishPostFrameImageComposition()

	seq := postFramePayloadForTest(m)
	if !strings.Contains(seq, "a=d,d=i,i=42,p=77") {
		t.Fatalf("stale placement delete should survive pre-flush rerenders, got %q", seq)
	}
	acknowledgePostFrameForTest(t, m)
	m.beginPostFrameImageComposition()
	m.finishPostFrameImageComposition()
	if seq := postFramePayloadForTest(m); seq != "" {
		t.Fatalf("acknowledged stale placement delete repeated: %q", seq)
	}
}

func TestAltScreenPostFrameRendersMultipleVisibleKittyImages(t *testing.T) {
	t.Setenv("TERM_LLM_IMAGE_PROTOCOL", "kitty")
	termimage.ClearCache()

	pathA := writeChatTestPNG(t)
	m := newTestChatModel(true)
	m.width = 80
	m.height = 30
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)

	artifactA := m.renderViewportImageArtifact(pathA)
	artifactB := m.renderViewportImageArtifact(pathA)
	content := strings.Join([]string{
		"before",
		artifactA.Display,
		"between",
		artifactB.Display,
		"after",
	}, "\n")
	content, blocks := m.extractViewportImageBlocks(content)
	m.viewportImageBlocks = blocks
	lines := splitViewportContentLines(content)

	m.beginPostFrameImageComposition()
	_ = m.renderAltScreenViewportLines(lines)
	m.finishPostFrameImageComposition()
	seq := postFramePayloadForTest(m)
	if got := strings.Count(seq, "a=t"); got != 1 {
		t.Fatalf("first post-frame render should transmit shared image data once, got %d in %q", got, seq)
	}
	if got := strings.Count(seq, "a=p"); got != 2 {
		t.Fatalf("first post-frame render should place both visible images, got %d in %q", got, seq)
	}
	if !strings.Contains(seq, "\x1b[s") || !strings.Contains(seq, "\x1b[u") {
		t.Fatalf("post-frame sequence should preserve Bubble Tea cursor/status position, got %q", seq)
	}

	acknowledgePostFrameForTest(t, m)
	m.beginPostFrameImageComposition()
	_ = m.renderAltScreenViewportLines(lines)
	m.finishPostFrameImageComposition()
	seq = postFramePayloadForTest(m)
	if seq != "" {
		t.Fatalf("second unchanged frame retransmitted acknowledged image state: %q", seq)
	}
}

func TestImageCleanupSequenceCachesUntilOwnedIDsChange(t *testing.T) {
	m := newTestChatModel(true)
	m.addOwnedKittyImageID(42)
	first := m.imageCleanupSequence()
	if !m.imageCleanupSeqValid || !strings.Contains(first, "a=d,i=42") {
		t.Fatalf("initial cached cleanup = %q valid=%t", first, m.imageCleanupSeqValid)
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = m.imageCleanupSequence() }); allocs != 0 {
		t.Fatalf("cached cleanup allocations = %v, want 0", allocs)
	}
	m.addOwnedKittyImageID(42)
	if !m.imageCleanupSeqValid {
		t.Fatal("duplicate owned ID invalidated cleanup cache")
	}
	m.addOwnedKittyImageID(7)
	if m.imageCleanupSeqValid {
		t.Fatal("new owned ID did not invalidate cleanup cache")
	}
	second := m.imageCleanupSequence()
	if strings.Index(second, "i=7") > strings.Index(second, "i=42") {
		t.Fatalf("cleanup IDs are not deterministic: %q", second)
	}
}

func BenchmarkImageCleanupSequenceCached(b *testing.B) {
	m := newTestChatModel(true)
	for i := 1; i <= 4096; i++ {
		m.addOwnedKittyImageID(uint32(i))
	}
	_ = m.imageCleanupSequence()
	b.ReportAllocs()
	for b.Loop() {
		_ = m.imageCleanupSequence()
	}
}

func TestAltScreenQuitViewCarriesKittyCleanup(t *testing.T) {
	m := newTestChatModel(true)
	m.ownedKittyImageIDs[42] = struct{}{}
	m.quitting = true

	view := m.View()
	cleanup := view.TerminalCleanup
	if !strings.Contains(cleanup, "a=d,i=42") {
		t.Fatalf("quitting View should delete owned Kitty image, got %q", cleanup)
	}
	if strings.Contains(cleanup, "a=t") || strings.Contains(cleanup, "a=p") {
		t.Fatalf("quitting View must not compose uploads or placements, got %q", cleanup)
	}
}

func TestAltScreenQuitViewSkipsCleanupWithoutImageActivity(t *testing.T) {
	m := newTestChatModel(true)
	m.quitting = true
	if payload := m.View().TerminalCleanup; len(payload) != 0 {
		t.Fatalf("cleanup without image activity = %q, want empty", payload)
	}
}

func TestSuspendInvalidatesKittyAcknowledgements(t *testing.T) {
	m := newTestChatModel(true)
	m.ownedKittyImageIDs[42] = struct{}{}
	m.postFrameKnownImages["old"] = postFrameImageState{ImageID: 42, PlacementID: 7}
	generation := m.imageGeneration

	updated, _ := m.Update(tea.SuspendMsg{})
	m = updated.(*Model)
	if m.imageGeneration != generation+1 {
		t.Fatalf("image generation = %d, want %d", m.imageGeneration, generation+1)
	}
	if len(m.postFrameKnownImages) != 0 || len(m.postFrameUploadedImages) != 0 {
		t.Fatalf("SuspendMsg retained acknowledged Kitty state: known=%v uploaded=%v", m.postFrameKnownImages, m.postFrameUploadedImages)
	}
	if !strings.Contains(m.postFrameImagePrefixSeq, "a=d,i=42") {
		t.Fatalf("SuspendMsg did not retain cleanup transition for resumed frame: %q", m.postFrameImagePrefixSeq)
	}
}

func TestAltScreenResetCarriesCleanupAcrossReplacementViews(t *testing.T) {
	m := newTestChatModel(true)
	m.ownedKittyImageIDs[42] = struct{}{}
	m.postFrameKnownImages["old"] = postFrameImageState{ImageID: 42, PlacementID: 7}

	m.resetImageUploadState()
	first := string(m.View().PostFrame)
	second := string(m.View().PostFrame)
	for i, payload := range []string{first, second} {
		if !strings.Contains(payload, "a=d,i=42") {
			t.Fatalf("replacement View %d lost reset cleanup: %q", i+1, payload)
		}
		if strings.Contains(payload, "a=t") || strings.Contains(payload, "a=p") {
			t.Fatalf("replacement View %d recreated reset image: %q", i+1, payload)
		}
	}
}

func TestAltScreenImageResizeComposesCleanupAndReuploadOnSameView(t *testing.T) {
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

	settleVisibleKittyImagesForTest(t, m)
	m.applyWindowSize(tea.WindowSizeMsg{Width: 24, Height: 20})
	resized := m.View()
	payload := string(resized.PostFrame)
	cleanup := strings.Index(payload, "a=d")
	upload := strings.Index(payload, "a=t")
	placement := strings.Index(payload, "a=p")
	if cleanup < 0 || upload < 0 || placement < 0 || !(cleanup < upload && upload < placement) {
		t.Fatalf("resize payload order cleanup=%d upload=%d placement=%d in %q", cleanup, upload, placement, payload)
	}
	if strings.Contains(resized.Content, "\x1b_G") || strings.Contains(m.viewCache.lastViewportView, "\U0010eeee") {
		t.Fatalf("resize renderer frame contains image protocol bytes/placeholders: %q", resized.Content)
	}
}

func TestAltScreenResizeKeepsImageBlocksForSameFrameRecomposition(t *testing.T) {
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

	settleVisibleKittyImagesForTest(t, m)
	m.applyWindowSize(tea.WindowSizeMsg{Width: 24, Height: 20})
	view := m.View()
	if len(m.viewportImageBlocks) == 0 {
		t.Fatal("resize frame should retain image reservation blocks")
	}
	if !strings.Contains(string(view.PostFrame), "a=t") || !strings.Contains(string(view.PostFrame), "a=p") {
		t.Fatalf("resize frame did not carry fresh upload/placement: %q", view.PostFrame)
	}
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

	settleVisibleKittyImagesForTest(t, m)

	oldGeneration := m.imageGeneration
	m.applyWindowSize(tea.WindowSizeMsg{Width: 40, Height: 12})
	if m.imageGeneration == oldGeneration {
		t.Fatalf("height resize should bump image generation")
	}
	view := m.View()
	payload := string(view.PostFrame)
	if !strings.Contains(payload, "a=d") || !strings.Contains(payload, "a=t") || !strings.Contains(payload, "a=p") {
		t.Fatalf("height resize should compose cleanup, upload, and placement together, got %q", payload)
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

	settleVisibleKittyImagesForTest(t, m)

	m.tracker.AddImageSegment(pathB)
	m.viewCache.lastSetContentAt = time.Time{}
	m.bumpContentVersion()
	seq := string(m.View().PostFrame)
	if strings.Contains(seq, "a=d,d=A") {
		t.Fatalf("adding a later image must not globally cleanup/delete already-visible images: %q", seq)
	}
	if got := strings.Count(seq, "a=t"); got == 0 {
		t.Fatalf("later image should queue post-frame Kitty upload, got %d in %q", got, seq)
	}
}

func TestQueuePostFrameViewportImageRejectsRowsBelowScreen(t *testing.T) {
	m := newTestChatModel(true)
	m.postFrameRenderCache = map[string]postFrameImageState{
		"image:direct-render:0:1": {ImageID: 42, WidthCells: 2, HeightCells: 1, Upload: "upload"},
	}
	m.beginPostFrameImageComposition()

	m.queuePostFrameViewportImage(viewportImageArtifact{Key: "image", Path: "cached.png"}, 0, 0, 1, m.height)

	if got := len(m.postFrameCurrentImages); got != 0 {
		t.Fatalf("queued %d image placements below the terminal, want 0", got)
	}
}

func TestPostFrameAcknowledgementIgnoresStaleGeneration(t *testing.T) {
	m := newTestChatModel(true)
	image := postFrameImageState{ImageID: 42, PlacementID: 7, WidthCells: 2, HeightCells: 1, ScreenRow: 3, Upload: "kitty-upload"}
	m.beginPostFrameImageComposition()
	m.postFrameCurrentImages["image"] = image
	m.ownedKittyImageIDs[image.ImageID] = struct{}{}
	m.finishPostFrameImageComposition()
	_, stale := m.postFrameImagePayloadForView()
	if stale == nil {
		t.Fatal("expected receipt for initial image transition")
	}

	m.resetImageUploadState()
	prefix := m.postFrameImagePrefixSeq
	if prefix == "" {
		t.Fatal("reset did not retain cleanup prefix")
	}
	if cmd := m.handlePostFrameImageResult(stale, nil); cmd != nil {
		t.Fatal("stale acknowledgement returned a command")
	}
	if m.postFrameImagePrefixSeq != prefix {
		t.Fatalf("stale acknowledgement cleared current cleanup prefix: got %q want %q", m.postFrameImagePrefixSeq, prefix)
	}
	if _, ok := m.postFrameUploadedImages[image.ImageID]; ok {
		t.Fatal("stale acknowledgement committed an upload into the new generation")
	}
}

func TestPostFrameFailurePausesRetriesUntilNextGeneration(t *testing.T) {
	m := newTestChatModel(true)
	image := postFrameImageState{ImageID: 42, PlacementID: 7, WidthCells: 2, HeightCells: 1, ScreenRow: 3, Upload: "kitty-upload"}
	compose := func() string {
		m.beginPostFrameImageComposition()
		m.postFrameCurrentImages["image"] = image
		m.finishPostFrameImageComposition()
		return postFramePayloadForTest(m)
	}
	first := compose()
	_, receipt := m.postFrameImagePayloadForView()
	failure := errors.New("short write")
	if cmd := m.handlePostFrameImageResult(receipt, failure); cmd == nil {
		t.Fatal("post-frame failure did not schedule one surfaced footer error")
	}
	if cmd := m.handlePostFrameImageResult(receipt, failure); cmd != nil {
		t.Fatal("repeated post-frame failure scheduled a footer storm")
	}
	if payload, _ := m.postFrameImagePayloadForView(); payload != "" {
		t.Fatalf("disabled generation retained frame-rate payload retry: %q", payload)
	}
	if m.postFrameImageCompositionEnabled() {
		t.Fatal("failed generation remained enabled")
	}
	if !strings.Contains(first, image.Upload) || !strings.Contains(first, "a=p") {
		t.Fatalf("initial transition incomplete: %q", first)
	}

	m.resetImageUploadState()
	if !m.postFrameImageCompositionEnabled() {
		t.Fatal("next generation did not deliberately re-enable image composition")
	}
	second := compose()
	if !strings.Contains(second, image.Upload) || !strings.Contains(second, "a=p") {
		t.Fatalf("next-generation recovery was not a complete transition: %q", second)
	}
}

func TestPostFrameChangedImageDeletesOldPlacementBeforeReplacement(t *testing.T) {
	m := newTestChatModel(true)
	old := postFrameImageState{ImageID: 41, PlacementID: 7, WidthCells: 2, HeightCells: 1, ScreenRow: 3}
	current := postFrameImageState{ImageID: 42, PlacementID: 7, WidthCells: 2, HeightCells: 1, ScreenRow: 3, Upload: "new-upload"}
	m.postFrameKnownImages["image"] = old
	m.postFrameUploadedImages[old.ImageID] = struct{}{}
	m.beginPostFrameImageComposition()
	m.postFrameCurrentImages["image"] = current
	m.finishPostFrameImageComposition()
	payload := postFramePayloadForTest(m)
	deleteAt := strings.Index(payload, "a=d,d=i,i=41,p=7")
	uploadAt := strings.Index(payload, current.Upload)
	placeAt := strings.Index(payload, "a=p,i=42,p=7")
	if deleteAt < 0 || uploadAt < 0 || placeAt < 0 || !(uploadAt < deleteAt && deleteAt < placeAt) {
		t.Fatalf("replacement transition upload=%d delete=%d place=%d in %q", uploadAt, deleteAt, placeAt, payload)
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
