package ui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

// maxTextBufferBytes is the maximum size of a subagent's text buffer.
// Once exceeded, new text is only added to previewLines, not the full buffer.
// This prevents unbounded memory growth from verbose subagents.
const (
	maxTextBufferBytes              = 64 * 1024 // 64KB
	maxSubagentPreviewLineBytes     = 4 * 1024  // Bounds newline-free verbose output retained for display.
	defaultSubagentTextPreviewLines = 4         // Independent of the nested tool-call preview limit.
)

// SubagentUsageCall preserves one child provider request for request-scoped pricing.
type SubagentUsageCall struct {
	Provider string
	Model    string
	Usage    llm.Usage
	Guardian bool
}

// SubagentProgress tracks progress from a single spawned subagent.
type SubagentProgress struct {
	ToolCallID     string          // Links to parent's spawn_agent call
	AgentName      string          // e.g., "reviewer"
	Prompt         string          // Task/question passed to the subagent
	TextBuffer     strings.Builder // Text output (capped at maxTextBufferBytes)
	ActiveTools    []ToolSegment   // Currently running tools
	CompletedTools []ToolSegment   // Completed tools (for expanded view)
	Phase          string          // "Thinking", "Searching"
	StartTime      time.Time
	EndTime        time.Time
	Done           bool

	// Provider/model info used for accounting. Provider/Model remain the
	// display-only values and are blank when they match the parent.
	ResolvedProvider string
	ResolvedModel    string
	Provider         string
	Model            string

	// Stats retained after completion for /stats.
	ToolCalls         int
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int
	CacheWriteTokens  int
	UsageCalls        []SubagentUsageCall
	ToolTime          time.Duration
	toolIntervalStart time.Time

	// For preview: last N lines of text
	previewLines    []string
	pendingGuardian map[string]tools.GuardianEvent // Guardian events received before nested tool start

	bufferTruncated bool // true if TextBuffer hit the cap and stopped growing
}

// ToolSegment represents a tool's execution state in a subagent.
type ToolSegment struct {
	CallID   string
	Name     string
	Info     string
	Args     json.RawMessage
	Guardian *tools.GuardianEvent
	Success  bool
	Done     bool
}

// SubagentTracker tracks progress from multiple concurrent subagents.
type SubagentTracker struct {
	mu           sync.RWMutex
	agents       map[string]*SubagentProgress // live agents by ToolCallID
	completed    map[string]*SubagentProgress // retained accounting after inline removal
	removed      map[string]struct{}          // tombstones for removed agents (prevents resurrection)
	previewLines int                          // preview mode line count (default 4)
	expanded     bool                         // true = show ALL content

	// Main agent's provider/model for comparison (only show subagent's if different)
	mainProvider string
	mainModel    string
}

// NewSubagentTracker creates a new tracker with default settings.
func NewSubagentTracker() *SubagentTracker {
	return &SubagentTracker{
		agents:       make(map[string]*SubagentProgress),
		completed:    make(map[string]*SubagentProgress),
		removed:      make(map[string]struct{}),
		previewLines: defaultSubagentTextPreviewLines,
		expanded:     false,
	}
}

// SetMainProviderModel sets the main agent's provider and model for comparison.
// Subagent provider/model will only be displayed if different from main.
func (t *SubagentTracker) SetMainProviderModel(provider, model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.mainProvider = provider
	t.mainModel = model
}

// ToggleExpanded switches between preview (4 lines) and full content.
func (t *SubagentTracker) ToggleExpanded() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expanded = !t.expanded
}

// IsExpanded returns whether expanded mode is active.
func (t *SubagentTracker) IsExpanded() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.expanded
}

// GetOrCreate returns the progress tracker for a subagent, creating if needed.
// Returns nil if the subagent has already been removed (tombstone exists).
func (t *SubagentTracker) GetOrCreate(callID, agentName string) *SubagentProgress {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Check tombstone first - don't resurrect removed agents
	if _, removed := t.removed[callID]; removed {
		return nil
	}
	if p, ok := t.agents[callID]; ok {
		return p
	}
	p := &SubagentProgress{
		ToolCallID:      callID,
		AgentName:       agentName,
		StartTime:       time.Now(),
		pendingGuardian: make(map[string]tools.GuardianEvent),
	}
	t.agents[callID] = p
	return p
}

// Get returns the progress tracker for a subagent, or nil if not found.
func (t *SubagentTracker) Get(callID string) *SubagentProgress {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.agents[callID]
}

// MarkDone marks a subagent as completed using the local clock.
func (t *SubagentTracker) MarkDone(callID string) { t.MarkDoneAt(callID, time.Now()) }

