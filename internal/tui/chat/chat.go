package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"

	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	render "github.com/samsaffron/term-llm/internal/render/chat"
	"github.com/samsaffron/term-llm/internal/sessiontitle"
	"github.com/samsaffron/term-llm/internal/termimage"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
	sessionsui "github.com/samsaffron/term-llm/internal/tui/sessions"
	worktreesui "github.com/samsaffron/term-llm/internal/tui/worktrees"
	"github.com/samsaffron/term-llm/internal/ui"
	"golang.org/x/term"
)

// Model is the main chat TUI model
type pendingInterjectionUI struct {
	ID      string
	Text    string
	UIState string
}

type pendingStreamModelSwitch struct {
	provider string
	model    string
	applied  bool
}

type promptHistoryState struct {
	active          bool
	cursorID        int64
	cursorCreatedAt time.Time
	lookupSeq       uint64
	lookupPending   bool
	memoryMode      bool
	memoryIndex     int
	draftText       string
	draftFiles      []FileAttachment
	draftImages     []ImageAttachment
	draftPastes     map[int]string
	recalledText    string
}

type RuntimeSystemContext struct {
	SystemPrompt string
	ApplySkills  func(engine *llm.Engine, toolMgr *tools.ToolManager)
}

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
	// olderScrollbackLoaded is false when a compacted resume initially loaded only
	// the active tail; scrolling upward can hydrate the older display prefix once.
	olderScrollbackLoaded bool
	messagesMu            sync.Mutex // Protects messages from concurrent compaction callback
	streaming             bool
	shareInFlight         bool
	pendingShare          *shareRequest
	phase                 string // "Thinking", "Searching", "Reading", "Responding"

	// Reasoning display/status state. Provider replay metadata is persisted in
	// assistant parts by the LLM engine; these fields only affect live UI policy.
	reasoningConfig       config.ReasoningConfig
	reasoningModeOverride string
	currentReasoning      strings.Builder
	currentReasoningKind  llm.ReasoningKind
	currentReasoningTitle string
	// Per-block expansion override for the live (uncommitted) reasoning
	// block; carried onto the segment when it is committed to the tracker.
	currentReasoningExpanded *bool
	committedReasoning       []llm.Part
	reasoningPhaseActive     bool
	reasoningRawWarned       bool

	// Streaming state
	currentResponse       strings.Builder
	currentTokens         int
	streamStartTime       time.Time
	webSearchUsed         bool
	retryStatus           string
	streamCancelFunc      context.CancelFunc
	streamDone            chan struct{}       // closed when the engine goroutine exits
	streamGeneration      uint64              // increments for each stream; used to ignore stale listener messages
	streamCancelRequested *atomic.Bool        // user requested stream cancellation; wait for stream exit before final cleanup
	tracker               *ui.ToolTracker     // Tool and segment tracking (shared component)
	subagentTracker       *ui.SubagentTracker // Subagent progress tracking

	// Persist-as-we-go: row ID and latest per-turn snapshot of the in-progress
	// assistant message. Written from engine callbacks on a non-UI goroutine;
	// protected by pendingMu.
	pendingAssistantMsgID       int64
	pendingAssistantTextSet     bool
	pendingAssistantSnapshot    llm.Message
	pendingAssistantSnapshotSet bool
	completedAssistantTurns     int
	pendingMu                   sync.Mutex

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
	// Per-stream text-delta coalescer wrapping streamChan. Created alongside
	// streamChan at stream start so a stale listener from a cancelled stream
	// can never deliver its pending event into a newer stream.
	streamCoalescer *streamEventCoalescer

	// Smooth text buffer for 60fps rendering
	smoothBuffer            *ui.SmoothBuffer
	smoothTickPending       bool
	streamRenderTickPending bool
	newlineCompactor        *ui.StreamingNewlineCompactor

	// External UI state
	pausedForExternalUI bool // True when paused for ask_user or approval prompts
	approvalMgr         *tools.ApprovalManager
	toolMgr             *tools.ToolManager

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
	runner       runpkg.Runner
	config       *config.Config
	providerName string
	providerKey  string
	modelName    string
	agentName    string

	platformDeveloperMessage string
	currentOrigin            session.SessionOrigin

	// Agent handover
	agentResolver                func(name string, cfg *config.Config) (*agents.Agent, error)
	agentLister                  func(cfg *config.Config) ([]string, error) // Lists available agent names
	handoverSystemPromptResolver func(agent *agents.Agent, providerKey, modelName string) (string, error)
	runtimeSystemContextResolver func(agent *agents.Agent, providerKey, modelName, dir string) (RuntimeSystemContext, error)
	runtimeSystemContext         RuntimeSystemContext
	systemPromptOverridden       bool
	systemPromptOverride         string
	guardianReviewerRefresh      func(providerKey, modelName string) error
	pendingHandover              *handoverDoneMsg       // Non-nil while awaiting confirmation
	handoverPreview              *handoverPreviewModel  // Inline confirmation UI (alt screen)
	currentAgent                 *agents.Agent          // Current agent config (for enable_handover)
	handoverApprovalMgr          *tools.ApprovalManager // Shell approval flow for handover scripts
	handoverToolDoneCh           chan<- bool            // Signal back to initiate_handover tool

	// Pending message context
	files                   []FileAttachment // Attached files for next message
	images                  []ImageAttachment
	selectedImage           int            // -1 means no image chip selected
	pasteChunks             map[int]string // Collapsed paste placeholders → actual content
	pasteSeq                int            // Incrementing ID for paste placeholders
	searchEnabled           bool           // Web search toggle
	fastMode                bool           // Effective ChatGPT/OpenAI fast service-tier state shown in the footer
	fastProviderDefault     bool           // Provider config requests fast by default; inherited unless overridden in-session
	fastOverride            serviceTierOverride
	fastMetadataLoaded      bool // ChatGPT model metadata has been loaded
	fastMetadataStale       bool // Loaded metadata came from stale cache and should refresh in background
	fastMetadataLoading     bool // Metadata load command is in flight
	pendingFastToggle       bool // User requested /fast while waiting for metadata
	modelMetadata           []llm.ModelInfo
	forceExternalSearch     bool     // Force external search tools even if provider supports native
	disableExternalWebFetch bool     // Disable external read_url injection even when provider lacks native fetch
	localTools              []string // Names of enabled local tools (read, write, etc.)
	toolsStr                string   // Original tools setting (for session persistence)
	mcpStr                  string   // Original MCP setting (for session persistence)
	pendingInterjection     string   // Interrupt text waiting to be injected or cancelled (latest, for compatibility)
	pendingInterjectionID   string   // Stable ID for the latest displayed pending interjection
	pendingInterjections    []pendingInterjectionUI
	selectedInterjection    int       // Selected pending interjection; -1 means none
	interjectionSeq         uint64    // Monotonic sequence for locally generated interjection IDs
	interruptRequestSeq     uint64    // Monotonic sequence for async interrupt classification
	activeInterruptSeq      uint64    // Currently active async interrupt classification request
	pendingInterruptUI      string    // UI state of latest pending interjection: "", "deciding", "interject"
	interruptNotice         string    // One-line UI notice for recent interrupt actions
	ctrlCExitArmedUntil     time.Time // Second Ctrl+C before this time exits the TUI
	promptHistory           promptHistoryState
	promptHistoryLookupSeq  uint64
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

	// Deferred model switch marker for non-submitting shortcuts such as Ctrl+R.
	// Coalesces repeated effort changes and is appended when the next user turn is sent.
	pendingModelSwitch *llm.ModelSwapMarker

	// Deferred model/effort switch requested while a provider stream is active.
	// The active llm.Engine must not be replaced mid-turn, so this is applied at
	// the earliest safe point: when the stream stops, or just before the next send
	// if the turn was aborted before a terminal stream event arrived.
	pendingStreamModelSwitch *pendingStreamModelSwitch

	// Stats tracking
	showStats  bool
	stats      *ui.SessionStats
	streamPerf *streamPerfTelemetry

	// Terminal/window title state
	titleMode      TerminalTitleMode
	titleFormat    string
	titleProgress  bool
	titleFormatter *terminalTitleFormatter
	titleManager   *terminalTitleManager
	// Live generated session-title state
	titleGenerationSessionID        string
	titleGenerationAttempts         int
	titleGenerationLastMessageCount int
	titleGenerationInFlight         bool
	titleManualEditVersion          uint64

	// Inspector mode
	inspectorMode  bool
	inspectorModel *inspector.Model

	// Resume browser mode
	resumeBrowserMode  bool
	resumeBrowserModel *sessionsui.Model

	// Worktree browser mode
	worktreeBrowserMode      bool
	worktreeBrowserModel     *worktreesui.Model
	worktreeBrowserRoot      string
	worktreeBrowserOperation string

	// Alt screen mode (full-screen rendering)
	altScreen               bool
	mouseMode               bool
	viewport                viewport.Model // Scrollable viewport for alt screen mode
	scrollToBottom          bool           // Flag to scroll to bottom after response completes
	streamRenderMinInterval time.Duration

	// Render cache for alt screen mode (avoids re-rendering unchanged content)
	viewCache struct {
		historyContent      string   // Cached rendered history
		historyLines        []string // Cached split history lines for cheap streaming-tail recomposition
		historyMsgCount     int      // Number of messages when cache was built
		historyWidth        int      // Width when cache was built
		historyScrollOffset int      // Scroll offset when cache was built
		historyValid        bool     // Whether cache has been populated
		lastViewportView    string   // Cached viewport.View() output
		lastYOffset         int      // Viewport Y offset when view was cached
		lastVPWidth         int      // Viewport width when view was cached
		lastVPHeight        int      // Viewport height when view was cached
		lastXOffset         int      // Viewport horizontal offset when view was cached
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
		lastSelection          Selection
		lastContentStr         string // stored for lazy contentLines split
		reasoningClickSnapshot reasoningClickSnapshot
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

	// Per-history reasoning block click overrides, keyed by rendered reasoning ordinal.
	reasoningExpansionOverrides map[int]bool

	// Mouse layout tracking for textarea click-to-cursor support
	textareaBoundsValid    bool
	textareaTopY           int
	textareaBottomY        int
	textareaLeftX          int
	textareaRightX         int
	textareaPromptWidth    int
	textareaEffectiveWidth int

	// Alt-screen terminal image rendering. Raw upload/control bytes are emitted
	// outside the Bubble Tea viewport; viewport content only contains captions plus
	// line-clipping-safe image display text (Kitty placeholders or ANSI blocks).
	// pendingImageUploads contains raw terminal image upload bytes that must be
	// emitted with tea.Raw, not embedded in View content (Bubble Tea parses View
	// content into cells and drops non-styling control sequences).
	pendingImageUploads         []string
	pendingImageUploadKeys      map[string]struct{}
	pendingImagePlaceKeys       map[string]struct{}
	uploadedImageKeys           map[string]struct{}
	placedImageKeys             map[string]struct{}
	visibleImageKeys            map[string]struct{}
	ownedKittyImageIDs          map[uint32]struct{}
	imageGeneration             uint64
	imageUploadFlushScheduled   bool
	imageCleanupQueued          bool
	imagePlaceholdersSuppressed bool
	viewportImageArtifacts      map[string]viewportImageArtifact
	viewportImageBlocks         []viewportImageBlock
	postFrameImageSeq           string
	postFrameImageUploadSeq     string
	postFrameImagePlaceSeq      string
	postFrameImageMu            sync.Mutex
	postFrameVisibleImages      map[string]postFrameImageState
	postFramePendingImages      map[string]postFrameImageState
	postFrameCurrentImages      map[string]postFrameImageState
	postFrameRenderCache        map[string]postFrameImageState
	postFrameQueuedImages       map[uint32]struct{}
	postFrameTransmittedImages  map[uint32]struct{}

	// Text selection state (alt-screen only)
	selection               Selection
	contentLines            []string // full viewport content split by \n
	copyStatus              string   // transient status message after copy attempt
	footerMessage           string   // transient footer message for short system notices
	footerMessageTone       string   // "", "muted", "success", "warning", or "error"
	footerMessageSeq        uint64   // monotonically increasing footer message timer token
	worktreeOperation       string   // non-empty while an async /worktree operation is running
	pendingWorktreeRecovery *pendingWorktreeRecovery

	attemptInput          int
	attemptOutput         int
	attemptCached         int
	attemptCacheWrite     int
	attemptUsageCalls     int
	attemptUsageCommitted bool
}

