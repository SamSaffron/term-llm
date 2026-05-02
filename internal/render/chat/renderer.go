package chat

import (
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/session"
)

// historyBuilderPool reuses strings.Builder instances across renderHistory
// calls to reduce per-frame allocations in the hot render path.
var historyBuilderPool = sync.Pool{
	New: func() any { return new(strings.Builder) },
}

const (
	fnv64Offset = 14695981039346656037
	fnv64Prime  = 1099511628211
)

// RenderMode determines how content is displayed
type RenderMode int

const (
	RenderModeInline    RenderMode = iota // Traditional inline mode with scrollback
	RenderModeAltScreen                   // Full-screen alternate screen mode
)

// ViewportState tracks the viewport scroll position
type ViewportState struct {
	Height       int // Visible height in lines
	ScrollOffset int // Scroll offset from bottom (0 = bottom)
	AtBottom     bool
}

// InputState tracks input area state for rendering
type InputState struct {
	Height  int  // Height of input area in lines
	Focused bool // Whether input is focused
}

// StreamingState represents the state of active streaming
type StreamingState struct {
	Phase       string   // Current phase ("Thinking", "Responding", etc.)
	WavePos     int      // Wave animation position (-1 = paused)
	SpinnerView string   // Pre-rendered spinner string
	RetryStatus string   // Retry status message if any
	PausedForUI bool     // True when paused for external UI
	ElapsedSecs float64  // Seconds since stream started
	Tokens      int      // Current token count
	ActiveTools []string // Names of currently executing tools
}

// RenderState holds all state needed for rendering
type RenderState struct {
	Messages  []session.Message
	Streaming *StreamingState // nil if not streaming
	Viewport  ViewportState
	Input     InputState
	Mode      RenderMode
	Width     int
	Height    int
	ShowStats bool
	Error     error // Display error if set
}

// FlushResult contains the result of flushing content to scrollback
type FlushResult struct {
	// Content to print to scrollback (empty if nothing to flush)
	Content string
}

// MarkdownRenderer is a function that renders markdown content
type MarkdownRenderer func(content string, width int) string

// ChatRenderer is the main interface for the chat rendering module.
// It provides a clean abstraction over the rendering logic, supporting
// both inline and alt-screen modes with virtualized rendering for
// large message histories.
type ChatRenderer interface {
	// Render returns the full view content for the current state.
	// This is called by View() in the bubbletea model.
	Render(state RenderState) string

	// HandleEvent processes a render event and returns any commands
	// that should be executed (e.g., starting animations).
	HandleEvent(event RenderEvent) tea.Cmd

	// SetSize updates the terminal dimensions.
	// This invalidates caches as needed.
	SetSize(width, height int)

	// Flush returns content that should be printed to scrollback
	// and clears it from the active view (inline mode only).
	Flush() FlushResult

	// FlushAll flushes all remaining content to scrollback.
	// Called when streaming ends.
	FlushAll() FlushResult

	// InvalidateCache forces re-rendering of all cached content.
	// Call this on terminal resize.
	InvalidateCache()

	// SetMarkdownRenderer sets the function used to render markdown.
	SetMarkdownRenderer(renderer MarkdownRenderer)
}

// sigCacheEntry caches the expensive messagePartsSignature result for a message.
// Validity is checked via cheap comparisons before reuse.
type sigCacheEntry struct {
	textLen  int
	partsCnt int
	textHash uint64 // FNV-1a of TextContent for same-length change detection
	sig      uint64
}

// Renderer implements ChatRenderer with virtualized rendering
// and caching for performance.
type Renderer struct {
	// Dimensions
	width  int
	height int

	// Caches
	blockCache *BlockCache
	sigCache   map[int64]sigCacheEntry // message ID → cached parts signature

	// Streaming state
	streaming *StreamingBlock

	// Configuration
	markdownRenderer MarkdownRenderer
	toolsExpanded    bool
}

// NewRenderer creates a new chat renderer with the given dimensions.
func NewRenderer(width, height int) *Renderer {
	// Size cache proportional to viewport: estimate ~5 lines/message average,
	// then 3x buffer for smooth scrolling. Minimum 50, maximum 2000.
	cacheSize := (height / 5) * 3
	if cacheSize < 50 {
		cacheSize = 50
	} else if cacheSize > 2000 {
		cacheSize = 2000
	}
	r := &Renderer{
		width:      width,
		height:     height,
		blockCache: NewBlockCache(cacheSize),
		sigCache:   make(map[int64]sigCacheEntry),
	}
	return r
}

