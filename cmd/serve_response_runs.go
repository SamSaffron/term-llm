package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/runboundary"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
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
	GuardianReviews    []map[string]any
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
	mu                      sync.Mutex
	terminalMu              sync.Mutex
	interactionSubmitMu     sync.Mutex
	id                      string
	sessionID               string
	previousResponseID      string
	clientMessageID         string
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
	previousResponseID         string
	uiSession                  bool
	resetResponseIDsOnSuccess  bool
	modelSwap                  *responseModelSwapExecution
	idempotencyKey             string
	idempotencyScope           string
	requestFingerprint         string
	notificationSubscriptionID string
	onDone                     func()
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

func (r *responseRun) appendEvent(event string, payload map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.appendEventLocked(event, payload, false)
}

func responseRunOwnedOutputKeys(runID string, messages []llm.Message) []string {
	keys := make([]string, 0, len(messages))
	for _, msg := range messages {
		if msg.ResponseID != runID {
			continue
		}
		switch msg.Role {
		case llm.RoleAssistant:
			keys = append(keys, fmt.Sprintf("assistant:%d", msg.AssistantSegmentOrdinal))
		case llm.RoleTool:
			callID := ""
			for _, part := range msg.Parts {
				if part.ToolResult != nil && part.ToolResult.ID != "" {
					callID = part.ToolResult.ID
					break
				}
				if part.ToolCall != nil && part.ToolCall.ID != "" {
					callID = part.ToolCall.ID
					break
				}
			}
			if callID != "" {
				keys = append(keys, "tool:"+callID)
			} else {
				keys = append(keys, "")
			}
		case llm.RoleEvent:
			if msg.SegmentStartSequence > 0 || msg.SegmentEndSequence > 0 {
				keys = append(keys, fmt.Sprintf("event:%d:%d", msg.SegmentStartSequence, msg.SegmentEndSequence))
			} else {
				keys = append(keys, "")
			}
		}
	}
	return keys
}

func beginResponseRunPersistence(ctx context.Context, messages []llm.Message) (func(int64, error), int) {
	run, _ := ctx.Value(responseRunContextKey{}).(*responseRun)
	if run == nil || run.persistence == nil {
		return func(int64, error) {}, 0
	}
	keys := responseRunOwnedOutputKeys(run.id, messages)
	if len(keys) == 0 {
		return func(int64, error) {}, 0
	}

	ledger := run.persistence
	ledger.mu.Lock()
	if ledger.inflight == 0 {
		ledger.idle = make(chan struct{})
	}
	ledger.inflight++
	for _, key := range keys {
		if key == "" {
			ledger.nextOutputID++
			key = fmt.Sprintf("write:%d", ledger.nextOutputID)
		}
		ledger.outputKeys[key] = struct{}{}
	}
	reservedOutputCount := len(ledger.outputKeys)
	ledger.mu.Unlock()

	var once sync.Once
	return func(rev int64, persistErr error) {
		once.Do(func() {
			ledger.mu.Lock()
			if rev > ledger.maxRev {
				ledger.maxRev = rev
			}
			if persistErr != nil {
				ledger.failed = true
				if ledger.failureText == "" {
					ledger.failureText = persistErr.Error()
				}
			}
			ledger.inflight--
			if ledger.inflight == 0 {
				close(ledger.idle)
			}
			outputCount := len(ledger.outputKeys)
			ledger.mu.Unlock()
			if persistErr == nil && run.checkpointLifecycle != nil && !run.atomicTranscriptFencing {
				if checkpointErr := run.checkpointLifecycle(rev, outputCount); checkpointErr != nil {
					ledger.mu.Lock()
					ledger.failed = true
					if ledger.failureText == "" {
						ledger.failureText = checkpointErr.Error()
					}
					ledger.mu.Unlock()
					if errors.Is(checkpointErr, session.ErrResponseRunLeaseLost) {
						run.mu.Lock()
						cancelRun := run.cancel
						run.mu.Unlock()
						if cancelRun != nil {
							cancelRun()
						}
					}
				}
			}
		})
	}, reservedOutputCount
}

func runResponseRunPersistence(ctx context.Context, messages []llm.Message, persist func(session.ResponseRunFence) (int64, error)) (rev int64, err error) {
	run := responseRunFromContext(ctx)
	if run != nil && run.validateLifecycle != nil {
		if err := run.validateLifecycle(); err != nil {
			return 0, err
		}
	}
	finish, outputCount := beginResponseRunPersistence(ctx, messages)
	defer func() { finish(rev, err) }()
	var fence session.ResponseRunFence
	if run != nil {
		run.mu.Lock()
		fence = session.ResponseRunFence{ResponseID: run.id, OwnerInstanceID: run.ownerInstanceID,
			FencingToken: run.fencingToken, DurableOutputCount: outputCount}
		run.mu.Unlock()
	}
	return persist(fence)
}

func addResponseRunMessage(ctx context.Context, store session.Store, sessionID string, message *session.Message) (int64, error) {
	writer, ok := store.(session.TranscriptRevisionWriter)
	if !ok {
		if err := store.AddMessage(ctx, sessionID, message); err != nil {
			return 0, err
		}
		return 0, nil
	}
	rev, err := writer.AddMessageWithTranscriptRev(ctx, sessionID, message)
	if errors.Is(err, session.ErrTranscriptRevisionUnsupported) {
		if err := store.AddMessage(ctx, sessionID, message); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return rev, err
}

func replaceResponseRunMessages(ctx context.Context, store session.Store, sessionID string, messages []session.Message) (int64, error) {
	writer, ok := store.(session.TranscriptRevisionWriter)
	if !ok {
		if err := store.ReplaceMessages(ctx, sessionID, messages); err != nil {
			return 0, err
		}
		return 0, nil
	}
	rev, err := writer.ReplaceMessagesWithTranscriptRev(ctx, sessionID, messages)
	if errors.Is(err, session.ErrTranscriptRevisionUnsupported) {
		if err := store.ReplaceMessages(ctx, sessionID, messages); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return rev, err
}

func replaceCompactedResponseRunMessages(ctx context.Context, store session.Store, sessionID string, messages []session.Message) (int64, error) {
	writer, ok := store.(session.CompactedTranscriptRevisionWriter)
	if !ok {
		replacer, supported := store.(interface {
			ReplaceCompactedMessages(context.Context, string, []session.Message) error
		})
		if !supported {
			return 0, errors.New("session store cannot replace compacted transcript")
		}
		if err := replacer.ReplaceCompactedMessages(ctx, sessionID, messages); err != nil {
			return 0, err
		}
		return 0, nil
	}
	rev, err := writer.ReplaceCompactedMessagesWithTranscriptRev(ctx, sessionID, messages)
	if errors.Is(err, session.ErrTranscriptRevisionUnsupported) {
		replacer, supported := store.(interface {
			ReplaceCompactedMessages(context.Context, string, []session.Message) error
		})
		if !supported {
			return 0, errors.New("session store cannot replace compacted transcript")
		}
		if err := replacer.ReplaceCompactedMessages(ctx, sessionID, messages); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return rev, err
}

func updateResponseRunStreamingMessage(ctx context.Context, store session.Store, sessionID string, message *session.Message, finalizeText bool) (int64, error) {
	writer, ok := store.(session.TranscriptRevisionWriter)
	if !ok {
		if err := session.UpdateStreamingMessage(ctx, store, sessionID, message, finalizeText); err != nil {
			return 0, err
		}
		return 0, nil
	}
	rev, err := writer.UpdateStreamingMessageWithTranscriptRev(ctx, sessionID, message, finalizeText)
	if errors.Is(err, session.ErrTranscriptRevisionUnsupported) {
		if err := session.UpdateStreamingMessage(ctx, store, sessionID, message, finalizeText); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return rev, err
}

func (r *responseRun) readDurableHandoff() responseRunDurableHandoff {
	return r.readDurableHandoffWithTimeout(responseRunRevisionReadTimeout)
}

func (r *responseRun) readDurableHandoffWithTimeout(timeout time.Duration) responseRunDurableHandoff {
	if r == nil || r.persistence == nil {
		return responseRunDurableHandoff{Valid: true}
	}
	ledger := r.persistence
	if timeout <= 0 {
		timeout = responseRunRevisionReadTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		ledger.mu.Lock()
		if ledger.inflight == 0 {
			break
		}
		idle := ledger.idle
		ledger.mu.Unlock()
		select {
		case <-idle:
			continue
		case <-timer.C:
			ledger.mu.Lock()
			handoff := responseRunDurableHandoff{
				Valid:       false,
				FinalRev:    ledger.maxRev,
				OutputCount: len(ledger.outputKeys),
				Error:       "persistence barrier timed out",
			}
			ledger.mu.Unlock()
			return handoff
		}
	}
	outputCount := len(ledger.outputKeys)
	maxRev := ledger.maxRev
	failed := ledger.failed
	errorText := ledger.failureText
	ledger.mu.Unlock()

	if !failed && outputCount > 0 && maxRev <= 0 && r.finalRevReader != nil {
		var err error
		maxRev, err = r.finalRevReader()
		if err != nil {
			failed = true
			errorText = err.Error()
		}
	}
	valid := !failed && (outputCount == 0 || maxRev > 0)
	if !valid && errorText == "" {
		errorText = "durable output has no transcript revision"
	}
	return responseRunDurableHandoff{
		Valid:       valid,
		FinalRev:    maxRev,
		OutputCount: outputCount,
		Error:       errorText,
	}
}

func (r *responseRun) applyTerminalContinuationLocked(payload map[string]any) {
	response := mapValue(payload["response"])
	if id := stringValue(response["id"]); id != "" {
		r.continuationResponseID = id
	}
}

func (r *responseRun) applyDurableHandoffLocked(handoff responseRunDurableHandoff) {
	r.finalRev = handoff.FinalRev
	r.durableHandoff = handoff.Valid
	r.durableOutputCount = handoff.OutputCount
	r.durableHandoffErr = handoff.Error
}

func (r *responseRun) finalizeLifecycleLocked(outcome session.ResponseRunState) error {
	if r.finalizeLifecycle == nil {
		return nil
	}
	attention, err := r.finalizeLifecycle(outcome, r.finalRev, r.durableOutputCount)
	if err != nil {
		return fmt.Errorf("persist terminal response lifecycle: %w", err)
	}
	r.attentionSeq = attention.LatestAttentionSeq
	r.attentionStoreID = attention.StoreInstanceID
	return nil
}

func (r *responseRun) appendLifecycleFailureLocked(payload map[string]any, lifecycleErr error) error {
	r.status = "failed"
	r.errorType = "server_error"
	r.errorMessage = "response lifecycle could not be finalized; recovery is required"
	if payload == nil {
		payload = map[string]any{}
	}
	payload["lifecycle_recovery_required"] = true
	response := mapValue(payload["response"])
	if len(response) == 0 {
		response = map[string]any{"id": r.id, "object": "response"}
		payload["response"] = response
	}
	response["status"] = "failed"
	response["error"] = map[string]any{"type": r.errorType, "message": r.errorMessage}
	delete(response, "usage")
	delete(response, "session_usage")
	if appendErr := r.appendEventLocked("response.failed", payload, true); appendErr != nil {
		return errors.Join(lifecycleErr, appendErr)
	}
	return lifecycleErr
}

func (r *responseRun) complete(payload map[string]any, usage llm.Usage, sessionUsage llm.Usage) error {
	r.terminalMu.Lock()
	defer r.terminalMu.Unlock()
	handoff := r.readDurableHandoff()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applyDurableHandoffLocked(handoff)
	r.applyTerminalContinuationLocked(payload)
	if r.cancelRequested {
		r.status = "cancelled"
		r.endedAt = time.Now().UnixMilli()
		r.errorType = ""
		r.errorMessage = ""
		r.cancel = nil
		r.cancelRequested = false
		if response := mapValue(payload["response"]); len(response) > 0 {
			response["status"] = "cancelled"
			delete(response, "usage")
			delete(response, "session_usage")
		}
		if err := r.finalizeLifecycleLocked(session.ResponseRunCancelled); err != nil {
			return r.appendLifecycleFailureLocked(payload, err)
		}
		return r.appendEventLocked("response.cancelled", payload, true)
	}
	r.endedAt = time.Now().UnixMilli()
	r.cancel = nil
	r.cancelRequested = false
	if !handoff.Valid {
		r.status = "failed"
		r.errorType = "server_error"
		r.errorMessage = "response persistence could not be durably verified"
		if handoff.Error != "" {
			r.errorMessage += ": " + handoff.Error
		}
		if response := mapValue(payload["response"]); len(response) > 0 {
			response["status"] = "failed"
			response["error"] = map[string]any{"type": r.errorType, "message": r.errorMessage}
			delete(response, "usage")
			delete(response, "session_usage")
		}
		if err := r.finalizeLifecycleLocked(session.ResponseRunFailed); err != nil {
			return r.appendLifecycleFailureLocked(payload, err)
		}
		return r.appendEventLocked("response.failed", payload, true)
	}
	r.status = "completed"
	r.errorType = ""
	r.errorMessage = ""
	r.usage = usage
	r.sessionUsage = sessionUsage
	if err := r.finalizeLifecycleLocked(session.ResponseRunCompleted); err != nil {
		return r.appendLifecycleFailureLocked(payload, err)
	}
	return r.appendEventLocked("response.completed", payload, true)
}

func (r *responseRun) finishCancelled(payload map[string]any) (bool, error) {
	r.terminalMu.Lock()
	defer r.terminalMu.Unlock()
	r.mu.Lock()
	if !r.cancelRequested {
		r.mu.Unlock()
		return false, nil
	}
	r.mu.Unlock()
	handoff := r.readDurableHandoff()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cancelRequested {
		return false, nil
	}
	r.applyDurableHandoffLocked(handoff)
	r.applyTerminalContinuationLocked(payload)
	r.status = "cancelled"
	r.endedAt = time.Now().UnixMilli()
	r.errorType = ""
	r.errorMessage = ""
	r.cancel = nil
	r.cancelRequested = false
	if err := r.finalizeLifecycleLocked(session.ResponseRunCancelled); err != nil {
		return true, r.appendLifecycleFailureLocked(payload, err)
	}
	return true, r.appendEventLocked("response.cancelled", payload, true)
}

func (r *responseRun) fail(payload map[string]any, errType, errMessage string) (bool, error) {
	r.terminalMu.Lock()
	defer r.terminalMu.Unlock()
	handoff := r.readDurableHandoff()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applyDurableHandoffLocked(handoff)
	r.applyTerminalContinuationLocked(payload)
	hadSubscribers := len(r.subscribers) > 0
	r.status = "failed"
	r.endedAt = time.Now().UnixMilli()
	r.errorType = errType
	r.errorMessage = errMessage
	r.cancel = nil
	r.cancelRequested = false
	if err := r.finalizeLifecycleLocked(session.ResponseRunFailed); err != nil {
		return hadSubscribers, r.appendLifecycleFailureLocked(payload, err)
	}
	return hadSubscribers, r.appendEventLocked("response.failed", payload, true)
}

func (r *responseRun) applyRuntimeMetadataLocked(event string, payload map[string]any) {
	var source map[string]any
	switch event {
	case "response.created", "response.completed", "response.cancelled":
		source = mapValue(payload["response"])
	case "response.model_switch":
		source = payload
	default:
		return
	}
	if len(source) == 0 {
		return
	}
	if model := stringValue(source["model"]); model != "" {
		r.model = model
	}
	if _, ok := source["reasoning_effort"]; ok {
		r.reasoningEffort = stringValue(source["reasoning_effort"])
		r.reasoningEffortSet = true
	}
}

func (r *responseRun) appendEventLocked(event string, payload map[string]any, terminal bool) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["response_id"] = r.id
	payload["run_epoch"] = r.runEpoch
	if event == "response.created" {
		payload["started_rev"] = r.startedRev
		payload["started_at"] = r.created * 1000
		if r.clientMessageID != "" {
			payload["client_message_id"] = r.clientMessageID
		}
		if r.anchorRowID > 0 {
			payload["anchor_row_id"] = r.anchorRowID
		}
		if response := mapValue(payload["response"]); len(response) > 0 {
			response["started_rev"] = r.startedRev
			response["run_epoch"] = r.runEpoch
			if r.clientMessageID != "" {
				response["client_message_id"] = r.clientMessageID
			}
			if r.anchorRowID > 0 {
				response["anchor_row_id"] = r.anchorRowID
			}
		}
	}
	if terminal {
		payload["ended_at"] = r.endedAt
		payload["final_rev"] = r.finalRev
		payload["durable_handoff"] = r.durableHandoff
		payload["durable_output_count"] = r.durableOutputCount
		if r.attentionSeq > 0 {
			payload["attention_seq"] = r.attentionSeq
			payload["attention_final_rev"] = r.finalRev
			payload["attention_response_id"] = r.id
			payload["attention_store_instance_id"] = r.attentionStoreID
		}
		payload["handoff_compaction_seq"] = r.startedCompactionSeq
		payload["handoff_compaction_count"] = r.startedCompactionCount
		if r.durableHandoffErr != "" {
			payload["durable_handoff_error"] = r.durableHandoffErr
		}
		if response := mapValue(payload["response"]); len(response) > 0 {
			response["ended_at"] = r.endedAt
			response["final_rev"] = r.finalRev
			response["durable_handoff"] = r.durableHandoff
			response["durable_output_count"] = r.durableOutputCount
			if r.attentionSeq > 0 {
				response["attention_seq"] = r.attentionSeq
				response["attention_final_rev"] = r.finalRev
				response["attention_response_id"] = r.id
				response["attention_store_instance_id"] = r.attentionStoreID
			}
			response["handoff_compaction_seq"] = r.startedCompactionSeq
			response["handoff_compaction_count"] = r.startedCompactionCount
			if r.durableHandoffErr != "" {
				response["durable_handoff_error"] = r.durableHandoffErr
			}
		}
	}
	if terminal {
		outcome := "cancelled-by-agent"
		if event == "response.failed" {
			outcome = "failed"
		}
		r.resolvePendingInteractionsLocked(outcome)
	}
	r.lastSequenceNumber++
	payload["sequence_number"] = r.lastSequenceNumber
	r.applyRuntimeMetadataLocked(event, payload)

	data, err := json.Marshal(payload)
	if err != nil {
		r.lastSequenceNumber--
		delete(payload, "sequence_number")
		return err
	}

	r.applyRecoveryEventLocked(event, payload)
	r.storeEventLocked(responseRunEvent{
		Sequence: r.lastSequenceNumber,
		Event:    event,
		Data:     data,
	}, terminal)
	if r.interactionStateChanged != nil {
		switch event {
		case "response.ask_user.prompt", "response.ask_user.resolved",
			"response.approval.prompt", "response.approval.resolved":
			r.interactionStateChanged(r.interactionStateLocked())
		}
	}
	if r.coarseEvent != nil {
		r.coarseEvent(event, payload)
	}
	if terminal && r.terminalNotify != nil && (event == "response.completed" || event == "response.failed") {
		outcome := "completed"
		if event == "response.failed" {
			outcome = "failed"
		}
		r.terminalNotifyOnce.Do(func() { go r.terminalNotify(outcome) })
	}
	return nil
}

// storeEventLocked appends stored to r.events, compacts the buffer, and fans
// out to all live subscribers. Must be called with r.mu held.
func (r *responseRun) storeEventLocked(stored responseRunEvent, terminal bool) {
	r.events = append(r.events, stored)
	r.compactEventsLocked()

	// Fan out to subscribers under the lock to guarantee event ordering.
	// Non-blocking send: the 256-event buffer provides ample headroom.
	// A subscriber that can't accept is truly stalled and gets dropped immediately.
	for id, ch := range r.subscribers {
		select {
		case ch <- stored:
			fill := len(ch)
			threshold := cap(ch) * 3 / 4
			if fill > threshold && !r.subscriberWarned[id] {
				log.Printf("response run %s subscriber %d buffer at %d/%d", r.id, id, fill, cap(ch))
				r.subscriberWarned[id] = true
			} else if fill <= threshold/2 && r.subscriberWarned[id] {
				r.subscriberWarned[id] = false
			}
		default:
			log.Printf("response run %s subscriber fell behind at sequence %d; closing stream", r.id, stored.Sequence)
			r.subscriberDropped[id] = true
			close(ch)
			delete(r.subscribers, id)
			delete(r.subscriberWarned, id)
		}
	}

	if terminal {
		for id, ch := range r.subscribers {
			close(ch)
			delete(r.subscribers, id)
			delete(r.subscriberWarned, id)
		}
	}
}

func responseRunInt64Value(value any, fallback int64) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return parsed
		}
	}
	return fallback
}

