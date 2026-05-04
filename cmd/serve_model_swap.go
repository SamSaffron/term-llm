package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type responseRuntimeSettings struct {
	provider string
	model    string
	effort   string
}

type responseModelSwapPlan struct {
	enabled           bool
	fallbackHandover  bool
	requestedProvider string
	requestedModel    string
	requestedEffort   string
	previousProvider  string
	previousModel     string
	previousEffort    string
}

type responseModelSwapExecution struct {
	plan            responseModelSwapPlan
	candidate       *serveRuntime
	previous        *serveRuntime
	previousHistory []llm.Message
	restoreHistory  []llm.Message
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
	return responseRuntimeSettings{
		provider: provider,
		model:    strings.TrimSpace(req.Model),
		effort:   normalizeReasoningEffort(req.ReasoningEffort),
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
		provider = resolveSessionProviderKey(s.cfgRef, sess)
	}
	if provider == "" {
		provider = strings.TrimSpace(defaultProvider)
	}
	settings.provider = provider
	settings.model = strings.TrimSpace(sess.Model)
	settings.effort = normalizeReasoningEffort(sess.ReasoningEffort)
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
	return providerModelLabel(p.previousProvider, p.previousModel)
}

func (p responseModelSwapPlan) targetLabel(candidate *serveRuntime) string {
	model := strings.TrimSpace(p.requestedModel)
	if model == "" && candidate != nil {
		model = strings.TrimSpace(candidate.defaultModel)
	}
	return providerModelLabel(p.requestedProvider, model)
}

func providerModelLabel(provider, model string) string {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	switch {
	case provider != "" && model != "":
		return provider + ":" + model
	case provider != "":
		return provider
	case model != "":
		return model
	default:
		return "model"
	}
}

func copyLLMMessageSlice(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]llm.Message, len(messages))
	copy(out, messages)
	return out
}

func (s *serveServer) loadPersistedLLMHistory(ctx context.Context, sessionID string) []llm.Message {
	if s.store == nil || sessionID == "" {
		return nil
	}
	stored, err := s.store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil || len(stored) == 0 {
		return nil
	}
	messages := make([]llm.Message, 0, len(stored))
	for _, msg := range stored {
		messages = append(messages, msg.ToLLMMessage())
	}
	return messages
}