// SetSize updates the terminal dimensions and invalidates width-dependent caches.
func (r *Renderer) SetSize(width, height int) {
	widthChanged := r.width != width
	r.width = width
	r.height = height

	if widthChanged {
		// Width affects all rendered content - invalidate everything
		r.blockCache.InvalidateAll()
		if r.streaming != nil {
			r.streaming.Resize(width)
		}
	}
}

// SetMarkdownRenderer sets the function used to render markdown.
func (r *Renderer) SetMarkdownRenderer(renderer MarkdownRenderer) {
	r.markdownRenderer = renderer
}

// SetToolsExpanded toggles expanded tool rendering in the streaming block.
func (r *Renderer) SetToolsExpanded(v bool) {
	r.toolsExpanded = v
	r.blockCache.InvalidateAll()
	if r.streaming != nil {
		r.streaming.SetToolsExpanded(v)
	}
}

// InvalidateCache forces re-rendering of all cached content.
func (r *Renderer) InvalidateCache() {
	r.blockCache.InvalidateAll()
	clear(r.sigCache)
}

// HandleEvent processes a render event and returns any commands.
func (r *Renderer) HandleEvent(event RenderEvent) tea.Cmd {
	switch event.Type {
	case RenderEventStreamStart:
		r.streaming = NewStreamingBlock(r.width, r.markdownRenderer)
		r.streaming.SetToolsExpanded(r.toolsExpanded)
		return r.streaming.StartWaveAnimation()

	case RenderEventStreamText:
		if r.streaming != nil {
			r.streaming.AddText(event.Text)
		}
		return nil

	case RenderEventStreamToolStart:
		if r.streaming != nil {
			started := r.streaming.StartTool(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolArgs)
			if started {
				return r.streaming.StartWaveAnimation()
			}
		}
		return nil

	case RenderEventStreamToolEnd:
		if r.streaming != nil {
			r.streaming.EndTool(event.ToolCallID, event.ToolSuccess)
		}
		return nil

	case RenderEventStreamImage:
		if r.streaming != nil {
			r.streaming.AddImage(event.ImagePath)
		}
		return nil

	case RenderEventStreamDiff:
		if r.streaming != nil {
			r.streaming.AddDiffWithOperation(event.DiffPath, event.DiffOld, event.DiffNew, event.DiffLine, event.DiffOperation)
		}
		return nil

	case RenderEventStreamAskUserResult:
		if r.streaming != nil {
			r.streaming.AddAskUserResult(event.AskUserSummary)
		}
		return nil

	case RenderEventStreamEnd:
		// Keep streaming content for display until cleared
		// but mark it as complete
		if r.streaming != nil {
			r.streaming.Complete()
		}
		return nil

	case RenderEventStreamError:
		if r.streaming != nil {
			r.streaming.SetError(event.Err)
		}
		return nil

	case RenderEventResize:
		r.SetSize(event.Width, event.Height)
		return nil

	case RenderEventMessageAdded:
		// When a new message is added, it might be a tool result for a previous message.
		// The previous message (with tool calls) may have been cached without the diff
		// because the tool result didn't exist yet. Invalidate all caches to ensure
		// diffs are rendered correctly. This is conservative but necessary for correctness.
		r.blockCache.InvalidateAll()
		return nil

	case RenderEventMessagesLoaded:
		// When messages are loaded from storage, invalidate caches to ensure
		// fresh rendering with the complete message context.
		r.blockCache.InvalidateAll()
		return nil

	case RenderEventMessagesClear:
		// Clear all message caches when messages are cleared
		r.blockCache.InvalidateAll()
		return nil

	case RenderEventScroll:
		// Scroll is handled by the viewport, not the renderer
		// This event exists for consistency but doesn't require action
		return nil

	case RenderEventInvalidateCache:
		r.InvalidateCache()
		return nil

	default:
		return nil
	}
}

// Render returns the full view content for the current state.
func (r *Renderer) Render(state RenderState) string {
	if state.Mode == RenderModeAltScreen {
		return r.renderAltScreen(state)
	}
	return r.renderInline(state)
}

// renderInline renders content for inline mode.
// In inline mode, historical messages are flushed to scrollback,
// and only the active streaming content is rendered in View().
func (r *Renderer) renderInline(state RenderState) string {
	// In inline mode with scroll offset, render history
	if state.Viewport.ScrollOffset > 0 {
		return r.renderHistory(state)
	}

	// Otherwise just render streaming content (if any)
	if state.Streaming != nil && r.streaming != nil {
		return r.streaming.Render(state.Streaming.WavePos, state.Streaming.PausedForUI, false)
	}

	return ""
}

