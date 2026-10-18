package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
	"github.com/samsaffron/term-llm/internal/ui"
	"golang.org/x/term"
)

// getTerminalWidth returns the terminal width or a default
func getTerminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		return 80 // default
	}
	return width
}

// askStreamModel is a bubbletea model for streaming ask responses
type askStreamModel struct {
	spinner spinner.Model
	styles  *ui.Styles
	width   int
	height  int

	// Tool and segment tracking (shared component)
	tracker          *ui.ToolTracker
	mediaByReference map[string]llm.MediaArtifact

	// Smooth text buffer for word-by-word rendering (shared with chat)
	smoothBuffer       *ui.SmoothBuffer
	smoothTickPending  bool
	deferredStreamRead bool
	streamChan         <-chan ui.StreamEvent
	streamErr          error

	// Subagent progress tracking
	subagentTracker *ui.SubagentTracker

	// State flags
	done bool // True when streaming is complete (prevents spinner from showing)

	// Status display
	retryStatus string    // Retry status (e.g., "Rate limited (2/5), waiting 5s...")
	startTime   time.Time // For elapsed time display
	totalTokens int       // Total tokens (input + output) used
	phase       string    // Current engine phase (Thinking, Searching, etc.)

	// Reasoning status title state. Ask keeps summaries out of stdout/plain text
	// by default, but can use safe summary titles for the live TUI status.
	reasoningConfig        config.ReasoningConfig
	currentReasoning       *strings.Builder
	currentReasoningItemID string
	currentReasoningTitle  string
	reasoningPhaseActive   bool

	// External UI state
	pausedForExternalUI bool // True when paused for ask_user or approval prompts

	// Approval prompt state (using huh form)
	approvalForm       *huh.Form
	approvalDesc       string
	approvalToolInfo   string      // Info for the tool that triggered approval (to avoid duplicates)
	approvalResponseCh chan<- bool // channel to send y/n response back to tool

	// Session for inspector (to fetch messages)
	store session.Store
	sess  *session.Session

	// Inspector mode
	inspectorMode  bool
	inspectorModel *inspector.Model

	// View caching (performance optimization - Issue 4)
	// Avoids rebuilding content on every spinner tick
	contentDirty           bool   // True when new content arrived, needs re-render
	cachedContent          string // Cached rendered content from last dirty=true render
	adaptiveFlushThreshold bool   // Scales flush threshold with smooth buffer size when enabled

	// Tool-start boundary gating: defer pending tool-row rendering until
	// pre-tool completed content has been visibly flushed to scrollback.
	pendingBoundaryFlushes int
	pendingWaveStart       bool
}

const askAdaptiveFlushThresholdEnv = "TERM_LLM_ASK_ADAPTIVE_FLUSH"

const (
	askFlushBufferThresholdMedium = 400
	askFlushBufferThresholdLarge  = 1000
	askFlushBufferThresholdXLarge = 2000

	askFlushThresholdMedium = 32
	askFlushThresholdLarge  = 64
	askFlushThresholdXLarge = 128
)

type askContentMsg string
type askAttemptDiscardMsg struct{}
type askStreamEventMsg struct {
	event ui.StreamEvent
}
type askDoneMsg struct{}
type askCancelledMsg struct{}
type askUsageMsg struct {
	InputTokens  int
	OutputTokens int
}
type askTickMsg time.Time
type askToolStartMsg struct {
	CallID   string          // Unique ID for this tool invocation
	Name     string          // Tool name being executed
	Info     string          // Additional info (e.g., URL)
	ToolArgs json.RawMessage // Raw args JSON
}
type askToolEndMsg struct {
	CallID  string // Unique ID for this tool invocation
	Success bool   // Whether the tool succeeded
}
type askRetryMsg struct {
	Attempt     int
	MaxAttempts int
	WaitSecs    float64
}
type askReasoningMsg ui.StreamEvent
type askPhaseMsg string
type askImageMsg string // Image path to display
type askDiffMsg struct {
	Path      string
	Old       string
	New       string
	Line      int
	Operation string
}
type askBoundaryFlushedMsg struct {
	CallID string
	Name   string
}
type askFlushBeforeAskUserMsg struct {
	Done chan<- struct{} // Signal when flush is complete
}
type askFlushBeforeApprovalMsg struct {
	Done chan<- struct{} // Signal when flush is complete
}
type askResumeFromExternalUIMsg struct{}
type askApprovalRequestMsg struct {
	Description string
	ToolName    string
	ToolInfo    string
	ResponseCh  chan<- bool
}

