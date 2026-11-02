package serve

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/app/serve/modelswap"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
	transcriptstore "github.com/samsaffron/term-llm/internal/session/transcript"
)

type responseRuntimeSettings struct {
	provider      string
	model         string
	effort        string
	reasoningMode string
}

type responseModelSwapPlan struct {
	enabled                bool
	fallbackHandover       bool
	requestedProvider      string
	requestedModel         string
	requestedEffort        string
	requestedReasoningMode string
	previousProvider       string
	previousModel          string
	previousEffort         string
}

type responseModelSwapExecution struct {
	plan            responseModelSwapPlan
	candidate       *serveRuntime
	previous        *serveRuntime
	previousHistory []core.Message
	restoreHistory  []core.Message
	commit          func()
	rollback        func()
	committed       bool
	rolledBack      bool
}

func responseRequestedRuntime(req responsesCreateRequest, defaultProvider string) responseRuntimeSettings {
	provider := strings.TrimSpace(req.Provider)
	if provider == "" {
		provider = strings.TrimSpace(defaultProvider)
	}
	effort := req.ReasoningEffort
	if req.Reasoning != nil && strings.TrimSpace(req.Reasoning.Effort) != "" {
		effort = req.Reasoning.Effort
	}
	model, effort := normalizeProviderModelEffort(provider, req.Model, effort)
	reasoningMode := ""
	if req.Reasoning != nil {
		reasoningMode = strings.ToLower(strings.TrimSpace(req.Reasoning.Mode))
	}
	return responseRuntimeSettings{
		provider:      provider,
		model:         model,
		effort:        effort,
		reasoningMode: reasoningMode,
	}
}

func (s *serveServer) persistedRuntimeSettings(ctx context.Context, sessionID string, defaultProvider string) responseRuntimeSettings {
	settings := responseRuntimeSettings{provider: strings.TrimSpace(defaultProvider)}
	if s.store == nil || sessionID == "" {
		return settings
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil || sess == nil {
		return settings
	}
	provider := strings.TrimSpace(sess.ProviderKey)
	if provider == "" {
		provider = appruntime.ResolveSessionProviderKey(s.cfgRef, sess)
	}
	if provider == "" {
		provider = strings.TrimSpace(defaultProvider)
	}
	settings.provider = provider
	settings.model, settings.effort = normalizeProviderModelEffort(provider, sess.Model, sess.ReasoningEffort)
	settings.reasoningMode = strings.ToLower(strings.TrimSpace(sess.ReasoningMode))
	return settings
}

func isModelSwapRequest(req responsesCreateRequest, persisted, requested responseRuntimeSettings) bool {
	if req.ModelSwap == nil {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(req.ModelSwap.Mode))
	if mode != "" && mode != "auto" && mode != "naive_then_handover" {
		return false
	}
	if requested.provider != "" && persisted.provider != "" && requested.provider != persisted.provider {
		return true
	}
	if requested.model != "" && requested.model != persisted.model {
		return true
	}
	if requested.effort != persisted.effort {
		return true
	}
	return false
}

// isEffortOnlySwap excludes real model changes, including selecting a known
// model when the persisted model is unknown. An omitted target keeps the model.
func isEffortOnlySwap(plan responseModelSwapPlan) bool {
	return plan.enabled && plan.requestedProvider == plan.previousProvider &&
		(plan.requestedModel == "" || plan.requestedModel == plan.previousModel) &&
		plan.requestedEffort != plan.previousEffort
}

func buildResponseModelSwapPlan(req responsesCreateRequest, persisted, requested responseRuntimeSettings) responseModelSwapPlan {
	if !isModelSwapRequest(req, persisted, requested) {
		return responseModelSwapPlan{}
	}
	fallback := true
	if req.ModelSwap != nil {
		switch strings.ToLower(strings.TrimSpace(req.ModelSwap.Fallback)) {
		case "", "handover":
			fallback = true
		case "none", "off", "disabled", "false":
			fallback = false
		}
	}
	return responseModelSwapPlan{
		enabled:           true,
		fallbackHandover:  fallback,
		requestedProvider: requested.provider,
		requestedModel:    requested.model,
		requestedEffort:   requested.effort,
		previousProvider:  persisted.provider,
		previousModel:     persisted.model,
		previousEffort:    persisted.effort,
	}
}

func (p responseModelSwapPlan) previousLabel() string {
	return modelswap.Label(p.previousProvider, p.previousModel)
}

func (p responseModelSwapPlan) targetLabel(candidate *serveRuntime) string {
	model := strings.TrimSpace(p.requestedModel)
	if model == "" && candidate != nil {
		model = strings.TrimSpace(candidate.defaultModel)
	}
	return modelswap.Label(p.requestedProvider, model)
}

func (p responseModelSwapPlan) startMessage(candidate *serveRuntime) string {
	return modelswap.StartMessage(p.previousLabel(), p.targetLabel(candidate), normalizeReasoningEffort(p.previousEffort), normalizeReasoningEffort(p.requestedEffort))
}

func copyLLMMessageSlice(messages []core.Message) []core.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]core.Message, len(messages))
	copy(out, messages)
	return out
}

