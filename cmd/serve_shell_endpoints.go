package cmd

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func collaborativeShellRuntimeAvailable(rt *serveRuntime) (bool, string) {
	if rt == nil || rt.toolMgr == nil || rt.toolMgr.Registry == nil {
		return false, "shell tool is unavailable"
	}
	if rt.provider == nil || !rt.provider.Capabilities().ToolCalls {
		return false, "active provider does not support tool calls"
	}
	if !rt.toolMgr.Registry.HasVisibleShell() {
		return false, "shell tool is not available to the active agent"
	}
	routing, controllerInstalled := rt.toolMgr.Registry.CollaborativeShellRouting()
	if routing != tools.ShellRoutingControllerRequired || !controllerInstalled {
		return false, "shared shell controller is unavailable"
	}
	for _, spec := range rt.toolMgr.GetSpecs() {
		if spec.Name == tools.ShellToolName {
			return true, ""
		}
	}
	return false, "shell tool is filtered from the active agent"
}

func (s *serveServer) collaborativeShellRuntime(ctx context.Context, sessionID string) (*serveRuntime, bool, string) {
	if s == nil || s.sessionMgr == nil {
		return nil, false, "shell runtime is unavailable"
	}
	// Snapshotting collaboration must never wait for rt.mu: shell SSE/create may
	// run while a response owns it. Existing runtimes are already fully wired;
	// enable's idle mutation guard below revalidates identity and exclusivity.
	if existing, ok := s.sessionMgr.Get(sessionID); ok && existing != nil {
		agentName := s.requestedRuntimeAgent(ctx, sessionID, "")
		if !runtimeHasAgent(existing, agentName) {
			if existing.hasActiveRun() {
				return existing, false, "active agent runtime is changing"
			}
		} else if available, reason := collaborativeShellRuntimeAvailable(existing); available {
			return existing, true, ""
		} else {
			return existing, false, reason
		}
	}
	rt, _, err := s.runtimeForRequest(ctx, sessionID)
	if err != nil {
		return rt, false, "shell runtime is unavailable"
	}
	available, reason := collaborativeShellRuntimeAvailable(rt)
	return rt, available, reason
}

func (s *serveServer) collaborationSnapshotForStore(shell *serveShell, toolAvailable bool, capabilityReason ...string) serveShellCollaborationSnapshot {
	reason := ""
	if len(capabilityReason) > 0 {
		reason = capabilityReason[0]
	}
	supported := session.SupportsBatchTranscriptWriter(s.store)
	if !supported {
		reason = "session store cannot atomically persist terminal activity"
	}
	return shell.updateCollaborationCapability(toolAvailable, supported, reason)
}

func (s *serveServer) shellCollaborationSnapshot(ctx context.Context, sessionID string, shell *serveShell) serveShellCollaborationSnapshot {
	_, available, reason := s.collaborativeShellRuntime(ctx, sessionID)
	return s.collaborationSnapshotForStore(shell, available, reason)
}

