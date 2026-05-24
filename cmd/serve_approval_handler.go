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

	rt, ok := s.sessionMgr.Get(sessionID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	choiceIndex := 0
	if req.Choice != nil {
		choiceIndex = *req.Choice
	}
	err := rt.submitApproval(approvalID, choiceIndex, req.Cancelled)
	if err != nil {
		switch {
		case errors.Is(err, errServeApprovalNotPending), errors.Is(err, errServeApprovalAnswered):
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
		default:
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		}
		return
	}
	if s.responseRuns != nil {
		if runID := s.responseRuns.activeRunID(sessionID); runID != "" {
			if run, ok := s.responseRuns.get(runID); ok {
				run.resolveApprovalRecovery(approvalID)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