// MarkDoneAt marks a subagent complete, freezes elapsed time, and closes any
// outstanding nested-tool interval.
func (t *SubagentTracker) MarkDoneAt(callID string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if at.IsZero() {
		at = time.Now()
	}
	if p := t.agents[callID]; p != nil {
		if p.Done {
			return
		}
		p.Done = true
		p.EndTime = at
		p.closeToolInterval(at)
	}
}

// Remove removes a subagent from live tracking after spawn_agent completes.
// Its compact accounting snapshot remains available for session statistics.
func (t *SubagentTracker) Remove(callID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, exists := t.agents[callID]; exists {
		if p.EndTime.IsZero() {
			p.EndTime = time.Now()
			p.closeToolInterval(p.EndTime)
		}
		p.Done = true
		snapshot := *p
		snapshot.TextBuffer = strings.Builder{}
		snapshot.ActiveTools = nil
		snapshot.CompletedTools = nil
		snapshot.previewLines = nil
		snapshot.pendingGuardian = nil
		t.completed[callID] = &snapshot
		delete(t.agents, callID)
		t.removed[callID] = struct{}{} // tombstone prevents resurrection
	}
}

// Snapshots returns stable copies of all live and completed runs. Large prose
// buffers and previews are intentionally omitted; this API is for accounting.
func (t *SubagentTracker) Snapshots() []SubagentProgress {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]SubagentProgress, 0, len(t.agents)+len(t.completed))
	appendSnapshot := func(p *SubagentProgress) {
		if p == nil {
			return
		}
		copy := *p
		copy.TextBuffer = strings.Builder{}
		copy.ActiveTools = nil
		copy.CompletedTools = nil
		copy.UsageCalls = append([]SubagentUsageCall(nil), p.UsageCalls...)
		copy.previewLines = nil
		copy.pendingGuardian = nil
		result = append(result, copy)
	}
	for _, p := range t.agents {
		appendSnapshot(p)
	}
	for _, p := range t.completed {
		appendSnapshot(p)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].StartTime.Equal(result[j].StartTime) {
			return result[i].ToolCallID < result[j].ToolCallID
		}
		return result[i].StartTime.Before(result[j].StartTime)
	})
	return result
}

// ActiveAgents returns all non-done subagents in order of start time.
func (t *SubagentTracker) ActiveAgents() []*SubagentProgress {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var active []*SubagentProgress
	for _, p := range t.agents {
		if !p.Done {
			active = append(active, p)
		}
	}
	return active
}

// HasActive returns true if there are any active (non-done) subagents.
func (t *SubagentTracker) HasActive() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, p := range t.agents {
		if !p.Done {
			return true
		}
	}
	return false
}

// HandleTextDelta appends text to a subagent's buffer.
// The full buffer is capped at maxTextBufferBytes to prevent unbounded memory growth.
// Preview lines are always updated regardless of buffer cap.
func (t *SubagentTracker) HandleTextDelta(callID, text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.agents[callID]
	if p == nil {
		return
	}
	// Only add to full buffer if under the cap
	if !p.bufferTruncated {
		if p.TextBuffer.Len()+len(text) <= maxTextBufferBytes {
			p.TextBuffer.WriteString(text)
		} else {
			// Write partial to fill up to cap, then stop
			remaining := maxTextBufferBytes - p.TextBuffer.Len()
			if remaining > 0 {
				p.TextBuffer.WriteString(text[:remaining])
			}
			p.bufferTruncated = true
		}
	}
	// Always update preview lines (these are bounded by maxLines)
	p.updatePreviewLines(text, defaultSubagentTextPreviewLines)
}

// HandleToolStart records a tool starting in a subagent.
func (t *SubagentTracker) HandleToolStart(callID, nestedCallID, toolName, toolInfo string, args json.RawMessage) {
	t.HandleToolStartAt(callID, nestedCallID, toolName, toolInfo, args, time.Now())
}

