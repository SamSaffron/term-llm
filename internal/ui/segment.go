package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/llm"
)

// SegmentType identifies the type of stream segment
type SegmentType int

const (
	SegmentText SegmentType = iota
	SegmentTool
	SegmentAskUserResult // For ask_user answers (plain text, styled at render time)
	SegmentImage         // For inline image display
	SegmentDiff          // For inline diff display from edit tool
)

// ToolStatus represents the execution state of a tool
type ToolStatus int

const (
	ToolPending ToolStatus = iota
	ToolSuccess
	ToolError
)

// Segment represents a discrete unit in the response stream (text or tool)
type Segment struct {
	Type       SegmentType
	Text       string     // For text segments: markdown content (finalized on completion)
	Rendered   string     // For text segments: cached rendered markdown
	ToolCallID string     // For tool segments: unique ID for this invocation
	ToolName   string     // For tool segments
	ToolInfo   string     // For tool segments: additional context
	ToolStatus ToolStatus // For tool segments
	Complete   bool       // For text segments: whether streaming is complete
	ImagePath  string     // For image segments: path to image file
	DiffPath   string     // For diff segments: file path
	DiffOld    string     // For diff segments: old content
	DiffNew    string     // For diff segments: new content
	Flushed    bool       // True if this segment has been printed to scrollback

	// Streaming text accumulation (O(1) append instead of O(n) string concat)
	TextBuilder *strings.Builder // Used during streaming; nil when Complete

	// Text snapshot cache - updated on append to avoid repeated String() calls
	TextSnapshot    string // Cached result of TextBuilder.String()
	TextSnapshotLen int    // Length when snapshot was taken (0 = invalid)

	// Incremental rendering cache (streaming optimization)
	SafePos      int    // Byte position of last safe markdown boundary
	SafeRendered string // Cached render of text[:SafePos]

	// Subagent stats (for spawn_agent tools only)
	SubagentToolCalls   int            // Number of tool calls made by subagent
	SubagentTotalTokens int            // Total tokens used by subagent
	SubagentHasProgress bool           // True if we have progress from this subagent
	SubagentProvider    string         // Provider name if different from parent
	SubagentModel       string         // Model name if different from parent
	SubagentPreview     []string       // Preview lines (active tools + last few text lines)
	SubagentStartTime   time.Time      // Start time for elapsed time display
	SubagentEndTime     time.Time      // When subagent completed (zero if still running)
	SubagentDiffs       []SubagentDiff // Diffs from subagent's edit_file calls
}

// SubagentDiff holds diff info from a subagent's edit_file call
type SubagentDiff struct {
	Path string
	Old  string
	New  string
}

// GetText returns the current text content of a segment.
// During streaming, it uses the cached TextSnapshot; after completion, it reads from Text.
// The snapshot is updated by AddTextSegment when content changes, avoiding
// repeated O(n) String() allocations on every render tick.
func (s *Segment) GetText() string {
	if s.TextBuilder != nil {
		// Return cached snapshot if valid (length matches current builder length)
		if s.TextSnapshotLen == s.TextBuilder.Len() {
			return s.TextSnapshot
		}
		// Snapshot is stale - update it (should rarely happen, AddTextSegment updates it)
		// IMPORTANT: Clone to prevent corruption from subsequent WriteString calls
		// (strings.Builder.String() shares memory with internal buffer)
		s.TextSnapshot = strings.Clone(s.TextBuilder.String())
		s.TextSnapshotLen = s.TextBuilder.Len()
		return s.TextSnapshot
	}
	return s.Text
}

// Tool status indicator colors using raw ANSI for reliable true color
const (
	pendingCircleANSI = "\033[38;5;245m\u25cb\033[0m"        // gray hollow circle
	workingCircleANSI = "\033[38;2;255;165;0m\u25cf\033[0m"  // orange filled circle for active tools
	successCircleANSI = "\033[38;2;79;185;101m\u25cf\033[0m" // #4FB965 green filled circle
	errorCircleANSI   = "\033[38;2;239;68;68m\u25cf\033[0m"  // #ef4444 red filled circle
)

// PendingCircle returns the pending status indicator
func PendingCircle() string { return pendingCircleANSI }

