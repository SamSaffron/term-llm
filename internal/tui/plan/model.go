package plan

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
	"golang.org/x/term"
)

// Model is the main plan TUI model for collaborative planning.
type Model struct {
	// Dimensions
	width  int
	height int

	// Document
	doc           *PlanDocument
	lastAgentSnap DocumentSnapshot // Snapshot when agent last started

	// Editor
	editor       textarea.Model
	cursorLine   int
	focusEditor  bool
	scrollOffset int

	// Vim mode
	vimMode    bool   // true = normal mode, false = insert mode
	vimPending string // pending multi-key command (e.g., "d" waiting for "d", "g" waiting for "g")
	yankBuffer string // yanked line(s) for paste

	// Visual mode
	visualMode  bool // true when in visual line mode
	visualStart int  // starting line of selection (0-indexed)
	visualEnd   int  // ending line of selection (0-indexed)

	// Command mode (for :w, :q, :wq)
	commandMode   bool   // true when typing a : command
	commandBuffer string // the command being typed

	// Agent state
	agentActive    bool
	agentStreaming bool
	agentPhase     string
	agentError     error
	streamCancel   context.CancelFunc
	tracker        *ui.ToolTracker // For tool tracking during agent runs

	// Embedded inline ask_user UI
	askUserModel  *tools.AskUserModel
	askUserDoneCh chan<- []tools.AskUserAnswer

	// LLM context
	provider       llm.Provider
	engine         *llm.Engine
	config         *config.Config
	modelName      string
	maxTurns       int
	addLineTool    *tools.AddLineTool
	removeLineTool *tools.RemoveLineTool

	// Streaming channels
	streamChan <-chan ui.StreamEvent

	// UI components
	spinner  spinner.Model
	viewport viewport.Model
	styles   *ui.Styles

	// Conversation history for planner context
	history         []llm.Message
	lastUserMessage string // The user message from the last agent invocation

	// File persistence
	filePath string // Path to save plan (e.g., plan.md)

	// UI state
	quitting bool
	err      error

	// Status message
	statusMsg     string
	statusMsgTime time.Time
}

// New creates a new plan model.
func New(cfg *config.Config, provider llm.Provider, engine *llm.Engine, modelName string, maxTurns int, filePath string) *Model {
	// Get terminal size
	width := 80
	height := 24
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		width = w
		height = h
	}

	// Create spinner
	s := spinner.New()
	s.Spinner = spinner.Dot

	styles := ui.DefaultStyles()
	s.Style = styles.Spinner

	// Create textarea for document editing
	ta := textarea.New()
	ta.Placeholder = "Start writing your plan...\n\nPress Ctrl+P to invoke the planner agent."
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0 // No limit
	ta.SetWidth(width - 4)
	ta.SetHeight(height - 4) // Leave room for status line
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(styles.Theme().Muted)
	ta.FocusedStyle.EndOfBuffer = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = lipgloss.NewStyle()
	ta.BlurredStyle = ta.FocusedStyle
	ta.Focus()

	// Create viewport (for potential scrolling in future)
	vp := viewport.New(width, height-2)
	vp.Style = lipgloss.NewStyle()

	// Create empty document
	doc := NewPlanDocument()

	// Create line-based editing tools
	addLineTool := tools.NewAddLineTool()
	removeLineTool := tools.NewRemoveLineTool()
	engine.Tools().Register(addLineTool)
	engine.Tools().Register(removeLineTool)

	return &Model{
		width:          width,
		height:         height,
		doc:            doc,
		editor:         ta,
		focusEditor:    true,
		spinner:        s,
		viewport:       vp,
		styles:         styles,
		provider:       provider,
		engine:         engine,
		config:         cfg,
		modelName:      modelName,
		maxTurns:       maxTurns,
		addLineTool:    addLineTool,
		removeLineTool: removeLineTool,
		history:        make([]llm.Message, 0),
		filePath:       filePath,
		tracker:        ui.NewToolTracker(),
	}
}

// Init initializes the model.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.spinner.Tick,
		tea.EnableMouseCellMotion,
	)
}

// streamEventMsg wraps ui.StreamEvent for bubbletea.
type streamEventMsg struct {
	event ui.StreamEvent
}

