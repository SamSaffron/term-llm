package plan

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func (m *Model) triggerPlanner() (tea.Model, tea.Cmd) {
	return m.triggerPlannerWithPrompt("")
}

func (m *Model) triggerPlannerWithPrompt(userInstruction string) (tea.Model, tea.Cmd) {
	// Sync document from editor
	m.syncDocFromEditor()

	// Take a snapshot before agent starts
	m.lastAgentSnap = m.doc.Snapshot()

	// Set up agent state
	m.agentActive = true
	m.agentStreaming = true
	m.agentPhase = "Thinking"
	m.agentError = nil
	m.stats = ui.NewSessionStats()
	m.streamStartTime = time.Now()
	m.agentReasoningTail = ""
	m.agentLastReasoningLn = ""
	m.deferredEditEvents = nil
	m.activityExpanded = true
	m.currentTurn = 0
	// Keep editor focused - user can continue editing during agent operation

	// Create context for cancellation
	ctx, cancel := context.WithCancel(context.Background())
	m.streamCancel = cancel

	// Build request
	req := m.buildPlannerRequest(userInstruction)

	// Note: ask_user handling is set up in SetProgram() once the program reference is available

	// Stream the request
	stream, err := m.engine.Stream(ctx, req)
	if err != nil {
		m.agentActive = false
		m.agentStreaming = false
		m.agentError = err
		m.setStatus(fmt.Sprintf("Failed to start agent: %v", err))
		return m, nil
	}

	// Create plan stream adapter with inline edit parsing
	adapter := ui.NewPlanStreamAdapter(ui.DefaultStreamBufferSize)
	go adapter.ProcessStream(ctx, stream)
	m.streamChan = adapter.Events()

	return m, tea.Batch(
		m.listenForStreamEvents(),
		m.spinner.Tick,
		m.tickEvery(),
	)
}

