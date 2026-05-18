package chat

import (
	"fmt"
	"hash/fnv"
	"image/color"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/termimage"
	"github.com/samsaffron/term-llm/internal/ui"
)

const chatImageMaxRows = 30
const viewportImageMarkerPrefix = "@@TERM_LLM_IMAGE:"

type viewportImageArtifact struct {
	Key         string
	Path        string
	Upload      string
	Place       string
	Rows        []string
	WidthCells  int
	HeightCells int
	ImageID     uint32
}

type viewportImageBlock struct {
	Key         string
	StartLine   int
	WidthCells  int
	HeightCells int
}

type postFrameImageState struct {
	ImageID     uint32
	PlacementID uint32
	WidthCells  int
	HeightCells int
}

func (m *Model) configureImageRenderer() {
	if m == nil || m.chatRenderer == nil {
		return
	}
	if m.altScreen {
		m.chatRenderer.SetImageRenderer(m.renderViewportImageArtifact)
		return
	}
	m.chatRenderer.SetImageRenderer(nil)
}

func (m *Model) imageArtifactRenderer() ui.ImageArtifactRenderer {
	if m == nil || !m.altScreen {
		return nil
	}
	return m.renderViewportImageArtifact
}

func (m *Model) renderViewportImageArtifact(path string) ui.ImageArtifact {
	path = strings.TrimSpace(path)
	artifact := ui.ImageArtifact{Caption: ui.ImageArtifactCaption(path)}
	if path == "" {
		return artifact
	}

	stableKey := m.viewportImageStableKey(path)
	if existing, ok := m.viewportImageArtifacts[viewportImageToken(stableKey)]; ok {
		artifact.Display = viewportImageMarkerGrid(existing.Key, existing.WidthCells, existing.HeightCells)
		artifact.CacheKey = stableKey
		artifact.Height = existing.HeightCells
		return artifact
	}

	result, err := termimage.Render(termimage.Request{
		Path:               path,
		MaxCols:            m.imageMaxCols(),
		MaxRows:            m.imageMaxRows(),
		Mode:               termimage.ModeViewport,
		Protocol:           termimage.ProtocolAuto,
		Background:         m.imageBackground(),
		AllowEscapeUploads: true,
	})
	if err != nil {
		artifact.Warnings = append(artifact.Warnings, err.Error())
		return artifact
	}

	termimage.Debugf(termimage.DefaultEnvironment(), "chat render image path=%s protocol=%s cells=%dx%d viewport=%dx%d model=%dx%d upload=%d display=%d suppressed=%t", path, result.Protocol, result.WidthCells, result.HeightCells, m.viewport.Width(), m.viewport.Height(), m.width, m.height, len(result.Upload), len(result.Display), m.imagePlaceholdersSuppressed)

	artifact.Display = result.Display
	artifact.Upload = result.Upload
	artifact.CacheKey = result.CacheKey
	artifact.Height = result.HeightCells
	artifact.Warnings = append(artifact.Warnings, result.Warnings...)

	if result.Protocol == termimage.ProtocolKitty && result.Display != "" {
		key := stableKey
		token := viewportImageToken(key)
		if m.viewportImageArtifacts == nil {
			m.viewportImageArtifacts = make(map[string]viewportImageArtifact)
		}
		m.viewportImageArtifacts[token] = viewportImageArtifact{
			Key:         token,
			Path:        path,
			Upload:      result.Upload,
			Place:       result.Place,
			Rows:        strings.Split(result.Display, "\n"),
			WidthCells:  result.WidthCells,
			HeightCells: result.HeightCells,
			ImageID:     result.ImageID,
		}
		if result.ImageID != 0 {
			if m.ownedKittyImageIDs == nil {
				m.ownedKittyImageIDs = make(map[uint32]struct{})
			}
			m.ownedKittyImageIDs[result.ImageID] = struct{}{}
		}
		artifact.Display = viewportImageMarkerGrid(token, result.WidthCells, result.HeightCells)
		artifact.Upload = ""
		return artifact
	}

	if artifact.Upload != "" {
		if m.imagePlaceholdersSuppressed {
			// During terminal-width changes, first erase stale placeholder text / real
			// Kitty placements without creating a new virtual placement. If cleanup and
			// reupload are emitted in the same raw chunk, the newly uploaded virtual image
			// can briefly bind to the old on-screen placeholder grid from the previous
			// layout, producing doubled/misplaced images after resize. The cleanup flush
			// clears suppression and invalidates the viewport; the next render queues the
			// fresh upload and draws the new placeholder grid.
			termimage.Debugf(termimage.DefaultEnvironment(), "chat suppress Kitty placeholders during resize cleanup path=%s", path)
			artifact.Display = ""
			artifact.Height = 0
			return artifact
		}
		key := artifact.CacheKey
		if key == "" {
			key = fmt.Sprintf("%s|%s|%dx%d", path, result.Protocol, result.WidthCells, result.HeightCells)
		}
		m.queueImageUpload(key, artifact.Upload)
	}

	return artifact
}