// HandleToolStartAt records a nested tool start and begins union tool timing.
func (t *SubagentTracker) HandleToolStartAt(callID, nestedCallID, toolName, toolInfo string, args json.RawMessage, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.agents[callID]
	if p == nil {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	if len(p.ActiveTools) == 0 {
		p.toolIntervalStart = at
	}
	tool := ToolSegment{
		CallID: nestedCallID,
		Name:   toolName,
		Info:   toolInfo,
		Args:   args,
	}
	if event, ok := p.pendingGuardian[nestedCallID]; ok {
		tool.Guardian = &event
		delete(p.pendingGuardian, nestedCallID)
	}
	p.ActiveTools = append(p.ActiveTools, tool)
	p.ToolCalls++
}

// HandleToolEnd marks a tool as completed in a subagent.
func (t *SubagentTracker) HandleToolEnd(callID, nestedCallID, toolName string, success bool) {
	t.HandleToolEndAt(callID, nestedCallID, toolName, success, time.Now())
}

// HandleToolEndAt records a nested tool completion and closes union timing when idle.
func (t *SubagentTracker) HandleToolEndAt(callID, nestedCallID, toolName string, success bool, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.agents[callID]
	if p == nil {
		return
	}
	// Find and remove from active, add to completed
	for i, tool := range p.ActiveTools {
		if (nestedCallID != "" && tool.CallID == nestedCallID) || (nestedCallID == "" && tool.Name == toolName) {
			tool.Success = success
			tool.Done = true
			p.CompletedTools = append(p.CompletedTools, tool)
			p.ActiveTools = append(p.ActiveTools[:i], p.ActiveTools[i+1:]...)
			if len(p.ActiveTools) == 0 {
				p.closeToolInterval(at)
			}
			break
		}
	}
}

// HandleGuardianEvent attaches a guardian review to the exact nested tool.
func (t *SubagentTracker) HandleGuardianEvent(callID string, event tools.GuardianEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.agents[callID]
	if p == nil {
		if completed := t.completed[callID]; completed != nil {
			completed.addUsage(event.Model, "", event.Usage, true)
		}
		return
	}
	if !event.Usage.BillableCountersZero() {
		p.addUsage(event.Model, "", event.Usage, true)
	}
	if event.ToolCallID == "" {
		return
	}
	for i := range p.ActiveTools {
		if p.ActiveTools[i].CallID == event.ToolCallID {
			p.ActiveTools[i].Guardian = &event
			return
		}
	}
	for i := range p.CompletedTools {
		if p.CompletedTools[i].CallID == event.ToolCallID {
			p.CompletedTools[i].Guardian = &event
			return
		}
	}
	if p.pendingGuardian == nil {
		p.pendingGuardian = make(map[string]tools.GuardianEvent)
	}
	p.pendingGuardian[event.ToolCallID] = event
}

// HandlePhase updates the phase of a subagent.
func (t *SubagentTracker) HandlePhase(callID, phase string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.agents[callID]
	if p == nil {
		return
	}
	p.Phase = phase
}

// HandleUsage accumulates token usage for a subagent.
func (t *SubagentTracker) HandleUsage(callID string, inputTokens, outputTokens int) {
	t.HandleUsageEvent(callID, tools.SubagentEvent{InputTokens: inputTokens, OutputTokens: outputTokens})
}

// HandleInit sets the provider and model for a subagent.
// Only stores values if they differ from the main agent's provider/model.
func (t *SubagentTracker) HandleInit(callID, provider, model string) {
	t.HandleInitAt(callID, provider, model, time.Time{})
}

// HandleInitAt captures the child start timestamp when provided.
func (t *SubagentTracker) HandleInitAt(callID, provider, model string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.agents[callID]
	if p == nil {
		return
	}
	if !at.IsZero() {
		p.StartTime = at
	}
	p.ResolvedProvider, p.ResolvedModel = provider, model
	// Only store if different from main agent
	if provider != t.mainProvider || model != t.mainModel {
		p.Provider = provider
		p.Model = model
	}
}

// GetPreviewLines returns the current preview lines for external access.
func (p *SubagentProgress) GetPreviewLines() []string {
	return p.previewLines
}

// updatePreviewLines keeps the last N lines of text for preview.
func (p *SubagentProgress) updatePreviewLines(newText string, maxLines int) {
	// Add new lines
	for line := range strings.SplitSeq(newText, "\n") {
		if line != "" || len(p.previewLines) > 0 {
			p.previewLines = append(p.previewLines, boundSubagentPreviewLine(line))
		}
	}
	// Keep only last N lines
	if len(p.previewLines) > maxLines {
		p.previewLines = p.previewLines[len(p.previewLines)-maxLines:]
	}
}

func boundSubagentPreviewLine(line string) string {
	if len(line) <= maxSubagentPreviewLineBytes {
		return line
	}
	const prefix = "..."
	start := len(line) - (maxSubagentPreviewLineBytes - len(prefix))
	for start < len(line) && !utf8.RuneStart(line[start]) {
		start++
	}
	return prefix + line[start:]
}

// RenderHeader returns "@name  N calls · X.Xk tokens [expanded]".
func (p *SubagentProgress) RenderHeader(expanded bool) string {
	var b strings.Builder
	b.WriteString("@")
	b.WriteString(p.AgentName)
	b.WriteString("  ")
	b.WriteString(fmt.Sprintf("%d calls", p.ToolCalls))
	b.WriteString(" · ")
	b.WriteString(formatTokens(p.InputTokens + p.OutputTokens + p.CachedInputTokens + p.CacheWriteTokens))
	b.WriteString(" tokens")
	if expanded {
		b.WriteString("  [expanded]")
	}
	return b.String()
}

// Render returns preview (last N lines) or full content based on expanded flag.
func (p *SubagentProgress) Render(expanded bool, maxPreviewLines int) string {
	var b strings.Builder

	// Active tools (always shown)
	for _, tool := range p.ActiveTools {
		b.WriteString("  │ ")
		b.WriteString(WorkingCircle())
		b.WriteString(" ")
		b.WriteString(tool.Name)
		if tool.Info != "" {
			b.WriteString(" ")
			b.WriteString(tool.Info)
		}
		b.WriteString("\n")
	}

	if expanded {
		// Completed tools (only in expanded)
		for _, tool := range p.CompletedTools {
			b.WriteString("  │ ")
			if tool.Success {
				b.WriteString(SuccessCircle())
			} else {
				b.WriteString(ErrorCircle())
			}
			b.WriteString(" ")
			b.WriteString(tool.Name)
			if tool.Info != "" {
				b.WriteString(" ")
				b.WriteString(tool.Info)
			}
			b.WriteString("\n")
		}
		// Full text content
		if p.TextBuffer.Len() > 0 {
			b.WriteString("  │\n")
			for line := range strings.SplitSeq(p.TextBuffer.String(), "\n") {
				b.WriteString("  │ ")
				b.WriteString(line)
				b.WriteString("\n")
			}
			if p.bufferTruncated {
				b.WriteString("  │ ... (output truncated)\n")
			}
		}
	} else {
		// Preview: last N lines only
		if len(p.previewLines) > 0 {
			lines := p.previewLines
			if len(lines) > maxPreviewLines {
				lines = lines[len(lines)-maxPreviewLines:]
			}
			for _, line := range lines {
				if line != "" {
					b.WriteString("  │ ")
					b.WriteString(line)
					b.WriteString("\n")
				}
			}
		}
	}

	return b.String()
}

// RenderSubagentProgress renders progress for a single subagent.
// This is used when displaying inline beneath a spawn_agent tool indicator.
func RenderSubagentProgress(p *SubagentProgress, expanded bool) string {
	if p == nil {
		return ""
	}
	return p.Render(expanded, defaultSubagentTextPreviewLines)
}

// formatTokens formats a token count in compact form: 1, 999, 1.5k, 12.3k
func formatTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	k := float64(n) / 1000
	if k < 10 {
		return fmt.Sprintf("%.1fk", k)
	}
	return fmt.Sprintf("%.0fk", k)
}