// statusClearMsg clears the status message after a delay.
type statusClearMsg struct{}

// AskUserRequestMsg triggers an inline ask_user prompt.
type AskUserRequestMsg struct {
	Questions []tools.AskUserQuestion
	DoneCh    chan<- []tools.AskUserAnswer
}

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.editor.SetWidth(m.width - 4)
		m.editor.SetHeight(m.height - 4)
		m.viewport.Width = m.width
		m.viewport.Height = m.height - 2
		if m.askUserModel != nil {
			m.askUserModel.SetWidth(m.width)
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKeyMsg(msg)

	case tea.MouseMsg:
		return m.handleMouseMsg(msg)

	case spinner.TickMsg:
		if m.agentActive {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}

	case ui.WaveTickMsg:
		if m.tracker != nil {
			if cmd := m.tracker.HandleWaveTick(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

	case ui.WavePauseMsg:
		if m.tracker != nil {
			if cmd := m.tracker.HandleWavePause(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

	case statusClearMsg:
		m.statusMsg = ""
		return m, nil

	case streamEventMsg:
		ev := msg.event
		cmds = append(cmds, m.handleStreamEvent(ev)...)

		// Continue listening for more events unless done or error
		if ev.Type != ui.StreamEventDone && ev.Type != ui.StreamEventError {
			cmds = append(cmds, m.listenForStreamEvents())
		}

	case AskUserRequestMsg:
		// Handle ask_user from the planner agent
		m.askUserDoneCh = msg.DoneCh
		m.askUserModel = tools.NewEmbeddedAskUserModel(msg.Questions, m.width)
		return m, nil
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) handleStreamEvent(ev ui.StreamEvent) []tea.Cmd {
	var cmds []tea.Cmd

	switch ev.Type {
	case ui.StreamEventError:
		if ev.Err != nil {
			m.agentActive = false
			m.agentStreaming = false
			m.agentError = ev.Err
			m.setStatus(fmt.Sprintf("Agent error: %v", ev.Err))
		}

	case ui.StreamEventToolStart:
		if m.tracker != nil {
			if m.tracker.HandleToolStart(ev.ToolCallID, ev.ToolName, ev.ToolInfo) {
				// Don't start wave for ask_user - it has its own UI
				if ev.ToolName != tools.AskUserToolName {
					cmds = append(cmds, m.tracker.StartWave())
				}
			}
		}
		m.agentPhase = fmt.Sprintf("Using %s...", ev.ToolName)

	case ui.StreamEventToolEnd:
		if m.tracker != nil {
			m.tracker.HandleToolEnd(ev.ToolCallID, ev.ToolSuccess)
			if !m.tracker.HasPending() {
				m.agentPhase = "Thinking"
			}
		}

	case ui.StreamEventPhase:
		m.agentPhase = ev.Phase

	case ui.StreamEventText:
		// Agent might emit text - we don't display it directly since edits go to document
		m.agentPhase = "Editing"

	case ui.StreamEventDone:
		m.agentActive = false
		m.agentStreaming = false
		m.agentPhase = ""
		m.tracker = ui.NewToolTracker() // Reset tracker

		// Sync editor with document content
		m.syncEditorFromDoc()

		// Add turn to history so agent has context for next invocation
		m.addTurnToHistory()

		m.setStatus("Agent finished")
	}

	return cmds
}

func (m *Model) handleMouseMsg(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// Ignore mouse events when ask_user UI is active
	if m.askUserModel != nil {
		return m, nil
	}

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		// Scroll up - move cursor up multiple lines
		for i := 0; i < 3; i++ {
			m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyUp})
		}
		return m, nil

	case tea.MouseButtonWheelDown:
		// Scroll down - move cursor down multiple lines
		for i := 0; i < 3; i++ {
			m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDown})
		}
		return m, nil

	case tea.MouseButtonLeft:
		if msg.Action == tea.MouseActionPress || msg.Action == tea.MouseActionMotion {
			// Click to position cursor
			m.moveCursorToMouse(msg.X, msg.Y)
			return m, nil
		}
	}

	return m, nil
}