func (m *Model) viewportImageStableKey(path string) string {
	if m == nil {
		return path
	}
	meta := ""
	if stat, err := os.Stat(path); err == nil {
		meta = fmt.Sprintf("|mtime:%d|size:%d", stat.ModTime().UnixNano(), stat.Size())
	}
	return fmt.Sprintf("gen:%d|%s%s|%dx%d", m.imageGeneration, path, meta, m.imageMaxCols(), m.imageMaxRows())
}

func viewportImageToken(key string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return strconv.FormatUint(h.Sum64(), 16)
}

func viewportImageMarkerGrid(token string, width, height int) string {
	if height < 1 {
		height = 1
	}
	var b strings.Builder
	for row := 0; row < height; row++ {
		if row > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s%s:%d@@", viewportImageMarkerPrefix, token, row)
	}
	return b.String()
}

func (m *Model) usePostFrameImageComposition() bool {
	return m != nil && m.altScreen
}

func (m *Model) beginPostFrameImageComposition() {
	if m == nil || !m.altScreen {
		return
	}
	m.postFrameImageMu.Lock()
	defer m.postFrameImageMu.Unlock()
	if m.postFrameTransmittedImages == nil {
		m.postFrameTransmittedImages = make(map[uint32]struct{})
	}
	m.postFrameCurrentImages = make(map[string]postFrameImageState)
	m.postFrameQueuedImages = make(map[uint32]struct{})
	m.postFrameImageSeq = ""
	m.postFrameImageUploadSeq = ""
	m.postFrameImagePlaceSeq = ""
}