// loadPersistedLLMHistory loads the session's active context, as runtime
// hydration does. It must not read the whole transcript: for a compacted
// session that would feed archived rows to the model and, via ReplaceMessages,
// copy them into the active tail.
func (s *serveServer) loadPersistedLLMHistory(ctx context.Context, sessionID string) []core.Message {
	if s.store == nil || sessionID == "" {
		return nil
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil || sess == nil {
		return nil
	}
	stored, err := session.LoadActiveMessages(ctx, s.store, sess)
	if err != nil || len(stored) == 0 {
		return nil
	}
	messages := make([]core.Message, 0, len(stored))
	for _, msg := range stored {
		messages = append(messages, msg.ToLLMMessage())
	}
	return messages
}

func (s *serveServer) beginResponseModelSwap(ctx context.Context, sessionID string, plan responseModelSwapPlan, inputMessages []core.Message) (*responseModelSwapExecution, error) {
	if !plan.enabled {
		return nil, nil
	}
	previous, _, err := s.runtimeForProviderModelRequest(ctx, sessionID, plan.previousProvider, plan.previousModel)
	if err != nil {
		return nil, err
	}
	if previous == nil {
		return nil, fmt.Errorf("session %q has no previous runtime", sessionID)
	}
	// Hydrate recreated runtimes before snapshotting so handover and naive
	// continuation start from the persisted pre-turn conversation.
	previous.ensurePersistedSession(ctx, sessionID, inputMessages)
	restoreHistory, durable, idle := previous.snapshotSeedHistory()
	if !idle {
		return nil, errServeSessionBusy
	}
	if len(restoreHistory) == 0 {
		restoreHistory = s.loadPersistedLLMHistory(ctx, sessionID)
		durable = len(restoreHistory) > 0 // an empty result may be a failed load
	}
	previousHistory := core.FilterConversationMessages(copyLLMMessageSlice(restoreHistory))

	agentName := strings.TrimSpace(previous.agentName)
	if agentName == "" {
		agentName = s.requestedRuntimeAgent(ctx, sessionID, "")
	}
	create := func(ctx context.Context) (*serveRuntime, error) {
		return s.createRequestRuntime(ctx, serveRuntimeRequest{SessionID: sessionID, Provider: plan.requestedProvider, Model: plan.requestedModel, Agent: agentName, Inputs: previous.selectedSessionInputs(), swapCandidate: true})
	}
	candidate, retainedPrevious, commit, rollback, err := s.sessionMgr.BeginSwap(ctx, sessionID, create)
	if err != nil {
		return nil, err
	}
	// A replacement runtime must restore the same session workspace as a
	// normal resume before MCP setup or any tool execution.
	if err := s.ensureRuntimeBaseDirForSession(ctx, sessionID, candidate); err != nil {
		if rollback != nil {
			rollback()
		}
		return nil, err
	}
	if err := s.ensureRuntimeMCPForSession(ctx, sessionID, candidate); err != nil {
		if rollback != nil {
			rollback()
		}
		return nil, err
	}
	if retainedPrevious != nil && retainedPrevious != previous {
		retainedPrevious.ensurePersistedSession(ctx, sessionID, inputMessages)
		if hist, histDurable, histIdle := retainedPrevious.snapshotSeedHistory(); histIdle {
			restoreHistory, durable = hist, histDurable
			previousHistory = core.FilterConversationMessages(copyLLMMessageSlice(hist))
		}
	}
	// Keep transcript-only events in the candidate's durable history. The
	// engine filters them at the provider boundary; filtering here would erase
	// earlier model-swap markers when the candidate persists its snapshot.
	seedRuntimeHistory(candidate, restoreHistory, durable)
	return &responseModelSwapExecution{
		plan:            plan,
		candidate:       candidate,
		previous:        retainedPrevious,
		previousHistory: previousHistory,
		restoreHistory:  restoreHistory,
		commit:          commit,
		rollback:        rollback,
	}, nil
}

// snapshotSeedHistory copies an idle runtime's history and reports whether the
// copy is the stored transcript.
func (rt *serveRuntime) snapshotSeedHistory() (history []core.Message, durable, idle bool) {
	if rt == nil || !rt.mu.TryLock() {
		return nil, false, false
	}
	defer rt.mu.Unlock()
	return copyLLMMessageSlice(rt.history), rt.historyPersisted, true
}

// seedRuntimeHistory starts rt on history with a fresh provider conversation.
// A durable seed lets rt's first turn append instead of rewriting the stored
// transcript from provider messages, which drops stored-only row fields.
func seedRuntimeHistory(rt *serveRuntime, history []core.Message, durable bool) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.history = copyLLMMessageSlice(history)
	rt.historyPersisted = durable
	rt.cumulativeUsage = core.Usage{}
	rt.lastInjectedPlatform = ""
	if rt.engine != nil {
		rt.engine.ResetConversation()
	}
}