// moveCursorToMouse positions the cursor based on mouse coordinates.
func (m *Model) moveCursorToMouse(mouseX, mouseY int) {
	content := m.editor.Value()
	lines := strings.Split(content, "\n")

	// Account for any padding/margin in the editor (2 chars on left)
	editorX := mouseX - 2
	if editorX < 0 {
		editorX = 0
	}

	// The editor has a viewport, so we need to account for scroll offset
	// mouseY is relative to the screen, we need to find which line that corresponds to
	// The editor's internal line is the visual line + scroll offset
	visibleLine := mouseY // Assuming editor starts at top

	// Get current scroll position from the editor
	// The textarea doesn't expose scroll offset directly, so we calculate based on cursor
	currentLine := m.editor.Line()
	editorHeight := m.editor.Height()

	// Estimate scroll offset: if cursor is visible, scroll offset is approximately
	// currentLine - (position of cursor in viewport)
	// This is an approximation since textarea doesn't expose scroll offset
	scrollOffset := 0
	if currentLine >= editorHeight {
		scrollOffset = currentLine - editorHeight/2
	}

	targetLine := visibleLine + scrollOffset
	if targetLine < 0 {
		targetLine = 0
	}
	if targetLine >= len(lines) {
		targetLine = len(lines) - 1
	}

	// Get target column, clamped to line length
	targetCol := editorX
	if targetLine >= 0 && targetLine < len(lines) {
		lineLen := len(lines[targetLine])
		if targetCol > lineLen {
			targetCol = lineLen
		}
	}

	// Move cursor to target position
	// First, go to document start
	m.editor.SetCursor(0)

	// Navigate to target line
	for i := 0; i < targetLine; i++ {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDown})
	}

	// Go to start of line, then navigate to target column
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})
	for i := 0; i < targetCol; i++ {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyRight})
	}
}

func (m *Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Handle ask_user UI if active
	if m.askUserModel != nil {
		cmd := m.askUserModel.UpdateEmbedded(msg)
		if m.askUserModel.IsDone() || m.askUserModel.IsCancelled() {
			// Send result and clean up
			if m.askUserModel.IsCancelled() {
				m.askUserDoneCh <- nil
			} else {
				m.askUserDoneCh <- m.askUserModel.Answers()
			}
			m.askUserModel = nil
			m.askUserDoneCh = nil
			return m, m.spinner.Tick
		}
		if cmd != nil {
			return m, cmd
		}
		return m, nil
	}

	// Handle command mode (for :w, :q, :wq)
	if m.commandMode {
		return m.handleCommandMode(msg)
	}

	// Global keys that work in both modes
	switch msg.String() {
	case "ctrl+c", "ctrl+q":
		// Cancel agent if running
		if m.agentActive && m.streamCancel != nil {
			m.streamCancel()
			m.agentActive = false
			m.agentStreaming = false
			m.setStatus("Agent cancelled")
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit

	case "ctrl+p":
		// Trigger planner agent
		if m.agentActive {
			m.setStatus("Agent already running - press Esc to cancel")
			return m, nil
		}
		return m.triggerPlanner()

	case "ctrl+s":
		// Save document
		return m.saveDocument()

	case "esc":
		// Exit command/visual mode or switch to normal mode (vim)
		m.commandMode = false
		m.commandBuffer = ""
		m.visualMode = false
		m.vimMode = true
		m.vimPending = ""
		return m, nil
	}

	// Vim normal mode handling
	if m.vimMode {
		return m.handleVimNormalMode(msg)
	}

	// Insert mode - pass to editor
	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(msg)
	m.syncDocFromEditor()
	return m, cmd
}

// handleVimNormalMode processes keys in vim normal mode.
func (m *Model) handleVimNormalMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Handle visual mode keys first
	if m.visualMode {
		return m.handleVisualMode(key)
	}

	// Handle pending multi-key commands
	if m.vimPending != "" {
		return m.handleVimPending(key)
	}

	switch key {
	// Mode switching
	case "i":
		m.vimMode = false
		return m, nil
	case "a":
		m.vimMode = false
		// Move cursor right (after current char)
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyRight})
		return m, nil
	case "o":
		m.vimMode = false
		m.vimInsertLineBelow()
		return m, nil
	case "O":
		m.vimMode = false
		m.vimInsertLineAbove()
		return m, nil

	// Navigation
	case "h":
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyLeft})
		return m, nil
	case "j":
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDown})
		return m, nil
	case "k":
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyUp})
		return m, nil
	case "l":
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyRight})
		return m, nil

	// Word navigation
	case "w":
		m.vimWordForward()
		return m, nil
	case "b":
		m.vimWordBackward()
		return m, nil

	// Line navigation
	case "0":
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})
		return m, nil
	case "$":
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyEnd})
		return m, nil

	// Document navigation (multi-key)
	case "g":
		m.vimPending = "g"
		return m, nil
	case "G":
		m.vimGotoEnd()
		return m, nil

	// Editing
	case "x":
		m.vimDeleteChar()
		m.syncDocFromEditor()
		return m, nil
	case "d":
		m.vimPending = "d"
		return m, nil
	case "y":
		m.vimPending = "y"
		return m, nil
	case "p":
		m.vimPaste()
		m.syncDocFromEditor()
		return m, nil

	// Visual mode
	case "V":
		m.vimEnterVisualMode()
		return m, nil

	// Command mode
	case ":":
		m.commandMode = true
		m.commandBuffer = ""
		return m, nil
	}

	return m, nil
}

