package ui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	planpkg "github.com/samsaffron/term-llm/internal/plan"
	"github.com/samsaffron/term-llm/internal/terminaltext"
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
	var prompt string
	// Extract agent name from tool info (format: "agent_name") and prompt from raw args.
	if seg := FindSegmentByCallID(tracker, callID); seg != nil {
		if seg.ToolInfo != "" {
			agentName = seg.ToolInfo
		}
		agentNameFromArgs, promptFromArgs := extractSpawnAgentArgs(seg)
		if agentName == "" {
			agentName = agentNameFromArgs
		}
		prompt = promptFromArgs
	}
	p := subagentTracker.GetOrCreate(callID, agentName)

	// If p is nil, the subagent was already removed (late async event) - ignore it
	if p == nil {
		return
	}
	if p.Prompt == "" && prompt != "" {
		p.Prompt = prompt
	}

	// Update subagent state based on event type
	switch event.Type {
	case tools.SubagentEventInit:
		subagentTracker.HandleInit(callID, event.Provider, event.Model)
	case tools.SubagentEventText:
		subagentTracker.HandleTextDelta(callID, event.Text)
	case tools.SubagentEventToolStart:
		subagentTracker.HandleToolStart(callID, event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolArgs)
	case tools.SubagentEventToolEnd:
		subagentTracker.HandleToolEnd(callID, event.ToolCallID, event.ToolName, event.Success)
		// Process structured image/diff data from subagent events
		for _, imagePath := range event.Images {
			tracker.AddImageSegment(imagePath)
		}
		for _, d := range event.Diffs {
			addDiffToSpawnAgentSegment(tracker, callID, d.File, d.Old, d.New, d.Line, d.Operation)
		}
	case tools.SubagentEventPhase:
		subagentTracker.HandlePhase(callID, event.Phase)
	case tools.SubagentEventUsage:
		subagentTracker.HandleUsage(callID, event.InputTokens, event.OutputTokens)
	case tools.SubagentEventGuardian:
		if event.Guardian != nil {
			subagentTracker.HandleGuardianEvent(callID, *event.Guardian)
		}
	case tools.SubagentEventDone:
		subagentTracker.MarkDone(callID)
		// Store completion time so elapsed timer freezes
		if seg := FindSegmentByCallID(tracker, callID); seg != nil {
			if seg.SubagentEndTime.IsZero() {
				seg.SubagentEndTime = time.Now()
				tracker.RecordActivity()
				tracker.Version++
			}
		}
	}

	// Update the spawn_agent segment's stats for display. Preview snapshots only
	// need rebuilding for events that can change visible nested activity.
	refreshPreview := event.Type == tools.SubagentEventToolStart ||
		event.Type == tools.SubagentEventToolEnd ||
		event.Type == tools.SubagentEventGuardian ||
		(event.Type == tools.SubagentEventText && len(p.CompletedTools) == 0 && len(p.ActiveTools) == 0)
	updateSegmentFromSubagentProgress(tracker, callID, p, refreshPreview)
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

// defaultSubagentPreviewCalls is the number of most recent nested tool calls
// shown for each spawn_agent while tool details are collapsed.
const defaultSubagentPreviewCalls = 5

// UpdateSegmentFromSubagentProgress updates the spawn_agent segment stats from subagent progress.
// This syncs the subagent's stats (tool calls, tokens, provider/model) to the segment for display.
func UpdateSegmentFromSubagentProgress(tracker *ToolTracker, callID string, p *SubagentProgress) {
	updateSegmentFromSubagentProgress(tracker, callID, p, true)
}

