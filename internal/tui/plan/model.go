package plan

import (
	"context"
	"os"
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
	agentActive     bool
	agentStreaming  bool
	agentPhase      string
	agentError      error
	streamCancel    context.CancelFunc
	tracker         *ui.ToolTracker     // For tool tracking during agent runs
	subagentTracker *ui.SubagentTracker // For subagent progress tracking

	// Activity panel state
	activityExpanded bool             // toggle with Ctrl+A
	streamStartTime  time.Time        // when current agent run started
	stats            *ui.SessionStats // usage/timing tracking for current run
	currentTurn      int              // which LLM turn we're on

	// Embedded inline ask_user UI
	askUserModel  *tools.AskUserModel
	askUserDoneCh chan<- []tools.AskUserAnswer

	// LLM context
	provider            llm.Provider
	engine              *llm.Engine
	config              *config.Config
	modelName           string
	maxTurns            int
	search              bool
	forceExternalSearch bool
	// Streaming channels
	streamChan <-chan ui.StreamEvent

	// Partial insert tracking for streaming
	partialInsertAfter string // The anchor text for current partial insert
	partialInsertIdx   int    // Next insertion index (-1 if not tracking)
	partialInsertLines int    // Number of pending partial insert lines since last editor sync
	editorSyncPending  bool   // True when a deferred editor sync timer is active
	lastEditorInputAt  time.Time
	deferredEditEvents []ui.StreamEvent // Stream edit events deferred while user is actively editing

	// Cached reasoning state for activity panel
	agentReasoningTail   string // Current unflushed reasoning line
	agentLastReasoningLn string // Last non-empty reasoning line

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
	quitting     bool
	handedOff    bool   // true when handing off to chat agent
	handoffAgent string // agent name selected during handoff
	helpVisible  bool   // true when help overlay is shown
	err          error

	// Agent picker for handoff
	agentPickerVisible bool
	agentPickerItems   []string
	agentPickerCursor  int

	// Status message
	statusMsg     string
	statusMsgTime time.Time

	// Async chat channel for queued instructions
	chatInput      textarea.Model // Input for queued instructions
	pendingPrompts []string       // Queue of user instructions
	chatFocused    bool           // true when chat input has focus
}

const (
	maxStreamEventsPerBatch    = 128
	partialInsertSyncBatchSize = 24
	partialInsertSyncDelay     = 250 * time.Millisecond
	editorInputQuietPeriod     = 400 * time.Millisecond
)