// handleVimPending handles multi-key commands like dd, yy, gg.
func (m *Model) handleVimPending(key string) (tea.Model, tea.Cmd) {
	pending := m.vimPending
	m.vimPending = ""

	switch pending {
	case "g":
		if key == "g" {
			m.vimGotoStart()
		}
	case "d":
		if key == "d" {
			m.vimDeleteLine()
			m.syncDocFromEditor()
		}
	case "y":
		if key == "y" {
			m.vimYankLine()
		}
	}

	return m, nil
}

// handleVisualMode processes keys in visual line mode.
func (m *Model) handleVisualMode(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.visualMode = false
		return m, nil
	case "j":
		// Extend selection down
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDown})
		m.visualEnd = m.editor.Line()
		return m, nil
	case "k":
		// Extend selection up
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyUp})
		m.visualEnd = m.editor.Line()
		return m, nil
	case "d":
		// Delete selected lines
		m.vimDeleteSelection()
		m.syncDocFromEditor()
		return m, nil
	case "y":
		// Yank selected lines
		m.vimYankSelection()
		return m, nil
	case "G":
		// Extend to end of document
		m.vimGotoEnd()
		m.visualEnd = m.editor.Line()
		return m, nil
	case "g":
		m.vimPending = "g"
		return m, nil
	}

	// Handle pending g command in visual mode
	if m.vimPending == "g" {
		m.vimPending = ""
		if key == "g" {
			m.vimGotoStart()
			m.visualEnd = m.editor.Line()
		}
		return m, nil
	}

	return m, nil
}

// handleCommandMode processes keys in command mode (after pressing :).
func (m *Model) handleCommandMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		// Execute the command
		return m.executeCommand()
	case tea.KeyEsc:
		// Cancel command mode
		m.commandMode = false
		m.commandBuffer = ""
		return m, nil
	case tea.KeyBackspace:
		// Delete last character
		if len(m.commandBuffer) > 0 {
			m.commandBuffer = m.commandBuffer[:len(m.commandBuffer)-1]
		}
		if len(m.commandBuffer) == 0 {
			// Exit command mode if buffer is empty
			m.commandMode = false
		}
		return m, nil
	default:
		// Add character to buffer
		key := msg.String()
		if len(key) == 1 {
			m.commandBuffer += key
		}
		return m, nil
	}
}

// executeCommand executes the command in the command buffer.
func (m *Model) executeCommand() (tea.Model, tea.Cmd) {
	cmd := strings.TrimSpace(m.commandBuffer)
	m.commandMode = false
	m.commandBuffer = ""

	switch cmd {
	case "w":
		// Save
		return m.saveDocument()
	case "wq", "x":
		// Save and quit
		_, _ = m.saveDocument()
		m.quitting = true
		return m, tea.Quit
	case "q":
		// Quit (without saving)
		m.quitting = true
		return m, tea.Quit
	case "q!":
		// Force quit
		m.quitting = true
		return m, tea.Quit
	default:
		m.setStatus(fmt.Sprintf("Unknown command: %s", cmd))
		return m, nil
	}
}

