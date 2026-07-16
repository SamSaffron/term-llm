package cmd

import (
	"errors"
	"net/http"

	"github.com/samsaffron/term-llm/internal/session"
)

type sideRelationshipResponse struct {
	Session         *session.Session         `json:"session"`
	Parent          *session.Session         `json:"parent,omitempty"`
	OpenSide        *session.Session         `json:"open_side,omitempty"`
	Sides           []session.SessionSummary `json:"sides,omitempty"`
	ParentActive    bool                     `json:"parent_active_run"`
	ParentAttention bool                     `json:"parent_attention"`
}

func (s *serveServer) sideStore() (session.SideStore, bool) {
	store, ok := s.store.(session.SideStore)
	return store, ok
}

func (s *serveServer) handleSessionSide(w http.ResponseWriter, r *http.Request, sessionID, action string) {
	sideStore, ok := s.sideStore()
	if !ok || s.store == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error", "side conversations require session persistence")
		return
	}
	current, err := s.store.Get(r.Context(), sessionID)
	if err != nil || current == nil {
		http.NotFound(w, r)
		return
	}

	switch action {
	case "side":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		var side *session.Session
		if existing, getErr := sideStore.GetOpenSide(r.Context(), current.RootConversationID()); getErr == nil && existing != nil {
			side = existing
		} else {
			side, err = sideStore.ForkSide(r.Context(), current.ID, session.OriginWeb)
		}
		if err != nil {
			status := http.StatusConflict
			if errors.Is(err, session.ErrNestedSide) {
				status = http.StatusBadRequest
			}
			writeOpenAIError(w, status, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, side)
	case "side/reopen":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		side, err := sideStore.ReopenSide(r.Context(), current.ID)
		if err != nil {
			writeOpenAIError(w, http.StatusConflict, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, side)
	case "side/close":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		// Serialize close against a start on an already-created runtime. A runtime
		// being created will observe the persisted closed state before its run.
		if s.sessionMgr != nil {
			if rt, exists := s.sessionMgr.Peek(current.ID); exists {
				if !rt.mu.TryLock() {
					writeOpenAIError(w, http.StatusConflict, "invalid_request_error", "side conversation is currently running")
					return
				}
				defer rt.mu.Unlock()
			}
		}
		if err := sideStore.CloseSide(r.Context(), current.ID); err != nil {
			writeOpenAIError(w, http.StatusConflict, "invalid_request_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"closed": true, "id": current.ID})
	case "relationship":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		resp := sideRelationshipResponse{Session: current}
		rootID := current.RootConversationID()
		if current.Kind == session.KindSide {
			resp.Parent, _ = s.store.Get(r.Context(), current.ParentID)
		} else {
			resp.OpenSide, _ = sideStore.GetOpenSide(r.Context(), rootID)
		}
		resp.Sides, _ = sideStore.ListSides(r.Context(), rootID)
		parentID := current.ParentID
		if parentID == "" {
			parentID = current.ID
		}
		if s.sessionMgr != nil {
			if parentRT, exists := s.sessionMgr.Peek(parentID); exists {
				resp.ParentActive = parentRT.hasActiveRun()
				resp.ParentAttention = len(parentRT.pendingAskUserPrompts()) > 0 || len(parentRT.pendingApprovalPrompts()) > 0
			}
		}
		writeJSON(w, http.StatusOK, resp)
	default:
		http.NotFound(w, r)
	}
}
