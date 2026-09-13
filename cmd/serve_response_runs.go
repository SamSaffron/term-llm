package cmd

import (
	"context"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/runboundary"
	"github.com/samsaffron/term-llm/internal/session"
)

type responseRunEvent struct {
	Sequence int64
	Event    string
	Data     []byte
}

type responseRunRecoveryTool struct {
	ID                 string
	Name               string
	Arguments          string
	ArgumentsFinalized bool
	Status             string
	ResultStatus       string
	AskUserAnswer      string
	Created            int64
	StartedAt          int64
	EndedAt            int64
	DurationMs         int64
	Images             []string
	Media              []webMediaEntry
	GuardianReviews    []map[string]any
	SubagentProgress   map[string]any
}

type responseRunRecoveryMessage struct {
	ID                      string
	Role                    string
	Content                 []byte
	Created                 int64
	Tools                   []responseRunRecoveryTool
	Attachments             []map[string]any
	Expanded                bool
	Status                  string
	Usage                   map[string]any
	InterruptState          string
	ClientMessageID         string
	ResponseID              string
	AssistantSegmentOrdinal int
	SegmentStartSequence    int64
	SegmentEndSequence      int64
	CompactionEventSequence int64
	DurableCompactionSeq    int
	CompactionCount         int
	ModelSwap               *llm.ModelSwapMarker
	EventSequence           int64
}

type responseRunRecoveryEvent struct {
	Event   string
	Payload map[string]any
}

type responseRunSubscribeResult struct {
	id               int
	replay           []responseRunEvent
	ch               <-chan responseRunEvent
	status           string
	snapshotRequired bool
	minReplayAfter   int64
}

type responseRunSegmentRange struct {
	Start int64
	End   int64
}

type responseRunDurableHandoff struct {
	Valid       bool
	FinalRev    int64
	OutputCount int
	Error       string
}

type responseRunPersistenceLedger struct {
	mu           sync.Mutex
	idle         chan struct{}
	inflight     int
	maxRev       int64
	outputKeys   map[string]struct{}
	nextOutputID int64
	failed       bool
	failureText  string
}

func newResponseRunPersistenceLedger() *responseRunPersistenceLedger {
	idle := make(chan struct{})
	close(idle)
	return &responseRunPersistenceLedger{
		idle:       idle,
		outputKeys: make(map[string]struct{}),
	}
}

type responseRunResolvedInteraction struct {
	Outcome    string
	ResolvedAt int64
}

type responseRun struct {
	reloadContinuation      *webRunContinuation
	settled                 chan struct{}
	rushStateful            bool
	rushRequest             llm.Request
	mu                      sync.Mutex
	terminalMu              sync.Mutex
	interactionSubmitMu     sync.Mutex
	id                      string
	sessionID               string
	previousResponseID      string
	clientMessageID         string
	idempotencyKey          string
	idempotencyScope        string
	requestFingerprint      string
	anchorRowID             int64 // latest durable completed boundary; zero means unavailable
	anchorAvailable         bool
	boundary                *runboundary.Tracker
	model                   string
	reasoningEffort         string
	reasoningEffortSet      bool
	created                 int64
	endedAt                 int64
	runEpoch                int64
	startedRev              int64
	startedCompactionSeq    int
	startedCompactionCount  int
	finalRev                int64
	attentionSeq            int64
	attentionStoreID        string
	ownerInstanceID         string
	fencingToken            int64
	leaseExpiresAt          time.Time
	validateLifecycle       func() error
	checkpointLifecycle     func(int64, int) error
	finalizeLifecycle       func(session.ResponseRunState, int64, int) (session.AttentionState, error)
	atomicTranscriptFencing bool
	finalRevReader          func() (int64, error)
	durableHandoff          bool
	durableOutputCount      int
	durableHandoffErr       string
	continuationResponseID  string
	persistence             *responseRunPersistenceLedger
	status                  string
	errorType               string
	errorMessage            string
	usage                   llm.Usage
	sessionUsage            llm.Usage
	lastSequenceNumber      int64
	// events[eventStart:] is the retained replay window; dropped prefix slots
	// are zeroed and reclaimed in batches to avoid per-token slice copies.
	events                  []responseRunEvent
	eventStart              int
	minReplayAfter          int64
	maxRetainedEvents       int
	recoveryMessages        []responseRunRecoveryMessage
	recoveryEvents          []responseRunRecoveryEvent
	resolvedInteractions    map[string]responseRunResolvedInteraction
	pendingGuardianByCall   map[string][]map[string]any
	nextMessageOrdinal      int64
	currentAssistant        int
	currentToolGroup        int
	segmentRanges           map[int]responseRunSegmentRange
	compactionEnabled       bool
	subscribers             map[int]chan responseRunEvent
	subscriberWarned        map[int]bool // tracks whether 75% buffer warning was logged
	subscriberDropped       map[int]bool // tracks subscribers dropped after their live buffer overflowed
	nextSubscriberID        int
	terminalNotifyOnce      sync.Once
	terminalNotify          func(string)
	coarseEvent             func(string, map[string]any)
	interactionStateChanged func(session.ResponseRunInteractionState)
	cancel                  context.CancelFunc
	cancelRequested         bool
}