// WorkingCircle returns the working status indicator
func WorkingCircle() string { return workingCircleANSI }

// SuccessCircle returns the success status indicator
func SuccessCircle() string { return successCircleANSI }

// ErrorCircle returns the error status indicator
func ErrorCircle() string { return errorCircleANSI }

// Wave animation colors
var (
	waveDimColor = lipgloss.Color("245") // dim gray
)

// RenderWaveText renders text with a wave animation effect using bold highlighting.
// wavePos is the position of the bright "peak" traveling through the text.
// If wavePos < 0, we're in the pause phase - show all dim.
func RenderWaveText(text string, wavePos int) string {
	textRunes := []rune(text)
	textLen := len(textRunes)

	dimStyle := lipgloss.NewStyle().Foreground(waveDimColor)
	boldStyle := lipgloss.NewStyle().Bold(true)

	// During pause (wavePos < 0 or >= textLen), show all dim
	if wavePos < 0 || wavePos >= textLen {
		return dimStyle.Render(text)
	}

	// Build string with bold highlight at wave position
	var result strings.Builder

	// Dim text before wave
	if wavePos > 0 {
		result.WriteString(dimStyle.Render(string(textRunes[:wavePos])))
	}

	// Bold highlighted character(s) at wave position - highlight 2 chars for visibility
	highlightEnd := wavePos + 2
	if highlightEnd > textLen {
		highlightEnd = textLen
	}
	result.WriteString(boldStyle.Render(string(textRunes[wavePos:highlightEnd])))

	// Dim text after wave
	if highlightEnd < textLen {
		result.WriteString(dimStyle.Render(string(textRunes[highlightEnd:])))
	}

	return result.String()
}

// Muted style for tool params (lighter than wave dim)
var paramStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))

// RenderToolSegment renders a tool segment with its status indicator.
// For pending tools, wavePos controls the wave animation (-1 = paused/all dim).
// Tool name is rendered normally, params are rendered in slightly muted gray.
// For spawn_agent tools with progress, stats are shown instead of wave animation.
func RenderToolSegment(seg *Segment, wavePos int) string {
	switch seg.ToolStatus {
	case ToolPending:
		// spawn_agent tools with progress show stats instead of wave animation
		if seg.ToolName == "spawn_agent" && seg.SubagentHasProgress {
			return PendingCircle() + " " + renderSpawnAgentStats(seg.ToolInfo, seg.SubagentToolCalls, seg.SubagentTotalTokens, seg.SubagentStartTime, seg.SubagentEndTime, seg.SubagentProvider, seg.SubagentModel)
		}
		// Wave animation for other pending tools
		phase := FormatToolPhase(seg.ToolName, seg.ToolInfo)
		return PendingCircle() + " " + RenderWaveText(phase.Active, wavePos)
	case ToolSuccess:
		// spawn_agent shows final stats on success
		if seg.ToolName == "spawn_agent" && seg.SubagentHasProgress {
			return SuccessCircle() + " " + renderSpawnAgentStats(seg.ToolInfo, seg.SubagentToolCalls, seg.SubagentTotalTokens, seg.SubagentStartTime, seg.SubagentEndTime, seg.SubagentProvider, seg.SubagentModel)
		}
		// Tool name normal, params slightly muted (with space before info if present)
		if seg.ToolInfo != "" {
			return SuccessCircle() + " " + seg.ToolName + " " + paramStyle.Render(seg.ToolInfo)
		}
		return SuccessCircle() + " " + seg.ToolName
	case ToolError:
		// spawn_agent shows stats even on error
		if seg.ToolName == "spawn_agent" && seg.SubagentHasProgress {
			return ErrorCircle() + " " + renderSpawnAgentStats(seg.ToolInfo, seg.SubagentToolCalls, seg.SubagentTotalTokens, seg.SubagentStartTime, seg.SubagentEndTime, seg.SubagentProvider, seg.SubagentModel)
		}
		// Tool name normal, params slightly muted (with space before info if present)
		if seg.ToolInfo != "" {
			return ErrorCircle() + " " + seg.ToolName + " " + paramStyle.Render(seg.ToolInfo)
		}
		return ErrorCircle() + " " + seg.ToolName
	}
	return ""
}