func (exec *responseModelSwapExecution) markCommitted() {
	if exec == nil || exec.committed || exec.rolledBack {
		return
	}
	exec.committed = true
	if exec.commit != nil {
		exec.commit()
	}
}

func (exec *responseModelSwapExecution) markRolledBack() {
	if exec == nil || exec.committed || exec.rolledBack {
		return
	}
	exec.rolledBack = true
	if exec.rollback != nil {
		exec.rollback()
	}
}

func isModelSwapFallbackEligible(err error) bool {
	return !errors.Is(err, errServeSessionBusy) && modelswap.FallbackEligible(err)
}

func modelSwapProgressPayload(stage, message string, plan responseModelSwapPlan, candidate *serveRuntime) map[string]any {
	return map[string]any{
		"stage":             stage,
		"message":           message,
		"previous_provider": plan.previousProvider,
		"previous_model":    plan.previousModel,
		"previous_effort":   plan.previousEffort,
		"target_provider":   plan.requestedProvider,
		"target_model":      effectiveTargetModel(plan, candidate),
		"target_effort":     plan.requestedEffort,
	}
}

func effectiveTargetModel(plan responseModelSwapPlan, candidate *serveRuntime) string {
	model := strings.TrimSpace(plan.requestedModel)
	if model == "" && candidate != nil {
		model = strings.TrimSpace(candidate.defaultModel)
	}
	return model
}

func (s *serveServer) runModelSwapHandover(ctx context.Context, exec *responseModelSwapExecution) (*engine.HandoverResult, error) {
	if exec == nil || exec.candidate == nil {
		return nil, fmt.Errorf("model swap execution is not initialized")
	}
	messages := core.FilterConversationMessages(copyLLMMessageSlice(exec.previousHistory))
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages to hand over")
	}
	previousModel := strings.TrimSpace(exec.plan.previousModel)
	if previousModel == "" && exec.previous != nil {
		previousModel = strings.TrimSpace(exec.previous.defaultModel)
	}
	targetModel := effectiveTargetModel(exec.plan, exec.candidate)
	sourceLabel := modelswap.Label(exec.plan.previousProvider, previousModel)
	targetLabel := modelswap.Label(exec.plan.requestedProvider, targetModel)

	provider := core.Provider(nil)
	var helper *serveRuntime
	if s.runtimeFactory != nil {
		rt, err := s.runtimeFactory(ctx, serveRuntimeRequest{Provider: exec.plan.previousProvider, Model: previousModel})
		if err != nil {
			return nil, err
		}
		helper = rt
		provider = rt.provider
		defer helper.Close()
	} else if exec.previous != nil {
		provider = exec.previous.provider
	}
	if provider == nil {
		return nil, fmt.Errorf("previous provider is unavailable for handover")
	}
	config := engine.DefaultCompactionConfig()
	return llm.Handover(ctx, provider, previousModel, runtimeSystemPrompt(exec.previous), runtimeSystemPrompt(exec.candidate), messages, sourceLabel, targetLabel, config, engine.HandoverOptions{AllowProviderFork: true})
}