// New creates a new plan model.
func New(cfg *config.Config, provider llm.Provider, engine *llm.Engine, modelName string, maxTurns int, filePath string, search bool, forceExternalSearch bool) *Model {
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
	ta.SetHeight(height - 9) // Leave room for status line, chat input, and separators
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

	// Create chat input for async instructions
	chatInput := textarea.New()
	chatInput.Placeholder = "Type instruction (Enter to queue)..."
	chatInput.Prompt = ""
	chatInput.ShowLineNumbers = false
	chatInput.CharLimit = 500
	chatInput.SetWidth(width - 4)
	chatInput.SetHeight(1)
	chatInput.FocusedStyle.Base = lipgloss.NewStyle()
	chatInput.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(styles.Theme().Muted)
	chatInput.BlurredStyle = chatInput.FocusedStyle
	chatInput.Blur() // Start unfocused

	return &Model{
		width:               width,
		height:              height,
		doc:                 doc,
		editor:              ta,
		focusEditor:         true,
		spinner:             s,
		viewport:            vp,
		styles:              styles,
		provider:            provider,
		engine:              engine,
		config:              cfg,
		modelName:           modelName,
		maxTurns:            maxTurns,
		search:              search,
		forceExternalSearch: forceExternalSearch,
		history:             make([]llm.Message, 0),
		filePath:            filePath,
		tracker:             ui.NewToolTracker(),
		subagentTracker:     ui.NewSubagentTracker(),
		partialInsertIdx:    -1,
		chatInput:           chatInput,
		pendingPrompts:      make([]string, 0),
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

// streamEventsMsg wraps a batch of stream events for bubbletea.
type streamEventsMsg struct {
	events []ui.StreamEvent
}

// statusClearMsg clears the status message after a delay.
type statusClearMsg struct{}

// AskUserRequestMsg triggers an inline ask_user prompt.
type AskUserRequestMsg struct {
	Questions []tools.AskUserQuestion
	DoneCh    chan<- []tools.AskUserAnswer
}

// SubagentProgressMsg carries progress events from running subagents.
type SubagentProgressMsg struct {
	CallID string
	Event  tools.SubagentEvent
}

// pendingPromptMsg triggers the planner with a pending user prompt.
type pendingPromptMsg struct {
	prompt string
}

// tickMsg is sent periodically to update elapsed time display.
type tickMsg time.Time

// editorSyncMsg triggers a deferred editor sync.
type editorSyncMsg struct{}

// SetProgram sets the tea.Program reference for sending messages.
// Note: ask_user handler is set up in cmd/plan.go after the program is created.
func (m *Model) SetProgram(p *tea.Program) {
	// Reserved for future use - program reference may be needed for other callbacks
	_ = p
}

// GetContent syncs the editor and returns the current document text.
func (m *Model) GetContent() string {
	m.syncDocFromEditor()
	return m.doc.Text()
}

// HandedOff returns true if the user triggered a handoff to chat.
func (m *Model) HandedOff() bool {
	return m.handedOff
}

// HandoffAgent returns the agent name selected during handoff (empty for default).
func (m *Model) HandoffAgent() string {
	return m.handoffAgent
}

// LoadContent loads content into the document and editor.
func (m *Model) LoadContent(content string) {
	m.doc = NewPlanDocumentFromText(content, "user")
	m.editor.SetValue(content)
}

func (m *Model) setStatus(msg string) {
	m.statusMsg = msg
	m.statusMsgTime = time.Now()
}

func (m *Model) tickEvery() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *Model) scheduleEditorSync() tea.Cmd {
	m.partialInsertLines++

	if m.partialInsertLines >= partialInsertSyncBatchSize {
		if m.editorSyncPending {
			return nil
		}
		return m.flushEditorSync()
	}

	if m.editorSyncPending {
		return nil
	}

	m.editorSyncPending = true
	return tea.Tick(partialInsertSyncDelay, func(time.Time) tea.Msg {
		return editorSyncMsg{}
	})
}

func (m *Model) flushEditorSync() tea.Cmd {
	if m.partialInsertLines == 0 {
		m.editorSyncPending = false
		return nil
	}

	if m.isEditorInputRecent() {
		m.editorSyncPending = true
		return tea.Tick(partialInsertSyncDelay, func(time.Time) tea.Msg {
			return editorSyncMsg{}
		})
	}

	m.editorSyncPending = false
	m.partialInsertLines = 0
	m.syncEditorFromDoc()
	return nil
}

func (m *Model) noteEditorInput() {
	m.lastEditorInputAt = time.Now()
}

func (m *Model) isEditorInputRecent() bool {
	if m.lastEditorInputAt.IsZero() {
		return false
	}
	return time.Since(m.lastEditorInputAt) < editorInputQuietPeriod
}

func (m *Model) shouldDeferStreamEdits() bool {
	return m.agentActive && m.isEditorInputRecent()
}

func (m *Model) listenForStreamEvents() tea.Cmd {
	return func() tea.Msg {
		if m.streamChan == nil {
			return nil
		}

		ev, ok := <-m.streamChan
		if !ok {
			return streamEventsMsg{events: []ui.StreamEvent{ui.DoneEvent(0)}}
		}

		events := make([]ui.StreamEvent, 0, maxStreamEventsPerBatch)
		events = append(events, ev)

		if ev.Type == ui.StreamEventDone || ev.Type == ui.StreamEventError {
			return streamEventsMsg{events: events}
		}

		for len(events) < maxStreamEventsPerBatch {
			select {
			case next, ok := <-m.streamChan:
				if !ok {
					return streamEventsMsg{events: append(events, ui.DoneEvent(0))}
				}
				events = append(events, next)
				if next.Type == ui.StreamEventDone || next.Type == ui.StreamEventError {
					return streamEventsMsg{events: events}
				}
			default:
				return streamEventsMsg{events: events}
			}
		}

		return streamEventsMsg{events: events}
	}
}
