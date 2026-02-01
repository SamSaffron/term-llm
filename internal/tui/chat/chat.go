package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	render "github.com/samsaffron/term-llm/internal/render/chat"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
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

	// Session state
	store     session.Store     // Session storage backend
	sess      *session.Session  // Current session
	messages  []session.Message // In-memory messages for current session
	streaming bool
	phase     string // "Thinking", "Searching", "Reading", "Responding"

	// Streaming state
	currentResponse  strings.Builder
	currentTokens    int
	streamStartTime  time.Time
	webSearchUsed    bool
	retryStatus      string
	streamCancelFunc context.CancelFunc
	tracker          *ui.ToolTracker     // Tool and segment tracking (shared component)
	subagentTracker  *ui.SubagentTracker // Subagent progress tracking

	// Streaming channels
	streamChan <-chan ui.StreamEvent

	// Smooth text buffer for 60fps rendering
	smoothBuffer *ui.SmoothBuffer

	// External UI state
	pausedForExternalUI bool // True when paused for ask_user or approval prompts

	// Embedded inline approval UI (alt screen mode only)
	approvalModel  *tools.ApprovalModel
	approvalDoneCh chan<- tools.ApprovalResult

	// Embedded inline ask_user UI (alt screen mode only)
	askUserModel  *tools.AskUserModel
	askUserDoneCh chan<- []tools.AskUserAnswer

	// LLM context
	provider     llm.Provider
	engine       *llm.Engine
	config       *config.Config
	providerName string
	modelName    string
	agentName    string

	// Pending message context
	files               []FileAttachment // Attached files for next message
	searchEnabled       bool             // Web search toggle
	forceExternalSearch bool             // Force external search tools even if provider supports native
	localTools          []string         // Names of enabled local tools (read, write, etc.)
	toolsStr            string           // Original tools setting (for session persistence)
	mcpStr              string           // Original MCP setting (for session persistence)

	// MCP (Model Context Protocol)
	mcpManager *mcp.Manager
	maxTurns   int

	// Directory approval
	approvedDirs    *ApprovedDirs
	pendingFilePath string // File waiting for directory approval

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

	// Stats tracking
	showStats bool
	stats     *ui.SessionStats

	// Inspector mode
	inspectorMode  bool
	inspectorModel *inspector.Model

	// Alt screen mode (full-screen rendering)
	altScreen      bool
	viewport       viewport.Model // Scrollable viewport for alt screen mode
	scrollToBottom bool           // Flag to scroll to bottom after response completes

	// Render cache for alt screen mode (avoids re-rendering unchanged content)
	viewCache struct {
		historyContent      string // Cached rendered history
		historyMsgCount     int    // Number of messages when cache was built
		historyWidth        int    // Width when cache was built
		historyScrollOffset int    // Scroll offset when cache was built
		historyValid        bool   // Whether cache has been populated
		lastViewportView    string // Cached viewport.View() output
		lastYOffset         int    // Viewport Y offset when view was cached
		lastVPWidth         int    // Viewport width when view was cached
		lastVPHeight        int    // Viewport height when view was cached
		// completedStream holds rendered streaming content (diffs, tools) that should
		// persist after streaming ends. Cleared when a new prompt is sent.
		completedStream string
		// Content versioning to avoid expensive string comparisons
		contentVersion      uint64 // Incremented when content changes
		lastRenderedVersion uint64 // Version that was last rendered to viewport
		lastTrackerVersion  uint64 // Last seen tracker.Version (to detect content changes)
		// Caching for streaming segments to avoid re-rendering on every frame
		cachedCompletedContent string // Rendered completed segments
		cachedTrackerVersion   uint64 // Tracker version when cache was built
		lastWavePos            int    // Last wave position for animation
	}

	// Cached glamour renderer (avoids expensive recreation during streaming)
	rendererCache struct {
		renderer *glamour.TermRenderer
		width    int
	}

	// New chat renderer (virtualized rendering for large histories)
	chatRenderer *render.Renderer

	// Auto-send mode (for benchmarking) - queue of messages to send
	autoSendQueue []string

	// Text mode (no markdown rendering)
	textMode bool
}

