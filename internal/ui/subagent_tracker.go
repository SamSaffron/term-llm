package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// maxTextBufferBytes is the maximum size of a subagent's text buffer.
// Once exceeded, new text is only added to previewLines, not the full buffer.
// This prevents unbounded memory growth from verbose subagents.
const maxTextBufferBytes = 64 * 1024 // 64KB

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
	Done           bool

	// Provider/model info (for displaying when different from parent)
	Provider string // Provider name (e.g., "anthropic", "openai")
	Model    string // Model name (e.g., "claude-sonnet-4-20250514")

	// Stats for header display
	ToolCalls    int // Total tool calls made
	InputTokens  int // Total input tokens
	OutputTokens int // Total output tokens

	// For preview: last N lines of text
	previewLines    []string
	bufferTruncated bool // true if TextBuffer hit the cap and stopped growing
}

// ToolSegment represents a tool's execution state in a subagent.
type ToolSegment struct {
	Name    string
	Info    string
	Success bool
	Done    bool
}

// SubagentTracker tracks progress from multiple concurrent subagents.
type SubagentTracker struct {
	mu           sync.RWMutex
	agents       map[string]*SubagentProgress // by ToolCallID
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
		removed:      make(map[string]struct{}),
		previewLines: 4,
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
		ToolCallID: callID,
		AgentName:  agentName,
		StartTime:  time.Now(),
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

// MarkDone marks a subagent as completed.
func (t *SubagentTracker) MarkDone(callID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := t.agents[callID]; p != nil {
		p.Done = true
	}
}

// Remove removes a subagent from tracking (after spawn_agent completes).
// A tombstone is added to prevent late async events from resurrecting the entry.
// Only adds a tombstone if the callID was actually being tracked, to avoid
// unbounded tombstone growth from non-spawn_agent tool calls.
func (t *SubagentTracker) Remove(callID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.agents[callID]; exists {
		delete(t.agents, callID)
		t.removed[callID] = struct{}{} // tombstone prevents resurrection
	}
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
	p.updatePreviewLines(text, 4)
}

// HandleToolStart records a tool starting in a subagent.
func (t *SubagentTracker) HandleToolStart(callID, toolName, toolInfo string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.agents[callID]
	if p == nil {
		return
	}
	p.ActiveTools = append(p.ActiveTools, ToolSegment{
		Name: toolName,
		Info: toolInfo,
	})
	p.ToolCalls++
}

// HandleToolEnd marks a tool as completed in a subagent.
func (t *SubagentTracker) HandleToolEnd(callID, toolName string, success bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.agents[callID]
	if p == nil {
		return
	}
	// Find and remove from active, add to completed
	for i, tool := range p.ActiveTools {
		if tool.Name == toolName {
			tool.Success = success
			tool.Done = true
			p.CompletedTools = append(p.CompletedTools, tool)
			p.ActiveTools = append(p.ActiveTools[:i], p.ActiveTools[i+1:]...)
			break
		}
	}
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
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.agents[callID]
	if p == nil {
		return
	}
	p.InputTokens += inputTokens
	p.OutputTokens += outputTokens
}

// HandleInit sets the provider and model for a subagent.
// Only stores values if they differ from the main agent's provider/model.
func (t *SubagentTracker) HandleInit(callID, provider, model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.agents[callID]
	if p == nil {
		return
	}
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
			p.previewLines = append(p.previewLines, line)
		}
	}
	// Keep only last N lines
	if len(p.previewLines) > maxLines {
		p.previewLines = p.previewLines[len(p.previewLines)-maxLines:]
	}
}

// RenderHeader returns "@name  N calls · X.Xk tokens [expanded]".
func (p *SubagentProgress) RenderHeader(expanded bool) string {
	var b strings.Builder
	b.WriteString("@")
	b.WriteString(p.AgentName)
	b.WriteString("  ")
	b.WriteString(fmt.Sprintf("%d calls", p.ToolCalls))
	b.WriteString(" · ")
	b.WriteString(formatTokens(p.InputTokens + p.OutputTokens))
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
	return p.Render(expanded, 4)
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
