package chat

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
	"golang.org/x/term"
)

// Model is the main chat TUI model
type Model struct {
	// Dimensions
	width  int
	height int

	// Components
	textarea textarea.Model
	spinner  spinner.Model
	styles   *ui.Styles
	keyMap   KeyMap

	// Conversation state
	session   *Session
	streaming bool
	phase     string // "Thinking", "Searching", "Reading", "Responding"

	// Streaming state
	currentResponse  strings.Builder
	currentTokens    int
	streamStartTime  time.Time
	webSearchUsed    bool
	retryStatus      string
	streamCancelFunc context.CancelFunc

	// Streaming channels
	streamChan chan streamEvent

	// LLM context
	provider     llm.Provider
	engine       *llm.Engine
	config       *config.Config
	providerName string
	modelName    string

	// Pending message context
	files         []FileAttachment // Attached files for next message
	searchEnabled bool             // Web search toggle

	// Directory approval
	approvedDirs       *ApprovedDirs
	pendingFilePath    string // File waiting for directory approval

	// History scroll
	scrollOffset int
	viewportRows int

	// UI state
	quitting bool
	err      error

	// Dialog components
	completions *CompletionsModel
	dialog      *DialogModel

	// Inline mode state
	program *tea.Program // Reference to program for tea.Println
}

// streamEvent represents an event from the streaming goroutine
type streamEvent struct {
	chunk       string
	phase       string
	tokens      int
	done        bool
	err         error
	retryStatus string
	webSearch   bool
}

// Messages for tea.Program
type (
	streamEventMsg    streamEvent
	sessionSavedMsg   struct{}
	sessionLoadedMsg  struct{ session *Session }
	tickMsg           time.Time
)

// New creates a new chat model
func New(cfg *config.Config, provider llm.Provider, engine *llm.Engine, modelName string) *Model {
	// Get terminal size
	width := 80
	height := 24
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		width = w
		height = h
	}

	// Create textarea with minimal styling for inline REPL
	ta := textarea.New()
	ta.Placeholder = ""
	ta.Prompt = ""                               // No prompt prefix (we add our own "> ")
	ta.ShowLineNumbers = false
	ta.CharLimit = 0                             // No limit
	ta.SetWidth(width - 4)
	ta.SetHeight(1)                              // Start with single line
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle()
	ta.FocusedStyle.EndOfBuffer = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = lipgloss.NewStyle()
	ta.BlurredStyle = ta.FocusedStyle
	ta.Focus()

	// Create spinner
	s := spinner.New()
	s.Spinner = spinner.Dot

	styles := ui.DefaultStyles()
	s.Style = styles.Spinner

	// Always start with a fresh session
	// Users can load previous sessions with /load command
	session := NewSession(provider.Name(), modelName)

	// Load approved directories
	approvedDirs, _ := LoadApprovedDirs()
	if approvedDirs == nil {
		approvedDirs = &ApprovedDirs{Directories: []string{}}
	}

	// Create completions and dialog
	completions := NewCompletionsModel(styles)
	completions.SetSize(width, height)

	dialog := NewDialogModel(styles)
	dialog.SetSize(width, height)

	return &Model{
		width:        width,
		height:       height,
		textarea:     ta,
		spinner:      s,
		styles:       styles,
		keyMap:       DefaultKeyMap(),
		session:      session,
		provider:     provider,
		engine:       engine,
		config:       cfg,
		providerName: provider.Name(),
		modelName:    modelName,
		phase:        "Thinking",
		viewportRows: height - 8, // Reserve space for input and status
		completions:  completions,
		dialog:       dialog,
		approvedDirs: approvedDirs,
	}
}

// Init initializes the model
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.spinner.Tick,
	)
}