// renderAltScreen renders content for alt-screen mode.
// This includes all historical messages plus streaming content.
func (r *Renderer) renderAltScreen(state RenderState) string {
	history := r.renderHistory(state)

	// Add streaming content if active
	if state.Streaming != nil && r.streaming != nil {
		streaming := r.streaming.Render(state.Streaming.WavePos, state.Streaming.PausedForUI, true)
		return history + streaming
	}

	return history
}

// renderHistory renders the message history with virtualization.
func (r *Renderer) renderHistory(state RenderState) string {
	if len(state.Messages) == 0 {
		return ""
	}

	start := 0
	end := len(state.Messages)
	if state.Mode != RenderModeAltScreen {
		// Inline mode keeps message-window virtualization while using scroll offset.
		vp := NewVirtualViewport(r.width, state.Viewport.Height)
		start, end = vp.GetVisibleRange(state.Messages, state.Viewport.ScrollOffset)
	}

	// Render only visible messages using cache
	// Skip system and tool messages (they render as empty anyway)
	b := historyBuilderPool.Get().(*strings.Builder)
	b.Reset()
	defer historyBuilderPool.Put(b)
	for i := start; i < end; i++ {
		msg := &state.Messages[i]
		// Skip non-renderable roles
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		block := r.getOrRenderBlock(msg, i, state.Messages)
		if block.Rendered != "" {
			b.WriteString(block.Rendered)
		}
	}

	// Clone so the returned string doesn't share the builder's backing
	// array, which will be overwritten when the pool reuses this builder.
	return strings.Clone(b.String())
}

// MessageHistorySignature fingerprints rendered message history so cache validity
// does not rely on message count alone.
func MessageHistorySignature(messages []session.Message) uint64 {
	h := uint64(fnv64Offset)
	for i := range messages {
		msg := &messages[i]
		h = writeInt64Hash(h, msg.ID)
		h = writeIntHash(h, msg.Sequence)
		h = writeStringHash(h, string(msg.Role))
		h = writeStringHash(h, msg.TextContent)
		h = writeUint64Hash(h, messagePartsSignature(msg))
	}
	return h
}

// getOrRenderBlock gets a rendered block from cache or renders it.
func (r *Renderer) getOrRenderBlock(msg *session.Message, index int, messages []session.Message) *MessageBlock {
	// Check cache first
	cacheKey := r.blockCacheKey(msg, index)
	if block := r.blockCache.Get(cacheKey); block != nil {
		return block
	}

	// Render the message
	block := r.renderMessageBlock(msg, index, messages)

	// Cache it
	r.blockCache.Put(cacheKey, block)

	return block
}

// blockCacheKey generates a cache key for a message.
// Key includes a content fingerprint, not just message ID, so stale blocks
// cannot survive same-ID content changes.
func (r *Renderer) blockCacheKey(msg *session.Message, index int) string {
	expanded := "0"
	if r.toolsExpanded {
		expanded = "1"
	}
	return strconv.FormatInt(msg.ID, 10) + ":" + strconv.Itoa(r.width) + ":" + expanded + ":" + strconv.FormatUint(r.cachedPartsSignature(msg), 16)
}

// cachedPartsSignature returns the parts signature for a message, using a
// per-message cache to avoid recomputing the expensive hash on every frame.
// Cache validity is checked via cheap length comparisons and a text hash.
func (r *Renderer) cachedPartsSignature(msg *session.Message) uint64 {
	th := quickTextHash(msg.TextContent)
	if entry, ok := r.sigCache[msg.ID]; ok {
		if entry.textLen == len(msg.TextContent) && entry.partsCnt == len(msg.Parts) && entry.textHash == th {
			return entry.sig
		}
	}
	sig := messagePartsSignature(msg)
	r.sigCache[msg.ID] = sigCacheEntry{
		textLen:  len(msg.TextContent),
		partsCnt: len(msg.Parts),
		textHash: th,
		sig:      sig,
	}
	return sig
}

// quickTextHash computes a fast hash of a string for cache invalidation.
// Only samples the first and last 64 bytes plus the length to avoid O(n)
// hashing on every frame for large messages. Uses inline FNV-1a to avoid
// heap allocations from fnv.New64a() and []byte conversions.
func quickTextHash(s string) uint64 {
	h := uint64(fnv64Offset)
	n := len(s)
	if n <= 128 {
		for i := 0; i < n; i++ {
			h ^= uint64(s[i])
			h *= fnv64Prime
		}
	} else {
		for i := 0; i < 64; i++ {
			h ^= uint64(s[i])
			h *= fnv64Prime
		}
		for i := n - 64; i < n; i++ {
			h ^= uint64(s[i])
			h *= fnv64Prime
		}
	}
	h ^= uint64(n)
	h *= fnv64Prime
	return h
}

