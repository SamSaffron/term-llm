package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"

	"github.com/samsaffron/term-llm/internal/mcp"
	render "github.com/samsaffron/term-llm/internal/render/chat"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
	sessionsui "github.com/samsaffron/term-llm/internal/tui/sessions"
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
	store         session.Store     // Session storage backend
	sess          *session.Session  // Current session
	messages      []session.Message // In-memory messages for current session
	compactionIdx int               // Prefix length to skip for LLM context; 0 means no prefix is skipped.
	messagesMu    sync.Mutex        // Protects messages from concurrent compaction callback
	streaming     bool
	phase         string // "Thinking", "Searching", "Reading", "Responding"

	// Streaming state
	currentResponse  strings.Builder
	currentTokens    int
	streamStartTime  time.Time
	webSearchUsed    bool
	retryStatus      string
	streamCancelFunc context.CancelFunc
	streamDone       chan struct{}       // closed when the engine goroutine exits
	tracker          *ui.ToolTracker     // Tool and segment tracking (shared component)
	subagentTracker  *ui.SubagentTracker // Subagent progress tracking

	// Persist-as-we-go: row ID of the in-progress assistant message (0 = none).
	// Written from engine callbacks on a non-UI goroutine; protected by pendingMu.
	pendingAssistantMsgID int64
	pendingMu             sync.Mutex

	// In-progress LLM context used only for the status-line token estimate while
	// a stream is active. The persisted session messages are not updated until
	// stream completion, so callbacks maintain this snapshot as assistant/tool
	// messages are produced. Written from engine callbacks; protected by
	// contextEstimateMu.
	contextEstimateMu                sync.Mutex
	contextEstimateVersion           uint64
	contextEstimateCachedVersion     uint64
	contextEstimateCachedTokens      int
	contextEstimateCachedStreaming   bool
	contextEstimateCachedValid       bool
	streamingContextMessages         []llm.Message
	streamingContextPendingAssistant bool

	// Streaming channels
	streamChan <-chan ui.StreamEvent

	// Smooth text buffer for 60fps rendering
	smoothBuffer            *ui.SmoothBuffer
	smoothTickPending       bool
	deferredStreamRead      bool
	streamRenderTickPending bool
	newlineCompactor        *ui.StreamingNewlineCompactor

	// External UI state
	pausedForExternalUI bool // True when paused for ask_user or approval prompts
	approvalMgr         *tools.ApprovalManager

	// Embedded inline approval UI (alt screen mode only)
	approvalModel  *tools.ApprovalModel
	approvalDoneCh chan<- tools.ApprovalResult

	// Embedded inline ask_user UI (alt screen mode only)
	askUserModel  *tools.AskUserModel
	askUserDoneCh chan<- []tools.AskUserAnswer

	// LLM context
	rootCtx      context.Context
	provider     llm.Provider
	fastProvider llm.Provider
	engine       *llm.Engine
	config       *config.Config
	providerName string
	providerKey  string
	modelName    string
	agentName    string

	platformDeveloperMessage string
	currentOrigin            session.SessionOrigin

	// Agent handover
	agentResolver       func(name string, cfg *config.Config) (*agents.Agent, error)
	agentLister         func(cfg *config.Config) ([]string, error) // Lists available agent names
	pendingHandover     *handoverDoneMsg                           // Non-nil while awaiting confirmation
	handoverPreview     *handoverPreviewModel                      // Inline confirmation UI (alt screen)
	currentAgent        *agents.Agent                              // Current agent config (for enable_handover)
	handoverApprovalMgr *tools.ApprovalManager                     // Shell approval flow for handover scripts
	handoverToolDoneCh  chan<- bool                                // Signal back to initiate_handover tool

	// Pending message context
	files                   []FileAttachment // Attached files for next message
	images                  []ImageAttachment
	selectedImage           int            // -1 means no image chip selected
	pasteChunks             map[int]string // Collapsed paste placeholders → actual content
	pasteSeq                int            // Incrementing ID for paste placeholders
	searchEnabled           bool           // Web search toggle
	forceExternalSearch     bool           // Force external search tools even if provider supports native
	disableExternalWebFetch bool           // Disable external read_url injection even when provider lacks native fetch
	localTools              []string       // Names of enabled local tools (read, write, etc.)
	toolsStr                string         // Original tools setting (for session persistence)
	mcpStr                  string         // Original MCP setting (for session persistence)
	pendingInterjection     string         // Interrupt text waiting to be injected or cancelled
	pendingInterjectionID   string         // Stable ID for the currently displayed pending interjection
	interjectionSeq         uint64         // Monotonic sequence for locally generated interjection IDs
	interruptRequestSeq     uint64         // Monotonic sequence for async interrupt classification
	activeInterruptSeq      uint64         // Currently active async interrupt classification request
	pendingInterruptUI      string         // UI state: "", "deciding", "interject"
	interruptNotice         string         // One-line UI notice for recent interrupt actions
	// MCP (Model Context Protocol)
	mcpManager    *mcp.Manager
	mcpStatusChan chan mcp.StatusUpdate
	maxTurns      int

	// Directory approval
	approvedDirs    *ApprovedDirs
	pendingFilePath string // File waiting for directory approval

	// History scroll
	scrollOffset int
	viewportRows int

	// UI state
	quitting bool
	err      error
	yolo     bool

	// reloadRequested signals the caller to re-exec the binary (e.g. after an upgrade).
	// The session ID to resume is stored in reloadSessionID.
	reloadRequested bool
	reloadSessionID string

	// Dialog components
	completions *CompletionsModel
	dialog      *DialogModel

	// Inline mode state
	program *tea.Program // Reference to program for tea.Println

	// If set, the caller should relaunch chat with this session ID.
	pendingResumeSessionID string

	// If set, the caller should auto-send this message after handover restart.
	pendingHandoverAutoSend string

	// If set, auto-send this message on Init (used after handover restart).
	handoverAutoSend string

	// Stats tracking
	showStats  bool
	stats      *ui.SessionStats
	streamPerf *streamPerfTelemetry

	// Inspector mode
	inspectorMode  bool
	inspectorModel *inspector.Model

	// Resume browser mode
	resumeBrowserMode  bool
	resumeBrowserModel *sessionsui.Model

	// Alt screen mode (full-screen rendering)
	altScreen               bool
	mouseMode               bool
	viewport                viewport.Model // Scrollable viewport for alt screen mode
	scrollToBottom          bool           // Flag to scroll to bottom after response completes
	streamRenderMinInterval time.Duration

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
		lastXOffset         int    // Viewport horizontal offset when view was cached
		lastSetContentAt    time.Time
		historySignature    uint64 // Content fingerprint for cached history
		// completedStream holds rendered streaming content (diffs, tools) that should
		// persist after streaming ends. Cleared when a new prompt is sent.
		completedStream string
		// Content versioning to avoid expensive string comparisons
		contentVersion               uint64 // Incremented when content changes
		lastRenderedVersion          uint64 // Version that was last rendered to viewport
		lastTrackerVersion           uint64 // Last seen tracker.Version (to detect content changes)
		lastStreamingContent         string // Last rendered streaming tail in alt-screen mode
		lastContentHistoryPlusStream bool   // Whether the rendered viewport is exactly history + streaming tail
		// Caching for streaming segments to avoid re-rendering on every frame
		cachedCompletedContent string // Rendered completed segments
		cachedTrackerVersion   uint64 // Tracker version when cache was built
		lastWavePos            int    // Last wave position for animation
		// Selection cache for invalidation
		lastSelection  Selection
		lastContentStr string // stored for lazy contentLines split
	}

	// New chat renderer (virtualized rendering for large histories)
	chatRenderer *render.Renderer

	// Auto-send mode (for benchmarking) - queue of messages to send
	autoSendQueue []string
	// autoSendExitOnDone causes the TUI to quit when the queue is exhausted;
	// when false the session continues in interactive mode after the queue drains.
	autoSendExitOnDone bool

	// Text mode (no markdown rendering)
	textMode bool
	// Expanded tool display (full commands/env)
	toolsExpanded bool
	// Whether the Ctrl+E discovery hint has been shown in this chat session.
	toolExpandHintShown bool

	// Mouse layout tracking for textarea click-to-cursor support
	textareaBoundsValid    bool
	textareaTopY           int
	textareaBottomY        int
	textareaLeftX          int
	textareaRightX         int
	textareaPromptWidth    int
	textareaEffectiveWidth int

	// Text selection state (alt-screen only)
	selection         Selection
	contentLines      []string // full viewport content split by \n
	copyStatus        string   // transient status message after copy attempt
	footerMessage     string   // transient footer message for short system notices
	footerMessageTone string   // "", "muted", "success", or "error"
	footerMessageSeq  uint64   // monotonically increasing footer message timer token
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
	tickMsg               time.Time
	streamRenderTickMsg   struct{}
	footerMessageClearMsg struct {
		Seq uint64
	}
	interruptClassifiedMsg struct {
		RequestID      uint64
		InterjectionID string
		Content        string
		Action         llm.InterruptAction
	}
	compactStartedMsg struct{}
	compactDoneMsg    struct {
		result *llm.CompactionResult
		err    error
	}
	handoverDoneMsg struct {
		result       *llm.HandoverResult
		err          error
		agentName    string
		providerStr  string // Optional "provider:model" override
		confirmed    bool   // True when the document was prepared after confirmation.
		instructions string // Additional instructions captured at confirmation time.
	}
	handoverConfirmMsg    struct{}
	handoverCancelMsg     struct{}
	handoverRenameDoneMsg struct{ err error }
	mcpStatusUpdateMsg    struct{ update mcp.StatusUpdate }
)