// Update handles messages
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewportRows = m.height - 8
		m.textarea.SetWidth(m.width - 2)
		m.completions.SetSize(m.width, m.height)
		m.dialog.SetSize(m.width, m.height)
		return m, nil

	case tea.KeyMsg:
		return m.handleKeyMsg(msg)

	case spinner.TickMsg:
		if m.streaming {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}

	case tickMsg:
		if m.streaming {
			cmds = append(cmds, m.tickEvery())
		}

	case streamEventMsg:
		if msg.err != nil {
			m.streaming = false
			m.err = msg.err
			m.textarea.Focus()
			return m, nil
		}

		if msg.chunk != "" {
			m.currentResponse.WriteString(msg.chunk)
			m.phase = "Responding"
			m.retryStatus = ""
		}

		if msg.phase != "" {
			m.phase = msg.phase
			m.retryStatus = ""
		}

		if msg.retryStatus != "" {
			m.retryStatus = msg.retryStatus
		}

		if msg.webSearch {
			m.webSearchUsed = true
		}

		if msg.done {
			m.streaming = false
			m.currentTokens = msg.tokens
			duration := time.Since(m.streamStartTime)

			// Get the response content before resetting
			responseContent := m.currentResponse.String()

			// Create assistant message
			assistantMsg := NewAssistantMessage(
				responseContent,
				m.currentTokens,
				duration.Milliseconds(),
				m.webSearchUsed,
			)
			m.session.Messages = append(m.session.Messages, assistantMsg)

			// Print response permanently to scrollback (inline mode)
			rendered := m.renderMarkdown(responseContent)
			cmds = append(cmds, tea.Println(rendered))
			cmds = append(cmds, tea.Println("")) // blank line

			// Reset streaming state
			m.currentResponse.Reset()
			m.currentTokens = 0
			m.webSearchUsed = false
			m.retryStatus = ""

			// Auto-save session
			cmds = append(cmds, m.saveSessionCmd())

			// Re-enable textarea
			m.textarea.Focus()
		} else {
			// Continue listening for more events
			cmds = append(cmds, m.listenForStreamEvents())
		}

	case sessionSavedMsg:
		// Session saved successfully, nothing to do

	case sessionLoadedMsg:
		if msg.session != nil {
			m.session = msg.session
			m.scrollOffset = 0
		}

	case clipboardCopiedMsg:
		// Show brief confirmation - add as system message
		return m.showSystemMessage("Copied last response to clipboard.")
	}

	// Update textarea if not streaming
	if !m.streaming {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Handle dialog first if open
	if m.dialog.IsOpen() {
		// Model picker supports typing to filter (like completions)
		if m.dialog.Type() == DialogModelPicker {
			switch {
			case key.Matches(msg, key.NewBinding(key.WithKeys("enter", "tab"))):
				selected := m.dialog.Selected()
				if selected != nil {
					m.dialog.Close()
					return m.switchModel(selected.ID)
				}
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
				m.dialog.Close()
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
				m.dialog.Update(msg)
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
				m.dialog.Update(msg)
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
				// Update query on backspace
				query := m.dialog.Query()
				if len(query) > 0 {
					m.dialog.SetQuery(query[:len(query)-1])
				}
				return m, nil
			default:
				// Type to filter
				if len(msg.String()) == 1 {
					m.dialog.SetQuery(m.dialog.Query() + msg.String())
					return m, nil
				}
			}
			return m, nil
		}

		// Other dialogs use standard handling
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter", "tab"))):
			selected := m.dialog.Selected()
			if selected != nil {
				switch m.dialog.Type() {
				case DialogSessionList:
					m.dialog.Close()
					return m.cmdLoad([]string{selected.ID})
				case DialogDirApproval:
					if selected.ID == "__deny__" {
						m.pendingFilePath = ""
						m.dialog.Close()
						return m.showSystemMessage("File access denied.")
					}
					// Approve the directory
					if err := m.approvedDirs.AddDirectory(selected.ID); err != nil {
						m.dialog.Close()
						return m.showSystemMessage(fmt.Sprintf("Failed to approve directory: %v", err))
					}
					// Now try to attach the file again
					filePath := m.pendingFilePath
					m.pendingFilePath = ""
					m.dialog.Close()
					return m.attachFile(filePath)
				}
			}
			m.dialog.Close()
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "q"))):
			m.pendingFilePath = ""
			m.dialog.Close()
			return m, nil
		default:
			m.dialog.Update(msg)
			return m, nil
		}
	}

	// Handle completions if visible
	if m.completions.IsVisible() {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			// Enter executes immediately with the selected command
			selected := m.completions.Selected()
			if selected != nil {
				// Extract any args typed after the command prefix
				// e.g., "/mo son" -> command "model", args "son"
				input := m.textarea.Value()
				args := ""
				if idx := strings.Index(input, " "); idx != -1 {
					args = strings.TrimSpace(input[idx+1:])
				}
				m.completions.Hide()
				m.textarea.SetValue("")
				m.textarea.SetHeight(1)
				if args != "" {
					return m.ExecuteCommand("/" + selected.Name + " " + args)
				}
				return m.ExecuteCommand("/" + selected.Name)
			}
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
			// Tab completes but doesn't execute (for adding args)
			selected := m.completions.Selected()
			if selected != nil {
				m.textarea.SetValue("/" + selected.Name + " ")
				m.completions.Hide()
			}
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			m.completions.Hide()
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("up", "ctrl+p"))):
			m.completions.Update(msg)
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("down", "ctrl+n"))):
			m.completions.Update(msg)
			return m, nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
			// Update query on backspace
			value := m.textarea.Value()
			if len(value) > 1 {
				m.textarea.SetValue(value[:len(value)-1])
				m.completions.SetQuery(strings.TrimPrefix(m.textarea.Value(), "/"))
			} else if len(value) == 1 {
				m.textarea.SetValue("")
				m.textarea.SetHeight(1)
				m.completions.Hide()
			}
			return m, nil
		default:
			// Add character to query
			if len(msg.String()) == 1 {
				m.textarea.SetValue(m.textarea.Value() + msg.String())
				m.completions.SetQuery(strings.TrimPrefix(m.textarea.Value(), "/"))
				return m, nil
			}
		}
	}

	// Handle quit
	if key.Matches(msg, m.keyMap.Quit) {
		if m.streaming && m.streamCancelFunc != nil {
			m.streamCancelFunc()
			m.streaming = false
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit
	}

	// Handle cancel during streaming
	if key.Matches(msg, m.keyMap.Cancel) {
		if m.streaming && m.streamCancelFunc != nil {
			m.streamCancelFunc()
			m.streaming = false
			m.textarea.Focus()
			return m, nil
		}
		// Clear input if not empty
		if m.textarea.Value() != "" {
			m.textarea.SetValue("")
			m.textarea.SetHeight(1)
		}
		return m, nil
	}

	// Don't process other keys while streaming
	if m.streaming {
		return m, nil
	}

	// Handle command palette (Ctrl+P)
	if key.Matches(msg, m.keyMap.Commands) {
		m.textarea.SetValue("/")
		m.completions.Show()
		return m, nil
	}

	// Handle model picker (Ctrl+L)
	if key.Matches(msg, m.keyMap.SwitchModel) {
		m.dialog.ShowModelPicker(m.modelName, GetAvailableProviders())
		return m, nil
	}

	// Handle new session (Ctrl+N)
	if key.Matches(msg, m.keyMap.NewSession) {
		return m.cmdNew()
	}

	// Handle clear
	if key.Matches(msg, m.keyMap.Clear) {
		return m.cmdClear()
	}

	// Handle newline (must be before Send since shift+enter contains enter)
	if key.Matches(msg, m.keyMap.Newline) || key.Matches(msg, m.keyMap.NewlineAlt) {
		m.textarea.SetValue(m.textarea.Value() + "\n")
		m.updateTextareaHeight()
		return m, nil
	}

	// Handle send
	if key.Matches(msg, m.keyMap.Send) {
		content := strings.TrimSpace(m.textarea.Value())

		// Check for backslash continuation
		if strings.HasSuffix(content, "\\") {
			// Remove backslash and insert newline
			m.textarea.SetValue(strings.TrimSuffix(content, "\\") + "\n")
			m.updateTextareaHeight()
			return m, nil
		}

		// Check for slash commands
		if strings.HasPrefix(content, "/") {
			return m.handleSlashCommand(content)
		}

		// Send message if not empty
		if content != "" {
			return m.sendMessage(content)
		}
		return m, nil
	}

	// Handle "/" at start of empty input to show completions
	if msg.String() == "/" && m.textarea.Value() == "" {
		m.textarea.SetValue("/")
		m.completions.Show()
		return m, nil
	}

	// Handle web toggle
	if key.Matches(msg, m.keyMap.ToggleWeb) {
		m.searchEnabled = !m.searchEnabled
		return m, nil
	}

	// Handle vim-style navigation when input is empty
	if m.textarea.Value() == "" {
		// Calculate total content height for scrolling
		totalMessages := len(m.session.Messages)

		// j - scroll down (show older messages, increase offset)
		if key.Matches(msg, m.keyMap.ScrollDown) {
			if m.scrollOffset > 0 {
				m.scrollOffset--
			}
			return m, nil
		}

		// k - scroll up (show newer messages, decrease offset from bottom)
		if key.Matches(msg, m.keyMap.ScrollUp) {
			maxScroll := totalMessages - 1
			if maxScroll < 0 {
				maxScroll = 0
			}
			if m.scrollOffset < maxScroll {
				m.scrollOffset++
			}
			return m, nil
		}

		// G - go to bottom (most recent)
		if key.Matches(msg, m.keyMap.GoToBottom) {
			m.scrollOffset = 0
			return m, nil
		}

		// g - go to top (oldest)
		if key.Matches(msg, m.keyMap.GoToTop) {
			maxScroll := totalMessages - 1
			if maxScroll < 0 {
				maxScroll = 0
			}
			m.scrollOffset = maxScroll
			return m, nil
		}

		// Page up/down
		if key.Matches(msg, m.keyMap.PageUp) {
			maxScroll := totalMessages - 1
			if maxScroll < 0 {
				maxScroll = 0
			}
			m.scrollOffset += 5
			if m.scrollOffset > maxScroll {
				m.scrollOffset = maxScroll
			}
			return m, nil
		}

		if key.Matches(msg, m.keyMap.PageDown) {
			m.scrollOffset -= 5
			if m.scrollOffset < 0 {
				m.scrollOffset = 0
			}
			return m, nil
		}

		// y - copy last assistant response to clipboard
		if key.Matches(msg, m.keyMap.Copy) {
			return m.copyLastResponse()
		}

		// ? - show help
		if key.Matches(msg, m.keyMap.Help) {
			return m.cmdHelp()
		}
	}

	// Update textarea for other keys
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	m.updateTextareaHeight()
	return m, cmd
}