func responseRunIntValue(value any, fallback int) int {
	return int(responseRunInt64Value(value, int64(fallback)))
}

func encodeTextDeltaPayloadWithIdentity(responseID string, runEpoch int64, outputIndex, segmentOrdinal int, segmentStartSequence int64, delta string, sequenceNumber int64) ([]byte, error) {
	data := make([]byte, 0, 160+len(responseID)+len(delta))
	data = append(data, `{"response_id":`...)
	data = appendJSONString(data, responseID)
	data = append(data, `,"run_epoch":`...)
	data = strconv.AppendInt(data, runEpoch, 10)
	data = append(data, `,"assistant_segment_ordinal":`...)
	data = strconv.AppendInt(data, int64(segmentOrdinal), 10)
	data = append(data, `,"segment_start_sequence":`...)
	data = strconv.AppendInt(data, segmentStartSequence, 10)
	data = append(data, `,"output_index":`...)
	data = strconv.AppendInt(data, int64(outputIndex), 10)
	data = append(data, `,"delta":`...)
	if utf8.ValidString(delta) {
		data = appendJSONString(data, delta)
	} else {
		encoded, err := json.Marshal(delta)
		if err != nil {
			return nil, err
		}
		data = append(data, encoded...)
	}
	data = append(data, `,"sequence_number":`...)
	data = strconv.AppendInt(data, sequenceNumber, 10)
	data = append(data, '}')
	return data, nil
}

// appendTextDeltaSegmentEvent is a fast path for response.output_text.delta that avoids
// allocating a map[string]any or a typed payload on every streamed token.
func (r *responseRun) appendTextDeltaSegmentEvent(outputIndex, segmentOrdinal int, delta string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastSequenceNumber++
	sequenceNumber := r.lastSequenceNumber
	rangeValue, hadRange := r.segmentRanges[segmentOrdinal]
	previousRange := rangeValue
	if rangeValue.Start == 0 {
		rangeValue.Start = sequenceNumber
	}
	rangeValue.End = sequenceNumber
	r.segmentRanges[segmentOrdinal] = rangeValue

	data, err := encodeTextDeltaPayloadWithIdentity(r.id, r.runEpoch, outputIndex, segmentOrdinal, rangeValue.Start, delta, sequenceNumber)
	if err != nil {
		r.lastSequenceNumber--
		if hadRange {
			r.segmentRanges[segmentOrdinal] = previousRange
		} else {
			delete(r.segmentRanges, segmentOrdinal)
		}
		return err
	}

	if delta != "" {
		r.closeToolGroupLocked()
		idx := r.ensureAssistantMessageLocked(segmentOrdinal)
		r.recoveryMessages[idx].Content = append(r.recoveryMessages[idx].Content, delta...)
		r.recoveryMessages[idx].SegmentStartSequence = rangeValue.Start
		r.recoveryMessages[idx].SegmentEndSequence = rangeValue.End
	}

	r.storeEventLocked(responseRunEvent{
		Sequence: r.lastSequenceNumber,
		Event:    "response.output_text.delta",
		Data:     data,
	}, false)
	return nil
}

func (r *responseRun) compactEventsLocked() {
	if !r.compactionEnabled || r.maxRetainedEvents <= 0 {
		return
	}

	activeLen := len(r.events) - r.eventStart
	if activeLen <= r.maxRetainedEvents {
		return
	}

	dropCount := activeLen - r.maxRetainedEvents
	firstKept := r.eventStart + dropCount

	nextReplayAfter := r.events[firstKept].Sequence - 1
	if nextReplayAfter > r.minReplayAfter {
		r.minReplayAfter = nextReplayAfter
	}

	for i := r.eventStart; i < firstKept; i++ {
		r.events[i] = responseRunEvent{}
	}
	r.eventStart = firstKept
	r.compactEventStorageLocked()
}

// compactEventStorageLocked reclaims the dropped prefix in batches so steady
// streaming appends avoid copying the replay window on every token while still
// keeping the backing array bounded to roughly twice maxRetainedEvents.
func (r *responseRun) compactEventStorageLocked() {
	if r.eventStart == 0 {
		return
	}
	if r.maxRetainedEvents > 0 && r.eventStart < r.maxRetainedEvents {
		return
	}

	activeLen := len(r.events) - r.eventStart
	copy(r.events, r.events[r.eventStart:])
	tail := r.events[activeLen:]
	for i := range tail {
		tail[i] = responseRunEvent{}
	}
	r.events = r.events[:activeLen]
	r.eventStart = 0
}

func (r *responseRun) activeEventsLocked() []responseRunEvent {
	return r.events[r.eventStart:]
}

func (r *responseRun) attachGuardianReviewLocked(callID string, review map[string]any) bool {
	for messageIndex := range r.recoveryMessages {
		group := &r.recoveryMessages[messageIndex]
		if group.Role != "tool-group" {
			continue
		}
		for toolIndex := range group.Tools {
			if group.Tools[toolIndex].ID == callID {
				group.Tools[toolIndex].GuardianReviews = append(group.Tools[toolIndex].GuardianReviews, cloneJSONMap(review))
				return true
			}
		}
	}
	return false
}

func (r *responseRun) flushPendingGuardianReviewsLocked() {
	callIDs := make([]string, 0, len(r.pendingGuardianByCall))
	for callID := range r.pendingGuardianByCall {
		callIDs = append(callIDs, callID)
	}
	sort.Strings(callIDs)
	for _, callID := range callIDs {
		for _, review := range r.pendingGuardianByCall[callID] {
			message := strings.TrimSpace(stringValue(review["message"]))
			if message == "" {
				outcome := strings.TrimSpace(stringValue(review["outcome"]))
				if outcome == "" {
					outcome = "warning"
				}
				message = fmt.Sprintf("Guardian %s review", outcome)
			}
			message = fmt.Sprintf("%s (unmatched tool call %s)", message, callID)
			r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
				ID: r.nextRecoveryMessageIDLocked("guardian_notice"), Role: "guardian-notice",
				Content: []byte(message), Created: time.Now().UnixMilli(),
			})
		}
		delete(r.pendingGuardianByCall, callID)
	}
}

func responseRunBoolValue(value any) bool {
	result, _ := value.(bool)
	return result
}

func interactionKindForRecoveryEvent(pending responseRunRecoveryEvent) string {
	switch pending.Event {
	case "response.ask_user.prompt":
		return "ask_user"
	case "response.approval.prompt":
		switch {
		case responseRunBoolValue(pending.Payload["is_workspace"]):
			return "approval.workspace"
		case responseRunBoolValue(pending.Payload["is_shell"]):
			return "approval.shell"
		case responseRunBoolValue(pending.Payload["is_write"]):
			return "approval.file_write"
		default:
			return "approval"
		}
	default:
		return ""
	}
}

func (r *responseRun) interactionStateLocked() session.ResponseRunInteractionState {
	state := session.ResponseRunInteractionState{
		ResponseID:      r.id,
		OwnerInstanceID: r.ownerInstanceID,
		FencingToken:    r.fencingToken,
		Revision:        r.lastSequenceNumber,
	}
	kindSet := make(map[string]struct{})
	for _, pending := range r.recoveryEvents {
		kind := interactionKindForRecoveryEvent(pending)
		if kind == "" {
			continue
		}
		state.Count++
		kindSet[kind] = struct{}{}
		createdAt := responseRunInt64Value(pending.Payload["created_at"], 0)
		if createdAt > 0 && (state.RequiredSince.IsZero() || createdAt < state.RequiredSince.UnixMilli()) {
			state.RequiredSince = time.UnixMilli(createdAt).UTC()
		}
	}
	state.Kinds = make([]string, 0, len(kindSet))
	for kind := range kindSet {
		state.Kinds = append(state.Kinds, kind)
	}
	sort.Strings(state.Kinds)
	return state
}