type askGuardianReviewMsg struct {
	Event tools.GuardianEvent
}

// Subagent progress messages
type askSubagentProgressMsg struct {
	CallID string
	Event  tools.SubagentEvent
}

// Use ui.WaveTickMsg and ui.WavePauseMsg from the shared ToolTracker

func newAskStreamModel() askStreamModel {
	width := getTerminalWidth()
	height := 24
	if _, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		height = h
	}
	styles := ui.DefaultStyles()

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styles.Spinner

	return askStreamModel{
		spinner:                s,
		styles:                 styles,
		width:                  width,
		height:                 height,
		tracker:                ui.NewToolTracker(),
		smoothBuffer:           ui.NewSmoothBuffer(),
		subagentTracker:        ui.NewSubagentTracker(),
		startTime:              time.Now(),
		reasoningConfig:        config.DefaultReasoningConfig(),
		adaptiveFlushThreshold: ui.ParseBoolDefault(os.Getenv(askAdaptiveFlushThresholdEnv), true),
	}
}

func NewRendererProgram(cfg *config.Config, toolMgr *tools.ToolManager, store session.Store, sess *session.Session, events <-chan ui.StreamEvent) (askStreamModel, *tea.Program) {
	model := newAskStreamModel()
	model.store = store
	model.sess = sess
	model.streamChan = events
	if cfg != nil {
		model.reasoningConfig = cfg.ResolveReasoning("ask")
	}

	mainModelName := ""
	if providerCfg := cfg.GetActiveProviderConfig(); providerCfg != nil {
		mainModelName = providerCfg.Model
	}
	model.subagentTracker.SetMainProviderModel(cfg.DefaultProvider, mainModelName)

	teaProgram := tea.NewProgram(model, tea.WithoutSignalHandler())
	if toolMgr == nil {
		return model, teaProgram
	}

	if spawnTool := toolMgr.GetSpawnAgentTool(); spawnTool != nil {
		send := func(callID string, event tools.SubagentEvent) {
			teaProgram.Send(askSubagentProgressMsg{CallID: callID, Event: event})
		}
		if SubagentProgressCallback != nil {
			spawnTool.SetEventCallback(SubagentProgressCallback(send))
		} else {
			spawnTool.SetEventCallback(send)
		}
	}

	toolMgr.ApprovalMgr.PromptUIFunc = func(path string, isWrite bool, isShell bool, workDir string) (tools.ApprovalResult, error) {
		done := make(chan struct{})
		teaProgram.Send(askFlushBeforeApprovalMsg{Done: done})
		<-done

		teaProgram.ReleaseTerminal()
		defer func() {
			teaProgram.RestoreTerminal()
			teaProgram.Send(askResumeFromExternalUIMsg{})
		}()

		if isShell {
			return tools.RunShellApprovalUI(path, workDir)
		}
		return tools.RunFileApprovalUI(path, isWrite)
	}
	toolMgr.ApprovalMgr.WorkspacePromptFunc = func(workspace string) (tools.WorkspaceApprovalResult, error) {
		done := make(chan struct{})
		teaProgram.Send(askFlushBeforeApprovalMsg{Done: done})
		<-done
		teaProgram.ReleaseTerminal()
		defer func() {
			teaProgram.RestoreTerminal()
			teaProgram.Send(askResumeFromExternalUIMsg{})
		}()
		return tools.RunWorkspaceApprovalUI(workspace)
	}

	start, end := tools.CreateTUIHooks(teaProgram, func() {
		done := make(chan struct{})
		teaProgram.Send(askFlushBeforeAskUserMsg{Done: done})
		<-done
	})
	originalEnd := end
	end = func() {
		originalEnd()
		teaProgram.Send(askResumeFromExternalUIMsg{})
	}
	tools.SetAskUserHooks(start, end)

	return model, teaProgram
}

