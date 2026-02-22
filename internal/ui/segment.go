package ui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui/ansisafe"
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
	Type         SegmentType
	Text         string          // For text segments: markdown content (finalized on completion)
	Rendered     string          // For text segments: cached rendered markdown
	ToolCallID   string          // For tool segments: unique ID for this invocation
	ToolName     string          // For tool segments
	ToolInfo     string          // For tool segments: additional context
	ToolArgs     json.RawMessage // Raw args JSON, stored for expanded rendering
	ToolStatus   ToolStatus      // For tool segments
	Complete     bool            // For text segments: whether streaming is complete
	ImagePath    string          // For image segments: path to image file
	DiffPath     string          // For diff segments: file path
	DiffOld      string          // For diff segments: old content
	DiffNew      string          // For diff segments: new content
	DiffLine     int             // For diff segments: 1-indexed starting line (0 = unknown)
	DiffRendered string          // For diff segments: cached rendered output
	DiffWidth    int             // For diff segments: width when rendered (for cache invalidation)
	Flushed      bool            // True if this segment has been printed to scrollback

	// Streaming text accumulation (O(1) append instead of O(n) string concat)
	TextBuilder *strings.Builder // Used during streaming; nil when Complete

	// Text snapshot cache - updated on append to avoid repeated String() calls
	TextSnapshot    string // Cached result of TextBuilder.String()
	TextSnapshotLen int    // Length when snapshot was taken (0 = invalid)

	// Streaming markdown renderer - renders complete blocks as they arrive
	StreamRenderer *TextSegmentRenderer // Active streaming renderer; nil when Complete

	// Incremental rendering cache (streaming optimization)
	SafePos            int    // Byte position of last safe markdown boundary
	SafeRendered       string // Cached render of text[:SafePos]
	FlushedPos         int    // Byte position up to which content has been flushed to scrollback
	FlushedRenderedPos int    // Number of rendered bytes already flushed to scrollback

	// Stitched rendering state (for correct inter-chunk spacing)
	LastFlushedRaw         string // Raw markdown of last flushed chunk (for stitched rendering)
	LastFlushedRenderedLen int    // Byte length of rendered LastFlushedRaw

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
	Path     string
	Old      string
	New      string
	Line     int    // 1-indexed starting line (0 = unknown)
	Rendered string // Cached rendered output
	Width    int    // Width when rendered (for cache invalidation)
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

// safeANSISlice returns s[pos:] but adjusts pos forward if it would split an ANSI CSI sequence.
// ANSI CSI sequences: ESC '[' <params> <terminator> where terminator is 0x40-0x7E
// Only CSI sequences (ESC '[') are handled; other escape sequences (OSC, etc.) are not adjusted.
func safeANSISlice(s string, pos int) string {
	return ansisafe.SuffixString(s, pos)
}

// renderPendingMarkdownPreview renders in-progress markdown tail content.
// Keep this raw so words appear immediately before the block is complete.
func renderPendingMarkdownPreview(pending string) string {
	return pending
}

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

// truncateToolInfo truncates raw tool info text so the full rendered line
// (circle + space + toolName + space + info) fits within width.
// Truncation is done on the raw text before styling to avoid breaking ANSI codes.
func truncateToolInfo(toolName, info string, width int) string {
	if width <= 0 || info == "" {
		return info
	}
	// 2 = circle (1 visible char) + space; 1 = space between name and info
	overhead := 2 + len(toolName) + 1
	maxInfoLen := width - overhead
	if maxInfoLen <= 0 {
		return ""
	}
	runes := []rune(info)
	if len(runes) <= maxInfoLen {
		return info
	}
	if maxInfoLen <= 3 {
		return string(runes[:maxInfoLen])
	}
	return string(runes[:maxInfoLen-3]) + "..."
}

func truncateShellDescription(desc string) string {
	runes := []rune(desc)
	if len(runes) > 100 {
		return string(runes[:97]) + "..."
	}
	return desc
}

func buildExpandedShellInfo(args tools.ShellArgs) string {
	if args.Command == "" {
		return ""
	}
	var b strings.Builder
	if args.Description != "" {
		desc := truncateShellDescription(args.Description)
		b.WriteString(desc)
		b.WriteString("\n")
	}
	b.WriteString(args.Command)
	if len(args.Env) > 0 {
		keys := make([]string, 0, len(args.Env))
		for key := range args.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			b.WriteString("\n  ")
			b.WriteString(key)
			b.WriteString("=")
			b.WriteString(args.Env[key])
		}
	}
	return b.String()
}