func (r *responseRun) interactionState() session.ResponseRunInteractionState {
	if r == nil {
		return session.ResponseRunInteractionState{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interactionStateLocked()
}

func (r *responseRun) resolvePendingInteractionsLocked(outcome string) {
	if len(r.recoveryEvents) == 0 {
		return
	}
	if r.resolvedInteractions == nil {
		r.resolvedInteractions = make(map[string]responseRunResolvedInteraction)
	}
	now := time.Now().UnixMilli()
	for _, pending := range r.recoveryEvents {
		kind, id, event, idField := "", "", "", ""
		switch pending.Event {
		case "response.approval.prompt":
			kind, id = "approval", stringValue(pending.Payload["approval_id"])
			event, idField = "response.approval.resolved", "approval_id"
		case "response.ask_user.prompt":
			kind, id = "ask_user", stringValue(pending.Payload["call_id"])
			event, idField = "response.ask_user.resolved", "call_id"
		}
		if id == "" {
			continue
		}
		key := kind + ":" + id
		if _, resolved := r.resolvedInteractions[key]; resolved {
			continue
		}
		r.resolvedInteractions[key] = responseRunResolvedInteraction{Outcome: outcome, ResolvedAt: now}
		r.lastSequenceNumber++
		payload := map[string]any{
			"response_id": r.id, "run_epoch": r.runEpoch, "sequence_number": r.lastSequenceNumber,
			idField: id, "outcome": outcome, "resolved_at": now,
		}
		data, err := json.Marshal(payload)
		if err == nil {
			r.storeEventLocked(responseRunEvent{Sequence: r.lastSequenceNumber, Event: event, Data: data}, false)
		} else {
			r.lastSequenceNumber--
		}
	}
	clear(r.recoveryEvents)
	r.recoveryEvents = nil
}

func (r *responseRun) applyRecoveryEventLocked(event string, payload map[string]any) {
	if event == "response.completed" || event == "response.cancelled" || event == "response.failed" {
		r.flushPendingGuardianReviewsLocked()
	}
	switch event {
	case "response.ask_user.prompt":
		r.recoveryEvents = append(r.recoveryEvents, responseRunRecoveryEvent{
			Event:   event,
			Payload: cloneJSONMap(payload),
		})
		callID := stringValue(payload["call_id"])
		if callID == "" {
			return
		}
		arguments := ""
		if encoded, err := json.Marshal(map[string]any{"questions": payload["questions"]}); err == nil {
			arguments = string(encoded)
		}
		if r.currentToolGroup >= 0 && r.currentToolGroup < len(r.recoveryMessages) {
			group := &r.recoveryMessages[r.currentToolGroup]
			for i := range group.Tools {
				if group.Tools[i].ID == callID {
					if group.Tools[i].Arguments == "" {
						group.Tools[i].Arguments = arguments
					}
					group.Tools[i].ArgumentsFinalized = true
					return
				}
			}
		}
		r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
			ID:      r.nextRecoveryMessageIDLocked("tool_group"),
			Role:    "tool-group",
			Created: time.Now().UnixMilli(),
			Tools: []responseRunRecoveryTool{{
				ID: callID, Name: tools.AskUserToolName, Arguments: arguments,
				ArgumentsFinalized: true, Status: "running", Created: time.Now().UnixMilli(),
			}},
			Status: "running",
		})
		r.currentToolGroup = len(r.recoveryMessages) - 1
		r.currentAssistant = -1
	case "response.approval.prompt":
		r.recoveryEvents = append(r.recoveryEvents, responseRunRecoveryEvent{
			Event:   event,
			Payload: cloneJSONMap(payload),
		})
	case "response.guardian.review":
		callID := stringValue(payload["tool_call_id"])
		review := cloneJSONMap(payload)
		delete(review, "response_id")
		delete(review, "run_epoch")
		delete(review, "sequence_number")
		delete(review, "tool_call_id")
		if callID != "" {
			if !r.attachGuardianReviewLocked(callID, review) {
				if r.pendingGuardianByCall == nil {
					r.pendingGuardianByCall = make(map[string][]map[string]any)
				}
				r.pendingGuardianByCall[callID] = append(r.pendingGuardianByCall[callID], review)
			}
			return
		}
		message := strings.TrimSpace(stringValue(payload["message"]))
		if message == "" {
			return
		}
		r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
			ID: r.nextRecoveryMessageIDLocked("guardian_notice"), Role: "guardian-notice",
			Content: []byte(message), Created: time.Now().UnixMilli(),
		})
		return
	case "response.interjection":
		text := stringValue(payload["text"])
		attachments := attachmentsFromPayload(payload["attachments"])
		explicitID := stringValue(payload["client_message_id"])
		if text == "" && len(attachments) == 0 && explicitID == "" {
			return
		}
		r.closeToolGroupLocked()
		r.currentAssistant = -1
		id := explicitID
		if id == "" {
			id = r.nextRecoveryMessageIDLocked("user")
		}
		r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
			ID:              id,
			Role:            "user",
			Content:         []byte(text),
			Created:         time.Now().UnixMilli(),
			Attachments:     attachments,
			InterruptState:  "interject",
			ClientMessageID: id,
		})
		return
	}

	switch event {
	case "response.attempt.discard":
		kept := r.recoveryMessages[:0]
		for _, message := range r.recoveryMessages {
			if message.Role == "assistant" || message.Role == "tool-group" {
				continue
			}
			kept = append(kept, message)
		}
		r.recoveryMessages = kept
		r.currentAssistant = -1
		r.currentToolGroup = -1
		clear(r.pendingGuardianByCall)
	case "response.output_text.delta":
		delta := stringValue(payload["delta"])
		if delta == "" {
			return
		}
		r.closeToolGroupLocked()
		ordinal := responseRunIntValue(payload["assistant_segment_ordinal"], 0)
		idx := r.ensureAssistantMessageLocked(ordinal)
		r.recoveryMessages[idx].Content = append(r.recoveryMessages[idx].Content, delta...)
		r.recoveryMessages[idx].SegmentStartSequence = responseRunInt64Value(payload["segment_start_sequence"], 0)
		r.recoveryMessages[idx].SegmentEndSequence = responseRunInt64Value(payload["sequence_number"], 0)
	case "response.output_text.new_segment":
		r.closeToolGroupLocked()
		r.currentAssistant = -1
	case "response.compaction":
		r.closeToolGroupLocked()
		r.currentAssistant = -1
		sequence := responseRunInt64Value(payload["sequence_number"], 0)
		r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
			ID:                      fmt.Sprintf("%s_compaction_%d", r.id, sequence),
			Role:                    "compaction-boundary",
			Created:                 time.Now().UnixMilli(),
			CompactionEventSequence: sequence,
			DurableCompactionSeq:    responseRunIntValue(payload["compaction_seq"], -1),
			CompactionCount:         responseRunIntValue(payload["compaction_count"], 0),
		})
	case "response.model_switch":
		r.closeToolGroupLocked()
		r.currentAssistant = -1
		marker := &llm.ModelSwapMarker{
			FromProvider: strings.TrimSpace(stringValue(payload["from_provider"])),
			FromModel:    strings.TrimSpace(stringValue(payload["from_model"])),
			FromEffort:   strings.TrimSpace(stringValue(payload["from_reasoning_effort"])),
			ToProvider:   strings.TrimSpace(stringValue(payload["to_provider"])),
			ToModel:      strings.TrimSpace(stringValue(payload["to_model"])),
			ToEffort:     strings.TrimSpace(stringValue(payload["to_reasoning_effort"])),
			BoundaryID:   strings.TrimSpace(stringValue(payload["boundary_id"])),
			Status:       strings.TrimSpace(stringValue(payload["swap_status"])),
			Strategy:     strings.TrimSpace(stringValue(payload["swap_strategy"])),
		}
		if marker.Status == "" {
			marker.Status = "succeeded"
		}
		content := strings.TrimSpace(stringValue(payload["message"]))
		if content == "" && marker.FromModel != "" && marker.ToModel != "" {
			content = llm.FormatModelSwapMarker(*marker)
		}
		if content != "" && marker.FromModel != "" && marker.ToModel != "" {
			marker.DisplayText = content
			r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
				ID:            r.nextRecoveryMessageIDLocked("model_switch"),
				Role:          "model-swap",
				Content:       []byte(content),
				Created:       time.Now().UnixMilli(),
				ModelSwap:     marker,
				EventSequence: responseRunInt64Value(payload["sequence_number"], 0),
			})
		}
	case "response.output_item.added":
		item := mapValue(payload["item"])
		if stringValue(item["type"]) != "function_call" {
			return
		}
		tool := responseRunRecoveryTool{
			ID:        stringValue(item["call_id"]),
			Name:      stringValue(item["name"]),
			Arguments: stringValue(item["arguments"]),
			Status:    "running",
			Created:   time.Now().UnixMilli(),
		}
		if tool.ID == "" {
			tool.ID = fmt.Sprintf("%s_tool_%d", r.id, len(r.recoveryMessages)+1)
		}
		if pending := r.pendingGuardianByCall[tool.ID]; len(pending) > 0 {
			for _, review := range pending {
				tool.GuardianReviews = append(tool.GuardianReviews, cloneJSONMap(review))
			}
			delete(r.pendingGuardianByCall, tool.ID)
		}
		if r.currentToolGroup < 0 || r.currentToolGroup >= len(r.recoveryMessages) {
			r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
				ID:       r.nextRecoveryMessageIDLocked("tool_group"),
				Role:     "tool-group",
				Created:  time.Now().UnixMilli(),
				Tools:    []responseRunRecoveryTool{tool},
				Expanded: false,
				Status:   "running",
			})
			r.currentToolGroup = len(r.recoveryMessages) - 1
		} else {
			group := &r.recoveryMessages[r.currentToolGroup]
			group.Tools = append(group.Tools, tool)
			group.Status = "running"
		}
		r.currentAssistant = -1
	case "response.output_item.done":
		item := mapValue(payload["item"])
		if stringValue(item["type"]) != "function_call" || r.currentToolGroup < 0 || r.currentToolGroup >= len(r.recoveryMessages) {
			return
		}
		callID := stringValue(item["call_id"])
		name := stringValue(item["name"])
		arguments := stringValue(item["arguments"])
		group := &r.recoveryMessages[r.currentToolGroup]
		for i := range group.Tools {
			if callID != "" && group.Tools[i].ID == callID {
				group.Tools[i].Arguments = arguments
				group.Tools[i].ArgumentsFinalized = true
				return
			}
			if callID == "" && name != "" && group.Tools[i].Name == name && group.Tools[i].Status == "running" {
				group.Tools[i].Arguments = arguments
				group.Tools[i].ArgumentsFinalized = true
				return
			}
		}
	case "response.tool_exec.start":
		if r.currentToolGroup >= 0 && r.currentToolGroup < len(r.recoveryMessages) {
			group := &r.recoveryMessages[r.currentToolGroup]
			callID := stringValue(payload["call_id"])
			for i := range group.Tools {
				if callID == "" || group.Tools[i].ID == callID {
					if group.Tools[i].StartedAt == 0 {
						group.Tools[i].StartedAt = responseRunInt64Value(payload["started_at"], group.Tools[i].Created)
					}
					if callID != "" {
						break
					}
				}
			}
		}
	case "response.tool_exec.end":
		images := stringSliceValue(payload["images"])
		askUserAnswer := strings.TrimSpace(stringValue(payload["ask_user_summary"]))
		succeeded := true
		if value, ok := payload["success"].(bool); ok {
			succeeded = value
		}
		if r.currentToolGroup >= 0 && r.currentToolGroup < len(r.recoveryMessages) {
			group := &r.recoveryMessages[r.currentToolGroup]
			callID := stringValue(payload["call_id"])
			for i := range group.Tools {
				if callID == "" || group.Tools[i].ID == callID {
					if startedAt := responseRunInt64Value(payload["started_at"], 0); startedAt > 0 && group.Tools[i].StartedAt == 0 {
						group.Tools[i].StartedAt = startedAt
					}
					group.Tools[i].EndedAt = responseRunInt64Value(payload["ended_at"], 0)
					group.Tools[i].DurationMs = responseRunInt64Value(payload["duration_ms"], 0)
					if group.Tools[i].DurationMs == 0 && group.Tools[i].StartedAt > 0 && group.Tools[i].EndedAt >= group.Tools[i].StartedAt {
						group.Tools[i].DurationMs = group.Tools[i].EndedAt - group.Tools[i].StartedAt
					}
					if succeeded {
						group.Tools[i].Status = "done"
						group.Tools[i].ResultStatus = "success"
					} else {
						group.Tools[i].Status = "error"
						group.Tools[i].ResultStatus = "error"
					}
					group.Tools[i].Images = appendUniqueStrings(group.Tools[i].Images, images...)
					if succeeded && callID != "" && group.Tools[i].Name == tools.AskUserToolName && askUserAnswer != "" {
						group.Tools[i].AskUserAnswer = askUserAnswer
					}
					if callID != "" {
						break
					}
				}
			}
			allDone := len(group.Tools) > 0
			for _, tool := range group.Tools {
				if tool.Status != "done" && tool.Status != "error" {
					allDone = false
					break
				}
			}
			if allDone {
				group.Status = "done"
			}
		}
	case "response.completed":
		r.closeToolGroupLocked()
		response := mapValue(payload["response"])
		usage := mapValue(response["usage"])
		if len(usage) == 0 {
			return
		}
		for i := len(r.recoveryMessages) - 1; i >= 0; i-- {
			if r.recoveryMessages[i].Role == "assistant" {
				r.recoveryMessages[i].Usage = cloneJSONMap(usage)
				return
			}
		}
	case "response.cancelled":
		r.closeToolGroupLocked()
	case "response.failed":
		r.closeToolGroupLocked()
		errPayload := mapValue(payload["error"])
		message := stringValue(errPayload["message"])
		if message == "" {
			return
		}
		r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
			ID:      r.nextRecoveryMessageIDLocked("error"),
			Role:    "error",
			Content: []byte(message),
			Created: time.Now().UnixMilli(),
		})
		r.currentAssistant = -1
	}
}

func (r *responseRun) nextRecoveryMessageIDLocked(kind string) string {
	r.nextMessageOrdinal++
	return fmt.Sprintf("%s_%s_%d", r.id, kind, r.nextMessageOrdinal)
}

func (r *responseRun) ensureAssistantMessageLocked(segmentOrdinal int) int {
	if r.currentAssistant >= 0 && r.currentAssistant < len(r.recoveryMessages) {
		current := &r.recoveryMessages[r.currentAssistant]
		if current.AssistantSegmentOrdinal == segmentOrdinal {
			return r.currentAssistant
		}
		r.currentAssistant = -1
	}
	rangeValue := r.segmentRanges[segmentOrdinal]
	r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
		ID:                      r.nextRecoveryMessageIDLocked("assistant"),
		Role:                    "assistant",
		Created:                 time.Now().UnixMilli(),
		ResponseID:              r.id,
		AssistantSegmentOrdinal: segmentOrdinal,
		SegmentStartSequence:    rangeValue.Start,
		SegmentEndSequence:      rangeValue.End,
	})
	r.currentAssistant = len(r.recoveryMessages) - 1
	return r.currentAssistant
}

func (r *responseRun) closeToolGroupLocked() {
	if r.currentToolGroup < 0 || r.currentToolGroup >= len(r.recoveryMessages) {
		return
	}
	group := &r.recoveryMessages[r.currentToolGroup]
	if group.Role != "tool-group" {
		r.currentToolGroup = -1
		return
	}
	for i := range group.Tools {
		group.Tools[i].Status = "done"
	}
	group.Status = "done"
	r.currentToolGroup = -1
}

func (r *responseRun) subscribe(after int64) responseRunSubscribeResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	if after < r.minReplayAfter {
		return responseRunSubscribeResult{
			status:           r.status,
			snapshotRequired: true,
			minReplayAfter:   r.minReplayAfter,
		}
	}

	replayEvents := r.activeEventsLocked()
	replay := make([]responseRunEvent, 0, len(replayEvents))
	for _, ev := range replayEvents {
		if ev.Sequence > after {
			replay = append(replay, ev)
		}
	}

	if r.status != "in_progress" {
		return responseRunSubscribeResult{replay: replay, status: r.status}
	}

	id := r.nextSubscriberID
	r.nextSubscriberID++
	ch := make(chan responseRunEvent, defaultResponseRunSubscriberBuffer)
	r.subscribers[id] = ch
	return responseRunSubscribeResult{id: id, replay: replay, ch: ch, status: r.status}
}

func (r *responseRun) subscriberWasDropped(id int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.subscriberDropped[id] {
		return false
	}
	delete(r.subscriberDropped, id)
	return true
}