// vimEnterVisualMode enters visual line mode.
func (m *Model) vimEnterVisualMode() {
	m.visualMode = true
	m.visualStart = m.editor.Line()
	m.visualEnd = m.visualStart
}

// vimDeleteSelection deletes the selected lines in visual mode.
func (m *Model) vimDeleteSelection() {
	content := m.editor.Value()
	lines := strings.Split(content, "\n")

	startLine := min(m.visualStart, m.visualEnd)
	endLine := max(m.visualStart, m.visualEnd)

	// Bounds check
	if startLine < 0 {
		startLine = 0
	}
	if endLine >= len(lines) {
		endLine = len(lines) - 1
	}

	// Store in yank buffer
	m.yankBuffer = strings.Join(lines[startLine:endLine+1], "\n")

	// Remove the lines
	newLines := append(lines[:startLine], lines[endLine+1:]...)
	if len(newLines) == 0 {
		newLines = []string{""}
	}
	m.editor.SetValue(strings.Join(newLines, "\n"))

	// Position cursor
	targetRow := startLine
	if targetRow >= len(newLines) {
		targetRow = len(newLines) - 1
	}

	// Reset cursor and navigate
	m.editor.SetCursor(0)
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})
	for i := 0; i < targetRow; i++ {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})

	// Exit visual mode
	m.visualMode = false

	lineCount := endLine - startLine + 1
	if lineCount == 1 {
		m.setStatus("Deleted 1 line")
	} else {
		m.setStatus(fmt.Sprintf("Deleted %d lines", lineCount))
	}
}

// vimYankSelection yanks the selected lines in visual mode.
func (m *Model) vimYankSelection() {
	content := m.editor.Value()
	lines := strings.Split(content, "\n")

	startLine := min(m.visualStart, m.visualEnd)
	endLine := max(m.visualStart, m.visualEnd)

	// Bounds check
	if startLine < 0 {
		startLine = 0
	}
	if endLine >= len(lines) {
		endLine = len(lines) - 1
	}

	// Store in yank buffer
	m.yankBuffer = strings.Join(lines[startLine:endLine+1], "\n")

	// Exit visual mode
	m.visualMode = false

	lineCount := endLine - startLine + 1
	if lineCount == 1 {
		m.setStatus("Yanked 1 line")
	} else {
		m.setStatus(fmt.Sprintf("Yanked %d lines", lineCount))
	}
}

// vimInsertLineBelow inserts a new line below and enters insert mode.
func (m *Model) vimInsertLineBelow() {
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.syncDocFromEditor()
}

// vimInsertLineAbove inserts a new line above and enters insert mode.
func (m *Model) vimInsertLineAbove() {
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyUp})
	m.syncDocFromEditor()
}

// vimWordForward moves cursor to next word.
func (m *Model) vimWordForward() {
	content := m.editor.Value()
	row := m.editor.Line()
	col := m.editor.LineInfo().ColumnOffset
	lines := strings.Split(content, "\n")

	if row >= len(lines) {
		return
	}

	line := lines[row]
	// Find next word boundary
	inWord := col < len(line) && !isWordChar(rune(line[col]))
	for col < len(line) {
		if inWord && isWordChar(rune(line[col])) {
			break
		}
		if !inWord && !isWordChar(rune(line[col])) {
			inWord = true
		}
		col++
	}

	// If at end of line, move to next line start
	if col >= len(line) && row < len(lines)-1 {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDown})
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})
		return
	}

	// Move cursor to target column
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})
	for i := 0; i < col; i++ {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyRight})
	}
}