type startResponseRunOptions struct {
	resume                     *webRunContinuation
	onInitialInput             func()
	rush                       *session.RushOperation
	previousResponseID         string
	uiSession                  bool
	resetResponseIDsOnSuccess  bool
	modelSwap                  *responseModelSwapExecution
	idempotencyKey             string
	idempotencyScope           string
	requestFingerprint         string
	notificationSubscriptionID string
	onDone                     func()
	onAdmissionDone            func() // release request preparation before streaming events
	runtimeSetup               func(*llm.Request) error
}

type responseRunContextKey struct{}

func responseRunFromContext(ctx context.Context) *responseRun {
	if ctx == nil {
		return nil
	}
	run, _ := ctx.Value(responseRunContextKey{}).(*responseRun)
	return run
}

func withResponseRunContext(ctx context.Context, run *responseRun) context.Context {
	if ctx == nil || run == nil {
		return ctx
	}
	return context.WithValue(ctx, responseRunContextKey{}, run)
}

func tagResponseRunMessage(ctx context.Context, msg llm.Message, segmentOrdinal int) llm.Message {
	if ctx == nil {
		return msg
	}
	run, _ := ctx.Value(responseRunContextKey{}).(*responseRun)
	if run == nil {
		return msg
	}
	if msg.Role == llm.RoleUser {
		msg.ResponseID = ""
		msg.AssistantSegmentOrdinal = -1
		return msg
	}
	msg.ResponseID = run.id
	if msg.Role != llm.RoleAssistant {
		msg.AssistantSegmentOrdinal = -1
		return msg
	}
	msg.AssistantSegmentOrdinal = segmentOrdinal
	run.mu.Lock()
	rangeValue := run.segmentRanges[segmentOrdinal]
	run.mu.Unlock()
	msg.SegmentStartSequence = rangeValue.Start
	msg.SegmentEndSequence = rangeValue.End
	return msg
}

func (r *responseRun) setInitialDurableBoundary(rowID int64) bool {
	if r == nil || rowID <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.boundary == nil || !r.boundary.SetInitialDurable(r.id, rowID) {
		return false
	}
	r.anchorRowID, r.anchorAvailable = rowID, true
	return true
}

func (r *responseRun) publishDurableBoundary(turnIndex int, rowID int64) bool {
	if r == nil || rowID <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.boundary == nil || !r.boundary.PublishDurable(r.id, turnIndex, rowID) {
		return false
	}
	r.anchorRowID, r.anchorAvailable = rowID, true
	return true
}

func (r *responseRun) commitCompletedBoundary(turnIndex int, messages []llm.Message, rowID int64, durable bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	boundary := r.boundary
	runID := r.id
	r.mu.Unlock()
	if boundary == nil || !boundary.Commit(runID, turnIndex, messages) {
		return
	}
	if durable && rowID > 0 {
		r.publishDurableBoundary(turnIndex, rowID)
	}
}

func (r *responseRun) invalidateDurableBoundary() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.boundary != nil {
		r.boundary.InvalidateDurable(r.id)
	}
	r.anchorRowID, r.anchorAvailable = 0, false
}

func newResponseRun(respID, sessionID, previousResponseID, model string, created int64, cancel context.CancelFunc) *responseRun {
	return &responseRun{
		id:                    respID,
		sessionID:             sessionID,
		previousResponseID:    previousResponseID,
		model:                 model,
		created:               created,
		status:                "in_progress",
		startedCompactionSeq:  -1,
		maxRetainedEvents:     defaultResponseRunReplayLimit,
		currentAssistant:      -1,
		currentToolGroup:      -1,
		pendingGuardianByCall: make(map[string][]map[string]any),
		segmentRanges:         make(map[int]responseRunSegmentRange),
		persistence:           newResponseRunPersistenceLedger(),
		boundary:              runboundary.New(respID, nil, 0, false),
		compactionEnabled:     true,
		subscribers:           make(map[int]chan responseRunEvent),
		subscriberWarned:      make(map[int]bool),
		subscriberDropped:     make(map[int]bool),
		cancel:                cancel,
	}
}