func (m *askStreamModel) streamingFlushThreshold() int {
	if !m.adaptiveFlushThreshold || m.smoothBuffer == nil {
		return 0
	}

	bufferLen := m.smoothBuffer.Len()
	// As buffered text grows, require larger safe chunks before flushing to
	// scrollback to reduce high-frequency partial render churn on long outputs.
	switch {
	case bufferLen >= askFlushBufferThresholdXLarge:
		return askFlushThresholdXLarge
	case bufferLen >= askFlushBufferThresholdLarge:
		return askFlushThresholdLarge
	case bufferLen >= askFlushBufferThresholdMedium:
		return askFlushThresholdMedium
	default:
		return 0
	}
}

func (m *askStreamModel) currentReasoningString() string {
	if m.currentReasoning == nil {
		return ""
	}
	return m.currentReasoning.String()
}

func (m *askStreamModel) reasoningBuilder() *strings.Builder {
	if m.currentReasoning == nil {
		m.currentReasoning = &strings.Builder{}
	}
	return m.currentReasoning
}

func (m *askStreamModel) resetCurrentReasoning() {
	if m.currentReasoning != nil {
		m.currentReasoning.Reset()
	}
	m.currentReasoningItemID = ""
	m.currentReasoningTitle = ""
	m.reasoningPhaseActive = false
}

func (m askStreamModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spinner.Tick, m.tickEvery()}
	if m.streamChan != nil {
		cmds = append(cmds, m.listenForStreamEvents())
	}
	return tea.Batch(cmds...)
}

// tickEvery returns a command that sends a tick every second for elapsed time updates.
func (m askStreamModel) tickEvery() tea.Cmd {
	return tea.Tick(1*time.Second, func(t time.Time) tea.Msg {
		return askTickMsg(t)
	})
}

func (m askStreamModel) listenForStreamEvents() tea.Cmd {
	return func() tea.Msg {
		if m.streamChan == nil {
			return askStreamEventMsg{event: ui.DoneEvent(0)}
		}
		event, ok := <-m.streamChan
		if !ok {
			return askStreamEventMsg{event: ui.DoneEvent(0)}
		}
		return askStreamEventMsg{event: event}
	}
}

// maxViewLines is the maximum number of lines to keep in View().
// Content beyond this is printed to scrollback to prevent scroll issues.
const maxViewLines = 8

// maybeFlushToScrollback checks if there are segments to flush to scrollback,
// keeping View() small to avoid terminal scroll issues.
// Note: Only flushes images/diffs immediately; text is accumulated and printed at end.
func (m *askStreamModel) maybeFlushToScrollback() tea.Cmd {
	result := m.tracker.FlushToScrollback(m.width, 0, maxViewLines, m.renderMdWithWidth)
	if result.ToPrint != "" {
		m.cachedContent = "" // Invalidate cache since segments were flushed
		m.contentDirty = true
		return tea.Printf("%s", result.ToPrint)
	}
	return nil
}

// flushCompletedBoundaryNow flushes completed segments up to the first pending
// tool barrier. Use this at tool boundaries to preserve strict text/tool
// interleaving order without flushing completed concurrent tools ahead of an
// earlier still-pending tool.
func (m *askStreamModel) flushCompletedBoundaryNow() tea.Cmd {
	result := m.tracker.FlushCompletedNow(m.width, m.renderMdWithWidth)
	if result.ToPrint != "" {
		m.cachedContent = "" // Invalidate cache since segments were flushed
		m.contentDirty = true
		return tea.Printf("%s", result.ToPrint)
	}
	return nil
}

