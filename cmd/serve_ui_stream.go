package cmd

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

func isServeUIRequest(r *http.Request) bool {
	value := strings.TrimSpace(strings.ToLower(r.Header.Get("X-Term-LLM-UI")))
	return value == "1" || value == "true" || value == "yes"
}

func (s *serveServer) streamUIResponses(w http.ResponseWriter, r *http.Request, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string, previousResponseID string) {
	// Persist session in the store so the client gets the session number in
	// headers before the streaming body begins. This is a store-only operation
	// that does NOT mutate runtime state (safe without rt.mu).
	if num := runtime.ensureSessionInStore(r.Context(), sessionID, inputMessages); num > 0 {
		w.Header().Set("x-session-number", strconv.FormatInt(num, 10))
	}

	s.streamResponseRun(r.Context(), w, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, startResponseRunOptions{
		previousResponseID: previousResponseID,
		uiSession:          true,
	})
}

func (s *serveServer) streamResponseRun(ctx context.Context, w http.ResponseWriter, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string, options startResponseRunOptions) bool {
	run, err := s.startResponseRun(runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, options)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return false
	}
	w.Header().Set("x-response-id", run.id)
	s.streamResponseRunEvents(ctx, w, run, 0)
	return true
}