// vimWordBackward moves cursor to previous word.
func (m *Model) vimWordBackward() {
	content := m.editor.Value()
	row := m.editor.Line()
	col := m.editor.LineInfo().ColumnOffset
	lines := strings.Split(content, "\n")

	if row >= len(lines) {
		return
	}

	line := lines[row]
	// Move back one char first
	if col > 0 {
		col--
	} else if row > 0 {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyUp})
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyEnd})
		return
	}

	// Skip non-word chars
	for col > 0 && col < len(line) && !isWordChar(rune(line[col])) {
		col--
	}
	// Skip word chars to find start
	for col > 0 && isWordChar(rune(line[col-1])) {
		col--
	}

	// Move cursor to target column
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})
	for i := 0; i < col; i++ {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyRight})
	}
}

func isWordChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
}

// vimGotoStart moves to document start.
func (m *Model) vimGotoStart() {
	m.editor.SetCursor(0)
	for m.editor.Line() > 0 {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
}

// vimGotoEnd moves to document end.
func (m *Model) vimGotoEnd() {
	content := m.editor.Value()
	lines := strings.Split(content, "\n")
	for m.editor.Line() < len(lines)-1 {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
}

// vimDeleteChar deletes char under cursor (x command).
func (m *Model) vimDeleteChar() {
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDelete})
}

// vimDeleteLine deletes current line (dd command).
func (m *Model) vimDeleteLine() {
	content := m.editor.Value()
	lines := strings.Split(content, "\n")
	row := m.editor.Line()

	if row >= len(lines) {
		return
	}

	// Store in yank buffer
	m.yankBuffer = lines[row]

	// Remove the line
	newLines := append(lines[:row], lines[row+1:]...)
	m.editor.SetValue(strings.Join(newLines, "\n"))

	// Position cursor: after SetValue(), cursor may be inconsistent.
	// First reset to document start, then navigate to target row.
	if row >= len(newLines) && row > 0 {
		row = len(newLines) - 1
	}

	// Reset cursor to beginning by going to position 0 and ensuring we're at line 0
	m.editor.SetCursor(0)
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})

	// Navigate down to target row
	for i := 0; i < row; i++ {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	// Go to beginning of line
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})
}

// vimYankLine yanks current line (yy command).
func (m *Model) vimYankLine() {
	content := m.editor.Value()
	lines := strings.Split(content, "\n")
	row := m.editor.Line()

	if row < len(lines) {
		m.yankBuffer = lines[row]
		m.setStatus("Yanked line")
	}
}