func (m *askStreamModel) flushCompletedBoundaryNowWithAck(callID, name string) tea.Cmd {
	result := m.tracker.FlushCompletedNow(m.width, m.renderMdWithWidth)
	if result.ToPrint != "" {
		m.cachedContent = "" // Invalidate cache since segments were flushed
		m.contentDirty = true
		return tea.Sequence(
			tea.Printf("%s", result.ToPrint),
			func() tea.Msg {
				return askBoundaryFlushedMsg{CallID: callID, Name: name}
			},
		)
	}
	return func() tea.Msg {
		return askBoundaryFlushedMsg{CallID: callID, Name: name}
	}
}

// flushSmoothBufferToTracker flushes any buffered words into the text tracker.
func (m *askStreamModel) flushSmoothBufferToTracker() {
	if m.smoothBuffer == nil {
		return
	}

	remaining := m.smoothBuffer.FlushAll()
	if remaining == "" {
		return
	}

	m.tracker.AddTextSegment(remaining, m.width)
	m.contentDirty = true
}

func (m askStreamModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Handle inspector mode
	if m.inspectorMode {
		return m.updateInspectorMode(msg)
	}

	// Handle tool start messages even while approval form is active
	if toolMsg, ok := msg.(askToolStartMsg); ok && m.approvalForm != nil {
		m.flushSmoothBufferToTracker()
		// Mark current text segment as complete before starting tool
		m.tracker.MarkCurrentTextComplete(func(text string) string {
			return m.renderMd(text)
		})
		if m.tracker.HandleToolStart(toolMsg.CallID, toolMsg.Name, toolMsg.Info, toolMsg.ToolArgs) {
			// New segment added, but don't start wave yet (approval form is active)
		}
	}

	// If approval form is active, delegate to it
	if m.approvalForm != nil {
		form, cmd := m.approvalForm.Update(msg)
		if f, ok := form.(*huh.Form); ok {
			m.approvalForm = f
		}

		// Check if form completed
		if m.approvalForm.State == huh.StateCompleted {
			approved := m.approvalForm.GetBool("confirm")
			// Send response - the tool segment will be updated by askToolEndMsg
			// when the tool actually completes (not when approval is granted)
			if m.approvalResponseCh != nil {
				m.approvalResponseCh <- approved
			}
			m.approvalForm = nil
			m.approvalResponseCh = nil
			m.approvalDesc = ""
			m.approvalToolInfo = ""
			// If there are pending tools, restart wave animation
			if m.tracker.HasPending() {
				return m, m.tracker.StartWave()
			}
			return m, m.spinner.Tick
		}

		// Check if form was aborted (esc/ctrl+c)
		if m.approvalForm.State == huh.StateAborted {
			if m.approvalResponseCh != nil {
				m.approvalResponseCh <- false
			}
			m.approvalForm = nil
			m.approvalResponseCh = nil
			m.approvalDesc = ""
			return m, tea.Quit
		}

		return m, cmd
	}

	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" || msg.String() == "esc" {
			return m, func() tea.Msg { return askCancelledMsg{} }
		}
		// Handle CTRL-O to open inspector (works during streaming too)
		if msg.String() == "ctrl+o" && m.store != nil && m.sess != nil {
			// Fetch messages from session store
			messages, err := m.store.GetMessages(context.Background(), m.sess.ID, 0, 0)
			if err != nil {
				// Log error but don't interrupt the user - just don't open inspector
				m.tracker.AddTextSegment(fmt.Sprintf("\n[Inspector error: %v]\n", err), m.width)
				return m, nil
			}
			if len(messages) > 0 {
				m.inspectorMode = true
				m.inspectorModel = inspector.NewWithStore(messages, m.width, m.height, m.styles, m.store)
				return m, nil
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.cachedContent = ""  // Invalidate cache
		m.contentDirty = true // Width changed, need to re-render
		// Invalidate cached markdown renderings since they are width-dependent
		for i := range m.tracker.Segments {
			if m.tracker.Segments[i].Type == ui.SegmentText {
				m.tracker.Segments[i].Rendered = ""
				// Also clear streaming cache for in-progress segments
				// so next render recalculates with new width
				m.tracker.Segments[i].SafeRendered = ""
				m.tracker.Segments[i].SafePos = 0
			}
		}
		// Resize active streaming renderers
		m.tracker.ResizeStreamRenderers(m.width)

	case askStreamEventMsg:
		return m.handleStreamEventMessage(msg)
	case askContentMsg:
		// Buffer text and release it in smooth word-paced ticks.
		if m.smoothBuffer != nil {
			m.smoothBuffer.Write(string(msg))
			if !m.smoothTickPending {
				m.smoothTickPending = true
				return m, ui.SmoothTick()
			}
			return m, nil
		}
		// Fallback: direct append if smooth buffer is unavailable.
		m.tracker.AddTextSegment(string(msg), m.width)
		m.contentDirty = true

	case askAttemptDiscardMsg:
		if m.smoothBuffer != nil {
			m.smoothBuffer.Reset()
		}
		m.smoothTickPending = false
		m.deferredStreamRead = false
		m.resetCurrentReasoning()
		m.phase = "Thinking"
		m.tracker.DiscardAttempt()
		m.contentDirty = true
		m.retryStatus = "Discarded interrupted partial response; retrying..."
		return m, nil

	case askReasoningMsg:
		return m.handleReasoningMessage(msg)
	case askDoneMsg:
		return m.handleDoneMessage(msg)
	case askCancelledMsg:
		return m.handleCancelledMessage(msg)
	case ui.SmoothTickMsg:
		return m.handleSmoothTickMessage(msg)
	case askUsageMsg:
		m.totalTokens = msg.InputTokens + msg.OutputTokens

	case askTickMsg:
		// Stop ticking when done
		if m.done {
			return m, nil
		}
		// Continue ticking for elapsed time and idle detection updates
		return m, m.tickEvery()

	case askRetryMsg:
		m.retryStatus = ui.FormatRetryStatus("Rate limited", msg.Attempt, msg.MaxAttempts, msg.WaitSecs, 0, "...")
		return m, m.tickEvery()

	case askPhaseMsg:
		m.phase = string(msg)
		// Display WARNING phases as visible text in the conversation
		if strings.HasPrefix(string(msg), llm.WarningPhasePrefix) {
			m.tracker.AddTextSegment(string(msg)+"\n", m.width)
			m.contentDirty = true
		}
		return m, nil

	case askImageMsg:
		// Add image segment for inline display
		m.tracker.AddImageSegment(string(msg))

		// Flush to scrollback so image appears
		if cmd := m.maybeFlushToScrollback(); cmd != nil {
			return m, cmd
		}
		return m, nil

	case askDiffMsg:
		// Add diff segment for inline display
		m.tracker.AddDiffSegmentWithOperation(msg.Path, msg.Old, msg.New, msg.Line, msg.Operation)
		// Flush to scrollback so diff appears
		if cmd := m.maybeFlushToScrollback(); cmd != nil {
			return m, cmd
		}
		return m, nil

	case askFlushBeforeAskUserMsg:
		// Set flag to suppress spinner in View() while external UI is active
		m.pausedForExternalUI = true
		m.flushSmoothBufferToTracker()

		// Partial flush - keep some context visible for after external UI returns
		result := m.tracker.FlushBeforeExternalUI(m.width, 0, maxViewLines, m.renderMdWithWidth)

		var cmds []tea.Cmd
		if result.ToPrint != "" {
			cmds = append(cmds, tea.Printf("%s", result.ToPrint))
		}

		// Signal that flush is complete (use a command to ensure tea.Println finishes first)
		cmds = append(cmds, func() tea.Msg {
			close(msg.Done)
			return nil
		})
		return m, tea.Sequence(cmds...)

	case askFlushBeforeApprovalMsg:
		// Set flag to suppress spinner in View() while approval UI is active
		m.pausedForExternalUI = true
		m.flushSmoothBufferToTracker()

		// Partial flush - keep some context visible for after external UI returns
		result := m.tracker.FlushBeforeExternalUI(m.width, 0, maxViewLines, m.renderMdWithWidth)

		var cmds []tea.Cmd
		if result.ToPrint != "" {
			cmds = append(cmds, tea.Printf("%s", result.ToPrint))
		}

		// Signal that flush is complete (use a command to ensure tea.Println finishes first)
		cmds = append(cmds, func() tea.Msg {
			close(msg.Done)
			return nil
		})
		return m, tea.Sequence(cmds...)

	case askResumeFromExternalUIMsg:
		// Resume from external UI (ask_user or approval)
		m.pausedForExternalUI = false

		// Check if there's an ask_user summary to display
		// Add to tracker so it appears in correct order, then flush
		if summary := tools.GetAndClearAskUserResult(); summary != "" {
			m.tracker.AddExternalUIResult(summary)
			if cmd := m.maybeFlushToScrollback(); cmd != nil {
				return m, ui.ComposeFlushFirstCommands([]tea.Cmd{cmd}, []tea.Cmd{m.spinner.Tick})
			}
		}

		return m, m.spinner.Tick

	case askToolStartMsg:
		m.retryStatus = ""
		m.flushSmoothBufferToTracker()
		m.smoothTickPending = false

		// Mark current text segment as complete before starting tool
		// This preserves interleaving order: text -> tool -> text
		m.tracker.MarkCurrentTextComplete(func(text string) string {
			return m.renderMd(text)
		})

		// Add or dedupe pending tool segment for this call.
		m.tracker.HandleToolStart(msg.CallID, msg.Name, msg.Info, msg.ToolArgs)
		ui.AttachSubagentProgressToSegment(m.tracker, m.subagentTracker, msg.CallID)

		// Defer pending tool rendering until boundary flush is visibly committed.
		m.pendingBoundaryFlushes++
		m.contentDirty = true

		// Start (or restart) wave animation for non-ask_user tools after the
		// boundary flush acknowledgement arrives.
		if msg.Name != tools.AskUserToolName {
			m.pendingWaveStart = true
		}
		return m, m.flushCompletedBoundaryNowWithAck(msg.CallID, msg.Name)

	case askBoundaryFlushedMsg:
		if m.pendingBoundaryFlushes > 0 {
			m.pendingBoundaryFlushes--
		}
		if m.pendingBoundaryFlushes > 0 {
			return m, nil
		}

		// Boundary is now visible in scrollback; refresh content and reveal tools.
		m.cachedContent = m.tracker.RenderUnflushed(m.width, renderMd, false)
		m.contentDirty = false

		if m.pendingWaveStart && m.tracker.HasPending() {
			m.pendingWaveStart = false
			return m, m.tracker.StartWave()
		}
		m.pendingWaveStart = false

	case askToolEndMsg:
		m.tracker.HandleToolEnd(msg.CallID, msg.Success)

		// Remove from subagent tracker when spawn_agent completes
		m.subagentTracker.Remove(msg.CallID)

		// Flush completed segments immediately at tool boundary.
		var flushCmd tea.Cmd
		if cmd := m.flushCompletedBoundaryNow(); cmd != nil {
			flushCmd = cmd
		}

		// Immediately rebuild cache with the remaining content
		m.cachedContent = m.tracker.RenderUnflushed(m.width, renderMd, false)

		if flushCmd != nil {
			return m, flushCmd
		}

		// If no more pending tools, start spinner for idle state
		if !m.tracker.HasPending() {
			return m, m.spinner.Tick
		}

	case askGuardianReviewMsg:
		event := msg.Event
		message := strings.TrimSpace(event.Message)
		if message == "" {
			break
		}
		if event.ToolCallID != "" {
			m.tracker.HandleGuardianEvent(event)
		} else {
			// Session-level guardian notices have no tool row to annotate.
			m.tracker.AddExternalUIResult(message)
		}
		m.contentDirty = true

	case askSubagentProgressMsg:
		// Handle subagent progress events and update segment stats
		ui.HandleSubagentProgress(m.tracker, m.subagentTracker, msg.CallID, msg.Event)
		if msg.Event.Type == tools.SubagentEventToolEnd {
			for _, media := range msg.Event.Media {
				if reference := strings.ToLower(strings.TrimSpace(media.Reference)); reference != "" {
					if m.mediaByReference == nil {
						m.mediaByReference = make(map[string]llm.MediaArtifact)
					}
					m.mediaByReference[reference] = media
				}
			}
		}

	case ui.WaveTickMsg:
		if cmd := m.tracker.HandleWaveTick(); cmd != nil {
			return m, cmd
		}

	case ui.WavePauseMsg:
		if cmd := m.tracker.HandleWavePause(); cmd != nil {
			return m, cmd
		}

	case askApprovalRequestMsg:
		m.approvalDesc = msg.Description
		m.approvalResponseCh = msg.ResponseCh
		m.approvalToolInfo = msg.ToolInfo

		// Don't add a new segment - the tool already has a pending segment from askToolStartMsg.
		// The approval is part of that tool's execution, not a separate operation.

		m.approvalForm = huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Key("confirm").
					Title(msg.Description).
					Affirmative("Yes").
					Negative("No").
					WithButtonAlignment(lipgloss.Left),
			),
		).WithShowHelp(false).WithShowErrors(false)
		return m, m.approvalForm.Init()

	case spinner.TickMsg:
		// Always maintain the tick chain to keep spinner animating
		// The idle check in View() controls visibility, not animation
		// Don't update spinner when done
		if !m.done {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	}

	// Update cached content when dirty (must be done in Update, not View)
	// View() uses a value receiver so mutations are discarded by Bubble Tea
	if m.contentDirty && m.pendingBoundaryFlushes == 0 {
		m.cachedContent = m.tracker.RenderUnflushed(m.width, renderMd, false)
		m.contentDirty = false
	}

	return m, nil
}

