package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/process"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/runboundary"
	"github.com/samsaffron/term-llm/internal/session"
)

// The process-private handoff preserves response identity, replay sequence and
// fence as well as the engine continuation. It is consumed only by this OS
// process's next image; it is not a crash-replay queue or a new user request.
type webReloadState struct {
	Owner string
	Runs  []webRunContinuation
}
type webRunContinuation struct {
	Engine          *llm.Continuation
	CumulativeUsage llm.Usage
	Provider        string
	Stateful        bool
	UI              bool
	Notification    string
	View            webRunView
	Boundary        runboundary.Snapshot
	Stream          webStreamView
	Ledger          webLedgerView
}
type webLedgerView struct {
	MaxRev       int64
	OutputKeys   map[string]struct{}
	NextOutputID int64
}
type webStreamView struct {
	OutputIndex              int
	ToolsSeen                bool
	AssistantBoundaryPending bool
	AssistantSegmentOrdinal  int
	Model                    string
	ReasoningEffort          string
	ReasoningEffortSet       bool
	ToolStartedAt            map[string]int64
}
type webRunView struct {
	Id                      string
	SessionID               string
	PreviousResponseID      string
	ClientMessageID         string
	IdempotencyKey          string
	IdempotencyScope        string
	RequestFingerprint      string
	AnchorRowID             int64
	AnchorAvailable         bool
	Model                   string
	ReasoningEffort         string
	ReasoningEffortSet      bool
	Created                 int64
	EndedAt                 int64
	RunEpoch                int64
	StartedRev              int64
	StartedCompactionSeq    int
	StartedCompactionCount  int
	FinalRev                int64
	AttentionSeq            int64
	AttentionStoreID        string
	OwnerInstanceID         string
	FencingToken            int64
	LeaseExpiresAt          time.Time
	AtomicTranscriptFencing bool
	DurableHandoff          bool
	DurableOutputCount      int
	DurableHandoffErr       string
	ContinuationResponseID  string
	Status                  string
	ErrorType               string
	ErrorMessage            string
	Usage                   llm.Usage
	SessionUsage            llm.Usage
	LastSequenceNumber      int64
	Events                  []responseRunEvent
	EventStart              int
	MinReplayAfter          int64
	MaxRetainedEvents       int
	RecoveryMessages        []responseRunRecoveryMessage
	RecoveryEvents          []responseRunRecoveryEvent
	ResolvedInteractions    map[string]responseRunResolvedInteraction
	PendingGuardianByCall   map[string][]map[string]any
	NextMessageOrdinal      int64
	CurrentAssistant        int
	CurrentToolGroup        int
	SegmentRanges           map[int]responseRunSegmentRange
	CompactionEnabled       bool
	CancelRequested         bool
}