func messagePartsSignature(msg *session.Message) uint64 {
	h := uint64(fnv64Offset)
	h = writeStringHash(h, msg.TextContent)
	h = writeIntHash(h, len(msg.Parts))
	for i := range msg.Parts {
		part := &msg.Parts[i]
		h = writeStringHash(h, string(part.Type))
		h = writeStringHash(h, part.Text)
		h = writeStringHash(h, part.ReasoningContent)
		h = writeStringHash(h, part.ReasoningItemID)
		h = writeStringHash(h, part.ReasoningEncryptedContent)
		h = writeStringHash(h, part.ImagePath)
		if part.ImageData != nil {
			h = writeStringHash(h, part.ImageData.MediaType)
			h = writeStringHash(h, part.ImageData.Base64)
		}
		if part.ToolCall != nil {
			h = writeStringHash(h, part.ToolCall.ID)
			h = writeStringHash(h, part.ToolCall.Name)
			h = writeBytesHash(h, part.ToolCall.Arguments)
		}
		if part.ToolResult != nil {
			h = writeStringHash(h, part.ToolResult.ID)
			h = writeStringHash(h, part.ToolResult.Name)
			h = writeStringHash(h, part.ToolResult.Content)
			h = writeStringHash(h, part.ToolResult.Display)
			h = writeBoolHash(h, part.ToolResult.IsError)
			for _, diff := range part.ToolResult.Diffs {
				h = writeStringHash(h, diff.File)
				h = writeStringHash(h, diff.Old)
				h = writeStringHash(h, diff.New)
				h = writeIntHash(h, diff.Line)
				h = writeStringHash(h, diff.Operation)
			}
			for _, image := range part.ToolResult.Images {
				h = writeStringHash(h, image)
			}
			for _, contentPart := range part.ToolResult.ContentParts {
				h = writeStringHash(h, string(contentPart.Type))
				h = writeStringHash(h, contentPart.Text)
				if contentPart.ImageData != nil {
					h = writeStringHash(h, contentPart.ImageData.MediaType)
					h = writeStringHash(h, contentPart.ImageData.Base64)
				}
			}
		}
	}
	return h
}

// write*Hash helpers inline FNV-1a over primitive fields on the render
// cache path. They avoid hash.Hash64 interface dispatch and []byte(s)
// conversions, and length-prefix variable-width fields to avoid delimiter
// ambiguities without allocating.
func writeStringHash(h uint64, s string) uint64 {
	h = writeUint64Hash(h, uint64(len(s)))
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnv64Prime
	}
	return h
}

func writeBytesHash(h uint64, b []byte) uint64 {
	h = writeUint64Hash(h, uint64(len(b)))
	for i := 0; i < len(b); i++ {
		h ^= uint64(b[i])
		h *= fnv64Prime
	}
	return h
}

func writeBoolHash(h uint64, v bool) uint64 {
	if v {
		return writeUint64Hash(h, 1)
	}
	return writeUint64Hash(h, 0)
}

func writeIntHash(h uint64, v int) uint64 {
	return writeUint64Hash(h, uint64(v))
}

func writeInt64Hash(h uint64, v int64) uint64 {
	return writeUint64Hash(h, uint64(v))
}

func writeUint64Hash(h uint64, v uint64) uint64 {
	for i := 0; i < 8; i++ {
		h ^= uint64(byte(v))
		h *= fnv64Prime
		v >>= 8
	}
	return h
}

// renderMessageBlock renders a single message to a block.
func (r *Renderer) renderMessageBlock(msg *session.Message, index int, messages []session.Message) *MessageBlock {
	rb := NewMessageBlockRendererWithContext(r.width, r.markdownRenderer, messages, index, r.toolsExpanded)
	return rb.Render(msg)
}

// Flush returns content that should be printed to scrollback.
func (r *Renderer) Flush() FlushResult {
	if r.streaming == nil {
		return FlushResult{}
	}
	return r.streaming.Flush()
}

// FlushAll flushes all remaining content to scrollback.
func (r *Renderer) FlushAll() FlushResult {
	if r.streaming == nil {
		return FlushResult{}
	}
	result := r.streaming.FlushAll()
	// Clear streaming state after final flush
	r.streaming = nil
	return result
}

// ClearStreaming clears the streaming state without flushing.
// Call this when starting a new prompt.
func (r *Renderer) ClearStreaming() {
	r.streaming = nil
}

// GetStreamingContent returns the completed streaming content for caching.
// Used in alt-screen mode to preserve diffs/images after streaming ends.
func (r *Renderer) GetStreamingContent() string {
	if r.streaming == nil {
		return ""
	}
	return r.streaming.GetCompletedContent()
}
