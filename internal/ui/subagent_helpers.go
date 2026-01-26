package ui

import (
	"time"

	"github.com/samsaffron/term-llm/internal/tools"
)

// HandleSubagentProgress processes subagent events and updates both the subagent tracker
// and the corresponding spawn_agent segment's stats. This is shared logic used by both
// the ask command (streaming mode) and the chat TUI.
//
// tracker: the ToolTracker containing segments
// subagentTracker: the SubagentTracker for subagent progress
// callID: the tool call ID of the spawn_agent invocation
// event: the subagent event to process
func HandleSubagentProgress(tracker *ToolTracker, subagentTracker *SubagentTracker, callID string, event tools.SubagentEvent) {
	if tracker == nil || subagentTracker == nil {
		return
	}

	// Get or create subagent progress entry
	var agentName string
	// Extract agent name from tool info (format: "agent_name")
	if seg := FindSegmentByCallID(tracker, callID); seg != nil && seg.ToolInfo != "" {
		agentName = seg.ToolInfo
	}
	p := subagentTracker.GetOrCreate(callID, agentName)

	// If p is nil, the subagent was already removed (late async event) - ignore it
	if p == nil {
		return
	}

	// Update subagent state based on event type
	switch event.Type {
	case tools.SubagentEventInit:
		subagentTracker.HandleInit(callID, event.Provider, event.Model)
	case tools.SubagentEventText:
		subagentTracker.HandleTextDelta(callID, event.Text)
	case tools.SubagentEventToolStart:
		subagentTracker.HandleToolStart(callID, event.ToolName, event.ToolInfo)
	case tools.SubagentEventToolEnd:
		subagentTracker.HandleToolEnd(callID, event.ToolName, event.Success)
		// Parse markers from tool output (images, diffs)
		if event.ToolOutput != "" {
			// Images still go to main tracker (they're standalone)
			for _, imagePath := range parseImageMarkers(event.ToolOutput) {
				tracker.AddImageSegment(imagePath)
			}
			// Diffs go to the spawn_agent segment itself (so they render within the subagent block)
			if event.ToolName == tools.EditFileToolName {
				for _, d := range parseDiffMarkers(event.ToolOutput) {
					addDiffToSpawnAgentSegment(tracker, callID, d.File, d.Old, d.New)
				}
			}
		}
	case tools.SubagentEventPhase:
		subagentTracker.HandlePhase(callID, event.Phase)
	case tools.SubagentEventUsage:
		subagentTracker.HandleUsage(callID, event.InputTokens, event.OutputTokens)
	case tools.SubagentEventDone:
		subagentTracker.MarkDone(callID)
		// Store completion time so elapsed timer freezes
		if seg := FindSegmentByCallID(tracker, callID); seg != nil {
			seg.SubagentEndTime = time.Now()
		}
	}

	// Update the spawn_agent segment's stats for display
	UpdateSegmentFromSubagentProgress(tracker, callID, p)
}

// FindSegmentByCallID finds a segment by its tool call ID.
// Returns nil if not found.
func FindSegmentByCallID(tracker *ToolTracker, callID string) *Segment {
	if tracker == nil {
		return nil
	}
	for i := range tracker.Segments {
		if tracker.Segments[i].ToolCallID == callID {
			return &tracker.Segments[i]
		}
	}
	return nil
}

// UpdateSegmentFromSubagentProgress updates the spawn_agent segment stats from subagent progress.
// This syncs the subagent's stats (tool calls, tokens, provider/model) to the segment for display.
func UpdateSegmentFromSubagentProgress(tracker *ToolTracker, callID string, p *SubagentProgress) {
	if tracker == nil || p == nil {
		return
	}
	for i := range tracker.Segments {
		if tracker.Segments[i].ToolCallID == callID && tracker.Segments[i].ToolName == "spawn_agent" {
			tracker.Segments[i].SubagentHasProgress = true
			tracker.Segments[i].SubagentToolCalls = p.ToolCalls
			tracker.Segments[i].SubagentTotalTokens = p.InputTokens + p.OutputTokens
			tracker.Segments[i].SubagentProvider = p.Provider
			tracker.Segments[i].SubagentModel = p.Model
			tracker.Segments[i].SubagentPreview = BuildSubagentPreview(p, 4)
			tracker.Segments[i].SubagentStartTime = p.StartTime
			break
		}
	}
}

// BuildSubagentPreview builds preview lines for a subagent in chronological order.
// Shows completed tools first (oldest first), then active tools (most recent).
// maxLines is the total number of lines to show.
func BuildSubagentPreview(p *SubagentProgress, maxLines int) []string {
	if p == nil {
		return nil
	}

	var preview []string

	// 1. Completed tools first (chronological order - oldest first)
	for _, tool := range p.CompletedTools {
		var circle string
		if tool.Success {
			circle = SuccessCircle()
		} else {
			circle = ErrorCircle()
		}
		line := circle + " " + tool.Name
		if tool.Info != "" {
			line += " " + tool.Info
		}
		preview = append(preview, line)
	}

	// 2. Active tools after (they are the most recent)
	for _, tool := range p.ActiveTools {
		line := WorkingCircle() + " " + tool.Name
		if tool.Info != "" {
			line += " " + tool.Info
		}
		preview = append(preview, line)
	}

	// 3. Text lines only if no tools shown
	if len(preview) == 0 {
		textLines := p.GetPreviewLines()
		if len(textLines) > 0 {
			for _, line := range textLines {
				if line != "" {
					preview = append(preview, line)
				}
			}
		}
	}

	// Limit to maxLines (keep the LAST N lines - most recent)
	if len(preview) > maxLines {
		preview = preview[len(preview)-maxLines:]
	}

	return preview
}

// addDiffToSpawnAgentSegment adds a diff to the spawn_agent segment for display after the preview.
func addDiffToSpawnAgentSegment(tracker *ToolTracker, callID string, path, old, new string) {
	if tracker == nil || path == "" {
		return
	}
	for i := range tracker.Segments {
		if tracker.Segments[i].ToolCallID == callID && tracker.Segments[i].ToolName == "spawn_agent" {
			tracker.Segments[i].SubagentDiffs = append(tracker.Segments[i].SubagentDiffs, SubagentDiff{
				Path: path,
				Old:  old,
				New:  new,
			})
			break
		}
	}
}