func snapshotWebRun(run *responseRun, stream *responseRunStreamState, engine *llm.Continuation, runtime *serveRuntime, stateful bool, options startResponseRunOptions) (*webRunContinuation, error) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.cancelRequested {
		return nil, context.Canceled
	}
	run.persistence.mu.Lock()
	defer run.persistence.mu.Unlock()
	if run.persistence.failed || run.persistence.inflight != 0 {
		return nil, errors.New("response persistence is not settled for reload")
	}
	saved := &webRunContinuation{Engine: engine, Provider: runtimeProviderKey(runtime), Stateful: stateful, UI: options.uiSession, Notification: options.notificationSubscriptionID,
		Ledger: webLedgerView{run.persistence.maxRev, run.persistence.outputKeys, run.persistence.nextOutputID},
		Stream: webStreamView{stream.outputIndex, stream.toolsSeen, stream.assistantBoundaryPending, stream.assistantSegmentOrdinal, stream.model, stream.reasoningEffort, stream.reasoningEffortSet, stream.toolStartedAt},
		View: webRunView{
			Id:                      run.id,
			SessionID:               run.sessionID,
			PreviousResponseID:      run.previousResponseID,
			ClientMessageID:         run.clientMessageID,
			IdempotencyKey:          run.idempotencyKey,
			IdempotencyScope:        run.idempotencyScope,
			RequestFingerprint:      run.requestFingerprint,
			AnchorRowID:             run.anchorRowID,
			AnchorAvailable:         run.anchorAvailable,
			Model:                   run.model,
			ReasoningEffort:         run.reasoningEffort,
			ReasoningEffortSet:      run.reasoningEffortSet,
			Created:                 run.created,
			EndedAt:                 run.endedAt,
			RunEpoch:                run.runEpoch,
			StartedRev:              run.startedRev,
			StartedCompactionSeq:    run.startedCompactionSeq,
			StartedCompactionCount:  run.startedCompactionCount,
			FinalRev:                run.finalRev,
			AttentionSeq:            run.attentionSeq,
			AttentionStoreID:        run.attentionStoreID,
			OwnerInstanceID:         run.ownerInstanceID,
			FencingToken:            run.fencingToken,
			LeaseExpiresAt:          run.leaseExpiresAt,
			AtomicTranscriptFencing: run.atomicTranscriptFencing,
			DurableHandoff:          run.durableHandoff,
			DurableOutputCount:      run.durableOutputCount,
			DurableHandoffErr:       run.durableHandoffErr,
			ContinuationResponseID:  run.continuationResponseID,
			Status:                  run.status,
			ErrorType:               run.errorType,
			ErrorMessage:            run.errorMessage,
			Usage:                   run.usage,
			SessionUsage:            run.sessionUsage,
			LastSequenceNumber:      run.lastSequenceNumber,
			Events:                  run.events,
			EventStart:              run.eventStart,
			MinReplayAfter:          run.minReplayAfter,
			MaxRetainedEvents:       run.maxRetainedEvents,
			RecoveryMessages:        run.recoveryMessages,
			RecoveryEvents:          run.recoveryEvents,
			ResolvedInteractions:    run.resolvedInteractions,
			PendingGuardianByCall:   run.pendingGuardianByCall,
			NextMessageOrdinal:      run.nextMessageOrdinal,
			CurrentAssistant:        run.currentAssistant,
			CurrentToolGroup:        run.currentToolGroup,
			SegmentRanges:           run.segmentRanges,
			CompactionEnabled:       run.compactionEnabled,
			CancelRequested:         run.cancelRequested,
		}}
	if run.boundary != nil {
		saved.Boundary = run.boundary.CompletedSnapshot()
	}
	return saved, nil
}

