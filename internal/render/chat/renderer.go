package chat

import (
	"strconv"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/session"
)

// historyBuilderPool reuses strings.Builder instances across renderHistory
// calls to reduce per-frame allocations in the hot render path.
var historyBuilderPool = sync.Pool{
	New: func() any { return new(strings.Builder) },
}

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

// Renderer implements ChatRenderer with virtualized rendering
// and caching for performance.
type Renderer struct {
	// Dimensions
	width  int
	height int

	// Caches
	blockCache *BlockCache

	// Streaming state
	streaming *StreamingBlock

	// Configuration
	markdownRenderer MarkdownRenderer
}

// NewRenderer creates a new chat renderer with the given dimensions.
func NewRenderer(width, height int) *Renderer {
	// Size cache proportional to viewport: estimate ~5 lines/message average,
	// then 3x buffer for smooth scrolling. Minimum 50, maximum 200.
	cacheSize := (height / 5) * 3
	if cacheSize < 50 {
		cacheSize = 50
	} else if cacheSize > 200 {
		cacheSize = 200
	}
	r := &Renderer{
		width:      width,
		height:     height,
		blockCache: NewBlockCache(cacheSize),
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

// InvalidateCache forces re-rendering of all cached content.
func (r *Renderer) InvalidateCache() {
	r.blockCache.InvalidateAll()
}

// HandleEvent processes a render event and returns any commands.
func (r *Renderer) HandleEvent(event RenderEvent) tea.Cmd {
	switch event.Type {
	case RenderEventStreamStart:
		r.streaming = NewStreamingBlock(r.width, r.markdownRenderer)
		return r.streaming.StartWaveAnimation()

	case RenderEventStreamText:
		if r.streaming != nil {
			r.streaming.AddText(event.Text)
		}
		return nil

	case RenderEventStreamToolStart:
		if r.streaming != nil {
			started := r.streaming.StartTool(event.ToolCallID, event.ToolName, event.ToolInfo)
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
			r.streaming.AddDiff(event.DiffPath, event.DiffOld, event.DiffNew, event.DiffLine)
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
// Key is based on message ID and width only - NOT index.
// This ensures cache hits even when new messages are added.
func (r *Renderer) blockCacheKey(msg *session.Message, index int) string {
	// Key includes message ID and width only
	// Using a simple format: "id:width"
	return strconv.FormatInt(msg.ID, 10) + ":" + strconv.Itoa(r.width)
}

// renderMessageBlock renders a single message to a block.
func (r *Renderer) renderMessageBlock(msg *session.Message, index int, messages []session.Message) *MessageBlock {
	rb := NewMessageBlockRendererWithContext(r.width, r.markdownRenderer, messages, index)
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