// RenderToolSegment renders a tool segment with its status indicator.
// For pending tools, wavePos controls the wave animation (-1 = paused/all dim).
// Tool name is rendered normally, params are rendered in slightly muted gray.
// For spawn_agent tools with progress, stats are shown instead of wave animation.
// width is the terminal width used to truncate long lines (0 = no truncation).
func RenderToolSegment(seg *Segment, wavePos int, width int, expanded bool) string {
	info := seg.ToolInfo
	expandedShellInfo := ""
	if expanded && seg.ToolName == "shell" && len(seg.ToolArgs) > 0 {
		var args tools.ShellArgs
		if err := json.Unmarshal(seg.ToolArgs, &args); err == nil {
			expandedShellInfo = buildExpandedShellInfo(args)
			if expandedShellInfo != "" {
				info = expandedShellInfo
			}
		}
	}
	renderWidth := width
	if expandedShellInfo != "" {
		renderWidth = 0
	}

	switch seg.ToolStatus {
	case ToolPending:
		// spawn_agent tools with progress show stats instead of wave animation
		if seg.ToolName == "spawn_agent" && seg.SubagentHasProgress {
			return PendingCircle() + " " + renderSpawnAgentStats(seg.ToolInfo, seg.SubagentToolCalls, seg.SubagentTotalTokens, seg.SubagentStartTime, seg.SubagentEndTime, seg.SubagentProvider, seg.SubagentModel)
		}
		// Wave animation for other pending tools
		activeText := ""
		if expandedShellInfo != "" {
			if info != "" {
				activeText = seg.ToolName + " " + info
			} else {
				activeText = seg.ToolName
			}
		} else {
			phase := FormatToolPhase(seg.ToolName, info)
			activeText = phase.Active
		}
		if renderWidth > 0 {
			// Truncate the plain text before wave styling (overhead: circle + space = 2)
			maxLen := renderWidth - 2
			if maxLen > 0 {
				runes := []rune(activeText)
				if len(runes) > maxLen {
					if maxLen <= 3 {
						activeText = string(runes[:maxLen])
					} else {
						activeText = string(runes[:maxLen-3]) + "..."
					}
				}
			}
		}
		return PendingCircle() + " " + RenderWaveText(activeText, wavePos)
	case ToolSuccess:
		// spawn_agent shows final stats on success
		if seg.ToolName == "spawn_agent" && seg.SubagentHasProgress {
			return SuccessCircle() + " " + renderSpawnAgentStats(seg.ToolInfo, seg.SubagentToolCalls, seg.SubagentTotalTokens, seg.SubagentStartTime, seg.SubagentEndTime, seg.SubagentProvider, seg.SubagentModel)
		}
		// Tool name normal, params slightly muted (with space before info if present)
		info = truncateToolInfo(seg.ToolName, info, renderWidth)
		if info != "" {
			return SuccessCircle() + " " + seg.ToolName + " " + paramStyle.Render(info)
		}
		return SuccessCircle() + " " + seg.ToolName
	case ToolError:
		// spawn_agent shows stats even on error
		if seg.ToolName == "spawn_agent" && seg.SubagentHasProgress {
			return ErrorCircle() + " " + renderSpawnAgentStats(seg.ToolInfo, seg.SubagentToolCalls, seg.SubagentTotalTokens, seg.SubagentStartTime, seg.SubagentEndTime, seg.SubagentProvider, seg.SubagentModel)
		}
		// Tool name normal, params slightly muted (with space before info if present)
		info = truncateToolInfo(seg.ToolName, info, renderWidth)
		if info != "" {
			return ErrorCircle() + " " + seg.ToolName + " " + paramStyle.Render(info)
		}
		return ErrorCircle() + " " + seg.ToolName
	}
	return ""
}

