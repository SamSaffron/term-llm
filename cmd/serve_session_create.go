package cmd

import (
	"net/http"
	"strings"
)

type createWebSessionRequest struct {
	Provider            string `json:"provider,omitempty"`
	Model               string `json:"model,omitempty"`
	ReasoningEffort     string `json:"reasoning_effort,omitempty"`
	ReasoningMode       string `json:"reasoning_mode,omitempty"`
	Agent               string `json:"agent,omitempty"`
	ProjectID           string `json:"project_id,omitempty"`
	WorktreeDir         string `json:"worktree_dir,omitempty"`
	NoProject           bool   `json:"no_project,omitempty"`
	UseDefaultWorkspace bool   `json:"use_default_workspace,omitempty"`
}

// handleCreateWebSession materializes an otherwise local Web draft without
// adding a transcript message. Shell collaboration needs this durable identity,
// while merely opening the terminal is allowed to remain sessionless.
func (s *serveServer) handleCreateWebSession(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.sessionMgr == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "session_unavailable", "session persistence is unavailable")
		return
	}
	if !isFirstPartyUIResponseRequest(r) {
		writeOpenAIError(w, http.StatusForbidden, "invalid_origin", "blank sessions may only be created by the first-party UI")
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	var req createWebSessionRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	sessionID := generateSessionID()
	binding, err := s.resolveWorkspace(r.Context(), serveWorkspaceRequest{
		SessionID: sessionID, ProjectID: req.ProjectID, WorktreeDir: req.WorktreeDir,
		FirstPartyUI: true, FreshConversation: true, AllowNoProject: req.NoProject,
	})
	if err != nil {
		writeWorkspaceError(w, err)
		return
	}
	runtime, _, err := s.runtimeForFreshAgentProviderRequest(r.Context(), sessionID, req.Provider, req.Agent)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := s.ensurePersistedSessionForProjectBinding(r.Context(), sessionID, runtime, req.Model); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "could not create the session")
		return
	}
	if err := s.bindResolvedWorkspace(r.Context(), sessionID, runtime, binding); err != nil {
		writeWorkspaceError(w, err)
		return
	}
	s.syncPersistedSessionRuntime(
		r.Context(), sessionID, runtime, req.Model, req.ReasoningEffort,
		strings.TrimSpace(req.ReasoningMode), true, "", req.UseDefaultWorkspace,
	)
	sess, err := s.store.Get(r.Context(), sessionID)
	if err != nil || sess == nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "could not load the created session")
		return
	}
	s.publishEvent(serveEventInput{Type: serveEventSessionCreated, SessionID: sessionID})
	writeJSON(w, http.StatusCreated, map[string]any{"session": s.webSessionEntryFromSession(sess)})
}