// streamEventMsg wraps ui.StreamEvent for bubbletea
type streamEventMsg struct {
	event ui.StreamEvent
}

// Messages for tea.Program
type (
	sessionSavedMsg  struct{}
	sessionLoadedMsg struct {
		sess     *session.Session
		messages []session.Message
	}
	tickMsg time.Time
)

// FlushBeforeAskUserMsg signals the TUI to flush content to scrollback
// before releasing the terminal for ask_user prompts.
type FlushBeforeAskUserMsg struct {
	Done chan<- struct{} // Signal when flush is complete
}

// FlushBeforeApprovalMsg signals the TUI to flush content to scrollback
// before releasing the terminal for approval prompts.
type FlushBeforeApprovalMsg struct {
	Done chan<- struct{} // Signal when flush is complete
}

// SubagentProgressMsg carries progress events from running subagents.
type SubagentProgressMsg struct {
	CallID string
	Event  tools.SubagentEvent
}

// ResumeFromExternalUIMsg signals that external UI (ask_user/approval) is done
type ResumeFromExternalUIMsg struct{}

// autoSendMsg triggers automatic message send (for benchmarking mode)
type autoSendMsg struct{}

// ApprovalRequestMsg triggers an inline approval prompt.
type ApprovalRequestMsg struct {
	Path    string
	IsWrite bool
	IsShell bool
	DoneCh  chan<- tools.ApprovalResult
}

// AskUserRequestMsg triggers an inline ask_user prompt.
type AskUserRequestMsg struct {
	Questions []tools.AskUserQuestion
	DoneCh    chan<- []tools.AskUserAnswer
}

// New creates a new chat model
func New(cfg *config.Config, provider llm.Provider, engine *llm.Engine, modelName string, mcpManager *mcp.Manager, maxTurns int, forceExternalSearch bool, searchEnabled bool, localTools []string, toolsStr string, mcpStr string, showStats bool, initialText string, store session.Store, sess *session.Session, altScreen bool, autoSendQueue []string, textMode bool, agentName string) *Model {
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

	// Create textarea with minimal styling for inline REPL
	ta := textarea.New()
	ta.Placeholder = "Type a message..."
	ta.Prompt = "❯ "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0 // No limit
	ta.SetWidth(width)
	ta.SetHeight(1) // Start with single line
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(styles.Theme().Muted)
	ta.FocusedStyle.EndOfBuffer = lipgloss.NewStyle()
	ta.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(styles.Theme().Primary).Bold(true)
	ta.BlurredStyle = ta.FocusedStyle
	ta.Focus()

	// Prefill with initial text if provided
	if initialText != "" {
		ta.SetValue(initialText)
	}

	// Use provided session or create a new one
	if sess == nil {
		sess = &session.Session{
			ID:        session.NewID(),
			Provider:  provider.Name(),
			Model:     modelName,
			Mode:      session.ModeChat,
			Agent:     agentName,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
			Search:    searchEnabled,
			Tools:     toolsStr,
			MCP:       mcpStr,
		}
		// Get current working directory
		if cwd, err := os.Getwd(); err == nil {
			sess.CWD = cwd
		}
		// Persist new session
		if store != nil {
			ctx := context.Background()
			_ = store.Create(ctx, sess)
			_ = store.SetCurrent(ctx, sess.ID)
		}
	}

	// Load existing messages if resuming
	var messages []session.Message
	if store != nil && sess.ID != "" {
		ctx := context.Background()
		if loadedMsgs, err := store.GetMessages(ctx, sess.ID, 0, 0); err == nil {
			messages = loadedMsgs
		}
	}

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

	subagentTracker := ui.NewSubagentTracker()
	// Set main provider/model for subagent comparison
	// Use cfg.DefaultProvider (e.g. "chatgpt") for cleaner display
	subagentTracker.SetMainProviderModel(cfg.DefaultProvider, modelName)

	// Create viewport for alt screen scrolling
	// Reserve space for input (3 lines) and status line (1 line)
	vpHeight := height - 4
	if vpHeight < 1 {
		vpHeight = 1
	}
	vp := viewport.New(width, vpHeight)
	vp.Style = lipgloss.NewStyle()

	// Create chat renderer for virtualized history rendering
	chatRenderer := render.NewRenderer(width, vpHeight)

	// Create tracker with text mode setting
	tracker := ui.NewToolTracker()
	tracker.TextMode = textMode

	return &Model{
		width:               width,
		height:              height,
		textarea:            ta,
		spinner:             s,
		styles:              styles,
		keyMap:              DefaultKeyMap(),
		store:               store,
		sess:                sess,
		messages:            messages,
		provider:            provider,
		engine:              engine,
		config:              cfg,
		providerName:        provider.Name(),
		modelName:           modelName,
		agentName:           agentName,
		phase:               "Thinking",
		viewportRows:        height - 8, // Reserve space for input and status
		tracker:             tracker,
		subagentTracker:     subagentTracker,
		smoothBuffer:        ui.NewSmoothBuffer(),
		completions:         completions,
		dialog:              dialog,
		approvedDirs:        approvedDirs,
		mcpManager:          mcpManager,
		maxTurns:            maxTurns,
		forceExternalSearch: forceExternalSearch,
		searchEnabled:       searchEnabled,
		localTools:          localTools,
		toolsStr:            toolsStr,
		mcpStr:              mcpStr,
		showStats:           showStats,
		stats:               ui.NewSessionStats(),
		altScreen:           altScreen,
		viewport:            vp,
		chatRenderer:        chatRenderer,
		autoSendQueue:       autoSendQueue,
		textMode:            textMode,
	}
}