func runtimeSystemPrompt(rt *serveRuntime) string {
	if rt == nil {
		return ""
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.systemPrompt
}

func modelSwapMarkerMessage(plan responseModelSwapPlan, candidate *serveRuntime, status, strategy string) core.Message {
	marker := core.ModelSwapMarker{
		FromProvider: plan.previousProvider,
		FromModel:    plan.previousModel,
		FromEffort:   plan.previousEffort,
		ToProvider:   plan.requestedProvider,
		ToModel:      effectiveTargetModel(plan, candidate),
		ToEffort:     plan.requestedEffort,
		Strategy:     strategy,
		Status:       status,
	}
	return core.ModelSwapEventMessage(marker)
}

func setRuntimeHistoryPreserveEngine(rt *serveRuntime, history []core.Message) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	rt.history = copyLLMMessageSlice(history)
	rt.historyPersisted = false
	rt.mu.Unlock()
}

func (s *serveServer) persistModelSwapMarker(ctx context.Context, sessionID string, plan responseModelSwapPlan, candidate *serveRuntime, status, strategy string) {
	if sessionID == "" || !plan.enabled {
		return
	}
	msg := tagResponseRunMessage(ctx, modelSwapMarkerMessage(plan, candidate, status, strategy), -1)
	if candidate != nil {
		candidate.mu.Lock()
		defer candidate.mu.Unlock()
		if candidate.store == nil {
			candidate.store = s.store
		}
		if candidate.store != nil && s.writeModelSwapMarkerLocked(ctx, sessionID, candidate, msg) {
			return
		}
		// Without the stored active context, persist the in-memory history. The
		// store reads the compaction boundary in the write transaction and keeps
		// any archive.
		updatedHistory := modelswap.InsertMarker(candidate.history, msg)
		candidate.history = updatedHistory
		candidate.historyPersisted = false
		if candidate.store != nil {
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			candidate.historyPersisted = candidate.persistSnapshot(persistCtx, sessionID, updatedHistory)
		}
		return
	}
	if s.store != nil {
		sm := session.NewMessage(sessionID, msg, -1)
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = runResponseRunPersistence(ctx, []core.Message{msg}, func(fence session.ResponseRunFence) (int64, error) {
			return transcriptstore.AddMessageWithTranscriptRev(session.WithResponseRunFence(dbCtx, fence), s.store, sessionID, sm)
		})
	}
}

func (s *serveServer) restoreModelSwapRollback(ctx context.Context, sessionID string, exec *responseModelSwapExecution, candidate *serveRuntime, status, strategy string) {
	if exec == nil || !exec.plan.enabled {
		return
	}
	msg := tagResponseRunMessage(ctx, modelSwapMarkerMessage(exec.plan, candidate, status, strategy), -1)
	// Roll back provider selection, not the conversation. The candidate may
	// have already committed the trigger input and many tool rounds. Replacing
	// them with restoreHistory would erase those durable rows and their IDs.
	if s.store != nil && sessionID != "" {
		setRuntimeHistoryPreserveEngine(exec.previous, nil)
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = runResponseRunPersistence(ctx, []core.Message{msg}, func(fence session.ResponseRunFence) (int64, error) {
			return transcriptstore.AddMessageWithTranscriptRev(session.WithResponseRunFence(dbCtx, fence), s.store, sessionID, session.NewMessage(sessionID, msg, -1))
		})
		if exec.previous != nil {
			s.syncPersistedSessionRuntime(dbCtx, sessionID, exec.previous, exec.plan.previousModel, exec.plan.previousEffort, "", false, "", false)
		}
		return
	}
	// Without storage, retain whatever the candidate produced in memory.
	restoredHistory := copyLLMMessageSlice(exec.restoreHistory)
	if candidate != nil {
		if history := candidate.snapshotHistory(); len(history) > 0 {
			restoredHistory = history
		}
	}
	setRuntimeHistoryPreserveEngine(exec.previous, append(restoredHistory, msg))
}