func (s *serveServer) beginResponseModelSwap(ctx context.Context, sessionID string, plan responseModelSwapPlan, inputMessages []llm.Message) (*responseModelSwapExecution, error) {
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
	restoreHistory := previous.snapshotHistory()
	if restoreHistory == nil {
		return nil, errServeSessionBusy
	}
	if len(restoreHistory) == 0 {
		restoreHistory = s.loadPersistedLLMHistory(ctx, sessionID)
	}
	restoreHistory = copyLLMMessageSlice(restoreHistory)
	previousHistory := llm.FilterConversationMessages(copyLLMMessageSlice(restoreHistory))

	create := func(ctx context.Context) (*serveRuntime, error) {
		if s.runtimeFactory == nil {
			return nil, fmt.Errorf("runtime factory is unavailable for model swap")
		}
		return s.runtimeFactory(ctx, plan.requestedProvider, plan.requestedModel)
	}
	candidate, retainedPrevious, commit, rollback, err := s.sessionMgr.BeginSwap(ctx, sessionID, create)
	if err != nil {
		return nil, err
	}
	if retainedPrevious != nil && retainedPrevious != previous {
		retainedPrevious.ensurePersistedSession(ctx, sessionID, inputMessages)
		if hist := retainedPrevious.snapshotHistory(); hist != nil {
			restoreHistory = copyLLMMessageSlice(hist)
			previousHistory = llm.FilterConversationMessages(copyLLMMessageSlice(hist))
		}
	}
	seedRuntimeHistory(candidate, previousHistory)
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

func seedRuntimeHistory(rt *serveRuntime, history []llm.Message) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.history = copyLLMMessageSlice(history)
	rt.cumulativeUsage = llm.Usage{}
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

func modelSwapVisibleEvent(ev llm.Event) bool {
	switch ev.Type {
	case llm.EventTextDelta:
		return ev.Text != ""
	case llm.EventToolCall, llm.EventToolExecStart, llm.EventToolExecEnd, llm.EventInterjection, llm.EventImageGenerated:
		return true
	default:
		return false
	}
}

func isModelSwapFallbackEligible(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errServeSessionBusy) || errors.Is(err, errServeSessionLimitReached) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, deny := range []string{"401", "403", "429", "unauthorized", "forbidden", "rate limit", "ratelimit", "quota", "insufficient", "model not found", "unknown model"} {
		if strings.Contains(msg, deny) {
			return false
		}
	}
	for _, allow := range []string{"400", "invalid_request", "invalid request", "messages", "reasoning", "thinking", "encrypted", "tool_use", "tool_result", "function_call", "role", "signature"} {
		if strings.Contains(msg, allow) {
			return true
		}
	}
	return strings.Contains(msg, "invalid")
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

func (s *serveServer) runModelSwapHandover(ctx context.Context, exec *responseModelSwapExecution) (*llm.HandoverResult, error) {
	if exec == nil || exec.candidate == nil {
		return nil, fmt.Errorf("model swap execution is not initialized")
	}
	messages := llm.FilterConversationMessages(copyLLMMessageSlice(exec.previousHistory))
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages to hand over")
	}
	previousModel := strings.TrimSpace(exec.plan.previousModel)
	if previousModel == "" && exec.previous != nil {
		previousModel = strings.TrimSpace(exec.previous.defaultModel)
	}
	targetModel := effectiveTargetModel(exec.plan, exec.candidate)
	sourceLabel := providerModelLabel(exec.plan.previousProvider, previousModel)
	targetLabel := providerModelLabel(exec.plan.requestedProvider, targetModel)

	provider := llm.Provider(nil)
	var helper *serveRuntime
	if s.runtimeFactory != nil {
		rt, err := s.runtimeFactory(ctx, exec.plan.previousProvider, previousModel)
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
	config := llm.DefaultCompactionConfig()
	return llm.Handover(ctx, provider, previousModel, runtimeSystemPrompt(exec.previous), runtimeSystemPrompt(exec.candidate), messages, sourceLabel, targetLabel, config)
}

func runtimeSystemPrompt(rt *serveRuntime) string {
	if rt == nil {
		return ""
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.systemPrompt
}

func modelSwapCombinedError(naiveErr, retryErr error) error {
	if naiveErr == nil {
		return retryErr
	}
	if retryErr == nil {
		return naiveErr
	}
	return fmt.Errorf("model swap naive continuation failed: %v; handover retry failed: %w", naiveErr, retryErr)
}

func modelSwapMarkerMessage(plan responseModelSwapPlan, candidate *serveRuntime, status, strategy string) llm.Message {
	marker := llm.ModelSwapMarker{
		FromProvider: plan.previousProvider,
		FromModel:    plan.previousModel,
		FromEffort:   plan.previousEffort,
		ToProvider:   plan.requestedProvider,
		ToModel:      effectiveTargetModel(plan, candidate),
		ToEffort:     plan.requestedEffort,
		Strategy:     strategy,
		Status:       status,
	}
	return llm.ModelSwapEventMessage(marker)
}

func appendRuntimeHistoryEvent(rt *serveRuntime, msg llm.Message) {
	if rt == nil || msg.Role == "" {
		return
	}
	rt.mu.Lock()
	rt.history = append(rt.history, msg)
	rt.mu.Unlock()
}

func setRuntimeHistoryPreserveEngine(rt *serveRuntime, history []llm.Message) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	rt.history = copyLLMMessageSlice(history)
	rt.mu.Unlock()
}

func (s *serveServer) persistModelSwapMarker(ctx context.Context, sessionID string, plan responseModelSwapPlan, candidate *serveRuntime, status, strategy string) {
	if sessionID == "" || !plan.enabled {
		return
	}
	msg := modelSwapMarkerMessage(plan, candidate, status, strategy)
	if s.store != nil {
		sm := session.NewMessage(sessionID, msg, -1)
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = s.store.AddMessage(dbCtx, sessionID, sm)
	}
	appendRuntimeHistoryEvent(candidate, msg)
}

func (s *serveServer) restoreModelSwapRollback(ctx context.Context, sessionID string, exec *responseModelSwapExecution, candidate *serveRuntime, status, strategy string) {
	if exec == nil || !exec.plan.enabled {
		return
	}
	msg := modelSwapMarkerMessage(exec.plan, candidate, status, strategy)
	restoredHistory := copyLLMMessageSlice(exec.restoreHistory)
	restoredHistory = append(restoredHistory, msg)
	if exec.previous != nil {
		setRuntimeHistoryPreserveEngine(exec.previous, restoredHistory)
	}
	if s.store != nil && sessionID != "" {
		dbMessages := make([]session.Message, 0, len(restoredHistory))
		for _, llmMsg := range restoredHistory {
			if llmMsg.Role == "" {
				continue
			}
			dbMessages = append(dbMessages, *session.NewMessage(sessionID, llmMsg, -1))
		}
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = s.store.ReplaceMessages(dbCtx, sessionID, dbMessages)
		if exec.previous != nil {
			s.syncPersistedSessionRuntime(dbCtx, sessionID, exec.previous, exec.plan.previousModel, exec.plan.previousEffort)
		}
	}
}

func (s *serveServer) executeResponseRunModelSwap(runCtx context.Context, runtime *serveRuntime, run *responseRun, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID, respID, model string, created int64, options startResponseRunOptions) {
	exec := options.modelSwap
	mgr := s.ensureResponseRuns()
	if exec == nil || !exec.plan.enabled {
		return
	}
	appendProgress := func(stage, message string) {
		_ = run.appendEvent("response.model_swap.progress", modelSwapProgressPayload(stage, message, exec.plan, runtime))
	}
	failRun := func(errType string, err error) {
		message := "model swap failed"
		if err != nil {
			message = err.Error()
		}
		hadSubscribers, failErr := run.fail(map[string]any{
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

	appendProgress("naive_start", fmt.Sprintf("Switching model: %s → %s; trying existing context…", exec.plan.previousLabel(), exec.plan.targetLabel(runtime)))
	visible := false
	streamState := &responseRunStreamState{}
	result, err := runtime.RunWithEventsAndStart(runCtx, true, false, inputMessages, llmReq, func() {
		mgr.setActiveRun(sessionID, respID)
	}, func(ev llm.Event) error {
		if modelSwapVisibleEvent(ev) {
			visible = true
		}
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
		s.syncPersistedSessionRuntime(runCtx, sessionID, runtime, effectiveTargetModel(exec.plan, runtime), exec.plan.requestedEffort)
		s.persistModelSwapMarker(runCtx, sessionID, exec.plan, runtime, "succeeded", "naive")
		s.registerResponseID(runtime, respID, sessionID)
		appendProgress("complete", fmt.Sprintf("Continuing on %s.", exec.plan.targetLabel(runtime)))
		completeResponse := map[string]any{
			"id":            respID,
			"object":        "response",
			"created":       created,
			"model":         model,
			"provider":      exec.plan.requestedProvider,
			"status":        "completed",
			"usage":         usagePayload(result.Usage),
			"session_usage": usagePayload(result.SessionUsage),
		}
		if effort := strings.TrimSpace(exec.plan.requestedEffort); effort != "" {
			completeResponse["reasoning_effort"] = effort
		}
		if err := run.complete(map[string]any{"response": completeResponse}, result.Usage, result.SessionUsage); err != nil {
			// Keep parity with the normal path: best-effort terminal event.
			_ = err
		}
		return
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		exec.markRolledBack()
		cancelled, _ := run.finishCancelled(map[string]any{
			"response": map[string]any{
				"id":      respID,
				"object":  "response",
				"created": created,
				"model":   model,
				"status":  "cancelled",
			},
		})
		if cancelled && options.uiSession {
			runtime.clearLastUIRunError()
		}
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
	if handoverErr != nil {
		exec.markRolledBack()
		s.restoreModelSwapRollback(runCtx, sessionID, exec, runtime, "failed", "handover")
		appendProgress("failed", fmt.Sprintf("Model swap failed; restored %s.", exec.plan.previousLabel()))
		failRun("invalid_request_error", modelSwapCombinedError(err, handoverErr))
		return
	}

	appendProgress("handover_done", fmt.Sprintf("Handover ready; retrying on %s…", exec.plan.targetLabel(runtime)))
	seedRuntimeHistory(runtime, nil)
	fallbackInput := append(copyLLMMessageSlice(handover.NewMessages), inputMessages...)
	streamState = &responseRunStreamState{}
	result, retryErr := runtime.RunWithEventsAndStart(runCtx, true, true, fallbackInput, llmReq, func() {
		mgr.setActiveRun(sessionID, respID)
	}, func(ev llm.Event) error {
		return s.appendResponseRunEvent(runtime, run, streamState, ev)
	})
	if retryErr != nil {
		exec.markRolledBack()
		s.restoreModelSwapRollback(runCtx, sessionID, exec, runtime, "failed", "handover")
		appendProgress("failed", fmt.Sprintf("Model swap failed; restored %s.", exec.plan.previousLabel()))
		failRun("invalid_request_error", modelSwapCombinedError(err, retryErr))
		return
	}

	if options.uiSession {
		runtime.clearLastUIRunError()
	}
	if options.resetResponseIDsOnSuccess {
		s.unregisterSessionResponseIDs(sessionID)
	}
	exec.markCommitted()
	s.syncPersistedSessionRuntime(runCtx, sessionID, runtime, effectiveTargetModel(exec.plan, runtime), exec.plan.requestedEffort)
	s.persistModelSwapMarker(runCtx, sessionID, exec.plan, runtime, "succeeded", "handover")
	s.registerResponseID(runtime, respID, sessionID)
	appendProgress("complete", fmt.Sprintf("Continuing on %s.", exec.plan.targetLabel(runtime)))
	completeResponse := map[string]any{
		"id":            respID,
		"object":        "response",
		"created":       created,
		"model":         model,
		"provider":      exec.plan.requestedProvider,
		"status":        "completed",
		"usage":         usagePayload(result.Usage),
		"session_usage": usagePayload(result.SessionUsage),
	}
	if effort := strings.TrimSpace(exec.plan.requestedEffort); effort != "" {
		completeResponse["reasoning_effort"] = effort
	}
	_ = run.complete(map[string]any{"response": completeResponse}, result.Usage, result.SessionUsage)
}

func (s *serveServer) runResponseWithModelSwapFallback(ctx context.Context, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string, exec *responseModelSwapExecution) (serveRunResult, string, error) {
	if exec == nil || !exec.plan.enabled {
		result, err := runtime.Run(ctx, stateful, replaceHistory, inputMessages, llmReq)
		return result, "", err
	}
	visible := false
	result, err := runtime.RunWithEvents(ctx, true, false, inputMessages, llmReq, func(ev llm.Event) error {
		if modelSwapVisibleEvent(ev) {
			visible = true
		}
		return nil
	})
	if err == nil {
		exec.markCommitted()
		s.syncPersistedSessionRuntime(ctx, sessionID, runtime, effectiveTargetModel(exec.plan, runtime), exec.plan.requestedEffort)
		s.persistModelSwapMarker(ctx, sessionID, exec.plan, runtime, "succeeded", "naive")
		return result, "naive", nil
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
		return serveRunResult{}, "", modelSwapCombinedError(err, handoverErr)
	}
	seedRuntimeHistory(runtime, nil)
	fallbackInput := append(copyLLMMessageSlice(handover.NewMessages), inputMessages...)
	result, retryErr := runtime.Run(ctx, true, true, fallbackInput, llmReq)
	if retryErr != nil {
		exec.markRolledBack()
		s.restoreModelSwapRollback(ctx, sessionID, exec, runtime, "failed", "handover")
		return serveRunResult{}, "", modelSwapCombinedError(err, retryErr)
	}
	exec.markCommitted()
	s.syncPersistedSessionRuntime(ctx, sessionID, runtime, effectiveTargetModel(exec.plan, runtime), exec.plan.requestedEffort)
	s.persistModelSwapMarker(ctx, sessionID, exec.plan, runtime, "succeeded", "handover")
	return result, "handover", nil
}