func (r *responseRun) droppedSubscriberTerminalEvent() (responseRunEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	payload := map[string]any{
		"error": map[string]any{
			"type":    "stream_buffer_overflow",
			"message": "response event stream subscriber fell behind; reconnect using the recovery payload to resume",
		},
		"sequence_number":  r.lastSequenceNumber,
		"min_replay_after": r.minReplayAfter,
		"recovery":         r.recoveryPayloadLocked(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return responseRunEvent{}, err
	}
	return responseRunEvent{
		Sequence: r.lastSequenceNumber,
		Event:    "response.stream_error",
		Data:     data,
	}, nil
}

func (r *responseRun) unsubscribe(ch <-chan responseRunEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, existing := range r.subscribers {
		if existing == ch {
			// Terminal/buffer-overflow paths own channel closing.
			// Explicit unsubscribe only detaches the subscriber to avoid
			// coupling normal teardown to a specific close ordering.
			delete(r.subscribers, id)
			delete(r.subscriberWarned, id)
			return
		}
	}
}

func (r *responseRun) snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()

	payload := map[string]any{
		"id":                   r.id,
		"object":               "response",
		"created":              r.created,
		"started_at":           r.created * 1000,
		"model":                r.model,
		"status":               r.status,
		"session_id":           r.sessionID,
		"previous_response_id": r.previousResponseID,
		"last_sequence_number": r.lastSequenceNumber,
		"run_epoch":            r.runEpoch,
		"started_rev":          r.startedRev,
	}
	if r.clientMessageID != "" {
		payload["client_message_id"] = r.clientMessageID
	}
	if r.continuationResponseID != "" {
		payload["continuation_response_id"] = r.continuationResponseID
	}
	if r.anchorRowID > 0 {
		payload["anchor_row_id"] = r.anchorRowID
	}
	if r.status != "in_progress" {
		if r.endedAt > 0 {
			payload["ended_at"] = r.endedAt
		}
		payload["final_rev"] = r.finalRev
		payload["durable_handoff"] = r.durableHandoff
		payload["durable_output_count"] = r.durableOutputCount
		payload["handoff_compaction_seq"] = r.startedCompactionSeq
		payload["handoff_compaction_count"] = r.startedCompactionCount
		if r.durableHandoffErr != "" {
			payload["durable_handoff_error"] = r.durableHandoffErr
		}
	}
	if r.reasoningEffortSet {
		payload["reasoning_effort"] = r.reasoningEffort
	}
	if r.status == "completed" {
		payload["usage"] = usagePayload(r.usage)
		payload["session_usage"] = usagePayload(r.sessionUsage)
	}
	if r.errorMessage != "" {
		payload["error"] = map[string]any{
			"type":    r.errorType,
			"message": r.errorMessage,
		}
	}
	payload["recovery"] = r.recoveryPayloadLocked()
	return payload
}

func (r *responseRun) recoveryPayloadLocked() map[string]any {
	recovery := map[string]any{
		"sequence_number":  r.lastSequenceNumber,
		"min_replay_after": r.minReplayAfter,
	}
	if len(r.recoveryMessages) == 0 && len(r.recoveryEvents) == 0 && len(r.resolvedInteractions) == 0 {
		return recovery
	}

	messages := make([]map[string]any, 0, len(r.recoveryMessages))
	for _, msg := range r.recoveryMessages {
		responseID := msg.ResponseID
		if responseID == "" {
			responseID = r.id
		}
		entry := map[string]any{
			"id":          msg.ID,
			"role":        msg.Role,
			"created":     msg.Created,
			"responseId":  responseID,
			"response_id": responseID,
		}
		if msg.Role == "assistant" {
			entry["assistantSegmentOrdinal"] = msg.AssistantSegmentOrdinal
			entry["assistant_segment_ordinal"] = msg.AssistantSegmentOrdinal
			if msg.SegmentStartSequence > 0 {
				entry["segment_start_sequence"] = msg.SegmentStartSequence
			}
			if msg.SegmentEndSequence > 0 {
				entry["segment_end_sequence"] = msg.SegmentEndSequence
			}
		}
		if msg.Role == "compaction-boundary" {
			if msg.CompactionEventSequence > 0 {
				entry["compaction_sequence"] = msg.CompactionEventSequence
			}
			if msg.DurableCompactionSeq >= 0 {
				entry["compaction_seq"] = msg.DurableCompactionSeq
				entry["compaction_count"] = msg.CompactionCount
			}
		}
		if msg.Role == "model-swap" && msg.ModelSwap != nil {
			entry["event_sequence"] = msg.EventSequence
			entry["boundary_id"] = msg.ModelSwap.BoundaryID
			entry["from_provider"] = msg.ModelSwap.FromProvider
			entry["from_model"] = msg.ModelSwap.FromModel
			entry["from_reasoning_effort"] = msg.ModelSwap.FromEffort
			entry["to_provider"] = msg.ModelSwap.ToProvider
			entry["to_model"] = msg.ModelSwap.ToModel
			entry["to_reasoning_effort"] = msg.ModelSwap.ToEffort
			entry["swap_status"] = msg.ModelSwap.Status
			entry["swap_strategy"] = msg.ModelSwap.Strategy
		}
		if len(msg.Content) > 0 {
			entry["content"] = string(msg.Content)
		}
		if msg.Status != "" {
			entry["status"] = msg.Status
		}
		if msg.InterruptState != "" {
			entry["interruptState"] = msg.InterruptState
			entry["interrupt_state"] = msg.InterruptState
		}
		if msg.ClientMessageID != "" {
			entry["clientMessageId"] = msg.ClientMessageID
			entry["client_message_id"] = msg.ClientMessageID
		}
		if msg.Expanded {
			entry["expanded"] = msg.Expanded
		}
		if len(msg.Tools) > 0 {
			toolsPayload := make([]map[string]any, 0, len(msg.Tools))
			for _, tool := range msg.Tools {
				toolEntry := map[string]any{
					"id":      tool.ID,
					"name":    tool.Name,
					"status":  tool.Status,
					"created": tool.Created,
				}
				if tool.Arguments != "" {
					toolEntry["arguments"] = tool.Arguments
				}
				if tool.ArgumentsFinalized {
					toolEntry["argumentsFinalized"] = true
				}
				if tool.StartedAt > 0 {
					toolEntry["startedAt"] = tool.StartedAt
					durationMs := tool.DurationMs
					if durationMs == 0 && tool.EndedAt > 0 {
						durationMs = max(int64(0), tool.EndedAt-tool.StartedAt)
					} else if durationMs == 0 && tool.Status == "running" {
						durationMs = max(int64(0), time.Now().UnixMilli()-tool.StartedAt)
					}
					toolEntry["durationMs"] = durationMs
				}
				if tool.EndedAt > 0 {
					toolEntry["endedAt"] = tool.EndedAt
				}
				if tool.ResultStatus != "" {
					toolEntry["resultStatus"] = tool.ResultStatus
				}
				if tool.AskUserAnswer != "" {
					toolEntry["askUserAnswer"] = tool.AskUserAnswer
				}
				if len(tool.GuardianReviews) > 0 {
					reviews := make([]map[string]any, 0, len(tool.GuardianReviews))
					for _, review := range tool.GuardianReviews {
						reviews = append(reviews, cloneJSONMap(review))
					}
					toolEntry["guardianReviews"] = reviews
				}
				if len(tool.Images) > 0 {
					images := make([]string, len(tool.Images))
					copy(images, tool.Images)
					toolEntry["images"] = images
				}
				toolsPayload = append(toolsPayload, toolEntry)
			}
			entry["tools"] = toolsPayload
		}
		if len(msg.Attachments) > 0 {
			atts := make([]map[string]any, 0, len(msg.Attachments))
			for _, att := range msg.Attachments {
				atts = append(atts, cloneJSONMap(att))
			}
			entry["attachments"] = atts
		}
		if len(msg.Usage) > 0 {
			entry["usage"] = cloneJSONMap(msg.Usage)
		}
		messages = append(messages, entry)
	}
	recovery["messages"] = messages
	if len(r.recoveryEvents) > 0 {
		events := make([]map[string]any, 0, len(r.recoveryEvents))
		for _, ev := range r.recoveryEvents {
			entry := map[string]any{"event": ev.Event}
			if payload := cloneJSONMap(ev.Payload); len(payload) > 0 {
				entry["payload"] = payload
			}
			events = append(events, entry)
		}
		recovery["events"] = events
	}
	if len(r.resolvedInteractions) > 0 {
		resolved := make([]map[string]any, 0, len(r.resolvedInteractions))
		for key, record := range r.resolvedInteractions {
			kind, id, ok := strings.Cut(key, ":")
			if !ok || id == "" {
				continue
			}
			resolved = append(resolved, map[string]any{
				"kind": kind, "request_id": id, "outcome": record.Outcome, "resolved_at": record.ResolvedAt,
			})
		}
		sort.Slice(resolved, func(i, j int) bool {
			return responseRunInt64Value(resolved[i]["resolved_at"], 0) < responseRunInt64Value(resolved[j]["resolved_at"], 0)
		})
		if len(resolved) > 100 {
			resolved = resolved[len(resolved)-100:]
		}
		recovery["resolved_interactions"] = resolved
	}
	return recovery
}

func (r *responseRun) resolvedInteractionsSnapshot() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	resolved := make([]map[string]any, 0, len(r.resolvedInteractions))
	for key, record := range r.resolvedInteractions {
		kind, id, ok := strings.Cut(key, ":")
		if !ok || id == "" {
			continue
		}
		resolved = append(resolved, map[string]any{
			"kind": kind, "request_id": id, "outcome": record.Outcome, "resolved_at": record.ResolvedAt,
		})
	}
	sort.Slice(resolved, func(i, j int) bool {
		return responseRunInt64Value(resolved[i]["resolved_at"], 0) < responseRunInt64Value(resolved[j]["resolved_at"], 0)
	})
	if len(resolved) > 100 {
		resolved = resolved[len(resolved)-100:]
	}
	return resolved
}

func (r *responseRun) cancelPendingInteractions(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolvePendingInteractionsLocked(outcome)
}

func (r *responseRun) requestCancel() (context.CancelFunc, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status != "in_progress" {
		return nil, false
	}
	cancel := r.cancel
	if cancel == nil && !r.cancelRequested {
		return nil, false
	}
	r.cancelRequested = true
	r.cancel = nil
	return cancel, true
}

func (r *responseRun) cancelRun() bool {
	cancel, ok := r.requestCancel()
	if !ok {
		return false
	}
	if cancel != nil {
		cancel()
	}
	return true
}

func responseRunRecoveryEventMatches(ev responseRunRecoveryEvent, event, key, value string) bool {
	if ev.Event != event || key == "" || value == "" {
		return false
	}
	return stringValue(ev.Payload[key]) == value
}

func (r *responseRun) resolveRecoveryEvent(event, key, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.recoveryEvents) == 0 {
		return
	}
	kept := r.recoveryEvents[:0]
	for _, ev := range r.recoveryEvents {
		if responseRunRecoveryEventMatches(ev, event, key, value) {
			continue
		}
		kept = append(kept, ev)
	}
	for i := len(kept); i < len(r.recoveryEvents); i++ {
		r.recoveryEvents[i] = responseRunRecoveryEvent{}
	}
	r.recoveryEvents = kept
}

func (r *responseRun) resolveAskUserRecovery(callID string) {
	r.resolveRecoveryEvent("response.ask_user.prompt", "call_id", strings.TrimSpace(callID))
}

func (r *responseRun) resolveApprovalRecovery(approvalID string) {
	r.resolveRecoveryEvent("response.approval.prompt", "approval_id", strings.TrimSpace(approvalID))
}

func (r *responseRun) hasPendingInteraction(kind, id string) bool {
	if r == nil {
		return false
	}
	id = strings.TrimSpace(id)
	r.mu.Lock()
	defer r.mu.Unlock()
	promptEvent, idField := "response.ask_user.prompt", "call_id"
	if kind == "approval" {
		promptEvent, idField = "response.approval.prompt", "approval_id"
	}
	for _, pending := range r.recoveryEvents {
		if pending.Event == promptEvent && strings.TrimSpace(stringValue(pending.Payload[idField])) == id {
			return true
		}
	}
	return false
}

func (r *responseRun) resolvedInteraction(kind, id string) (responseRunResolvedInteraction, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.resolvedInteractions[kind+":"+strings.TrimSpace(id)]
	return value, ok
}

func (r *responseRun) recordResolvedInteraction(kind, id, outcome string) responseRunResolvedInteraction {
	id = strings.TrimSpace(id)
	key := kind + ":" + id
	r.mu.Lock()
	if existing, ok := r.resolvedInteractions[key]; ok {
		r.mu.Unlock()
		return existing
	}
	if r.resolvedInteractions == nil {
		r.resolvedInteractions = make(map[string]responseRunResolvedInteraction)
	}
	resolved := responseRunResolvedInteraction{Outcome: outcome, ResolvedAt: time.Now().UnixMilli()}
	r.resolvedInteractions[key] = resolved
	// Remove the actionable recovery prompt under the same lock as recording
	// the outcome. Terminalization uses this lock too, so it cannot overwrite a
	// successful decision while the prompt is still visible in recoveryEvents.
	promptEvent, idField := "response.ask_user.prompt", "call_id"
	if kind == "approval" {
		promptEvent, idField = "response.approval.prompt", "approval_id"
	}
	kept := r.recoveryEvents[:0]
	for _, pending := range r.recoveryEvents {
		if pending.Event == promptEvent && strings.TrimSpace(stringValue(pending.Payload[idField])) == id {
			continue
		}
		kept = append(kept, pending)
	}
	for i := len(kept); i < len(r.recoveryEvents); i++ {
		r.recoveryEvents[i] = responseRunRecoveryEvent{}
	}
	r.recoveryEvents = kept
	// Keep this bounded to the live response recovery window, evicting the
	// oldest decision rather than a random still-relevant map entry.
	if len(r.resolvedInteractions) > 128 {
		oldestKey, oldestAt := "", int64(0)
		for candidate, value := range r.resolvedInteractions {
			if candidate == key {
				continue
			}
			if oldestKey == "" || value.ResolvedAt < oldestAt {
				oldestKey, oldestAt = candidate, value.ResolvedAt
			}
		}
		delete(r.resolvedInteractions, oldestKey)
	}
	r.mu.Unlock()

	payload := map[string]any{"outcome": resolved.Outcome, "resolved_at": resolved.ResolvedAt}
	if kind == "approval" {
		payload["approval_id"] = id
	} else {
		payload["call_id"] = id
	}
	_ = r.appendEvent("response."+kind+".resolved", payload)
	return resolved
}

func (m *responseRunManager) resolvedInteractionForSession(sessionID, kind, id string) (responseRunResolvedInteraction, bool) {
	if m == nil {
		return responseRunResolvedInteraction{}, false
	}
	m.mu.Lock()
	runs := make([]*responseRun, 0, len(m.runs))
	for _, run := range m.runs {
		if run != nil && run.sessionID == sessionID {
			runs = append(runs, run)
		}
	}
	m.mu.Unlock()
	for _, run := range runs {
		if resolved, ok := run.resolvedInteraction(kind, id); ok {
			return resolved, true
		}
	}
	return responseRunResolvedInteraction{}, false
}

func (m *responseRunManager) activeRun(sessionID string) *responseRun {
	if m == nil {
		return nil
	}
	if id := m.activeRunID(sessionID); id != "" {
		if run, ok := m.get(id); ok {
			return run
		}
	}
	return nil
}

func (m *responseRunManager) latestRun(sessionID string) *responseRun {
	if m == nil {
		return nil
	}
	if run := m.activeRun(sessionID); run != nil {
		return run
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var latest *responseRun
	for _, run := range m.runs {
		if run != nil && run.sessionID == sessionID && (latest == nil || run.created > latest.created) {
			latest = run
		}
	}
	return latest
}

type responseRunIdempotencyClaim struct {
	runID       string
	sessionID   string
	fingerprint string
}

type responseRunManager struct {
	mu                 sync.Mutex
	runs               map[string]*responseRun
	activeBySession    map[string]string
	idempotencyByKey   map[string]responseRunIdempotencyClaim
	cleanupTimers      map[string]*time.Timer
	nextEpochBySession map[string]int64
	terminalRetention  time.Duration
	runWG              sync.WaitGroup
	closed             bool
	boundaries         sync.Map // map[session ID]*sync.Mutex
	idempotencyReplays atomic.Uint64
}

type responseRunDiagnostics struct {
	IdempotencyReplays uint64 `json:"idempotency_replays"`
}

func (m *responseRunManager) Diagnostics() responseRunDiagnostics {
	if m == nil {
		return responseRunDiagnostics{}
	}
	return responseRunDiagnostics{IdempotencyReplays: m.idempotencyReplays.Load()}
}

const (
	defaultResponseRunRetention        = 5 * time.Minute
	defaultResponseRunReplayLimit      = 2048
	defaultResponseRunSubscriberBuffer = 256
	defaultServeRequestTimeout         = 30 * time.Minute
)

var (
	errResponseRunTimeout     = errors.New("response run timeout")
	errResponseRunKeyConflict = errors.New("idempotency key was already used with a different request")
)

type responseRunTimerHandle interface {
	Stop() bool
}

type responseRunClock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) responseRunTimerHandle
}