func (s *serveServer) handleShellCollaboration(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req serveShellCollaborationRequest
	if !decodeServeShellJSON(w, r, &req) {
		return
	}
	manager, err := s.shellManager()
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	shell, err := manager.get(sessionID, strings.TrimSpace(req.ShellID))
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	shell.collaborationMutation.Lock()
	defer shell.collaborationMutation.Unlock()
	if !req.Enabled {
		if err := shell.disableCollaboration(r.Context()); err != nil {
			writeCollaborativeShellHTTPError(w, err, s.shellCollaborationSnapshot(r.Context(), sessionID, shell))
			return
		}
		writeJSON(w, http.StatusOK, s.shellCollaborationSnapshot(r.Context(), sessionID, shell))
		return
	}

	// Runtime initialization precedes the idle guard by contract, but durable
	// response ownership is already authoritative and must reject without waiting
	// or writing probe bytes.
	if s.responseRuns != nil && s.responseRuns.activeRunID(sessionID) != "" {
		writeCollaborativeShellHTTPError(w, tools.NewCollaborativeShellError("session_busy", "a response is active"), s.shellCollaborationSnapshot(r.Context(), sessionID, shell))
		return
	}
	rt, available, reason := s.collaborativeShellRuntime(r.Context(), sessionID)
	if !available {
		writeCollaborativeShellHTTPError(w, tools.NewCollaborativeShellError("shell_tool_unavailable", reason), s.collaborationSnapshotForStore(shell, false, reason))
		return
	}
	lockedRT, unlock, err := s.sessionMgr.lockIdleMetadataMutation(sessionID)
	if err != nil {
		writeCollaborativeShellHTTPError(w, tools.NewCollaborativeShellError("session_busy", "a response is active"), s.collaborationSnapshotForStore(shell, true))
		return
	}
	defer unlock()
	if lockedRT != nil && lockedRT != rt {
		writeCollaborativeShellHTTPError(w, tools.NewCollaborativeShellError("runtime_changed", "active agent runtime changed"), s.collaborationSnapshotForStore(shell, true))
		return
	}
	if ok, reason := collaborativeShellRuntimeAvailable(rt); !ok {
		writeCollaborativeShellHTTPError(w, tools.NewCollaborativeShellError("shell_tool_unavailable", reason), s.collaborationSnapshotForStore(shell, false, reason))
		return
	}
	if !session.SupportsBatchTranscriptWriter(s.store) {
		writeCollaborativeShellHTTPError(w, tools.NewCollaborativeShellError("unsupported_store", "session store cannot atomically persist terminal activity"), s.collaborationSnapshotForStore(shell, true))
		return
	}
	shell.mu.Lock()
	if shell.exited {
		shell.mu.Unlock()
		writeCollaborativeShellHTTPError(w, tools.NewCollaborativeShellError("shell_exited", "shell has exited"), s.collaborationSnapshotForStore(shell, true))
		return
	}
	if shell.collaborationState == serveShellCollaborationDesynchronized {
		shell.mu.Unlock()
		writeCollaborativeShellHTTPError(w, tools.NewCollaborativeShellError("shell_not_synchronized", "stop sharing before trying again"), s.collaborationSnapshotForStore(shell, true))
		return
	}
	if shell.collaborationEnabled {
		snapshot := shell.collaborationSnapshotLocked()
		shell.mu.Unlock()
		writeJSON(w, http.StatusOK, snapshot)
		return
	}
	shell.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(r.Context(), 750*time.Millisecond)
	err = shell.probe(probeCtx)
	cancel()
	if err != nil {
		writeCollaborativeShellHTTPError(w, tools.NewCollaborativeShellError("shell_not_synchronized", "shared shell synchronization probe failed"), s.collaborationSnapshotForStore(shell, true))
		return
	}
	shell.mu.Lock()
	if shell.exited || shell.id != strings.TrimSpace(req.ShellID) {
		shell.mu.Unlock()
		writeCollaborativeShellHTTPError(w, tools.NewCollaborativeShellError("stale_shell", "shell generation changed during synchronization"), s.collaborationSnapshotForStore(shell, true))
		return
	}
	shell.resetActivityCursorLocked()
	shell.shellToolAvailable = true
	shell.collaborationSupported = true
	shell.collaborationCapabilityMsg = ""
	shell.transitionCollaborationLocked(serveShellCollaborationReady, true, "collaboration", "")
	snapshot := shell.collaborationSnapshotLocked()
	shell.mu.Unlock()
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *serveServer) handleShellInterrupt(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req serveShellInterruptRequest
	if !decodeServeShellJSON(w, r, &req) {
		return
	}
	manager, err := s.shellManager()
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	shell, err := manager.get(sessionID, strings.TrimSpace(req.ShellID))
	if err != nil {
		writeServeShellError(w, err)
		return
	}
	if err := shell.interruptCommand(r.Context(), strings.TrimSpace(req.CommandID)); err != nil {
		writeCollaborativeShellHTTPError(w, err, s.shellCollaborationSnapshot(r.Context(), sessionID, shell))
		return
	}
	writeJSON(w, http.StatusOK, s.shellCollaborationSnapshot(r.Context(), sessionID, shell))
}

func (s *serveShell) disableCollaboration(ctx context.Context) error {
	s.mu.Lock()
	if !s.collaborationEnabled && s.collaborationState == serveShellCollaborationOff {
		s.mu.Unlock()
		return nil
	}
	if s.collaborationState != serveShellCollaborationAgentRunning {
		s.commandID, s.toolCallID = "", ""
		s.resetActivityCursorLocked()
		s.transitionCollaborationLocked(serveShellCollaborationOff, false, "collaboration", "")
		s.mu.Unlock()
		return nil
	}
	s.disableRequested = true
	cancel := s.commandCancel
	changed := s.changed
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.generationCtx.Done():
			return ioErrShellClosed()
		case <-changed:
			s.mu.Lock()
			if s.collaborationState == serveShellCollaborationOff {
				s.mu.Unlock()
				return nil
			}
			changed = s.changed
			s.mu.Unlock()
		}
	}
}

func (s *serveShell) interruptCommand(ctx context.Context, commandID string) error {
	s.mu.Lock()
	if s.collaborationState != serveShellCollaborationAgentRunning || s.commandID == "" {
		s.mu.Unlock()
		return tools.NewCollaborativeShellError("no_agent_command", "no agent command is running")
	}
	if commandID == "" || commandID != s.commandID {
		s.mu.Unlock()
		return tools.NewCollaborativeShellError("stale_command", "agent command is stale")
	}
	cancel := s.commandCancel
	changed := s.changed
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.generationCtx.Done():
			return ioErrShellClosed()
		case <-changed:
			s.mu.Lock()
			if s.commandID == "" {
				s.mu.Unlock()
				return nil
			}
			changed = s.changed
			s.mu.Unlock()
		}
	}
}

func ioErrShellClosed() error {
	return tools.NewCollaborativeShellError("shell_exited", "shared shell has exited")
}

func writeCollaborativeShellHTTPError(w http.ResponseWriter, err error, snapshot serveShellCollaborationSnapshot) {
	kind := tools.CollaborativeShellErrorKind(err)
	status := http.StatusConflict
	if kind == "controller_unavailable" || kind == "unsupported_store" {
		status = http.StatusServiceUnavailable
	}
	if kind == "" {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			kind = "request_canceled"
		} else {
			kind = "shell_error"
			status = http.StatusInternalServerError
		}
	}
	writeJSON(w, status, map[string]any{
		"error":         map[string]any{"type": kind, "message": err.Error()},
		"collaboration": snapshot,
	})
}
