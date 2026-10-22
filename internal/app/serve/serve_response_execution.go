package serve

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) executeSynchronousResponse(ctx context.Context, w http.ResponseWriter, runtime *serveRuntime, stateful, replaceHistory bool, inputMessages []core.Message, llmReq core.Request, sessionID, previousResponseID string, modelSwapExec *responseModelSwapExecution, resetResponseIDsOnSuccess bool) {
	if stateful && s.sessionMgr != nil {
		var retained *serveRuntime
		if modelSwapExec != nil {
			retained = modelSwapExec.previous
		}
		releaseAdmission, admissionErr := s.sessionMgr.admitSynchronousActivity(sessionID, runtime, retained)
		if admissionErr != nil {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", admissionErr.Error())
			return
		}
		defer releaseAdmission()
	}
	// Synchronous API invocations do not acquire an OS destination, but an
	// admitted human must still retire prior held candidates. Runtime setup runs
	// only after tryLockRun succeeds, never on busy/rejected handler paths.
	if turn := s.notificationTurnAdmission(ctx, inputMessages, previousResponseID); turn != nil && turn.Human {
		if turns, ok := session.AsNotificationTurnStore(s.store); ok {
			previousSetup := serveRuntimeSetupFromContext(ctx)
			var once sync.Once
			var supersedeErr error
			ctx = withServeRuntimeSetup(ctx, func(req *core.Request) error {
				if previousSetup != nil {
					if err := previousSetup(req); err != nil {
						return err
					}
				}
				once.Do(func() { supersedeErr = turns.SupersedeSessionNotificationTurn(ctx, sessionID) })
				return supersedeErr
			})
		}
	}
	result, _, err := s.runResponseWithModelSwapFallback(ctx, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, modelSwapExec)
	if err != nil {
		if errors.Is(err, errServeSessionBusy) {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			writeOpenAIError(w, http.StatusRequestTimeout, "timeout_error", responseRunTimeoutMessage(s.responseTimeout()))
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if s.cfg.suppressServerTools {
		filtered := make([]core.ToolCall, 0, len(result.ToolCalls))
		for _, call := range result.ToolCalls {
			if !runtime.isServerExecutedTool(call.Name) {
				filtered = append(filtered, call)
			}
		}
		result.ToolCalls = filtered
	}

	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}

	setSessionNumberHeader(w, runtime)

	created := time.Now().Unix()
	respID, err := s.storeCompletedResponseRun(runtime, sessionID, previousResponseID, model, created, result, resetResponseIDsOnSuccess)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	s.scheduleAutoTitle(sessionID, runtime.providerKey)

	writeJSON(w, http.StatusOK, responsesFinalResponse(result, model, respID, created))
}
