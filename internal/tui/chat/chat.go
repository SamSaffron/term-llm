package chat

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
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
		lastViewport        string // Last content set to viewport
		lastViewportView    string // Cached viewport.View() output
		lastYOffset         int    // Viewport Y offset when view was cached
		lastVPWidth         int    // Viewport width when view was cached
		lastVPHeight        int    // Viewport height when view was cached
		// completedStream holds rendered streaming content (diffs, tools) that should
		// persist after streaming ends. Cleared when a new prompt is sent.
		completedStream string
	}

	// Cached glamour renderer (avoids expensive recreation during streaming)
	rendererCache struct {
		renderer *glamour.TermRenderer
		width    int
	}
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

// Use ui.WaveTickMsg and ui.WavePauseMsg from the shared ToolTracker

// New creates a new chat model
func New(cfg *config.Config, provider llm.Provider, engine *llm.Engine, modelName string, mcpManager *mcp.Manager, maxTurns int, forceExternalSearch bool, searchEnabled bool, localTools []string, toolsStr string, mcpStr string, showStats bool, initialText string, store session.Store, sess *session.Session, altScreen bool) *Model {
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
		providerName:        cfg.DefaultProvider,
		modelName:           modelName,
		phase:               "Thinking",
		viewportRows:        height - 8, // Reserve space for input and status
		tracker:             ui.NewToolTracker(),
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
	}
}