func (m *Model) buildPlannerRequest(userInstruction string) llm.Request {
	// Build context with document state
	docContent := m.doc.Text()
	var userChanges string
	if m.lastAgentSnap.Version > 0 {
		userChanges = m.doc.SummarizeChanges(m.lastAgentSnap)
	}

	// Build system prompt
	systemPrompt := `You are an expert software architect and planning assistant. Your role is to help the user develop comprehensive, actionable implementation plans.

The user is editing a plan document. Your job is to transform rough ideas into detailed, well-structured plans that can be directly executed.

## Investigation Tools

You have access to tools to explore the codebase:
- glob: Find files by pattern (e.g., "**/*.go", "src/**/*.ts")
- grep: Search file contents for patterns
- read_file: Read file contents
- web_search: Search the web for current information
- read_url: Fetch and read web pages
- shell: Run shell commands for git, npm, etc.

**IMPORTANT**: Before making edits, use these tools to understand:
- Existing code patterns and conventions
- Related implementations to reference
- Dependencies and integration points
- Test patterns used in the codebase

## Document Editing with Inline Markers

To edit the document, use inline XML markers directly in your response. These are parsed in real-time for instant feedback.

**INSERT** - Add content after a line matching the anchor text:
<INSERT after="anchor text to match">
line 1
line 2
</INSERT>

If 'after' is omitted, content is appended at the end:
<INSERT>
new content at end
</INSERT>

**DELETE** - Remove a single line or range:
<DELETE from="text of line to remove" />
<DELETE from="start line" to="end line" />

**CRITICAL**: Always INSERT new content first, then DELETE old content. This preserves context for subsequent edits.

## Plan Structure Requirements

Every plan section should address:

### 1. Task Breakdown
- Break features into small, independently testable steps
- Each step should be completable in isolation
- Order steps by dependencies (prerequisites first)
- Include specific file paths and function names

### 2. Edge Cases & Error Handling
- What inputs could break this?
- What external failures could occur? (network, disk, permissions)
- How should errors propagate or be handled?
- What validation is needed at boundaries?

### 3. Testing Strategy
- Unit tests: What functions need direct testing?
- Integration tests: What interactions need verification?
- Edge case tests: What boundary conditions to cover?
- Reference existing test patterns in the codebase

### 4. Dependencies & Prerequisites
- What existing code does this build on?
- What packages or tools are needed?
- What must be completed before each step?
- Are there database migrations or config changes?

### 5. Security Considerations
- Input validation requirements
- Authentication/authorization impacts
- Sensitive data handling
- Potential injection vectors

### 6. Performance Implications
- Will this affect hot paths?
- Database query impacts
- Memory/CPU considerations for large inputs
- Caching opportunities or requirements

### 7. Rollback & Migration
- Can this be deployed incrementally?
- What's the rollback procedure if issues arise?
- Are there breaking changes to handle?
- Data migration steps if applicable

### 8. Acceptance Criteria
- Concrete conditions that define "done"
- Measurable outcomes where possible
- User-facing behavior expectations

## Editing Guidelines
- Use fuzzy text matching - partial matches work (e.g., after="## Overview" matches "## Overview Section")
- INSERT content appears immediately as it streams
- Multiple edits in one response are processed sequentially
- Preserve the user's intent and phrasing where possible
- Add structure (headers, bullets, numbered lists) to make the plan clearer
- If something is unclear, ask the user using ask_user tool
- Reference specific files and line numbers when adding implementation details
- Be thorough but avoid unnecessary padding - every line should add value`

	// Append project instructions (AGENTS.md, CLAUDE.md, etc.) if found
	if projectInstructions := agents.DiscoverProjectInstructions(); projectInstructions != "" {
		systemPrompt += "\n\n---\n\n" + projectInstructions
	}

	// Build user message with document state
	var userMsg strings.Builder
	userMsg.WriteString("Current document:\n```\n")
	if docContent == "" {
		userMsg.WriteString("(empty document)")
	} else {
		userMsg.WriteString(docContent)
		if !strings.HasSuffix(docContent, "\n") {
			userMsg.WriteString("\n")
		}
	}
	userMsg.WriteString("```\n")

	if userChanges != "" && userChanges != "No changes" {
		fmt.Fprintf(&userMsg, "\nUser made changes since your last edit: %s\n", userChanges)
	}

	if userInstruction != "" {
		fmt.Fprintf(&userMsg, "\n**User instruction**: %s\n", userInstruction)
		userMsg.WriteString("\nPlease follow the user's instruction above. Use INSERT and DELETE markers to make targeted edits.")
	} else {
		userMsg.WriteString("\nPlease help improve and structure this plan. Use INSERT and DELETE markers to make targeted edits.")
	}

	// Save user message for history
	m.lastUserMessage = userMsg.String()

	// Build messages with system prompt first
	messages := []llm.Message{
		llm.SystemText(systemPrompt),
	}

	// Add conversation history for context
	messages = append(messages, m.history...)

	// Add current user message
	messages = append(messages, llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: userMsg.String()},
		},
	})

	return llm.Request{
		Model:               m.modelName,
		Messages:            messages,
		MaxTurns:            m.maxTurns,
		Tools:               m.engine.Tools().AllSpecs(),
		Search:              m.search,
		ForceExternalSearch: m.forceExternalSearch,
	}
}

// executePartialInsert handles a streaming partial insert - a single line as it arrives.
func (m *Model) executePartialInsert(afterText string, line string) {
	// Check if this is a new INSERT block (different anchor or first time)
	if m.partialInsertIdx < 0 || m.partialInsertAfter != afterText {
		// New INSERT block - find the anchor point
		lines := m.doc.Lines()
		lineTexts := make([]string, len(lines))
		for i, l := range lines {
			lineTexts[i] = l.Content
		}

		// Find insertion point using fuzzy matching
		insertAfterIdx := len(lines) - 1 // Default: append at end
		if afterText != "" {
			matchIdx := tools.FindBestMatch(lineTexts, afterText)
			if matchIdx >= 0 {
				insertAfterIdx = matchIdx
			}
		} else if len(lines) == 0 {
			insertAfterIdx = -1 // Empty doc, insert at beginning
		}

		// Set up tracking for this INSERT block
		m.partialInsertAfter = afterText
		m.partialInsertIdx = insertAfterIdx
	}

	// Insert the line at the tracked position
	m.doc.InsertLine(m.partialInsertIdx, line, "agent")
	m.partialInsertIdx++ // Next line goes after the one we just inserted
}

