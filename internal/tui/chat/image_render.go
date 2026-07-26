package chat

import (
	"fmt"
	"hash/fnv"
	"image/color"
	"maps"
	"os"
	"slices"
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
	ScreenRow   int
	Upload      string
}

type postFrameImageReceipt struct {
	Generation    uint64
	Reset         bool
	Prefix        string
	LegacyUploads []string
	UploadedIDs   []uint32
	Images        map[string]postFrameImageState
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

	termimage.Debugf(termimage.DefaultEnvironment(), "chat render image path=%s protocol=%s cells=%dx%d viewport=%dx%d model=%dx%d upload=%d display=%d", path, result.Protocol, result.WidthCells, result.HeightCells, m.viewport.Width(), m.viewport.Height(), m.width, m.height, len(result.Upload), len(result.Display))

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
		m.addOwnedKittyImageID(result.ImageID)
		artifact.Display = viewportImageMarkerGrid(token, result.WidthCells, result.HeightCells)
		artifact.Upload = ""
		return artifact
	}

	if artifact.Upload != "" {
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

func (m *Model) postFrameImageCompositionEnabled() bool {
	if m == nil {
		return false
	}
	m.postFrameImageMu.Lock()
	defer m.postFrameImageMu.Unlock()
	return !m.postFrameRetryDisabled || m.postFrameFailureGeneration != m.imageGeneration
}

func (m *Model) beginPostFrameImageComposition() {
	if m == nil || !m.altScreen || m.externalProcessActive || m.quitting {
		return
	}
	m.postFrameImageMu.Lock()
	defer m.postFrameImageMu.Unlock()
	if m.postFrameRenderCache == nil {
		m.postFrameRenderCache = make(map[string]postFrameImageState)
	}
	if m.postFrameKnownImages == nil {
		m.postFrameKnownImages = make(map[string]postFrameImageState)
	}
	if m.postFrameUploadedImages == nil {
		m.postFrameUploadedImages = make(map[uint32]struct{})
	}
	m.postFrameCurrentImages = clonePostFrameImageStates(m.postFrameLastImages)
	if m.postFrameCurrentImages == nil {
		m.postFrameCurrentImages = make(map[string]postFrameImageState)
	}
	m.postFrameImageUploadSeq = ""
	m.postFrameImagePlaceSeq = ""
}

func (m *Model) resetPostFrameCurrentImages() {
	if m == nil || !m.altScreen {
		return
	}
	m.postFrameImageMu.Lock()
	m.postFrameCurrentImages = make(map[string]postFrameImageState)
	m.postFrameImageMu.Unlock()
}

func (m *Model) queuePostFrameViewportImage(art viewportImageArtifact, blockStartLine, startRow, rows, screenRow int) {
	if m == nil || art.Path == "" || rows <= 0 || screenRow < 0 || screenRow >= m.height {
		return
	}
	placementKey := fmt.Sprintf("%s:direct:%d:%d:%d", art.Key, blockStartLine, startRow, rows)
	renderKey := fmt.Sprintf("%s:direct-render:%d:%d", art.Key, startRow, rows)
	placementID := postFramePlacementID(placementKey)

	m.postFrameImageMu.Lock()
	state, cached := m.postFrameRenderCache[renderKey]
	m.postFrameImageMu.Unlock()
	if !cached {
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
		state = postFrameImageState{ImageID: result.ImageID, WidthCells: result.WidthCells, HeightCells: result.HeightCells, Upload: result.Upload}
		m.postFrameImageMu.Lock()
		if m.postFrameRenderCache == nil {
			m.postFrameRenderCache = make(map[string]postFrameImageState)
		}
		m.postFrameRenderCache[renderKey] = state
		m.postFrameImageMu.Unlock()
	}

	state.PlacementID = placementID
	state.ScreenRow = screenRow
	m.postFrameImageMu.Lock()
	defer m.postFrameImageMu.Unlock()
	if m.postFrameCurrentImages == nil {
		m.postFrameCurrentImages = make(map[string]postFrameImageState)
	}
	m.postFrameCurrentImages[placementKey] = state
	m.addOwnedKittyImageID(state.ImageID)
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
	if m == nil || !m.altScreen || m.externalProcessActive || m.quitting {
		return
	}
	m.postFrameImageMu.Lock()
	defer m.postFrameImageMu.Unlock()

	prefix := m.postFrameImagePrefixSeq
	reset := prefix != ""
	knownImages := m.postFrameKnownImages
	uploadedImages := m.postFrameUploadedImages
	if reset {
		knownImages = nil
		uploadedImages = nil
	}

	knownKeys := slices.Sorted(maps.Keys(knownImages))
	for _, key := range knownKeys {
		previous := knownImages[key]
		current, stillVisible := m.postFrameCurrentImages[key]
		if (!stillVisible || previous.ImageID != current.ImageID || previous.PlacementID != current.PlacementID) && previous.ImageID != 0 {
			m.postFrameImagePlaceSeq += termimage.KittyDeletePlacementSequence(previous.ImageID, previous.PlacementID)
		}
	}

	currentKeys := slices.Sorted(maps.Keys(m.postFrameCurrentImages))
	queuedUploads := make(map[uint32]struct{}, len(currentKeys))
	uploadedIDs := make([]uint32, 0, len(currentKeys))
	for _, key := range currentKeys {
		current := m.postFrameCurrentImages[key]
		if _, acknowledged := uploadedImages[current.ImageID]; !acknowledged {
			if _, queued := queuedUploads[current.ImageID]; !queued {
				m.postFrameImageUploadSeq += current.Upload
				queuedUploads[current.ImageID] = struct{}{}
				uploadedIDs = append(uploadedIDs, current.ImageID)
			}
		}
		if previous, acknowledged := knownImages[key]; !acknowledged || !samePostFramePlacement(previous, current) {
			place := termimage.KittyDirectPlaceSequence(current.ImageID, current.PlacementID, current.WidthCells, current.HeightCells)
			m.postFrameImagePlaceSeq += fmt.Sprintf("\x1b[%d;1H%s", current.ScreenRow+1, place)
		}
	}
	m.postFrameLastImages = clonePostFrameImageStates(m.postFrameCurrentImages)
	m.postFrameCurrentImages = nil

	legacyUploads := append([]string(nil), m.pendingImageUploads...)
	legacySeq := strings.Join(legacyUploads, "")
	if legacySeq != "" {
		// Legacy inline protocols (notably iTerm2 and sixel) render at and may
		// advance the current cursor. Keep their upload transaction from changing
		// Bubble Tea's retained cursor position.
		legacySeq = "\x1b[s" + legacySeq + "\x1b[u"
	}
	m.postFrameImageSeq = legacySeq + prefix + m.postFrameImageUploadSeq
	if m.postFrameImagePlaceSeq != "" {
		m.postFrameImageSeq += "\x1b[s" + m.postFrameImagePlaceSeq + "\x1b[u"
	}
	if m.postFrameImageSeq != "" {
		m.postFrameReceipt = &postFrameImageReceipt{
			Generation:    m.imageGeneration,
			Reset:         reset,
			Prefix:        prefix,
			LegacyUploads: legacyUploads,
			UploadedIDs:   uploadedIDs,
			Images:        clonePostFrameImageStates(m.postFrameLastImages),
		}
	} else {
		m.postFrameReceipt = nil
	}
	m.postFrameImageUploadSeq = ""
	m.postFrameImagePlaceSeq = ""
}

func samePostFramePlacement(a, b postFrameImageState) bool {
	return a.ImageID == b.ImageID &&
		a.PlacementID == b.PlacementID &&
		a.WidthCells == b.WidthCells &&
		a.HeightCells == b.HeightCells &&
		a.ScreenRow == b.ScreenRow
}

func clonePostFrameImageStates(src map[string]postFrameImageState) map[string]postFrameImageState {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]postFrameImageState, len(src))
	for key, state := range src {
		dst[key] = state
	}
	return dst
}