func (m *askStreamModel) renderMd(text string) string {
	return ui.RenderMediaMarkdown(text, m.mediaByReference, func(value string) string {
		return ui.RenderMarkdown(value, m.width)
	}, nil)
}

func (m *askStreamModel) renderMdWithWidth(text string, _ int) string {
	return m.renderMd(text)
}

func renderMd(text string, width int) string {
	return ui.RenderMarkdown(text, width)
}

// updateInspectorMode handles updates while in inspector mode
func (m askStreamModel) updateInspectorMode(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Pass to inspector
		if m.inspectorModel != nil {
			m.inspectorModel, _ = m.inspectorModel.Update(msg)
		}
		return m, nil

	case tea.KeyPressMsg:
		// Pass to inspector
		if m.inspectorModel != nil {
			var cmd tea.Cmd
			m.inspectorModel, cmd = m.inspectorModel.Update(msg)
			return m, cmd
		}
		return m, nil

	case inspector.CloseMsg:
		// Exit inspector mode and return to stream UI
		m.inspectorMode = false
		m.inspectorModel = nil
		return m, nil

	default:
		// Pass through to inspector
		if m.inspectorModel != nil {
			var cmd tea.Cmd
			m.inspectorModel, cmd = m.inspectorModel.Update(msg)
			return m, cmd
		}
	}

	// Update cached content when dirty (must be done in Update, not View)
	// View() uses a value receiver so mutations are discarded by Bubble Tea
	if m.contentDirty && m.pendingBoundaryFlushes == 0 {
		m.cachedContent = m.tracker.RenderUnflushed(m.width, renderMd, false)
		m.contentDirty = false
	}

	return m, nil
}

