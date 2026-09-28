package cmd

import (
	"errors"
	"log"
	"net/http"

	"github.com/samsaffron/term-llm/internal/session"
)

// pinnedOrderRequest lists pinned session IDs in their new relative order.
type pinnedOrderRequest struct {
	SessionIDs []string `json:"session_ids"`
}

// handleSessionsPinnedOrder serves PATCH /v1/sessions/pinned-order. The listed
// pinned sessions move, in the given order, into the ranks they already
// occupy; pinned sessions the caller did not list (for example archived pins
// hidden from its sidebar) keep their positions. The request is atomic and
// idempotent, and the response is the complete committed order.
func (s *serveServer) handleSessionsPinnedOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		w.Header().Set("Allow", http.MethodPatch)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}
	pins, ok := session.AsPinnedSessionStore(s.store)
	if !ok {
		writeOpenAIError(w, http.StatusNotImplemented, "unsupported_error", "pinned conversation ordering is unavailable")
		return
	}
	var req pinnedOrderRequest
	if err := decodeSmallJSON(w, r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	order, err := pins.ReorderPinnedSessions(r.Context(), req.SessionIDs)
	if err != nil {
		writePinnedOrderError(w, err)
		return
	}
	for _, id := range order.Changed {
		s.publishEvent(serveEventInput{Type: serveEventSessionMetadataChanged, SessionID: id, Reason: "pin_order"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pinned": order.Positions})
}

func writePinnedOrderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrPinnedOrderInvalid):
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
	case errors.Is(err, session.ErrNotFound):
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "a listed session was not found")
	case errors.Is(err, session.ErrPinnedOrderConflict):
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "a listed session is no longer pinned; refresh and try again")
	default:
		log.Printf("[serve] reorder pinned sessions: %v", err)
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to save the pinned order")
	}
}

// setSessionPinnedForPatch applies a metadata PATCH's pin change through the
// narrow pin store so a new pin is appended after existing pins and an
// existing pin keeps its rank. It writes the error response on failure.
func (s *serveServer) setSessionPinnedForPatch(w http.ResponseWriter, r *http.Request, sess *session.Session, pinned bool) bool {
	state, err := session.SetSessionPinned(r.Context(), s.store, sess.ID, pinned)
	switch {
	case errors.Is(err, session.ErrNotFound):
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return false
	case err != nil:
		log.Printf("[serve] set session pinned for %s: %v", sess.ID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to update session")
		return false
	}
	sess.Pinned = state.Pinned
	sess.PinOrder = state.PinOrder
	return true
}