type realResponseRunClock struct{}

func (realResponseRunClock) Now() time.Time { return time.Now() }

func (realResponseRunClock) AfterFunc(delay time.Duration, fn func()) responseRunTimerHandle {
	return time.AfterFunc(delay, fn)
}

// responseRunTimer bounds inactivity between the user request and each completed
// LLM response. Interactive waits pause the current inactivity window.
type responseRunTimer struct {
	mu         sync.Mutex
	cancel     context.CancelCauseFunc
	timer      responseRunTimerHandle
	clock      responseRunClock
	timeout    time.Duration
	remaining  time.Duration
	activeAt   time.Time
	generation uint64
	pauses     int
	stopped    bool
}

func newResponseRunTimer(timeout time.Duration) (context.Context, *responseRunTimer) {
	return newResponseRunTimerWithClock(timeout, realResponseRunClock{})
}

func newResponseRunTimerWithClock(timeout time.Duration, clock responseRunClock) (context.Context, *responseRunTimer) {
	ctx, cancel := context.WithCancelCause(context.Background())
	t := &responseRunTimer{
		cancel:    cancel,
		clock:     clock,
		timeout:   timeout,
		remaining: timeout,
	}
	t.mu.Lock()
	t.armLocked(timeout)
	t.mu.Unlock()
	return ctx, t
}

func (t *responseRunTimer) armLocked(remaining time.Duration) {
	t.generation++
	generation := t.generation
	t.activeAt = t.clock.Now()
	t.timer = t.clock.AfterFunc(remaining, func() {
		t.mu.Lock()
		if t.stopped || t.pauses > 0 || t.generation != generation {
			t.mu.Unlock()
			return
		}
		t.remaining = 0
		t.stopped = true
		t.mu.Unlock()
		t.cancel(errResponseRunTimeout)
	})
}

// refresh starts a fresh inactivity window after an LLM response completes.
func (t *responseRunTimer) refresh() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	if t.timer != nil {
		t.timer.Stop()
	}
	t.generation++ // Invalidate a callback already leaving the stopped timer.
	t.remaining = t.timeout
	if t.pauses == 0 {
		t.armLocked(t.remaining)
	}
}

func (t *responseRunTimer) pause() func() {
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return func() {}
	}
	t.pauses++
	expired := false
	if t.pauses == 1 {
		if t.timer != nil {
			t.timer.Stop()
		}
		t.generation++ // Invalidate a callback already leaving the stopped timer.
		t.remaining -= t.clock.Now().Sub(t.activeAt)
		if t.remaining <= 0 {
			t.remaining = 0
			t.pauses = 0
			t.stopped = true
			expired = true
		}
	}
	t.mu.Unlock()
	if expired {
		t.cancel(errResponseRunTimeout)
		return func() {}
	}

	var once sync.Once
	return func() {
		once.Do(t.resume)
	}
}

func (t *responseRunTimer) resume() {
	t.mu.Lock()
	if t.stopped || t.pauses == 0 {
		t.mu.Unlock()
		return
	}
	t.pauses--
	if t.pauses > 0 {
		t.mu.Unlock()
		return
	}
	remaining := t.remaining
	if remaining > 0 {
		t.armLocked(remaining)
	}
	t.mu.Unlock()
	if remaining <= 0 {
		t.cancel(errResponseRunTimeout)
	}
}

func (t *responseRunTimer) stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	t.generation++
	if t.timer != nil {
		t.timer.Stop()
	}
	t.mu.Unlock()
	t.cancel(nil)
}

func responseRunTimedOut(ctx context.Context) bool {
	return ctx != nil && errors.Is(context.Cause(ctx), errResponseRunTimeout)
}

func responseRunTimeoutMessage(timeout time.Duration) string {
	return fmt.Sprintf("Timed out because no LLM response completed within %s. Continue to resume from saved progress, or move long-running investigations to a background job.", humanDuration(timeout))
}

func responseRunDeadlineMessage(runCtx context.Context, timeout time.Duration) string {
	if responseRunTimedOut(runCtx) || (runCtx != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded)) {
		return responseRunTimeoutMessage(timeout)
	}
	return "The model provider request timed out before the response run deadline. Continue to retry from saved progress."
}

func humanDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		hours := int(d / time.Hour)
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	}
	if d%time.Minute == 0 {
		minutes := int(d / time.Minute)
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}
	return d.String()
}

func newServeResponseRunManager() *responseRunManager {
	return newServeResponseRunManagerWithRetention(defaultResponseRunRetention)
}

func newServeResponseRunManagerWithRetention(retention time.Duration) *responseRunManager {
	return &responseRunManager{
		runs:               make(map[string]*responseRun),
		activeBySession:    make(map[string]string),
		idempotencyByKey:   make(map[string]responseRunIdempotencyClaim),
		cleanupTimers:      make(map[string]*time.Timer),
		nextEpochBySession: make(map[string]int64),
		terminalRetention:  retention,
	}
}

func (s *serveServer) responseOwnerID() string {
	s.responseOwnerOnce.Do(func() {
		s.responseOwnerInstanceID = "owner_" + randomSuffix()
	})
	return s.responseOwnerInstanceID
}

func (s *serveServer) ensureResponseRuns() *responseRunManager {
	s.responseRunsOnce.Do(func() {
		if s.responseRuns == nil {
			s.responseRuns = newServeResponseRunManager()
		}
	})
	s.responseOwnerID()
	if s.shutdownCh != nil {
		s.startResponseLifecycle()
	}
	return s.responseRuns
}

func (s *serveServer) startResponseLifecycle() {
	s.responseLifecycleOnce.Do(func() {
		lifecycle, ok := session.AsServeResponseLifecycleStore(s.store)
		if !ok {
			return
		}
		ownerID := s.responseOwnerID()
		ctx, cancel := context.WithCancel(context.Background())
		s.responseLifecycleCancel = cancel
		s.responseLifecycleWG.Add(1)
		go func() {
			defer s.responseLifecycleWG.Done()
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			s.sweepAndRenewResponseLifecycle(ctx, lifecycle, ownerID)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.sweepAndRenewResponseLifecycle(ctx, lifecycle, ownerID)
				}
			}
		}()
	})
}

func (s *serveServer) sweepAndRenewResponseLifecycle(ctx context.Context, lifecycle session.ServeResponseLifecycleStore, processOwnerID string) {
	interactionStore, interactionProjectionSupported := session.AsResponseRunInteractionStore(s.store)
	type ownedRun struct {
		run                 *responseRun
		responseID, ownerID string
		token               int64
		leaseExpiresAt      time.Time
		cancel              context.CancelFunc
		interactionState    session.ResponseRunInteractionState
	}
	owned := make([]ownedRun, 0)
	skipRecovery := false
	if s.responseRuns != nil {
		s.responseRuns.mu.Lock()
		runs := make([]*responseRun, 0, len(s.responseRuns.runs))
		for _, run := range s.responseRuns.runs {
			if run != nil {
				runs = append(runs, run)
			}
		}
		s.responseRuns.mu.Unlock()
		for _, run := range runs {
			// Terminalization may briefly own run.mu while committing its durable
			// marker. Skipping that run for one 10s pass is safer than allowing one
			// slow finalizer to delay every other lease renewal.
			if !run.mu.TryLock() {
				skipRecovery = true
				continue
			}
			if run.ownerInstanceID == processOwnerID && run.fencingToken > 0 && run.status == "in_progress" {
				owned = append(owned, ownedRun{run: run, responseID: run.id, ownerID: run.ownerInstanceID,
					token: run.fencingToken, leaseExpiresAt: run.leaseExpiresAt, cancel: run.cancel,
					interactionState: run.interactionStateLocked()})
			}
			run.mu.Unlock()
		}
	}
	semaphore := make(chan struct{}, 32)
	var renewWG sync.WaitGroup
	for _, candidate := range owned {
		candidate := candidate
		renewWG.Add(1)
		go func() {
			defer renewWG.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-semaphore }()
			renewCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			lease, err := lifecycle.RenewResponseRunLease(renewCtx, candidate.responseID, candidate.ownerID, candidate.token)
			cancel()
			if err != nil {
				s.attentionDiagnostics.LeaseRenewFailures.Add(1)
				log.Printf("[serve] response lifecycle lease renewal failed for %.12s: %v", candidate.responseID, err)
				if candidate.cancel != nil && (errors.Is(err, session.ErrResponseRunLeaseLost) || time.Until(candidate.leaseExpiresAt) <= 12*time.Second) {
					candidate.cancel()
				}
				return
			}
			candidate.run.mu.Lock()
			candidate.run.leaseExpiresAt = lease.LeaseExpiresAt
			candidate.run.mu.Unlock()
			if interactionProjectionSupported {
				projectionCtx, projectionCancel := context.WithTimeout(ctx, 5*time.Second)
				err := interactionStore.SetResponseRunInteractionState(projectionCtx, candidate.interactionState)
				projectionCancel()
				if err != nil && !errors.Is(err, session.ErrResponseRunLeaseLost) {
					log.Printf("[serve] response interaction reconciliation failed for %.12s: %v", candidate.responseID, err)
				}
			}
		}()
	}
	renewWG.Wait()
	// A contended run may be inside its terminal transaction. Do not let this
	// process's sweeper orphan it merely because renewal intentionally used
	// TryLock; the next pass will recover genuinely abandoned rows.
	if skipRecovery {
		return
	}
	recoverCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	recovered, err := lifecycle.RecoverExpiredResponseRuns(recoverCtx, 100)
	cancel()
	if len(recovered) > 0 {
		s.attentionDiagnostics.OrphanRecoveries.Add(uint64(len(recovered)))
		s.attentionDiagnostics.MarkerWrites.Add(uint64(len(recovered)))
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("[serve] response lifecycle orphan sweep failed: %v", err)
	}
}

func responseRunIdempotencyScope(sessionID, key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return key
	}
	return sessionID + "\x00" + key
}

func (m *responseRunManager) create(run *responseRun) error {
	_, duplicate, err := m.createOrGetByIdempotency(run, "")
	if duplicate {
		return fmt.Errorf("response run %q already exists", run.id)
	}
	return err
}

func responseRunClaimMatches(claim responseRunIdempotencyClaim, fingerprint string) bool {
	fingerprint = strings.TrimSpace(fingerprint)
	return fingerprint == "" || claim.fingerprint == "" || claim.fingerprint == fingerprint
}

func (m *responseRunManager) createOrGetByIdempotency(run *responseRun, idempotencyKey string) (*responseRun, bool, error) {
	if run == nil || strings.TrimSpace(run.id) == "" {
		return nil, false, fmt.Errorf("response run id is required")
	}
	scope := strings.TrimSpace(run.idempotencyScope)
	if scope == "" {
		scope = run.sessionID
	}
	key := responseRunIdempotencyScope(scope, idempotencyKey)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false, fmt.Errorf("server is shutting down")
	}
	if key != "" {
		if claim, ok := m.idempotencyByKey[key]; ok {
			if !responseRunClaimMatches(claim, run.requestFingerprint) {
				return nil, false, errResponseRunKeyConflict
			}
			if existing, exists := m.runs[claim.runID]; exists && existing != nil {
				m.idempotencyReplays.Add(1)
				return existing, true, nil
			}
			if claim.runID != "" {
				delete(m.idempotencyByKey, key)
			}
		}
	}
	if _, exists := m.runs[run.id]; exists {
		return nil, false, fmt.Errorf("response run %q already exists", run.id)
	}
	previousEpoch := m.nextEpochBySession[run.sessionID]
	nextEpoch := previousEpoch + 1
	if now := time.Now().UnixMicro(); nextEpoch < now {
		nextEpoch = now
	}
	m.nextEpochBySession[run.sessionID] = nextEpoch
	run.runEpoch = nextEpoch
	m.runs[run.id] = run
	if key != "" {
		m.idempotencyByKey[key] = responseRunIdempotencyClaim{runID: run.id, sessionID: run.sessionID, fingerprint: strings.TrimSpace(run.requestFingerprint)}
	}
	return run, false, nil
}

func (m *responseRunManager) reserveSessionForIdempotency(scope, idempotencyKey, fingerprint string) (string, error) {
	key := responseRunIdempotencyScope(scope, idempotencyKey)
	if key == "" {
		return "", fmt.Errorf("idempotency scope and key are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", fmt.Errorf("server is shutting down")
	}
	if claim, ok := m.idempotencyByKey[key]; ok {
		if !responseRunClaimMatches(claim, fingerprint) {
			return "", errResponseRunKeyConflict
		}
		if claim.sessionID != "" {
			return claim.sessionID, nil
		}
	}
	sessionID := session.NewID()
	m.idempotencyByKey[key] = responseRunIdempotencyClaim{
		sessionID:   sessionID,
		fingerprint: strings.TrimSpace(fingerprint),
	}
	return sessionID, nil
}

func (m *responseRunManager) getByIdempotencyKey(sessionID, idempotencyKey string) (*responseRun, bool) {
	run, found, _ := m.getByIdempotencyClaim(sessionID, idempotencyKey, "")
	return run, found
}

func (m *responseRunManager) getByIdempotencyClaim(scope, idempotencyKey, fingerprint string) (*responseRun, bool, error) {
	key := responseRunIdempotencyScope(scope, idempotencyKey)
	if key == "" {
		return nil, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	claim, ok := m.idempotencyByKey[key]
	if !ok {
		return nil, false, nil
	}
	if !responseRunClaimMatches(claim, fingerprint) {
		return nil, false, errResponseRunKeyConflict
	}
	if strings.TrimSpace(claim.runID) == "" {
		return nil, false, nil
	}
	run, ok := m.runs[claim.runID]
	if !ok || run == nil {
		delete(m.idempotencyByKey, key)
		return nil, false, nil
	}
	m.idempotencyReplays.Add(1)
	return run, true, nil
}

func (m *responseRunManager) start(fn func()) error {
	if fn == nil {
		return fmt.Errorf("response run function is required")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("server is shutting down")
	}
	m.runWG.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.runWG.Done()
		fn()
	}()
	return nil
}

func (m *responseRunManager) delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if timer, ok := m.cleanupTimers[id]; ok {
		timer.Stop()
		delete(m.cleanupTimers, id)
	}
	delete(m.runs, id)
	for key, claim := range m.idempotencyByKey {
		if claim.runID == id {
			delete(m.idempotencyByKey, key)
		}
	}
	for sessionID, activeID := range m.activeBySession {
		if activeID == id {
			delete(m.activeBySession, sessionID)
		}
	}
}

func (m *responseRunManager) scheduleCleanup(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.runs[id]; !ok {
		return
	}
	if timer, ok := m.cleanupTimers[id]; ok {
		timer.Stop()
		delete(m.cleanupTimers, id)
	}

	if m.closed || m.terminalRetention <= 0 {
		delete(m.runs, id)
		for key, claim := range m.idempotencyByKey {
			if claim.runID == id {
				delete(m.idempotencyByKey, key)
			}
		}
		for sessionID, activeID := range m.activeBySession {
			if activeID == id {
				delete(m.activeBySession, sessionID)
			}
		}
		return
	}

	m.cleanupTimers[id] = time.AfterFunc(m.terminalRetention, func() {
		m.delete(id)
	})
}