// RenderToolCallFromPart renders a historical tool call from an llm.ToolCall.
// Uses success styling since historical calls have completed.
// width is the terminal width used to truncate long lines (0 = no truncation).
func RenderToolCallFromPart(tc *llm.ToolCall, width int, expanded bool) string {
	if tc == nil {
		return ""
	}

	if expanded && tc.Name == "shell" && len(tc.Arguments) > 0 {
		var args tools.ShellArgs
		if err := json.Unmarshal(tc.Arguments, &args); err == nil {
			info := buildExpandedShellInfo(args)
			if info != "" {
				return SuccessCircle() + " " + tc.Name + " " + paramStyle.Render(info)
			}
		}
	}

	info := truncateToolInfo(tc.Name, llm.ExtractToolInfo(*tc), width)
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

// SegmentSeparator returns the vertical spacing (newlines) required between two segments.
func SegmentSeparator(prevType, currType SegmentType) string {
	// No extra spacing between consecutive text segments
	// (each segment already ends with \n from rendering)
	if prevType == SegmentText && currType == SegmentText {
		return ""
	}

	// Keep one blank line between a tool row and following prose content.
	if prevType == SegmentTool && currType == SegmentText {
		return "\n\n"
	}

	// Keep all other boundaries compact and consistent.
	return "\n"
}

// RenderSegments renders a list of segments with proper spacing.
func RenderSegments(segments []*Segment, width int, wavePos int, renderMarkdown func(string, int) string, includeImages bool, expanded bool) string {
	return RenderSegmentsWithLeading(nil, segments, width, wavePos, renderMarkdown, includeImages, expanded)
}

// RenderSegmentsWithLeading renders a list of segments, optionally using a leading segment for initial spacing.
func RenderSegmentsWithLeading(leading *Segment, segments []*Segment, width int, wavePos int, renderMarkdown func(string, int) string, includeImages bool, expanded bool) string {
	var b strings.Builder
	lastType := SegmentText
	hasPrev := false
	if leading != nil {
		lastType = leading.Type
		hasPrev = true
	}

	for _, seg := range segments {
		var rendered string
		switch seg.Type {
		case SegmentText:
			text := seg.GetText()
			if text == "" {
				continue
			}

			if seg.Complete && seg.Rendered != "" {
				rendered = seg.Rendered
			} else if seg.StreamRenderer != nil {
				rendered = seg.StreamRenderer.RenderedUnflushed()

				// Show pending in-progress text for word-by-word feedback, except
				// tables/lists which are withheld until content becomes stable.
				pending := seg.StreamRenderer.PendingMarkdown()
				if pending != "" && !seg.StreamRenderer.PendingIsTable() && !seg.StreamRenderer.PendingIsList() {
					rendered += renderPendingMarkdownPreview(pending)
				}
			} else if seg.Complete && renderMarkdown != nil {
				rendered = renderMarkdown(text, width)
			} else {
				rendered = text
			}

			// If this is a completed segment that was partially flushed,
			// only show the unflushed part. Use safeANSISlice to avoid slicing
			// mid-escape-sequence which would produce broken ANSI output.
			if seg.Complete && seg.FlushedRenderedPos > 0 && seg.FlushedRenderedPos < len(rendered) {
				rendered = safeANSISlice(rendered, seg.FlushedRenderedPos)
			} else if seg.Complete && seg.FlushedRenderedPos >= len(rendered) {
				rendered = ""
			}

		case SegmentTool:
			rendered = RenderToolSegment(seg, wavePos, width, expanded)
			// Render subagent preview lines beneath spawn_agent tools
			if seg.ToolName == "spawn_agent" && len(seg.SubagentPreview) > 0 {
				var sb strings.Builder
				sb.WriteString(rendered)
				for _, line := range seg.SubagentPreview {
					sb.WriteString("\n  │ ")
					sb.WriteString(line)
				}
				rendered = sb.String()
			}
			// Render subagent diffs after preview (still within spawn_agent block)
			if seg.ToolName == "spawn_agent" && len(seg.SubagentDiffs) > 0 {
				var sb strings.Builder
				sb.WriteString(rendered)
				for i := range seg.SubagentDiffs {
					diff := &seg.SubagentDiffs[i]
					sb.WriteString("\n")
					if diff.Rendered != "" && diff.Width == width {
						sb.WriteString(diff.Rendered)
					} else if d := RenderDiffSegment(diff.Path, diff.Old, diff.New, width, diff.Line); d != "" {
						diff.Rendered = d
						diff.Width = width
						sb.WriteString(d)
					}
				}
				rendered = sb.String()
			}
		case SegmentAskUserResult:
			rendered = renderAskUserResult(seg.Text)
		case SegmentImage:
			if includeImages {
				if r := RenderInlineImage(seg.ImagePath); r != "" {
					rendered = r + "\r\n"
				}
			}
		case SegmentDiff:
			if seg.DiffRendered != "" && seg.DiffWidth == width {
				rendered = seg.DiffRendered
			} else if r := RenderDiffSegment(seg.DiffPath, seg.DiffOld, seg.DiffNew, width, seg.DiffLine); r != "" {
				seg.DiffRendered = r
				seg.DiffWidth = width
				rendered = r
			}
		}

		if rendered != "" {
			if hasPrev {
				b.WriteString(SegmentSeparator(lastType, seg.Type))
			}
			b.WriteString(rendered)
			lastType = seg.Type
			hasPrev = true
		}
	}

	return b.String()
}

// RenderImagesAndDiffs renders only image and diff segments from a list.
// This is used for alt screen mode to preserve images/diffs after streaming ends,
// since text content is already stored in message history.
func RenderImagesAndDiffs(segments []*Segment, width int) string {
	var b strings.Builder
	first := true

	for _, seg := range segments {
		switch seg.Type {
		case SegmentImage:
			if !first {
				b.WriteString("\n")
			}
			if rendered := RenderInlineImage(seg.ImagePath); rendered != "" {
				b.WriteString(rendered)
				b.WriteString("\r\n")
				first = false
			}
		case SegmentDiff:
			if !first {
				b.WriteString("\n")
			}
			if seg.DiffRendered != "" && seg.DiffWidth == width {
				b.WriteString(seg.DiffRendered)
				first = false
			} else if rendered := RenderDiffSegment(seg.DiffPath, seg.DiffOld, seg.DiffNew, width, seg.DiffLine); rendered != "" {
				seg.DiffRendered = rendered
				seg.DiffWidth = width
				b.WriteString(rendered)
				first = false
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
