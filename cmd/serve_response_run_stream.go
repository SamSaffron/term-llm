package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
)

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
		return s.appendResponseTextDelta(runtime, run, state, ev)
	case llm.EventAttemptDiscard:
		return s.appendResponseAttemptDiscard(runtime, run, state, ev)
	case llm.EventToolCall:
		return s.appendResponseToolCall(runtime, run, state, ev)
	case llm.EventToolExecStart:
		return s.appendResponseToolExecStart(runtime, run, state, ev)
	case llm.EventToolExecEnd:
		return s.appendResponseToolExecEnd(runtime, run, state, ev)
	case llm.EventHeartbeat:
		return s.appendResponseHeartbeat(runtime, run, state, ev)
	case llm.EventPhase:
		return s.appendResponsePhase(runtime, run, state, ev)
	case llm.EventCompaction:
		return s.appendResponseCompaction(runtime, run, state, ev)
	case llm.EventRetry:
		return s.appendResponseRetry(runtime, run, state, ev)
	case llm.EventSteering:
		return s.appendResponseSteering(runtime, run, state, ev)
	case llm.EventModelSwitch:
		return s.appendResponseModelSwitch(runtime, run, state, ev)
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

func (s *serveServer) steeringAttachmentsForEvent(msg llm.Message) []map[string]any {
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
	completedResponse := map[string]any{
		"id":            completedID,
		"object":        "response",
		"created":       created,
		"model":         model,
		"status":        "completed",
		"usage":         usagePayload(result.Usage),
		"session_usage": usagePayload(result.SessionUsage),
		"context_usage": result.ContextUsage,
	}
	if err := run.complete(map[string]any{
		"response": completedResponse,
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

func (s *serveServer) discardPendingSteeringForResponseRun(run *responseRun) {
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
	rt.discardPendingSteering(ctx, sessionID)
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
		if transition := s.ensureResponseRuns().steeringTransition(run.sessionID); transition != nil && (runID == transition.source.id || runID == transition.replacementID) {
			if store, ok := session.AsRushStore(s.store); ok {
				op, err := s.cancelSteeringRush(r.Context(), store, run.sessionID, transition.owner.OperationID)
				if err != nil {
					writeOpenAIError(w, 409, "rush_conflict", err.Error())
					return
				}
				writeJSON(w, 200, map[string]any{"id": runID, "status": "cancelling", "rush": op})
				return
			}
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
		// tool, and steering cleanup can wind down without holding the HTTP
		// acknowledgement open.
		_ = restart.Default.Go(r.Context(), func(context.Context) {
			if cancel != nil {
				cancel()
			}
			s.discardPendingSteeringForResponseRun(run)
		})
		return
	}

	http.NotFound(w, r)
}

func (s *serveServer) streamResponseRunEvents(ctx context.Context, w http.ResponseWriter, run *responseRun, after int64) {
	restart.Passive(ctx)
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
	_ = restart.Default.Go(context.Background(), func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := store.SetResponseRunInteractionState(ctx, state); err != nil && !errors.Is(err, session.ErrResponseRunLeaseLost) {
			log.Printf("[serve] response interaction projection failed for %.12s: %v", state.ResponseID, err)
		}
	})
}

func (s *serveServer) configureResponseRunRevision(run *responseRun, sessionID string) {
	if run == nil {
		return
	}
	s.configureResponseRunCallbacks(run, sessionID)
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
}

func (s *serveServer) configureResponseRunCallbacks(run *responseRun, sessionID string) {
	run.coarseEvent = func(event string, payload map[string]any) { s.publishResponseRunEvent(run, event, payload) }
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
	if stateful && s.sessionMgr != nil && sessionID != "" {
		release, err := s.sessionMgr.pinCurrentRuntime(sessionID, runtime)
		if err != nil {
			if options.onDone != nil {
				options.onDone()
			}
			return nil, err
		}
		defer release()
	}
	mgr := s.ensureResponseRuns()

	if t := mgr.steeringTransition(sessionID); t != nil && (options.rush == nil || options.rush.RequestID != t.owner.OperationID) {
		return nil, errServeSessionBusy
	}
	if options.rush == nil {
		if store, ok := session.AsRushStore(s.store); ok {
			if _, err := store.ActiveRush(context.Background(), sessionID); err == nil {
				return nil, errServeSessionBusy
			} else if !errors.Is(err, session.ErrNotFound) {
				return nil, err
			}
		}
	}

	respID := "resp_" + randomSuffix()
	if options.resume != nil {
		respID = options.resume.View.Id
	}
	if options.rush != nil {
		respID = options.rush.ReplacementResponseID
	}
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
	runCtx, releaseReload, reloadErr := restart.Default.Root(runCtx)
	if reloadErr != nil {
		runTimer.stop()
		return nil, reloadErr
	}
	launched := false
	defer func() {
		if !launched {
			releaseReload()
		}
	}()
	cancel := runTimer.stop
	run := newResponseRun(respID, sessionID, options.previousResponseID, model, created, cancel)
	if options.resume != nil {
		run = restoreWebRun(options.resume, cancel)
		created = run.created
	}
	closeTask := func() {}
	if options.rush == nil && options.modelSwap == nil {
		runCtx, closeTask = restart.Default.NewTask(runCtx)
	}
	defer func() {
		if !launched {
			closeTask()
		}
	}()
	run.settled = make(chan struct{})
	run.rushStateful = stateful && !replaceHistory
	run.rushRequest = llmReq
	if options.rush != nil {
		runCtx = context.WithValue(runCtx, rushContextKey{}, options.rush)
		if options.onInitialInput != nil {
			runCtx = context.WithValue(runCtx, rushInitialInputKey{}, options.onInitialInput)
		}
	}
	if options.resume == nil {
		run.idempotencyScope = strings.TrimSpace(options.idempotencyScope)
		run.requestFingerprint = strings.TrimSpace(options.requestFingerprint)
	}
	s.configureResponseRunNotification(run, options.notificationSubscriptionID)
	for i := len(inputMessages) - 1; i >= 0; i-- {
		if inputMessages[i].Role == llm.RoleUser && strings.TrimSpace(inputMessages[i].ClientMessageID) != "" {
			run.clientMessageID = strings.TrimSpace(inputMessages[i].ClientMessageID)
			break
		}
	}
	runCtx = withResponseRunContext(runCtx, run)
	if options.resume == nil {
		s.configureResponseRunRevision(run, sessionID)
	} else {
		s.configureResponseRunCallbacks(run, sessionID)
	}
	createdRun := run
	var duplicate bool
	var err error
	if options.resume != nil {
		err = mgr.restore(run)
	} else {
		createdRun, duplicate, err = mgr.createOrGetByIdempotency(run, options.idempotencyKey)
	}
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
	// Reserve the source/replacement slot before durable admission. A losing
	// start must never enter runOnce merely because the old runtime unlocks.
	if sessionID != "" && !mgr.trySetActiveRun(sessionID, respID) {
		cancel()
		mgr.delete(respID)
		if options.onDone != nil {
			options.onDone()
		}
		return nil, errServeSessionBusy
	}

	if lifecycle, ok := session.AsServeResponseLifecycleStore(s.store); ok && sessionID != "" && s.shutdownCh != nil {
		ownerID := s.responseOwnerID()
		admitCtx, admitCancel := context.WithTimeout(context.Background(), 5*time.Second)
		var lease session.ResponseRunLease
		var admitErr error
		if options.resume != nil {
			lease, admitErr = lifecycle.RenewResponseRunLease(admitCtx, respID, ownerID, run.fencingToken)
		} else {
			lease, admitErr = lifecycle.AdmitResponseRun(admitCtx, session.ResponseRunAdmission{
				ResponseID:      respID,
				SessionID:       sessionID,
				RunEpoch:        run.runEpoch,
				OwnerInstanceID: ownerID,
				StartedRev:      run.startedRev,
				StartedAt:       time.Unix(created, 0).UTC(),
				LeaseDuration:   30 * time.Second,
			})
		}
		admitCancel()
		if admitErr != nil {
			cancel()
			mgr.delete(respID)
			if options.onDone != nil {
				options.onDone()
			}
			return nil, fmt.Errorf("admit durable response run: %w", admitErr)
		}
		s.attentionDiagnostics.LifecycleAdmissions.Add(1)
		s.configureResponseRunLifecycle(run, lifecycle, ownerID, lease)
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
	var createdErr error
	if options.resume == nil {
		createdErr = run.appendEvent("response.created", map[string]any{"response": createdResponse})
	}
	if err := createdErr; err != nil {
		cancel()
		mgr.clearActiveRun(sessionID, respID)
		mgr.delete(respID)
		if options.onDone != nil {
			options.onDone()
		}
		return nil, err
	}

	if err := mgr.start(func() {
		s.executeResponseRun(runCtx, releaseReload, closeTask, cancel, runTimer, mgr, runtime, run, stateful, replaceHistory, inputMessages, llmReq, sessionID, respID, model, created, options)

	}); err != nil {
		cancel()
		mgr.clearActiveRun(sessionID, respID)
		mgr.delete(respID)
		if options.onDone != nil {
			options.onDone()
		}
		return nil, err
	}

	launched = true
	return run, nil
}

func (s *serveServer) configureResponseRunNotification(run *responseRun, subscriptionID string) {
	if subscriptionID = strings.TrimSpace(subscriptionID); subscriptionID != "" {
		run.terminalNotify = func(outcome string) {
			s.enqueueCompletionPush(run.id, run.sessionID, subscriptionID, outcome, time.Now().UTC())
		}
	}
}

// configureResponseRunLifecycle binds callbacks to an existing owner/fence. It
// never admits a new run, so restore failures can terminalize only their own row.
func (s *serveServer) configureResponseRunLifecycle(run *responseRun, lifecycle session.ServeResponseLifecycleStore, ownerID string, lease session.ResponseRunLease) {
	respID := run.id
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