func (m *Model) handleSlashCommand(input string) (tea.Model, tea.Cmd) {
	return m.ExecuteCommand(input)
}

func (m *Model) sendMessage(content string) (tea.Model, tea.Cmd) {
	// Build the full message content including file attachments
	fullContent := content
	var fileNames []string

	if len(m.files) > 0 {
		var filesContent strings.Builder
		filesContent.WriteString("\n\n---\n**Attached files:**\n")
		for _, f := range m.files {
			fileNames = append(fileNames, f.Name)
			filesContent.WriteString(fmt.Sprintf("\n### %s\n```\n%s\n```\n", f.Name, f.Content))
		}
		fullContent += filesContent.String()
	}

	// Create user message with full content (including files) for replay/history
	userMsg := NewUserMessage(fullContent, fileNames)
	m.session.Messages = append(m.session.Messages, userMsg)

	// Print user message permanently to scrollback (inline mode)
	theme := m.styles.Theme()
	var userDisplay strings.Builder
	userDisplay.WriteString(lipgloss.NewStyle().Foreground(theme.Primary).Bold(true).Render("❯") + " ")
	userDisplay.WriteString(content)
	if len(fileNames) > 0 {
		userDisplay.WriteString("\n")
		userDisplay.WriteString(lipgloss.NewStyle().Foreground(theme.Muted).Render(
			fmt.Sprintf("[with: %s]", strings.Join(fileNames, ", "))))
	}
	// tea.Println adds newline, no need for extra

	// Clear input and files
	m.textarea.SetValue("")
	m.textarea.SetHeight(1)
	m.files = nil
	m.textarea.Blur()

	// Start streaming
	m.streaming = true
	m.phase = "Thinking"
	m.streamStartTime = time.Now()
	m.currentResponse.Reset()
	m.webSearchUsed = false

	// Start the stream (print user message first)
	return m, tea.Batch(
		tea.Println(userDisplay.String()),
		m.startStream(fullContent),
		m.spinner.Tick,
		m.tickEvery(),
	)
}