func (m *Model) postFrameImagePayloadForView() (string, *postFrameImageReceipt) {
	if m == nil || !m.altScreen || m.externalProcessActive {
		return "", nil
	}
	m.postFrameImageMu.Lock()
	defer m.postFrameImageMu.Unlock()
	if m.quitting || (m.postFrameRetryDisabled && m.postFrameFailureGeneration == m.imageGeneration) {
		return "", nil
	}
	return m.postFrameImageSeq, clonePostFrameImageReceipt(m.postFrameReceipt)
}

func clonePostFrameImageReceipt(receipt *postFrameImageReceipt) *postFrameImageReceipt {
	if receipt == nil {
		return nil
	}
	clone := *receipt
	clone.LegacyUploads = append([]string(nil), receipt.LegacyUploads...)
	clone.UploadedIDs = append([]uint32(nil), receipt.UploadedIDs...)
	clone.Images = clonePostFrameImageStates(receipt.Images)
	return &clone
}

func (m *Model) queuePostFrameImagePrefix(seq string) {
	if m == nil || seq == "" {
		return
	}
	m.postFrameImageMu.Lock()
	m.postFrameImagePrefixSeq += seq
	m.postFrameImageMu.Unlock()
}

func (m *Model) hasPostFrameImageActivity() bool {
	return m != nil && (len(m.pendingImageUploads) > 0 || len(m.ownedKittyImageIDs) > 0 || len(m.postFrameKnownImages) > 0)
}