// RenderToolCallFromPart renders a historical tool call from an llm.ToolCall.
// Uses success styling since historical calls have completed.
func RenderToolCallFromPart(tc *llm.ToolCall) string {
	if tc == nil {
		return ""
	}
	info := llm.ExtractToolInfo(*tc)
	if info != "" {
		return SuccessCircle() + " " + tc.Name + " " + paramStyle.Render(info)
	}
	return SuccessCircle() + " " + tc.Name
}

// renderSpawnAgentStats renders the stats line for a spawn_agent tool.
// Format: @agentName  N calls · X.Xk tokens · 12s [provider:model]
func renderSpawnAgentStats(agentName string, toolCalls, totalTokens int, startTime, endTime time.Time, provider, model string) string {
	var b strings.Builder

	// Extract just the agent name from toolInfo (format: "@name: prompt..." or "name")
	displayName := extractAgentName(agentName)
	b.WriteString("@")
	if displayName != "" {
		b.WriteString(displayName)
	} else {
		b.WriteString("agent")
	}
	b.WriteString("  ")
	b.WriteString(paramStyle.Render(formatSpawnAgentStats(toolCalls, totalTokens, startTime, endTime)))

	// Show provider:model if set (indicates different from parent)
	if provider != "" || model != "" {
		b.WriteString("  ")
		providerModel := formatProviderModel(provider, model)
		b.WriteString(paramStyle.Render(providerModel))
	}
	return b.String()
}

// extractAgentName extracts just the agent name from tool info.
// Input formats: "(@reviewer: prompt...)", "@reviewer: prompt...", "reviewer: prompt...", "reviewer"
func extractAgentName(toolInfo string) string {
	if toolInfo == "" {
		return ""
	}
	// Remove leading ( if present (from getToolPreview wrapping)
	name := strings.TrimPrefix(toolInfo, "(")
	// Remove leading @ if present
	name = strings.TrimPrefix(name, "@")
	// Take everything before : or first space
	if idx := strings.Index(name, ":"); idx > 0 {
		name = name[:idx]
	} else if idx := strings.Index(name, " "); idx > 0 {
		name = name[:idx]
	}
	return strings.TrimSpace(name)
}

// formatProviderModel formats provider and model for display.
// Returns formats like "anthropic:claude-sonnet" or just "openai" if no model.
func formatProviderModel(provider, model string) string {
	if provider == "" && model == "" {
		return ""
	}
	if model == "" {
		return "[" + provider + "]"
	}
	// Shorten common model names for display
	shortModel := shortenModelName(model)
	if provider == "" {
		return "[" + shortModel + "]"
	}
	return "[" + provider + ":" + shortModel + "]"
}

// shortenModelName shortens common model names for compact display.
func shortenModelName(model string) string {
	// Common shortenings
	replacements := map[string]string{
		"claude-sonnet-4-20250514":   "sonnet-4",
		"claude-opus-4-20250514":     "opus-4",
		"claude-3-5-sonnet-20241022": "sonnet-3.5",
		"claude-3-opus-20240229":     "opus-3",
		"gpt-4o":                     "4o",
		"gpt-4o-mini":                "4o-mini",
		"gpt-4-turbo":                "4-turbo",
		"gemini-2.0-flash":           "flash-2",
		"gemini-1.5-pro":             "pro-1.5",
	}
	if short, ok := replacements[model]; ok {
		return short
	}
	// If model is very long, truncate
	if len(model) > 20 {
		return model[:17] + "..."
	}
	return model
}

// formatSpawnAgentStats formats tool count, tokens, and elapsed time as "N calls · X.Xk tokens · 12s"
// If endTime is non-zero, elapsed time is frozen at endTime - startTime.
func formatSpawnAgentStats(toolCalls, totalTokens int, startTime, endTime time.Time) string {
	if toolCalls == 0 && totalTokens == 0 {
		if !startTime.IsZero() {
			elapsed := calcElapsed(startTime, endTime)
			return "starting… · " + formatElapsed(elapsed)
		}
		return "starting..."
	}
	calls := "calls"
	if toolCalls == 1 {
		calls = "call"
	}
	result := formatToolCount(toolCalls) + " " + calls + " · " + formatTokensCompact(totalTokens) + " tokens"
	if !startTime.IsZero() {
		elapsed := calcElapsed(startTime, endTime)
		result += " · " + formatElapsed(elapsed)
	}
	return result
}

