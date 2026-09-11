package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gitcommit"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/mentions"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	render "github.com/samsaffron/term-llm/internal/render/chat"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/runboundary"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/sessiontitle"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/subagentview"
	"github.com/samsaffron/term-llm/internal/termimage"
	"github.com/samsaffron/term-llm/internal/tooldiscovery"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/inspector"
	sessionsui "github.com/samsaffron/term-llm/internal/tui/sessions"
	worktreesui "github.com/samsaffron/term-llm/internal/tui/worktrees"
	"github.com/samsaffron/term-llm/internal/ui"
	"golang.org/x/term"
)

// Model is the main chat TUI model
type pendingSteeringUI struct {
	ID   string
	Text string
}

type pendingStreamModelSwitch struct {
	provider string
	model    string
	applied  bool
}

type conversationBranchPoint struct {
	sourceSessionID     string
	anchorMessageID     int64
	expected            session.TranscriptMutationState
	idempotencyKey      string
	prefill             string
	sourceMessages      []llm.Message
	laterMessageCount   int
	sourceRole          llm.Role
	sourceMessageNumber int
	sourcePreview       string
	autoSend            string
	skipExpectedState   bool
}

// BranchPathNotesRequest carries an abandoned-path suffix across the short TUI
// relaunch used to enter a newly-created child session. Generation deliberately
// starts in the child so navigation is immediate and uses a fresh provider.
type BranchPathNotesRequest struct {
	ChildSessionID  string
	SourceSessionID string
	AnchorMessageID int64
	SourceMessages  []llm.Message
	Focus           string
}

type pendingBranchSend struct {
	content       string
	composer      composerSnapshot
	files         []FileAttachment
	images        []ImageAttachment
	selectedImage int
	pasteChunks   map[int]string
}

// BackgroundRunsMsg reports process-scoped main runs to the attached model.
type BackgroundRunsMsg struct {
	Count int

	// owner scopes the periodic status tick to the model that scheduled it, so
	// a residual tick from a model replaced by an in-process session switch
	// cannot start a second permanent tick loop. A zero owner is a broadcast.
	owner *Model
}

type mainRunSubscriberClosedMsg struct {
	sessionID    string
	runID        string
	subscription uint64
}

type mainRunStartedMsg struct {
	sessionID  string
	runID      string
	generation uint64
}

type mainRunUIEnvelope struct {
	sessionID string
	sinkID    uint64
	message   tea.Msg
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
	draftShellMode  bool
	draftFiles      []FileAttachment
	draftImages     []ImageAttachment
	draftPastes     map[int]string
	recalledText    string
}

type RuntimeSystemContext struct {
	SystemPrompt string
	ApplySkills  func(engine *llm.Engine, toolMgr *tools.ToolManager)
	Skills       *skills.Setup
}