func (m *Model) startStream(content string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		m.streamCancelFunc = cancel

		// Create channel for streaming events
		m.streamChan = make(chan streamEvent, 100)

		// Build messages from conversation history
		messages := m.buildMessages()

		req := llm.Request{
			Messages:          messages,
			Search:            m.searchEnabled,
			ParallelToolCalls: true,
		}

		// Start streaming in background
		go func() {
			defer close(m.streamChan)

			stream, err := m.engine.Stream(ctx, req)
			if err != nil {
				m.streamChan <- streamEvent{err: err}
				return
			}
			defer stream.Close()

			var totalTokens int
			for {
				event, err := stream.Recv()
				if err == io.EOF {
					m.streamChan <- streamEvent{done: true, tokens: totalTokens}
					return
				}
				if err != nil {
					// Check if context was cancelled
					if ctx.Err() != nil {
						m.streamChan <- streamEvent{done: true, tokens: totalTokens}
						return
					}
					m.streamChan <- streamEvent{err: err}
					return
				}

				switch event.Type {
				case llm.EventTextDelta:
					if event.Text != "" {
						m.streamChan <- streamEvent{chunk: event.Text}
					}
				case llm.EventToolExecStart:
					var phase string
					if event.ToolName == llm.WebSearchToolName {
						phase = "Searching"
						m.streamChan <- streamEvent{phase: phase, webSearch: true}
					} else if event.ToolName == llm.ReadURLToolName {
						if event.ToolInfo != "" {
							phase = fmt.Sprintf("Reading %s", truncateURL(event.ToolInfo, 40))
						} else {
							phase = "Reading"
						}
						m.streamChan <- streamEvent{phase: phase}
					}
				case llm.EventRetry:
					status := fmt.Sprintf("Rate limited (%d/%d), waiting %.0fs...",
						event.RetryAttempt, event.RetryMaxAttempts, event.RetryWaitSecs)
					m.streamChan <- streamEvent{retryStatus: status}
				case llm.EventUsage:
					if event.Use != nil {
						totalTokens = event.Use.OutputTokens
					}
				}
			}
		}()

		// Return initial listen command
		return m.listenForStreamEventsSync()
	}
}

// listenForStreamEvents returns a command that listens for the next stream event
func (m *Model) listenForStreamEvents() tea.Cmd {
	return func() tea.Msg {
		return m.listenForStreamEventsSync()
	}
}

// listenForStreamEventsSync synchronously waits for the next stream event
func (m *Model) listenForStreamEventsSync() tea.Msg {
	if m.streamChan == nil {
		return streamEventMsg{done: true}
	}

	event, ok := <-m.streamChan
	if !ok {
		return streamEventMsg{done: true}
	}
	return streamEventMsg(event)
}

// truncateURL shortens a URL for display
func truncateURL(url string, maxLen int) string {
	if len(url) <= maxLen {
		return url
	}
	// Remove protocol prefix for cleaner display
	display := strings.TrimPrefix(url, "https://")
	display = strings.TrimPrefix(display, "http://")
	if len(display) <= maxLen {
		return display
	}
	// Truncate with ellipsis
	return display[:maxLen-3] + "..."
}

func (m *Model) buildMessages() []llm.Message {
	var messages []llm.Message

	// Add system instructions if configured
	if m.config.Ask.Instructions != "" {
		messages = append(messages, llm.SystemText(m.config.Ask.Instructions))
	}

	// Add conversation history
	// Note: User messages already contain file contents if files were attached
	for _, msg := range m.session.Messages {
		switch msg.Role {
		case RoleUser:
			messages = append(messages, llm.UserText(msg.Content))
		case RoleAssistant:
			messages = append(messages, llm.AssistantText(msg.Content))
		}
	}

	return messages
}

