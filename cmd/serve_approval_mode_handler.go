package cmd

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/tools"
)

type sessionApprovalModeRequest struct {
	Mode string `json:"mode"`
}

func runtimeApprovalPolicy(rt *serveRuntime) map[string]any {
	policy := map[string]any{
		"default_mode":            tools.ModePrompt.String(),
		"requested_mode":          tools.ModePrompt.String(),
		"effective_mode":          tools.ModePrompt.String(),
		"guardian_available":      false,
		"guardian_auto_suspended": false,
	}
	if rt == nil {
		return policy
	}
	policy["default_mode"] = rt.approvalDefault.String()
	policy["requested_mode"] = rt.approvalDefault.String()
	policy["effective_mode"] = rt.approvalDefault.String()
	if rt.toolMgr == nil || rt.toolMgr.ApprovalMgr == nil {
		return policy
	}
	mgr := rt.toolMgr.ApprovalMgr
	policy["requested_mode"] = mgr.RequestedApprovalMode().String()
	policy["effective_mode"] = mgr.ApprovalMode().String()
	policy["guardian_available"] = mgr.GuardianReviewerAvailable()
	policy["guardian_auto_suspended"] = mgr.GuardianAutoSuspended()
	return policy
}

func (s *serveServer) approvalRuntime(ctx context.Context, sessionID string) (*serveRuntime, error) {
	if s.sessionMgr == nil {
		return nil, errors.New("approval runtime is not configured")
	}
	if rt, ok := s.sessionMgr.Get(sessionID); ok && rt != nil {
		// Existing runtimes are returned directly, including while a response owns
		// rt.mu. ApprovalManager mode transitions have their own synchronization.
		return rt, nil
	}
	rt, _, err := s.runtimeForRequest(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return rt, nil
}

func (s *serveServer) handleSessionApprovalMode(w http.ResponseWriter, r *http.Request, sessionID string) {
	// A Yolo launch is intentionally atomic: it installs no Guardian guardrails
	// and cannot be weakened or reconfigured through per-session web controls.
	if s.approvalDefault == tools.ModeYolo {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "approval controls are unavailable")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var requestedMode *tools.ApprovalMode
	if r.Method == http.MethodPost {
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		var req sessionApprovalModeRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		mode, valid := parseApprovalMode(strings.TrimSpace(req.Mode), true)
		if !valid {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "mode must be prompt, auto, or yolo")
			return
		}
		requestedMode = &mode
	}

	if s.store != nil {
		sess, loadErr := s.store.Get(r.Context(), sessionID)
		if loadErr != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load session approval policy")
			return
		}
		if sess == nil {
			writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
			return
		}
	}

	rt, err := s.approvalRuntime(r.Context(), sessionID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to prepare session approval controls")
		return
	}
	if requestedMode == nil {
		writeJSON(w, http.StatusOK, runtimeApprovalPolicy(rt))
		return
	}
	mode := *requestedMode
	var mgr *tools.ApprovalManager
	if rt.toolMgr != nil {
		mgr = rt.toolMgr.ApprovalMgr
	}
	if mgr == nil {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "this session has no approval-managed tools")
		return
	}
	if mode == tools.ModeAuto && !mgr.GuardianReviewerAvailable() {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "Guardian auto-approval is unavailable")
		return
	}

	// Serialize competing web overrides without taking rt.mu, which an active turn
	// owns. ApprovalManager synchronizes policy reads and Guardian transitions.
	rt.approvalModeMu.Lock()
	if mode == tools.ModeAuto && mgr.RequestedApprovalMode() == tools.ModeAuto {
		mgr.ResumeAuto()
	} else {
		mgr.SetApprovalMode(mode)
	}
	if mcpManager := rt.mcpManagerSnapshot(); mcpManager != nil {
		mcpManager.SetSamplingYoloMode(rt.yoloEnabled())
	}
	policy := runtimeApprovalPolicy(rt)
	rt.approvalModeMu.Unlock()

	s.publishEvent(serveEventInput{Type: serveEventSessionRuntimeChanged, SessionID: sessionID, Reason: "approval_mode"})
	writeJSON(w, http.StatusOK, policy)
}