func (m *Model) releaseStreamCancelFunc() {
	if m == nil || m.streamCancelFunc == nil {
		return
	}
	m.streamCancelFunc()
	m.streamCancelFunc = nil
}

func (m *Model) setStreamCancelRequested(requested bool) {
	if m.streamCancelRequested == nil {
		return
	}
	m.streamCancelRequested.Store(requested)
}

func (m *Model) isStreamCancelRequested() bool {
	if m.streamCancelRequested == nil {
		return false
	}
	return m.streamCancelRequested.Load()
}

// Messages for tea.Program
type (
	// streamEventMsg wraps ui.StreamEvent for bubbletea
	streamEventMsg struct {
		event      ui.StreamEvent
		generation uint64
	}
	streamCancelTimeoutMsg struct {
		done       <-chan struct{}
		generation uint64
	}
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
		Parts          []llm.Part
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
	shellExitedMsg        struct {
		dir      string
		exitCode int
		err      error
	}
	titleGeneratedMsg struct {
		sessionID         string
		candidate         sessiontitle.Candidate
		generatedAt       time.Time
		basisMsgSeq       int
		err               error
		force             bool
		clearManualName   bool
		manualEditVersion uint64
	}
	mcpStatusUpdateMsg struct{ update mcp.StatusUpdate }
	GuardianReviewMsg  struct{ Event tools.GuardianEvent }
)

const (
	chatRenderThrottleEnv  = "TERM_LLM_CHAT_RENDER_THROTTLE_MS"
	chatSpinnerIntervalEnv = "TERM_LLM_CHAT_SPINNER_MS"
	chatDisableMouseEnv    = "TERM_LLM_DISABLE_MOUSE"
	streamCancelMaxWait    = 3 * time.Second
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

type chatGPTModelsLoadedMsg struct {
	models []llm.ModelInfo
	fresh  bool
	err    error
}

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
	return session.LoadActiveMessages(ctx, store, sess)
}

func loadSessionMessagesForScrollback(ctx context.Context, store session.Store, sess *session.Session) ([]session.Message, int, error) {
	return session.LoadScrollbackWithBoundary(ctx, store, sess)
}