func (m *Model) tickEvery() tea.Cmd {
	return tea.Tick(1*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *Model) saveSessionCmd() tea.Cmd {
	return func() tea.Msg {
		_ = SaveCurrentSession(m.session)
		return sessionSavedMsg{}
	}
}

// View renders the model (inline mode - only active frame)
func (m *Model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Streaming response (if active)
	if m.streaming {
		b.WriteString(m.renderStreamingInline())
	}

	// Completions popup (if visible)
	if m.completions.IsVisible() {
		b.WriteString(m.completions.View())
		b.WriteString("\n")
	}

	// Dialog (if open)
	if m.dialog.IsOpen() {
		b.WriteString(m.dialog.View())
		b.WriteString("\n")
	}

	// Input prompt
	b.WriteString(m.renderInputInline())

	// Set terminal title
	title := m.getTerminalTitle()
	titleSeq := fmt.Sprintf("\x1b]0;%s\x07", title)

	return titleSeq + b.String()
}

// renderStreamingInline renders the streaming response for inline mode
func (m *Model) renderStreamingInline() string {
	var b strings.Builder
	theme := m.styles.Theme()

	// Breathing room
	b.WriteString("\n")

	indicator := ui.StreamingIndicator{
		Spinner:    m.spinner.View(),
		Phase:      m.phase,
		Elapsed:    time.Since(m.streamStartTime),
		Tokens:     m.currentTokens,
		ShowCancel: true,
	}

	if m.currentResponse.Len() == 0 {
		// Still waiting for first token
		b.WriteString(indicator.Render(m.styles))
		b.WriteString("\n")
	} else {
		// Render streamed content with progress line
		b.WriteString(m.renderMarkdown(m.currentResponse.String()))
		b.WriteString("\n")
		b.WriteString(indicator.Render(m.styles))
		b.WriteString("\n")
	}

	// Retry status if present (shown as warning on separate line)
	if m.retryStatus != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(theme.Warning).Render("⚠ " + m.retryStatus))
		b.WriteString("\n")
	}

	return b.String()
}

// renderInputInline renders the input prompt for inline mode
func (m *Model) renderInputInline() string {
	theme := m.styles.Theme()

	// During streaming, don't show input prompt (streaming indicator is shown instead)
	if m.streaming {
		return ""
	}

	var b strings.Builder

	// Breathing room before prompt
	b.WriteString("\n")

	// Show attached files if any
	if len(m.files) > 0 {
		var fileNames []string
		for _, f := range m.files {
			fileNames = append(fileNames, f.Name)
		}
		filesInfo := lipgloss.NewStyle().Foreground(theme.Secondary).Render(
			fmt.Sprintf("[with: %s]", strings.Join(fileNames, ", ")))
		b.WriteString(filesInfo)
		b.WriteString("\n")
	}

	// Input prompt
	b.WriteString("❯ ")
	b.WriteString(m.textarea.View())
	b.WriteString("\n")

	// Status line
	b.WriteString(m.renderStatusLine())

	return b.String()
}

// updateTextareaHeight adjusts textarea height based on content lines including wrapping
func (m *Model) updateTextareaHeight() {
	content := m.textarea.Value()
	textareaWidth := m.textarea.Width()
	if textareaWidth <= 0 {
		textareaWidth = m.width - 4
	}

	// Count visual lines (accounting for word wrap)
	visualLines := 0
	for _, line := range strings.Split(content, "\n") {
		lineLen := len([]rune(line))
		if lineLen == 0 {
			visualLines++
		} else {
			visualLines += (lineLen + textareaWidth - 1) / textareaWidth
		}
	}

	if visualLines < 1 {
		visualLines = 1
	}
	m.textarea.SetHeight(visualLines)
}

// renderStatusLine renders a tiny status line showing model and options
func (m *Model) renderStatusLine() string {
	theme := m.styles.Theme()
	mutedStyle := lipgloss.NewStyle().Foreground(theme.Muted)

	var parts []string

	// Provider and model - avoid duplication if provider already contains model info
	model := shortenModelName(m.modelName)
	if strings.Contains(m.providerName, "(") {
		// Provider already has model info in parens, just use it
		parts = append(parts, m.providerName)
	} else {
		parts = append(parts, fmt.Sprintf("%s (%s)", m.providerName, model))
	}

	// Web search status
	if m.searchEnabled {
		parts = append(parts, lipgloss.NewStyle().Foreground(theme.Success).Render("web:on"))
	}

	// File count if any
	if len(m.files) > 0 {
		parts = append(parts, fmt.Sprintf("%d file(s)", len(m.files)))
	}

	// Help tip (only show when no messages yet)
	if len(m.session.Messages) == 0 {
		parts = append(parts, "/help for commands")
	}

	return mutedStyle.Render(strings.Join(parts, " · "))
}

// shortenModelName removes date suffixes from model names (e.g., "claude-sonnet-4-20250514" -> "claude-sonnet-4")
func shortenModelName(name string) string {
	// Remove date suffix pattern like -20250514 or -20241022
	if len(name) > 9 {
		suffix := name[len(name)-9:]
		if suffix[0] == '-' && isAllDigits(suffix[1:]) {
			return name[:len(name)-9]
		}
	}
	return name
}

// isAllDigits checks if a string contains only digits
func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// getTerminalTitle returns the appropriate terminal title based on state
func (m *Model) getTerminalTitle() string {
	if m.streaming {
		elapsed := time.Since(m.streamStartTime)
		return fmt.Sprintf("term-llm · %s... (%.0fs)", m.phase, elapsed.Seconds())
	}

	msgCount := len(m.session.Messages)
	if msgCount == 0 {
		return "term-llm chat"
	}

	return fmt.Sprintf("term-llm · %d messages · %s", msgCount, m.modelName)
}

// overlayPopup positions the completions popup above the input
func (m *Model) overlayPopup(base, popup string) string {
	baseLines := strings.Split(base, "\n")
	popupLines := strings.Split(popup, "\n")

	// Insert popup before the last few lines (input + status bar)
	insertAt := len(baseLines) - 5 // Before input area
	if insertAt < 0 {
		insertAt = 0
	}

	var result []string
	result = append(result, baseLines[:insertAt]...)
	result = append(result, popupLines...)
	result = append(result, baseLines[insertAt:]...)

	return strings.Join(result, "\n")
}