func (m *responseRunManager) get(id string) (*responseRun, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[id]
	return run, ok
}

func (m *responseRunManager) sessionBoundary(sessionID string) *sync.Mutex {
	boundary, _ := m.boundaries.LoadOrStore(sessionID, &sync.Mutex{})
	return boundary.(*sync.Mutex)
}

func (m *responseRunManager) trySetActiveRun(sessionID, runID string) bool {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(runID) == "" {
		return false
	}
	boundary := m.sessionBoundary(sessionID)
	boundary.Lock()
	defer boundary.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if active := m.activeBySession[sessionID]; active != "" && active != runID {
		return false
	}
	m.activeBySession[sessionID] = runID
	return true
}

func (m *responseRunManager) setActiveRun(sessionID, runID string) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(runID) == "" {
		return
	}
	boundary := m.sessionBoundary(sessionID)
	boundary.Lock()
	defer boundary.Unlock()
	m.mu.Lock()
	m.activeBySession[sessionID] = runID
	m.mu.Unlock()
}

func (m *responseRunManager) clearActiveRun(sessionID, runID string) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(runID) == "" {
		return
	}
	boundary := m.sessionBoundary(sessionID)
	boundary.Lock()
	defer boundary.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeBySession[sessionID] == runID {
		delete(m.activeBySession, sessionID)
	}
}

func (m *responseRunManager) withExpectedActiveRun(sessionID, expectedID string, expectedEpoch int64, fn func()) bool {
	if m == nil || strings.TrimSpace(sessionID) == "" || fn == nil {
		return false
	}
	boundary := m.sessionBoundary(sessionID)
	boundary.Lock()
	defer boundary.Unlock()
	m.mu.Lock()
	activeID := m.activeBySession[sessionID]
	run := m.runs[activeID]
	matches := strings.TrimSpace(expectedID) == "" || activeID == strings.TrimSpace(expectedID)
	if matches && expectedEpoch > 0 {
		matches = run != nil && run.runEpoch == expectedEpoch
	}
	m.mu.Unlock()
	if !matches {
		return false
	}
	fn()
	return true
}

func (m *responseRunManager) activeRunID(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeBySession[sessionID]
}

func (m *responseRunManager) runIfSessionIdle(sessionID string, fn func()) bool {
	if strings.TrimSpace(sessionID) == "" || fn == nil {
		return false
	}
	// Serialize only work for this session. The manager mutex protects the
	// activity map briefly and is never held across persistence or other I/O.
	boundary := m.sessionBoundary(sessionID)
	boundary.Lock()
	defer boundary.Unlock()
	m.mu.Lock()
	active := m.activeBySession[sessionID] != ""
	m.mu.Unlock()
	if active {
		return false
	}
	fn()
	return true
}

// ActiveSessionIDs returns session IDs that currently have an active
// response run. Does not touch any runtime TTLs.
func (m *responseRunManager) ActiveSessionIDs() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]bool, len(m.activeBySession))
	for sid := range m.activeBySession {
		result[sid] = true
	}
	return result
}

func (m *responseRunManager) Close() {
	m.CloseContext(context.Background())
}

func (m *responseRunManager) CloseContext(ctx context.Context) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	runs := make([]*responseRun, 0, len(m.runs))
	for _, run := range m.runs {
		runs = append(runs, run)
	}
	for id, timer := range m.cleanupTimers {
		timer.Stop()
		delete(m.cleanupTimers, id)
	}
	m.mu.Unlock()

	for _, run := range runs {
		run.cancelPendingInteractions("cancelled-by-agent")
		_ = run.cancelRun()
	}
	waitDone := make(chan struct{})
	go func() {
		m.runWG.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-ctx.Done():
	}
}

func usagePayload(usage llm.Usage) map[string]any {
	return map[string]any{
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
		"total_tokens":  usage.InputTokens + usage.CachedInputTokens + usage.CacheWriteTokens + usage.OutputTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens":      usage.CachedInputTokens,
			"cache_write_tokens": usage.CacheWriteTokens,
		},
	}
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

// appendJSONString appends a JSON-encoded string to dst without allocating a
// separate []byte (unlike json.Marshal). Handles all characters that require
// escaping in JSON strings; non-ASCII UTF-8 bytes pass through unchanged.
func appendJSONString(dst []byte, s string) []byte {
	const hexChars = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 0x20 && b != '"' && b != '\\' {
			continue
		}
		dst = append(dst, s[start:i]...)
		start = i + 1
		switch b {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexChars[b>>4], hexChars[b&0xf])
		}
	}
	dst = append(dst, s[start:]...)
	dst = append(dst, '"')
	return dst
}

func mapValue(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func stringSliceValue(v any) []string {
	switch values := v.(type) {
	case []string:
		out := make([]string, len(values))
		copy(out, values)
		return out
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if s, ok := value.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func appendUniqueStrings(dst []string, values ...string) []string {
	for _, value := range values {
		if value == "" {
			continue
		}
		seen := false
		for _, existing := range dst {
			if existing == value {
				seen = true
				break
			}
		}
		if !seen {
			dst = append(dst, value)
		}
	}
	return dst
}

func cloneJSONMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(src))
	for key, value := range src {
		cloned[key] = cloneJSONValue(value)
	}
	return cloned
}

func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneJSONMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = cloneJSONValue(typed[i])
		}
		return out
	default:
		return typed
	}
}

func writeStoredResponseEvent(w io.Writer, ev responseRunEvent) error {
	b := make([]byte, 0, 4+20+1+7+len(ev.Event)+1+6+len(ev.Data)+2)
	b = append(b, "id: "...)
	b = strconv.AppendInt(b, ev.Sequence, 10)
	b = append(b, "\nevent: "...)
	b = append(b, ev.Event...)
	b = append(b, "\ndata: "...)
	b = append(b, ev.Data...)
	b = append(b, "\n\n"...)
	_, err := w.Write(b)
	return err
}

type responseRunStreamState struct {
	outputIndex              int
	toolsSeen                bool
	assistantBoundaryPending bool
	assistantSegmentOrdinal  int
	model                    string
	reasoningEffort          string
	reasoningEffortSet       bool
	toolStartedAt            map[string]int64
}

func newResponseRunStreamState(model, reasoningEffort string) *responseRunStreamState {
	effort := strings.TrimSpace(reasoningEffort)
	return &responseRunStreamState{
		model:              strings.TrimSpace(model),
		reasoningEffort:    effort,
		reasoningEffortSet: effort != "",
		toolStartedAt:      make(map[string]int64),
	}
}

func (s *responseRunStreamState) appliedModel(fallback string) string {
	if s != nil && strings.TrimSpace(s.model) != "" {
		return strings.TrimSpace(s.model)
	}
	return strings.TrimSpace(fallback)
}

func (s *responseRunStreamState) appliedReasoningEffort(fallback string) (string, bool) {
	if s != nil && s.reasoningEffortSet {
		return strings.TrimSpace(s.reasoningEffort), true
	}
	fallback = strings.TrimSpace(fallback)
	return fallback, fallback != ""
}

func (s *serveServer) toolImageURLs(imagePaths []string) []string {
	if len(imagePaths) == 0 {
		return nil
	}
	imageURLs := make([]string, 0, len(imagePaths))
	for _, imgPath := range imagePaths {
		if s.cfg.filesDir != "" {
			if served, ok := s.ensureFileServeable(imgPath); ok {
				imageURLs = append(imageURLs, serveRoutePath(s.cfg.filesRoute(), s.cfg.filesDir, served))
			}
			continue
		}
		if served, ok := s.ensureImageServeable(imgPath); ok {
			imageURLs = append(imageURLs, serveRoutePath(s.cfg.imagesRoute(), s.imageOutputDir(), served))
		}
	}
	return imageURLs
}

func (s *serveServer) suppressResponseRunServerToolEvent(runtime *serveRuntime, toolName string) bool {
	return s != nil && s.cfg.suppressServerTools && runtime != nil && runtime.isServerExecutedTool(toolName)
}

func (s *serveServer) persistResponseRunErrorEvent(ctx context.Context, runtime *serveRuntime, sessionID, respID, errType, errMessage string) {
	if runtime == nil || runtime.store == nil || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(errMessage) == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	msg := llm.RunErrorEventMessage(llm.RunErrorMarker{
		ResponseID: respID,
		ErrorType:  errType,
		Message:    errMessage,
	})
	msg.ResponseID = respID
	_, err := runResponseRunPersistence(ctx, []llm.Message{msg}, func(fence session.ResponseRunFence) (int64, error) {
		return addResponseRunMessage(session.WithResponseRunFence(dbCtx, fence), runtime.store, sessionID, session.NewMessage(sessionID, msg, -1))
	})
	if err != nil {
		log.Printf("[serve] persist response run error event failed for %s: %v", sessionID, err)
	}
}

func (s *serveServer) appendResponseRunEvent(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	switch ev.Type {
	case llm.EventTextDelta:
		segmentOrdinal := state.assistantSegmentOrdinal
		if ev.ProviderTurnIndexSet && ev.ProviderTurnIndex > segmentOrdinal {
			// Durable assistant rows are keyed by the engine's provider turn index.
			// Tool-only turns emit no text, so counting only text-after-tool
			// boundaries makes the live projection lag those durable identities.
			segmentOrdinal = ev.ProviderTurnIndex
		} else if state.toolsSeen || state.assistantBoundaryPending {
			// Preserve an ordered boundary for inline tools and interjections that
			// continue within one provider turn.
			segmentOrdinal++
		}
		if segmentOrdinal != state.assistantSegmentOrdinal {
			state.assistantSegmentOrdinal = segmentOrdinal
			if err := run.appendEvent("response.output_text.new_segment", map[string]any{
				"output_index":              state.outputIndex,
				"assistant_segment_ordinal": state.assistantSegmentOrdinal,
			}); err != nil {
				return err
			}
			state.toolsSeen = false
			state.assistantBoundaryPending = false
		}
		return run.appendTextDeltaSegmentEvent(state.outputIndex, state.assistantSegmentOrdinal, ev.Text)
	case llm.EventAttemptDiscard:
		state.toolsSeen = false
		return run.appendEvent("response.attempt.discard", map[string]any{
			"output_index": state.outputIndex,
		})
	case llm.EventToolCall:
		if ev.Tool == nil {
			return nil
		}
		// Suppress tool call metadata for server-executed tools in API mode, but
		// retain the assistant boundary so persisted callback turn ordinals and
		// streamed segment identities remain aligned end to end.
		if s.suppressResponseRunServerToolEvent(runtime, ev.Tool.Name) {
			state.toolsSeen = true
			return nil
		}
		state.toolsSeen = true
		itemID := "fc_" + ev.Tool.ID
		item := map[string]any{
			"id":        itemID,
			"type":      "function_call",
			"call_id":   ev.Tool.ID,
			"name":      ev.Tool.Name,
			"arguments": string(ev.Tool.Arguments),
		}
		identity := map[string]any{
			"output_index":              state.outputIndex,
			"assistant_segment_ordinal": state.assistantSegmentOrdinal,
			"call_id":                   ev.Tool.ID,
			"item_id":                   itemID,
		}
		if ev.ProviderTurnIndexSet {
			identity["provider_turn_index"] = ev.ProviderTurnIndex
		}
		added := maps.Clone(identity)
		added["item"] = item
		if err := run.appendEvent("response.output_item.added", added); err != nil {
			return err
		}
		delta := maps.Clone(identity)
		delta["delta"] = string(ev.Tool.Arguments)
		if err := run.appendEvent("response.function_call_arguments.delta", delta); err != nil {
			return err
		}
		done := maps.Clone(identity)
		done["item"] = item
		if err := run.appendEvent("response.output_item.done", done); err != nil {
			return err
		}
		state.outputIndex++
		return nil
	case llm.EventToolExecStart:
		// ask_user is a user-facing control event, not tool metadata. Emit its
		// prompt even when server-executed tool details are hidden; otherwise the
		// live web stream stalls until a reload recovers the pending prompt.
		if ev.ToolName == tools.AskUserToolName && runtime != nil {
			if prompt, err := runtime.prepareAskUserFromToolArgs(ev.ToolCallID, ev.ToolArgs); err == nil {
				if err := run.appendEvent("response.ask_user.prompt", map[string]any{
					"call_id":    prompt.CallID,
					"questions":  prompt.Questions,
					"created_at": prompt.CreatedAt,
				}); err != nil {
					return err
				}
			}
		}
		if s.suppressResponseRunServerToolEvent(runtime, ev.ToolName) {
			return nil
		}
		startedAt := time.Now().UnixMilli()
		if state.toolStartedAt == nil {
			state.toolStartedAt = make(map[string]int64)
		}
		if previous := state.toolStartedAt[ev.ToolCallID]; previous > 0 {
			startedAt = previous
		} else {
			state.toolStartedAt[ev.ToolCallID] = startedAt
		}
		return run.appendEvent("response.tool_exec.start", map[string]any{
			"call_id":        ev.ToolCallID,
			"tool_name":      ev.ToolName,
			"tool_info":      ev.ToolInfo,
			"tool_arguments": string(ev.ToolArgs),
			"started_at":     startedAt,
		})
	case llm.EventToolExecEnd:
		if ev.ToolName == tools.AskUserToolName && runtime != nil {
			runtime.clearPendingAskUser(ev.ToolCallID)
		}
		suppressed := s.suppressResponseRunServerToolEvent(runtime, ev.ToolName)
		if suppressed && ev.ToolName != tools.AskUserToolName {
			return nil
		}
		payload := map[string]any{
			"call_id":   ev.ToolCallID,
			"tool_name": ev.ToolName,
			"success":   ev.ToolSuccess,
		}
		if startedAt := state.toolStartedAt[ev.ToolCallID]; startedAt > 0 {
			endedAt := time.Now().UnixMilli()
			payload["started_at"] = startedAt
			payload["ended_at"] = endedAt
			payload["duration_ms"] = max(int64(0), endedAt-startedAt)
			delete(state.toolStartedAt, ev.ToolCallID)
		}
		if !suppressed && ev.ToolInfo != "" {
			payload["tool_info"] = ev.ToolInfo
		}
		if !suppressed && len(ev.ToolArgs) > 0 {
			payload["tool_arguments"] = string(ev.ToolArgs)
		}
		if ev.ToolSuccess && ev.ToolName == tools.AskUserToolName {
			if summary := askUserResultSummary(ev.ToolOutput); summary != "" {
				payload["ask_user_summary"] = summary
			}
		}
		if !suppressed && len(ev.ToolImages) > 0 {
			if imageURLs := s.toolImageURLs(ev.ToolImages); len(imageURLs) > 0 {
				payload["images"] = imageURLs
			}
		}
		if err := run.appendEvent("response.tool_exec.end", payload); err != nil {
			return err
		}
		if suppressed {
			return nil
		}
		// Metadata only — diff content is served by the session
		// file-changes endpoints on demand.
		for _, fc := range ev.ToolFileChanges {
			if !fc.TrustedPersisted {
				if err := run.appendEvent("response.output_claim_diagnostic", map[string]any{
					"reason": "metadata_unverified", "coverage_status": "unavailable",
					"message": "unverified tool file metadata was not attributed", "tool_call_id": ev.ToolCallID,
				}); err != nil {
					return err
				}
				continue
			}
			if err := run.appendEvent("response.file_change", map[string]any{
				"path": fc.Path, "kind": fc.Kind, "adds": fc.Adds, "dels": fc.Dels,
				"seq": fc.Seq, "event_seq": fc.EventSeq, "truncated": fc.Truncated,
				"provenance": fc.Provenance, "provenances": fc.Provenances,
				"baseline_state": fc.BaselineState, "content_status": fc.ContentStatus,
				"content_available": fc.ContentAvailable, "claim_coverage": fc.ClaimCoverage,
				"tool_call_id": ev.ToolCallID,
			}); err != nil {
				return err
			}
		}
		for _, observation := range ev.ToolFilesystemObservations {
			if err := run.appendEvent("response.filesystem_observation", map[string]any{
				"id": observation.ID, "classification": observation.Classification, "root": observation.Root,
				"created_count": observation.CreatedCount, "modified_count": observation.ModifiedCount, "deleted_count": observation.DeletedCount,
				"sampled_paths": observation.SampledPaths, "samples_truncated": observation.SamplesTruncated,
				"coverage_status": observation.CoverageStatus, "event_seq": observation.EventSeq,
			}); err != nil {
				return err
			}
		}
		for _, diagnostic := range ev.ToolOutputClaimDiagnostics {
			if err := run.appendEvent("response.output_claim_diagnostic", map[string]any{
				"normalized_pattern": diagnostic.NormalizedPattern, "claim_kind": diagnostic.ClaimKind,
				"reason": diagnostic.Reason, "coverage_status": diagnostic.CoverageStatus,
				"matching_path_count": diagnostic.MatchingPathCount, "message": diagnostic.Message,
			}); err != nil {
				return err
			}
		}
		return nil
	case llm.EventHeartbeat:
		return run.appendEvent("response.heartbeat", map[string]any{
			"call_id":   ev.ToolCallID,
			"tool_name": ev.ToolName,
		})
	case llm.EventPhase:
		if ev.Text == "" {
			return nil
		}
		return run.appendEvent("response.phase", map[string]any{
			"text": ev.Text,
		})
	case llm.EventCompaction:
		payload := map[string]any{}
		if sequence, count, ok := runtime.takePendingCompactionIdentity(); ok {
			// The durable sequence is the stable identity shared by the persisted
			// summary and this ordered stream boundary. The web projection uses it
			// to adopt the durable row at this exact event position.
			payload["compaction_seq"] = sequence
			payload["compaction_count"] = count
		}
		return run.appendEvent("response.compaction", payload)
	case llm.EventRetry:
		payload := map[string]any{
			"attempt":      ev.RetryAttempt,
			"max_attempts": ev.RetryMaxAttempts,
			"wait_seconds": ev.RetryWaitSecs,
		}
		if ev.Err != nil {
			payload["error"] = ev.Err.Error()
		}
		if ev.RetryMaxAttempts > 0 {
			payload["message"] = fmt.Sprintf("Model stream interrupted; reconnecting (%d/%d)…", ev.RetryAttempt, ev.RetryMaxAttempts)
		} else {
			payload["message"] = "Model stream interrupted; reconnecting…"
		}
		return run.appendEvent("response.retry", payload)
	case llm.EventInterjection:
		if strings.TrimSpace(ev.InterjectionID) == "" {
			return errors.New("interjection event is missing client_message_id")
		}
		// One or more committed interjections form a single user boundary before
		// the next assistant text. Defer the ordinal bump until text arrives so a
		// batch of interjections cannot create skipped segment identities.
		state.assistantBoundaryPending = true
		payload := map[string]any{
			"text":              ev.Text,
			"client_message_id": ev.InterjectionID,
		}
		if ev.InterjectionStatus != "" {
			payload["status"] = string(ev.InterjectionStatus)
		}
		if atts := s.interjectionAttachmentsForEvent(ev.Message); len(atts) > 0 {
			payload["attachments"] = atts
		}
		return run.appendEvent("response.interjection", payload)
	case llm.EventModelSwitch:
		fromModel := strings.TrimSpace(ev.PreviousModel)
		toModel := strings.TrimSpace(ev.Model)
		if toModel == "" {
			toModel = strings.TrimSpace(ev.Text)
		}
		boundaryID := strings.TrimSpace(ev.ModelSwitchBoundaryID)
		if fromModel == "" || toModel == "" || boundaryID == "" || !ev.ProviderTurnIndexSet {
			return fmt.Errorf("model switch event is missing authoritative runtime identity")
		}
		fromEffort := strings.TrimSpace(ev.PreviousReasoningEffort)
		toEffort := strings.TrimSpace(ev.ReasoningEffort)
		provider := runtimeProviderKey(runtime)
		marker := llm.ModelSwapMarker{
			FromProvider: provider,
			FromModel:    fromModel,
			FromEffort:   fromEffort,
			ToProvider:   provider,
			ToModel:      toModel,
			ToEffort:     toEffort,
			BoundaryID:   boundaryID,
			Status:       "succeeded",
		}
		message := llm.FormatModelSwapMarker(marker)
		if state != nil {
			state.model = toModel
			state.reasoningEffort = toEffort
			state.reasoningEffortSet = true
		}
		return run.appendEvent("response.model_switch", map[string]any{
			"model":                 toModel,
			"reasoning_effort":      toEffort,
			"from_provider":         provider,
			"from_model":            fromModel,
			"from_reasoning_effort": fromEffort,
			"to_provider":           provider,
			"to_model":              toModel,
			"to_reasoning_effort":   toEffort,
			"provider_turn_index":   ev.ProviderTurnIndex,
			"boundary_id":           boundaryID,
			"swap_status":           "succeeded",
			"message":               message,
		})
	default:
		return nil
	}
}