func updateSegmentFromSubagentProgress(tracker *ToolTracker, callID string, p *SubagentProgress, refreshPreview bool) {
	if tracker == nil || p == nil {
		return
	}
	for i := range tracker.Segments {
		if tracker.Segments[i].ToolCallID == callID && tracker.Segments[i].ToolName == "spawn_agent" {
			seg := &tracker.Segments[i]
			preview := seg.SubagentPreview
			expandedPreview := seg.SubagentExpandedPreview
			previewTextOnly := seg.SubagentPreviewTextOnly
			if refreshPreview {
				preview = BuildSubagentPreview(p, defaultSubagentPreviewCalls)
				expandedPreview = BuildSubagentPreview(p, 0)
				previewTextOnly = len(preview) > 0 && len(p.CompletedTools) == 0 && len(p.ActiveTools) == 0
			}
			changed := !seg.SubagentHasProgress ||
				seg.SubagentToolCalls != p.ToolCalls ||
				seg.SubagentTotalTokens != p.InputTokens+p.OutputTokens ||
				seg.SubagentProvider != p.Provider ||
				seg.SubagentModel != p.Model ||
				seg.SubagentPrompt != p.Prompt ||
				seg.SubagentPreviewTextOnly != previewTextOnly ||
				!slices.Equal(seg.SubagentPreview, preview) ||
				!slices.Equal(seg.SubagentExpandedPreview, expandedPreview) ||
				!seg.SubagentStartTime.Equal(p.StartTime)

			seg.SubagentHasProgress = true
			seg.SubagentToolCalls = p.ToolCalls
			seg.SubagentTotalTokens = p.InputTokens + p.OutputTokens
			seg.SubagentProvider = p.Provider
			seg.SubagentModel = p.Model
			seg.SubagentPrompt = p.Prompt
			seg.SubagentPreview = preview
			seg.SubagentExpandedPreview = expandedPreview
			seg.SubagentPreviewTextOnly = previewTextOnly
			seg.SubagentStartTime = p.StartTime
			if changed {
				tracker.RecordActivity()
				tracker.Version++
			}
			break
		}
	}
}

// AttachSubagentProgressToSegment copies already-received subagent progress onto
// a newly visible spawn_agent tool segment. Progress can arrive through a TUI
// Program.Send before the stream ToolStart event has been processed, because the
// parent stream and subagent callback use independent channels.
func AttachSubagentProgressToSegment(tracker *ToolTracker, subagentTracker *SubagentTracker, callID string) {
	if tracker == nil || subagentTracker == nil || callID == "" {
		return
	}
	p := subagentTracker.Get(callID)
	if p == nil {
		return
	}
	if seg := FindSegmentByCallID(tracker, callID); seg != nil {
		agentName, prompt := extractSpawnAgentArgs(seg)
		if p.AgentName == "" {
			if seg.ToolInfo != "" {
				p.AgentName = seg.ToolInfo
			} else {
				p.AgentName = agentName
			}
		}
		if p.Prompt == "" && prompt != "" {
			p.Prompt = prompt
		}
	}
	UpdateSegmentFromSubagentProgress(tracker, callID, p)
}

// BuildSubagentPreview builds preview lines for a subagent in chronological order.
// Shows completed tools first (oldest first), then active tools (most recent).
// maxCalls limits the number of nested tool calls, not rendered lines. A guardian
// annotation remains grouped with its tool; annotations on older omitted calls
// are available in the unbounded expanded preview. A non-positive limit shows all calls.
func BuildSubagentPreview(p *SubagentProgress, maxCalls int) []string {
	if p == nil {
		return nil
	}

	type previewTool struct {
		tool   ToolSegment
		active bool
	}
	toolsToShow := make([]previewTool, 0, len(p.CompletedTools)+len(p.ActiveTools))
	for _, tool := range p.CompletedTools {
		toolsToShow = append(toolsToShow, previewTool{tool: tool})
	}
	for _, tool := range p.ActiveTools {
		toolsToShow = append(toolsToShow, previewTool{tool: tool, active: true})
	}
	if maxCalls > 0 && len(toolsToShow) > maxCalls {
		toolsToShow = toolsToShow[len(toolsToShow)-maxCalls:]
	}

	var preview []string
	for _, item := range toolsToShow {
		circle := WorkingCircle()
		if !item.active {
			if item.tool.Success {
				circle = SuccessCircle()
			} else {
				circle = ErrorCircle()
			}
		}
		if item.tool.Name == planpkg.ToolName && (item.active || item.tool.Success) {
			if planLines, ok := renderSubagentPlanPreview(item.tool.Args, item.active, circle, maxCalls); ok {
				preview = append(preview, planLines...)
				preview = append(preview, renderSubagentGuardian(item.tool.Guardian)...)
				continue
			}
		}
		line := circle + " " + terminaltext.SanitizeSingleLine(item.tool.Name)
		if item.tool.Info != "" {
			line += " " + terminaltext.SanitizeSingleLine(item.tool.Info)
		}
		preview = append(preview, line)
		preview = append(preview, renderSubagentGuardian(item.tool.Guardian)...)
	}

	// Text is a fallback only when the subagent has not emitted any tool calls.
	if len(toolsToShow) == 0 {
		for _, line := range p.GetPreviewLines() {
			line = terminaltext.SanitizeSingleLine(line)
			if line != "" {
				preview = append(preview, line)
			}
		}
		if maxCalls > 0 && len(preview) > maxCalls {
			preview = preview[len(preview)-maxCalls:]
		}
	}

	return preview
}

