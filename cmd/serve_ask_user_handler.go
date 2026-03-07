package cmd

import (
	"errors"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/tools"
)

type sessionAskUserRequest struct {
	CallID    string                `json:"call_id"`
	Answers   []tools.AskUserAnswer `json:"answers,omitempty"`
	Cancelled bool                  `json:"cancelled,omitempty"`
}

func (s *serveServer) handleSessionAskUser(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req sessionAskUserRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	callID := strings.TrimSpace(req.CallID)
	if callID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "call_id is required")
		return
	}

	rt, ok := s.sessionMgr.Get(sessionID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}

	normalized, err := rt.submitAskUser(callID, req.Answers, req.Cancelled)
	if err != nil {
		switch {
		case errors.Is(err, errServeAskUserNotPending), errors.Is(err, errServeAskUserAnswered):
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
		default:
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		}
		return
	}

	resp := map[string]any{"status": "ok"}
	if !req.Cancelled {
		resp["answers"] = normalized
		resp["summary"] = tools.AskUserAnswerSummary(normalized)
	}
	writeJSON(w, http.StatusOK, resp)
}