// executeInlineInsert handles an inline INSERT edit from the stream.
func (m *Model) executeInlineInsert(afterText string, content []string) {
	if len(content) == 0 {
		return
	}

	// Get current lines for fuzzy matching
	lines := m.doc.Lines()
	lineTexts := make([]string, len(lines))
	for i, line := range lines {
		lineTexts[i] = line.Content
	}

	// Find insertion point using fuzzy matching
	insertAfterIdx := len(lines) - 1 // Default: append at end
	if afterText != "" {
		matchIdx := tools.FindBestMatch(lineTexts, afterText)
		if matchIdx >= 0 {
			insertAfterIdx = matchIdx
		}
	} else if len(lines) == 0 {
		insertAfterIdx = -1 // Empty doc, insert at beginning
	}

	// Insert each line and sync editor after each for streaming effect
	for _, line := range content {
		m.doc.InsertLine(insertAfterIdx, line, "agent")
		insertAfterIdx++ // Next line goes after the one we just inserted
		m.syncEditorFromDoc()
	}
}

// executeInlineDelete handles an inline DELETE edit from the stream.
func (m *Model) executeInlineDelete(fromText string, toText string, syncEditor bool) {
	if fromText == "" {
		return
	}

	// Get current lines for fuzzy matching
	lines := m.doc.Lines()
	lineTexts := make([]string, len(lines))
	for i, line := range lines {
		lineTexts[i] = line.Content
	}

	// Find start line using fuzzy matching
	startIdx := tools.FindBestMatch(lineTexts, fromText)
	if startIdx < 0 {
		return // Line not found
	}

	// Determine end index
	endIdx := startIdx // Default: single line delete
	if toText != "" {
		// Find end line for range delete
		endMatchIdx := tools.FindBestMatch(lineTexts, toText)
		if endMatchIdx >= 0 && endMatchIdx >= startIdx {
			endIdx = endMatchIdx
		}
	}

	// Delete lines from end to start (to avoid index shifting issues)
	for i := endIdx; i >= startIdx; i-- {
		m.doc.DeleteLine(i)
	}

	if syncEditor {
		// Sync editor to show the change immediately.
		m.syncEditorFromDoc()
	}
}

func (m *Model) deferStreamEdit(ev ui.StreamEvent) {
	m.deferredEditEvents = append(m.deferredEditEvents, ev)
}

func (m *Model) flushDeferredStreamEdits() {
	if len(m.deferredEditEvents) == 0 {
		return
	}

	events := m.deferredEditEvents
	m.deferredEditEvents = nil

	for _, deferred := range events {
		switch deferred.Type {
		case ui.StreamEventPartialInsert:
			m.executePartialInsert(deferred.InlineAfter, deferred.InlineLine)
		case ui.StreamEventInlineInsert:
			// Partial inserts in this deferred batch were already replayed above; reset tracking.
			m.partialInsertIdx = -1
			m.partialInsertAfter = ""
		case ui.StreamEventInlineDelete:
			m.executeInlineDelete(deferred.InlineFrom, deferred.InlineTo, false)
		}
	}

	m.editorSyncPending = false
	m.partialInsertLines = 0
	m.syncEditorFromDoc()
}

func truncateResult(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// addTurnToHistory adds the completed turn to conversation history.
// This gives the agent context about previous interactions.
func (m *Model) addTurnToHistory() {
	if m.lastUserMessage == "" {
		return
	}

	// Add user message to history
	m.history = append(m.history, llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: m.lastUserMessage},
		},
	})

	// Add a summary of what changed as the assistant response
	changeSummary := m.doc.SummarizeChanges(m.lastAgentSnap)
	assistantMsg := fmt.Sprintf("I made the following changes: %s\n\nThe document now has %d lines.",
		changeSummary, m.doc.LineCount())

	m.history = append(m.history, llm.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: assistantMsg},
		},
	})

	// Keep history manageable - limit to last 10 turns (20 messages)
	maxHistory := 20
	if len(m.history) > maxHistory {
		m.history = m.history[len(m.history)-maxHistory:]
	}

	// Clear for next turn
	m.lastUserMessage = ""
}