func attachmentsFromPayload(v any) []map[string]any {
	switch items := v.(type) {
	case []map[string]any:
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if len(item) > 0 {
				out = append(out, cloneJSONMap(item))
			}
		}
		return out
	case []any:
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if m := mapValue(item); len(m) > 0 {
				out = append(out, cloneJSONMap(m))
			}
		}
		return out
	default:
		return nil
	}
}

func (s *serveServer) interjectionAttachmentsForEvent(msg llm.Message) []map[string]any {
	var out []map[string]any
	imageCount := 0
	for _, part := range msg.Parts {
		if part.Type != llm.PartImage {
			continue
		}
		imageURL, serveablePath := s.sessionMessageImageURL(part)
		if imageURL == "" {
			continue
		}
		imageCount++
		mediaType := "image/*"
		if part.ImageData != nil && part.ImageData.MediaType != "" {
			mediaType = part.ImageData.MediaType
		}
		attachment := map[string]any{
			"name": fmt.Sprintf("image %d", imageCount),
			"type": mediaType,
			"url":  imageURL,
		}
		width, height := sessionMessageImageDimensions(part, serveablePath)
		if width > 0 && height > 0 {
			attachment["width"] = width
			attachment["height"] = height
		}
		out = append(out, attachment)
	}
	return out
}

func (s *serveServer) storeCompletedResponseRun(runtime *serveRuntime, sessionID, previousResponseID, model string, created int64, result serveRunResult, resetResponseIDsOnSuccess bool) (string, error) {
	mgr := s.ensureResponseRuns()

	respID := "resp_" + randomSuffix()
	run := newResponseRun(respID, sessionID, previousResponseID, model, created, nil)
	s.configureResponseRunRevision(run, sessionID)
	if err := mgr.create(run); err != nil {
		return "", err
	}

	cleanup := func() {
		mgr.delete(respID)
	}
	createdResponse := map[string]any{
		"id":      respID,
		"object":  "response",
		"created": created,
		"model":   model,
		"status":  "in_progress",
	}
	if err := run.appendEvent("response.created", map[string]any{
		"response": createdResponse,
	}); err != nil {
		cleanup()
		return "", err
	}
	if result.Text.Len() > 0 {
		if err := run.appendTextDeltaSegmentEvent(0, 0, result.Text.String()); err != nil {
			cleanup()
			return "", err
		}
	}
	durableID := s.latestDurableResponseIDForSessionBestEffort(context.Background(), sessionID)
	completedID := respID
	if durableID != "" {
		completedID = durableID
	}
	if err := run.complete(map[string]any{
		"response": map[string]any{
			"id":            completedID,
			"object":        "response",
			"created":       created,
			"model":         model,
			"status":        "completed",
			"usage":         usagePayload(result.Usage),
			"session_usage": usagePayload(result.SessionUsage),
		},
	}, result.Usage, result.SessionUsage); err != nil {
		cleanup()
		return "", err
	}

	mgr.scheduleCleanup(respID)
	if resetResponseIDsOnSuccess {
		s.unregisterSessionResponseIDs(sessionID)
	}
	if completedID != respID {
		s.registerResponseID(runtime, respID, sessionID)
	}
	s.registerResponseID(runtime, completedID, sessionID)
	return completedID, nil
}

func (s *serveServer) streamFailedResponseRun(ctx context.Context, w http.ResponseWriter, sessionID, previousResponseID, model, errType, errMessage string) {
	mgr := s.ensureResponseRuns()

	respID := "resp_" + randomSuffix()
	created := time.Now().Unix()
	run := newResponseRun(respID, sessionID, previousResponseID, model, created, nil)
	s.configureResponseRunRevision(run, sessionID)
	if err := mgr.create(run); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	cleanup := func() {
		mgr.delete(respID)
	}
	createdResponse := map[string]any{
		"id":      respID,
		"object":  "response",
		"created": created,
		"model":   model,
		"status":  "in_progress",
	}
	if err := run.appendEvent("response.created", map[string]any{
		"response": createdResponse,
	}); err != nil {
		cleanup()
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if _, err := run.fail(map[string]any{
		"error": map[string]any{
			"message": errMessage,
			"type":    errType,
		},
	}, errType, errMessage); err != nil {
		cleanup()
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	mgr.scheduleCleanup(respID)
	s.streamResponseRunEvents(ctx, w, run, 0)
}

func (s *serveServer) discardPendingInterjectionsForResponseRun(run *responseRun) {
	if s == nil || s.sessionMgr == nil || run == nil {
		return
	}
	sessionID := strings.TrimSpace(run.sessionID)
	if sessionID == "" {
		return
	}
	rt, ok := s.sessionMgr.Get(sessionID)
	if !ok || rt == nil || rt.engine == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt.discardPendingInterjections(ctx, sessionID)
}

func (s *serveServer) handleResponseByID(w http.ResponseWriter, r *http.Request) {
	mgr := s.ensureResponseRuns()

	path := strings.TrimPrefix(r.URL.Path, "/v1/responses/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	runID := parts[0]
	run, ok := mgr.get(runID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "response not found")
		return
	}

	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, run.snapshot())
		return
	}

	if len(parts) == 2 && parts[1] == "events" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		after, err := parseNonNegativeIntQuery(r, "after", 0)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		s.streamResponseRunEvents(r.Context(), w, run, int64(after))
		return
	}

	if len(parts) == 2 && parts[1] == "cancel" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		cancel, accepted := run.requestCancel()
		if !accepted {
			snapshot := run.snapshot()
			writeJSON(w, http.StatusOK, map[string]any{
				"id":       runID,
				"object":   "response.cancel",
				"status":   snapshot["status"],
				"replayed": true,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":       runID,
			"object":   "response.cancel",
			"status":   "cancelling",
			"replayed": false,
		})
		// The cancellation request is accepted once it is recorded above. Provider,
		// tool, and interjection cleanup can wind down without holding the HTTP
		// acknowledgement open.
		go func() {
			if cancel != nil {
				cancel()
			}
			s.discardPendingInterjectionsForResponseRun(run)
		}()
		return
	}

	http.NotFound(w, r)
}

func (s *serveServer) streamResponseRunEvents(ctx context.Context, w http.ResponseWriter, run *responseRun, after int64) {
	w = newStreamingResponseWriter(w, serveStreamWriteTimeout)
	subscription := run.subscribe(after)
	if subscription.snapshotRequired {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"type":    "conflict_error",
				"message": "response replay no longer available; fetch the response snapshot and resume from its sequence number",
			},
			"snapshot_required": true,
			"min_replay_after":  subscription.minReplayAfter,
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	replay := subscription.replay
	replayThrough := after
	if len(replay) > 0 {
		replayThrough = replay[len(replay)-1].Sequence
	}
	w.Header().Set("X-Term-LLM-Replay-Through", strconv.FormatInt(replayThrough, 10))
	w.Header().Set("X-Term-LLM-Response-Status", subscription.status)
	setSSEHeaders(w)
	flusher.Flush()
	ch := subscription.ch
	subscriberID := subscription.id

	pingMu, stopPing := sseKeepalive(ctx, w, flusher, 10*time.Second)
	var stopPingOnce sync.Once
	stopKeepalive := func() {
		stopPingOnce.Do(stopPing)
	}
	defer stopKeepalive()
	if ch != nil {
		defer run.unsubscribe(ch)
	}

	writeDone := func() {
		stopKeepalive()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	writeDroppedStreamError := func() {
		ev, err := run.droppedSubscriberTerminalEvent()
		if err != nil {
			return
		}
		pingMu.Lock()
		writeErr := writeStoredResponseEvent(w, ev)
		flusher.Flush()
		pingMu.Unlock()
		if writeErr != nil {
			return
		}
		writeDone()
	}

	if len(replay) > 0 {
		pingMu.Lock()
		var replayErr error
		for _, ev := range replay {
			if replayErr = writeStoredResponseEvent(w, ev); replayErr != nil {
				break
			}
		}
		flusher.Flush()
		pingMu.Unlock()
		if replayErr != nil {
			return
		}
	}

	if ch == nil {
		writeDone()
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.shutdownCh:
			return
		case ev, ok := <-ch:
			if !ok {
				if run.subscriberWasDropped(subscriberID) {
					writeDroppedStreamError()
					return
				}
				writeDone()
				return
			}
			// Drain any immediately available events and write them as a
			// batch under a single lock+Flush to cut syscall overhead at
			// high token rates (~100 events/sec during streaming).
			pingMu.Lock()
			closed := false
			writeErr := writeStoredResponseEvent(w, ev)
		drainLoop:
			for writeErr == nil {
				select {
				case next, nextOK := <-ch:
					if !nextOK {
						closed = true
						break drainLoop
					}
					writeErr = writeStoredResponseEvent(w, next)
				default:
					break drainLoop
				}
			}
			flusher.Flush()
			pingMu.Unlock()
			if writeErr != nil {
				return
			}
			if closed {
				if run.subscriberWasDropped(subscriberID) {
					writeDroppedStreamError()
					return
				}
				writeDone()
				return
			}
		}
	}
}

func (s *serveServer) transcriptRev(ctx context.Context, sessionID string) (int64, error) {
	indexer, ok := s.transcriptIndexerForWeb()
	if !ok {
		return 0, errors.New("revisioned transcript unavailable")
	}
	return indexer.TranscriptRev(ctx, sessionID)
}

const responseRunRevisionReadTimeout = 5 * time.Second

func latestResponseRunDurableBoundary(items []session.TranscriptIndexItem) int64 {
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.ID <= 0 || item.Flags&session.TranscriptFlagCompactionTail != 0 {
			continue
		}
		switch llm.Role(item.Role) {
		case llm.RoleUser, llm.RoleAssistant, llm.RoleTool:
			return item.ID
		}
	}
	return 0
}