func (m *Model) queuePostFrameImageCleanupIfActive() {
	if m == nil || !m.hasPostFrameImageActivity() {
		return
	}
	m.queuePostFrameImagePrefix(m.imageCleanupSequence())
}

func (m *Model) handlePostFrameImageResult(receipt *postFrameImageReceipt, err error) tea.Cmd {
	if m == nil || m.quitting || receipt == nil || receipt.Generation != m.imageGeneration {
		return nil
	}
	if err != nil {
		// Disable automatic retries for this generation. The terminal may have
		// consumed an arbitrary payload prefix, so acknowledged image state stays
		// unchanged; a resize/manual image reset advances the generation and
		// deliberately recomposes the complete transition.
		m.postFrameImageMu.Lock()
		alreadySurfaced := m.postFrameRetryDisabled && m.postFrameFailureGeneration == receipt.Generation
		m.postFrameRetryDisabled = true
		m.postFrameFailureGeneration = receipt.Generation
		m.postFrameImageMu.Unlock()
		if alreadySurfaced {
			return nil
		}
		_, cmd := m.showFooterError(fmt.Sprintf("Terminal image update failed; image retries paused until refresh: %v", err))
		return cmd
	}

	m.postFrameImageMu.Lock()
	defer m.postFrameImageMu.Unlock()
	if receipt.Reset {
		m.postFrameKnownImages = make(map[string]postFrameImageState)
		m.postFrameUploadedImages = make(map[uint32]struct{})
	}
	if m.postFrameUploadedImages == nil {
		m.postFrameUploadedImages = make(map[uint32]struct{})
	}
	for _, id := range receipt.UploadedIDs {
		if id != 0 {
			m.postFrameUploadedImages[id] = struct{}{}
		}
	}
	m.postFrameKnownImages = clonePostFrameImageStates(receipt.Images)
	if m.postFrameKnownImages == nil {
		m.postFrameKnownImages = make(map[string]postFrameImageState)
	}
	if receipt.Prefix != "" && m.postFrameImagePrefixSeq == receipt.Prefix {
		m.postFrameImagePrefixSeq = ""
	}
	if postFrameLegacyPrefixMatches(m.pendingImageUploads, receipt.LegacyUploads) {
		m.pendingImageUploads = m.pendingImageUploads[len(receipt.LegacyUploads):]
		if len(m.pendingImageUploads) == 0 {
			m.pendingImageUploadKeys = make(map[string]struct{})
			m.pendingImagePlaceKeys = make(map[string]struct{})
			m.imageCleanupQueued = false
		}
	}
	return nil
}

