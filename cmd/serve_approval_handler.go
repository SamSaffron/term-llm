package cmd

import (
	"errors"
	"net/http"
	"strings"
)

type sessionApprovalRequest struct {
	ApprovalID string `json:"approval_id"`
	Choice     *int   `json:"choice"`
	Cancelled  bool   `json:"cancelled,omitempty"`
	ResumeAuto bool   `json:"resume_auto,omitempty"`
}

func (s *serveServer) handleSessionApproval(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req sessionApprovalRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	approvalID := strings.TrimSpace(req.ApprovalID)
	if approvalID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "approval_id is required")
		return
	}

	if !req.Cancelled && req.Choice == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "choice is required when not cancelled")
		return
	}

	run := s.responseRuns.latestRun(sessionID)
	if resolved, ok := s.responseRuns.resolvedInteractionForSession(sessionID, "approval", approvalID); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "already_resolved", "outcome": resolved.Outcome, "resolved_at": resolved.ResolvedAt,
		})
		return
	}

	if run != nil {
		run.interactionSubmitMu.Lock()
		defer run.interactionSubmitMu.Unlock()
		if resolved, ok := run.resolvedInteraction("approval", approvalID); ok {
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
	choiceIndex := 0
	if req.Choice != nil {
		choiceIndex = *req.Choice
	}
	outcome := rt.approvalOutcome(approvalID, choiceIndex, req.Cancelled)
	err := rt.submitApproval(approvalID, choiceIndex, req.Cancelled, req.ResumeAuto)
	if err != nil {
		if resolved, ok := s.responseRuns.resolvedInteractionForSession(sessionID, "approval", approvalID); ok {
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "already_resolved", "outcome": resolved.Outcome, "resolved_at": resolved.ResolvedAt,
			})
			return
		}
		switch {
		case errors.Is(err, errServeApprovalNotPending), errors.Is(err, errServeApprovalAnswered):
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
		default:
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		}
		return
	}
	resolvedAt := int64(0)
	if run != nil {
		resolved := run.recordResolvedInteraction("approval", approvalID, outcome)
		outcome, resolvedAt = resolved.Outcome, resolved.ResolvedAt
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "outcome": outcome, "resolved_at": resolvedAt})
}