const (
	chatRenderThrottleEnv  = "TERM_LLM_CHAT_RENDER_THROTTLE_MS"
	chatSpinnerIntervalEnv = "TERM_LLM_CHAT_SPINNER_MS"
	chatDisableMouseEnv    = "TERM_LLM_DISABLE_MOUSE"
)

var readPrimarySelection = clipboard.ReadPrimarySelection

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
	WorkDir string // directory where a shell command will execute (may be empty)
	DoneCh  chan<- tools.ApprovalResult
}

// AskUserRequestMsg triggers an inline ask_user prompt.
type AskUserRequestMsg struct {
	Questions []tools.AskUserQuestion
	DoneCh    chan<- []tools.AskUserAnswer
}

// HandoverRequestMsg triggers a handover flow from a tool call.
type HandoverRequestMsg struct {
	Agent  string
	DoneCh chan<- bool
}

func loadSessionMessagesForContext(ctx context.Context, store session.Store, sess *session.Session) ([]session.Message, error) {
	if sess == nil {
		return nil, nil
	}
	if sess.CompactionSeq >= 0 {
		return store.GetMessagesFrom(ctx, sess.ID, sess.CompactionSeq)
	}
	return store.GetMessages(ctx, sess.ID, 0, 0)
}

func loadSessionMessagesForScrollback(ctx context.Context, store session.Store, sess *session.Session) ([]session.Message, int, error) {
	if sess == nil {
		return nil, 0, nil
	}
	messages, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		return nil, 0, err
	}
	if sess.CompactionSeq < 0 {
		return messages, 0, nil
	}
	for i, msg := range messages {
		if msg.Sequence >= sess.CompactionSeq {
			return messages, i, nil
		}
	}
	return messages, len(messages), nil
}

func (m *Model) refreshSessionFromStore(ctx context.Context) error {
	if m.store == nil || m.sess == nil {
		return nil
	}
	refreshed, err := m.store.Get(ctx, m.sess.ID)
	if err != nil {
		return err
	}
	if refreshed != nil {
		m.sess = refreshed
	}
	return nil
}

// New creates a new chat model.
// fast-provider aware callers should use NewWithFastProvider.
func New(cfg *config.Config, provider llm.Provider, engine *llm.Engine, providerKey string, modelName string, mcpManager *mcp.Manager, maxTurns int, forceExternalSearch bool, disableExternalWebFetch bool, searchEnabled bool, localTools []string, toolsStr string, mcpStr string, showStats bool, initialText string, store session.Store, sess *session.Session, altScreen bool, autoSendQueue []string, autoSendExitOnDone bool, textMode bool, agentName string, platformDeveloperMessage string, yolo bool) *Model {
	return NewWithFastProvider(cfg, provider, nil, engine, providerKey, modelName, mcpManager, maxTurns, forceExternalSearch, disableExternalWebFetch, searchEnabled, localTools, toolsStr, mcpStr, showStats, initialText, store, sess, altScreen, autoSendQueue, autoSendExitOnDone, textMode, agentName, platformDeveloperMessage, yolo)
}

