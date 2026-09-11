package cmd

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

func (s *serveServer) executeResponseRun(runCtx context.Context, releaseReload, closeTask, cancel func(), runTimer *responseRunTimer, mgr *responseRunManager, runtime *serveRuntime, run *responseRun, stateful, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID, respID, model string, created int64, options startResponseRunOptions) {
	defer closeTask()
	defer func() { releaseReload() }()
	defer close(run.settled)
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
	if options.resume != nil {
		streamState = options.resume.streamState()
	}
	var result serveRunResult
	var totalUsage llm.Usage
	if options.resume != nil {
		totalUsage = options.resume.View.Usage
	}
	var err error
	for {
		runtimeRunCtx := withServeRuntimeSetup(runCtx, options.runtimeSetup)
		result, err = runtime.RunWithEventsAndStart(runtimeRunCtx, stateful, replaceHistory, inputMessages, llmReq, func() {
			mgr.setActiveRun(sessionID, respID)
		}, func(ev llm.Event) error { return s.appendResponseRunEvent(runtime, run, streamState, ev) })
		totalUsage.Add(result.Usage)
		result.Usage = totalUsage
		var suspended *llm.SuspendedError
		if !errors.As(err, &suspended) {
			break
		}
		run.mu.Lock()
		run.usage, run.sessionUsage = result.Usage, result.SessionUsage
		run.mu.Unlock()
		runCtx, err = s.pauseResponseRun(run, runtime, streamState, suspended.Continuation, stateful, options, runCtx, &releaseReload, runTimer)
		if err != nil {
			break
		}
		llmReq = suspended.Continuation.Request
		llmReq.Resume = suspended.Continuation
		inputMessages = nil
		replaceHistory = false
	}
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
		"context_usage": result.ContextUsage,
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
}
