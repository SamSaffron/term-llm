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

	run := s.responseRuns.latestRun(sessionID)
	if resolved, ok := s.responseRuns.resolvedInteractionForSession(sessionID, "ask_user", callID); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "already_resolved", "outcome": resolved.Outcome, "resolved_at": resolved.ResolvedAt,
		})
		return
	}
	if run != nil {
		run.interactionSubmitMu.Lock()
		defer run.interactionSubmitMu.Unlock()
		if resolved, ok := run.resolvedInteraction("ask_user", callID); ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "already_resolved", "outcome": resolved.Outcome, "resolved_at": resolved.ResolvedAt,
			})
			return
		}
	}

	rt, ok := s.sessionMgr.Get(sessionID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	normalized, err := rt.submitAskUser(callID, req.Answers, req.Cancelled)
	if err != nil {
		if resolved, ok := s.responseRuns.resolvedInteractionForSession(sessionID, "ask_user", callID); ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "already_resolved", "outcome": resolved.Outcome, "resolved_at": resolved.ResolvedAt,
			})
			return
		}
		switch {
		case errors.Is(err, errServeAskUserNotPending), errors.Is(err, errServeAskUserAnswered):
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
		default:
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		}
		return
	}
	outcome := "answered"
	if req.Cancelled {
		outcome = "cancelled-by-user"
	}
	resolvedAt := int64(0)
	if run != nil {
		resolved := run.recordResolvedInteraction("ask_user", callID, outcome)
		outcome, resolvedAt = resolved.Outcome, resolved.ResolvedAt
	}

	resp := map[string]any{"status": "ok", "outcome": outcome, "resolved_at": resolvedAt}
	if !req.Cancelled {
		resp["answers"] = normalized
		resp["summary"] = tools.AskUserAnswerSummary(normalized)
	}
	writeJSON(w, http.StatusOK, resp)
}