// HandleUsageEvent retains full per-request usage, including cache counters.
func (t *SubagentTracker) HandleUsageEvent(callID string, event tools.SubagentEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.agents[callID]
	if p == nil {
		p = t.completed[callID]
	}
	if p == nil {
		return
	}
	p.addUsage(event.Model, event.Provider, llm.Usage{InputTokens: event.InputTokens, OutputTokens: event.OutputTokens, CachedInputTokens: event.CachedInputTokens, CacheWriteTokens: event.CacheWriteTokens}, false)
}
func (p *SubagentProgress) addUsage(model, provider string, u llm.Usage, guardian bool) {
	if u.BillableCountersZero() {
		return
	}
	if model == "" && !guardian {
		model = p.ResolvedModel
	}
	if provider == "" {
		provider = p.ResolvedProvider
	}
	p.UsageCalls = append(p.UsageCalls, SubagentUsageCall{Model: model, Provider: provider, Usage: u, Guardian: guardian})
	p.InputTokens += u.InputTokens
	p.OutputTokens += u.OutputTokens
	p.CachedInputTokens += u.CachedInputTokens
	p.CacheWriteTokens += u.CacheWriteTokens
}
func (p *SubagentProgress) closeToolInterval(at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	if !p.toolIntervalStart.IsZero() {
		p.ToolTime += max(time.Duration(0), at.Sub(p.toolIntervalStart))
		p.toolIntervalStart = time.Time{}
	}
}

// Timing returns wall elapsed and union nested-tool time; parallel tools count once.
func (p SubagentProgress) Timing(now time.Time) (time.Duration, time.Duration) {
	end := p.EndTime
	if end.IsZero() {
		end = now
	}
	elapsed := max(time.Duration(0), end.Sub(p.StartTime))
	tools := p.ToolTime
	if !p.toolIntervalStart.IsZero() {
		tools += max(time.Duration(0), end.Sub(p.toolIntervalStart))
	}
	return elapsed, min(elapsed, tools)
}

// ResolvedModel reads one run's model without copying the retained accounting history.
func (t *SubagentTracker) ResolvedModel(callID string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if p := t.agents[callID]; p != nil {
		return p.ResolvedModel
	}
	if p := t.completed[callID]; p != nil {
		return p.ResolvedModel
	}
	return ""
}
