package cmd

import (
	"errors"
	"net/http"

	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) handleSessionTree(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s == nil || s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}
	store, ok := s.store.(session.ConversationBranchStore)
	if !ok {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "conversation branching is unavailable")
		return
	}
	tree, err := store.GetBranchTree(r.Context(), sessionID)
	switch {
	case errors.Is(err, session.ErrNotFound):
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
	case errors.Is(err, session.ErrBranchingUnsupported):
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "conversation branching is unavailable")
	case err != nil:
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load conversation tree")
	default:
		writeJSON(w, http.StatusOK, tree)
	}
}
