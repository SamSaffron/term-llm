package chat

import (
	"github.com/term-llm/term-llm/internal/session"
)

// VirtualViewport handles virtualized rendering of message history.
// Instead of rendering all messages, it calculates which messages
// are visible in the viewport and only renders those.
type VirtualViewport struct {
	width  int
	height int

	// Estimated heights for messages we haven't rendered yet
	defaultUserMsgHeight      int
	defaultAssistantMsgHeight int
}

// NewVirtualViewport creates a new virtual viewport.
func NewVirtualViewport(width, height int) *VirtualViewport {
	return &VirtualViewport{
		width:  width,
		height: height,
		// Conservative estimates for unrendered messages
		defaultUserMsgHeight:      3,  // Typically 1-3 lines
		defaultAssistantMsgHeight: 10, // Typically longer
	}
}

// GetVisibleRange returns the start and end indices (exclusive) of messages
// that should be rendered for the current viewport.
//
// scrollOffset is the number of messages scrolled up from the bottom.
// Returns (start, end) where messages[start:end] should be rendered.
func (v *VirtualViewport) GetVisibleRange(messages []session.Message, scrollOffset int) (int, int) {
	total := len(messages)
	if total == 0 {
		return 0, 0
	}

	// For now, use a simple heuristic: render enough messages to fill the viewport
	// plus a small buffer for smooth scrolling.
	//
	// In a fully virtualized implementation, we'd track actual heights of rendered
	// blocks and use binary search to find the visible range. For now, we estimate
	// based on typical message heights.

	// Calculate how many messages fit in the viewport first
	avgMsgHeight := (v.defaultUserMsgHeight + v.defaultAssistantMsgHeight) / 2
	if avgMsgHeight < 1 {
		avgMsgHeight = 1
	}

	// Messages that fit + buffer for partial visibility
	messagesNeeded := (v.height / avgMsgHeight) + 4

	// Calculate end index based on scroll offset
	endIdx := total - scrollOffset
	if endIdx > total {
		endIdx = total
	}
	// Minimum endIdx is messagesNeeded (or total if fewer messages)
	// This ensures we can fill the viewport when scrolled to top
	minEnd := messagesNeeded
	if minEnd > total {
		minEnd = total
	}
	if endIdx < minEnd {
		endIdx = minEnd
	}

	startIdx := endIdx - messagesNeeded
	if startIdx < 0 {
		startIdx = 0
	}

	return startIdx, endIdx
}

// GetVisibleRangeWithHeights returns the visible range using actual rendered heights.
// This is more accurate but requires heights to be known.
func (v *VirtualViewport) GetVisibleRangeWithHeights(messages []session.Message, heights []int, scrollOffset int) (int, int) {
	total := len(messages)
	if total == 0 || len(heights) != total {
		// Fall back to estimate-based range if heights aren't available
		return v.GetVisibleRange(messages, scrollOffset)
	}

	// First, calculate how many messages we need to fill the viewport
	// by walking backwards from the end to estimate minimum messages needed
	messagesNeeded := 0
	accHeight := 0
	for i := total - 1; i >= 0 && accHeight < v.height+10; i-- {
		accHeight += heights[i]
		messagesNeeded++
	}
	if messagesNeeded < 1 {
		messagesNeeded = 1
	}

	// Calculate end index based on scroll offset
	endIdx := total - scrollOffset
	if endIdx > total {
		endIdx = total
	}
	// Minimum endIdx ensures we can fill the viewport when scrolled to top
	minEnd := messagesNeeded
	if minEnd > total {
		minEnd = total
	}
	if endIdx < minEnd {
		endIdx = minEnd
	}

	// Walk backwards from end, accumulating heights until we fill the viewport
	accumulatedHeight := 0
	startIdx := endIdx

	for i := endIdx - 1; i >= 0 && accumulatedHeight < v.height+10; i-- {
		accumulatedHeight += heights[i]
		startIdx = i
	}

	return startIdx, endIdx
}

// EstimateMessageHeight gives a rough estimate of how many lines a message will take.
// This is used for initial viewport calculations before actual rendering.
func (v *VirtualViewport) EstimateMessageHeight(msg *session.Message) int {
	// Simple heuristic based on content length and role
	contentLen := len(msg.TextContent)

	// Estimate characters per line (accounting for markdown overhead)
	charsPerLine := v.width - 4 // Leave margin for formatting
	if charsPerLine < 20 {
		charsPerLine = 20
	}

	// Estimate lines from content
	estimatedLines := contentLen / charsPerLine
	if estimatedLines < 1 {
		estimatedLines = 1
	}

	// Add overhead for role header, spacing, code blocks, etc.
	switch msg.Role {
	case "user":
		return estimatedLines + 2 // Prompt + blank line
	case "assistant":
		// Assistant messages often have more formatting (code blocks, lists, etc.)
		return estimatedLines + 3
	default:
		return estimatedLines + 1
	}
}

// CalculateTotalHeight calculates the total height of all messages.
// This is used for scroll calculations.
func (v *VirtualViewport) CalculateTotalHeight(heights []int) int {
	total := 0
	for _, h := range heights {
		total += h
	}
	return total
}

// ViewportInfo contains information about the current viewport state.
type ViewportInfo struct {
	StartMessage int  // First visible message index
	EndMessage   int  // Last visible message index (exclusive)
	TotalHeight  int  // Total height of all messages
	VisibleStart int  // First visible line (from top of all content)
	VisibleEnd   int  // Last visible line (from top of all content)
	AtTop        bool // True if showing the first message
	AtBottom     bool // True if showing the last message
}

// GetViewportInfo returns detailed information about the current viewport.
func (v *VirtualViewport) GetViewportInfo(messages []session.Message, heights []int, scrollOffset int) ViewportInfo {
	start, end := v.GetVisibleRangeWithHeights(messages, heights, scrollOffset)

	totalHeight := v.CalculateTotalHeight(heights)

	// Calculate visible line range
	visibleStart := 0
	for i := 0; i < start && i < len(heights); i++ {
		visibleStart += heights[i]
	}
	visibleEnd := visibleStart
	for i := start; i < end && i < len(heights); i++ {
		visibleEnd += heights[i]
	}

	return ViewportInfo{
		StartMessage: start,
		EndMessage:   end,
		TotalHeight:  totalHeight,
		VisibleStart: visibleStart,
		VisibleEnd:   visibleEnd,
		AtTop:        start == 0,
		AtBottom:     end >= len(messages),
	}
}