// settleStoppedModelSwap resolves a swap whose target turn was cancelled or
// timed out. A stop ends the turn, not the switch: a target that began
// answering has accepted the carried-over context and keeps the session.
func (s *serveServer) settleStoppedModelSwap(ctx context.Context, sessionID string, exec *responseModelSwapExecution, candidate *serveRuntime, responded bool, strategy string) {
	// A run that lost its lease no longer owns the session's runtime identity.
	if run := responseRunFromContext(ctx); responded && run != nil && run.validateLifecycle != nil && run.validateLifecycle() != nil {
		responded = false
	}
	if !responded {
		exec.markRolledBack()
		// The candidate may have committed input; reload it when resuming.
		if exec.previous != nil && exec.previous.store != nil {
			setRuntimeHistoryPreserveEngine(exec.previous, nil)
		}
		return
	}
	exec.markCommitted()
	if s.store == nil || sessionID == "" {
		return
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	s.syncPersistedSessionRuntime(dbCtx, sessionID, candidate, effectiveTargetModel(exec.plan, candidate), exec.plan.requestedEffort, exec.plan.requestedReasoningMode, true, "", false)
	candidate.mu.Lock()
	defer candidate.mu.Unlock()
	if candidate.store == nil {
		candidate.store = s.store
	}
	// Put the marker where a completed swap does, before the target's output.
	// No in-memory fallback: persistSnapshot would mark the session active again.
	s.writeModelSwapMarkerLocked(ctx, sessionID, candidate, tagResponseRunMessage(ctx, modelSwapMarkerMessage(exec.plan, candidate, "succeeded", strategy), -1))
}

// writeModelSwapMarkerLocked records msg after the trigger in the stored
// active context and in candidate's history. It rewrites stored rows, never
// provider messages, so every row keeps the fields a round trip would drop.
// It runs under candidate.mu and reports whether the marker was stored.
func (s *serveServer) writeModelSwapMarkerLocked(ctx context.Context, sessionID string, candidate *serveRuntime, msg core.Message) bool {
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	rows, err := modelswap.MarkerRows(dbCtx, candidate.store, sessionID, *session.NewMessage(sessionID, msg, -1))
	if err == nil {
		_, err = runResponseRunPersistence(ctx, []core.Message{msg}, func(fence session.ResponseRunFence) (int64, error) {
			return transcriptstore.WriteTranscriptWithRev(session.WithResponseRunFence(dbCtx, fence), candidate.store, sessionID, rows, false)
		})
	}
	if err != nil {
		log.Printf("[serve] persist model swap marker for %s: %v", sessionID, err)
		return false
	}
	responseRunFromContext(ctx).invalidateDurableBoundary() // rewritten rows have new IDs
	candidate.refreshSessionMetaAfterSnapshot(dbCtx, sessionID, nil)
	if candidate.historyPersisted {
		candidate.history = modelswap.InsertMarker(candidate.history, msg)
	} else {
		candidate.history = nil // reload the stored transcript, marker included
	}
	return true
}

func addRuntimeUsage(rt *serveRuntime, usage core.Usage) {
	if rt == nil || usage.IsZero() {
		return
	}
	rt.mu.Lock()
	rt.cumulativeUsage.Add(usage)
	rt.mu.Unlock()
}

func (s *serveServer) recordModelSwapHandoverUsage(ctx context.Context, sessionID string, exec *responseModelSwapExecution, usage core.Usage) {
	if exec == nil || usage.IsZero() {
		return
	}

	// The source runtime survives rollback, so account there as soon as the
	// helper finishes. The candidate receives the same cumulative usage after
	// its retry history is seeded; only one of the two runtimes survives.
	addRuntimeUsage(exec.previous, usage)
	model := strings.TrimSpace(exec.plan.previousModel)
	if model == "" && exec.previous != nil {
		model = exec.previous.defaultModel
	}
	if exec.previous != nil {
		exec.previous.recordHelperStats("handover", model, usage)
	}
	if exec.candidate != nil && exec.candidate != exec.previous {
		exec.candidate.recordHelperStats("handover", model, usage)
	}

	if s.store == nil || sessionID == "" || usage.BillableCountersZero() {
		return
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.store.UpdateMetrics(dbCtx, sessionID, 0, 0, usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens, usage.CacheWriteTokens); err != nil {
		log.Printf("[serve] failed to persist model-swap handover usage for %s: %v", sessionID, err)
		return
	}
	appruntime.RecordStoreModelUsage(dbCtx, s.store, sessionID, model, session.ModelUsageHandover, usage, 1, 0)
	if exec.previous != nil {
		exec.previous.mu.Lock()
		if meta := exec.previous.sessionMeta; meta != nil {
			meta.InputTokens += usage.InputTokens
			meta.OutputTokens += usage.OutputTokens
			meta.CachedInputTokens += usage.CachedInputTokens
			meta.CacheWriteTokens += usage.CacheWriteTokens
		}
		exec.previous.mu.Unlock()
	}
}

func (s *serveServer) executeResponseRunModelSwap(runCtx context.Context, runtime *serveRuntime, run *responseRun, stateful bool, replaceHistory bool, inputMessages []core.Message, llmReq core.Request, sessionID, respID, model string, created int64, options startResponseRunOptions) {
	exec := options.modelSwap
	mgr := s.ensureResponseRuns()
	if exec == nil || !exec.plan.enabled {
		return
	}
	appendProgress := func(stage, message string) {
		_ = run.appendEvent("response.model_swap.progress", modelSwapProgressPayload(stage, message, exec.plan, runtime))
	}
	continuationID := func() string {
		owner := runtime
		if exec.rolledBack && exec.previous != nil {
			owner = exec.previous
		}
		return s.responseRunContinuationID(runCtx, owner, sessionID, respID)
	}
	failRun := func(errType string, err error) {
		message := "model swap failed"
		if err != nil {
			message = err.Error()
		}
		hadSubscribers, failErr := run.fail(map[string]any{
			"response": map[string]any{
				"id":      continuationID(),
				"object":  "response",
				"created": created,
				"model":   model,
				"status":  "failed",
			},
			"error": map[string]any{
				"message": message,
				"type":    errType,
			},
		}, errType, message)
		if options.uiSession {
			switch {
			case hadSubscribers:
				runtime.clearLastUIRunError()
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				runtime.clearLastUIRunError()
			default:
				runtime.setLastUIRunError(message)
			}
		}
		if failErr != nil {
			_ = failErr
		}
	}
	failRunForError := func(err error) {
		if responseRunTimedOut(runCtx) {
			failRun("timeout_error", errors.New(responseRunTimeoutMessage(s.responseTimeout())))
			return
		}
		failRun("invalid_request_error", err)
	}
	// finishStopped ends a turn stopped by timeout or cancellation; the switch
	// itself survives only if the target had begun answering.
	finishStopped := func(err error, responded bool, strategy string) bool {
		timedOut := responseRunTimedOut(runCtx)
		if !timedOut && !modelswap.Stopped(err) {
			return false
		}
		s.settleStoppedModelSwap(runCtx, sessionID, exec, runtime, responded, strategy)
		if timedOut {
			failRun("timeout_error", errors.New(responseRunTimeoutMessage(s.responseTimeout())))
			return true
		}
		cancelled, _ := run.finishCancelled(map[string]any{
			"response": map[string]any{"id": continuationID(), "object": "response", "created": created, "model": model, "status": "cancelled"},
		})
		if cancelled && options.uiSession {
			runtime.clearLastUIRunError()
		}
		return true
	}

	appendProgress("naive_start", exec.plan.startMessage(runtime))
	visible, responded := false, false
	streamState := &responseRunStreamState{}
	result, err := runtime.RunWithEventsAndStart(runCtx, true, false, inputMessages, llmReq, func() {
		mgr.setActiveRun(sessionID, respID)
	}, func(ev core.Event) error {
		visible = visible || modelswap.VisibleOutput(ev)
		responded = responded || modelswap.Responded(ev)
		return s.appendResponseRunEvent(runtime, run, streamState, ev)
	})
	if err == nil {
		if options.uiSession {
			runtime.clearLastUIRunError()
		}
		if options.resetResponseIDsOnSuccess {
			s.unregisterSessionResponseIDs(sessionID)
		}
		exec.markCommitted()
		s.syncPersistedSessionRuntime(runCtx, sessionID, runtime, effectiveTargetModel(exec.plan, runtime), exec.plan.requestedEffort, exec.plan.requestedReasoningMode, true, "", false)
		s.persistModelSwapMarker(runCtx, sessionID, exec.plan, runtime, "succeeded", "naive")
		appendProgress("complete", fmt.Sprintf("Continuing on %s.", exec.plan.targetLabel(runtime)))
		completeResponse := map[string]any{
			"id":            continuationID(),
			"object":        "response",
			"created":       created,
			"model":         model,
			"provider":      exec.plan.requestedProvider,
			"status":        "completed",
			"usage":         usagePayload(result.Usage),
			"session_usage": usagePayload(result.SessionUsage),
			"context_usage": result.ContextUsage,
			// The client tool calls this response stopped on; authoritative even when empty.
			"pending_client_calls": pendingClientCallsPayload(result.PendingClientCalls()),
		}
		if effort := strings.TrimSpace(exec.plan.requestedEffort); effort != "" {
			completeResponse["reasoning_effort"] = effort
		}
		if err := run.complete(map[string]any{"response": completeResponse}, result.Usage, result.SessionUsage); err != nil {
			// Keep parity with the normal path: best-effort terminal event.
			_ = err
			return
		}
		s.scheduleAutoTitle(sessionID, runtime.providerKey)
		return
	}

	if finishStopped(err, responded, "naive") {
		return
	}

	if visible || !exec.plan.fallbackHandover || !isModelSwapFallbackEligible(err) {
		exec.markRolledBack()
		s.restoreModelSwapRollback(runCtx, sessionID, exec, runtime, "failed", "naive")
		appendProgress("failed", fmt.Sprintf("Model swap failed; restored %s.", exec.plan.previousLabel()))
		errType := "invalid_request_error"
		if errors.Is(err, errServeSessionBusy) {
			errType = "conflict_error"
		}
		failRun(errType, err)
		return
	}

	appendProgress("handover_start", fmt.Sprintf("Naive continuation failed; preparing handover from %s…", exec.plan.previousLabel()))
	handover, handoverErr := s.runModelSwapHandover(runCtx, exec)
	if handoverErr != nil && finishStopped(handoverErr, false, "handover") {
		return
	}
	if handoverErr != nil {
		exec.markRolledBack()
		s.restoreModelSwapRollback(runCtx, sessionID, exec, runtime, "failed", "handover")
		appendProgress("failed", fmt.Sprintf("Model swap failed; restored %s.", exec.plan.previousLabel()))
		failRunForError(modelswap.CombinedError(err, handoverErr))
		return
	}

	s.recordModelSwapHandoverUsage(runCtx, sessionID, exec, handover.Usage)
	appendProgress("handover_done", fmt.Sprintf("Handover ready; retrying on %s…", exec.plan.targetLabel(runtime)))
	seedRuntimeHistory(runtime, nil, false)
	fallbackInput := append(copyLLMMessageSlice(handover.NewMessages), inputMessages...)
	streamState = &responseRunStreamState{thought: streamState.thought.Continue()}
	retryResponded := false
	result, retryErr := runtime.RunWithEventsAndStart(runCtx, true, true, fallbackInput, llmReq, func() {
		mgr.setActiveRun(sessionID, respID)
	}, func(ev core.Event) error {
		retryResponded = retryResponded || modelswap.Responded(ev)
		return s.appendResponseRunEvent(runtime, run, streamState, ev)
	})
	if retryErr != nil && finishStopped(retryErr, retryResponded, "handover") {
		return
	}
	if retryErr != nil {
		exec.markRolledBack()
		s.restoreModelSwapRollback(runCtx, sessionID, exec, runtime, "failed", "handover")
		appendProgress("failed", fmt.Sprintf("Model swap failed; restored %s.", exec.plan.previousLabel()))
		failRunForError(modelswap.CombinedError(err, retryErr))
		return
	}
	addRuntimeUsage(runtime, handover.Usage)
	result.Usage.Add(handover.Usage)
	result.SessionUsage.Add(handover.Usage)

	if options.uiSession {
		runtime.clearLastUIRunError()
	}
	if options.resetResponseIDsOnSuccess {
		s.unregisterSessionResponseIDs(sessionID)
	}
	exec.markCommitted()
	s.syncPersistedSessionRuntime(runCtx, sessionID, runtime, effectiveTargetModel(exec.plan, runtime), exec.plan.requestedEffort, exec.plan.requestedReasoningMode, true, "", false)
	s.persistModelSwapMarker(runCtx, sessionID, exec.plan, runtime, "succeeded", "handover")
	appendProgress("complete", fmt.Sprintf("Continuing on %s.", exec.plan.targetLabel(runtime)))
	completeResponse := map[string]any{
		"id":            continuationID(),
		"object":        "response",
		"created":       created,
		"model":         model,
		"provider":      exec.plan.requestedProvider,
		"status":        "completed",
		"usage":         usagePayload(result.Usage),
		"session_usage": usagePayload(result.SessionUsage),
		"context_usage": result.ContextUsage,
		// The client tool calls this response stopped on; authoritative even when empty.
		"pending_client_calls": pendingClientCallsPayload(result.PendingClientCalls()),
	}
	if effort := strings.TrimSpace(exec.plan.requestedEffort); effort != "" {
		completeResponse["reasoning_effort"] = effort
	}
	if err := run.complete(map[string]any{"response": completeResponse}, result.Usage, result.SessionUsage); err == nil {
		s.scheduleAutoTitle(sessionID, runtime.providerKey)
	}
}

func (s *serveServer) runResponseWithModelSwapFallback(ctx context.Context, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []core.Message, llmReq core.Request, sessionID string, exec *responseModelSwapExecution) (serveRunResult, string, error) {
	if exec == nil || !exec.plan.enabled {
		result, err := runtime.Run(ctx, stateful, replaceHistory, inputMessages, llmReq)
		return result, "", err
	}
	visible, responded := false, false
	result, err := runtime.RunWithEvents(ctx, true, false, inputMessages, llmReq, func(ev core.Event) error {
		visible = visible || modelswap.VisibleOutput(ev)
		responded = responded || modelswap.Responded(ev)
		return nil
	})
	if err == nil {
		exec.markCommitted()
		s.syncPersistedSessionRuntime(ctx, sessionID, runtime, effectiveTargetModel(exec.plan, runtime), exec.plan.requestedEffort, exec.plan.requestedReasoningMode, true, "", false)
		s.persistModelSwapMarker(ctx, sessionID, exec.plan, runtime, "succeeded", "naive")
		return result, "naive", nil
	}
	if responded && modelswap.Stopped(err) {
		s.settleStoppedModelSwap(ctx, sessionID, exec, runtime, true, "naive")
		return serveRunResult{}, "", err
	}
	if visible || !exec.plan.fallbackHandover || !isModelSwapFallbackEligible(err) {
		exec.markRolledBack()
		s.restoreModelSwapRollback(ctx, sessionID, exec, runtime, "failed", "naive")
		return serveRunResult{}, "", err
	}

	handover, handoverErr := s.runModelSwapHandover(ctx, exec)
	if handoverErr != nil {
		exec.markRolledBack()
		s.restoreModelSwapRollback(ctx, sessionID, exec, runtime, "failed", "handover")
		return serveRunResult{}, "", modelswap.CombinedError(err, handoverErr)
	}
	s.recordModelSwapHandoverUsage(ctx, sessionID, exec, handover.Usage)
	seedRuntimeHistory(runtime, nil, false)
	fallbackInput := append(copyLLMMessageSlice(handover.NewMessages), inputMessages...)
	retryResponded := false
	result, retryErr := runtime.RunWithEvents(ctx, true, true, fallbackInput, llmReq, func(ev core.Event) error {
		retryResponded = retryResponded || modelswap.Responded(ev)
		return nil
	})
	if retryErr != nil && retryResponded && modelswap.Stopped(retryErr) {
		s.settleStoppedModelSwap(ctx, sessionID, exec, runtime, true, "handover")
		return serveRunResult{}, "", modelswap.CombinedError(err, retryErr)
	}
	if retryErr != nil {
		exec.markRolledBack()
		s.restoreModelSwapRollback(ctx, sessionID, exec, runtime, "failed", "handover")
		return serveRunResult{}, "", modelswap.CombinedError(err, retryErr)
	}
	addRuntimeUsage(runtime, handover.Usage)
	result.Usage.Add(handover.Usage)
	result.SessionUsage.Add(handover.Usage)
	exec.markCommitted()
	s.syncPersistedSessionRuntime(ctx, sessionID, runtime, effectiveTargetModel(exec.plan, runtime), exec.plan.requestedEffort, exec.plan.requestedReasoningMode, true, "", false)
	s.persistModelSwapMarker(ctx, sessionID, exec.plan, runtime, "succeeded", "handover")
	return result, "handover", nil
}