func restoreWebRun(saved *webRunContinuation, cancel context.CancelFunc) *responseRun {
	run := newResponseRun(saved.View.Id, saved.View.SessionID, saved.View.PreviousResponseID, saved.View.Model, saved.View.Created, cancel)
	run.id = saved.View.Id
	run.sessionID = saved.View.SessionID
	run.previousResponseID = saved.View.PreviousResponseID
	run.clientMessageID = saved.View.ClientMessageID
	run.idempotencyKey = saved.View.IdempotencyKey
	run.idempotencyScope = saved.View.IdempotencyScope
	run.requestFingerprint = saved.View.RequestFingerprint
	run.anchorRowID = saved.View.AnchorRowID
	run.anchorAvailable = saved.View.AnchorAvailable
	run.model = saved.View.Model
	run.reasoningEffort = saved.View.ReasoningEffort
	run.reasoningEffortSet = saved.View.ReasoningEffortSet
	run.created = saved.View.Created
	run.endedAt = saved.View.EndedAt
	run.runEpoch = saved.View.RunEpoch
	run.startedRev = saved.View.StartedRev
	run.startedCompactionSeq = saved.View.StartedCompactionSeq
	run.startedCompactionCount = saved.View.StartedCompactionCount
	run.finalRev = saved.View.FinalRev
	run.attentionSeq = saved.View.AttentionSeq
	run.attentionStoreID = saved.View.AttentionStoreID
	run.ownerInstanceID = saved.View.OwnerInstanceID
	run.fencingToken = saved.View.FencingToken
	run.leaseExpiresAt = saved.View.LeaseExpiresAt
	run.atomicTranscriptFencing = saved.View.AtomicTranscriptFencing
	run.durableHandoff = saved.View.DurableHandoff
	run.durableOutputCount = saved.View.DurableOutputCount
	run.durableHandoffErr = saved.View.DurableHandoffErr
	run.continuationResponseID = saved.View.ContinuationResponseID
	run.status = saved.View.Status
	run.errorType = saved.View.ErrorType
	run.errorMessage = saved.View.ErrorMessage
	run.usage = saved.View.Usage
	run.sessionUsage = saved.View.SessionUsage
	run.lastSequenceNumber = saved.View.LastSequenceNumber
	run.events = saved.View.Events
	run.eventStart = saved.View.EventStart
	run.minReplayAfter = saved.View.MinReplayAfter
	run.maxRetainedEvents = saved.View.MaxRetainedEvents
	run.recoveryMessages = saved.View.RecoveryMessages
	run.recoveryEvents = saved.View.RecoveryEvents
	run.resolvedInteractions = saved.View.ResolvedInteractions
	run.pendingGuardianByCall = saved.View.PendingGuardianByCall
	run.nextMessageOrdinal = saved.View.NextMessageOrdinal
	run.currentAssistant = saved.View.CurrentAssistant
	run.currentToolGroup = saved.View.CurrentToolGroup
	run.segmentRanges = saved.View.SegmentRanges
	run.compactionEnabled = saved.View.CompactionEnabled
	run.cancelRequested = saved.View.CancelRequested
	run.persistence.maxRev = saved.Ledger.MaxRev
	run.persistence.outputKeys = saved.Ledger.OutputKeys
	run.persistence.nextOutputID = saved.Ledger.NextOutputID
	run.boundary = runboundary.New(run.id, saved.Boundary.Messages, saved.Boundary.DurableAnchorID, saved.Boundary.Durable)
	return run
}
func (saved *webRunContinuation) streamState() *responseRunStreamState {
	v := saved.Stream
	return &responseRunStreamState{outputIndex: v.OutputIndex, toolsSeen: v.ToolsSeen, assistantBoundaryPending: v.AssistantBoundaryPending, assistantSegmentOrdinal: v.AssistantSegmentOrdinal, model: v.Model, reasoningEffort: v.ReasoningEffort, reasoningEffortSet: v.ReasoningEffortSet, toolStartedAt: v.ToolStartedAt}
}

