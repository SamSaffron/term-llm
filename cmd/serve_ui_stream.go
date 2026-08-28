package cmd

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/samsaffron/term-llm/internal/llm"
)

func (s *serveServer) streamUIResponses(w http.ResponseWriter, r *http.Request, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string, previousResponseID string, resetResponseIDsOnSuccess bool, modelSwap *responseModelSwapExecution, idempotencyKey, idempotencyScope, requestFingerprint, notificationSubscriptionID string, onDone func()) {
	// Persist session in the store so the client gets the session number in
	// headers before the streaming body begins. This is a store-only operation
	// that does NOT mutate runtime state (safe without rt.mu).
	if num := runtime.ensureSessionInStore(r.Context(), sessionID, inputMessages); num > 0 {
		w.Header().Set("x-session-number", strconv.FormatInt(num, 10))
	}

	s.streamResponseRun(r.Context(), w, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, startResponseRunOptions{
		previousResponseID:         previousResponseID,
		uiSession:                  true,
		resetResponseIDsOnSuccess:  resetResponseIDsOnSuccess,
		modelSwap:                  modelSwap,
		idempotencyKey:             idempotencyKey,
		idempotencyScope:           idempotencyScope,
		requestFingerprint:         requestFingerprint,
		notificationSubscriptionID: notificationSubscriptionID,
		onDone:                     onDone,
	})
}

func (s *serveServer) streamResponseRun(ctx context.Context, w http.ResponseWriter, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string, options startResponseRunOptions) bool {
	run, err := s.startResponseRun(runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, options)
	if err != nil {
		if options.modelSwap != nil && options.modelSwap.plan.enabled {
			options.modelSwap.markRolledBack()
			s.restoreModelSwapRollback(ctx, sessionID, options.modelSwap, runtime, "failed", "naive")
		}
		status := http.StatusInternalServerError
		errType := "server_error"
		if errors.Is(err, errResponseRunKeyConflict) {
			status = http.StatusConflict
			errType = "conflict_error"
		}
		writeOpenAIError(w, status, errType, err.Error())
		return false
	}
	w.Header().Set("x-response-id", run.id)
	s.streamResponseRunEvents(ctx, w, run, 0)
	return true
}