func (m askStreamModel) View() tea.View {
	// Inspector mode uses alternate screen
	if m.inspectorMode && m.inspectorModel != nil {
		return m.inspectorModel.View()
	}

	// When done, return empty string so flushed content isn't duplicated
	if m.done {
		return tea.NewView("")
	}

	var b strings.Builder

	// Get segments from tracker (excludes flushed segments)
	active := m.tracker.ActiveSegments()
	if m.pendingBoundaryFlushes > 0 {
		active = nil
	}

	// Use cached content (updated in Update() when contentDirty was true)
	// Fall back to rendering on-demand if cache is empty but we have segments
	// (handles tests that call View() directly without Update())
	content := m.cachedContent
	if content == "" && m.pendingBoundaryFlushes == 0 {
		content = m.tracker.RenderUnflushed(m.width, renderMd, false)
	}
	if content != "" {
		b.WriteString(content)
	}

	// If approval form is active, show it after content (no spinner during approval)
	if m.approvalForm != nil {
		if b.Len() > 0 {
			b.WriteString("\n")
		} else {
			b.WriteString(m.tracker.FlushLeadingSeparator(ui.SegmentTool))
		}
		b.WriteString(m.approvalForm.View())
		return tea.NewView(b.String())
	}

	// Show spinner when idle (no activity for >1s) or when tools are active
	// Don't show spinner when done or paused for external UI (ask_user/approval)
	if !m.done && !m.pausedForExternalUI && (len(active) > 0 || m.tracker.IsIdle(time.Second)) {
		hasContent := b.Len() > 0
		if hasContent {
			if len(active) > 0 {
				completed := m.tracker.CompletedSegments()
				if len(completed) > 0 {
					b.WriteString(ui.SegmentSeparatorBetween(completed[len(completed)-1], active[0]))
				}
			} else {
				b.WriteString("\n")
			}
		}
		phase := m.phase
		if phase == "" {
			phase = "Thinking"
		}

		indicator := ui.StreamingIndicator{
			Spinner:         m.spinner.View(),
			Phase:           phase,
			Elapsed:         time.Since(m.startTime),
			Tokens:          m.totalTokens,
			Status:          m.retryStatus,
			ShowCancel:      true,
			Segments:        active,
			WavePos:         m.tracker.WavePos,
			Width:           m.width,
			RenderMarkdown:  func(text string, _ int) string { return m.renderMd(text) },
			HasFlushed:      !hasContent && m.tracker.HasFlushed,
			LastFlushedType: m.tracker.LastFlushedType,
			LastFlushedPlan: m.tracker.LastFlushedPlan,
		}.Render(m.styles)
		b.WriteString(indicator)
	}

	return tea.NewView(b.String())
}