func (s *serveServer) installWebReload() error {
	var incoming webReloadState
	_, err := process.RestoreState("web-runs", &incoming)
	if err != nil {
		return err
	}
	s.reloadRunsUnregister = restart.Default.Register(&restart.Resource{Prepare: func(ctx context.Context) (func(context.Context), error) {
		state := webReloadState{Owner: s.responseOwnerID()}
		if s.responseRuns != nil {
			s.responseRuns.mu.Lock()
			defer s.responseRuns.mu.Unlock()
			for _, run := range s.responseRuns.runs {
				run.mu.Lock()
				saved := run.reloadContinuation
				cancelled := run.cancelRequested
				run.mu.Unlock()
				if saved != nil {
					if cancelled {
						return nil, errors.New("suspended response was cancelled before handoff")
					}
					state.Runs = append(state.Runs, *saved)
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return process.SaveState("web-runs", state)
	}})
	s.restoreWebRuns(incoming)
	return nil
}

// Restore failures belong to individual responses. Once exec succeeds there is
// no old process to roll back to; one missing provider must not stop the server.
func (s *serveServer) restoreWebRuns(incoming webReloadState) {
	if incoming.Owner != "" {
		s.responseOwnerOnce.Do(func() { s.responseOwnerInstanceID = incoming.Owner })
	}
	for i := range incoming.Runs {
		saved := &incoming.Runs[i]
		if err := s.resumeWebRun(saved, incoming.Owner); err != nil {
			log.Printf("[serve] restore response %s: %v", saved.View.Id, err)
			s.failWebRunRestore(saved)
		}
	}
}

func (s *serveServer) resumeWebRun(saved *webRunContinuation, owner string) error {
	if saved.Engine == nil || saved.View.Status != "in_progress" || (saved.View.FencingToken != 0 && (owner == "" || saved.View.OwnerInstanceID != owner)) {
		return errors.New("invalid suspended response handoff")
	}
	rt, _, err := s.runtimeForProviderModelRequest(context.Background(), saved.View.SessionID, saved.Provider, saved.Engine.Request.Model)
	if err != nil {
		return fmt.Errorf("restore suspended response runtime: %w", err)
	}
	rt.cumulativeUsage = saved.CumulativeUsage
	request := saved.Engine.Request
	request.Resume = saved.Engine
	_, err = s.startResponseRun(rt, saved.Stateful, false, nil, request, saved.View.SessionID, startResponseRunOptions{resume: saved, uiSession: saved.UI, notificationSubscriptionID: saved.Notification, previousResponseID: saved.View.PreviousResponseID})
	if err != nil && !saved.Stateful {
		rt.Close()
	}
	return err
}

func (s *serveServer) failWebRunRestore(saved *webRunContinuation) {
	run := restoreWebRun(saved, nil)
	mgr := s.ensureResponseRuns()
	if err := mgr.restore(run); err != nil {
		// Do not replace an already-restored response or conflicting claim.
		log.Printf("[serve] retain failed response %s: %v", saved.View.Id, err)
		return
	}
	s.configureResponseRunCallbacks(run, run.sessionID)
	s.configureResponseRunNotification(run, saved.Notification)
	if lifecycle, ok := session.AsServeResponseLifecycleStore(s.store); ok && run.fencingToken != 0 && run.ownerInstanceID == s.responseOwnerID() {
		s.configureResponseRunLifecycle(run, lifecycle, run.ownerInstanceID, session.ResponseRunLease{ResponseID: run.id, FencingToken: run.fencingToken, LeaseExpiresAt: run.leaseExpiresAt})
	}
	// Provider/config errors can contain credentials; details stay in the log.
	const message = "The interrupted response could not resume after process replacement."
	if _, err := run.fail(map[string]any{"response": map[string]any{
		"id": run.id, "object": "response", "status": "failed",
		"error": map[string]any{"type": "server_error", "message": message},
	}}, "server_error", message); err != nil {
		log.Printf("[serve] finalize unrestorable response %s: %v", run.id, err)
	}
	mgr.scheduleCleanup(run.id)
}

// pauseResponseRun executes inside the response owner after the producer and
// persistence callbacks have returned. Failed exec resumes this same goroutine,
// response object, subscribers and timer rather than starting a duplicate run.
func (s *serveServer) pauseResponseRun(run *responseRun, runtime *serveRuntime, stream *responseRunStreamState, continuation *llm.Continuation, stateful bool, options startResponseRunOptions, runCtx context.Context, release *func(), timer *responseRunTimer) (context.Context, error) {
	saved, err := snapshotWebRun(run, stream, continuation, runtime, stateful, options)
	if err != nil {
		return runCtx, err
	}
	saved.CumulativeUsage = runtime.cumulativeUsage
	run.mu.Lock()
	run.reloadContinuation = saved
	run.mu.Unlock()
	resumeTimer := timer.pause()
	defer resumeTimer()
	(*release)()
	// The owner may be user-cancelled while parked. Wait for rollback before
	// terminal persistence so cancellation cannot race the sealed exec boundary.
	active, done, err := restart.Default.Resume(context.WithoutCancel(runCtx))
	if err != nil {
		return runCtx, err
	}
	*release = done
	active = restart.Inherit(runCtx, active)
	run.mu.Lock()
	run.reloadContinuation = nil
	run.mu.Unlock()
	return active, runCtx.Err()
}