func loadInitialSessionMessagesForScrollback(ctx context.Context, store session.Store, sess *session.Session) ([]session.Message, int, error) {
	return session.LoadInitialScrollbackWithBoundary(ctx, store, sess)
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

func (m *Model) reloadMessagesFromStore(ctx context.Context) error {
	if m.store == nil || m.sess == nil {
		return nil
	}
	if err := m.refreshSessionFromStore(ctx); err != nil {
		return err
	}
	loadedMsgs, compactionIdx, err := loadSessionMessagesForScrollback(ctx, m.store, m.sess)
	if err != nil {
		return err
	}
	if len(loadedMsgs) == 0 {
		m.messagesMu.Lock()
		hasExisting := len(m.messages) > 0
		m.messagesMu.Unlock()
		if hasExisting {
			return nil
		}
	}
	m.messagesMu.Lock()
	m.messages = loadedMsgs
	m.compactionIdx = compactionIdx
	m.messagesMu.Unlock()
	m.invalidateHistoryCache()
	return nil
}

func (m *Model) loadOlderScrollbackPrefix(ctx context.Context) {
	if m == nil || m.store == nil || m.sess == nil || m.olderScrollbackLoaded || !session.HasCompactionBoundary(m.sess) {
		return
	}
	// Streaming turns keep in-flight assistant state in m.messages while store
	// callbacks are still assigning IDs/updating rows. Do not replace that slice
	// with a persisted snapshot mid-stream; hydrate older display history after the
	// stream completes or on a later scroll.
	if m.streaming {
		return
	}
	loadedMsgs, compactionIdx, err := loadSessionMessagesForScrollback(ctx, m.store, m.sess)
	if err != nil || len(loadedMsgs) == 0 || compactionIdx <= 0 {
		m.olderScrollbackLoaded = true
		return
	}
	m.messagesMu.Lock()
	m.messages = loadedMsgs
	m.compactionIdx = compactionIdx
	m.messagesMu.Unlock()
	m.olderScrollbackLoaded = true
	m.invalidateHistoryCache()
}

// New creates a new chat model.
// fast-provider aware callers should use NewWithFastProvider.
func New(cfg *config.Config, provider llm.Provider, engine *llm.Engine, providerKey string, modelName string, mcpManager *mcp.Manager, maxTurns int, forceExternalSearch bool, disableExternalWebFetch bool, searchEnabled bool, localTools []string, toolsStr string, mcpStr string, showStats bool, initialText string, store session.Store, sess *session.Session, altScreen bool, autoSendQueue []string, autoSendExitOnDone bool, textMode bool, agentName string, platformDeveloperMessage string, yolo bool, toolMgrs ...*tools.ToolManager) *Model {
	return NewWithFastProvider(cfg, provider, nil, engine, providerKey, modelName, mcpManager, maxTurns, forceExternalSearch, disableExternalWebFetch, searchEnabled, localTools, toolsStr, mcpStr, showStats, initialText, store, sess, altScreen, autoSendQueue, autoSendExitOnDone, textMode, agentName, platformDeveloperMessage, yolo, toolMgrs...)
}

// NewWithFastProvider creates a new chat model with an optional fast provider
// for control-plane classification tasks.
func NewWithFastProvider(cfg *config.Config, provider llm.Provider, fastProvider llm.Provider, engine *llm.Engine, providerKey string, modelName string, mcpManager *mcp.Manager, maxTurns int, forceExternalSearch bool, disableExternalWebFetch bool, searchEnabled bool, localTools []string, toolsStr string, mcpStr string, showStats bool, initialText string, store session.Store, sess *session.Session, altScreen bool, autoSendQueue []string, autoSendExitOnDone bool, textMode bool, agentName string, platformDeveloperMessage string, yolo bool, toolMgrs ...*tools.ToolManager) *Model {
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
	composerPrompt := "❯ "
	ta.Placeholder = "Type a message..."
	ta.Prompt = composerPrompt
	ta.ShowLineNumbers = false
	ta.CharLimit = 0 // No limit
	ta.SetWidth(width)
	ta.SetHeight(1) // Start with single line
	// Use Bubble Tea's real cursor instead of the textarea's virtual cursor so
	// terminal-level composition/preedit UIs (for example macOS Dictation) have a
	// physical cursor location inside the composer, even though the status line is
	// rendered after it.
	ta.SetVirtualCursor(false)
	// Bubble's textarea currently forgets to apply Prompt style to the extra
	// end-of-buffer prompt rows unless a prompt func is used; without this the
	// empty composer prompt at the bottom renders plain while typed prompt rows
	// are themed. Use a constant prompt func so every prompt row takes the same
	// styling path.
	ta.SetPromptFunc(lipgloss.Width(composerPrompt), func(textarea.PromptInfo) string {
		return composerPrompt
	})
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
	// Keep full persisted scrollback available for the human UI, but remember the
	// compaction boundary so buildMessages only sends the active post-compaction
	// window to the LLM.
	var messages []session.Message
	var compactionIdx int
	if store != nil && sess.ID != "" {
		if loadedMsgs, idx, err := loadInitialSessionMessagesForScrollback(context.Background(), store, sess); err == nil {
			messages = loadedMsgs
			compactionIdx = idx
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
	reasoningCfg := config.DefaultReasoningConfig()
	if cfg != nil {
		reasoningCfg = cfg.ResolveReasoning("chat")
	}
	chatRenderer.SetReasoningConfig(reasoningCfg)

	// Create tracker with text mode setting
	tracker := ui.NewToolTracker()
	tracker.TextMode = textMode

	stats := ui.NewSessionStats()
	if sess != nil {
		stats.SeedTotals(sess.InputTokens, sess.OutputTokens, sess.CachedInputTokens, sess.CacheWriteTokens, sess.ToolCalls, sess.LLMTurns+sess.CompactionCount)
	}

	var mcpStatusChan chan mcp.StatusUpdate
	if mcpManager != nil {
		mcpStatusChan = make(chan mcp.StatusUpdate, 32)
		mcpManager.SetStatusChannel(mcpStatusChan)
	}

	fastProviderDefault := false
	fastMode := false
	var modelMetadata []llm.ModelInfo
	fastMetadataLoaded := false
	providerIsChatGPT := false
	providerType := config.ProviderType("")
	if pc, ok := cfg.Providers[providerKey]; ok {
		providerType = config.InferProviderType(providerKey, pc.Type)
		fastProviderDefault = llm.NormalizeServiceTier(pc.ServiceTier) == llm.ServiceTierFast
	} else {
		providerType = config.InferProviderType(providerKey, "")
	}
	providerIsChatGPT = providerType == config.ProviderTypeChatGPT
	fastMode = fastProviderDefault
	fastMetadataStale := false
	if providerIsChatGPT {
		if cached, fresh, err := llm.CachedChatGPTModels(); err == nil {
			modelMetadata = cached
			fastMetadataLoaded = true
			fastMetadataStale = !fresh
		}
	}

	titleMode, _ := ParseTerminalTitleMode(cfg.Chat.TerminalTitle)
	var toolMgr *tools.ToolManager
	if len(toolMgrs) > 0 {
		toolMgr = toolMgrs[0]
	}

	model := &Model{
		width:                      width,
		height:                     height,
		textarea:                   ta,
		spinner:                    s,
		styles:                     styles,
		keyMap:                     DefaultKeyMap(),
		store:                      store,
		sess:                       sess,
		messages:                   messages,
		compactionIdx:              compactionIdx,
		olderScrollbackLoaded:      !session.HasCompactionBoundary(sess) || compactionIdx > 0,
		rootCtx:                    context.Background(),
		provider:                   provider,
		fastProvider:               fastProvider,
		engine:                     engine,
		config:                     cfg,
		providerName:               provider.Name(),
		providerKey:                providerKey,
		modelName:                  modelName,
		agentName:                  agentName,
		platformDeveloperMessage:   strings.TrimSpace(platformDeveloperMessage),
		currentOrigin:              session.OriginTUI,
		yolo:                       yolo,
		phase:                      "Thinking",
		reasoningConfig:            reasoningCfg,
		viewportRows:               ui.RemainingLines(height, 8), // Reserve space for input and status
		tracker:                    tracker,
		toolMgr:                    toolMgr,
		subagentTracker:            subagentTracker,
		smoothBuffer:               ui.NewSmoothBuffer(),
		completions:                completions,
		dialog:                     dialog,
		approvedDirs:               approvedDirs,
		mcpManager:                 mcpManager,
		mcpStatusChan:              mcpStatusChan,
		maxTurns:                   maxTurns,
		forceExternalSearch:        forceExternalSearch,
		disableExternalWebFetch:    disableExternalWebFetch,
		searchEnabled:              searchEnabled,
		fastMode:                   fastMode,
		fastProviderDefault:        fastProviderDefault,
		fastMetadataLoaded:         fastMetadataLoaded,
		fastMetadataStale:          fastMetadataStale,
		modelMetadata:              modelMetadata,
		localTools:                 localTools,
		toolsStr:                   toolsStr,
		mcpStr:                     mcpStr,
		showStats:                  showStats,
		stats:                      stats,
		streamPerf:                 newStreamPerfTelemetryFromEnv(),
		titleMode:                  titleMode,
		titleFormat:                cfg.Chat.TerminalTitleFormat,
		titleProgress:              cfg.Chat.TerminalProgress,
		titleFormatter:             newTerminalTitleFormatter(cfg.Chat.TerminalTitleFormat, TerminalTitleEnvironment{}),
		titleManager:               newTerminalTitleManager(titleMode, TerminalTitleEnvironment{}, cfg.Chat.TerminalProgress),
		streamCancelRequested:      &atomic.Bool{},
		altScreen:                  altScreen,
		mouseMode:                  chatMouseModeFromEnv(),
		viewport:                   vp,
		streamRenderMinInterval:    chatRenderMinIntervalFromEnv(),
		chatRenderer:               chatRenderer,
		autoSendQueue:              autoSendQueue,
		autoSendExitOnDone:         autoSendExitOnDone,
		textMode:                   textMode,
		uploadedImageKeys:          make(map[string]struct{}),
		placedImageKeys:            make(map[string]struct{}),
		pendingImageUploadKeys:     make(map[string]struct{}),
		pendingImagePlaceKeys:      make(map[string]struct{}),
		visibleImageKeys:           make(map[string]struct{}),
		ownedKittyImageIDs:         make(map[uint32]struct{}),
		postFrameVisibleImages:     make(map[string]postFrameImageState),
		postFrameRenderCache:       make(map[string]postFrameImageState),
		postFrameTransmittedImages: make(map[uint32]struct{}),
		viewportImageArtifacts:     make(map[string]viewportImageArtifact),
		selectedImage:              -1,
		selectedInterjection:       -1,
	}
	if internalreasoning.RawDisplayBlocked(reasoningCfg) {
		model.SetFooterWarning("Raw reasoning display is disabled. Set reasoning.raw=true or TERM_LLM_SHOW_RAW_REASONING=1 to allow it.")
		model.reasoningRawWarned = true
	}
	model.configureImageRenderer()
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
	m.stats.SeedTotals(m.sess.InputTokens, m.sess.OutputTokens, m.sess.CachedInputTokens, m.sess.CacheWriteTokens, m.sess.ToolCalls, m.sess.LLMTurns+m.sess.CompactionCount)
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
	if total <= 0 {
		if m.sess.LastTotalTokens != 0 || m.sess.LastMessageCount != 0 {
			_ = session.ResetContextEstimate(ctx, m.store, m.sess)
		}
		return
	}
	_ = m.store.UpdateContextEstimate(ctx, m.sess.ID, total, count)
	m.sess.LastTotalTokens = total
	m.sess.LastMessageCount = count
}

func (m *Model) resetContextEstimateBaseline(ctx context.Context) {
	if m == nil {
		return
	}
	if m.engine != nil {
		m.engine.SetContextEstimateBaseline(0, 0)
	}
	if m.sess == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if m.store != nil && (m.sess.LastTotalTokens != 0 || m.sess.LastMessageCount != 0) {
		_ = session.ResetContextEstimate(ctx, m.store, m.sess)
	}
	m.sess.LastTotalTokens = 0
	m.sess.LastMessageCount = 0
}

// WantsReload reports whether the user requested a binary reload via /reload.
func (m *Model) WantsReload() bool { return m.reloadRequested }

// ReloadSessionID returns the session ID to resume after a reload, if any.
func (m *Model) ReloadSessionID() string { return m.reloadSessionID }

func (m *Model) applyWindowSize(msg tea.WindowSizeMsg) {
	oldWidth := m.width
	oldViewportHeight := 0
	if m.altScreen {
		oldViewportHeight = m.viewport.Height()
	}
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
		viewportHeightChanged := oldViewportHeight > 0 && oldViewportHeight != m.viewport.Height()
		widthChanged := oldWidth > 0 && oldWidth != m.width
		if widthChanged || viewportHeightChanged {
			m.imageGeneration++
			termimage.ClearCache()
			termimage.Debugf(termimage.DefaultEnvironment(), "chat resize width %d->%d viewport_h %d->%d model_h=%d generation=%d: invalidate image viewport render", oldWidth, m.width, oldViewportHeight, m.viewport.Height(), m.height, m.imageGeneration)
			if m.chatRenderer != nil {
				m.chatRenderer.InvalidateCache()
			}
			m.viewCache.lastSetContentAt = time.Time{}
			m.viewCache.lastViewportView = ""
			m.viewCache.cachedTrackerVersion = 0
			// Drop old-generation raw operations before queuing cleanup. Otherwise a
			// resize that lands between View() and the upload flush can transmit stale
			// image bytes after the cleanup for the new layout.
			m.pendingImageUploads = nil
			m.pendingImageUploadKeys = make(map[string]struct{})
			m.pendingImagePlaceKeys = make(map[string]struct{})
			m.imageCleanupQueued = false
			m.imagePlaceholdersSuppressed = true
			m.queueImageCleanup()
			m.viewportImageArtifacts = make(map[string]viewportImageArtifact)
			m.viewportImageBlocks = nil
			m.ownedKittyImageIDs = make(map[uint32]struct{})
			m.postFrameVisibleImages = make(map[string]postFrameImageState)
			m.postFramePendingImages = nil
			m.postFrameCurrentImages = nil
			m.postFrameRenderCache = make(map[string]postFrameImageState)
			m.postFrameQueuedImages = nil
			m.postFrameTransmittedImages = make(map[uint32]struct{})
			m.resetUploadedImageKeys()
		}
	} else if m.chatRenderer != nil {
		m.chatRenderer.SetSize(m.width, m.height)
	}
}

func formatStreamErrorFooter(err error) string {
	if err == nil {
		return "Stream failed."
	}
	if errors.Is(err, context.Canceled) {
		return "Stream cancelled."
	}
	var incomplete *llm.StreamIncompleteError
	if errors.As(err, &incomplete) {
		return "Stream interrupted before completion."
	}
	return "Stream failed: " + err.Error()
}

func (m *Model) resetAttemptUsage() {
	m.attemptInput, m.attemptOutput, m.attemptCached, m.attemptCacheWrite, m.attemptUsageCalls = 0, 0, 0, 0, 0
	m.attemptUsageCommitted = false
}

func (m *Model) markAttemptCommitted() {
	m.attemptInput, m.attemptOutput, m.attemptCached, m.attemptCacheWrite, m.attemptUsageCalls = 0, 0, 0, 0, 0
	m.attemptUsageCommitted = true
}

func (m *Model) setRetryStatus(status string) {
	if m.retryStatus == status {
		return
	}
	m.retryStatus = status
	if m.altScreen {
		// Retry status is part of the streaming viewport. Bypass render throttling
		// so stale retry banners do not linger after forward progress resumes.
		m.viewCache.lastSetContentAt = time.Time{}
		m.viewCache.lastViewportView = ""
		m.resetAltScreenStreamingAppendCache()
	}
	m.bumpContentVersion()
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

// resetRetainedStreamTracker clears the tracker that is intentionally kept
// populated after an alt-screen chat stream finishes (so reasoning headers stay
// click-toggleable). Call this before starting a fresh stream that does not go
// through sendMessage — compaction and manual handover — so the previous turn,
// shown from history once completedStream has been cleared (e.g. by a resize),
// is not re-rendered a second time from the stale tracker by
// renderStreamingInline. Tool-initiated handovers continue the current engine
// stream and must keep their tracker, so those callers skip this.
func (m *Model) resetRetainedStreamTracker() {
	if m.altScreen && m.tracker != nil {
		m.resetTracker()
	}
}

func (m *Model) preserveStreamingContentOnError() {
	if !m.altScreen || m.tracker == nil {
		return
	}
	m.tracker.CompleteTextSegments(func(text string) string {
		return m.renderMarkdown(text)
	})
	m.tracker.ForceFailPendingTools()
	completed := m.tracker.CompletedSegments()
	if len(completed) == 0 {
		return
	}
	m.resetAltScreenStreamingAppendCache()
	m.viewCache.completedStream = ui.RenderSegmentsWithImageRenderer(completed, m.width, -1, m.renderMd, true, m.toolsExpanded, m.imageArtifactRenderer())
}

func (m *Model) renderStreamingContentOnErrorForScrollback() string {
	if m.altScreen || m.tracker == nil {
		return ""
	}
	m.tracker.CompleteTextSegments(func(text string) string {
		return m.renderMarkdown(text)
	})
	m.tracker.ForceFailPendingTools()
	return m.tracker.FlushAllRemaining(m.width, 0, m.renderMd).ToPrint
}

func (m *Model) flushStreamingContentOnErrorToScrollback() []tea.Cmd {
	output := m.renderStreamingContentOnErrorForScrollback()
	if output == "" {
		return nil
	}
	return ui.ScrollbackPrintlnCommands(output, true)
}

func (m *Model) interruptedAssistantFallbackMessage() (llm.Message, bool) {
	m.pendingMu.Lock()
	if m.pendingAssistantSnapshotSet {
		assistantMsg := m.pendingAssistantSnapshot
		m.pendingMu.Unlock()
		return assistantMsg, true
	}
	completedAssistantTurns := m.completedAssistantTurns
	m.pendingMu.Unlock()

	// Store-backed streams persist each assistant/tool response as a separate
	// turn row. Once a turn has completed, currentResponse is cumulative across
	// turns, so using it as a fallback would duplicate earlier assistant text into
	// the interrupted turn. Without a per-turn snapshot, fail open and leave the
	// already persisted rows intact.
	if m.store != nil && completedAssistantTurns > 0 {
		return llm.Message{}, false
	}

	responseContent := m.currentResponse.String()
	reasoningContent, reasoningKind, reasoningTitle := m.currentReasoningPartMetadata()
	if responseContent == "" && reasoningContent == "" {
		return llm.Message{}, false
	}

	part := llm.Part{Type: llm.PartText, Text: responseContent}
	if reasoningContent != "" {
		part.ReasoningContent = reasoningContent
		part.ReasoningKind = reasoningKind
		part.ReasoningSummaryTitle = reasoningTitle
	}

	return llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{part}}, true
}

type interruptedAssistantSalvageResult struct {
	message          session.Message
	ok               bool
	persisted        bool
	replaceMessageID int64
}

func (m *Model) salvageInterruptedAssistantMessage() interruptedAssistantSalvageResult {
	if m.sess == nil {
		return interruptedAssistantSalvageResult{}
	}
	assistantMsg, ok := m.interruptedAssistantFallbackMessage()
	if !ok {
		return interruptedAssistantSalvageResult{}
	}

	sessionMsg := session.NewMessageWithReasoningPolicy(m.sess.ID, assistantMsg, -1, m.effectiveReasoningConfig())
	sessionMsg.DurationMs = time.Since(m.streamStartTime).Milliseconds()

	m.messagesMu.Lock()
	localMsg := *sessionMsg
	localMsg.Sequence = len(m.messages)
	appendedIdx := len(m.messages)
	m.messages = append(m.messages, localMsg)
	m.messagesMu.Unlock()
	m.invalidateHistoryCache()

	result := interruptedAssistantSalvageResult{message: localMsg, ok: true}

	if m.store == nil {
		result.persisted = true
		return result
	}

	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()

	m.pendingMu.Lock()
	pendingAssistantMsgID := m.pendingAssistantMsgID
	m.pendingMu.Unlock()
	if pendingAssistantMsgID != 0 {
		result.replaceMessageID = pendingAssistantMsgID
		sessionMsg.ID = pendingAssistantMsgID
		err := m.store.UpdateMessage(dbCtx, m.sess.ID, sessionMsg)
		if err == nil {
			result.persisted = true
			result.message.ID = sessionMsg.ID
			m.messagesMu.Lock()
			if appendedIdx >= 0 && appendedIdx < len(m.messages) {
				m.messages[appendedIdx].ID = sessionMsg.ID
			}
			m.messagesMu.Unlock()
			return result
		}
		if !errors.Is(err, session.ErrNotFound) {
			return result
		}
		result.replaceMessageID = 0
		sessionMsg.ID = 0
	}

	if err := m.store.AddMessage(dbCtx, m.sess.ID, sessionMsg); err == nil {
		result.persisted = true
		result.message.ID = sessionMsg.ID
		m.messagesMu.Lock()
		if appendedIdx >= 0 && appendedIdx < len(m.messages) {
			m.messages[appendedIdx].ID = sessionMsg.ID
		}
		m.messagesMu.Unlock()
	}
	return result
}

func (m *Model) mergeUnpersistedInterruptedAssistant(result interruptedAssistantSalvageResult) {
	if !result.ok || result.persisted {
		return
	}

	m.messagesMu.Lock()
	changed := false
	msg := result.message
	if result.replaceMessageID != 0 {
		for i := range m.messages {
			if m.messages[i].ID == result.replaceMessageID {
				msg.ID = result.replaceMessageID
				msg.Sequence = m.messages[i].Sequence
				m.messages[i] = msg
				changed = true
				break
			}
		}
	}
	if !changed {
		for _, existing := range m.messages {
			if msg.ID != 0 && existing.ID == msg.ID {
				m.messagesMu.Unlock()
				return
			}
		}
		msg.Sequence = len(m.messages)
		m.messages = append(m.messages, msg)
		changed = true
	}
	m.messagesMu.Unlock()

	if changed {
		m.invalidateHistoryCache()
	}
}

// SetAgentResolver configures the function used to resolve agent names
// during /handover. The function should match cmd.LoadAgent's signature.
func (m *Model) SetAgentResolver(resolver func(name string, cfg *config.Config) (*agents.Agent, error)) {
	m.agentResolver = resolver
}

// SetHandoverSystemPromptResolver configures the normal chat-startup prompt
// pipeline used to resolve the target agent's persisted handover system prompt.
func (m *Model) SetHandoverSystemPromptResolver(resolver func(agent *agents.Agent, providerKey, modelName string) (string, error)) {
	m.handoverSystemPromptResolver = resolver
}

// SetRuntimeSystemContextResolver configures directory-aware prompt and skill
// resolution for live worktree changes and handovers.
func (m *Model) SetRuntimeSystemContextResolver(resolver func(agent *agents.Agent, providerKey, modelName, dir string) (RuntimeSystemContext, error), current RuntimeSystemContext) {
	m.runtimeSystemContextResolver = resolver
	m.runtimeSystemContext = current
}

// SetGuardianReviewerRefresh configures reviewer replacement after model changes.
func (m *Model) SetGuardianReviewerRefresh(refresh func(providerKey, modelName string) error) {
	m.guardianReviewerRefresh = refresh
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

// SetRunner configures the shared execution runner used for chat turns.
func (m *Model) SetRunner(runner runpkg.Runner) {
	m.runner = runner
}

// SetProgram gives the model a handle to the running Bubble Tea program for
// scheduling raw terminal writes that are discovered during View rendering.
func (m *Model) SetProgram(p *tea.Program) {
	m.program = p
}

func (m *Model) rootContext() context.Context {
	if m.rootCtx != nil {
		return m.rootCtx
	}
	return context.Background()
}

func (m *Model) autoSendMessageStats() string {
	if m == nil || !m.showStats || m.stats == nil || m.streamStartTime.IsZero() {
		return ""
	}
	elapsed := time.Since(m.streamStartTime)
	return fmt.Sprintf("[Message %d] %.1fs", m.stats.LLMCallCount, elapsed.Seconds())
}

// Init initializes the model.
func (m *Model) Init() tea.Cmd {
	// Update textarea height for any initial text
	m.updateTextareaHeight()

	baseCmds := []tea.Cmd{textarea.Blink, m.spinner.Tick}
	if (!m.fastMetadataLoaded || m.fastMetadataStale) && !m.fastMetadataLoading {
		if cmd := m.loadChatGPTModelsCmd(); cmd != nil {
			baseCmds = append(baseCmds, cmd)
		}
	}
	if cmd := m.listenForMCPStatusUpdates(); cmd != nil {
		baseCmds = append(baseCmds, cmd)
	}
	if cmd := m.terminalTitleCmd(); cmd != nil {
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

// YoloModeActive returns the current effective yolo state, including approval
// managers that may have been toggled during the session.
func (m *Model) YoloModeActive() bool {
	return m.isYoloModeActive()
}

// ApprovalModeActive returns the current effective approval mode.
func (m *Model) ApprovalModeActive() tools.ApprovalMode {
	return m.currentApprovalMode()
}

// PersistApprovalModeActive stores the model's current effective approval mode on the session.
func (m *Model) PersistApprovalModeActive() {
	m.persistApprovalMode(m.currentApprovalMode())
}

// WaitStreamDone blocks until the engine streaming goroutine has exited.
// It is safe to call when no stream was started (no-op). Shutdown must not
// hang forever if a provider/tool ignores cancellation, so the wait is bounded
// by the same hard stop budget used by the interactive cancel watchdog.
func (m *Model) WaitStreamDone() {
	if m.streamDone == nil {
		return
	}
	select {
	case <-m.streamDone:
	case <-time.After(streamCancelMaxWait):
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

func (m *Model) handleStreamCancelTimeout(msg streamCancelTimeoutMsg) (tea.Model, tea.Cmd) {
	if msg.done != m.streamDone || msg.generation != m.streamGeneration {
		return m, nil
	}
	if !m.streaming || !m.isStreamCancelRequested() {
		return m, nil
	}
	return m.Update(streamEventMsg{event: ui.ErrorEvent(context.Canceled), generation: msg.generation})
}

func (m *Model) shouldIgnoreStreamEvent(msg streamEventMsg) bool {
	if msg.generation != 0 && msg.generation != m.streamGeneration {
		return true
	}
	switch msg.event.Type {
	case ui.StreamEventDone, ui.StreamEventError:
		return !m.streaming
	default:
		return false
	}
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
	if timeoutMsg, ok := msg.(streamCancelTimeoutMsg); ok {
		return m.handleStreamCancelTimeout(timeoutMsg)
	}
	if usageMsg, ok := msg.(compactionUsageMsg); ok {
		// Compaction callbacks run on the stream goroutine. Apply stats and
		// session-counter mutations only on Bubble Tea's Update goroutine.
		m.recordCompactionUsage(context.Background(), usageMsg.sessionID, usageMsg.model, usageMsg.usage)
		return m, nil
	}
	if handled, cmd := m.handleTerminalTitleProviderMsg(msg); handled {
		return m, cmd
	}

	// Chat-owned self-scheduling ticks must keep running even while an embedded
	// modal is active. If a spinner tick is forwarded to the inspector/session
	// browser, the child ignores it and the spinner never schedules its next tick,
	// so it appears frozen after returning to chat.
	_, isSpinnerTick := msg.(spinner.TickMsg)

	// Handle worktree browser mode. Its operation completion messages must be
	// routed back to the child rather than the normal slash-command handler.
	if m.worktreeBrowserMode && !isSpinnerTick {
		return m.updateWorktreeBrowserMode(msg)
	}

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
		// Open dialogs are modal: route mouse wheel events to scrollable content
		// dialogs before text selection, textarea clicks, or viewport scrolling.
		if m.dialog.IsOpen() && m.dialog.Type() == DialogContent {
			if _, ok := msg.(tea.MouseWheelMsg); ok {
				m.dialog.Update(msg)
				return m, nil
			}
		}
		// Single-clicking a reasoning header toggles just that block. This runs
		// before drag-selection, and only consumes clicks on recognized headers.
		if m.handleReasoningMouseClick(msg) {
			m.selection = Selection{}
			return m, nil
		}
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
			if mouse := msg.Mouse(); mouse.Button == tea.MouseWheelUp {
				m.loadOlderScrollbackPrefix(context.Background())
			}
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

	case imageUploadFlushMsg:
		if cmd := m.drainPendingImageUploadCmd(); cmd != nil {
			return m, cmd
		}

	case imageCleanupFlushedMsg:
		if cmd := m.finishImageCleanupFlush(); cmd != nil {
			return m, cmd
		}
		return m, nil

	case footerMessageClearMsg:
		if msg.Seq == m.footerMessageSeq {
			m.clearFooterMessage()
		}

	case shellExitedMsg:
		m.pausedForExternalUI = false
		if msg.err != nil {
			return m.showFooterError(fmt.Sprintf("Shell failed: %v", msg.err))
		}
		if msg.exitCode != 0 {
			return m.showFooterMuted(fmt.Sprintf("Shell exited with status %d.", msg.exitCode))
		}
		return m.showFooterMuted("Shell exited.")

	case worktreeOperationDoneMsg:
		return m.handleWorktreeOperationDone(msg)

	case shareDoneMsg:
		return m.handleShareDone(msg)

	case chatGPTModelsLoadedMsg:
		return m.applyChatGPTModelsLoaded(msg)

	case promptHistoryLookupMsg:
		return m.handlePromptHistoryLookupMsg(msg)

	case mcpStatusUpdateMsg:
		m.refreshMCPPickerIfOpen()
		cmds = append(cmds, m.listenForMCPStatusUpdates())

	case GuardianReviewMsg:
		message := strings.TrimSpace(msg.Event.Message)
		tone := guardianFooterTone(message)
		if m.tracker != nil {
			if msg.Event.ToolCallID != "" {
				m.tracker.HandleGuardianEvent(msg.Event)
			} else if message != "" {
				// Session-level guardian status (for example a circuit breaker)
				// has no tool row to annotate, so retain it durably in the stream.
				m.tracker.AddExternalUIResult(message)
			}
			m.invalidateViewCache()
		}
		_, cmd := m.showFooterMessageWithTone(message, tone)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case interruptClassifiedMsg:
		return m.handleInterruptClassified(msg)

	case compactDoneMsg:
		m.streaming = false
		m.phase = "Thinking"
		m.releaseStreamCancelFunc()
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) || errors.Is(msg.err, context.DeadlineExceeded) {
				return m.showFooterMuted("Compaction cancelled.")
			}
			return m.showFooterError(fmt.Sprintf("Compaction failed: %v", msg.err))
		}
		if msg.result == nil {
			return m.showFooterError("Compaction failed: no result returned.")
		}
		m.messagesMu.Lock()
		full := append([]session.Message(nil), m.messages...)
		m.messagesMu.Unlock()
		var updated []session.Message
		var activeStart int
		var refreshed *session.Session
		if m.store != nil {
			var err error
			updated, activeStart, refreshed, err = session.ApplyCompaction(context.Background(), m.store, m.sess, full, msg.result)
			if err != nil {
				return m.showFooterError(fmt.Sprintf("Compaction finished, but saving failed: %v", err))
			}
		} else {
			updated, activeStart, refreshed, _ = session.ApplyCompaction(context.Background(), nil, m.sess, full, msg.result)
		}
		if refreshed != nil {
			m.sess = refreshed
		}
		m.recordCompactionUsage(context.Background(), sessionIDOf(m.sess), msg.result.Model, msg.result.Usage)
		m.messagesMu.Lock()
		m.messages = updated
		m.compactionIdx = activeStart
		m.messagesMu.Unlock()
		m.invalidateHistoryCache()

		if m.engine != nil {
			m.engine.ResetConversation()
			m.engine.SetContextEstimateBaseline(0, 0)
		}
		return m.showFooterSuccess("Conversation compacted.")

	case handoverDoneMsg:
		m.streaming = false
		m.phase = "Thinking"
		// Don't cancel the stream when a tool-initiated handover is pending:
		// the engine must stay alive to receive the tool result.
		if m.handoverToolDoneCh == nil {
			m.releaseStreamCancelFunc()
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
		return m, m.terminalTitleCmd()

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
		return m, m.terminalTitleCmd()

	case handoverRenameDoneMsg:
		// Silently ignore — rename is best-effort background work.
		return m, nil

	case titleFallbackTickMsg:
		if m.sess != nil && msg.sessionID == m.sess.ID {
			if cmd := m.maybeGenerateSessionTitleCmd(); cmd != nil {
				return m, cmd
			}
		}
		return m, nil

	case titleGeneratedMsg:
		if msg.sessionID == m.titleGenerationSessionID {
			m.titleGenerationInFlight = false
		}
		if msg.sessionID == "" || m.sess == nil || msg.sessionID != m.sess.ID {
			return m, nil
		}
		if msg.err != nil {
			if msg.force {
				return m.showFooterError(fmt.Sprintf("Title generation failed: %v", msg.err))
			}
			return m, nil
		}
		if msg.force {
			if msg.manualEditVersion != m.titleManualEditVersion {
				return m, nil
			}
			if msg.clearManualName {
				m.sess.Name = ""
			} else if strings.TrimSpace(m.sess.Name) != "" || m.sess.TitleSource == session.TitleSourceUser {
				return m, nil
			}
			m.sess.GeneratedShortTitle = msg.candidate.ShortTitle
			m.sess.GeneratedLongTitle = msg.candidate.LongTitle
			m.sess.TitleSource = session.TitleSourceGenerated
			m.sess.TitleGeneratedAt = msg.generatedAt
			m.sess.TitleBasisMsgSeq = msg.basisMsgSeq
			if m.store != nil {
				var err error
				if msg.clearManualName {
					err = m.store.Update(context.Background(), m.sess)
				} else {
					err = session.UpdateGeneratedTitle(context.Background(), m.store, m.sess, msg.candidate.ShortTitle, msg.candidate.LongTitle, msg.generatedAt, msg.basisMsgSeq)
				}
				if err != nil {
					return m.showFooterError(fmt.Sprintf("Failed to update title: %v", err))
				}
			}
			updated, footerCmd := m.showFooterSuccess(fmt.Sprintf("Updated title: %s", msg.candidate.ShortTitle))
			return updated, tea.Batch(footerCmd, m.terminalTitleCmd())
		}
		if strings.TrimSpace(m.sess.GeneratedShortTitle) == "" {
			m.sess.GeneratedShortTitle = msg.candidate.ShortTitle
			m.sess.GeneratedLongTitle = msg.candidate.LongTitle
			m.sess.TitleSource = session.TitleSourceGenerated
			m.sess.TitleGeneratedAt = msg.generatedAt
			m.sess.TitleBasisMsgSeq = msg.basisMsgSeq
		}
		return m, m.terminalTitleCmd()

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
			if m.streamPerf != nil {
				m.streamPerf.RecordSmoothTickHandled(hadWords, m.smoothBuffer.Len())
				m.streamPerf.RecordDuration(durationMetricSmoothTick, time.Since(tickStart))
			}
		}

	case streamEventMsg:
		streamEventStart := time.Now()
		ev := msg.event
		if m.shouldIgnoreStreamEvent(msg) {
			return m, nil
		}
		if m.streamPerf != nil {
			m.streamPerf.tracef("stream_event type=%v", ev.Type)
		}

		switch ev.Type {
		case ui.StreamEventError:
			if ev.Err != nil {
				m.setRetryStatus("")
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
				// This resets local scheduling state for the next stream.
				// An already in-flight render tick may still arrive and no-op.
				m.streamRenderTickPending = false
				m.preserveStreamingContentOnError()
				errorOutputCmds := m.flushStreamingContentOnErrorToScrollback()
				salvageResult := m.salvageInterruptedAssistantMessage()
				m.resetCurrentReasoning()
				m.streaming = false
				m.releaseStreamCancelFunc()
				m.setStreamCancelRequested(false)
				m.err = nil
				var footerCmd tea.Cmd
				if !errors.Is(ev.Err, context.Canceled) {
					_, footerCmd = m.showFooterMessageWithTone(formatStreamErrorFooter(ev.Err), "error")
				}
				// Stream errors are transient status, not durable conversation content.
				// Force the viewport to repaint now so stale streamed rows do not linger
				// until an external resize invalidates Bubble Tea's render cache.
				if m.altScreen {
					m.viewCache.historyValid = false
					m.viewCache.lastViewportView = ""
					m.viewCache.cachedCompletedContent = ""
					m.viewCache.cachedTrackerVersion = 0
					m.resetAltScreenStreamingAppendCache()
					m.bumpContentVersion()
				}

				// Clear callbacks and update status
				m.clearStreamCallbacks()
				if m.store != nil && m.sess != nil {
					// Use interrupted for cancellation, error for other failures
					status := session.StatusError
					if errors.Is(ev.Err, context.Canceled) {
						status = session.StatusInterrupted
					}
					ctx := context.Background()
					_ = m.store.UpdateStatus(ctx, m.sess.ID, status)
					if err := m.reloadMessagesFromStore(ctx); err != nil {
						if footerCmd == nil {
							_, footerCmd = m.showFooterMessageWithTone(fmt.Sprintf("Session message reload failed after interruption: %v", err), "error")
						}
					} else {
						m.mergeUnpersistedInterruptedAssistant(salvageResult)
					}
				}

				if cmd := m.applyPendingStreamModelSwitch(); cmd != nil {
					errorOutputCmds = append(errorOutputCmds, cmd)
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

				titleCmd := m.terminalTitleCmd()
				if m.altScreen {
					return m, tea.Batch(tea.ClearScreen, footerCmd, titleCmd)
				}
				if titleCmd != nil {
					errorOutputCmds = append(errorOutputCmds, titleCmd)
				}
				if footerCmd != nil {
					errorOutputCmds = append(errorOutputCmds, footerCmd)
				}
				return m, tea.Sequence(errorOutputCmds...)
			}

		case ui.StreamEventToolStart:
			m.commitCurrentReasoningToStream()
			m.setRetryStatus("")
			m.markAttemptCommitted()
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
				ui.AttachSubagentProgressToSegment(m.tracker, m.subagentTracker, ev.ToolCallID)
			}

			// Check for web search
			if ev.ToolName == llm.WebSearchToolName || ev.ToolName == "WebSearch" {
				m.webSearchUsed = true
			}

		case ui.StreamEventToolEnd:
			m.setRetryStatus("")
			m.resetAttemptUsage()
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
			if m.stats != nil {
				m.stats.GenerationEnd()
			}
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
			if m.stats != nil {
				// Even an all-zero terminal usage event consumes pending request
				// timing; SessionStats intentionally does not count it as a call.
				m.stats.AddUsage(inputTokens, outputTokens, cachedTokens, writeTokens)
			}
			if inputTokens > 0 || outputTokens > 0 || cachedTokens > 0 || writeTokens > 0 {
				if !m.attemptUsageCommitted {
					m.attemptInput += inputTokens
					m.attemptOutput += outputTokens
					m.attemptCached += cachedTokens
					m.attemptCacheWrite += writeTokens
					m.attemptUsageCalls++
				}
			}
		case ui.StreamEventText:
			m.commitCurrentReasoningToStream()
			m.attemptUsageCommitted = false
			if m.stats != nil && ev.Text != "" {
				m.stats.ObserveOutput()
			}
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
			m.setRetryStatus("")

		case ui.StreamEventGenerationActivity:
			if m.stats != nil {
				m.stats.ObserveOutput()
			}

		case ui.StreamEventReasoning:
			if m.stats != nil && ev.ReasoningText != "" {
				m.stats.ObserveOutput()
			}
			m.handleReasoningStreamEvent(ev)

		case ui.StreamEventAttemptDiscard:
			if m.stats != nil {
				m.stats.DiscardUsage(m.attemptInput, m.attemptOutput, m.attemptCached, m.attemptCacheWrite, m.attemptUsageCalls)
			}
			m.resetAttemptUsage()
			m.currentResponse.Reset()
			m.pendingMu.Lock()
			m.pendingAssistantSnapshot = llm.Message{}
			m.pendingAssistantSnapshotSet = false
			m.pendingMu.Unlock()
			m.resetCurrentReasoning()
			if m.smoothBuffer != nil {
				m.smoothBuffer.Reset()
			}
			m.smoothTickPending = false
			m.streamRenderTickPending = false
			if m.tracker != nil {
				m.tracker.DiscardAttempt()
			}
			m.newlineCompactor = ui.NewStreamingNewlineCompactor(ui.MaxStreamingConsecutiveNewlines)
			m.setRetryStatus("Interrupted response discarded; retrying...")
			m.phase = "Retrying"
			m.viewCache.cachedCompletedContent = ""
			m.viewCache.cachedTrackerVersion = 0
			m.viewCache.lastViewportView = ""
			m.resetAltScreenStreamingAppendCache()
			// Discard is a rollback, not another append. Force the very next View to
			// rebuild viewport content immediately even when streaming render throttling
			// is active; otherwise stale partial text can remain visible until resize or
			// the next throttle tick.
			m.viewCache.lastSetContentAt = time.Time{}
			m.scrollToBottom = true
			m.bumpContentVersion()
			if m.altScreen {
				cmds = append(cmds, tea.ClearScreen)
			}

		case ui.StreamEventPhase:
			m.phase = ev.Phase
			m.setRetryStatus("")
			// Display WARNING phases as visible text in the conversation
			if strings.HasPrefix(ev.Phase, llm.WarningPhasePrefix) && m.tracker != nil {
				m.tracker.AddTextSegment(ev.Phase+"\n", m.width)
			}

		case ui.StreamEventModelSwitch:
			if m.stats != nil {
				m.stats.SetModel(ev.Text)
			}
			if m.markPendingStreamModelSwitchApplied(ev.Text) {
				m.bumpContentVersion()
			}

		case ui.StreamEventRetry:
			if m.stats != nil {
				m.stats.ScheduleRetryStart(ev.RetryWait)
			}
			m.setRetryStatus(ev.RetryStatus("Retrying stream", 1, "..."))

		case ui.StreamEventImage:
			m.setRetryStatus("")
			// Add image segment for inline display
			if m.tracker != nil && ev.ImagePath != "" {
				m.tracker.AddImageSegment(ev.ImagePath)
				// In alt-screen mode, pre-render now so the Kitty/iTerm/etc. upload
				// can be emitted with tea.Raw from this Update. Rendering again in
				// View() will hit the image cache and reuse the same display cells.
				if m.altScreen {
					_ = m.renderViewportImageArtifact(ev.ImagePath)
				}
				// Flush to scrollback so image appears
				if m.scrollOffset == 0 {
					if cmd := m.maybeFlushToScrollback(); cmd != nil {
						flushCmds = append(flushCmds, cmd)
					}
				}
			}

		case ui.StreamEventDiff:
			m.setRetryStatus("")
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
			m.setRetryStatus("")
			// User interjected a message mid-stream (injected between tool turns).
			matchedPending := false
			switch {
			case ev.InterjectionID != "":
				// FIFO interjection events may arrive for an older queue item while a newer
				// item is still pending, so remove by ID across the whole stack rather than
				// comparing only with the latest pending row.
				matchedPending = m.removePendingInterjectionByID(ev.InterjectionID)
			case strings.TrimSpace(ev.Text) != "":
				// Legacy/no-ID fallback: remove the first same-text pending item.
				for i := range m.pendingInterjections {
					if strings.TrimSpace(m.pendingInterjections[i].Text) == strings.TrimSpace(ev.Text) {
						matchedPending = true
						copy(m.pendingInterjections[i:], m.pendingInterjections[i+1:])
						m.pendingInterjections = m.pendingInterjections[:len(m.pendingInterjections)-1]
						m.syncLatestPendingInterjection()
						break
					}
				}
				if !matchedPending && m.pendingInterjectionID == "" && strings.TrimSpace(ev.Text) == strings.TrimSpace(m.pendingInterjection) {
					matchedPending = true
					m.clearPendingInterjectionState()
				}
			}
			_ = matchedPending
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
			// Once the interjection is visibly injected into the transcript, force the
			// viewport to the bottom so the user sees where it landed.
			m.scrollToBottom = true
			// Persist interjected message to session store, preserving structured parts.
			if m.store != nil {
				msg := ev.Message
				if len(msg.Parts) == 0 {
					msg = llm.UserText(ev.Text)
				}
				msg.Role = llm.RoleUser
				userMsg := session.NewMessage(m.sess.ID, msg, -1)
				if userMsg.TextContent == "" {
					userMsg.TextContent = ev.Text
				}
				_ = m.store.AddMessage(context.Background(), m.sess.ID, userMsg)
			}

		case ui.StreamEventDone:
			m.setRetryStatus("")
			m.currentTokens = ev.Tokens
			m.resetAttemptUsage()

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
			m.releaseStreamCancelFunc()
			m.setStreamCancelRequested(false)

			// Flag to scroll to bottom after response completes (alt screen mode)
			if m.altScreen {
				m.scrollToBottom = true
			}

			// Clear callbacks
			m.clearStreamCallbacks()

			// Mark all text segments as complete and render
			m.commitCurrentReasoningToStream()
			if m.tracker != nil {
				m.tracker.CompleteTextSegments(func(text string) string {
					return m.renderMarkdown(text)
				})

				if m.altScreen {
					// The stream is finished, so no tool should still be pending.
					// Force any stragglers complete before rendering: the tracker
					// is now retained after done (for reasoning click-toggling),
					// and CompletedSegments() truncates at the first pending tool,
					// so a stale pending tool would otherwise drop trailing content
					// both here and in later rerenderCompletedStreamFromTracker calls.
					m.tracker.ForceCompletePendingTools()
					// In alt screen mode, save the full rendered content to completedStream.
					// This preserves the correct position of images/diffs relative to text.
					// The last assistant message will be skipped in renderHistory() to avoid duplication.
					completed := m.tracker.CompletedSegments()
					m.viewCache.completedStream = ui.RenderSegmentsWithImageRenderer(completed, m.width, -1, m.renderMd, true, m.toolsExpanded, m.imageArtifactRenderer())
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
					part := llm.Part{Type: llm.PartText, Text: responseContent}
					if reasoningContent, reasoningKind, reasoningTitle := m.currentReasoningPartMetadata(); reasoningContent != "" {
						part.ReasoningContent = reasoningContent
						part.ReasoningKind = reasoningKind
						part.ReasoningSummaryTitle = reasoningTitle
					}
					assistantMsg := session.Message{
						SessionID:   m.sess.ID,
						Role:        llm.RoleAssistant,
						Parts:       []llm.Part{part},
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
			m.resetCurrentReasoning()
			m.currentTokens = 0
			m.webSearchUsed = false
			m.setRetryStatus("")
			// Keep the tracker's completed segments alive after the stream ends.
			// In alt-screen mode the finished turn is shown from completedStream,
			// but the tracker still backs reasoning-header click toggling (via
			// rerenderCompletedStreamFromTracker). The tracker is reset when the
			// next assistant turn starts (sendMessage), so leaving it populated
			// here is render-neutral while preserving click metadata.
			if !m.altScreen {
				m.resetTracker()
			}
			if m.smoothBuffer != nil {
				m.smoothBuffer.Reset()
			}
			m.newlineCompactor = nil
			m.smoothTickPending = false
			// This resets local scheduling state for the next stream.
			// An already in-flight render tick may still arrive and no-op.
			m.streamRenderTickPending = false
			if m.streamPerf != nil {
				m.streamPerf.EmitSummaryIfActive(time.Now())
			}

			if cmd := m.applyPendingStreamModelSwitch(); cmd != nil {
				cmds = append(cmds, cmd)
			}

			// Auto-save session
			cmds = append(cmds, m.saveSessionCmd())

			// Try to rename random-word handover files to descriptive slugs
			if cmd := m.maybeRenameHandoverCmd(); cmd != nil {
				cmds = append(cmds, cmd)
			}

			// Generate a short task title for session browsers and live terminal titles.
			if cmd := m.maybeGenerateSessionTitleCmd(); cmd != nil {
				cmds = append(cmds, cmd)
			}

			m.appendTerminalTitleCmd(&cmds)

			// In auto-send mode, check if there are more messages to send
			if m.autoSendQueue != nil {
				var messageStatsCmd tea.Cmd
				if summary := m.autoSendMessageStats(); summary != "" {
					// tea.Println writes above the managed program output without
					// bypassing Bubble Tea's renderer.
					messageStatsCmd = tea.Println(summary)
				}

				if len(m.autoSendQueue) > 0 {
					// Pop next message and send it
					m.textarea.SetValue(m.autoSendQueue[0])
					m.autoSendQueue = m.autoSendQueue[1:]
					m.updateTextareaHeight()
					model, sendCmd := m.sendMessage(m.textarea.Value())
					if messageStatsCmd != nil {
						return model, tea.Sequence(messageStatsCmd, sendCmd)
					}
					return model, sendCmd
				}

				if m.autoSendExitOnDone {
					// Queue exhausted and exit requested, quit
					m.quitting = true
					if summary := m.exitStatsSummary(); summary != "" {
						return m, m.quitCmd(messageStatsCmd, tea.Println(summary))
					}
					return m, m.quitCmd(messageStatsCmd)
				}

				// Queue exhausted, continue in interactive mode
				if messageStatsCmd != nil {
					cmds = append(cmds, messageStatsCmd)
				}
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
		// Keep draining the provider stream immediately even when text rendering is
		// frame-paced, so bursty deltas don't back up behind the bounded adapter channel.
		if ev.Type != ui.StreamEventDone && ev.Type != ui.StreamEventError {
			cmds = append(cmds, m.listenForStreamEvents())
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
			m.resetTitleGenerationStateForSession()
			m.olderScrollbackLoaded = true
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
				cmds := []tea.Cmd{m.spinner.Tick}
				m.appendTerminalTitleCmd(&cmds)
				return m, ui.ComposeFlushFirstCommands([]tea.Cmd{cmd}, cmds)
			}
		}

		return m, m.withTerminalTitleCmd(m.spinner.Tick)

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
			return m, m.terminalTitleCmd()
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
			return m, m.terminalTitleCmd()
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

	if cmd := m.drainPendingImageUploadCmd(); cmd != nil {
		flushCmds = append(flushCmds, cmd)
	}

	m.appendTerminalTitleCmd(&cmds)

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