// NewWithFastProvider creates a new chat model with an optional fast provider
// for control-plane classification tasks.
func NewWithFastProvider(cfg *config.Config, provider llm.Provider, fastProvider llm.Provider, engine *llm.Engine, providerKey string, modelName string, mcpManager *mcp.Manager, maxTurns int, forceExternalSearch bool, disableExternalWebFetch bool, searchEnabled bool, localTools []string, toolsStr string, mcpStr string, showStats bool, initialText string, store session.Store, sess *session.Session, altScreen bool, autoSendQueue []string, autoSendExitOnDone bool, textMode bool, agentName string, platformDeveloperMessage string, yolo bool) *Model {
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
	s.Spinner.FPS = chatSpinnerFPSFromEnv()

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
	taStyles := ta.Styles()
	taStyles.Focused.CursorLine = lipgloss.NewStyle()
	taStyles.Focused.Base = lipgloss.NewStyle()
	taStyles.Focused.Placeholder = lipgloss.NewStyle().Foreground(styles.Theme().Muted)
	taStyles.Focused.EndOfBuffer = lipgloss.NewStyle()
	taStyles.Focused.Prompt = lipgloss.NewStyle().Foreground(styles.Theme().Primary).Bold(true)
	taStyles.Blurred = taStyles.Focused
	ta.SetStyles(taStyles)
	ta.Focus()

	// Prefill with initial text if provided
	if initialText != "" {
		ta.SetValue(initialText)
	}

	// Use provided session or create a new one
	if sess == nil {
		sess = &session.Session{
			ID:          session.NewID(),
			Provider:    provider.Name(),
			ProviderKey: providerKey,
			Model:       modelName,
			Mode:        session.ModeChat,
			Origin:      session.OriginTUI,
			Agent:       agentName,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
			Search:      searchEnabled,
			Tools:       toolsStr,
			MCP:         mcpStr,
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

	// Load existing messages if resuming.
	// If the session was compacted, only load post-compaction messages — the
	// summary + recent context is all the LLM needs, and skipping older messages
	// avoids a large DB read and memory cost for long-lived sessions.
	var messages []session.Message
	if store != nil && sess.ID != "" {
		if loadedMsgs, err := loadSessionMessagesForContext(context.Background(), store, sess); err == nil {
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
	vpHeight := ui.RemainingLines(height, 4)
	vp := viewport.New(viewport.WithWidth(width), viewport.WithHeight(vpHeight))
	vp.Style = lipgloss.NewStyle()
	// Chat history never intentionally scrolls horizontally. Keep the viewport's
	// hidden x-offset pinned at zero so stray shift-wheel/trackpad horizontal
	// events can't clip every rendered line until reload.
	vp.SetHorizontalStep(0)

	// Create chat renderer for virtualized history rendering
	chatRenderer := render.NewRenderer(width, vpHeight)

	// Create tracker with text mode setting
	tracker := ui.NewToolTracker()
	tracker.TextMode = textMode

	stats := ui.NewSessionStats()
	if sess != nil {
		stats.SeedTotals(sess.InputTokens, sess.OutputTokens, sess.CachedInputTokens, sess.CacheWriteTokens, sess.ToolCalls, sess.LLMTurns)
	}

	var mcpStatusChan chan mcp.StatusUpdate
	if mcpManager != nil {
		mcpStatusChan = make(chan mcp.StatusUpdate, 32)
		mcpManager.SetStatusChannel(mcpStatusChan)
	}

	model := &Model{
		width:                    width,
		height:                   height,
		textarea:                 ta,
		spinner:                  s,
		styles:                   styles,
		keyMap:                   DefaultKeyMap(),
		store:                    store,
		sess:                     sess,
		messages:                 messages,
		rootCtx:                  context.Background(),
		provider:                 provider,
		fastProvider:             fastProvider,
		engine:                   engine,
		config:                   cfg,
		providerName:             provider.Name(),
		providerKey:              providerKey,
		modelName:                modelName,
		agentName:                agentName,
		platformDeveloperMessage: strings.TrimSpace(platformDeveloperMessage),
		currentOrigin:            session.OriginTUI,
		yolo:                     yolo,
		phase:                    "Thinking",
		viewportRows:             ui.RemainingLines(height, 8), // Reserve space for input and status
		tracker:                  tracker,
		subagentTracker:          subagentTracker,
		smoothBuffer:             ui.NewSmoothBuffer(),
		completions:              completions,
		dialog:                   dialog,
		approvedDirs:             approvedDirs,
		mcpManager:               mcpManager,
		mcpStatusChan:            mcpStatusChan,
		maxTurns:                 maxTurns,
		forceExternalSearch:      forceExternalSearch,
		disableExternalWebFetch:  disableExternalWebFetch,
		searchEnabled:            searchEnabled,
		localTools:               localTools,
		toolsStr:                 toolsStr,
		mcpStr:                   mcpStr,
		showStats:                showStats,
		stats:                    stats,
		streamPerf:               newStreamPerfTelemetryFromEnv(),
		altScreen:                altScreen,
		mouseMode:                chatMouseModeFromEnv(),
		viewport:                 vp,
		streamRenderMinInterval:  chatRenderMinIntervalFromEnv(),
		chatRenderer:             chatRenderer,
		autoSendQueue:            autoSendQueue,
		autoSendExitOnDone:       autoSendExitOnDone,
		textMode:                 textMode,
		selectedImage:            -1,
	}
	model.configureContextManagementForSession()
	return model
}

func (m *Model) refreshMCPPickerIfOpen() {
	if m == nil || m.dialog == nil || m.mcpManager == nil || m.dialog.Type() != DialogMCPPicker {
		return
	}
	query := m.dialog.Query()
	cursor := m.dialog.Cursor()
	m.dialog.ShowMCPPicker(m.mcpManager)
	m.dialog.SetQuery(query)
	m.dialog.SetCursor(cursor)
}

func (m *Model) seedStatsFromSession() {
	if m.sess == nil {
		m.stats = ui.NewSessionStats()
		return
	}
	if m.stats == nil {
		m.stats = ui.NewSessionStats()
	}
	m.stats.SeedTotals(m.sess.InputTokens, m.sess.OutputTokens, m.sess.CachedInputTokens, m.sess.CacheWriteTokens, m.sess.ToolCalls, m.sess.LLMTurns)
}

func (m *Model) configureContextManagementForSession() {
	if m == nil || m.engine == nil || m.provider == nil || m.config == nil || m.sess == nil {
		return
	}

	providerForLimits := strings.TrimSpace(m.sess.ProviderKey)
	if providerForLimits == "" {
		providerForLimits = strings.TrimSpace(m.providerKey)
	}
	if providerForLimits == "" {
		providerForLimits = strings.TrimSpace(m.sess.Provider)
	}

	modelForLimits := strings.TrimSpace(m.sess.Model)
	if modelForLimits == "" {
		modelForLimits = strings.TrimSpace(m.modelName)
	}
	if modelForLimits == "" {
		return
	}

	m.engine.ConfigureContextManagement(m.provider, providerForLimits, modelForLimits, m.config.AutoCompact)
	m.engine.SetContextEstimateBaseline(m.sess.LastTotalTokens, m.sess.LastMessageCount)
}

func (m *Model) persistContextEstimate(ctx context.Context) {
	if m == nil || m.store == nil || m.sess == nil || m.engine == nil {
		return
	}
	total, count := m.engine.ContextEstimateBaseline()
	if total <= 0 || count <= 0 {
		return
	}
	_ = m.store.UpdateContextEstimate(ctx, m.sess.ID, total, count)
	m.sess.LastTotalTokens = total
	m.sess.LastMessageCount = count
}

// WantsReload reports whether the user requested a binary reload via /reload.
func (m *Model) WantsReload() bool { return m.reloadRequested }

// ReloadSessionID returns the session ID to resume after a reload, if any.
func (m *Model) ReloadSessionID() string { return m.reloadSessionID }

func (m *Model) applyWindowSize(msg tea.WindowSizeMsg) {
	m.selection = Selection{}
	m.width = msg.Width
	m.height = msg.Height
	m.viewportRows = ui.RemainingLines(m.height, 8)
	m.textarea.SetWidth(m.width)
	m.updateTextareaHeight()
	if m.completions != nil {
		m.completions.SetSize(m.width, m.height)
	}
	if m.dialog != nil {
		m.dialog.SetSize(m.width, m.height)
	}

	// Invalidate cached markdown renderings since they are width-dependent.
	if m.tracker != nil {
		for i := range m.tracker.Segments {
			m.tracker.Segments[i].Rendered = ""
			m.tracker.Segments[i].SafeRendered = ""
			m.tracker.Segments[i].SafePos = 0
			// Also clear diff caches (Issue 2: diff render cache invalidation).
			m.tracker.Segments[i].DiffRendered = ""
			m.tracker.Segments[i].DiffWidth = 0
			// Clear subagent diff caches.
			for j := range m.tracker.Segments[i].SubagentDiffs {
				m.tracker.Segments[i].SubagentDiffs[j].Rendered = ""
				m.tracker.Segments[i].SubagentDiffs[j].Width = 0
			}
		}
		// Resize active streaming renderers.
		m.tracker.ResizeStreamRenderers(m.width)
	}

	// Invalidate completed stream cache since it's width-dependent (Issue 1).
	// Also invalidate history cache because renderHistory() skips the last turn
	// when completedStream is non-empty — clearing it without rebuilding history
	// would leave a stale cache that excludes the last assistant message.
	m.resetAltScreenStreamingAppendCache()
	if m.viewCache.completedStream != "" {
		m.viewCache.completedStream = ""
		m.invalidateHistoryCache()
	} else {
		m.bumpContentVersion()
	}

	// Resize viewport for alt screen mode.
	m.viewport.SetWidth(m.width)
	m.viewport.SetHorizontalStep(0)
	m.resetViewportHorizontalOffset()

	// Propagate size to embedded dialogs if active.
	if m.approvalModel != nil {
		m.approvalModel.SetWidth(m.width)
	}
	if m.askUserModel != nil {
		m.askUserModel.SetWidth(m.width)
	}

	// Update chat renderer size (invalidates cache).
	if m.altScreen {
		m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	} else if m.chatRenderer != nil {
		m.chatRenderer.SetSize(m.width, m.height)
	}
}

func (m *Model) syncAltScreenViewportHeight(footerHeight int) {
	vpHeight := ui.RemainingLines(m.height, footerHeight)
	m.viewport.SetWidth(m.width)
	m.viewport.SetHorizontalStep(0)
	m.resetViewportHorizontalOffset()
	m.viewport.SetHeight(vpHeight)
	m.viewportRows = vpHeight
	m.viewport.SetYOffset(m.viewport.YOffset())
	if m.chatRenderer != nil {
		m.chatRenderer.SetSize(m.width, vpHeight)
	}
}

func (m *Model) resetViewportHorizontalOffset() {
	if m.viewport.XOffset() != 0 {
		m.viewport.SetXOffset(0)
	}
}

func (m *Model) resetTracker() {
	m.tracker = ui.NewToolTracker()
	m.tracker.TextMode = m.textMode
	m.tracker.SetExpanded(m.toolsExpanded)
	m.tracker.SetExpandHintShown(m.toolExpandHintShown)
	m.viewCache.cachedCompletedContent = ""
	m.viewCache.cachedTrackerVersion = 0
	m.viewCache.lastTrackerVersion = 0
	m.viewCache.lastWavePos = 0
}

// SetAgentResolver configures the function used to resolve agent names
// during /handover. The function should match cmd.LoadAgent's signature.
func (m *Model) SetAgentResolver(resolver func(name string, cfg *config.Config) (*agents.Agent, error)) {
	m.agentResolver = resolver
}

// SetAgentLister configures the function used to list available agent names
// for /handover completions.
func (m *Model) SetAgentLister(lister func(cfg *config.Config) ([]string, error)) {
	m.agentLister = lister
}

// SetCurrentAgent sets the current agent configuration (used by /handover
// to check for enable_handover).
func (m *Model) SetCurrentAgent(agent *agents.Agent) {
	m.currentAgent = agent
}

// SetApprovalManager configures the active approval manager used by chat-level
// controls such as the yolo-mode toggle.
func (m *Model) SetApprovalManager(mgr *tools.ApprovalManager) {
	m.approvalMgr = mgr
	m.handoverApprovalMgr = mgr
}

// SetHandoverApprovalManager configures shell approval checks for handover scripts.
func (m *Model) SetHandoverApprovalManager(mgr *tools.ApprovalManager) {
	m.SetApprovalManager(mgr)
}

// SetRootContext configures the parent context for long-running chat commands.
func (m *Model) SetRootContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.rootCtx = ctx
}

func (m *Model) rootContext() context.Context {
	if m.rootCtx != nil {
		return m.rootCtx
	}
	return context.Background()
}

// Init initializes the model
func (m *Model) Init() tea.Cmd {
	// Update textarea height for any initial text
	m.updateTextareaHeight()

	baseCmds := []tea.Cmd{textarea.Blink, m.spinner.Tick}
	if cmd := m.listenForMCPStatusUpdates(); cmd != nil {
		baseCmds = append(baseCmds, cmd)
	}

	// Set markdown renderer for chat renderer
	if m.chatRenderer != nil {
		m.chatRenderer.SetMarkdownRenderer(m.renderMd)
		m.chatRenderer.SetToolsExpanded(m.toolsExpanded)
	}

	// Handover auto-send: send the target agent's default prompt after restart
	if m.handoverAutoSend != "" {
		m.textarea.SetValue(m.handoverAutoSend)
		m.handoverAutoSend = ""
		m.updateTextareaHeight()
		cmds := append([]tea.Cmd{}, baseCmds...)
		cmds = append(cmds, func() tea.Msg { return autoSendMsg{} })
		return tea.Batch(cmds...)
	}

	// In auto-send mode, pop first message from queue and send it
	if len(m.autoSendQueue) > 0 {
		// Set textarea to first queued message
		m.textarea.SetValue(m.autoSendQueue[0])
		m.autoSendQueue = m.autoSendQueue[1:]
		m.updateTextareaHeight()
		cmds := append([]tea.Cmd{}, baseCmds...)
		cmds = append(cmds, func() tea.Msg { return autoSendMsg{} })
		return tea.Batch(cmds...)
	}

	return tea.Batch(baseCmds...)
}

func (m *Model) listenForMCPStatusUpdates() tea.Cmd {
	if m == nil || m.mcpStatusChan == nil {
		return nil
	}
	return func() tea.Msg {
		update, ok := <-m.mcpStatusChan
		if !ok {
			return nil
		}
		return mcpStatusUpdateMsg{update: update}
	}
}

// RequestedResumeSessionID returns a pending session ID to relaunch, if any.
func (m *Model) RequestedResumeSessionID() string {
	return strings.TrimSpace(m.pendingResumeSessionID)
}

// RequestedHandoverAutoSend returns a message to auto-send after handover restart.
func (m *Model) RequestedHandoverAutoSend() string {
	return strings.TrimSpace(m.pendingHandoverAutoSend)
}

// WaitStreamDone blocks until the engine streaming goroutine has exited.
// It is safe to call when no stream was started (no-op).
func (m *Model) WaitStreamDone() {
	if m.streamDone != nil {
		<-m.streamDone
	}
}

// SetHandoverAutoSend sets a message to auto-send on Init (for handover restart).
func (m *Model) SetHandoverAutoSend(text string) {
	m.handoverAutoSend = strings.TrimSpace(text)
}

func chatMouseModeFromEnv() bool {
	return !ui.ParseBoolDefault(os.Getenv(chatDisableMouseEnv), false)
}

func chatRenderMinIntervalFromEnv() time.Duration {
	const defaultInterval = 16 * time.Millisecond
	raw := strings.TrimSpace(os.Getenv(chatRenderThrottleEnv))
	if raw == "" {
		return defaultInterval
	}
	millis, err := strconv.Atoi(raw)
	if err != nil || millis < 0 {
		return defaultInterval
	}
	return time.Duration(millis) * time.Millisecond
}

func chatSpinnerFPSFromEnv() time.Duration {
	const defaultFPS = 250 * time.Millisecond
	raw := strings.TrimSpace(os.Getenv(chatSpinnerIntervalEnv))
	if raw == "" {
		return defaultFPS
	}
	millis, err := strconv.Atoi(raw)
	if err != nil || millis <= 0 {
		return defaultFPS
	}
	return time.Duration(millis) * time.Millisecond
}

// Update handles messages
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var flushCmds []tea.Cmd

	// The yolo toggle is intentionally global so it works while streaming,
	// inspecting, or browsing embedded views.
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok && m.isYoloToggleKey(keyMsg) {
		return m.toggleYoloMode()
	}

	// Chat-owned self-scheduling ticks must keep running even while an embedded
	// modal is active. If a spinner tick is forwarded to the inspector/session
	// browser, the child ignores it and the spinner never schedules its next tick,
	// so it appears frozen after returning to chat.
	_, isSpinnerTick := msg.(spinner.TickMsg)

	// Handle resume browser mode
	if m.resumeBrowserMode && !isSpinnerTick {
		return m.updateResumeBrowserMode(msg)
	}

	// Handle inspector mode
	if m.inspectorMode && !isSpinnerTick {
		return m.updateInspectorMode(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.applyWindowSize(msg)

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

	case tea.KeyPressMsg:
		return m.handleKeyMsg(msg)

	case tea.PasteMsg:
		return m.handlePasteMsg(msg)

	case tea.MouseMsg:
		// Text selection in alt-screen viewport (before textarea handling)
		if m.altScreen && m.handleSelectionMouse(msg) {
			return m, nil
		}
		if m.handleTextareaMouse(msg) {
			return m, nil
		}
		// Handle middle-click paste: read primary selection and route through
		// the PasteMsg path so collapse logic applies.
		if click, ok := msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseMiddle {
			text, err := readPrimarySelection()
			if err == nil && text != "" {
				return m.handlePasteMsg(tea.PasteMsg{Content: text})
			}
			return m, nil
		}
		// Forward mouse events to viewport in alt-screen mode for scroll wheel support.
		// Do not let horizontal wheel/shift-wheel gestures modify the viewport's
		// hidden x-offset; chat history is always rendered at column zero.
		if m.altScreen {
			if isHorizontalViewportScroll(msg) {
				m.resetViewportHorizontalOffset()
				return m, nil
			}
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			m.resetViewportHorizontalOffset()
			return m, cmd
		}
		return m, nil

	case spinner.TickMsg:
		if m.streaming && !m.pausedForExternalUI {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}

	case tickMsg:
		if m.streaming {
			cmds = append(cmds, m.tickEvery())
		}

	case streamRenderTickMsg:
		m.streamRenderTickPending = false
		// No explicit action is needed here: Bubble Tea re-renders after each Update.
		// This tick exists to ensure View() runs again after the throttle window, so
		// pending content can pass shouldThrottleSetContent().

	case footerMessageClearMsg:
		if msg.Seq == m.footerMessageSeq {
			m.clearFooterMessage()
		}

	case mcpStatusUpdateMsg:
		m.refreshMCPPickerIfOpen()
		cmds = append(cmds, m.listenForMCPStatusUpdates())

	case interruptClassifiedMsg:
		return m.handleInterruptClassified(msg)

	case compactDoneMsg:
		m.streaming = false
		m.phase = "Thinking"
		if m.streamCancelFunc != nil {
			m.streamCancelFunc()
			m.streamCancelFunc = nil
		}
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) || errors.Is(msg.err, context.DeadlineExceeded) {
				return m.showFooterMuted("Compaction cancelled.")
			}
			return m.showFooterError(fmt.Sprintf("Compaction failed: %v", msg.err))
		}
		if msg.result == nil {
			return m.showFooterError("Compaction failed: no result returned.")
		}
		var newSessionMsgs []session.Message
		for _, msg := range msg.result.NewMessages {
			newSessionMsgs = append(newSessionMsgs, *session.NewMessage(m.sess.ID, msg, -1))
		}
		if m.store != nil {
			if err := m.store.CompactMessages(context.Background(), m.sess.ID, newSessionMsgs); err != nil {
				return m.showFooterError(fmt.Sprintf("Compaction finished, but saving failed: %v", err))
			}
			if err := m.refreshSessionFromStore(context.Background()); err != nil {
				return m.showFooterError(fmt.Sprintf("Conversation compacted, but session refresh failed: %v", err))
			}
		}
		m.messagesMu.Lock()
		m.compactionIdx = len(m.messages)
		m.messages = append(m.messages, newSessionMsgs...)
		m.messagesMu.Unlock()
		m.invalidateHistoryCache()

		if m.engine != nil {
			m.engine.ResetConversation()
		}
		return m.showFooterSuccess("Conversation compacted.")

	case handoverDoneMsg:
		m.streaming = false
		m.phase = "Thinking"
		// Don't cancel the stream when a tool-initiated handover is pending:
		// the engine must stay alive to receive the tool result.
		if m.handoverToolDoneCh == nil && m.streamCancelFunc != nil {
			m.streamCancelFunc()
			m.streamCancelFunc = nil
		}
		if msg.err != nil {
			m.cancelHandoverTool()
			if msg.confirmed {
				m.pendingHandover = nil
			}
			if errors.Is(msg.err, context.Canceled) || errors.Is(msg.err, context.DeadlineExceeded) {
				return m.showFooterMuted("Handover cancelled.")
			}
			return m.showFooterError(fmt.Sprintf("Handover failed: %v", msg.err))
		}
		if msg.result == nil {
			m.cancelHandoverTool()
			if msg.confirmed {
				m.pendingHandover = nil
			}
			return m.showFooterError("Handover failed: no result returned.")
		}
		if msg.confirmed {
			if m.pendingHandover == nil {
				m.pendingHandover = &handoverDoneMsg{
					agentName:   msg.agentName,
					providerStr: msg.providerStr,
				}
			}
			m.pendingHandover.result = msg.result
			if instructions := strings.TrimSpace(msg.instructions); instructions != "" {
				m.pendingHandover.instructions = instructions
			}
			return m.executeHandover()
		}
		// Show inline handover confirmation UI
		m.pendingHandover = &msg
		m.handoverPreview = newHandoverPreviewModel(
			msg.result.Document, msg.agentName, msg.providerStr,
			m.width, m.styles,
		)
		m.scrollToBottom = true
		return m, nil

	case handoverConfirmMsg:
		// Don't signal the tool yet — wait until the handover is actually
		// committed (in executeHandover) so the old turn doesn't resume
		// prematurely if a later step fails.
		// Capture instructions before clearing the preview
		instructions := ""
		if m.handoverPreview != nil {
			instructions = m.handoverPreview.Instructions()
		}
		m.handoverPreview = nil
		if m.pendingHandover == nil {
			m.cancelHandoverTool()
			return m, nil
		}
		m.pendingHandover.instructions = strings.TrimSpace(instructions)
		if m.agentResolver != nil {
			targetAgent, err := m.agentResolver(m.pendingHandover.agentName, m.config)
			if err != nil {
				m.cancelHandoverTool()
				m.pendingHandover = nil
				return m.showFooterError(fmt.Sprintf("Handover failed to resolve target agent: %v", err))
			}
			if targetAgent != nil && strings.TrimSpace(targetAgent.HandoverScript) != "" {
				sourceAgent := handoverSourceAgent(m.pendingHandover, m.agentName)
				return m.startHandoverScriptHandover(targetAgent, sourceAgent, targetAgent, m.pendingHandover.providerStr, true, m.pendingHandover.instructions)
			}
		}
		return m.executeHandover()

	case handoverCancelMsg:
		toolWasPending := m.handoverToolDoneCh != nil
		m.cancelHandoverTool()
		m.pendingHandover = nil
		m.handoverPreview = nil
		m.invalidateHistoryCache()
		// Resume the engine stream so the tool result is delivered.
		if toolWasPending {
			m.streaming = true
		}
		return m, nil

	case handoverRenameDoneMsg:
		// Silently ignore — rename is best-effort background work.
		return m, nil

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
		tickStart := time.Now()
		m.smoothTickPending = false
		// Release buffered text word-by-word for smooth 60fps rendering
		if m.smoothBuffer != nil && m.streaming {
			words := m.smoothBuffer.NextWords()
			hadWords := words != ""
			if words != "" {
				m.currentResponse.WriteString(words)
				if m.tracker != nil {
					m.tracker.AddTextSegment(words, m.width)
				}
				m.phase = "Responding"
				// Flush excess content if needed
				if m.scrollOffset == 0 {
					if cmd := m.maybeFlushToScrollback(); cmd != nil {
						flushCmds = append(flushCmds, cmd)
					}
				}
			}
			// Continue ticking if not drained
			if !m.smoothBuffer.IsDrained() {
				if !m.smoothTickPending {
					m.smoothTickPending = true
					if m.streamPerf != nil {
						m.streamPerf.RecordSmoothTickScheduled()
					}
					cmds = append(cmds, ui.SmoothTick())
				}
			}
			if m.deferredStreamRead && m.streaming {
				m.deferredStreamRead = false
				cmds = append(cmds, m.listenForStreamEvents())
			}
			if m.streamPerf != nil {
				m.streamPerf.RecordSmoothTickHandled(hadWords, m.smoothBuffer.Len())
				m.streamPerf.RecordDuration(durationMetricSmoothTick, time.Since(tickStart))
			}
		}

	case streamEventMsg:
		streamEventStart := time.Now()
		ev := msg.event
		if m.streamPerf != nil {
			m.streamPerf.tracef("stream_event type=%v", ev.Type)
		}

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
				m.smoothTickPending = false
				m.deferredStreamRead = false
				// This resets local scheduling state for the next stream.
				// An already in-flight render tick may still arrive and no-op.
				m.streamRenderTickPending = false
				m.streaming = false
				m.err = ev.Err
				// Error line is part of alt-screen viewport content; force refresh.
				if m.altScreen {
					m.bumpContentVersion()
				}

				// Clear callbacks and update status
				m.clearStreamCallbacks()
				if m.store != nil {
					// Use interrupted for cancellation, error for other failures
					status := session.StatusError
					if errors.Is(ev.Err, context.Canceled) {
						status = session.StatusInterrupted
					}
					_ = m.store.UpdateStatus(context.Background(), m.sess.ID, status)
				}

				if m.streamPerf != nil {
					m.streamPerf.RecordDuration(durationMetricStreamEvent, time.Since(streamEventStart))
					m.streamPerf.EmitSummaryIfActive(time.Now())
				}

				m.textarea.Focus()

				// Recover pending interjection text into textarea on error.
				// If the engine queue is already empty but we never rendered the
				// interjection inline, fall back to the visible pending draft.
				m.restorePendingInterjectionDraft()
				m.clearPendingInterjectionState()

				return m, nil
			}

		case ui.StreamEventToolStart:
			m.stats.ToolStart()
			if m.tracker != nil {
				m.tracker.SetExpandHintShown(m.toolExpandHintShown)
			}

			// Flush smooth buffer before tool starts (user wants to see tool output right away)
			if m.smoothBuffer != nil {
				remaining := m.smoothBuffer.FlushAll()
				if remaining != "" {
					m.currentResponse.WriteString(remaining)
					if m.tracker != nil {
						m.tracker.AddTextSegment(remaining, m.width)
					}
				}
				m.smoothTickPending = false
			}

			// Mark current text segment as complete before starting tool
			if m.tracker != nil {
				m.tracker.MarkCurrentTextComplete(func(text string) string {
					return m.renderMarkdown(text)
				})
				if m.tracker.HandleToolStart(ev.ToolCallID, ev.ToolName, ev.ToolInfo, ev.ToolArgs) {
					m.toolExpandHintShown = m.tracker.ExpandHintShown()
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
						flushCmds = append(flushCmds, cmd)
					}
				}
			}

		case ui.StreamEventUsage:
			inputTokens := ev.InputTokens
			outputTokens := ev.OutputTokens
			cachedTokens := ev.CachedTokens
			writeTokens := ev.WriteTokens
			if inputTokens < 0 {
				inputTokens = 0
			}
			if outputTokens < 0 {
				outputTokens = 0
			}
			if cachedTokens < 0 {
				cachedTokens = 0
			}
			if writeTokens < 0 {
				writeTokens = 0
			}
			if m.stats != nil && (inputTokens > 0 || outputTokens > 0 || cachedTokens > 0 || writeTokens > 0) {
				m.stats.AddUsage(inputTokens, outputTokens, cachedTokens, writeTokens)
			}
		case ui.StreamEventText:
			text := ev.Text
			if m.newlineCompactor == nil {
				m.newlineCompactor = ui.NewStreamingNewlineCompactor(ui.MaxStreamingConsecutiveNewlines)
			}
			text = m.newlineCompactor.CompactChunk(text)
			if text == "" {
				break
			}

			// Buffer text for smooth 60fps rendering instead of immediate display
			if m.smoothBuffer != nil {
				m.smoothBuffer.Write(text)
				if m.streamPerf != nil {
					m.streamPerf.RecordTextDelta(text, m.smoothBuffer.Len())
				}
				// Start smooth tick if not already running
				if !m.smoothTickPending {
					m.smoothTickPending = true
					if m.streamPerf != nil {
						m.streamPerf.RecordSmoothTickScheduled()
					}
					cmds = append(cmds, ui.SmoothTick())
				}
			} else {
				// Fallback: direct display if no smooth buffer
				m.currentResponse.WriteString(text)
				if m.tracker != nil {
					m.tracker.AddTextSegment(text, m.width)
				}
				if m.streamPerf != nil {
					m.streamPerf.RecordTextDelta(text, 0)
				}
			}

			m.phase = "Responding"
			m.retryStatus = ""

		case ui.StreamEventPhase:
			m.phase = ev.Phase
			m.retryStatus = ""
			// Display WARNING phases as visible text in the conversation
			if strings.HasPrefix(ev.Phase, llm.WarningPhasePrefix) && m.tracker != nil {
				m.tracker.AddTextSegment(ev.Phase+"\n", m.width)
			}

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
						flushCmds = append(flushCmds, cmd)
					}
				}
			}

		case ui.StreamEventDiff:
			// Add diff segment for inline display
			if m.tracker != nil && ev.DiffPath != "" {
				m.tracker.AddDiffSegmentWithOperation(ev.DiffPath, ev.DiffOld, ev.DiffNew, ev.DiffLine, ev.DiffOperation)
				// Flush to scrollback so diff appears
				if m.scrollOffset == 0 {
					if cmd := m.maybeFlushToScrollback(); cmd != nil {
						flushCmds = append(flushCmds, cmd)
					}
				}
			}

		case ui.StreamEventInterjection:
			// User interjected a message mid-stream (injected between tool turns).
			matchedPending := false
			switch {
			case ev.InterjectionID != "" && ev.InterjectionID == m.pendingInterjectionID:
				matchedPending = true
			case ev.InterjectionID == "" && m.pendingInterjectionID == "" && strings.TrimSpace(ev.Text) == strings.TrimSpace(m.pendingInterjection):
				matchedPending = true
			}
			if matchedPending {
				m.clearPendingInterjectionState()
			}
			// Flush smooth buffer so any pending text appears before the interjection.
			if m.smoothBuffer != nil {
				remaining := m.smoothBuffer.FlushAll()
				if remaining != "" {
					m.currentResponse.WriteString(remaining)
					if m.tracker != nil {
						m.tracker.AddTextSegment(remaining, m.width)
					}
				}
				m.smoothTickPending = false
			}
			// Mark current text segment as complete before interjection
			if m.tracker != nil {
				m.tracker.MarkCurrentTextComplete(func(text string) string {
					return m.renderMarkdown(text)
				})
				// Add interjection as a pre-rendered text segment.
				// Store the styled version directly so it bypasses markdown rendering
				// and avoids ANSI escape code artifacts in the output.
				theme := m.styles.Theme()
				promptStyle := lipgloss.NewStyle().Foreground(theme.Primary).Bold(true)
				rendered := promptStyle.Render("❯") + " " + ev.Text + "\n\n"
				m.tracker.AddPreRenderedTextSegment(rendered)
			}
			// Persist interjected message to session store
			if m.store != nil {
				userMsg := &session.Message{
					SessionID:   m.sess.ID,
					Role:        llm.RoleUser,
					Parts:       []llm.Part{{Type: llm.PartText, Text: ev.Text}},
					TextContent: ev.Text,
					CreatedAt:   time.Now(),
					Sequence:    -1, // Store will assign the next sequence number
				}
				_ = m.store.AddMessage(context.Background(), m.sess.ID, userMsg)
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

			m.persistContextEstimate(context.Background())
			m.streaming = false

			// Flag to scroll to bottom after response completes (alt screen mode)
			if m.altScreen {
				m.scrollToBottom = true
			}

			// Clear callbacks
			m.clearStreamCallbacks()

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
					m.viewCache.completedStream = ui.RenderSegments(completed, m.width, -1, m.renderMd, true, m.toolsExpanded)
					m.bumpContentVersion()
				} else {
					// In inline mode, print remaining content to scrollback
					m.tracker.ForceCompletePendingTools()
					result := m.tracker.FlushAllRemaining(m.width, 0, m.renderMd)
					cmds = append(cmds, ui.ScrollbackPrintlnCommands(result.ToPrint, true)...)
				}
			} else if !m.altScreen {
				cmds = append(cmds, ui.ScrollbackPrintlnCommands("", true)...)
			}

			// Sync in-memory messages with persisted state.
			// Keep full scrollback loaded for live sessions, but recompute the
			// compacted-window prefix so the next LLM request skips old history.
			if m.store != nil {
				ctx := context.Background()
				if err := m.refreshSessionFromStore(ctx); err != nil {
					_, cmd := m.showFooterError(fmt.Sprintf("Session refresh failed after compaction: %v", err))
					cmds = append(cmds, cmd)
				} else if loadedMsgs, compactionIdx, err := loadSessionMessagesForScrollback(ctx, m.store, m.sess); err != nil {
					_, cmd := m.showFooterError(fmt.Sprintf("Session message reload failed after compaction: %v", err))
					cmds = append(cmds, cmd)
				} else {
					m.messagesMu.Lock()
					m.messages = loadedMsgs
					m.compactionIdx = compactionIdx
					m.messagesMu.Unlock()
					m.invalidateHistoryCache()
				}
				_ = m.store.UpdateStatus(ctx, m.sess.ID, session.StatusComplete)
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
					m.invalidateHistoryCache()
				}
			}

			// Reset streaming state
			m.currentResponse.Reset()
			m.currentTokens = 0
			m.webSearchUsed = false
			m.retryStatus = ""
			m.resetTracker()
			if m.smoothBuffer != nil {
				m.smoothBuffer.Reset()
			}
			m.newlineCompactor = nil
			m.smoothTickPending = false
			m.deferredStreamRead = false
			// This resets local scheduling state for the next stream.
			// An already in-flight render tick may still arrive and no-op.
			m.streamRenderTickPending = false
			if m.streamPerf != nil {
				m.streamPerf.EmitSummaryIfActive(time.Now())
			}

			// Auto-save session
			cmds = append(cmds, m.saveSessionCmd())

			// Try to rename random-word handover files to descriptive slugs
			if cmd := m.maybeRenameHandoverCmd(); cmd != nil {
				cmds = append(cmds, cmd)
			}

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

				if m.autoSendExitOnDone {
					// Queue exhausted and exit requested, quit
					m.quitting = true
					if m.showStats && m.stats.LLMCallCount > 0 {
						m.stats.Finalize()
						return m, tea.Sequence(tea.Println(m.stats.Render()), tea.Quit)
					}
					return m, tea.Quit
				}

				// Queue exhausted, continue in interactive mode
				m.autoSendQueue = nil
			}

			// Re-enable textarea
			m.textarea.Focus()

			// Recover any pending interjection that wasn't consumed. If the
			// engine queue is already empty but the UI still shows a pending
			// interjection, restore that draft rather than letting it vanish.
			m.restorePendingInterjectionDraft()
			if m.activeInterruptSeq == 0 {
				m.clearPendingInterjection()
			}
		}

		// Continue listening for more events unless we're done or got an error.
		// When a smooth tick is already pending for streamed text, defer the next
		// blocking stream read until that tick so Bubble Tea can coalesce provider
		// deltas into frame-paced updates.
		if ev.Type != ui.StreamEventDone && ev.Type != ui.StreamEventError {
			if ev.Type == ui.StreamEventText && m.smoothTickPending {
				m.deferredStreamRead = true
			} else {
				cmds = append(cmds, m.listenForStreamEvents())
			}
		}
		if m.streamPerf != nil {
			m.streamPerf.RecordDuration(durationMetricStreamEvent, time.Since(streamEventStart))
		}

	case sessionSavedMsg:
		// Session saved successfully, nothing to do

	case sessionLoadedMsg:
		if msg.sess != nil {
			m.sess = msg.sess
			m.messages = msg.messages
			m.seedStatsFromSession()
			m.configureContextManagementForSession()
			m.invalidateHistoryCache()
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
				return m, ui.ComposeFlushFirstCommands([]tea.Cmd{cmd}, []tea.Cmd{m.spinner.Tick})
			}
		}

		return m, m.spinner.Tick

	case ApprovalRequestMsg:
		if m.isYoloModeActive() {
			msg.DoneCh <- tools.ApprovalResult{Choice: tools.ApprovalChoiceOnce}
			m.pausedForExternalUI = false
			m.approvalModel = nil
			m.approvalDoneCh = nil
			return m, nil
		}

		// In alt screen mode, render approval UI inline
		if m.altScreen {
			m.pausedForExternalUI = true
			m.approvalDoneCh = msg.DoneCh
			if msg.IsShell {
				m.approvalModel = tools.NewEmbeddedShellApprovalModel(msg.Path, msg.WorkDir, m.width)
			} else {
				m.approvalModel = tools.NewEmbeddedApprovalModel(msg.Path, msg.IsWrite, m.width)
			}
			// Mark current text as complete so it shows above the approval UI
			if m.tracker != nil {
				m.tracker.MarkCurrentTextComplete(func(text string) string {
					return m.renderMarkdown(text)
				})
			}
			// Scroll to bottom so the prompt is visible even if the user had scrolled up.
			m.scrollToBottom = true
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
			// Scroll to bottom so the prompt is visible even if the user had scrolled up.
			m.scrollToBottom = true
			return m, nil
		}
		// Non-alt screen mode: shouldn't happen, but fall back to cancelled
		msg.DoneCh <- nil
		return m, nil

	case HandoverRequestMsg:
		// Tool-initiated handover: trigger the same flow as /handover @agent.
		// Keep streaming paused until the handover is confirmed, cancelled,
		// or fails — the render path needs !m.streaming to show the preview.
		m.handoverToolDoneCh = msg.DoneCh
		m.streaming = false
		return m.cmdHandover([]string{msg.Agent})

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

	if cmd := m.maybeScheduleStreamRenderTick(); cmd != nil {
		cmds = append(cmds, cmd)
	}

	return m, ui.ComposeFlushFirstCommands(flushCmds, cmds)
}

func (m *Model) maybeScheduleStreamRenderTick() tea.Cmd {
	if !m.streaming || !m.altScreen {
		return nil
	}
	if m.streamRenderTickPending {
		return nil
	}
	if m.streamRenderMinInterval <= 0 {
		return nil
	}
	if m.viewCache.contentVersion == m.viewCache.lastRenderedVersion {
		return nil
	}
	if m.approvalModel != nil || m.askUserModel != nil {
		return nil
	}
	if m.viewCache.lastSetContentAt.IsZero() {
		return nil
	}
	elapsed := time.Since(m.viewCache.lastSetContentAt)
	if elapsed >= m.streamRenderMinInterval {
		return nil
	}

	delay := m.streamRenderMinInterval - elapsed
	m.streamRenderTickPending = true
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return streamRenderTickMsg{}
	})
}