func renderSubagentPlanPreview(args json.RawMessage, active bool, circle string, maxCalls int) ([]string, bool) {
	snapshot, err := planpkg.Parse(args)
	if err != nil {
		return nil, false
	}
	if maxCalls > 0 {
		if len(snapshot.Plan) == 0 {
			label := "Plan cleared"
			if active {
				label = "Clearing plan…"
			}
			return []string{circle + " " + label}, true
		}
		summary := snapshot.Summary()
		label := "Plan updated"
		if active {
			label = "Updating plan…"
		}
		line := fmt.Sprintf("%s %s · %d/%d completed", circle, label, summary.Completed, summary.Total)
		if summary.CurrentStep != "" {
			line += " · " + terminaltext.SanitizeSingleLine(summary.CurrentStep)
		}
		return []string{line}, true
	}

	lines := strings.Split(terminaltext.Sanitize(snapshot.ChecklistText(active)), "\n")
	if len(lines) == 0 {
		return nil, false
	}
	lines[0] = circle + " " + lines[0]
	for i := 1; i < len(lines); i++ {
		if lines[i] != "" {
			lines[i] = "  " + lines[i]
		}
	}
	return lines, true
}

func renderSubagentGuardian(event *tools.GuardianEvent) []string {
	if event == nil || strings.TrimSpace(event.Message) == "" {
		return nil
	}
	message := strings.TrimSpace(terminaltext.SanitizeSingleLine(event.Message))
	message = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(message, "guardian:"), "Guardian:"))
	return []string{"  Guardian: " + message}
}

func extractSpawnAgentArgs(seg *Segment) (agentName, prompt string) {
	if seg == nil || len(seg.ToolArgs) == 0 {
		return "", ""
	}
	var args tools.SpawnAgentArgs
	if err := json.Unmarshal(seg.ToolArgs, &args); err != nil {
		return "", ""
	}
	return args.AgentName, strings.TrimSpace(args.Prompt)
}

// addDiffToSpawnAgentSegment adds a diff to the spawn_agent segment for display after the preview.
func addDiffToSpawnAgentSegment(tracker *ToolTracker, callID string, path, old, new string, line int, operation string) {
	if tracker == nil || path == "" {
		return
	}
	for i := range tracker.Segments {
		if tracker.Segments[i].ToolCallID == callID && tracker.Segments[i].ToolName == "spawn_agent" {
			// Deduplicate: check if this file is already in SubagentDiffs
			for _, d := range tracker.Segments[i].SubagentDiffs {
				if d.Path == path && d.Old == old && d.New == new && d.Line == line && d.Operation == operation {
					return
				}
			}
			tracker.Segments[i].SubagentDiffs = append(tracker.Segments[i].SubagentDiffs, SubagentDiff{
				Path:      path,
				Old:       old,
				New:       new,
				Line:      line,
				Operation: operation,
			})
			tracker.RecordActivity()
			tracker.Version++
			break
		}
	}
}
