package serve

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) retireNotificationTurn(ctx context.Context, responseID string) bool {
	store, ok := session.AsNotificationTurnStore(s.store)
	if !ok {
		return true
	}
	if err := store.CancelNotificationTurn(ctx, responseID); err != nil && !errors.Is(err, session.ErrNotFound) {
		log.Printf("warning: cancel logical notification turn: %v", err)
		return false
	}
	return true
}

func (s *serveServer) cancelNotificationTurn(w http.ResponseWriter, r *http.Request, responseID string) bool {
	store, ok := session.AsNotificationTurnStore(s.store)
	if !ok {
		return false
	}
	err := store.CancelNotificationTurn(r.Context(), responseID)
	if errors.Is(err, session.ErrNotFound) {
		return false
	}
	if err != nil {
		writeOpenAIError(w, 500, "server_error", "failed to cancel notification turn")
		return true
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": responseID, "object": "response.cancel", "status": "cancelled"})
	return true
}