// overlayDialog centers the dialog on screen
func (m *Model) overlayDialog(base, dialog string) string {
	baseLines := strings.Split(base, "\n")
	dialogLines := strings.Split(dialog, "\n")

	// Calculate position to center dialog
	dialogHeight := len(dialogLines)
	dialogWidth := 0
	for _, line := range dialogLines {
		if w := lipgloss.Width(line); w > dialogWidth {
			dialogWidth = w
		}
	}

	startY := (m.height - dialogHeight) / 2
	startX := (m.width - dialogWidth) / 2

	if startY < 0 {
		startY = 0
	}
	if startX < 0 {
		startX = 0
	}

	// Overlay dialog onto base
	for i, dialogLine := range dialogLines {
		y := startY + i
		if y >= len(baseLines) {
			break
		}

		baseLine := baseLines[y]
		// Simple overlay - replace portion of base line
		padding := strings.Repeat(" ", startX)
		if startX < len(baseLine) {
			// Keep beginning, insert dialog, keep end if any
			endX := startX + lipgloss.Width(dialogLine)
			if endX < len(baseLine) {
				baseLines[y] = baseLine[:startX] + dialogLine + baseLine[endX:]
			} else {
				baseLines[y] = baseLine[:startX] + dialogLine
			}
		} else {
			baseLines[y] = padding + dialogLine
		}
	}

	return strings.Join(baseLines, "\n")
}

func (m *Model) renderHeader() string {
	theme := m.styles.Theme()

	left := lipgloss.NewStyle().Bold(true).Render("term-llm chat")
	right := lipgloss.NewStyle().Foreground(theme.Muted).Render(m.modelName)

	// Calculate padding
	padding := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if padding < 1 {
		padding = 1
	}

	return left + strings.Repeat(" ", padding) + right
}

func (m *Model) renderHR() string {
	theme := m.styles.Theme()
	return lipgloss.NewStyle().Foreground(theme.Border).Render(strings.Repeat("─", m.width))
}

func (m *Model) renderHistory() string {
	if len(m.session.Messages) == 0 {
		return lipgloss.NewStyle().Foreground(m.styles.Theme().Muted).Render("No messages yet. Type your question and press Enter.\n\n")
	}

	var b strings.Builder
	theme := m.styles.Theme()

	// Determine which messages to show based on scroll offset
	// scrollOffset=0 means show all (bottom), higher values scroll up
	messages := m.session.Messages
	endIdx := len(messages)
	if m.scrollOffset > 0 {
		endIdx = len(messages) - m.scrollOffset
		if endIdx < 1 {
			endIdx = 1
		}
	}
	visibleMessages := messages[:endIdx]

	// Show scroll indicator if not at bottom
	if m.scrollOffset > 0 {
		scrollInfo := fmt.Sprintf("↑ Scrolled up %d message(s) · Press G to go to bottom", m.scrollOffset)
		b.WriteString(lipgloss.NewStyle().Foreground(theme.Warning).Render(scrollInfo))
		b.WriteString("\n\n")
	}

	for _, msg := range visibleMessages {
		// Role header
		b.WriteString(m.renderHR())
		b.WriteString("\n")

		roleStyle := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true)
		if msg.Role == RoleUser {
			header := "❯"
			// Show file attachment indicator
			if len(msg.Files) > 0 {
				header += fmt.Sprintf("  📎 %d file(s)", len(msg.Files))
			}
			b.WriteString(roleStyle.Render(header) + " ")
		} else {
			b.WriteString(roleStyle.Render("❯ Assistant"))
		}
		b.WriteString("\n")
		b.WriteString(m.renderHR())
		b.WriteString("\n")

		// Content - for user messages, extract just the text before file attachments
		displayContent := msg.Content
		if msg.Role == RoleUser && len(msg.Files) > 0 {
			// Extract content before the file attachment marker
			if idx := strings.Index(displayContent, "\n\n---\n**Attached files:**"); idx != -1 {
				displayContent = strings.TrimSpace(displayContent[:idx])
			}
		}

		rendered := m.renderMarkdown(displayContent)
		b.WriteString(rendered)

		// Show file names for user messages
		if msg.Role == RoleUser && len(msg.Files) > 0 {
			b.WriteString("\n")
			fileList := strings.Join(msg.Files, ", ")
			b.WriteString(lipgloss.NewStyle().Foreground(theme.Muted).Render("Files: " + fileList))
		}

		// Metadata for assistant messages
		if msg.Role == RoleAssistant && (msg.Tokens > 0 || msg.WebSearch) {
			var meta []string
			if msg.Tokens > 0 {
				meta = append(meta, fmt.Sprintf("%d tokens", msg.Tokens))
			}
			if msg.WebSearch {
				meta = append(meta, "web search")
			}
			metaStr := strings.Join(meta, " · ")
			padding := m.width - len(metaStr)
			if padding > 0 {
				b.WriteString(strings.Repeat(" ", padding))
			}
			b.WriteString(lipgloss.NewStyle().Foreground(theme.Muted).Render(metaStr))
		}

		b.WriteString("\n\n")
	}

	return b.String()
}