// calcElapsed calculates elapsed time, freezing at endTime if set.
func calcElapsed(startTime, endTime time.Time) time.Duration {
	if !endTime.IsZero() {
		return endTime.Sub(startTime)
	}
	return time.Since(startTime)
}

// formatElapsed formats a duration in a compact human-readable form.
// Examples: 0s, 5s, 1m30s, 5m
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	if seconds == 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%dm%ds", minutes, seconds)
}

// formatToolCount formats a tool count
func formatToolCount(n int) string {
	if n < 1000 {
		return strings.TrimLeft(strings.Repeat(" ", 3)+strconv.Itoa(n), " ")
	}
	return strconv.Itoa(n)
}

// formatTokensCompact formats tokens in compact form (1.2k, 12k, etc.)
func formatTokensCompact(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	k := float64(n) / 1000
	if k < 10 {
		return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(k, 'f', 1, 64), "0"), ".") + "k"
	}
	return strconv.FormatFloat(k, 'f', 0, 64) + "k"
}

// renderAskUserResult renders an ask_user result with styling applied at render time.
// Input format: "Header: Value" or "Header: Value | Header2: Value2"
func renderAskUserResult(text string) string {
	checkStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	borderStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	valueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)

	var b strings.Builder
	b.WriteString(borderStyle.Render("│") + " ")
	b.WriteString(checkStyle.Render("✓") + " ")

	// Parse "Header: Value | Header2: Value2"
	parts := strings.Split(text, " | ")
	for i, part := range parts {
		if i > 0 {
			b.WriteString(" ")
		}
		if idx := strings.Index(part, ": "); idx != -1 {
			b.WriteString(labelStyle.Render(part[:idx+2]))
			b.WriteString(valueStyle.Render(part[idx+2:]))
		} else {
			b.WriteString(part)
		}
	}
	return b.String() + "\n"
}