// StreamWithRenderer renders markdown progressively as content streams in.
// It creates an ask stream program wired to read from events itself.
func StreamWithRenderer(ctx context.Context, cfg *config.Config, events <-chan ui.StreamEvent, store session.Store, sess *session.Session) error {
	model := newAskStreamModel()
	model.store = store
	model.sess = sess
	model.streamChan = events
	if cfg != nil {
		model.reasoningConfig = cfg.ResolveReasoning("ask")
	}
	return RunStreamProgram(ctx, tea.NewProgram(model, tea.WithoutSignalHandler()))
}

// RunStreamProgram runs a preconfigured ask-stream Bubble Tea program.
// Callers are responsible for wiring the underlying askStreamModel to its
// stream source before calling this helper.
func RunStreamProgram(ctx context.Context, p *tea.Program) error {
	if p == nil {
		return fmt.Errorf("ask stream program is nil")
	}

	type programResult struct {
		model tea.Model
		err   error
	}
	programDone := make(chan programResult, 1)
	go func() {
		m, err := p.Run()
		programDone <- programResult{model: m, err: err}
	}()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			p.Send(askCancelledMsg{})
		case <-done:
		}
	}()

	result := <-programDone
	close(done)
	if result.err != nil {
		return result.err
	}
	if model, ok := result.model.(askStreamModel); ok && model.streamErr != nil {
		return model.streamErr
	}
	return nil
}