type Model struct {
	reloadContinuation *llm.Continuation
	reloadEnabled      bool
	autoSendPending    bool
	// Dimensions
	width  int
	height int

	// Components
	textarea textarea.Model
	spinner  spinner.Model
	styles   *ui.Styles
	keyMap   KeyMap

	// Session state
	store    session.Store     // Session storage backend
	sess     *session.Session  // Current session
	messages []session.Message // In-memory messages for current session
	// pendingTerminalDirectory is emitted as OSC 7 after a successful runtime
	// directory change, keeping terminal workspace metadata in sync without a
	// process-wide chdir.
	pendingTerminalDirectory string
	compactionIdx            int // Prefix length to skip for LLM context; 0 means no prefix is skipped.
	// olderScrollbackLoaded is false when a compacted resume initially loaded only
	// the active tail; scrolling upward can hydrate the older display prefix once.
	olderScrollbackLoaded      bool
	messagesMu                 sync.Mutex // Protects messages read by the compaction callback.
	compactionApplyMu          sync.Mutex
	pendingCompactionApplies   []compactionAppliedMsg
	streaming                  bool
	transcriptMutationInFlight bool
	shareInFlight              bool
	pendingShare               *shareRequest
	phase                      string // "Thinking", "Searching", "Reading", "Responding"

	// Reasoning display/status state. Provider replay metadata is persisted in
	// assistant parts by the LLM engine; these fields only affect live UI policy.
	reasoningConfig        config.ReasoningConfig
	reasoningModeOverride  string
	currentReasoning       strings.Builder
	currentReasoningItemID string
	currentReasoningKind   llm.ReasoningKind
	currentReasoningTitle  string
	// Per-block expansion override for the live (uncommitted) reasoning
	// block; carried onto the segment when it is committed to the tracker.
	currentReasoningExpanded *bool
	committedReasoning       []llm.Part
	reasoningPhaseActive     bool
	reasoningRawWarned       bool

	// Streaming state
	currentResponse strings.Builder
	currentTokens   int
	// streamStartTime is the true run clock used by persistence and telemetry;
	// streamElapsedOffset contributes only to visible elapsed time.
	streamStartTime             time.Time
	streamElapsedOffset         time.Duration
	webSearchUsed               bool
	retryStatus                 string
	streamCancelFunc            context.CancelFunc  // explicit user Stop, including a pending handoff
	streamCleanupFunc           context.CancelFunc  // release only this stream's local resources
	streamDone                  <-chan struct{}     // closed when the engine goroutine exits
	streamGeneration            uint64              // increments for each stream; used to ignore stale listener messages
	streamCancelRequested       *atomic.Bool        // user requested stream cancellation; wait for stream exit before final cleanup
	tracker                     *ui.ToolTracker     // Tool and segment tracking (shared component)
	subagentTracker             *ui.SubagentTracker // Live subagent progress tracking
	persistedSubagents          map[string]subagentview.CompletedRun
	mediaByReference            map[string]llm.MediaArtifact
	persistedSubagentGeneration uint64

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
	pausedForExternalUI   bool // True when paused for ask_user or approval prompts
	externalProcessActive bool // True while Bubble Tea is handing the terminal to /shell

	// Direct shell mode (`! command`) streams a user-invoked process inside the
	// managed TUI. While eligible, the textarea stores only the command body; the
	// red `! ` prompt owns the activation marker. Durable history and submitted
	// shell turns retain canonical leading-bang syntax.
	directShellEligible bool
	directShellRun      *directShellRun
	directShellGen      uint64

	approvalMgr              *tools.ApprovalManager
	requestedApprovalMode    tools.ApprovalMode
	requestedApprovalChanged bool
	toolMgr                  *tools.ToolManager

	// Embedded inline approval UI (alt screen mode only)
	approvalModel       *tools.ApprovalModel
	approvalDoneCh      chan<- tools.ApprovalResult
	approvalIsWorkspace bool

	// Embedded inline ask_user UI (alt screen mode only)
	askUserModel  *tools.AskUserModel
	askUserDoneCh chan<- []tools.AskUserAnswer

	// LLM context
	rootCtx                    context.Context
	provider                   llm.Provider
	fastProvider               llm.Provider
	sideProviderFactory        func(providerKey, model string) (llm.Provider, error)
	sideQuestion               SideQuestionState
	engine                     *llm.Engine
	agentMentionEngine         atomic.Pointer[llm.Engine]
	runner                     runpkg.Runner
	childRunner                runpkg.ChildRunner
	commit                     *CommitState
	commitMutationCoordinator  gitcommit.MutationCoordinator
	skillRuns                  map[string]*skillRunState
	skillRunSeq                uint64
	pendingSkillResults        []skillRunDoneMsg
	queuedMainSkillActivations []queuedMainSkillActivation
	config                     *config.Config
	providerName               string
	providerKey                string
	modelName                  string
	agentName                  string

	platformDeveloperMessage string
	currentOrigin            session.SessionOrigin

	// Agent handover
	agentResolver                func(name string, cfg *config.Config) (*agents.Agent, error)
	agentLister                  func(cfg *config.Config) ([]string, error) // Lists available agent names
	handoverSystemPromptResolver func(agent *agents.Agent, providerKey, modelName string) (string, error)
	runtimeSystemContextResolver func(agent *agents.Agent, providerKey, modelName, dir string) (RuntimeSystemContext, error)
	runtimeSystemContext         RuntimeSystemContext
	skillsSetup                  *skills.Setup
	skillFilterRestoreTools      []string
	skillFilterRestorePresent    bool
	skillFilterPending           bool
	skillDynamicToolNames        []string
	skillDynamicEnginePrevious   map[string]llm.Tool
	skillDynamicRegistryPrevious map[string]llm.Tool
	sessionInputsObserver        func(*session.Session, string, string)
	systemPromptOverridden       bool
	systemPromptOverride         string
	guardianReviewerRefresh      func(providerKey, modelName string) error
	pendingHandover              *handoverDoneMsg       // Non-nil while awaiting confirmation
	handoverPreview              *handoverPreviewModel  // Inline confirmation UI (alt screen)
	currentAgent                 *agents.Agent          // Current agent config (for enable_handover)
	handoverApprovalMgr          *tools.ApprovalManager // Shell approval flow for handover scripts
	handoverToolDoneCh           chan<- bool            // Signal back to initiate_handover tool

	// Pending message context
	files         []FileAttachment // Attached files for next message
	images        []ImageAttachment
	selectedImage int            // -1 means no image chip selected
	pasteChunks   map[int]string // Collapsed paste placeholders → actual content
	pasteSeq      int            // Incrementing ID for paste placeholders

	// Project @ autocomplete. The index and queries are background/cancellable;
	// selections remain ordinary text and are resolved only when submitted.
	mentionEnabled         bool
	agentMentionEnabled    bool
	mentionRoot            string
	mentionIndex           *mentions.Snapshot
	mentionIndexGeneration uint64
	mentionBuildCancel     context.CancelFunc
	mentionQueryRequest    uint64
	mentionQueryCtx        context.Context
	mentionQueryCancel     context.CancelFunc
	mentionPopup           mentionPopupModel
	agentMentionCapability AgentMentionCapability

	searchEnabled           bool // Web search toggle
	fastMode                bool // Effective ChatGPT/OpenAI fast service-tier state shown in the footer
	fastProviderDefault     bool // Provider config requests fast by default; inherited unless overridden in-session
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
	pendingSteeringText     string   // Interrupt text waiting to be injected or cancelled (latest, for compatibility)
	steeringHandoff         string
	pendingSteeringID       string // Stable ID for the latest displayed pending steering
	pendingSteering         []pendingSteeringUI
	selectedSteering        int       // Selected pending steering; -1 means none
	steeringSeq             uint64    // Monotonic sequence for locally generated steering IDs
	steeringNonce           string    // Per-process entropy keeping those IDs unique across resumes
	interruptNotice         string    // One-line UI notice for recent interrupt actions
	ctrlCExitArmedUntil     time.Time // Second Ctrl+C before this time exits the TUI
	promptHistory           promptHistoryState
	promptHistoryLookupSeq  uint64
	// MCP (Model Context Protocol)
	mcpManager       *mcp.Manager
	discoveryPlanner *tooldiscovery.Planner
	mcpStatusChan    chan mcp.StatusUpdate
	maxTurns         int

	// Directory approval
	approvedDirs    *ApprovedDirs
	pendingFilePath string // File waiting for directory approval

	// History scroll
	scrollOffset int
	viewportRows int

	// UI state
	quitting           bool
	quitAfterSkillRuns bool
	err                error
	yolo               bool

	// reloadRequested signals the caller to re-exec the binary (e.g. after an upgrade).
	// The session ID to resume is stored in reloadSessionID.
	reloadRequested bool
	reloadSessionID string

	// Dialog components
	completions *CompletionsModel
	dialog      *DialogModel

	// Inline mode state
	program                    *tea.Program // Reference to program for tea.Println
	inlineCompletionPending    bool
	inlineCompletionPendingKey *tea.KeyPressMsg

	// If set, the caller should relaunch chat with this session ID.
	pendingResumeSessionID  string
	pendingBranchPrefill    string
	pendingBranchPathNotes  *BranchPathNotesRequest
	pendingBranchAutoSend   string
	branchTreeChoices       map[string]conversationBranchPoint
	pendingBranchPoint      *conversationBranchPoint
	runtimeOperations       operationGate
	branchOperationCancel   context.CancelFunc
	branchOperationStarted  time.Time
	branchPathNotesRequest  *BranchPathNotesRequest
	queuedBranchSend        *pendingBranchSend
	pendingBranchNavigation *SessionSwitchRequest
	transitionAutoSendDraft *pendingBranchSend
	branchAutoSend          string
	activeBranchAnchorID    int64
	runBoundary             *runboundary.Tracker

	mainRunManager      *MainRunManager
	mainRunLive         <-chan MainRunEvent
	mainRunCoalescer    *mainRunEventCoalescer
	mainRunReplay       []MainRunEvent
	mainRunDetach       func()
	mainRunUIDetach     func()
	mainRunID           string
	mainRunSubscription uint64
	mainRunLastSeq      uint64
	mainRunViewComplete bool
	backgroundRunCount  int

	// In-process session switching (nil switcher falls back to quit+relaunch).
	sessionSwitcher      SessionSwitcher
	sessionSwitchPending bool
	sessionTransition    *sessionTransition

	// If set, the caller should auto-send this message after handover restart.
	pendingHandoverAutoSend string

	// If set, auto-send this message on Init (used after handover restart).
	handoverAutoSend string
	// branchPrefill restores an edited/follow-up draft after a branch relaunch
	// without submitting it to the model.
	branchPrefill string

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
	titleMode          TerminalTitleMode
	titleFormat        string
	conversationBranch bool
	titleFormatter     *terminalTitleFormatter
	titleManager       *terminalTitleManager
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
	altScreen                    bool
	mouseMode                    bool
	stickyUserPromptHovered      bool
	stickyUserPromptHoverAction  stickyUserPromptHoverAction
	userPromptMouseKnown         bool
	userPromptMouseX             int
	userPromptMouseY             int
	hoveredUserPromptAnchorIndex int
	hoveredUserPromptRow         int
	hoveredUserPromptSticky      bool
	viewport                     viewport.Model // Scrollable viewport for alt screen mode
	scrollToBottom               bool           // Flag to scroll to bottom after response completes
	streamRenderMinInterval      time.Duration
	// Debounced alt-screen resize reflow state.
	resizeReflowPending        bool
	resizeReflowWasAtBottom    bool
	resizeReflowScrollFraction float64
	resizeReflowRestoreAnchor  bool
	resizeReflowHadImages      bool
	resizeReflowGeneration     uint64

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
		userMessageAnchors     []render.UserMessageAnchor
	}

	// New chat renderer (virtualized rendering for large histories)
	chatRenderer *render.Renderer

	// Auto-send mode (for benchmarking) - queue of messages to send
	autoSendQueue []string
	// startupWorkspaceApproval runs once after the event loop starts and before
	// any startup auto-send so the user can decide workspace trust up front.
	startupWorkspaceApproval func() error
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

	// Alt-screen terminal image rendering. Upload/control bytes are attached to
	// the same tea.View.PostFrame as the viewport which composed them; viewport
	// content contains only captions plus line-clipping-safe display cells.
	// pendingImageUploads retains non-direct protocol bytes until the exact View
	// carrying them is acknowledged. Direct Kitty uploads and placements diff
	// against postFrameUploadedImages/postFrameKnownImages only after success.
	pendingImageUploads        []string
	pendingImageUploadKeys     map[string]struct{}
	pendingImagePlaceKeys      map[string]struct{}
	ownedKittyImageIDs         map[uint32]struct{}
	imageCleanupSeq            string
	imageCleanupSeqValid       bool
	imageGeneration            uint64
	imageCleanupQueued         bool
	viewportImageArtifacts     map[string]viewportImageArtifact
	viewportImageBlocks        []viewportImageBlock
	postFrameImageSeq          string
	postFrameImageUploadSeq    string
	postFrameImagePlaceSeq     string
	postFrameImagePrefixSeq    string
	postFrameImageMu           sync.Mutex
	postFrameCurrentImages     map[string]postFrameImageState
	postFrameLastImages        map[string]postFrameImageState
	postFrameKnownImages       map[string]postFrameImageState
	postFrameUploadedImages    map[uint32]struct{}
	postFrameRenderCache       map[string]postFrameImageState
	postFrameReceipt           *postFrameImageReceipt
	postFrameRetryDisabled     bool
	postFrameFailureGeneration uint64

	// Text selection state (alt-screen only)
	selection               Selection
	contentLines            []string // full viewport content split by \n
	copyStatus              string   // transient status message after copy attempt
	copyStatusSeq           uint64   // monotonically increasing copy-status timer token
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
	if m == nil {
		return
	}
	// Terminal delivery is cleanup, not a user Stop. A managed source may
	// finish while Rush already owns the session or has started its successor.
	if m.streamCleanupFunc != nil {
		m.streamCleanupFunc()
	} else if m.streamCancelFunc != nil {
		// Legacy/model-owned streams have only a local cancellation callback.
		m.streamCancelFunc()
	}
	m.streamCleanupFunc = nil
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
		event               ui.StreamEvent
		generation          uint64
		mainRunID           string
		mainRunSubscription uint64
		mainRunSeq          uint64
	}
	streamCancelTimeoutMsg struct {
		done       <-chan struct{}
		generation uint64
	}
	startupWorkspaceApprovalMsg struct{ err error }
	sessionSavedMsg             struct{}
	sessionLoadedMsg            struct {
		sess     *session.Session
		messages []session.Message
	}
	tickMsg               time.Time
	streamRenderTickMsg   struct{}
	footerMessageClearMsg struct {
		Seq uint64
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
	mcpStatusUpdateMsg      struct{ update mcp.StatusUpdate }
	GuardianReviewMsg       struct{ Event tools.GuardianEvent }
	postFrameImageResultMsg struct {
		Receipt *postFrameImageReceipt
		Err     error
	}
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

// FooterNoticeMsg surfaces a background warning in the footer. Subsystems that
// would otherwise write to stderr must route through this: the TUI owns the
// alt screen, so a direct write lands wherever the cursor sits and corrupts the
// frame. Tone defaults to "error" when empty.
type FooterNoticeMsg struct {
	Text string
	Tone string
}

// autoSendMsg triggers automatic message send (for benchmarking mode)
type autoSendMsg struct{}

// inlineCompletionRenderedMsg acknowledges that Bubble Tea has inserted the
// completed assistant turn (including its spacer) into inline scrollback.
type inlineCompletionRenderedMsg struct{}

type chatGPTModelsLoadedMsg struct {
	models []llm.ModelInfo
	fresh  bool
	err    error
}

// ApprovalRequestMsg triggers an inline approval prompt.
type ApprovalRequestMsg struct {
	Path        string
	IsWrite     bool
	IsShell     bool
	IsWorkspace bool
	WorkDir     string // directory where a shell command will execute (may be empty)
	DoneCh      chan<- tools.ApprovalResult
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

func (m *Model) applyLoadedScrollback(messages []session.Message, compactionIdx int) {
	if m == nil {
		return
	}
	m.messagesMu.Lock()
	m.messages = messages
	m.compactionIdx = compactionIdx
	m.messagesMu.Unlock()
	m.olderScrollbackLoaded = true
	m.invalidateHistoryCache()
}

func (m *Model) loadOlderScrollbackPrefix(ctx context.Context) tea.Cmd {
	if m == nil || m.store == nil || m.sess == nil || m.olderScrollbackLoaded || !session.HasCompactionBoundary(m.sess) {
		return nil
	}
	// Streaming turns keep in-flight assistant state in m.messages while store
	// callbacks are still assigning IDs/updating rows. Do not replace that slice
	// with a persisted snapshot mid-stream; hydrate older display history after the
	// stream completes or on a later scroll.
	if m.streaming {
		return nil
	}
	loadedMsgs, compactionIdx, err := loadSessionMessagesForScrollback(ctx, m.store, m.sess)
	if err != nil || len(loadedMsgs) == 0 || compactionIdx <= 0 {
		m.olderScrollbackLoaded = true
		return nil
	}
	m.applyLoadedScrollback(loadedMsgs, compactionIdx)
	return m.loadPersistedSubagentsCmd()
}

// New creates a new chat model.
// fast-provider aware callers should use NewWithFastProvider.
func New(cfg *config.Config, provider llm.Provider, engine *llm.Engine, providerKey string, modelName string, mcpManager *mcp.Manager, maxTurns int, forceExternalSearch bool, disableExternalWebFetch bool, searchEnabled bool, localTools []string, toolsStr string, mcpStr string, showStats bool, initialText string, store session.Store, sess *session.Session, altScreen bool, autoSendQueue []string, autoSendExitOnDone bool, textMode bool, agentName string, platformDeveloperMessage string, yolo bool, toolMgrs ...*tools.ToolManager) *Model {
	return NewWithFastProvider(cfg, provider, nil, engine, providerKey, modelName, mcpManager, maxTurns, forceExternalSearch, disableExternalWebFetch, searchEnabled, localTools, toolsStr, mcpStr, showStats, initialText, store, sess, altScreen, autoSendQueue, autoSendExitOnDone, textMode, agentName, platformDeveloperMessage, yolo, toolMgrs...)
}

// NewWithFastProvider creates a new chat model with an optional fast provider
// for control-plane classification tasks. Callers that distinguish requested
// policy from runtime fallback should use NewWithFastProviderAndApproval.
func NewWithFastProvider(cfg *config.Config, provider llm.Provider, fastProvider llm.Provider, engine *llm.Engine, providerKey string, modelName string, mcpManager *mcp.Manager, maxTurns int, forceExternalSearch bool, disableExternalWebFetch bool, searchEnabled bool, localTools []string, toolsStr string, mcpStr string, showStats bool, initialText string, store session.Store, sess *session.Session, altScreen bool, autoSendQueue []string, autoSendExitOnDone bool, textMode bool, agentName string, platformDeveloperMessage string, yolo bool, toolMgrs ...*tools.ToolManager) *Model {
	requested := tools.ModePrompt
	if yolo {
		requested = tools.ModeYolo
	} else if len(toolMgrs) > 0 && toolMgrs[0] != nil && toolMgrs[0].ApprovalMgr != nil {
		requested = toolMgrs[0].ApprovalMgr.ApprovalMode()
	}
	return NewWithFastProviderAndApproval(cfg, provider, fastProvider, engine, providerKey, modelName, mcpManager, maxTurns, forceExternalSearch, disableExternalWebFetch, searchEnabled, localTools, toolsStr, mcpStr, showStats, initialText, store, sess, altScreen, autoSendQueue, autoSendExitOnDone, textMode, agentName, platformDeveloperMessage, yolo, requested, toolMgrs...)
}

// NewWithFastProviderAndApproval preserves requested policy independently from
// the approval manager's actual mode after an interactive Guardian fallback.
func NewWithFastProviderAndApproval(cfg *config.Config, provider llm.Provider, fastProvider llm.Provider, engine *llm.Engine, providerKey string, modelName string, mcpManager *mcp.Manager, maxTurns int, forceExternalSearch bool, disableExternalWebFetch bool, searchEnabled bool, localTools []string, toolsStr string, mcpStr string, showStats bool, initialText string, store session.Store, sess *session.Session, altScreen bool, autoSendQueue []string, autoSendExitOnDone bool, textMode bool, agentName string, platformDeveloperMessage string, yolo bool, requestedApprovalMode tools.ApprovalMode, toolMgrs ...*tools.ToolManager) *Model {
	var discoveryPlanner *tooldiscovery.Planner
	if cfg != nil && engine != nil && mcpManager != nil {
		var err error
		discoveryPlanner, err = tooldiscovery.NewPlanner(cfg.ToolDiscovery, mcpManager, engine)
		if err != nil {
			slog.Warn("failed to configure MCP tool discovery", "error", err)
		}
	}
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
	ta.Placeholder = "Type a message…"
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

	// Prefill with initial text if provided.
	initialBody, initialShellComposer := directShellComposerBody(initialText)
	if initialBody != "" {
		ta.SetValue(initialBody)
	}

	// Use provided session or create a new one
	newSession := sess == nil
	if newSession {
		sess = &session.Session{
			ID:           session.NewID(),
			Provider:     provider.Name(),
			ProviderKey:  providerKey,
			Model:        modelName,
			Mode:         session.ModeChat,
			Origin:       session.OriginTUI,
			Agent:        agentName,
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
			Search:       searchEnabled,
			Tools:        toolsStr,
			MCP:          mcpStr,
			ApprovalMode: sessionApprovalModeFromTools(requestedApprovalMode),
		}
		// Get current working directory
		if cwd, err := os.Getwd(); err == nil {
			sess.CWD = cwd
		}
		// Persist new session and infer its registered project from the CWD.
		persistNewTUISession(context.Background(), store, sess)
	}

	// Load existing messages if resuming.
	// Keep full persisted scrollback available for the human UI, but remember the
	// compaction boundary so buildMessages only sends the active post-compaction
	// window to the LLM.
	var messages []session.Message
	var compactionIdx int
	if !newSession && store != nil && sess.ID != "" {
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
	} else if providerType == config.ProviderTypeOpenCodeGo {
		if cached, fresh, err := llm.CachedOpenCodeGoModels(); err == nil {
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
		width:                    width,
		height:                   height,
		textarea:                 ta,
		spinner:                  s,
		styles:                   styles,
		keyMap:                   DefaultKeyMap(),
		store:                    store,
		sess:                     sess,
		messages:                 messages,
		compactionIdx:            compactionIdx,
		olderScrollbackLoaded:    !session.HasCompactionBoundary(sess) || compactionIdx > 0,
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
		requestedApprovalMode:    requestedApprovalMode,
		phase:                    "Thinking",
		reasoningConfig:          reasoningCfg,
		viewportRows:             ui.RemainingLines(height, 8), // Reserve space for input and status
		tracker:                  tracker,
		toolMgr:                  toolMgr,
		subagentTracker:          subagentTracker,
		mainRunViewComplete:      true,
		smoothBuffer:             ui.NewSmoothBuffer(),
		completions:              completions,
		dialog:                   dialog,
		approvedDirs:             approvedDirs,
		mcpManager:               mcpManager,
		discoveryPlanner:         discoveryPlanner,
		mcpStatusChan:            mcpStatusChan,
		maxTurns:                 maxTurns,
		forceExternalSearch:      forceExternalSearch,
		disableExternalWebFetch:  disableExternalWebFetch,
		searchEnabled:            searchEnabled,
		fastMode:                 fastMode,
		fastProviderDefault:      fastProviderDefault,
		fastMetadataLoaded:       fastMetadataLoaded,
		fastMetadataStale:        fastMetadataStale,
		modelMetadata:            modelMetadata,
		localTools:               localTools,
		toolsStr:                 toolsStr,
		mcpStr:                   mcpStr,
		showStats:                showStats,
		stats:                    stats,
		streamPerf:               newStreamPerfTelemetryFromEnv(),
		titleMode:                titleMode,
		directShellEligible:      initialShellComposer,
		titleFormat:              cfg.Chat.TerminalTitleFormat,
		titleFormatter:           newTerminalTitleFormatter(cfg.Chat.TerminalTitleFormat, TerminalTitleEnvironment{}),
		titleManager:             newTerminalTitleManager(titleMode, TerminalTitleEnvironment{}),
		streamCancelRequested:    &atomic.Bool{},
		altScreen:                altScreen,
		mouseMode:                chatMouseModeFromEnv(),
		viewport:                 vp,
		streamRenderMinInterval:  chatRenderMinIntervalFromEnv(),
		chatRenderer:             chatRenderer,
		autoSendQueue:            autoSendQueue,
		autoSendExitOnDone:       autoSendExitOnDone,
		textMode:                 textMode,
		pendingImageUploadKeys:   make(map[string]struct{}),
		pendingImagePlaceKeys:    make(map[string]struct{}),
		ownedKittyImageIDs:       make(map[uint32]struct{}),
		postFrameLastImages:      make(map[string]postFrameImageState),
		postFrameKnownImages:     make(map[string]postFrameImageState),
		postFrameUploadedImages:  make(map[uint32]struct{}),
		postFrameRenderCache:     make(map[string]postFrameImageState),
		viewportImageArtifacts:   make(map[string]viewportImageArtifact),
		selectedImage:            -1,
		selectedSteering:         -1,
	}
	model.agentMentionEngine.Store(engine)
	if internalreasoning.RawDisplayBlocked(reasoningCfg) {
		model.SetFooterWarning("Raw reasoning display is disabled. Set reasoning.raw=true or TERM_LLM_SHOW_RAW_REASONING=1 to allow it.")
		model.reasoningRawWarned = true
	}
	model.configureImageRenderer()
	model.configureContextManagementForSession()
	model.initializeMentions()
	return model
}

func sessionMessageForSteering(sessionID, visibleText string, message llm.Message) *session.Message {
	if len(message.Parts) == 0 {
		message = llm.UserText(visibleText)
	}
	message.Role = llm.RoleUser
	userMessage := session.NewMessage(sessionID, message, -1)
	// Steering parts may include provider-only eager file or delegation
	// context. Keep visible consumers clean while retaining full Parts.
	if visibleText != "" {
		userMessage.TextContent = visibleText
	} else if userMessage.TextContent == "" {
		userMessage.TextContent = llm.MessageText(message)
	}
	return userMessage
}

func addSteeringMessage(ctx context.Context, store session.Store, sessionID string, msg *session.Message) error {
	if unlogged, ok := store.(interface {
		AddMessageUnlogged(context.Context, string, *session.Message) error
	}); ok {
		return unlogged.AddMessageUnlogged(ctx, sessionID, msg)
	}
	return store.AddMessage(ctx, sessionID, msg)
}

func reportSteeringAddError(store session.Store, err error) {
	if reporter, ok := store.(interface{ ReportAddMessageError(error) }); ok {
		reporter.ReportAddMessageError(err)
	}
}

// isClientMessageIDConflict reports whether a store write failed because the
// message identity already exists in this session. SQLite does not expose the
// constrained columns through a portable error interface.
func isClientMessageIDConflict(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "client_message_id")
}

// persistSteering preserves identities: matching receipts reconcile, while
// conflicting content is an error, never another invented user turn.
func (m *Model) persistSteering(ctx context.Context, visibleText string, message llm.Message) {
	if m.store == nil || m.sess == nil {
		return
	}
	userMsg := sessionMessageForSteering(m.sess.ID, visibleText, message)
	err := addSteeringMessage(ctx, m.store, m.sess.ID, userMsg)
	if err == nil {
		return
	}
	if isClientMessageIDConflict(err) {
		existing, lookupErr := session.FindMessageByClientMessageID(ctx, m.store, m.sess.ID, userMsg.ClientMessageID)
		if lookupErr == nil && existing != nil {
			a, aErr := existing.PartsJSONForStorage(false)
			b, bErr := userMsg.PartsJSONForStorage(false)
			if aErr == nil && bErr == nil && existing.Role == userMsg.Role && existing.TextContent == userMsg.TextContent && a == b {
				return
			}
			err = session.ErrSteeringConflict
		}
	}
	reportSteeringAddError(m.store, err)
}

func (m *Model) setMCPServerSelected(name string, selected bool) {
	name = strings.TrimSpace(name)
	if m == nil || name == "" {
		return
	}
	servers := make(map[string]bool)
	for _, current := range strings.Split(m.mcpStr, ",") {
		current = strings.TrimSpace(current)
		if current != "" {
			servers[current] = true
		}
	}
	if selected {
		servers[name] = true
	} else {
		delete(servers, name)
	}
	names := make([]string, 0, len(servers))
	for current := range servers {
		names = append(names, current)
	}
	sort.Strings(names)
	m.mcpStr = strings.Join(names, ",")
	if m.sess != nil {
		m.sess.MCP = m.mcpStr
		if m.store != nil {
			_ = m.store.Update(context.Background(), m.sess)
		}
	}
}

func (m *Model) showMCPPicker() {
	if m == nil || m.dialog == nil || m.mcpManager == nil {
		return
	}
	sessionID := ""
	if m.sess != nil {
		sessionID = m.sess.ID
	}
	if m.engine != nil {
		if diagnostics, ok := m.engine.ToolDiscoveryDiagnostics(sessionID); ok {
			m.dialog.ShowMCPPicker(m.mcpManager, diagnostics)
			return
		}
	}
	m.dialog.ShowMCPPicker(m.mcpManager)
}

func (m *Model) refreshMCPPickerIfOpen() {
	if m == nil || m.dialog == nil || m.mcpManager == nil || m.dialog.Type() != DialogMCPPicker {
		return
	}
	query := m.dialog.Query()
	cursor := m.dialog.Cursor()
	m.showMCPPicker()
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

type resizeReflowMsg struct {
	generation uint64
}

const resizeReflowDebounce = 75 * time.Millisecond

func (m *Model) beginAltScreenResizeReflow() tea.Cmd {
	if !m.altScreen || len(m.messages) == 0 {
		m.resizeReflowPending = false
		m.resizeReflowGeneration++
		return nil
	}
	m.resizeReflowPending = true
	m.resizeReflowGeneration++
	generation := m.resizeReflowGeneration
	return m.presentationTick(resizeReflowDebounce, func(time.Time) tea.Msg {
		return resizeReflowMsg{generation: generation}
	})
}

func (m *Model) captureResizeReflowAnchor() {
	m.resizeReflowWasAtBottom = m.viewport.AtBottom()
	maxYOffset := max(0, m.viewport.TotalLineCount()-m.viewport.Height())
	if maxYOffset == 0 {
		m.resizeReflowScrollFraction = 0
		return
	}
	m.resizeReflowScrollFraction = float64(m.viewport.YOffset()) / float64(maxYOffset)
}

func (m *Model) applyWindowSize(msg tea.WindowSizeMsg) {
	oldWidth := m.width
	widthChanged := oldWidth > 0 && oldWidth != msg.Width
	oldViewportHeight := 0
	if m.altScreen {
		oldViewportHeight = m.viewport.Height()
		hadImages := len(m.viewportImageBlocks) > 0 || len(m.viewportImageArtifacts) > 0 || strings.Contains(m.viewCache.lastViewportView, "\U0010eeee")
		if !m.resizeReflowPending {
			m.captureResizeReflowAnchor()
			m.resizeReflowHadImages = hadImages
		} else if hadImages {
			m.resizeReflowHadImages = true
		}
	}
	m.selection = Selection{}
	m.width = msg.Width
	m.height = msg.Height
	m.viewportRows = ui.RemainingLines(m.height, 8)
	m.textarea.SetWidth(m.width)
	m.updateTextareaHeight()
	m.resizeSideComposer()
	if m.completions != nil {
		m.completions.SetSize(m.width, m.height)
	}
	if m.dialog != nil {
		m.dialog.SetSize(m.width, m.height)
	}

	// Invalidate cached markdown renderings only when their width changes.
	if widthChanged && m.tracker != nil {
		m.tracker.InvalidateRenderCaches(m.width)
	}

	// Completed streaming output and historical markdown are width-dependent.
	// Height-only viewport changes can reuse them without rebuilding all messages.
	if widthChanged {
		m.resetAltScreenStreamingAppendCache()
		if m.viewCache.completedStream != "" {
			m.viewCache.completedStream = ""
			m.invalidateHistoryCache()
		} else {
			m.bumpContentVersion()
		}
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
		if widthChanged || viewportHeightChanged {
			m.imageGeneration++
			m.postFrameImageMu.Lock()
			m.postFrameRetryDisabled = false
			m.postFrameFailureGeneration = 0
			m.postFrameImageMu.Unlock()
			termimage.ClearCache()
			termimage.Debugf(termimage.DefaultEnvironment(), "chat resize width %d->%d viewport_h %d->%d model_h=%d generation=%d: invalidate image viewport render", oldWidth, m.width, oldViewportHeight, m.viewport.Height(), m.height, m.imageGeneration)
			if !widthChanged && viewportHeightChanged && m.resizeReflowHadImages {
				m.invalidateHistoryCache()
			}
			m.viewCache.lastSetContentAt = time.Time{}
			// Keep the last rendered viewport available for the cheap resize frame.
			// Bubble Tea erases the alt screen before delivering WindowSizeMsg; clearing
			// this cache here would leave nothing to draw while a large history reflows.
			m.viewCache.cachedTrackerVersion = 0
			// Preserve cleanup for every replacement-capable post-frame payload in
			// this resize generation. The new renderer frame erases old placement
			// anchors before this cleanup and the fresh upload/placement are written.
			m.queuePostFrameImageCleanupIfActive()
			m.pendingImageUploads = nil
			m.pendingImageUploadKeys = make(map[string]struct{})
			m.pendingImagePlaceKeys = make(map[string]struct{})
			m.imageCleanupQueued = false
			m.viewportImageArtifacts = make(map[string]viewportImageArtifact)
			m.viewportImageBlocks = nil
			m.clearOwnedKittyImageIDs()
			m.postFrameCurrentImages = nil
			m.postFrameLastImages = make(map[string]postFrameImageState)
			m.postFrameKnownImages = make(map[string]postFrameImageState)
			m.postFrameUploadedImages = make(map[uint32]struct{})
			m.postFrameRenderCache = make(map[string]postFrameImageState)
			m.postFrameReceipt = nil
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
	if !m.mainRunViewComplete {
		m.viewCache.completedStream = ""
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

// SetStartupWorkspaceApproval configures a one-time workspace trust decision
// that runs after the event loop starts and before any startup auto-send.
func (m *Model) SetStartupWorkspaceApproval(confirm func() error) {
	m.startupWorkspaceApproval = confirm
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
	m.SetSkillsSetup(current.Skills)
}

// SetSkillsSetup installs the session-bound skill registry used for slash
// discovery and direct activation. The setup is reused across keystrokes; its
// registry cache notices ordinary SKILL.md edits through file fingerprints.
func (m *Model) SetSkillsSetup(setup *skills.Setup) {
	m.skillsSetup = setup
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

// SetChildRunner configures fresh child-agent execution for direct isolated
// skills. It is independent of whether the current agent exposes spawn_agent.
func (m *Model) SetChildRunner(runner runpkg.ChildRunner) {
	m.childRunner = runner
}

// SetProgram gives the model a handle to the running Bubble Tea program for
// model wakeups that originate outside the Update loop.
func (m *Model) SetProgram(p *tea.Program) {
	m.program = p
}

// SetMainRunManager attaches process-scoped execution ownership to this model.
func (m *Model) SetMainRunManager(manager *MainRunManager) {
	m.mainRunManager = manager
}

// AttachMainRunUISink routes session-owned prompts to the currently visible
// Bubble Tea program. Navigation detaches it before the program exits so a
// background run retains later prompts for the next attachment.
func (m *Model) AttachMainRunUISink(sink func(tea.Msg)) func() {
	if m.mainRunManager == nil || sink == nil {
		return func() {}
	}
	if m.mainRunUIDetach != nil {
		m.mainRunUIDetach()
	}
	detach := m.mainRunManager.AttachUISink(m.SessionID(), sink)
	m.mainRunUIDetach = detach
	return func() {
		detach()
		m.mainRunUIDetach = nil
	}
}

// DetachMainRunUISink releases the attached prompt sink, if any. Later prompts
// for this session are retained by the manager until the next attachment.
func (m *Model) DetachMainRunUISink() {
	if m == nil || m.mainRunUIDetach == nil {
		return
	}
	m.mainRunUIDetach()
	m.mainRunUIDetach = nil
}

// SessionID returns the persisted session owned by this visible model.
func (m *Model) SessionID() string {
	if m == nil || m.sess == nil {
		return ""
	}
	return strings.TrimSpace(m.sess.ID)
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

func (m *Model) startupWorkspaceApprovalCmd() tea.Cmd {
	confirm := m.startupWorkspaceApproval
	if confirm == nil {
		return nil
	}
	m.startupWorkspaceApproval = nil
	return func() tea.Msg {
		return startupWorkspaceApprovalMsg{err: confirm()}
	}
}

func (m *Model) initialAutoSendCmd() tea.Cmd {
	// Branch command auto-send: submit only when the command included a message.
	// When path notes are active Init moves this into queuedBranchSend instead.
	if m.branchAutoSend != "" {
		m.textarea.SetValue(m.branchAutoSend)
		m.branchAutoSend = ""
		m.updateTextareaHeight()
		m.autoSendPending = true
		return func() tea.Msg { return autoSendMsg{} }
	}

	// Handover auto-send: send the target agent's default prompt after restart.
	if m.handoverAutoSend != "" {
		m.textarea.SetValue(m.handoverAutoSend)
		m.handoverAutoSend = ""
		m.updateTextareaHeight()
		m.autoSendPending = true
		return func() tea.Msg { return autoSendMsg{} }
	}

	// In auto-send mode, pop first message from queue and send it.
	if len(m.autoSendQueue) > 0 {
		m.textarea.SetValue(m.autoSendQueue[0])
		m.autoSendQueue = m.autoSendQueue[1:]
		m.updateTextareaHeight()
		m.autoSendPending = true
		return func() tea.Msg { return autoSendMsg{} }
	}
	return nil
}

func (m *Model) mainRunStatusCmd() tea.Cmd {
	if m.mainRunManager == nil {
		return nil
	}
	return m.presentationTick(time.Second, func(time.Time) tea.Msg {
		return BackgroundRunsMsg{Count: m.mainRunManager.ActiveCount(), owner: m}
	})
}

// Init initializes the model.
func (m *Model) Init() tea.Cmd {
	// Update textarea height for any initial text
	m.updateTextareaHeight()

	baseCmds := []tea.Cmd{m.passiveCommand(textarea.Blink), m.passiveCommand(m.spinner.Tick)}
	if m.reloadContinuation != nil {
		baseCmds = append(baseCmds, func() tea.Msg { return ReloadResumeMsg{} })
	}
	if (!m.fastMetadataLoaded || m.fastMetadataStale) && !m.fastMetadataLoading {
		if cmd := m.loadModelMetadataCmd(); cmd != nil {
			baseCmds = append(baseCmds, cmd)
		}
	}
	if cmd := m.listenForMCPStatusUpdates(); cmd != nil {
		baseCmds = append(baseCmds, cmd)
	}
	if cmd := m.terminalTitleCmd(); cmd != nil {
		baseCmds = append(baseCmds, cmd)
	}
	if cmd := terminalWorkingDirectoryCmd(m.effectiveWorkingDir()); cmd != nil {
		baseCmds = append(baseCmds, cmd)
	}
	if cmd := m.startMentionIndex(); cmd != nil {
		baseCmds = append(baseCmds, cmd)
	}

	// Set markdown renderer for chat renderer
	if m.chatRenderer != nil {
		m.chatRenderer.SetMarkdownRenderer(m.renderMd)
		m.chatRenderer.SetToolsExpanded(m.toolsExpanded)
	}

	// Branch relaunches restore the user's edited/follow-up draft but deliberately
	// require a fresh Enter confirmation instead of auto-sending it.
	if m.branchPrefill != "" {
		m.textarea.SetValue(m.branchPrefill)
		m.branchPrefill = ""
		m.updateTextareaHeight()
		_, footerCmd := m.showFooterMuted("Selected message restored as draft. Ctrl+U clears it.")
		if footerCmd != nil {
			baseCmds = append(baseCmds, footerCmd)
		}
	}
	if m.branchAutoSend != "" && m.branchPathNotesRequest != nil {
		m.queuedBranchSend = &pendingBranchSend{content: m.branchAutoSend, selectedImage: -1}
		m.branchAutoSend = ""
	}
	if cmd := m.attachMainRun(m.SessionID()); cmd != nil {
		baseCmds = append(baseCmds, cmd)
	}
	if cmd := m.mainRunStatusCmd(); cmd != nil {
		baseCmds = append(baseCmds, cmd)
	}
	if cmd := m.loadPersistedSubagentsCmd(); cmd != nil {
		baseCmds = append(baseCmds, cmd)
	}
	if cmd := m.startPendingBranchPathNotes(); cmd != nil {
		baseCmds = append(baseCmds, cmd)
	}

	if cmd := m.startupWorkspaceApprovalCmd(); cmd != nil {
		baseCmds = append(baseCmds, cmd)
		return tea.Batch(baseCmds...)
	}

	if cmd := m.initialAutoSendCmd(); cmd != nil {
		baseCmds = append(baseCmds, cmd)
	}
	return tea.Batch(baseCmds...)
}

func (m *Model) listenForMCPStatusUpdates() tea.Cmd {
	if m == nil || m.mcpStatusChan == nil {
		return nil
	}
	return m.passiveCommand(func() tea.Msg {
		update, ok := <-m.mcpStatusChan
		if !ok {
			return nil
		}
		return mcpStatusUpdateMsg{update: update}
	})
}

// RequestedResumeSessionID returns a pending session ID to relaunch, if any.
func (m *Model) RequestedResumeSessionID() string {
	return strings.TrimSpace(m.pendingResumeSessionID)
}

// RequestedHandoverAutoSend returns a message to auto-send after handover restart.
func (m *Model) RequestedHandoverAutoSend() string {
	return strings.TrimSpace(m.pendingHandoverAutoSend)
}

// RequestedBranchPrefill returns a draft to restore without auto-submitting it.
func (m *Model) RequestedBranchPrefill() string {
	return m.pendingBranchPrefill
}

// RequestedBranchPathNotes returns helper work that should begin after the new
// child session has relaunched.
func (m *Model) RequestedBranchPathNotes() *BranchPathNotesRequest {
	if m.pendingBranchPathNotes == nil {
		return nil
	}
	request := *m.pendingBranchPathNotes
	request.SourceMessages = append([]llm.Message(nil), request.SourceMessages...)
	return &request
}

// RequestedBranchAutoSend returns the optional first message for a newly
// created thread or fork. Empty commands deliberately produce no send.
func (m *Model) RequestedBranchAutoSend() string {
	return strings.TrimSpace(m.pendingBranchAutoSend)
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

// ApprovalModeRequested returns the policy selected by resolution or a runtime
// user toggle, even if the actual manager temporarily fell back to prompt.
func (m *Model) ApprovalModeRequested() tools.ApprovalMode {
	if m == nil {
		return tools.ModePrompt
	}
	return m.requestedApprovalMode
}

// ApprovalModeChanged reports whether the user changed approval policy during
// this model's lifetime. Initial resolution and temporary runtime fallback do
// not count as user changes.
func (m *Model) ApprovalModeChanged() bool {
	return m != nil && m.requestedApprovalChanged
}

// PersistApprovalMode stores a requested approval policy separately from the
// manager's actual runtime mode, preserving requested auto when guardian
// initialization temporarily falls back to prompt.
func (m *Model) PersistApprovalMode(mode tools.ApprovalMode) {
	m.requestedApprovalMode = mode
	m.persistApprovalMode(mode)
}

// WaitStreamDone waits briefly for engine streaming and seals the operation
// gate. It reports whether every operation drained within the shutdown budget;
// callers must not close runtime resources while it returns false.
func (m *Model) WaitStreamDone() bool {
	return m.waitStreamDone(false)
}

// CancelAndWaitStreamDone also cancels branch work. Final process shutdown uses
// this variant; session switches let owned work finish in the background.
func (m *Model) CancelAndWaitStreamDone() bool {
	return m.waitStreamDone(true)
}

func (m *Model) waitStreamDone(cancelBranch bool) bool {
	if cancelBranch && m.branchOperationCancel != nil {
		m.branchOperationCancel()
	}
	if m.streamDone != nil {
		select {
		case <-m.streamDone:
		case <-time.After(streamCancelMaxWait):
		}
	}
	return m.runtimeOperations.sealAndWait(streamCancelMaxWait)
}

// WaitRuntimeOperations waits without a deadline after the gate was sealed.
// Session-switch disposal uses this off the UI goroutine so resources are
// eventually released without ever closing SQLite underneath a worker.
func (m *Model) WaitRuntimeOperations() {
	m.runtimeOperations.wait()
}

// SetHandoverAutoSend sets a message to auto-send on Init (for handover restart).
func (m *Model) SetHandoverAutoSend(text string) {
	m.handoverAutoSend = strings.TrimSpace(text)
}

// SetBranchPrefill restores a branch draft on Init without scheduling send.
func (m *Model) SetBranchPrefill(text string) {
	m.branchPrefill = text
}

// SetBranchPathNotes schedules abandoned-path summarization after this model is
// initialized in the newly-created child session.
func (m *Model) SetBranchPathNotes(request *BranchPathNotesRequest) {
	if request == nil {
		m.branchPathNotesRequest = nil
		return
	}
	copyRequest := *request
	if m.sess == nil || (strings.TrimSpace(copyRequest.ChildSessionID) != "" && copyRequest.ChildSessionID != m.sess.ID) {
		m.branchPathNotesRequest = nil
		return
	}
	copyRequest.SourceMessages = append([]llm.Message(nil), request.SourceMessages...)
	m.branchPathNotesRequest = &copyRequest
}

// SetBranchAutoSend schedules a non-empty first message after a branch relaunch.
func (m *Model) SetBranchAutoSend(text string) {
	m.branchAutoSend = strings.TrimSpace(text)
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

// isParentChatMessage identifies background messages owned by the chat model.
// Embedded full-screen views may consume their own input and result messages,
// but must let these pass so parent stream and UI handshakes keep progressing.
// Add new asynchronous chat messages here unless an embedded child owns them.
func isParentChatMessage(msg tea.Msg) bool {
	switch msg.(type) {
	case streamEventMsg,
		tickMsg,
		streamRenderTickMsg,
		ui.SmoothTickMsg,
		ui.WaveTickMsg,
		ui.WavePauseMsg,
		footerMessageClearMsg,
		copyResultMsg,
		copyStatusClearMsg,
		compactStartedMsg,
		compactDoneMsg,
		handoverDoneMsg,
		handoverRenameDoneMsg,
		sessionSavedMsg,
		sessionLoadedMsg,
		shellExitedMsg,
		titleGeneratedMsg,
		mcpStatusUpdateMsg,
		GuardianReviewMsg,
		chatGPTModelsLoadedMsg,
		transcriptMutationDoneMsg,
		conversationBranchCreatedMsg,
		conversationBranchNotesDoneMsg,
		FlushBeforeAskUserMsg,
		FlushBeforeApprovalMsg,
		ResumeFromExternalUIMsg,
		ApprovalRequestMsg,
		AskUserRequestMsg,
		HandoverRequestMsg,
		mentionIndexReadyMsg,
		mentionDebounceMsg,
		mentionMatchesMsg,
		SubagentProgressMsg:
		return true
	default:
		return false
	}
}

func (m *Model) closeEmbeddedViewsForInteractivePrompt() {
	m.inspectorMode = false
	m.inspectorModel = nil
	m.resumeBrowserMode = false
	m.resumeBrowserModel = nil
	m.worktreeBrowserMode = false
	m.worktreeBrowserModel = nil
	m.worktreeBrowserRoot = ""
	m.worktreeBrowserOperation = ""
	if m.commit == nil || (m.commit.Phase != CommitCommitting && m.commit.Phase != CommitStaging) {
		if m.commit != nil && m.commit.cancel != nil {
			m.commit.cancel()
		}
		m.commit = nil
	}
	m.sideQuestion.Visible = false
	m.sideQuestion.ConfirmClear = false
	m.selection = Selection{}
	m.textarea.Focus()
}

func (m *Model) flushBeforeExternalUI(done chan<- struct{}) (tea.Model, tea.Cmd) {
	m.pausedForExternalUI = true
	markComplete := func() {
		if m.tracker != nil {
			m.tracker.MarkCurrentTextComplete(func(text string) string {
				return m.renderMarkdown(text)
			})
		}
	}
	if m.altScreen {
		markComplete()
		close(done)
		return m, nil
	}
	if m.tracker != nil {
		markComplete()
		result := m.tracker.FlushBeforeExternalUI(m.width, 0, maxViewLines, m.renderMd)
		if result.ToPrint != "" {
			return m, tea.Sequence(
				tea.Println(result.ToPrint),
				func() tea.Msg {
					close(done)
					return nil
				},
			)
		}
	}
	close(done)
	return m, nil
}

// Update handles messages
func (m *Model) Update(msg tea.Msg) (model tea.Model, cmd tea.Cmd) {
	if _, ok := msg.(ReloadResumeMsg); ok {
		return m.resumeAfterReload()
	}
	if inspect, ok := msg.(ReloadInspectMsg); ok {
		m.inspectReload(inspect)
		return m, nil
	}
	if m.reloadBlocksInput(msg) {
		return m, nil
	}
	if failed, ok := msg.(steeringStartFailedMsg); ok {
		if failed.generation != m.streamGeneration || failed.operationID != m.steeringHandoff {
			return m, nil
		}
		m.steeringHandoff = ""
		m.interruptNotice = "Steered run did not start; guidance remains in the conversation"
		return m.Update(streamEventMsg{generation: failed.generation, event: ui.ErrorEvent(failed.err)})
	}

	m.textarea.Placeholder = "Type a message…"
	if m.streaming && m.engine != nil && m.engine.SteeringAvailability().CanSteer {
		m.textarea.Placeholder = "Steer conversation…"
	}
	if ready, ok := msg.(steeringReadyMsg); ok {
		return m.handleSteeringReady(ready)
	}

	if envelope, ok := msg.(mainRunUIEnvelope); ok {
		if envelope.sessionID != m.SessionID() || m.mainRunManager == nil || !m.mainRunManager.IsUISinkCurrent(envelope.sessionID, envelope.sinkID) {
			manager := m.mainRunManager
			return m, func() tea.Msg {
				if manager != nil {
					manager.RetainUI(envelope.sessionID, envelope.sinkID, envelope.message)
				}
				return nil
			}
		}
		msg = envelope.message
	}
	defer func() {
		if reportCmd := m.takeTerminalWorkingDirectoryCmd(); reportCmd != nil {
			cmd = tea.Batch(cmd, reportCmd)
		}
	}()
	if m.resizeReflowPending {
		_, isKey := msg.(tea.KeyPressMsg)
		_, isMouse := msg.(tea.MouseMsg)
		if isKey || isMouse {
			oldYOffset := m.viewport.YOffset()
			defer func() {
				if m.resizeReflowPending && m.viewport.YOffset() != oldYOffset {
					m.captureResizeReflowAnchor()
				}
			}()
		}
	}

	var cmds []tea.Cmd
	var flushCmds []tea.Cmd

	if _, ok := msg.(inlineCompletionRenderedMsg); ok {
		m.inlineCompletionPending = false
		pendingKey := m.inlineCompletionPendingKey
		m.inlineCompletionPendingKey = nil
		if pendingKey != nil {
			return m.handleKeyMsg(*pendingKey)
		}
		return m, nil
	}

	if resizeMsg, ok := msg.(resizeReflowMsg); ok {
		if resizeMsg.generation == m.resizeReflowGeneration {
			m.resizeReflowPending = false
			m.resizeReflowHadImages = false
			if m.resizeReflowWasAtBottom {
				m.scrollToBottom = true
				m.resizeReflowRestoreAnchor = false
			} else {
				m.resizeReflowRestoreAnchor = true
			}
		}
		return m, nil
	}
	if progress, ok := msg.(skillRunProgressMsg); ok {
		return m, m.handleSkillRunProgress(progress)
	}
	if done, ok := msg.(skillRunDoneMsg); ok {
		return m, m.handleSkillRunDone(done)
	}
	if _, ok := msg.(queuedMainSkillRetryMsg); ok {
		return m, m.startNextQueuedMainSkill()
	}
	if sideMsg, ok := msg.(sideQuestionEventMsg); ok {
		return m, m.updateSideQuestion(sideMsg)
	}
	if pasteMsg, ok := msg.(tea.PasteMsg); ok && m.sideQuestion.Visible {
		m.focusSideComposer()
		var cmd tea.Cmd
		m.sideQuestion.Composer, cmd = m.sideQuestion.Composer.Update(pasteMsg)
		return m, cmd
	}
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok && m.sideQuestion.Visible {
		return m.handleSideQuestionKey(keyMsg)
	}
	if m.commit != nil {
		switch msg.(type) {
		case tea.MouseMsg:
			return m, nil // Do not click or scroll the conversation through the modal.
		case cursor.BlinkMsg:
			if m.commit.Phase == CommitEditing {
				var cmd tea.Cmd
				m.commit.Message, cmd = m.commit.Message.Update(msg)
				return m, cmd
			}
			return m, nil
		case commitInspectMsg, commitStageMsg, commitScopeMsg, commitDraftMsg, commitDoneMsg, tea.KeyPressMsg, tea.PasteMsg:
			return m.updateCommit(msg)
		}
	}

	// The yolo toggle is intentionally global so it works while streaming,
	// inspecting, or browsing embedded views.
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok && m.isYoloToggleKey(keyMsg) {
		return m.toggleYoloMode()
	}
	if timeoutMsg, ok := msg.(streamCancelTimeoutMsg); ok {
		return m.handleStreamCancelTimeout(timeoutMsg)
	}
	if started, ok := msg.(mainRunStartedMsg); ok {
		if started.generation != m.streamGeneration || started.sessionID != m.SessionID() {
			return m, nil
		}
		m.mainRunID = started.runID
		m.steeringHandoff = ""
		m.interruptNotice = ""
		return m, m.attachMainRun(started.sessionID)
	}
	if closed, ok := msg.(mainRunSubscriberClosedMsg); ok {
		if closed.sessionID != m.SessionID() || closed.runID != m.mainRunID || closed.subscription != m.mainRunSubscription {
			return m, nil
		}
		return m, m.attachMainRun(closed.sessionID)
	}
	if runStatus, ok := msg.(BackgroundRunsMsg); ok {
		if runStatus.owner != nil && runStatus.owner != m {
			return m, nil
		}
		m.backgroundRunCount = runStatus.Count
		m.refreshBranchTreeRunActivity()
		return m, m.mainRunStatusCmd()
	}
	if switched, ok := msg.(sessionSwitchedMsg); ok {
		return m.handleSessionSwitched(switched)
	}
	if loaded, ok := msg.(persistedSubagentsLoadedMsg); ok {
		m.applyPersistedSubagents(loaded)
		return m, nil
	}
	if handled, mentionCmd := m.handleMentionMessage(msg); handled {
		return m, mentionCmd
	}
	if handled, cmd := m.handleTerminalTitleProviderMsg(msg); handled {
		return m, cmd
	}

	// Chat-owned self-scheduling ticks must keep running even while an embedded
	// modal is active. If a spinner tick is forwarded to the inspector/session
	// browser, the child ignores it and the spinner never schedules its next tick,
	// so it appears frozen after returning to chat.
	_, isSpinnerTick := msg.(spinner.TickMsg)

	// Parent chat lifecycle messages must not be swallowed by an embedded view.
	// In particular, the inspector can be opened while a response is streaming;
	// losing its terminal event or approval handshake leaves the composer gated
	// behind stale streaming/external-UI state until the user interrupts it.
	parentChatMsg := isParentChatMessage(msg)

	// Handle worktree browser mode. Its operation completion messages must be
	// routed back to the child rather than the normal slash-command handler.
	if m.worktreeBrowserMode && !isSpinnerTick && !parentChatMsg {
		return m.updateWorktreeBrowserMode(msg)
	}

	// Handle resume browser mode
	if m.resumeBrowserMode && !isSpinnerTick && !parentChatMsg {
		return m.updateResumeBrowserMode(msg)
	}

	// Handle inspector mode
	if m.inspectorMode && !isSpinnerTick && !parentChatMsg {
		return m.updateInspectorMode(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleWindowSizeMsg(msg)
	case tea.KeyPressMsg:
		if m.inlineCompletionPending {
			if m.inlineCompletionPendingKey != nil {
				// Enter has already been accepted for submission. Keep the captured
				// composer state stable until the scrollback acknowledgement, while
				// still allowing the user to cancel that pending submission.
				if key.Matches(msg, m.keyMap.Cancel) || key.Matches(msg, m.keyMap.Quit) {
					m.inlineCompletionPendingKey = nil
					return m.showFooterMuted("Pending send cancelled.")
				}
				return m, nil
			}
			if key.Matches(msg, m.keyMap.Send) {
				pendingKey := msg
				m.inlineCompletionPendingKey = &pendingKey
				return m, nil
			}
		}
		return m.handleKeyMsg(msg)

	case tea.PasteMsg:
		if m.inlineCompletionPendingKey != nil {
			return m, nil
		}
		return m.handlePasteMsg(msg)

	case tea.MouseMsg:
		return m.handleMouseMsg(msg)
	case spinner.TickMsg:
		if (m.streaming || m.directShellRun != nil || m.sideQuestion.Running || m.branchContextInFlight() || m.sessionTransition != nil || m.commitBusy()) && !m.pausedForExternalUI {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, m.passiveCommand(cmd))
		}

	case tickMsg:
		if m.streaming || m.directShellRun != nil {
			cmds = append(cmds, m.tickEvery())
		}

	case streamGoalElapsedMsg:
		if !m.streaming || m.sess == nil || msg.goal == nil || m.sess.ID != msg.sessionID || !m.streamStartTime.Equal(msg.streamStarted) {
			return m, nil
		}
		if m.sess.Goal != nil && m.sess.Goal.UpdatedAt.After(msg.goal.UpdatedAt) {
			return m, nil
		}
		m.sess.Goal = msg.goal.Clone()
		m.streamElapsedOffset = m.goalStreamElapsedOffset()

	case streamRenderTickMsg:
		m.streamRenderTickPending = false
		// No explicit action is needed here: Bubble Tea re-renders after each Update.
		// This tick exists to ensure View() runs again after the throttle window, so
		// pending content can pass shouldThrottleSetContent().

	case tea.SuspendMsg:
		// Bubble Tea restores the terminal before delivering SuspendMsg to the
		// model. Renderer shutdown has removed Kitty resources, so invalidate all
		// acknowledgements before the first resumed frame is composed.
		m.resetImageUploadState()
		m.invalidateImageViewportContent()

	case postFrameImageResultMsg:
		return m, m.handlePostFrameImageResult(msg.Receipt, msg.Err)

	case footerMessageClearMsg:
		if msg.Seq == m.footerMessageSeq {
			m.clearFooterMessage()
		}

	case FooterNoticeMsg:
		text := strings.TrimSpace(msg.Text)
		if text == "" {
			return m, nil
		}
		tone := msg.Tone
		if tone == "" {
			tone = "error"
		}
		return m.showFooterMessageWithTone(text, tone)

	case copyResultMsg:
		return m.handleCopyResult(msg)

	case copyStatusClearMsg:
		if msg.seq == m.copyStatusSeq {
			m.copyStatus = ""
		}

	case directShellOutputMsg:
		return m.handleDirectShellOutput(msg)

	case directShellDoneMsg:
		return m.handleDirectShellDone(msg)

	case shellExitedMsg:
		m.setShellTerminalHandoff(false)
		if msg.err != nil {
			return m.showFooterError(fmt.Sprintf("Shell failed: %v", msg.err))
		}
		if msg.exitCode != 0 {
			return m.showFooterMuted(fmt.Sprintf("Shell exited with status %d.", msg.exitCode))
		}
		return m.showFooterMuted("Shell exited.")

	case worktreeOperationDoneMsg:
		return m.handleWorktreeOperationDone(msg)

	case shareCapabilitiesMsg:
		return m.handleShareCapabilities(msg)

	case shareDoneMsg:
		return m.handleShareDone(msg)

	case chatGPTModelsLoadedMsg:
		return m.applyChatGPTModelsLoaded(msg)

	case providerUsageDoneMsg:
		return m.handleProviderUsageDone(msg)

	case transcriptMutationDoneMsg:
		return m.handleTranscriptMutationDone(msg)

	case conversationBranchCreatedMsg:
		return m.handleConversationBranchCreated(msg)

	case conversationBranchNotesDoneMsg:
		return m.handleConversationBranchNotesDone(msg)

	case promptHistoryLookupMsg:
		return m.handlePromptHistoryLookupMsg(msg)

	case mcpOAuthResultMsg:
		m.refreshMCPPickerIfOpen()
		if msg.err != nil {
			detail := safeMCPOAuthMessage(msg.err)
			action := fmt.Sprintf("Try `/mcp login %s` again.", msg.name)
			if msg.logout {
				action = fmt.Sprintf("Try `/mcp logout %s` again.", msg.name)
			}
			m.dialog.ShowContent("MCP authentication", detail+"\n\n"+action)
			_, footerCmd := m.showFooterMessageWithTone("MCP authentication failed: "+detail, "error")
			cmds = append(cmds, footerCmd)
		} else {
			verb := "Signed in to"
			if msg.logout {
				verb = "Signed out of"
			}
			_, footerCmd := m.showFooterMessage(verb + " MCP server " + msg.name)
			cmds = append(cmds, footerCmd)
		}

	case mcpStatusUpdateMsg:
		m.refreshMCPPickerIfOpen()
		cmds = append(cmds, m.listenForMCPStatusUpdates())
		if msg.update.Status == mcp.StatusFailed {
			failureMessage := formatMCPFailureMessage(msg.update)
			cmds = append(cmds, tea.Println(m.renderMarkdown(failureMessage)+"\n"))
			footer := fmt.Sprintf("MCP server %s failed", msg.update.Name)
			if msg.update.Error != nil {
				footer += ": " + strings.Join(strings.Fields(msg.update.Error.Error()), " ")
			}
			_, footerCmd := m.showFooterMessageWithToneFor(footer, "error", mcpFailureFooterDuration)
			cmds = append(cmds, footerCmd)
		}

	case GuardianReviewMsg:
		m.recordGuardianUsage(context.Background(), msg.Event.Model, msg.Event.Usage)
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

	case compactDoneMsg:
		return m.handleCompactDone(msg)
	case handoverDoneMsg:
		return m.handleHandoverDone(msg)
	case handoverConfirmMsg:
		return m.handleHandoverConfirm(msg)
	case handoverCancelMsg:
		return m.handleHandoverCancel(msg)
	case handoverRenameDoneMsg:
		// Silently ignore — rename is best-effort background work.
		return m, nil

	case titleFallbackTickMsg:
		return m.handleTitleFallbackTick(msg)
	case titleGeneratedMsg:
		return m.handleTitleGenerated(msg)
	case ui.WaveTickMsg:
		if m.tracker != nil {
			if cmd := m.tracker.HandleWaveTick(); cmd != nil {
				cmds = append(cmds, m.passiveCommand(cmd))
			}
		}

	case ui.WavePauseMsg:
		if m.tracker != nil {
			if cmd := m.tracker.HandleWavePause(); cmd != nil {
				cmds = append(cmds, m.passiveCommand(cmd))
			}
		}

	case startupWorkspaceApprovalMsg:
		if m.rootContext().Err() != nil {
			return m, nil
		}
		if errors.Is(msg.err, tools.ErrWorkspaceApprovalCancelled) {
			return m.showFooterMuted("Workspace decision deferred; file tools will ask again when needed.")
		}
		if msg.err != nil {
			return m.showFooterWarning("Workspace access was not granted; your message was not sent.")
		}
		return m, m.initialAutoSendCmd()

	case autoSendMsg:
		m.autoSendPending = false
		// Auto-send has no editable recovery loop. Fail fast and retain the queued
		// input instead of silently dropping it and waiting forever for a stream.
		if _, err := m.agentMentionDelegationContext(m.textarea.Value()); err != nil {
			m.err = err
			m.quitting = true
			_, footerCmd := m.showFooterError(err.Error())
			return m, tea.Sequence(footerCmd, tea.Quit)
		}
		if draft := m.transitionAutoSendDraft; draft != nil {
			m.transitionAutoSendDraft = nil
			updated, cmd := m.sendMessage(m.textarea.Value())
			model := updated.(*Model)
			model.restoreComposerSnapshot(draft.composer)
			model.files = draft.files
			model.images = draft.images
			model.selectedImage = draft.selectedImage
			model.pasteChunks = draft.pasteChunks
			return model, cmd
		}
		return m.sendMessage(m.textarea.Value())

	case ui.SmoothTickMsg:
		m.handleSmoothTick(msg, &cmds, &flushCmds)
	case streamEventMsg:
		updated, streamCmd, immediate, streamCmds, streamFlushCmds := m.handleStreamEvent(msg)
		if immediate {
			return updated, streamCmd
		}
		cmds = append(cmds, streamCmds...)
		flushCmds = append(flushCmds, streamFlushCmds...)
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
		return m.flushBeforeExternalUI(msg.Done)

	case FlushBeforeApprovalMsg:
		return m.flushBeforeExternalUI(msg.Done)

	case ResumeFromExternalUIMsg:
		return m.handleResumeFromExternalUI(msg)
	case ApprovalRequestMsg:
		return m.handleApprovalRequest(msg)
	case AskUserRequestMsg:
		return m.handleAskUserRequest(msg)
	case HandoverRequestMsg:
		return m.handleHandoverRequest(msg)
	case SubagentProgressMsg:
		// Handle subagent progress events and update segment stats.
		if msg.Event.Type == tools.SubagentEventGuardian && msg.Event.Guardian != nil {
			m.recordGuardianUsage(context.Background(), msg.Event.Guardian.Model, msg.Event.Guardian.Usage)
		}
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
	}

	// Update textarea if not streaming
	if !m.streaming {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		if cmd != nil {
			cmds = append(cmds, m.passiveCommand(cmd))
		}
	}

	if cmd := m.maybeScheduleStreamRenderTick(); cmd != nil {
		cmds = append(cmds, cmd)
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
	return m.presentationTick(delay, func(time.Time) tea.Msg {
		return streamRenderTickMsg{}
	})
}

// SetSessionInputsObserver reports successful owning-surface context selections.
// Persistence remains the caller's responsibility; borrowed engines do not use it.
func (m *Model) SetSessionInputsObserver(observer func(*session.Session, string, string)) {
	m.sessionInputsObserver = observer
	m.notifySessionInputs()
}
func (m *Model) notifySessionInputs() {
	if m.sessionInputsObserver != nil && m.sess != nil && m.config != nil {
		m.sessionInputsObserver(m.sess, m.config.Chat.Instructions, m.toolsStr)
	}
}