// vimPaste pastes yanked line(s) below (p command).
func (m *Model) vimPaste() {
	if m.yankBuffer == "" {
		return
	}

	content := m.editor.Value()
	lines := strings.Split(content, "\n")
	row := m.editor.Line()

	// Split yank buffer in case it contains multiple lines
	yankLines := strings.Split(m.yankBuffer, "\n")

	// Insert after current line
	newLines := make([]string, 0, len(lines)+len(yankLines))
	newLines = append(newLines, lines[:row+1]...)
	newLines = append(newLines, yankLines...)
	if row+1 < len(lines) {
		newLines = append(newLines, lines[row+1:]...)
	}

	m.editor.SetValue(strings.Join(newLines, "\n"))

	// Move to the first pasted line
	for m.editor.Line() <= row {
		m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	m.editor, _ = m.editor.Update(tea.KeyMsg{Type: tea.KeyHome})
}

func (m *Model) triggerPlanner() (tea.Model, tea.Cmd) {
	// Sync document from editor
	m.syncDocFromEditor()

	// Take a snapshot before agent starts
	m.lastAgentSnap = m.doc.Snapshot()

	// Set up agent state
	m.agentActive = true
	m.agentStreaming = true
	m.agentPhase = "Thinking"
	m.agentError = nil
	// Keep editor focused - user can continue editing during agent operation

	// Create context for cancellation
	ctx, cancel := context.WithCancel(context.Background())
	m.streamCancel = cancel

	// Set up line editing executors
	m.addLineTool.SetExecutor(m.executeAddLine)
	m.removeLineTool.SetExecutor(m.executeRemoveLine)

	// Build request
	req := m.buildPlannerRequest()

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

	// Create stream adapter
	adapter := ui.NewStreamAdapter(ui.DefaultStreamBufferSize)
	go adapter.ProcessStream(ctx, stream)
	m.streamChan = adapter.Events()

	return m, tea.Batch(
		m.listenForStreamEvents(),
		m.spinner.Tick,
	)
}

func (m *Model) buildPlannerRequest() llm.Request {
	// Build context with document state
	docContent := m.doc.Text()
	var userChanges string
	if m.lastAgentSnap.Version > 0 {
		userChanges = m.doc.SummarizeChanges(m.lastAgentSnap)
	}

	// Build system prompt
	systemPrompt := `You are a planning assistant helping the user structure their ideas into a clear plan.

The user is editing a plan document. Your job is to help organize, structure, and improve the document.

You have access to investigation tools to explore the codebase:
- glob: Find files by pattern (e.g., "**/*.go", "src/**/*.ts")
- grep: Search file contents for patterns
- read_file: Read file contents
- shell: Run shell commands for git, npm, etc.

Use these tools to understand the codebase context when planning implementation details.

Document editing tools:
- add_line: Add lines to the document. Content can include multiple lines (newline-separated) - each streams to UI immediately.
- remove_line: Remove a line by fuzzy matching its content.

IMPORTANT: Put multiple lines in a SINGLE add_line call using newlines in the content. Each line streams to the UI as it's added.

Example - adding a section with one tool call:
  add_line(content: "## Overview\n- First point\n- Second point\n- Third point", after: "existing text")

Guidelines:
- Batch multiple lines into single add_line calls using newlines in content
- Use text anchoring (partial content match) to specify insertion points
- Investigate the codebase first if you need to understand existing code or patterns
- Preserve the user's intent and phrasing where possible
- Add structure (headers, bullets, numbered lists) to make the plan clearer
- If something is unclear, ask the user using ask_user tool
- Be concise in your edits
- Reference specific files and code when adding implementation details`

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
		userMsg.WriteString(fmt.Sprintf("\nUser made changes since your last edit: %s\n", userChanges))
	}

	userMsg.WriteString("\nPlease help improve and structure this plan. Make targeted edits using plan_edit.")

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
		Model:    m.modelName,
		Messages: messages,
		MaxTurns: m.maxTurns,
		Tools:    m.engine.Tools().AllSpecs(),
	}
}

func (m *Model) executeAddLine(content string, afterText string) (string, error) {
	// Split content into individual lines
	newLines := strings.Split(content, "\n")

	// Get current lines for fuzzy matching
	lines := m.doc.Lines()
	lineTexts := make([]string, len(lines))
	for i, line := range lines {
		lineTexts[i] = line.Content
	}

	// Find initial insertion point using fuzzy matching
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
	for _, line := range newLines {
		m.doc.InsertLine(insertAfterIdx, line, "agent")
		insertAfterIdx++ // Next line goes after the one we just inserted
		m.syncEditorFromDoc()
	}

	if len(newLines) == 1 {
		return fmt.Sprintf("Added: %s", truncateResult(newLines[0], 40)), nil
	}
	return fmt.Sprintf("Added %d lines", len(newLines)), nil
}

func (m *Model) executeRemoveLine(matchText string) (string, error) {
	// Get current lines for fuzzy matching
	lines := m.doc.Lines()
	lineTexts := make([]string, len(lines))
	for i, line := range lines {
		lineTexts[i] = line.Content
	}

	// Find line to remove using fuzzy matching
	matchIdx := tools.FindBestMatch(lineTexts, matchText)
	if matchIdx < 0 {
		return "", fmt.Errorf("no line matched: %s", truncateResult(matchText, 30))
	}

	removedContent := lineTexts[matchIdx]
	m.doc.DeleteLine(matchIdx)

	// Sync editor to show the change immediately
	m.syncEditorFromDoc()

	return fmt.Sprintf("Removed: %s", truncateResult(removedContent, 40)), nil
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
	m.editor.SetValue(content)
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

func (m *Model) setStatus(msg string) {
	m.statusMsg = msg
	m.statusMsgTime = time.Now()
}

func (m *Model) listenForStreamEvents() tea.Cmd {
	return func() tea.Msg {
		if m.streamChan == nil {
			return nil
		}
		ev, ok := <-m.streamChan
		if !ok {
			return streamEventMsg{event: ui.DoneEvent(0)}
		}
		return streamEventMsg{event: ev}
	}
}

// View renders the UI.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Render ask_user UI if active
	if m.askUserModel != nil {
		// Show document above ask_user
		b.WriteString(m.renderDocument())
		b.WriteString("\n")
		b.WriteString(m.askUserModel.View())
		return b.String()
	}

	// Main content: editor
	b.WriteString(m.editor.View())
	b.WriteString("\n")

	// Status line
	b.WriteString(m.renderStatusLine())

	return b.String()
}