func (m *Model) renderStreamingResponse() string {
	var b strings.Builder
	theme := m.styles.Theme()

	b.WriteString(m.renderHR())
	b.WriteString("\n")

	// Role header with phase
	roleStyle := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true)
	b.WriteString(roleStyle.Render("> Assistant"))

	// Phase on the right
	elapsed := time.Since(m.streamStartTime)
	phaseStr := fmt.Sprintf("%s...", m.phase)
	padding := m.width - 12 - len(phaseStr) - 10 // Account for elapsed time
	if padding > 0 {
		b.WriteString(strings.Repeat(" ", padding))
	}
	b.WriteString(lipgloss.NewStyle().Foreground(theme.Muted).Render(phaseStr))

	b.WriteString("\n")
	b.WriteString(m.renderHR())
	b.WriteString("\n")

	// Spinner and content
	if m.currentResponse.Len() == 0 {
		// Still waiting for first token
		b.WriteString(m.spinner.View())
		b.WriteString(" ")
		b.WriteString(lipgloss.NewStyle().Foreground(theme.Muted).Render(fmt.Sprintf("%s... %.0fs", m.phase, elapsed.Seconds())))
	} else {
		// Render streamed content
		rendered := m.renderMarkdown(m.currentResponse.String())
		b.WriteString(rendered)
	}

	// Retry status if present
	if m.retryStatus != "" {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Foreground(theme.Warning).Render("⚠ " + m.retryStatus))
	}

	b.WriteString("\n")

	return b.String()
}

func (m *Model) renderInput() string {
	theme := m.styles.Theme()

	// Show attached files if any
	var filesLine string
	if len(m.files) > 0 {
		var fileNames []string
		for _, f := range m.files {
			fileNames = append(fileNames, fmt.Sprintf("📎 %s (%s)", f.Name, FormatFileSize(f.Size)))
		}
		filesLine = strings.Join(fileNames, "  ")
		filesLine = lipgloss.NewStyle().Foreground(theme.Secondary).Render(filesLine)
		filesLine += "  " + lipgloss.NewStyle().Foreground(theme.Muted).Render("[/file clear to remove]")
		filesLine += "\n" + m.renderHR() + "\n"
	}

	// Input prompt
	prompt := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true).Render("❯ ")

	if m.streaming {
		// Show disabled state during streaming
		return filesLine + prompt + lipgloss.NewStyle().Foreground(theme.Muted).Render("Waiting for response...") +
			"  " + lipgloss.NewStyle().Foreground(theme.Muted).Render("[Esc to cancel]")
	}

	return filesLine + prompt + m.textarea.View()
}

func (m *Model) renderStatusBar() string {
	theme := m.styles.Theme()
	mutedStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	keyStyle := lipgloss.NewStyle().Foreground(theme.Secondary)

	// Contextual hints based on state
	var hints []string

	if m.dialog.IsOpen() {
		// Dialog open
		hints = []string{
			keyStyle.Render("↑↓") + " navigate",
			keyStyle.Render("Enter") + " select",
			keyStyle.Render("Esc") + " cancel",
		}
	} else if m.completions.IsVisible() {
		// Completions visible
		hints = []string{
			keyStyle.Render("↑↓") + " navigate",
			keyStyle.Render("Tab") + " select",
			keyStyle.Render("Esc") + " cancel",
		}
	} else if m.streaming {
		// Streaming response
		hints = []string{
			keyStyle.Render("Esc") + " cancel",
		}
	} else if m.textarea.Value() == "" {
		// Empty input - vim navigation active
		hints = []string{
			keyStyle.Render("j/k") + " scroll",
			keyStyle.Render("g/G") + " top/bottom",
			keyStyle.Render("y") + " copy",
			keyStyle.Render("/") + " commands",
			keyStyle.Render("?") + " help",
		}
	} else {
		// Typing
		hints = []string{
			keyStyle.Render("Enter") + " send",
			keyStyle.Render("Ctrl+J") + " newline",
			keyStyle.Render("Esc") + " clear",
		}
	}

	left := mutedStyle.Render(strings.Join(hints, "  "))

	// Right side: status indicators
	var statusParts []string

	// File count if any
	if len(m.files) > 0 {
		statusParts = append(statusParts, lipgloss.NewStyle().Foreground(theme.Secondary).Render(
			fmt.Sprintf("📎 %d", len(m.files))))
	}

	// Web search status
	if m.searchEnabled {
		statusParts = append(statusParts, lipgloss.NewStyle().Foreground(theme.Success).Render("web: on"))
	} else {
		statusParts = append(statusParts, mutedStyle.Render("web: off"))
	}

	// Model name (abbreviated)
	modelDisplay := m.modelName
	if len(modelDisplay) > 20 {
		modelDisplay = modelDisplay[:17] + "..."
	}
	statusParts = append(statusParts, mutedStyle.Render(modelDisplay))

	right := strings.Join(statusParts, "  ")

	// Calculate padding
	padding := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if padding < 1 {
		padding = 1
	}

	return left + strings.Repeat(" ", padding) + right
}

func (m *Model) renderMarkdown(content string) string {
	if content == "" {
		return ""
	}

	style := styles.DraculaStyleConfig
	style.Document.Margin = uintPtr(0)
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = uintPtr(0)

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(m.width-2),
	)
	if err != nil {
		return content
	}

	rendered, err := renderer.Render(content)
	if err != nil {
		return content
	}

	return strings.TrimSpace(rendered)
}

func uintPtr(v uint) *uint {
	return &v
}