// Init initializes the model
func (m *Model) Init() tea.Cmd {
	// Update textarea height for any initial text
	m.updateTextareaHeight()
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
			if m.smoothBuffer != nil {
				m.smoothBuffer.Reset()
			}

			// Auto-save session
			cmds = append(cmds, m.saveSessionCmd())

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

	case clipboardCopiedMsg:
		// Show brief confirmation - add as system message
		return m.showSystemMessage("Copied last response to clipboard.")

	case FlushBeforeAskUserMsg:
		// Set flag to suppress spinner in View() while external UI is active
		m.pausedForExternalUI = true

		// In alt screen mode, no flushing needed - just mark text complete and signal done.
		// NOTE: External UIs (ask_user prompts) are handed off via ReleaseTerminal/RestoreTerminal
		// in cmd/chat.go. The external UI inherits cooked mode from ReleaseTerminal(), but the
		// cursor visibility state may vary. If external UIs appear to have cursor issues,
		// consider explicitly showing the cursor before handoff.
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
		// NOTE: Approval UIs are handed off via ReleaseTerminal/RestoreTerminal in cmd/chat.go.
		// The external UI inherits cooked mode from ReleaseTerminal(), but the cursor visibility
		// state may vary. If approval UIs appear to have cursor issues, consider explicitly
		// showing the cursor before handoff.
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

// updateInspectorMode handles updates while in inspector mode
func (m *Model) updateInspectorMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Pass to inspector
		if m.inspectorModel != nil {
			m.inspectorModel, _ = m.inspectorModel.Update(msg)
		}
		return m, nil

	case tea.KeyMsg:
		// Pass to inspector
		if m.inspectorModel != nil {
			var cmd tea.Cmd
			m.inspectorModel, cmd = m.inspectorModel.Update(msg)
			return m, cmd
		}
		return m, nil

	case inspector.CloseMsg:
		// Exit inspector mode
		m.inspectorMode = false
		m.inspectorModel = nil
		m.textarea.Focus()
		// Only exit alt screen if chat isn't in alt screen mode
		if !m.altScreen {
			return m, tea.ExitAltScreen
		}
		return m, nil

	default:
		// Pass through to inspector
		if m.inspectorModel != nil {
			var cmd tea.Cmd
			m.inspectorModel, cmd = m.inspectorModel.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

func (m *Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Handle embedded approval UI first if active
	if m.approvalModel != nil {
		done := m.approvalModel.UpdateEmbedded(msg)
		if done {
			result := m.approvalModel.Result()
			// Add summary to tracker for display
			if m.tracker != nil && !result.Cancelled {
				m.tracker.AddExternalUIResult(m.approvalModel.RenderSummary())
			}
			// Send result and clean up
			m.approvalDoneCh <- result
			m.approvalModel = nil
			m.approvalDoneCh = nil
			m.pausedForExternalUI = false
			return m, m.spinner.Tick
		}
		return m, nil
	}

	// Handle embedded ask_user UI first if active
	if m.askUserModel != nil {
		cmd := m.askUserModel.UpdateEmbedded(msg)
		if m.askUserModel.IsDone() || m.askUserModel.IsCancelled() {
			// Add summary to tracker for display
			if m.tracker != nil && !m.askUserModel.IsCancelled() {
				m.tracker.AddExternalUIResult(m.askUserModel.RenderPlainSummary())
			}
			// Send result and clean up
			if m.askUserModel.IsCancelled() {
				m.askUserDoneCh <- nil
			} else {
				m.askUserDoneCh <- m.askUserModel.Answers()
			}
			m.askUserModel = nil
			m.askUserDoneCh = nil
			m.pausedForExternalUI = false
			return m, m.spinner.Tick
		}
		if cmd != nil {
			return m, cmd
		}
		return m, nil
	}

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
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "ctrl+c"))):
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

		// MCP picker supports typing to filter and toggle without closing
		if m.dialog.Type() == DialogMCPPicker {
			switch {
			case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
				selected := m.dialog.Selected()
				if selected != nil {
					// Toggle the selected MCP server
					name := selected.ID
					status, _ := m.mcpManager.ServerStatus(name)
					if status == "ready" || status == "starting" {
						m.mcpManager.Disable(name)
					} else {
						m.mcpManager.Enable(context.Background(), name)
					}
					// Refresh the picker to show updated state (stays open!)
					// Preserve query and cursor position
					query := m.dialog.Query()
					cursor := m.dialog.Cursor()
					m.dialog.ShowMCPPicker(m.mcpManager)
					m.dialog.SetQuery(query)
					m.dialog.SetCursor(cursor)
				}
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("esc", "ctrl+c"))):
				m.dialog.Close()
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k", "ctrl+p"))):
				m.dialog.Update(msg)
				return m, nil
			case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j", "ctrl+n"))):
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

		// Other dialogs (SessionList, DirApproval) use standard handling
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
				m.setTextareaValue("")
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
				m.setTextareaValue("/" + selected.Name + " ")
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
				m.setTextareaValue(value[:len(value)-1])
				m.updateCompletions()
			} else if len(value) == 1 {
				m.setTextareaValue("")
				m.completions.Hide()
			}
			return m, nil
		default:
			// Add character to query
			if len(msg.String()) == 1 {
				m.setTextareaValue(m.textarea.Value() + msg.String())
				m.updateCompletions()
				return m, nil
			}
		}
	}

	// Handle quit
	if key.Matches(msg, m.keyMap.Quit) {
		if m.streaming && m.streamCancelFunc != nil {
			// Flush buffered text on cancel
			if m.smoothBuffer != nil {
				m.smoothBuffer.FlushAll()
				m.smoothBuffer.Reset()
			}
			m.streamCancelFunc()
			m.streaming = false

			// Clear callbacks and update status
			m.engine.SetResponseCompletedCallback(nil)
			m.engine.SetTurnCompletedCallback(nil)
			if m.store != nil {
				_ = m.store.UpdateStatus(context.Background(), m.sess.ID, session.StatusInterrupted)
			}

			return m, nil
		}
		m.quitting = true
		// Print stats if enabled
		if m.showStats && m.stats.LLMCallCount > 0 {
			m.stats.Finalize()
			return m, tea.Sequence(tea.Println(m.stats.Render()), tea.Quit)
		}
		return m, tea.Quit
	}

	// Handle cancel during streaming
	if key.Matches(msg, m.keyMap.Cancel) {
		if m.streaming && m.streamCancelFunc != nil {
			// Flush buffered text on cancel
			if m.smoothBuffer != nil {
				m.smoothBuffer.FlushAll()
				m.smoothBuffer.Reset()
			}
			m.streamCancelFunc()
			m.streaming = false

			// Clear callbacks and update status
			m.engine.SetResponseCompletedCallback(nil)
			m.engine.SetTurnCompletedCallback(nil)
			if m.store != nil {
				_ = m.store.UpdateStatus(context.Background(), m.sess.ID, session.StatusInterrupted)
			}

			m.textarea.Focus()
			return m, nil
		}
		// Clear input if not empty
		if m.textarea.Value() != "" {
			m.setTextareaValue("")
		}
		return m, nil
	}

	// Handle inspector view (Ctrl+O) - works even during streaming
	if key.Matches(msg, m.keyMap.Inspector) {
		// Only open inspector if we have messages
		if len(m.messages) > 0 {
			// Collect tool specs for the inspector
			var toolSpecs []llm.ToolSpec
			if m.mcpManager != nil {
				for _, t := range m.mcpManager.AllTools() {
					toolSpecs = append(toolSpecs, llm.ToolSpec{
						Name:        t.Name,
						Description: t.Description,
						Schema:      t.Schema,
					})
				}
			}
			if len(m.localTools) > 0 {
				for _, specName := range m.localTools {
					if tool, ok := m.engine.Tools().Get(specName); ok {
						toolSpecs = append(toolSpecs, tool.Spec())
					}
				}
			}

			cfg := &inspector.Config{
				ProviderName: m.providerName,
				ModelName:    m.modelName,
				ToolSpecs:    toolSpecs,
			}
			m.inspectorMode = true
			m.inspectorModel = inspector.NewWithConfig(m.messages, m.width, m.height, m.styles, m.store, cfg)
			// Only enter alt screen if chat isn't already in alt screen mode
			if !m.altScreen {
				return m, tea.EnterAltScreen
			}
			return m, nil
		}
		return m, nil
	}

	// Allow viewport scrolling even while streaming (in alt screen mode)
	if m.altScreen {
		if key.Matches(msg, m.keyMap.PageUp) {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		if key.Matches(msg, m.keyMap.PageDown) {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		// Arrow keys scroll viewport when textarea is empty
		if m.textarea.Value() == "" {
			// Scroll faster during streaming when content is out of view
			// This provides smooth scrolling through history while new content streams in
			scrollAmount := 1
			if m.streaming && !m.viewport.AtBottom() {
				scrollAmount = 5
			}
			if key.Matches(msg, m.keyMap.HistoryUp) {
				m.viewport.LineUp(scrollAmount)
				return m, nil
			}
			if key.Matches(msg, m.keyMap.HistoryDown) {
				m.viewport.LineDown(scrollAmount)
				return m, nil
			}
		}
	}

	// During streaming: allow typing but block send
	if m.streaming {
		// Block send key
		if key.Matches(msg, m.keyMap.Send) {
			return m, nil
		}
		// Allow textarea to receive input
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		m.updateTextareaHeight()
		return m, cmd
	}

	// Handle command palette (Ctrl+P)
	if key.Matches(msg, m.keyMap.Commands) {
		m.setTextareaValue("/")
		m.completions.Show()
		return m, nil
	}

	// Handle model picker (Ctrl+L)
	if key.Matches(msg, m.keyMap.SwitchModel) {
		m.dialog.ShowModelPicker(m.modelName, GetAvailableProviders(m.config))
		return m, nil
	}

	// Handle new session (Ctrl+N)
	if key.Matches(msg, m.keyMap.NewSession) {
		return m.cmdNew()
	}

	// Handle MCP picker (Ctrl+M)
	if key.Matches(msg, m.keyMap.MCPPicker) {
		if m.mcpManager == nil {
			return m.showSystemMessage("MCP not initialized.")
		}
		if len(m.mcpManager.AvailableServers()) == 0 {
			return m.showMCPQuickStart()
		}
		m.dialog.ShowMCPPicker(m.mcpManager)
		return m, nil
	}

	// Handle clear
	if key.Matches(msg, m.keyMap.Clear) {
		return m.cmdClear()
	}

	// Handle newline (must be before Send since shift+enter contains enter)
	if key.Matches(msg, m.keyMap.Newline) || key.Matches(msg, m.keyMap.NewlineAlt) {
		m.textarea.InsertString("\n")
		m.updateTextareaHeight()
		return m, nil
	}

	// Handle tab completion for /mcp commands
	if key.Matches(msg, key.NewBinding(key.WithKeys("tab"))) {
		value := m.textarea.Value()
		valueLower := strings.ToLower(value)

		// Tab completion for /mcp add <server> (from bundled servers)
		if strings.HasPrefix(valueLower, "/mcp add ") {
			partial := strings.TrimSpace(value[9:]) // after "/mcp add "
			if partial != "" {
				bundled := mcp.GetBundledServers()
				partialLower := strings.ToLower(partial)

				var match string
				for _, s := range bundled {
					if strings.HasPrefix(strings.ToLower(s.Name), partialLower) {
						match = s.Name
						break
					}
				}
				if match == "" {
					for _, s := range bundled {
						if strings.Contains(strings.ToLower(s.Name), partialLower) {
							match = s.Name
							break
						}
					}
				}
				if match != "" {
					m.setTextareaValue("/mcp add " + match)
				}
			}
			return m, nil
		}

		// Tab completion for /mcp start <server> (from configured servers)
		if strings.HasPrefix(valueLower, "/mcp start ") && m.mcpManager != nil {
			partial := strings.TrimSpace(value[11:]) // after "/mcp start "
			if partial != "" {
				if match := m.mcpFindServerMatch(partial); match != "" {
					m.setTextareaValue("/mcp start " + match)
				}
			}
			return m, nil
		}

		// Tab completion for /mcp stop <server> (from configured servers)
		if strings.HasPrefix(valueLower, "/mcp stop ") && m.mcpManager != nil {
			partial := strings.TrimSpace(value[10:]) // after "/mcp stop "
			if partial != "" {
				if match := m.mcpFindServerMatch(partial); match != "" {
					m.setTextareaValue("/mcp stop " + match)
				}
			}
			return m, nil
		}

		// Tab completion for /mcp restart <server> (from configured servers)
		if strings.HasPrefix(valueLower, "/mcp restart ") && m.mcpManager != nil {
			partial := strings.TrimSpace(value[13:]) // after "/mcp restart "
			if partial != "" {
				if match := m.mcpFindServerMatch(partial); match != "" {
					m.setTextareaValue("/mcp restart " + match)
				}
			}
			return m, nil
		}

		return m, nil
	}

	// Handle send
	if key.Matches(msg, m.keyMap.Send) {
		content := strings.TrimSpace(m.textarea.Value())

		// Check for backslash continuation
		if strings.HasSuffix(content, "\\") {
			// Remove backslash and insert newline
			m.setTextareaValue(strings.TrimSuffix(content, "\\") + "\n")
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
		m.setTextareaValue("/")
		m.completions.Show()
		return m, nil
	}

	// Handle web toggle (Ctrl+S)
	if key.Matches(msg, m.keyMap.ToggleWeb) {
		m.toggleSearch()
		return m, nil
	}

	// Page up/down for scrolling (inline mode only - alt screen handled above)
	if !m.altScreen {
		if key.Matches(msg, m.keyMap.PageUp) {
			totalMessages := len(m.messages)
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

	// Create user message and store it
	userMsg := &session.Message{
		SessionID:   m.sess.ID,
		Role:        llm.RoleUser,
		Parts:       []llm.Part{{Type: llm.PartText, Text: fullContent}},
		TextContent: fullContent,
		CreatedAt:   time.Now(),
		Sequence:    -1, // Auto-allocate sequence
	}
	m.messages = append(m.messages, *userMsg)
	if m.store != nil {
		_ = m.store.AddMessage(context.Background(), m.sess.ID, userMsg)
		_ = m.store.IncrementUserTurns(context.Background(), m.sess.ID)
		m.sess.UserTurns++ // Keep in-memory value in sync
		// Update session summary from first user message
		if m.sess.Summary == "" {
			m.sess.Summary = session.TruncateSummary(content)
			_ = m.store.Update(context.Background(), m.sess)
		}
	}

	// Print user message permanently to scrollback (inline mode)
	theme := m.styles.Theme()
	promptStyle := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true)
	prompt := promptStyle.Render("❯") + " "
	promptWidth := 2 // "❯ " is 2 cells

	// Wrap content to fit terminal width minus prompt
	wrapWidth := m.width - promptWidth
	if wrapWidth < 20 {
		wrapWidth = 20
	}
	wrappedContent := wordwrap.String(content, wrapWidth)

	// Add prompt to first line, indent continuation lines
	lines := strings.Split(wrappedContent, "\n")
	var userDisplay strings.Builder
	for i, line := range lines {
		if i == 0 {
			userDisplay.WriteString(prompt)
		} else {
			userDisplay.WriteString("\n  ") // 2-space indent for continuation
		}
		userDisplay.WriteString(line)
	}
	if len(fileNames) > 0 {
		userDisplay.WriteString("\n")
		userDisplay.WriteString(lipgloss.NewStyle().Foreground(theme.Muted).Render(
			fmt.Sprintf("[with: %s]", strings.Join(fileNames, ", "))))
	}
	// tea.Println adds newline, no need for extra

	// Clear input and files
	m.setTextareaValue("")
	m.files = nil

	// Start streaming
	m.streaming = true
	m.phase = "Thinking"
	m.streamStartTime = time.Now()
	m.currentResponse.Reset()
	m.err = nil // Clear any previous error
	m.webSearchUsed = false
	m.viewCache.completedStream = "" // Clear previous response's diffs/tools
	if m.smoothBuffer != nil {
		m.smoothBuffer.Reset()
	}

	// Start the stream
	// In alt screen mode, View() renders history including user message
	// In inline mode, print user message to scrollback first
	if m.altScreen {
		return m, tea.Batch(
			m.startStream(fullContent),
			m.spinner.Tick,
			m.tickEvery(),
		)
	}
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

		// Mark session as active when starting a new stream
		if m.store != nil && m.sess != nil {
			_ = m.store.UpdateStatus(ctx, m.sess.ID, session.StatusActive)
		}

		// Create stream adapter for unified event handling with proper buffering
		adapter := ui.NewStreamAdapter(ui.DefaultStreamBufferSize)
		m.streamChan = adapter.Events()

		// Build messages from conversation history
		messages := m.buildMessages()

		// Collect MCP tools if available and register them with the engine
		var reqTools []llm.ToolSpec
		if m.mcpManager != nil {
			mcpTools := m.mcpManager.AllTools()
			for _, t := range mcpTools {
				reqTools = append(reqTools, llm.ToolSpec{
					Name:        t.Name,
					Description: t.Description,
					Schema:      t.Schema,
				})
				// Register MCP tool with engine for execution
				m.engine.RegisterTool(mcp.NewMCPTool(m.mcpManager, t))
			}
		}

		// Add local tools (read_file, write_file, shell, etc.) if enabled
		// These are already registered in the engine, we just need their specs
		if len(m.localTools) > 0 {
			for _, specName := range m.localTools {
				if tool, ok := m.engine.Tools().Get(specName); ok {
					reqTools = append(reqTools, tool.Spec())
				}
			}
		}

		req := llm.Request{
			Messages:            messages,
			Tools:               reqTools,
			Search:              m.searchEnabled,
			ForceExternalSearch: m.forceExternalSearch,
			ParallelToolCalls:   true,
			MaxTurns:            m.maxTurns,
		}

		// Set up callbacks for incremental message saving (sequence auto-allocated)
		// Capture streamStartTime for duration calculation
		streamStart := m.streamStartTime
		if m.store != nil && m.sess != nil {
			// Response callback saves assistant message immediately (before tool execution)
			// This ensures the message is persisted even if tool execution fails/crashes
			m.engine.SetResponseCompletedCallback(func(ctx context.Context, turnIndex int, assistantMsg llm.Message, metrics llm.TurnMetrics) error {
				// Calculate duration from stream start
				durationMs := time.Since(streamStart).Milliseconds()

				sessionMsg := session.NewMessage(m.sess.ID, assistantMsg, -1)
				sessionMsg.DurationMs = durationMs
				_ = m.store.AddMessage(ctx, m.sess.ID, sessionMsg)
				return nil
			})

			// Turn callback saves tool result messages and updates metrics
			m.engine.SetTurnCompletedCallback(func(ctx context.Context, turnIndex int, turnMessages []llm.Message, metrics llm.TurnMetrics) error {
				// Save messages (tool results, or assistant message when no tools were executed)
				for _, msg := range turnMessages {
					sessionMsg := session.NewMessage(m.sess.ID, msg, -1)
					// Set duration for assistant messages (when responseCallback didn't run)
					if msg.Role == llm.RoleAssistant {
						sessionMsg.DurationMs = time.Since(streamStart).Milliseconds()
					}
					_ = m.store.AddMessage(ctx, m.sess.ID, sessionMsg)
				}
				// Update metrics
				_ = m.store.UpdateMetrics(ctx, m.sess.ID, 1, metrics.ToolCalls, metrics.InputTokens, metrics.OutputTokens)
				return nil
			})
		}

		// Start streaming in background - adapter handles all event conversion
		go func() {
			stream, err := m.engine.Stream(ctx, req)
			if err != nil {
				adapter.EmitErrorAndClose(err)
				return
			}
			defer stream.Close()
			// ProcessStream handles all events and closes the channel when done
			adapter.ProcessStream(ctx, stream)
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
		return streamEventMsg{event: ui.DoneEvent(0)}
	}

	event, ok := <-m.streamChan
	if !ok {
		return streamEventMsg{event: ui.DoneEvent(0)}
	}
	return streamEventMsg{event: event}
}

func (m *Model) buildMessages() []llm.Message {
	var messages []llm.Message

	// Add system instructions if configured
	if m.config.Chat.Instructions != "" {
		messages = append(messages, llm.SystemText(m.config.Chat.Instructions))
	}

	// Add conversation history - convert session messages to llm messages
	for _, msg := range m.messages {
		messages = append(messages, msg.ToLLMMessage())
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
		// Sessions are now auto-saved via the store
		// This is kept for compatibility but does nothing
		return sessionSavedMsg{}
	}
}

// View renders the model
func (m *Model) View() string {
	if m.quitting {
		return ""
	}

	// Inspector mode uses alternate screen
	if m.inspectorMode && m.inspectorModel != nil {
		return m.inspectorModel.View()
	}

	// Set terminal title
	title := m.getTerminalTitle()
	titleSeq := fmt.Sprintf("\x1b]0;%s\x07", title)

	// Alt screen mode: use viewport for scrollable content
	if m.altScreen {
		return titleSeq + m.viewAltScreen()
	}

	// Inline mode: traditional rendering
	var b strings.Builder

	// History (if scrolling)
	if m.scrollOffset > 0 {
		b.WriteString(m.renderHistory())
		b.WriteString("\n")
	}

	// Streaming response (if active)
	if m.streaming {
		b.WriteString(m.renderStreamingInline())
	}

	// Error display (if error occurred and not streaming)
	if m.err != nil && !m.streaming {
		b.WriteString(m.renderError())
		b.WriteString("\n\n")
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

	return titleSeq + b.String()
}

// viewAltScreen renders the full-screen alt screen view with scrollable viewport
func (m *Model) viewAltScreen() string {
	var b strings.Builder

	// Build scrollable content with caching to avoid re-rendering unchanged content

	// Check if history cache is valid
	historyValid := m.viewCache.historyValid &&
		m.viewCache.historyMsgCount == len(m.messages) &&
		m.viewCache.historyWidth == m.width &&
		m.viewCache.historyScrollOffset == m.scrollOffset
	if !historyValid {
		m.viewCache.historyContent = m.renderHistory()
		m.viewCache.historyMsgCount = len(m.messages)
		m.viewCache.historyWidth = m.width
		m.viewCache.historyScrollOffset = m.scrollOffset
		m.viewCache.historyValid = true
	}

	// Build combined content
	var contentStr string
	if m.streaming {
		// During streaming, combine history + streaming content
		streamingContent := m.renderStreamingInline()
		contentStr = m.viewCache.historyContent + streamingContent

		// If an embedded UI is active, append it to the content
		if m.approvalModel != nil {
			contentStr += "\n" + m.approvalModel.View()
		} else if m.askUserModel != nil {
			contentStr += "\n" + m.askUserModel.View()
		}
	} else {
		// Include any completed streaming content (diffs, tools) that was saved
		// when the last stream ended. This persists until a new prompt is sent.
		contentStr = m.viewCache.historyContent + m.viewCache.completedStream

		// Display error if one occurred
		if m.err != nil {
			contentStr += "\n" + m.renderError() + "\n"
		}
	}

	// Only call SetContent if content actually changed (expensive operation)
	contentChanged := contentStr != m.viewCache.lastViewport
	if contentChanged {
		// Check if user is at bottom BEFORE setting content (which changes maxYOffset)
		wasAtBottom := m.viewport.AtBottom()
		m.viewport.SetContent(contentStr)
		m.viewCache.lastViewport = contentStr
		// Only auto-scroll to bottom if user was already at bottom
		// This allows users to scroll up and read during streaming
		// GotoBottom() is expensive as it calls visibleLines() internally
		if m.streaming && wasAtBottom {
			m.viewport.GotoBottom()
		}
	}

	// Scroll to bottom after response completes (regardless of previous scroll position)
	if m.scrollToBottom {
		m.viewport.GotoBottom()
		m.scrollToBottom = false
	}

	// Cache viewport.View() output - only regenerate if content, scroll position, or size changed
	// Check YOffset after GotoBottom() since it modifies the offset
	yOffsetChanged := m.viewport.YOffset != m.viewCache.lastYOffset
	sizeChanged := m.viewport.Width != m.viewCache.lastVPWidth || m.viewport.Height != m.viewCache.lastVPHeight
	needViewRender := contentChanged || yOffsetChanged || sizeChanged || m.viewCache.lastViewportView == ""
	if needViewRender {
		m.viewCache.lastViewportView = m.viewport.View()
		m.viewCache.lastYOffset = m.viewport.YOffset
		m.viewCache.lastVPWidth = m.viewport.Width
		m.viewCache.lastVPHeight = m.viewport.Height
	}

	// Render viewport (scrollable area)
	b.WriteString(m.viewCache.lastViewportView)
	b.WriteString("\n")

	// Completions popup (if visible) - overlaid on content
	if m.completions.IsVisible() {
		b.WriteString(m.completions.View())
		b.WriteString("\n")
	}

	// Dialog (if open) - overlaid on content
	if m.dialog.IsOpen() {
		b.WriteString(m.dialog.View())
		b.WriteString("\n")
	}

	// Input area (fixed at bottom)
	b.WriteString(m.renderInputInline())

	return b.String()
}

func (m *Model) renderMd(text string, width int) string {
	if text == "" {
		return ""
	}
	return m.renderMarkdown(text)
}

// maxViewLines is the maximum number of lines to keep in View().
// Content beyond this is printed to scrollback to prevent scroll issues.
const maxViewLines = 8

// maybeFlushToScrollback checks if there are segments to flush to scrollback,
// keeping View() small to avoid terminal scroll issues.
// In alt screen mode, we never flush to scrollback since View() renders everything.
func (m *Model) maybeFlushToScrollback() tea.Cmd {
	if m.altScreen || m.tracker == nil {
		return nil
	}

	result := m.tracker.FlushToScrollback(m.width, 0, maxViewLines, m.renderMd)
	if result.ToPrint != "" {
		return tea.Println(result.ToPrint)
	}
	return nil
}

// renderStreamingInline renders the streaming response for inline mode
func (m *Model) renderStreamingInline() string {
	var b strings.Builder

	// Get segments from tracker (excludes flushed segments)
	var completed, active []*ui.Segment
	if m.tracker != nil {
		completed = m.tracker.CompletedSegments()
		active = m.tracker.ActiveSegments()
	}

	// Render completed segments (segment-based tracking handles what's already flushed)
	// In alt screen mode, include images since we never flush to scrollback
	content := ui.RenderSegments(completed, m.width, -1, m.renderMd, m.altScreen)

	if content != "" {
		b.WriteString(content)
		b.WriteString("\n")
	}

	// Show the indicator with current phase, unless paused for external UI
	if !m.pausedForExternalUI {
		wavePos := 0
		if m.tracker != nil {
			wavePos = m.tracker.WavePos
		}
		indicator := ui.StreamingIndicator{
			Spinner:        m.spinner.View(),
			Phase:          m.phase,
			Elapsed:        time.Since(m.streamStartTime),
			Tokens:         m.currentTokens,
			ShowCancel:     true,
			HideProgress:   true, // progress shown in status line instead
			Segments:       active,
			WavePos:        wavePos,
			Width:          m.width,
			RenderMarkdown: m.renderMd,
		}
		b.WriteString(indicator.Render(m.styles))
		b.WriteString("\n")

		// Retry status if present (shown as warning on separate line)
		if m.retryStatus != "" {
			b.WriteString(lipgloss.NewStyle().Foreground(m.styles.Theme().Warning).Render("⚠ " + m.retryStatus))
			b.WriteString("\n")
		}
	}

	return b.String()
}

// renderInputInline renders the input prompt for inline mode
func (m *Model) renderInputInline() string {
	theme := m.styles.Theme()

	var b strings.Builder

	// Separator line above input (no extra newline - content already has one)
	separator := lipgloss.NewStyle().Foreground(theme.Muted).Render(strings.Repeat("─", m.width))
	b.WriteString(separator)

	// Show attached files if any
	if len(m.files) > 0 {
		b.WriteString("\n")
		var fileNames []string
		for _, f := range m.files {
			fileNames = append(fileNames, f.Name)
		}
		filesInfo := lipgloss.NewStyle().Foreground(theme.Secondary).Render(
			fmt.Sprintf("[with: %s]", strings.Join(fileNames, ", ")))
		b.WriteString(filesInfo)
	}

	// Input prompt
	b.WriteString("\n")
	b.WriteString(m.textarea.View())
	b.WriteString("\n")

	// Separator line below input
	b.WriteString(separator)
	b.WriteString("\n")

	// Status line
	b.WriteString(m.renderStatusLine())

	return b.String()
}

// renderError renders the error message when m.err is set
func (m *Model) renderError() string {
	if m.err == nil {
		return ""
	}

	// User cancellation gets a softer message
	if errors.Is(m.err, context.Canceled) {
		return m.styles.Muted.Render("(cancelled)")
	}

	// API errors: red circle + red error text
	errMsg := m.err.Error()
	return ui.ErrorCircle() + " " + m.styles.Error.Render("Error: "+errMsg)
}

// updateTextareaHeight adjusts textarea height based on content lines including wrapping
func (m *Model) updateTextareaHeight() {
	content := m.textarea.Value()
	textareaWidth := m.textarea.Width()
	if textareaWidth <= 0 {
		textareaWidth = m.width
	}

	// Account for prompt "❯ " (2 cells)
	effectiveWidth := textareaWidth - 2
	if effectiveWidth <= 0 {
		effectiveWidth = 1
	}

	// Count visual lines (accounting for word wrap)
	visualLines := 0
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		lineLen := runewidth.StringWidth(line)
		if lineLen == 0 {
			visualLines++
		} else {
			visualLines += (lineLen + effectiveWidth - 1) / effectiveWidth
		}
	}

	if visualLines < 1 {
		visualLines = 1
	}

	// Limit height to about 1/3 of the screen or at least 5 lines
	maxHeight := m.height / 3
	if maxHeight < 5 {
		maxHeight = 5
	}
	if visualLines > maxHeight {
		visualLines = maxHeight
	}

	m.textarea.SetHeight(visualLines)
}

// setTextareaValue sets the textarea value and updates its height for proper wrapping
func (m *Model) setTextareaValue(s string) {
	m.textarea.SetValue(s)
	m.updateTextareaHeight()
}

// renderStatusLine renders a tiny status line showing model and options
func (m *Model) renderStatusLine() string {
	theme := m.styles.Theme()
	mutedStyle := lipgloss.NewStyle().Foreground(theme.Muted)
	successStyle := lipgloss.NewStyle().Foreground(theme.Success)

	const sep = " · "

	// Build fixed parts first (these are always shown as-is)
	var fixedParts []string

	// Provider:model format (e.g. "chatgpt:gpt-5.2-codex")
	model := shortenModelName(m.modelName)
	if model == "" {
		fixedParts = append(fixedParts, m.providerName)
	} else {
		fixedParts = append(fixedParts, fmt.Sprintf("%s:%s", m.providerName, model))
	}

	// Web search status
	if m.searchEnabled {
		fixedParts = append(fixedParts, successStyle.Render("web:on"))
	}

	// File count if any
	if len(m.files) > 0 {
		fixedParts = append(fixedParts, fmt.Sprintf("%d file(s)", len(m.files)))
	}

	// During streaming, add progress info
	var streamingPart string
	if m.streaming {
		elapsed := time.Since(m.streamStartTime)
		progressParts := []string{m.spinner.View() + " " + m.phase}
		if m.currentTokens > 0 {
			progressParts = append(progressParts, fmt.Sprintf("%d tok", m.currentTokens))
		}
		progressParts = append(progressParts, fmt.Sprintf("%.1fs", elapsed.Seconds()))
		streamingPart = strings.Join(progressParts, " ")
	}

	// Calculate used width from fixed parts
	// Use lipgloss.Width which properly strips ANSI escape sequences
	fixedWidth := 0
	for i, p := range fixedParts {
		fixedWidth += lipgloss.Width(p)
		if i > 0 {
			fixedWidth += len(sep)
		}
	}
	if streamingPart != "" {
		fixedWidth += len(sep) + lipgloss.Width(streamingPart)
	}

	// Calculate available width for tools and mcp
	availableWidth := m.width - fixedWidth

	// Build tools string - use full names if they fit, otherwise abbreviate
	var toolsPart string
	if len(m.localTools) > 0 {
		if len(m.localTools) == len(tools.AllToolNames()) {
			toolsPart = successStyle.Render("tools:all")
		} else {
			fullTools := "tools:" + strings.Join(m.localTools, ",")
			shortTools := fmt.Sprintf("tools:%d", len(m.localTools))
			// Account for separator
			needed := len(sep) + len(fullTools)
			if needed <= availableWidth {
				toolsPart = successStyle.Render(fullTools)
			} else {
				toolsPart = successStyle.Render(shortTools)
			}
		}
	}

	// Build mcp string - use full names if they fit, otherwise abbreviate
	var mcpPart string
	if m.mcpManager != nil {
		available := m.mcpManager.AvailableServers()
		if len(available) > 0 {
			enabled := m.mcpManager.EnabledServers()
			if len(enabled) > 0 {
				fullMcp := "mcp:" + strings.Join(enabled, ",")
				shortMcp := fmt.Sprintf("mcp:%d", len(enabled))
				// Account for separator and tools part
				usedByTools := 0
				if toolsPart != "" {
					usedByTools = len(sep) + runewidth.StringWidth(toolsPart)
				}
				needed := len(sep) + len(fullMcp)
				if needed <= availableWidth-usedByTools {
					mcpPart = successStyle.Render(fullMcp)
				} else {
					mcpPart = successStyle.Render(shortMcp)
				}
			} else {
				mcpPart = mutedStyle.Render("mcp:off")
			}
		} else if len(m.messages) == 0 {
			// Show hint for new users on empty conversation
			mcpPart = mutedStyle.Render("Ctrl+T:mcp")
		}
	}

	// Combine all parts
	var parts []string
	parts = append(parts, fixedParts...)
	if toolsPart != "" {
		parts = append(parts, toolsPart)
	}
	if mcpPart != "" {
		parts = append(parts, mcpPart)
	}
	if streamingPart != "" {
		parts = append(parts, streamingPart)
	}

	return mutedStyle.Render(strings.Join(parts, sep))
}

// mcpFindServerMatch finds the best matching server name for tab completion
func (m *Model) mcpFindServerMatch(partial string) string {
	if m.mcpManager == nil {
		return ""
	}
	available := m.mcpManager.AvailableServers()
	partialLower := strings.ToLower(partial)

	// Try prefix match first
	for _, s := range available {
		if strings.HasPrefix(strings.ToLower(s), partialLower) {
			return s
		}
	}
	// Try contains match
	for _, s := range available {
		if strings.Contains(strings.ToLower(s), partialLower) {
			return s
		}
	}
	return ""
}

// updateCompletions updates the completions popup based on current input
// Handles both static command completions and dynamic server completions
func (m *Model) updateCompletions() {
	value := m.textarea.Value()
	query := strings.TrimPrefix(value, "/")

	// Check for MCP server argument completions
	// /mcp start <server>, /mcp stop <server>, /mcp add <server>
	lowerQuery := strings.ToLower(query)

	// Check for "/mcp start ", "/mcp stop ", "/mcp restart " - show configured servers
	if strings.HasPrefix(lowerQuery, "mcp start ") ||
		strings.HasPrefix(lowerQuery, "mcp stop ") ||
		strings.HasPrefix(lowerQuery, "mcp restart ") {
		if m.mcpManager != nil {
			// Extract the partial server name after the subcommand
			parts := strings.SplitN(query, " ", 3)
			partial := ""
			if len(parts) >= 3 {
				partial = strings.ToLower(parts[2])
			}

			// Get configured servers
			servers := m.mcpManager.AvailableServers()
			var items []Command
			for _, s := range servers {
				if partial == "" || strings.Contains(strings.ToLower(s), partial) {
					status, _ := m.mcpManager.ServerStatus(s)
					desc := "stopped"
					if status == "ready" {
						desc = "running"
					} else if status == "starting" {
						desc = "starting..."
					}
					items = append(items, Command{
						Name:        parts[0] + " " + parts[1] + " " + s,
						Description: desc,
					})
				}
			}
			m.completions.SetItems(items)
			return
		}
	}

	// Check for "/mcp add " - show bundled servers not yet configured
	if strings.HasPrefix(lowerQuery, "mcp add ") {
		bundled := mcp.GetBundledServers()

		// Get already configured servers
		configured := make(map[string]bool)
		if m.mcpManager != nil {
			for _, s := range m.mcpManager.AvailableServers() {
				configured[strings.ToLower(s)] = true
			}
		}

		// Extract partial name
		parts := strings.SplitN(query, " ", 3)
		partial := ""
		if len(parts) >= 3 {
			partial = strings.ToLower(parts[2])
		}

		var items []Command
		for _, s := range bundled {
			if configured[strings.ToLower(s.Name)] {
				continue // Skip already configured
			}
			if partial == "" || strings.Contains(strings.ToLower(s.Name), partial) {
				items = append(items, Command{
					Name:        "mcp add " + s.Name,
					Description: s.Description,
				})
			}
			if len(items) >= 15 { // Limit to avoid huge list
				break
			}
		}
		m.completions.SetItems(items)
		return
	}

	// Default: use standard command filtering
	m.completions.SetQuery(query)
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

	msgCount := len(m.messages)
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
	if len(m.messages) == 0 {
		return lipgloss.NewStyle().Foreground(m.styles.Theme().Muted).Render("No messages yet. Type your question and press Enter.\n\n")
	}

	var b strings.Builder
	theme := m.styles.Theme()

	// Determine which messages to show based on scroll offset
	// scrollOffset=0 means show all (bottom), higher values scroll up
	messages := m.messages
	endIdx := len(messages)
	if m.scrollOffset > 0 {
		endIdx = len(messages) - m.scrollOffset
		if endIdx < 1 {
			endIdx = 1
		}
	}
	visibleMessages := messages[:endIdx]

	// In alt screen mode, skip the last assistant message if completedStream is showing it
	// (completedStream contains the full rendered response with images in correct position)
	if m.altScreen && m.viewCache.completedStream != "" && len(visibleMessages) > 0 {
		if visibleMessages[len(visibleMessages)-1].Role == llm.RoleAssistant {
			visibleMessages = visibleMessages[:len(visibleMessages)-1]
		}
	}

	// Show scroll indicator if not at bottom
	if m.scrollOffset > 0 {
		scrollInfo := fmt.Sprintf("↑ Scrolled up %d message(s) · Press G to go to bottom", m.scrollOffset)
		b.WriteString(lipgloss.NewStyle().Foreground(theme.Warning).Render(scrollInfo))
		b.WriteString("\n\n")
	}

	promptStyle := lipgloss.NewStyle().
		Foreground(theme.Primary).
		Bold(true).
		Background(theme.UserMsgBg)
	userMsgStyle := lipgloss.NewStyle().Background(theme.UserMsgBg)

	for _, msg := range visibleMessages {
		if msg.Role == llm.RoleUser {
			// User message: ❯ content (with background)
			// Extract content before file attachments for display
			displayContent := msg.TextContent
			if idx := strings.Index(displayContent, "\n\n---\n**Attached files:**"); idx != -1 {
				displayContent = strings.TrimSpace(displayContent[:idx])
			}

			// Wrap content to fit terminal width minus prompt
			promptWidth := 2 // "❯ " is 2 cells
			wrapWidth := m.width - promptWidth
			if wrapWidth < 20 {
				wrapWidth = 20
			}
			wrappedContent := wordwrap.String(displayContent, wrapWidth)

			// Render with prompt on first line, indent continuation lines
			lines := strings.Split(wrappedContent, "\n")
			for i, line := range lines {
				if i == 0 {
					b.WriteString(promptStyle.Render("❯ "))
				} else {
					b.WriteString(userMsgStyle.Render("  ")) // 2-space indent for continuation
				}
				b.WriteString(userMsgStyle.Render(line))
				b.WriteString("\n")
			}
			b.WriteString("\n")
		} else {
			// Assistant message: render all parts (text + tool calls)
			hasContent := false
			for _, part := range msg.Parts {
				switch part.Type {
				case llm.PartText:
					if part.Text != "" {
						rendered := m.renderMarkdown(part.Text)
						b.WriteString(rendered)
						hasContent = true
					}
				case llm.PartToolCall:
					if part.ToolCall != nil {
						b.WriteString(ui.RenderToolCallFromPart(part.ToolCall))
						b.WriteString("\n")
						hasContent = true
					}
					// Skip PartToolResult - they're in user messages and verbose
				}
			}
			// Fallback: if no parts rendered, use TextContent (for backward compatibility)
			if !hasContent && msg.TextContent != "" {
				rendered := m.renderMarkdown(msg.TextContent)
				b.WriteString(rendered)
			}
			b.WriteString("\n")
		}
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
	if m.streaming {
		theme := m.styles.Theme()
		prompt := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true).Render("❯ ")
		// Show disabled state during streaming
		return filesLine + prompt + lipgloss.NewStyle().Foreground(theme.Muted).Render("Waiting for response...") +
			"  " + lipgloss.NewStyle().Foreground(theme.Muted).Render("[Esc to cancel]")
	}

	return filesLine + m.textarea.View()
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
			keyStyle.Render("Shift+Enter") + " newline",
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

	targetWidth := m.width - 2

	// Reuse cached renderer if width matches
	if m.rendererCache.renderer != nil && m.rendererCache.width == targetWidth {
		rendered, err := m.rendererCache.renderer.Render(content)
		if err != nil {
			return content
		}
		return strings.TrimSpace(rendered)
	}

	// Create new renderer and cache it
	style := ui.GlamourStyle()
	margin := uint(0)
	style.Document.Margin = &margin
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = &margin

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(targetWidth),
	)
	if err != nil {
		return content
	}

	m.rendererCache.renderer = renderer
	m.rendererCache.width = targetWidth

	rendered, err := renderer.Render(content)
	if err != nil {
		return content
	}

	return strings.TrimSpace(rendered)
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

	// Create new provider using the centralized factory
	provider, err := llm.NewProviderByName(m.config, providerName, modelName)
	if err != nil {
		return m.showSystemMessage(fmt.Sprintf("Failed to switch model: %v", err))
	}

	// Update model state
	m.provider = provider
	// Preserve existing tool registry when creating new engine
	m.engine = llm.NewEngine(provider, m.engine.Tools())
	m.providerName = providerName
	m.modelName = modelName

	return m.showSystemMessage(fmt.Sprintf("Switched to %s:%s", providerName, modelName))
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
	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].Role == llm.RoleAssistant {
			content := m.messages[i].TextContent
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