func (m *Model) renderDocument() string {
	var b strings.Builder
	lines := m.doc.Lines()

	for _, line := range lines {
		// Indicate agent-edited lines with a subtle marker
		if line.Author == "agent" {
			b.WriteString(m.styles.Muted.Render("│ "))
		} else {
			b.WriteString("  ")
		}
		b.WriteString(line.Content)
		b.WriteString("\n")
	}

	return b.String()
}

func (m *Model) renderStatusLine() string {
	theme := m.styles.Theme()

	// Command mode: show command line
	if m.commandMode {
		cmdLine := ":" + m.commandBuffer + "_"
		return lipgloss.NewStyle().Foreground(theme.Primary).Render(cmdLine)
	}

	// Left side: vim mode indicator
	var vimIndicator string
	if m.visualMode {
		vimIndicator = "-- VISUAL LINE --"
	} else if m.vimMode {
		vimIndicator = "-- NORMAL --"
	} else {
		vimIndicator = "-- INSERT --"
	}

	// Second part: agent status
	var status string
	if m.agentActive {
		status = m.spinner.View() + " " + m.agentPhase
	} else if m.statusMsg != "" && time.Since(m.statusMsgTime) < 5*time.Second {
		status = m.statusMsg
	}

	// Combine left parts
	var left string
	if status != "" {
		left = vimIndicator + "  " + status
	} else {
		left = vimIndicator
	}

	// Middle: document info
	var middle string
	lineCount := m.doc.LineCount()
	if lineCount == 1 {
		middle = "1 line"
	} else {
		middle = fmt.Sprintf("%d lines", lineCount)
	}
	if len(m.history) > 0 {
		turns := len(m.history) / 2
		if turns == 1 {
			middle += " | 1 turn"
		} else {
			middle += fmt.Sprintf(" | %d turns", turns)
		}
	}

	// Right side: shortcuts
	right := "Ctrl+P: planner  Ctrl+S: save  Ctrl+Q: quit"
	if m.agentActive {
		right = "Ctrl+C: cancel  (you can keep editing)"
	}

	// Build status line
	leftStyle := lipgloss.NewStyle().Foreground(theme.Primary)
	middleStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	rightStyle := lipgloss.NewStyle().Foreground(theme.Muted)

	leftStr := leftStyle.Render(left)
	middleStr := middleStyle.Render(middle)
	rightStr := rightStyle.Render(right)

	// Calculate padding to distribute space
	leftWidth := lipgloss.Width(leftStr)
	middleWidth := lipgloss.Width(middleStr)
	rightWidth := lipgloss.Width(rightStr)
	totalContent := leftWidth + middleWidth + rightWidth
	availableSpace := m.width - totalContent

	if availableSpace < 2 {
		// Not enough space - just show left and right
		padding := max(1, m.width-leftWidth-rightWidth)
		return leftStr + strings.Repeat(" ", padding) + rightStr
	}

	// Split padding between left-middle and middle-right
	leftPad := availableSpace / 2
	rightPad := availableSpace - leftPad

	return leftStr + strings.Repeat(" ", leftPad) + middleStr + strings.Repeat(" ", rightPad) + rightStr
}

// SetProgram sets the tea.Program reference for sending messages.
// Note: ask_user handler is set up in cmd/plan.go after the program is created.
func (m *Model) SetProgram(p *tea.Program) {
	// Reserved for future use - program reference may be needed for other callbacks
	_ = p
}

// LoadContent loads content into the document and editor.
func (m *Model) LoadContent(content string) {
	m.doc = NewPlanDocumentFromText(content, "user")
	m.editor.SetValue(content)
}