// attachFile attempts to attach a file, prompting for directory approval if needed
func (m *Model) attachFile(path string) (tea.Model, tea.Cmd) {
	// Check if the path is approved
	if !m.approvedDirs.IsPathApproved(path) {
		// Need approval - show dialog
		options := GetParentOptions(path)
		m.pendingFilePath = path
		m.dialog.ShowDirApproval(path, options)
		return m, nil
	}

	// Path is approved, attach the file
	attachment, err := AttachFile(path)
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to attach file: %v", err))
	}

	// Check if already attached
	for _, f := range m.files {
		if f.Path == attachment.Path {
			return m.showSystemMessage(fmt.Sprintf("File already attached: %s", attachment.Name))
		}
	}

	m.files = append(m.files, *attachment)
	m.textarea.SetValue("")
	return m.showSystemMessage(fmt.Sprintf("Attached: %s (%s)", attachment.Name, FormatFileSize(attachment.Size)))
}

// switchModel switches to a new provider:model
func (m *Model) switchModel(providerModel string) (tea.Model, tea.Cmd) {
	parts := strings.SplitN(providerModel, ":", 2)
	if len(parts) != 2 {
		return m.showSystemMessage(fmt.Sprintf("Invalid model format: %s", providerModel))
	}

	providerName := parts[0]
	modelName := parts[1]

	// Update config temporarily
	oldProvider := m.config.Provider
	m.config.Provider = providerName

	// Set model in the appropriate provider config
	switch providerName {
	case "anthropic":
		m.config.Anthropic.Model = modelName
	case "openai":
		m.config.OpenAI.Model = modelName
	case "gemini":
		m.config.Gemini.Model = modelName
	case "zen":
		m.config.Zen.Model = modelName
	case "ollama":
		m.config.Ollama.Model = modelName
	case "openrouter":
		m.config.OpenRouter.Model = modelName
	}

	// Create new provider
	provider, err := llm.NewProvider(m.config)
	if err != nil {
		// Restore old provider
		m.config.Provider = oldProvider
		return m.showSystemMessage(fmt.Sprintf("Failed to switch model: %v", err))
	}

	// Update model state
	m.provider = provider
	m.engine = llm.NewEngine(provider, nil)
	m.providerName = providerName
	m.modelName = modelName

	return m.showSystemMessage(fmt.Sprintf("Switched to %s (%s)", providerName, modelName))
}

// attachFiles attaches multiple files from a glob pattern
func (m *Model) attachFiles(pattern string) (tea.Model, tea.Cmd) {
	// Expand the glob pattern
	paths, err := ExpandGlob(pattern)
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to expand pattern: %v", err))
	}

	if len(paths) == 0 {
		return m.showSystemMessage(fmt.Sprintf("No files match pattern: %s", pattern))
	}

	// For multiple files, check approval first
	for _, path := range paths {
		if !m.approvedDirs.IsPathApproved(path) {
			// Need approval for this path - show dialog for first unapproved
			options := GetParentOptions(path)
			m.pendingFilePath = path
			m.dialog.ShowDirApproval(path, options)
			return m, nil
		}
	}

	// All paths approved, attach them
	var attached []string
	var totalSize int64
	for _, path := range paths {
		attachment, err := AttachFile(path)
		if err != nil {
			continue // Skip files that can't be read
		}

		// Check if already attached
		alreadyAttached := false
		for _, f := range m.files {
			if f.Path == attachment.Path {
				alreadyAttached = true
				break
			}
		}
		if !alreadyAttached {
			m.files = append(m.files, *attachment)
			attached = append(attached, attachment.Name)
			totalSize += attachment.Size
		}
	}

	if len(attached) == 0 {
		return m.showSystemMessage("No new files attached (all may already be attached or unreadable).")
	}

	m.textarea.SetValue("")
	if len(attached) == 1 {
		return m.showSystemMessage(fmt.Sprintf("Attached: %s (%s)", attached[0], FormatFileSize(totalSize)))
	}
	return m.showSystemMessage(fmt.Sprintf("Attached %d files (%s):\n- %s",
		len(attached), FormatFileSize(totalSize), strings.Join(attached, "\n- ")))
}

// clearFiles removes all attached files
func (m *Model) clearFiles() {
	m.files = nil
}

// copyLastResponse copies the last assistant response to clipboard
func (m *Model) copyLastResponse() (tea.Model, tea.Cmd) {
	// Find last assistant message
	for i := len(m.session.Messages) - 1; i >= 0; i-- {
		if m.session.Messages[i].Role == RoleAssistant {
			content := m.session.Messages[i].Content
			// Try to copy to clipboard using OSC 52 escape sequence
			// This works in most modern terminals
			return m, tea.Batch(
				func() tea.Msg {
					copyToClipboard(content)
					return nil
				},
				func() tea.Msg {
					return clipboardCopiedMsg{}
				},
			)
		}
	}
	return m.showSystemMessage("No assistant response to copy.")
}

// clipboardCopiedMsg signals clipboard copy completed
type clipboardCopiedMsg struct{}

// copyToClipboard uses OSC 52 escape sequence
func copyToClipboard(text string) {
	// OSC 52 clipboard escape sequence
	// Works in terminals that support it (iTerm2, Ghostty, kitty, etc.)
	encoded := base64Encode(text)
	fmt.Printf("\x1b]52;c;%s\x07", encoded)
}

// base64Encode encodes text for clipboard
func base64Encode(text string) string {
	return base64.StdEncoding.EncodeToString([]byte(text))
}