func (s *serveServer) projectResponseInteractionState(store session.ResponseRunInteractionStore, state session.ResponseRunInteractionState) {
	if store == nil || state.ResponseID == "" || state.OwnerInstanceID == "" || state.FencingToken <= 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.SetResponseRunInteractionState(ctx, state); err != nil && !errors.Is(err, session.ErrResponseRunLeaseLost) {
			log.Printf("[serve] response interaction projection failed for %.12s: %v", state.ResponseID, err)
		}
	}()
}

func (s *serveServer) configureResponseRunRevision(run *responseRun, sessionID string) {
	if run == nil {
		return
	}
	run.coarseEvent = func(event string, payload map[string]any) {
		s.publishResponseRunEvent(run, event, payload)
	}
	startedCtx, startedCancel := context.WithTimeout(context.Background(), responseRunRevisionReadTimeout)
	configured := false
	compactReadAllowed := true
	if reporter, ok := s.store.(session.TranscriptVersionReporter); ok && !reporter.TranscriptVersioned() {
		compactReadAllowed = false
	}
	if reader, ok := s.store.(session.ResponseRunStartStateReader); compactReadAllowed && ok {
		if state, err := reader.GetResponseRunStartState(startedCtx, sessionID); err == nil {
			run.startedRev = state.Rev
			run.startedCompactionSeq = state.CompactionSeq
			run.startedCompactionCount = state.CompactionCount
			if state.DurableBoundaryID > 0 {
				run.setInitialDurableBoundary(state.DurableBoundaryID)
			}
			configured = true
		}
	}
	if !configured {
		if indexer, ok := s.transcriptIndexerForWeb(); ok {
			if snapshot, err := indexer.GetTranscriptSnapshot(startedCtx, sessionID); err == nil {
				run.startedRev = snapshot.Rev
				run.startedCompactionSeq = snapshot.CompactionSeq
				run.startedCompactionCount = snapshot.CompactionCount
				if boundaryID := latestResponseRunDurableBoundary(snapshot.Items); boundaryID > 0 {
					run.setInitialDurableBoundary(boundaryID)
				}
			}
		}
	}
	startedCancel()
	run.finalRevReader = func() (int64, error) {
		ctx, cancel := context.WithTimeout(context.Background(), responseRunRevisionReadTimeout)
		defer cancel()
		return s.transcriptRev(ctx, sessionID)
	}
}

func (s *serveServer) responseRunContinuationID(ctx context.Context, runtime *serveRuntime, sessionID, responseID string) string {
	continuationID := responseID
	if durableID := s.latestDurableResponseIDForSessionBestEffort(ctx, sessionID); durableID != "" {
		continuationID = durableID
	}
	if continuationID != responseID {
		s.registerResponseID(runtime, responseID, sessionID)
	}
	s.registerResponseID(runtime, continuationID, sessionID)
	return continuationID
}

func (s *serveServer) startResponseRun(runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string, options startResponseRunOptions) (*responseRun, error) {
	mgr := s.ensureResponseRuns()

	respID := "resp_" + randomSuffix()
	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}
	created := time.Now().Unix()

	// Intentionally detached from the HTTP request context. Runs must survive
	// client disconnects so that:
	//  - SSE connections are fragile (network blips, mobile tab switches, etc.);
	//    killing a run on disconnect would waste partial work.
	//  - Clients reconnect via GET /v1/responses/{id}/events?after=N and replay
	//    events they missed, which only works if the run kept going.
	//  - Explicit cancellation is available via POST /v1/responses/{id}/cancel.
	//  - serve.response_timeout bounds inactivity until the next completed LLM
	//    response, excluding time spent waiting for an interactive answer.
	runCtx, runTimer := newResponseRunTimer(s.responseTimeout())
	cancel := runTimer.stop
	run := newResponseRun(respID, sessionID, options.previousResponseID, model, created, cancel)
	run.idempotencyScope = strings.TrimSpace(options.idempotencyScope)
	run.requestFingerprint = strings.TrimSpace(options.requestFingerprint)
	if subscriptionID := strings.TrimSpace(options.notificationSubscriptionID); subscriptionID != "" {
		run.terminalNotify = func(outcome string) {
			s.enqueueCompletionPush(run.id, run.sessionID, subscriptionID, outcome, time.Now().UTC())
		}
	}
	for i := len(inputMessages) - 1; i >= 0; i-- {
		if inputMessages[i].Role == llm.RoleUser && strings.TrimSpace(inputMessages[i].ClientMessageID) != "" {
			run.clientMessageID = strings.TrimSpace(inputMessages[i].ClientMessageID)
			break
		}
	}
	runCtx = withResponseRunContext(runCtx, run)
	s.configureResponseRunRevision(run, sessionID)
	createdRun, duplicate, err := mgr.createOrGetByIdempotency(run, options.idempotencyKey)
	if err != nil {
		cancel()
		if options.onDone != nil {
			options.onDone()
		}
		return nil, err
	}
	if duplicate {
		cancel()
		if options.onDone != nil {
			options.onDone()
		}
		return createdRun, nil
	}
	if s.commitActiveForSession(context.Background(), sessionID) {
		cancel()
		mgr.delete(respID)
		if options.onDone != nil {
			options.onDone()
		}
		return nil, fmt.Errorf("%w: commit workflow is active for this session or checkout", errServeSessionBusy)
	}
	if lifecycle, ok := session.AsServeResponseLifecycleStore(s.store); ok && sessionID != "" && s.shutdownCh != nil {
		ownerID := s.responseOwnerID()
		admitCtx, admitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		lease, admitErr := lifecycle.AdmitResponseRun(admitCtx, session.ResponseRunAdmission{
			ResponseID:      respID,
			SessionID:       sessionID,
			RunEpoch:        run.runEpoch,
			OwnerInstanceID: ownerID,
			StartedRev:      run.startedRev,
			StartedAt:       time.Unix(created, 0).UTC(),
			LeaseDuration:   30 * time.Second,
		})
		admitCancel()
		if admitErr != nil {
			cancel()
			mgr.delete(respID)
			if options.onDone != nil {
				options.onDone()
			}
			return nil, fmt.Errorf("admit durable response run: %w", admitErr)
		}
		if admitErr == nil {
			s.attentionDiagnostics.LifecycleAdmissions.Add(1)
			run.mu.Lock()
			run.ownerInstanceID = ownerID
			run.fencingToken = lease.FencingToken
			run.leaseExpiresAt = lease.LeaseExpiresAt
			run.atomicTranscriptFencing = session.SupportsAtomicResponseRunTranscriptFencing(s.store)
			run.mu.Unlock()
			run.validateLifecycle = func() error {
				validateCtx, validateCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer validateCancel()
				return lifecycle.ValidateResponseRunLease(validateCtx, respID, ownerID, lease.FencingToken)
			}
			run.checkpointLifecycle = func(finalRev int64, outputCount int) error {
				checkpointCtx, checkpointCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer checkpointCancel()
				return lifecycle.CheckpointResponseRun(checkpointCtx, session.ResponseRunCheckpoint{
					ResponseID: respID, OwnerInstanceID: ownerID, FencingToken: lease.FencingToken,
					FinalRev: finalRev, DurableOutputCount: outputCount,
				})
			}
			run.finalizeLifecycle = func(outcome session.ResponseRunState, finalRev int64, outputCount int) (session.AttentionState, error) {
				terminal := session.ResponseRunTerminal{ResponseID: respID, OwnerInstanceID: ownerID,
					FencingToken: lease.FencingToken, Outcome: outcome, FinalRev: finalRev,
					DurableOutputCount: outputCount, EndedAt: time.Now().UTC()}
				finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer finalizeCancel()
				var attention session.AttentionState
				var finalizeErr error
				for attempt := 0; attempt < 2; attempt++ {
					attention, finalizeErr = lifecycle.FinalizeResponseRun(finalizeCtx, terminal)
					if finalizeErr == nil || errors.Is(finalizeErr, session.ErrResponseRunLeaseLost) || errors.Is(finalizeErr, context.Canceled) || errors.Is(finalizeErr, context.DeadlineExceeded) {
						break
					}
					select {
					case <-finalizeCtx.Done():
						finalizeErr = finalizeCtx.Err()
						break
					case <-time.After(100 * time.Millisecond):
					}
				}
				if finalizeErr == nil {
					s.attentionDiagnostics.LifecycleFinalized.Add(1)
					if attention.ResponseID == respID && attention.LatestAttentionSeq > 0 {
						s.attentionDiagnostics.MarkerWrites.Add(1)
					}
				}
				return attention, finalizeErr
			}
			if interactionStore, supported := session.AsResponseRunInteractionStore(s.store); supported {
				run.interactionStateChanged = func(state session.ResponseRunInteractionState) {
					s.projectResponseInteractionState(interactionStore, state)
				}
			}
		}
	}
	// Publish activity before the goroutine can hydrate history or begin provider
	// work. If another run already owns the session, the per-runtime TryLock will
	// preserve the existing behavior of producing a failed response-run object;
	// trySetActiveRun never overwrites that owner.
	if sessionID != "" {
		mgr.trySetActiveRun(sessionID, respID)
	}

	if options.uiSession {
		runtime.clearLastUIRunError()
	}

	createdResponse := map[string]any{
		"id":      respID,
		"object":  "response",
		"created": created,
		"model":   model,
		"status":  "in_progress",
	}
	if effort := strings.TrimSpace(llmReq.ReasoningEffort); effort != "" {
		createdResponse["reasoning_effort"] = effort
	}
	if options.modelSwap != nil && options.modelSwap.plan.enabled {
		createdResponse["provider"] = options.modelSwap.plan.requestedProvider
	}
	if err := run.appendEvent("response.created", map[string]any{
		"response": createdResponse,
	}); err != nil {
		cancel()
		mgr.clearActiveRun(sessionID, respID)
		mgr.delete(respID)
		if options.onDone != nil {
			options.onDone()
		}
		return nil, err
	}

	if err := mgr.start(func() {
		defer cancel()
		if options.onDone != nil {
			defer options.onDone()
		}
		defer func() {
			mgr.clearActiveRun(sessionID, respID)
			mgr.scheduleCleanup(respID)
		}()
		if !stateful {
			defer func() {
				runtime.Close()
				s.unregisterResponseIDs(runtime)
			}()
		}

		// Wire approval event callback so PromptUIFunc can emit SSE events
		runtime.approvalMu.Lock()
		runtime.approvalEventFunc = func(event string, data map[string]any) error {
			return run.appendEvent(event, data)
		}
		runtime.approvalCtx = runCtx
		runtime.pauseResponseTimeout = runTimer.pause
		runtime.refreshResponseTimeout = runTimer.refresh
		runtime.approvalMu.Unlock()
		defer func() {
			runtime.approvalMu.Lock()
			runtime.approvalEventFunc = nil
			runtime.approvalCtx = nil
			runtime.pauseResponseTimeout = nil
			runtime.refreshResponseTimeout = nil
			runtime.approvalMu.Unlock()
		}()

		if options.modelSwap != nil && options.modelSwap.plan.enabled {
			s.executeResponseRunModelSwap(runCtx, runtime, run, stateful, replaceHistory, inputMessages, llmReq, sessionID, respID, model, created, options)
			return
		}

		streamState := newResponseRunStreamState(model, llmReq.ReasoningEffort)
		runtimeRunCtx := withServeRuntimeSetup(runCtx, options.runtimeSetup)
		result, err := runtime.RunWithEventsAndStart(runtimeRunCtx, stateful, replaceHistory, inputMessages, llmReq, func() {
			mgr.setActiveRun(sessionID, respID)
		}, func(ev llm.Event) error {
			return s.appendResponseRunEvent(runtime, run, streamState, ev)
		})
		if err != nil {
			runTimedOut := responseRunTimedOut(runCtx)
			if errors.Is(err, context.Canceled) && !runTimedOut {
				continuationID := s.responseRunContinuationID(runCtx, runtime, sessionID, respID)
				cancelled, cancelErr := run.finishCancelled(map[string]any{
					"response": map[string]any{
						"id":      continuationID,
						"object":  "response",
						"created": created,
						"model":   model,
						"status":  "cancelled",
					},
				})
				if cancelled {
					if options.uiSession {
						runtime.clearLastUIRunError()
					}
					if cancelErr != nil {
						log.Printf("response run %s failed to append cancellation event: %v", respID, cancelErr)
					}
					return
				}
			}
			errType := "invalid_request_error"
			errMessage := err.Error()
			if runTimedOut || errors.Is(err, context.DeadlineExceeded) {
				errType = "timeout_error"
				errMessage = responseRunDeadlineMessage(runCtx, s.responseTimeout())
			} else if errors.Is(err, errServeSessionBusy) {
				errType = "conflict_error"
			} else if errors.Is(err, errServeSessionPersistence) {
				errType = "server_error"
			}
			if !errors.Is(err, context.Canceled) || runTimedOut {
				s.persistResponseRunErrorEvent(runCtx, runtime, sessionID, respID, errType, errMessage)
			}
			continuationID := s.responseRunContinuationID(runCtx, runtime, sessionID, respID)
			hadSubscribers, failErr := run.fail(map[string]any{
				"response": map[string]any{
					"id":      continuationID,
					"object":  "response",
					"created": created,
					"model":   model,
					"status":  "failed",
				},
				"error": map[string]any{
					"message": errMessage,
					"type":    errType,
				},
			}, errType, errMessage)
			if options.uiSession {
				switch {
				case hadSubscribers:
					runtime.clearLastUIRunError()
				case errors.Is(err, context.Canceled) && !runTimedOut:
					runtime.clearLastUIRunError()
				default:
					runtime.setLastUIRunError(errMessage)
				}
			}
			if failErr != nil {
				log.Printf("response run %s failed to append terminal event: %v", respID, failErr)
			}
			if failErr != nil && options.uiSession && (!errors.Is(err, context.Canceled) || runTimedOut) {
				runtime.setLastUIRunError(errMessage)
			}
			return
		}

		if options.uiSession {
			runtime.clearLastUIRunError()
		}
		if options.resetResponseIDsOnSuccess {
			s.unregisterSessionResponseIDs(sessionID)
		}
		completedID := s.responseRunContinuationID(runCtx, runtime, sessionID, respID)
		finalModel := streamState.appliedModel(model)
		finalEffort, finalEffortSet := streamState.appliedReasoningEffort(llmReq.ReasoningEffort)
		if options.uiSession && (finalModel != model || finalEffort != strings.TrimSpace(llmReq.ReasoningEffort) || finalEffortSet != (strings.TrimSpace(llmReq.ReasoningEffort) != "")) {
			s.syncPersistedSessionRuntime(runCtx, sessionID, runtime, finalModel, finalEffort, "", false, "", false)
		}
		completeResponse := map[string]any{
			"id":            completedID,
			"object":        "response",
			"created":       created,
			"model":         finalModel,
			"status":        "completed",
			"usage":         usagePayload(result.Usage),
			"session_usage": usagePayload(result.SessionUsage),
		}
		if finalEffortSet {
			completeResponse["reasoning_effort"] = finalEffort
		}
		if err := run.complete(map[string]any{
			"response": completeResponse,
		}, result.Usage, result.SessionUsage); err != nil {
			log.Printf("response run %s failed to append completion event: %v", respID, err)
			return
		}
		s.scheduleAutoTitle(sessionID, runtime.providerKey)
	}); err != nil {
		cancel()
		mgr.clearActiveRun(sessionID, respID)
		mgr.delete(respID)
		if options.onDone != nil {
			options.onDone()
		}
		return nil, err
	}

	return run, nil
}