func mapKeys[K comparable, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func (m *Model) queuePostFrameViewportImage(art viewportImageArtifact, blockStartLine, startRow, rows, screenRow int) {
	if m == nil || art.Path == "" || rows <= 0 || screenRow < 0 {
		return
	}
	key := fmt.Sprintf("%s:direct:%d:%d:%d", art.Key, blockStartLine, startRow, rows)
	placementID := postFramePlacementID(key)
	result, err := termimage.Render(termimage.Request{
		Path:               art.Path,
		MaxCols:            m.imageMaxCols(),
		MaxRows:            m.imageMaxRows(),
		Mode:               termimage.ModeOneShot,
		Protocol:           termimage.ProtocolKitty,
		Background:         m.imageBackground(),
		AllowEscapeUploads: true,
		SliceStartRow:      startRow,
		SliceRows:          rows,
	})
	if err != nil || result.ImageID == 0 {
		if err != nil {
			termimage.Debugf(termimage.DefaultEnvironment(), "chat post-frame image render failed path=%s start=%d rows=%d err=%v", art.Path, startRow, rows, err)
		}
		return
	}

	place := termimage.KittyDirectPlaceSequence(result.ImageID, placementID, result.WidthCells, result.HeightCells)
	m.postFrameImageMu.Lock()
	defer m.postFrameImageMu.Unlock()
	if m.postFrameCurrentImages == nil {
		m.postFrameCurrentImages = make(map[string]postFrameImageState)
	}
	if m.postFrameTransmittedImages == nil {
		m.postFrameTransmittedImages = make(map[uint32]struct{})
	}
	m.postFrameCurrentImages[key] = postFrameImageState{ImageID: result.ImageID, PlacementID: placementID, WidthCells: result.WidthCells, HeightCells: result.HeightCells}
	if _, transmitted := m.postFrameTransmittedImages[result.ImageID]; !transmitted {
		if _, queued := m.postFrameQueuedImages[result.ImageID]; !queued {
			if result.Upload != "" {
				m.postFrameImageUploadSeq += result.Upload
				m.postFrameQueuedImages[result.ImageID] = struct{}{}
			} else {
				m.postFrameImageUploadSeq += result.Full
				m.postFrameQueuedImages[result.ImageID] = struct{}{}
				if result.ImageID != 0 {
					if m.ownedKittyImageIDs == nil {
						m.ownedKittyImageIDs = make(map[uint32]struct{})
					}
					m.ownedKittyImageIDs[result.ImageID] = struct{}{}
				}
				return
			}
		}
	}
	m.postFrameImagePlaceSeq += fmt.Sprintf("\x1b[%d;1H%s", screenRow+1, place)
	if result.ImageID != 0 {
		if m.ownedKittyImageIDs == nil {
			m.ownedKittyImageIDs = make(map[uint32]struct{})
		}
		m.ownedKittyImageIDs[result.ImageID] = struct{}{}
	}
}

func postFramePlacementID(key string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	id := h.Sum32()
	if id == 0 {
		return 1
	}
	return id
}

func (m *Model) finishPostFrameImageComposition() {
	if m == nil || !m.altScreen {
		return
	}
	m.postFrameImageMu.Lock()
	defer m.postFrameImageMu.Unlock()
	for key, previous := range m.postFrameVisibleImages {
		if _, stillVisible := m.postFrameCurrentImages[key]; !stillVisible && previous.ImageID != 0 {
			m.postFrameImagePlaceSeq += termimage.KittyDeletePlacementSequence(previous.ImageID, previous.PlacementID)
		}
	}
	m.postFrameVisibleImages = m.postFrameCurrentImages
	m.postFrameCurrentImages = nil
	if m.postFrameImageUploadSeq != "" || m.postFrameImagePlaceSeq != "" {
		m.postFrameImageSeq = m.postFrameImageUploadSeq + "\x1b[s" + m.postFrameImagePlaceSeq + "\x1b[u"
	} else {
		m.postFrameImageSeq = ""
	}
	m.postFrameImageUploadSeq = ""
	m.postFrameImagePlaceSeq = ""
}

func (m *Model) TakePostFrameImageSequence() string {
	if m == nil {
		return ""
	}
	m.postFrameImageMu.Lock()
	defer m.postFrameImageMu.Unlock()
	seq := m.postFrameImageSeq
	if seq != "" {
		if m.postFrameTransmittedImages == nil {
			m.postFrameTransmittedImages = make(map[uint32]struct{})
		}
		for _, image := range m.postFrameVisibleImages {
			if image.ImageID != 0 {
				m.postFrameTransmittedImages[image.ImageID] = struct{}{}
			}
		}
	}
	m.postFrameImageSeq = ""
	return seq
}

func (m *Model) renderViewportImageSliceArtifact(art viewportImageArtifact, startRow, rows int) (viewportImageArtifact, bool) {
	if m == nil || art.Path == "" || rows <= 0 {
		return viewportImageArtifact{}, false
	}
	if startRow < 0 {
		startRow = 0
	}
	key := fmt.Sprintf("%s:slice:%d:%d", art.Key, startRow, rows)
	if existing, ok := m.viewportImageArtifacts[key]; ok {
		return existing, true
	}
	result, err := termimage.Render(termimage.Request{
		Path:               art.Path,
		MaxCols:            m.imageMaxCols(),
		MaxRows:            m.imageMaxRows(),
		Mode:               termimage.ModeViewport,
		Protocol:           termimage.ProtocolAuto,
		Background:         m.imageBackground(),
		AllowEscapeUploads: true,
		SliceStartRow:      startRow,
		SliceRows:          rows,
	})
	if err != nil || result.Protocol != termimage.ProtocolKitty || result.Display == "" {
		if err != nil {
			termimage.Debugf(termimage.DefaultEnvironment(), "chat render image slice failed path=%s start=%d rows=%d err=%v", art.Path, startRow, rows, err)
		}
		return viewportImageArtifact{}, false
	}
	slice := viewportImageArtifact{
		Key:         key,
		Path:        art.Path,
		Upload:      result.Upload,
		Place:       result.Place,
		Rows:        strings.Split(result.Display, "\n"),
		WidthCells:  result.WidthCells,
		HeightCells: result.HeightCells,
		ImageID:     result.ImageID,
	}
	if m.viewportImageArtifacts == nil {
		m.viewportImageArtifacts = make(map[string]viewportImageArtifact)
	}
	m.viewportImageArtifacts[key] = slice
	if result.ImageID != 0 {
		if m.ownedKittyImageIDs == nil {
			m.ownedKittyImageIDs = make(map[uint32]struct{})
		}
		m.ownedKittyImageIDs[result.ImageID] = struct{}{}
	}
	termimage.Debugf(termimage.DefaultEnvironment(), "chat render image slice path=%s start=%d rows=%d cells=%dx%d upload=%d display=%d", art.Path, startRow, rows, result.WidthCells, result.HeightCells, len(result.Upload), len(result.Display))
	return slice, true
}

func (m *Model) viewportImageUploadKey(token string) string {
	if m == nil {
		return token
	}
	return fmt.Sprintf("gen:%d:%s", m.imageGeneration, token)
}

func (m *Model) extractViewportImageBlocks(content string) (string, []viewportImageBlock) {
	if content == "" || !strings.Contains(content, viewportImageMarkerPrefix) {
		m.viewportImageBlocks = nil
		return content, nil
	}
	lines := strings.Split(content, "\n")
	blocks := make([]viewportImageBlock, 0)
	active := make(map[string]int)
	for i, line := range lines {
		idx := strings.Index(line, viewportImageMarkerPrefix)
		if idx < 0 {
			continue
		}
		marker := strings.TrimSpace(line[idx:])
		marker = strings.TrimSuffix(strings.TrimPrefix(marker, viewportImageMarkerPrefix), "@@")
		parts := strings.Split(marker, ":")
		if len(parts) != 2 {
			continue
		}
		token := parts[0]
		row, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		art, ok := m.viewportImageArtifacts[token]
		if !ok {
			continue
		}
		if row == 0 {
			active[token] = len(blocks)
			blocks = append(blocks, viewportImageBlock{Key: token, StartLine: i, WidthCells: art.WidthCells, HeightCells: art.HeightCells})
		} else if bi, ok := active[token]; ok {
			blocks[bi].HeightCells = max(blocks[bi].HeightCells, row+1)
		}
		lines[i] = strings.Repeat(" ", max(1, art.WidthCells))
	}
	m.viewportImageBlocks = blocks
	termimage.Debugf(termimage.DefaultEnvironment(), "chat extracted viewport image blocks=%d", len(blocks))
	return strings.Join(lines, "\n"), blocks
}

func (m *Model) imageMaxCols() int {
	if m == nil || m.width <= 0 {
		return termimage.DefaultMaxCols
	}
	cols := m.width - 2
	if cols < 1 {
		cols = 1
	}
	return cols
}

func (m *Model) imageMaxRows() int {
	rows := chatImageMaxRows
	if m != nil && m.altScreen {
		viewportRows := m.viewport.Height()
		if viewportRows <= 0 {
			viewportRows = m.viewportRows
		}
		// Keep generated images within the visible chat viewport. Kitty resolves
		// Unicode placeholders by their row/column coordinates, so if Bubble Tea
		// scrolls away row 0 of a tall image the terminal can anchor the real image
		// above the visible area. Reserve a few rows for the caption, surrounding
		// spacing, and streaming status line.
		if limit := viewportRows - 4; limit > 0 && limit < rows {
			rows = limit
		}
	}
	if rows < 1 {
		rows = 1
	}
	return rows
}

func (m *Model) imageBackground() color.Color {
	if m == nil || m.styles == nil || m.styles.Theme() == nil {
		return nil
	}
	return m.styles.Theme().Background
}

// queueImageUpload queues upload bytes for direct tea.Raw emission. Placeholder
// text remains in viewport content immediately so Bubble Tea lays out and scrolls
// the final image area from the first frame; Kitty will attach the real image as
// soon as the queued upload/placement bytes are flushed.
func (m *Model) queueImageUpload(key, upload string) {
	if m == nil || key == "" || upload == "" {
		return
	}
	if m.uploadedImageKeys == nil {
		m.uploadedImageKeys = make(map[string]struct{})
	}
	if _, ok := m.uploadedImageKeys[key]; ok {
		return
	}
	if m.pendingImageUploadKeys == nil {
		m.pendingImageUploadKeys = make(map[string]struct{})
	}
	if _, ok := m.pendingImageUploadKeys[key]; ok {
		return
	}
	firstImageUpload := len(m.uploadedImageKeys) == 0 && len(m.pendingImageUploadKeys) == 0 && len(m.pendingImageUploads) == 0
	m.pendingImageUploadKeys[key] = struct{}{}
	termimage.Debugf(termimage.DefaultEnvironment(), "chat queue image upload key=%s bytes=%d first=%t", key, len(upload), firstImageUpload)
	if firstImageUpload {
		m.queueImageCleanup()
	}
	m.pendingImageUploads = append(m.pendingImageUploads, upload)
	m.scheduleImageUploadFlush()
}

func (m *Model) queueImagePlacement(key, place string) {
	if m == nil || key == "" || place == "" {
		return
	}
	if m.placedImageKeys == nil {
		m.placedImageKeys = make(map[string]struct{})
	}
	if _, ok := m.placedImageKeys[key]; ok {
		return
	}
	if m.pendingImagePlaceKeys == nil {
		m.pendingImagePlaceKeys = make(map[string]struct{})
	}
	if _, ok := m.pendingImagePlaceKeys[key]; ok {
		return
	}
	m.pendingImagePlaceKeys[key] = struct{}{}
	m.pendingImageUploads = append(m.pendingImageUploads, place)
	termimage.Debugf(termimage.DefaultEnvironment(), "chat queue image placement key=%s bytes=%d", key, len(place))
	m.scheduleImageUploadFlush()
}

func (m *Model) scheduleImageUploadFlush() {
	if m == nil || m.imageUploadFlushScheduled {
		return
	}
	m.imageUploadFlushScheduled = true
	// Uploads discovered while rendering View() cannot be returned as commands
	// from View(). If the real Bubble Tea program is available, poke the update
	// loop so it can emit the pending bytes with tea.Raw before/alongside the next
	// frame. Use a goroutine because Program.Send is blocking and View() runs on
	// Bubble Tea's event-loop goroutine.
	if m.program != nil {
		p := m.program
		go p.Send(imageUploadFlushMsg{})
	}
}

func (m *Model) imageCleanupSequence() string {
	if m == nil {
		return ""
	}
	if len(m.ownedKittyImageIDs) > 0 {
		ids := make([]uint32, 0, len(m.ownedKittyImageIDs))
		for id := range m.ownedKittyImageIDs {
			ids = append(ids, id)
		}
		if seq := termimage.KittyDeleteImageSequence(ids...); seq != "" {
			return seq
		}
	}
	return termimage.CleanupSequence(termimage.DefaultEnvironment())
}

func (m *Model) queueImageCleanup() {
	if m == nil || !m.altScreen || m.imageCleanupQueued {
		return
	}
	seq := m.imageCleanupSequence()
	if seq == "" {
		return
	}
	m.pendingImageUploads = append([]string{seq}, m.pendingImageUploads...)
	termimage.Debugf(termimage.DefaultEnvironment(), "chat queue image cleanup bytes=%d", len(seq))
	m.imageCleanupQueued = true
	m.scheduleImageUploadFlush()
}

func (m *Model) drainPendingImageUploads() string {
	if m == nil || len(m.pendingImageUploads) == 0 {
		if m != nil {
			m.imageUploadFlushScheduled = false
		}
		return ""
	}
	uploads := strings.Join(m.pendingImageUploads, "")
	m.pendingImageUploads = nil
	for key := range m.pendingImageUploadKeys {
		if m.uploadedImageKeys == nil {
			m.uploadedImageKeys = make(map[string]struct{})
		}
		m.uploadedImageKeys[key] = struct{}{}
	}
	for key := range m.pendingImagePlaceKeys {
		if m.placedImageKeys == nil {
			m.placedImageKeys = make(map[string]struct{})
		}
		m.placedImageKeys[key] = struct{}{}
	}
	m.pendingImageUploadKeys = make(map[string]struct{})
	m.pendingImagePlaceKeys = make(map[string]struct{})
	m.imageCleanupQueued = false
	m.imagePlaceholdersSuppressed = false
	m.imageUploadFlushScheduled = false
	return uploads
}

func (m *Model) drainPendingImageUploadCmd() tea.Cmd {
	cleanupOnlyResize := m != nil && m.imagePlaceholdersSuppressed && len(m.pendingImageUploadKeys) == 0 && len(m.pendingImagePlaceKeys) == 0 && len(m.pendingImageUploads) > 0
	if cleanupOnlyResize {
		uploads := strings.Join(m.pendingImageUploads, "")
		m.pendingImageUploads = nil
		m.imageCleanupQueued = false
		m.imageUploadFlushScheduled = false
		if uploads == "" {
			return nil
		}
		termimage.Debugf(termimage.DefaultEnvironment(), "chat flush cleanup-only resize bytes=%d", len(uploads))
		return tea.Sequence(
			tea.ClearScreen,
			tea.Raw("\x1b[2J\x1b[H"+uploads),
			func() tea.Msg { return imageCleanupFlushedMsg{} },
		)
	}

	uploads := m.drainPendingImageUploads()
	if uploads == "" {
		return nil
	}
	m.invalidateImageViewportContent()
	termimage.Debugf(termimage.DefaultEnvironment(), "chat flush image uploads bytes=%d", len(uploads))
	return tea.Raw(uploads)
}

func (m *Model) invalidateImageViewportContent() {
	if m == nil {
		return
	}
	if m.chatRenderer != nil {
		m.chatRenderer.InvalidateCache()
	}
	m.viewCache.lastViewportView = ""
	m.viewCache.lastSetContentAt = time.Time{}
	m.viewCache.cachedTrackerVersion = 0
	m.resetAltScreenStreamingAppendCache()
	m.bumpContentVersion()
}

func (m *Model) finishImageCleanupFlush() tea.Cmd {
	if m == nil {
		return nil
	}
	m.imagePlaceholdersSuppressed = false
	termimage.Debugf(termimage.DefaultEnvironment(), "chat image cleanup flushed; re-enable placeholders and schedule repaint")
	m.invalidateImageViewportContent()
	m.scheduleImageUploadFlush()
	return nil
}

func (m *Model) resetImageUploadState() {
	if m == nil {
		return
	}
	m.pendingImageUploads = nil
	m.pendingImageUploadKeys = make(map[string]struct{})
	m.pendingImagePlaceKeys = make(map[string]struct{})
	m.uploadedImageKeys = make(map[string]struct{})
	m.placedImageKeys = make(map[string]struct{})
	m.visibleImageKeys = make(map[string]struct{})
	m.ownedKittyImageIDs = make(map[uint32]struct{})
	m.postFrameVisibleImages = make(map[string]postFrameImageState)
	m.postFrameCurrentImages = nil
	m.postFrameQueuedImages = nil
	m.postFrameTransmittedImages = make(map[uint32]struct{})
	m.viewportImageArtifacts = make(map[string]viewportImageArtifact)
	m.viewportImageBlocks = nil
	m.imageCleanupQueued = false
	m.imagePlaceholdersSuppressed = false
	m.imageUploadFlushScheduled = false
}

func (m *Model) resetUploadedImageKeys() {
	if m == nil {
		return
	}
	m.uploadedImageKeys = make(map[string]struct{})
	m.placedImageKeys = make(map[string]struct{})
}

func (m *Model) terminalImageCleanupCmd() tea.Cmd {
	if m == nil || !m.altScreen || (len(m.uploadedImageKeys) == 0 && len(m.placedImageKeys) == 0 && len(m.pendingImageUploadKeys) == 0 && len(m.pendingImagePlaceKeys) == 0 && len(m.pendingImageUploads) == 0 && len(m.ownedKittyImageIDs) == 0) {
		return nil
	}
	seq := m.imageCleanupSequence()
	if seq == "" {
		return nil
	}
	return tea.Raw(seq)
}

func (m *Model) quitCmd(cmds ...tea.Cmd) tea.Cmd {
	seq := make([]tea.Cmd, 0, len(cmds)+2)
	for _, cmd := range cmds {
		if cmd != nil {
			seq = append(seq, cmd)
		}
	}
	if cleanup := m.terminalImageCleanupCmd(); cleanup != nil {
		seq = append(seq, cleanup)
	}
	seq = append(seq, tea.Quit)
	return tea.Sequence(seq...)
}

type imageUploadFlushMsg struct{}
type imageCleanupFlushedMsg struct{}