// Init initializes the model
func (m *Model) Init() tea.Cmd {
	// Update textarea height for any initial text
	m.updateTextareaHeight()

	// Set markdown renderer for chat renderer
	if m.chatRenderer != nil {
		m.chatRenderer.SetMarkdownRenderer(m.renderMd)
	}

	// In auto-send mode, pop first message from queue and send it
	if len(m.autoSendQueue) > 0 {
		// Set textarea to first queued message
		m.textarea.SetValue(m.autoSendQueue[0])
		m.autoSendQueue = m.autoSendQueue[1:]
		m.updateTextareaHeight()
		return tea.Batch(
			textarea.Blink,
			m.spinner.Tick,
			func() tea.Msg { return autoSendMsg{} },
		)
	}

	return tea.Batch(
		textarea.Blink,
		m.spinner.Tick,
	)
}

// Update handles messages
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	// Handle inspector mode
	if m.inspectorMode {
		return m.updateInspectorMode(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewportRows = m.height - 8
		m.textarea.SetWidth(m.width)
		m.completions.SetSize(m.width, m.height)
		m.dialog.SetSize(m.width, m.height)

		// Invalidate cached markdown renderings since they are width-dependent
		if m.tracker != nil {
			for i := range m.tracker.Segments {
				m.tracker.Segments[i].Rendered = ""
				m.tracker.Segments[i].SafeRendered = ""
				m.tracker.Segments[i].SafePos = 0
				// Also clear diff caches (Issue 2: diff render cache invalidation)
				m.tracker.Segments[i].DiffRendered = ""
				m.tracker.Segments[i].DiffWidth = 0
				// Clear subagent diff caches
				for j := range m.tracker.Segments[i].SubagentDiffs {
					m.tracker.Segments[i].SubagentDiffs[j].Rendered = ""
					m.tracker.Segments[i].SubagentDiffs[j].Width = 0
				}
			}
			// Resize active streaming renderers
			m.tracker.ResizeStreamRenderers(m.width)
		}

		// Invalidate completed stream cache since it's width-dependent (Issue 1)
		m.viewCache.completedStream = ""
		m.viewCache.contentVersion++

		// Resize viewport for alt screen mode
		// Reserve space for input area (textarea + separators + status)
		vpHeight := m.height - 4
		if vpHeight < 1 {
			vpHeight = 1
		}
		m.viewport.Width = m.width
		m.viewport.Height = vpHeight

		// Propagate size to embedded dialogs if active
		if m.approvalModel != nil {
			m.approvalModel.SetWidth(m.width)
		}
		if m.askUserModel != nil {
			m.askUserModel.SetWidth(m.width)
		}

		// Update chat renderer size (invalidates cache)
		if m.chatRenderer != nil {
			m.chatRenderer.SetSize(m.width, vpHeight)
		}

		// In alt screen mode, just clear screen (View() renders history)
		// In inline mode, reprint history to scrollback after clearing
		if m.altScreen {
			return m, nil
		}
		if len(m.messages) > 0 {
			history := m.renderHistory()
			return m, tea.Sequence(tea.ClearScreen, tea.Println(history))
		}
		return m, tea.ClearScreen

	case tea.KeyMsg:
		return m.handleKeyMsg(msg)

	case tea.MouseMsg:
		// Forward mouse events to viewport in alt-screen mode for scroll wheel support
		if m.altScreen {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		return m, nil

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

	case autoSendMsg:
		// In auto-send mode, immediately send the initial message
		return m.sendMessage(m.textarea.Value())

	case ui.SmoothTickMsg:
		// Release buffered text word-by-word for smooth 60fps rendering
		if m.smoothBuffer != nil && m.streaming {
			words := m.smoothBuffer.NextWords()
			if words != "" {
				m.currentResponse.WriteString(words)
				if m.tracker != nil {
					m.tracker.AddTextSegment(words, m.width)
				}
				m.phase = "Responding"
				// Flush excess content if needed
				if m.scrollOffset == 0 {
					if cmd := m.maybeFlushToScrollback(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}
			// Continue ticking if not drained
			if !m.smoothBuffer.IsDrained() {
				cmds = append(cmds, ui.SmoothTick())
			}
		}

	case streamEventMsg:
		ev := msg.event

		switch ev.Type {
		case ui.StreamEventError:
			if ev.Err != nil {
				// Flush any buffered text on error
				if m.smoothBuffer != nil {
					remaining := m.smoothBuffer.FlushAll()
					if remaining != "" {
						m.currentResponse.WriteString(remaining)
						if m.tracker != nil {
							m.tracker.AddTextSegment(remaining, m.width)
						}
					}
					m.smoothBuffer.Reset()
				}
				m.streaming = false
				m.err = ev.Err

				// Clear callbacks and update status
				m.engine.SetResponseCompletedCallback(nil)
				m.engine.SetTurnCompletedCallback(nil)
				if m.store != nil {
					// Use interrupted for cancellation, error for other failures
					status := session.StatusError
					if errors.Is(ev.Err, context.Canceled) {
						status = session.StatusInterrupted
					}
					_ = m.store.UpdateStatus(context.Background(), m.sess.ID, status)
				}

				m.textarea.Focus()
				return m, nil
			}

		case ui.StreamEventToolStart:
			m.stats.ToolStart()

			// Flush smooth buffer before tool starts (user wants to see tool output right away)
			if m.smoothBuffer != nil {
				remaining := m.smoothBuffer.FlushAll()
				if remaining != "" {
					m.currentResponse.WriteString(remaining)
					if m.tracker != nil {
						m.tracker.AddTextSegment(remaining, m.width)
					}
				}
			}

			// Mark current text segment as complete before starting tool
			if m.tracker != nil {
				m.tracker.MarkCurrentTextComplete(func(text string) string {
					return m.renderMarkdown(text)
				})
				if m.tracker.HandleToolStart(ev.ToolCallID, ev.ToolName, ev.ToolInfo) {
					// New segment added, start wave animation (but not for ask_user which has its own UI)
					if ev.ToolName != tools.AskUserToolName {
						cmds = append(cmds, m.tracker.StartWave())
					}
				} else {
					// Already have pending segment, just restart wave (but not for ask_user)
					if ev.ToolName != tools.AskUserToolName {
						cmds = append(cmds, m.tracker.StartWave())
					}
				}
			}

			// Check for web search
			if ev.ToolName == llm.WebSearchToolName || ev.ToolName == "WebSearch" {
				m.webSearchUsed = true
			}

		case ui.StreamEventToolEnd:
			m.stats.ToolEnd()
			// Update segment status
			if m.tracker != nil {
				m.tracker.HandleToolEnd(ev.ToolCallID, ev.ToolSuccess)

				// Remove from subagent tracker when spawn_agent completes
				if m.subagentTracker != nil {
					m.subagentTracker.Remove(ev.ToolCallID)
				}

				// Back to thinking phase if no more pending tools
				if !m.tracker.HasPending() {
					m.phase = "Thinking"
				}

				// Flush completed segments (chronological order)
				if m.scrollOffset == 0 {
					if cmd := m.maybeFlushToScrollback(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}

		case ui.StreamEventUsage:
			if ev.InputTokens > 0 || ev.OutputTokens > 0 {
				m.stats.AddUsage(ev.InputTokens, ev.OutputTokens, ev.CachedTokens)
			}

		case ui.StreamEventText:
			// Buffer text for smooth 60fps rendering instead of immediate display
			if m.smoothBuffer != nil {
				m.smoothBuffer.Write(ev.Text)
				// Start smooth tick if not already running
				cmds = append(cmds, ui.SmoothTick())
			} else {
				// Fallback: direct display if no smooth buffer
				m.currentResponse.WriteString(ev.Text)
				if m.tracker != nil {
					m.tracker.AddTextSegment(ev.Text, m.width)
				}
			}

			m.phase = "Responding"
			m.retryStatus = ""

		case ui.StreamEventPhase:
			m.phase = ev.Phase
			m.retryStatus = ""

		case ui.StreamEventRetry:
			m.retryStatus = fmt.Sprintf("Rate limited (%d/%d), waiting %.0fs...",
				ev.RetryAttempt, ev.RetryMax, ev.RetryWait)

		case ui.StreamEventImage:
			// Add image segment for inline display
			if m.tracker != nil && ev.ImagePath != "" {
				m.tracker.AddImageSegment(ev.ImagePath)
				// Flush to scrollback so image appears
				if m.scrollOffset == 0 {
					if cmd := m.maybeFlushToScrollback(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}

		case ui.StreamEventDiff:
			// Add diff segment for inline display
			if m.tracker != nil && ev.DiffPath != "" {
				m.tracker.AddDiffSegment(ev.DiffPath, ev.DiffOld, ev.DiffNew, ev.DiffLine)
				// Flush to scrollback so diff appears
				if m.scrollOffset == 0 {
					if cmd := m.maybeFlushToScrollback(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}

		case ui.StreamEventDone:
			m.currentTokens = ev.Tokens

			// Flush any remaining buffered text and mark done
			if m.smoothBuffer != nil {
				remaining := m.smoothBuffer.FlushAll()
				if remaining != "" {
					m.currentResponse.WriteString(remaining)
					if m.tracker != nil {
						m.tracker.AddTextSegment(remaining, m.width)
					}
				}
				m.smoothBuffer.MarkDone()
			}

			m.streaming = false

			// Flag to scroll to bottom after response completes (alt screen mode)
			if m.altScreen {
				m.scrollToBottom = true
			}

			// Clear callbacks
			m.engine.SetResponseCompletedCallback(nil)
			m.engine.SetTurnCompletedCallback(nil)

			// Mark all text segments as complete and render
			if m.tracker != nil {
				m.tracker.CompleteTextSegments(func(text string) string {
					return m.renderMarkdown(text)
				})

				if m.altScreen {
					// In alt screen mode, save the full rendered content to completedStream.
					// This preserves the correct position of images/diffs relative to text.
					// The last assistant message will be skipped in renderHistory() to avoid duplication.
					completed := m.tracker.CompletedSegments()
					m.viewCache.completedStream = ui.RenderSegments(completed, m.width, -1, m.renderMd, true)
					m.viewCache.contentVersion++
				} else {
					// In inline mode, print remaining content to scrollback
					result := m.tracker.FlushAllRemaining(m.width, 0, m.renderMd)
					if result.ToPrint != "" {
						cmds = append(cmds, tea.Println(result.ToPrint))
					}
					cmds = append(cmds, tea.Println("")) // blank line
				}
			} else if !m.altScreen {
				cmds = append(cmds, tea.Println("")) // blank line
			}

			// Sync in-memory messages with persisted state
			if m.store != nil {
				// Reload from store to ensure consistency (callback saved messages incrementally)
				if loadedMsgs, err := m.store.GetMessages(context.Background(), m.sess.ID, 0, 0); err == nil {
					m.messages = loadedMsgs
				}
				_ = m.store.UpdateStatus(context.Background(), m.sess.ID, session.StatusComplete)
			} else {
				// No store - append locally for in-memory only sessions
				responseContent := m.currentResponse.String()
				if responseContent != "" {
					assistantMsg := session.Message{
						SessionID:   m.sess.ID,
						Role:        llm.RoleAssistant,
						Parts:       []llm.Part{{Type: llm.PartText, Text: responseContent}},
						TextContent: responseContent,
						CreatedAt:   time.Now(),
						Sequence:    len(m.messages),
					}
					m.messages = append(m.messages, assistantMsg)
				}
			}

			// Reset streaming state
			m.currentResponse.Reset()
			m.currentTokens = 0
			m.webSearchUsed = false
			m.retryStatus = ""
			m.tracker = ui.NewToolTracker() // Reset tracker
			m.tracker.TextMode = m.textMode // Preserve text mode setting
			if m.smoothBuffer != nil {
				m.smoothBuffer.Reset()
			}

			// Auto-save session
			cmds = append(cmds, m.saveSessionCmd())

			// In auto-send mode, check if there are more messages to send
			if m.autoSendQueue != nil {
				// Print per-message stats if enabled
				if m.showStats {
					elapsed := time.Since(m.streamStartTime)
					fmt.Printf("[Message %d] %.1fs\n", m.stats.LLMCallCount, elapsed.Seconds())
				}

				if len(m.autoSendQueue) > 0 {
					// Pop next message and send it
					m.textarea.SetValue(m.autoSendQueue[0])
					m.autoSendQueue = m.autoSendQueue[1:]
					m.updateTextareaHeight()
					return m.sendMessage(m.textarea.Value())
				}

				// Queue exhausted, quit
				m.quitting = true
				if m.showStats && m.stats.LLMCallCount > 0 {
					m.stats.Finalize()
					return m, tea.Sequence(tea.Println(m.stats.Render()), tea.Quit)
				}
				return m, tea.Quit
			}

			// Re-enable textarea
			m.textarea.Focus()
		}

		// Continue listening for more events unless we're done or got an error
		if ev.Type != ui.StreamEventDone && ev.Type != ui.StreamEventError {
			cmds = append(cmds, m.listenForStreamEvents())
		}

	case sessionSavedMsg:
		// Session saved successfully, nothing to do

	case sessionLoadedMsg:
		if msg.sess != nil {
			m.sess = msg.sess
			m.messages = msg.messages
			m.scrollOffset = 0
			if m.store != nil {
				_ = m.store.SetCurrent(context.Background(), m.sess.ID)
			}
		}

	case FlushBeforeAskUserMsg:
		// Set flag to suppress spinner in View() while external UI is active
		m.pausedForExternalUI = true

		// In alt screen mode, no flushing needed - just mark text complete and signal done.
		if m.altScreen {
			if m.tracker != nil {
				m.tracker.MarkCurrentTextComplete(func(text string) string {
					return m.renderMarkdown(text)
				})
			}
			close(msg.Done)
			return m, nil
		}

		// Partial flush - keep last maxViewLines visible for after external UI returns
		if m.tracker != nil {
			// Mark current text as complete
			m.tracker.MarkCurrentTextComplete(func(text string) string {
				return m.renderMarkdown(text)
			})

			// Partial flush - keep some context visible
			result := m.tracker.FlushBeforeExternalUI(m.width, 0, maxViewLines, m.renderMd)
			if result.ToPrint != "" {
				// Signal that flush is complete after the print finishes
				return m, tea.Sequence(
					tea.Println(result.ToPrint),
					func() tea.Msg {
						close(msg.Done)
						return nil
					},
				)
			}
		}
		// Nothing to flush, signal done immediately
		close(msg.Done)
		return m, nil

	case FlushBeforeApprovalMsg:
		// Set flag to suppress spinner in View() while approval UI is active
		m.pausedForExternalUI = true

		// In alt screen mode, no flushing needed - just mark text complete and signal done.
		if m.altScreen {
			if m.tracker != nil {
				m.tracker.MarkCurrentTextComplete(func(text string) string {
					return m.renderMarkdown(text)
				})
			}
			close(msg.Done)
			return m, nil
		}

		// Partial flush - keep some context visible for after external UI returns
		if m.tracker != nil {
			// Mark current text as complete
			m.tracker.MarkCurrentTextComplete(func(text string) string {
				return m.renderMarkdown(text)
			})

			// Partial flush - keep some context visible
			result := m.tracker.FlushBeforeExternalUI(m.width, 0, maxViewLines, m.renderMd)
			if result.ToPrint != "" {
				// Signal that flush is complete after the print finishes
				return m, tea.Sequence(
					tea.Println(result.ToPrint),
					func() tea.Msg {
						close(msg.Done)
						return nil
					},
				)
			}
		}
		// Nothing to flush, signal done immediately
		close(msg.Done)
		return m, nil

	case ResumeFromExternalUIMsg:
		// Resume from external UI (ask_user or approval)
		m.pausedForExternalUI = false

		// Check if there's an ask_user summary to display
		// Add to tracker so it appears in correct order, then flush immediately
		if summary := tools.GetAndClearAskUserResult(); summary != "" && m.tracker != nil {
			m.tracker.AddExternalUIResult(summary)
			// Flush now to ensure it's printed in correct sequence
			if cmd := m.maybeFlushToScrollback(); cmd != nil {
				return m, tea.Batch(cmd, m.spinner.Tick)
			}
		}

		return m, m.spinner.Tick

	case ApprovalRequestMsg:
		// In alt screen mode, render approval UI inline
		if m.altScreen {
			m.pausedForExternalUI = true
			m.approvalDoneCh = msg.DoneCh
			if msg.IsShell {
				m.approvalModel = tools.NewEmbeddedShellApprovalModel(msg.Path, m.width)
			} else {
				m.approvalModel = tools.NewEmbeddedApprovalModel(msg.Path, msg.IsWrite, m.width)
			}
			// Mark current text as complete so it shows above the approval UI
			if m.tracker != nil {
				m.tracker.MarkCurrentTextComplete(func(text string) string {
					return m.renderMarkdown(text)
				})
			}
			return m, nil
		}
		// Non-alt screen mode: shouldn't happen, but fall back to immediate deny
		msg.DoneCh <- tools.ApprovalResult{Choice: tools.ApprovalChoiceCancelled, Cancelled: true}
		return m, nil

	case AskUserRequestMsg:
		// In alt screen mode, render ask_user UI inline
		if m.altScreen {
			m.pausedForExternalUI = true
			m.askUserDoneCh = msg.DoneCh
			m.askUserModel = tools.NewEmbeddedAskUserModel(msg.Questions, m.width)
			// Mark current text as complete so it shows above the ask_user UI
			if m.tracker != nil {
				m.tracker.MarkCurrentTextComplete(func(text string) string {
					return m.renderMarkdown(text)
				})
			}
			return m, nil
		}
		// Non-alt screen mode: shouldn't happen, but fall back to cancelled
		msg.DoneCh <- nil
		return m, nil

	case SubagentProgressMsg:
		// Handle subagent progress events and update segment stats
		ui.HandleSubagentProgress(m.tracker, m.subagentTracker, msg.CallID, msg.Event)
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