func (m *Model) syncDocFromEditor() {
	content := m.editor.Value()
	m.doc.SetText(content, "user")
}

func (m *Model) syncEditorFromDoc() {
	content := m.doc.Text()

	// Save cursor position before SetValue() resets it
	savedLine := m.editor.Line()
	savedCol := 0
	if li := m.editor.LineInfo(); li.ColumnOffset >= 0 {
		savedCol = li.ColumnOffset
	}

	m.editor.SetValue(content)

	// Restore cursor position (clamped to valid range)
	lines := strings.Split(content, "\n")
	targetLine := savedLine
	if targetLine >= len(lines) {
		targetLine = len(lines) - 1
	}
	if targetLine < 0 {
		targetLine = 0
	}

	targetCol := savedCol
	if targetLine < len(lines) {
		lineLen := len(lines[targetLine])
		if targetCol > lineLen {
			targetCol = lineLen
		}
	}

	// Navigate to saved position (same pattern as moveCursorToMouse)
	m.editor.SetCursor(0)
	for i := 0; i < targetLine; i++ {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})
	for i := 0; i < targetCol; i++ {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyRight})
	}
}

func (m *Model) appendReasoningDelta(delta string) {
	if delta == "" {
		return
	}

	combined := m.agentReasoningTail + delta
	combined = strings.ReplaceAll(combined, "\r\n", "\n")
	parts := strings.Split(combined, "\n")

	if len(parts) == 1 {
		m.agentReasoningTail = parts[0]
		if trimmed := strings.TrimSpace(parts[0]); trimmed != "" {
			m.agentLastReasoningLn = trimmed
		}
		return
	}

	for i := 0; i < len(parts)-1; i++ {
		if trimmed := strings.TrimSpace(parts[i]); trimmed != "" {
			m.agentLastReasoningLn = trimmed
		}
	}

	m.agentReasoningTail = parts[len(parts)-1]
	if trimmed := strings.TrimSpace(m.agentReasoningTail); trimmed != "" {
		m.agentLastReasoningLn = trimmed
	}
}

// handoff triggers a plan-to-chat handoff by showing the agent picker.
func (m *Model) handoff() (tea.Model, tea.Cmd) {
	if m.agentActive {
		m.setStatus("Cancel agent first (Ctrl+C)")
		return m, nil
	}
	m.syncDocFromEditor()
	content := strings.TrimSpace(m.doc.Text())
	if content == "" {
		m.setStatus("Nothing to hand off")
		return m, nil
	}
	// Build agent list: "(no agent)" + builtin agents + user agents
	m.agentPickerItems = []string{"(no agent)"}
	registry, err := agents.NewRegistry(agents.RegistryConfig{
		UseBuiltin:  m.config.Agents.UseBuiltin,
		SearchPaths: m.config.Agents.SearchPaths,
	})
	if err == nil {
		if names, err := registry.ListNames(); err == nil {
			m.agentPickerItems = append(m.agentPickerItems, names...)
		}
	}
	m.agentPickerCursor = 0
	m.agentPickerVisible = true
	return m, nil
}

func (m *Model) saveDocument() (tea.Model, tea.Cmd) {
	if m.filePath == "" {
		m.setStatus("No file path configured")
		return m, nil
	}

	m.syncDocFromEditor()
	content := m.doc.Text()

	if err := os.WriteFile(m.filePath, []byte(content), 0644); err != nil {
		m.setStatus(fmt.Sprintf("Failed to save: %v", err))
		return m, nil
	}

	m.setStatus(fmt.Sprintf("Saved to %s", m.filePath))
	return m, nil
}