// RenderSegments renders a list of segments with proper spacing.
// This is the main entry point for rendering the stream content.
// renderMarkdown should be a function that renders markdown content.
// includeImages controls whether image segments are rendered - should be false
// for View() (called constantly) and true for scrollback flush (one-time).
// Accepts []*Segment so mutations (like SafeRendered caching) persist to originals.
func RenderSegments(segments []*Segment, width int, wavePos int, renderMarkdown func(string, int) string, includeImages bool) string {
	var b strings.Builder

	for i, seg := range segments {
		if i > 0 {
			prev := segments[i-1]
			prevIsTool := prev.Type == SegmentTool || prev.Type == SegmentAskUserResult
			currIsTool := seg.Type == SegmentTool || seg.Type == SegmentAskUserResult
			// Blank line between text and tools/ask_user results
			if prev.Type == SegmentText && currIsTool {
				b.WriteString("\n\n")
			}
			// Single newline between consecutive tools/ask_user results
			if prevIsTool && currIsTool {
				b.WriteString("\n")
			}
			// Blank line between tools/ask_user results and text
			if prevIsTool && seg.Type == SegmentText {
				b.WriteString("\n\n")
			}
			// Blank line between tools/ask_user results and images
			if prevIsTool && seg.Type == SegmentImage {
				b.WriteString("\n\n")
			}
			// Blank line between text and images
			if prev.Type == SegmentText && seg.Type == SegmentImage {
				b.WriteString("\n\n")
			}
			// Blank line between tools/ask_user results and diffs
			if prevIsTool && seg.Type == SegmentDiff {
				b.WriteString("\n\n")
			}
			// Blank line between text and diffs
			if prev.Type == SegmentText && seg.Type == SegmentDiff {
				b.WriteString("\n\n")
			}
			// Blank line between diffs and text
			if prev.Type == SegmentDiff && seg.Type == SegmentText {
				b.WriteString("\n\n")
			}
			// Blank line between diffs and tools
			if prev.Type == SegmentDiff && currIsTool {
				b.WriteString("\n\n")
			}
		}

		switch seg.Type {
		case SegmentText:
			if seg.Complete && seg.Rendered != "" {
				// Completed segment with cached render
				b.WriteString(seg.Rendered)
			} else {
				// Get text from TextBuilder (streaming) or Text (completed)
				text := seg.GetText()
				if text == "" {
					break
				}

				if !seg.Complete {
					// STREAMING: Skip expensive Glamour rendering entirely.
					// Use plain text with only safe boundary caching for prefix.
					if seg.SafePos > 0 && seg.SafeRendered != "" {
						// Write cached rendered prefix
						b.WriteString(seg.SafeRendered)
						// Write remaining text as plain (no Glamour)
						if seg.SafePos < len(text) {
							b.WriteString(text[seg.SafePos:])
						}
					} else if seg.SafePos > 0 && renderMarkdown != nil {
						// First time seeing this safe boundary - render and cache it
						seg.SafeRendered = renderMarkdown(text[:seg.SafePos], width)
						b.WriteString(seg.SafeRendered)
						if seg.SafePos < len(text) {
							b.WriteString(text[seg.SafePos:])
						}
					} else {
						// No safe boundary yet - just output plain text
						b.WriteString(text)
					}
				} else if renderMarkdown != nil {
					// COMPLETED but no cached Rendered: render with Glamour
					b.WriteString(renderMarkdown(text, width))
				} else {
					b.WriteString(text)
				}
			}
		case SegmentTool:
			b.WriteString(RenderToolSegment(seg, wavePos))
			// Render subagent preview lines beneath spawn_agent tools
			if seg.ToolName == "spawn_agent" && len(seg.SubagentPreview) > 0 {
				for _, line := range seg.SubagentPreview {
					b.WriteString("\n  │ ")
					b.WriteString(line)
				}
			}
			// Render subagent diffs after preview (still within spawn_agent block)
			if seg.ToolName == "spawn_agent" && len(seg.SubagentDiffs) > 0 {
				for _, diff := range seg.SubagentDiffs {
					b.WriteString("\n")
					if rendered := RenderDiffSegment(diff.Path, diff.Old, diff.New, width); rendered != "" {
						b.WriteString(rendered)
					}
				}
			}
		case SegmentAskUserResult:
			b.WriteString(renderAskUserResult(seg.Text))
		case SegmentImage:
			// Render images inline when includeImages is true
			if includeImages {
				if rendered := RenderInlineImage(seg.ImagePath); rendered != "" {
					b.WriteString(rendered)
					b.WriteString("\r\n")
				}
			}
		case SegmentDiff:
			// Render diffs inline (always render, not gated like images)
			if rendered := RenderDiffSegment(seg.DiffPath, seg.DiffOld, seg.DiffNew, width); rendered != "" {
				b.WriteString(rendered)
			}
		}
	}

	return b.String()
}

// HasPendingTool returns true if any segment has a pending tool
func HasPendingTool(segments []Segment) bool {
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i].Type == SegmentTool && segments[i].ToolStatus == ToolPending {
			return true
		}
	}
	return false
}

// GetPendingToolTextLen returns the text length of the first pending tool (for wave animation)
func GetPendingToolTextLen(segments []Segment) int {
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i].Type == SegmentTool && segments[i].ToolStatus == ToolPending {
			phase := FormatToolPhase(segments[i].ToolName, segments[i].ToolInfo)
			return len([]rune(phase.Active))
		}
	}
	return 0
}

// UpdateToolStatus updates the status of a pending tool matching the given call ID.
// Matching by unique ID ensures we update the correct tool when multiple calls
// to the same tool may be in progress.
func UpdateToolStatus(segments []Segment, callID string, success bool) []Segment {
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i].Type == SegmentTool &&
			segments[i].ToolStatus == ToolPending &&
			segments[i].ToolCallID == callID {
			if success {
				segments[i].ToolStatus = ToolSuccess
			} else {
				segments[i].ToolStatus = ToolError
			}
			break
		}
	}
	return segments
}