func postFrameLegacyPrefixMatches(pending, acknowledged []string) bool {
	if len(acknowledged) > len(pending) {
		return false
	}
	for i := range acknowledged {
		if pending[i] != acknowledged[i] {
			return false
		}
	}
	return true
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

// queueImageUpload retains upload bytes in replacement-capable Views until the
// exact PostFrame attempt is acknowledged.
func (m *Model) queueImageUpload(key, upload string) {
	if m == nil || key == "" || upload == "" {
		return
	}
	if m.pendingImageUploadKeys == nil {
		m.pendingImageUploadKeys = make(map[string]struct{})
	}
	if _, ok := m.pendingImageUploadKeys[key]; ok {
		return
	}
	firstImageUpload := len(m.pendingImageUploadKeys) == 0 && len(m.pendingImageUploads) == 0
	m.pendingImageUploadKeys[key] = struct{}{}
	termimage.Debugf(termimage.DefaultEnvironment(), "chat queue image upload key=%s bytes=%d first=%t", key, len(upload), firstImageUpload)
	if firstImageUpload {
		m.queueImageCleanup()
	}
	m.pendingImageUploads = append(m.pendingImageUploads, upload)
}

func (m *Model) queueImagePlacement(key, place string) {
	if m == nil || key == "" || place == "" {
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
}

func (m *Model) addOwnedKittyImageID(id uint32) {
	if m == nil || id == 0 {
		return
	}
	if m.ownedKittyImageIDs == nil {
		m.ownedKittyImageIDs = make(map[uint32]struct{})
	}
	if _, exists := m.ownedKittyImageIDs[id]; exists {
		return
	}
	m.ownedKittyImageIDs[id] = struct{}{}
	m.imageCleanupSeqValid = false
}

func (m *Model) clearOwnedKittyImageIDs() {
	if m == nil || len(m.ownedKittyImageIDs) == 0 {
		return
	}
	m.ownedKittyImageIDs = make(map[uint32]struct{})
	m.imageCleanupSeqValid = false
}

func (m *Model) imageCleanupSequence() string {
	if m == nil {
		return ""
	}
	if m.imageCleanupSeqValid {
		return m.imageCleanupSeq
	}
	seq := ""
	if len(m.ownedKittyImageIDs) > 0 {
		ids := slices.Sorted(maps.Keys(m.ownedKittyImageIDs))
		seq = termimage.KittyDeleteImageSequence(ids...)
	}
	if seq == "" {
		seq = termimage.CleanupSequence(termimage.DefaultEnvironment())
	}
	m.imageCleanupSeq = seq
	m.imageCleanupSeqValid = true
	return seq
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

func (m *Model) resetImageUploadState() {
	if m == nil {
		return
	}
	m.imageGeneration++
	m.postFrameImageMu.Lock()
	m.postFrameRetryDisabled = false
	m.postFrameFailureGeneration = 0
	m.postFrameImageMu.Unlock()
	m.queuePostFrameImageCleanupIfActive()
	m.pendingImageUploads = nil
	m.pendingImageUploadKeys = make(map[string]struct{})
	m.pendingImagePlaceKeys = make(map[string]struct{})
	m.clearOwnedKittyImageIDs()
	m.postFrameCurrentImages = nil
	m.postFrameLastImages = make(map[string]postFrameImageState)
	m.postFrameKnownImages = make(map[string]postFrameImageState)
	m.postFrameUploadedImages = make(map[uint32]struct{})
	m.postFrameRenderCache = make(map[string]postFrameImageState)
	m.postFrameReceipt = nil
	m.viewportImageArtifacts = make(map[string]viewportImageArtifact)
	m.viewportImageBlocks = nil
	m.imageCleanupQueued = false
}

func (m *Model) quitCmd(cmds ...tea.Cmd) tea.Cmd {
	// Bubble Tea can call View again for commands sequenced before Quit and once
	// more during its final renderer flush. Disable normal image composition so
	// none of those Views can recreate a placement. View.TerminalCleanup remains
	// armed for every renderer shutdown path.
	m.quitting = true

	seq := make([]tea.Cmd, 0, len(cmds)+1)
	for _, cmd := range cmds {
		if cmd != nil {
			seq = append(seq, cmd)
		}
	}
	seq = append(seq, tea.Quit)
	return tea.Sequence(seq...)
}
