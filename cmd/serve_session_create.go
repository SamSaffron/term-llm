package cmd

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
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

// serveSessionCreateError is a failure to materialize a blank conversation,
// classified for HTTP. Callers that are not HTTP (the live voice lane) read the
// message instead, so it is written to be spoken.
type serveSessionCreateError struct {
	Status  int
	Code    string
	Message string
}

func (e *serveSessionCreateError) Error() string { return e.Message }

// newConversationWorkspaceError marks a failure raised while resolving or
// binding the workspace of a conversation being created. The HTTP handler
// reports it exactly as it reports the same failure from a workspace endpoint.
type newConversationWorkspaceError struct{ err error }

func (e *newConversationWorkspaceError) Error() string { return e.err.Error() }
func (e *newConversationWorkspaceError) Unwrap() error { return e.err }

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
	sess, err := s.createWebSession(r.Context(), req)
	if err != nil {
		writeCreateWebSessionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"session": s.webSessionEntryFromSession(sess)})
}

// writeCreateWebSessionError reports a creation failure with the response this
// endpoint produced before creation moved behind createWebSession.
func writeCreateWebSessionError(w http.ResponseWriter, err error) {
	var workspaceErr *newConversationWorkspaceError
	if errors.As(err, &workspaceErr) {
		writeWorkspaceError(w, workspaceErr.err)
		return
	}
	var createErr *serveSessionCreateError
	if errors.As(err, &createErr) {
		writeOpenAIError(w, createErr.Status, createErr.Code, createErr.Message)
		return
	}
	writeOpenAIError(w, http.StatusInternalServerError, "server_error", "could not create the session")
}

// createWebSession materializes a blank Web conversation, binds its workspace,
// and publishes session.created so every sidebar picks it up. It is the whole of
// what a first-party blank conversation is: the browser posts to it, and the
// live voice lane calls it directly, so a conversation started by voice is the
// same row, in the same workspace, with the same provider and model defaults, as
// one started in the browser.
//
// Like the endpoint, it assumes a first-party caller and does not re-check the
// request's origin: both callers are the host itself.
func (s *serveServer) createWebSession(ctx context.Context, req createWebSessionRequest) (*session.Session, error) {
	if s.store == nil || s.sessionMgr == nil {
		return nil, &serveSessionCreateError{Status: http.StatusServiceUnavailable, Code: "session_unavailable", Message: "session persistence is unavailable"}
	}
	sessionID := generateSessionID()
	binding, err := s.resolveWorkspace(ctx, serveWorkspaceRequest{
		SessionID: sessionID, ProjectID: req.ProjectID, WorktreeDir: req.WorktreeDir,
		FirstPartyUI: true, FreshConversation: true, AllowNoProject: req.NoProject,
	})
	if err != nil {
		return nil, &newConversationWorkspaceError{err: err}
	}
	var runtime *serveRuntime
	if _, supported := session.AsSessionInputRefresher(s.store); supported {
		runtime, err = s.prepareUIRuntime(ctx, serveRuntimeRequest{SessionID: sessionID, Provider: req.Provider, Model: req.Model, Agent: req.Agent, RefreshInputs: true}, binding)
	} else {
		runtime, _, err = s.runtimeForFreshAgentProviderRequest(ctx, sessionID, req.Provider, req.Agent)
	}
	if err != nil {
		return nil, &serveSessionCreateError{Status: http.StatusBadRequest, Code: "invalid_request_error", Message: err.Error()}
	}
	if err := s.ensurePersistedSessionForProjectBinding(ctx, sessionID, runtime, req.Model); err != nil {
		return nil, &serveSessionCreateError{Status: http.StatusInternalServerError, Code: "server_error", Message: "could not create the session"}
	}
	if err := s.bindResolvedWorkspace(ctx, sessionID, runtime, binding); err != nil {
		return nil, &newConversationWorkspaceError{err: err}
	}
	s.syncPersistedSessionRuntime(
		ctx, sessionID, runtime, req.Model, req.ReasoningEffort,
		strings.TrimSpace(req.ReasoningMode), true, "", req.UseDefaultWorkspace,
	)
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil || sess == nil {
		return nil, &serveSessionCreateError{Status: http.StatusInternalServerError, Code: "server_error", Message: "could not load the created session"}
	}
	s.publishEvent(serveEventInput{Type: serveEventSessionCreated, SessionID: sessionID})
	return sess, nil
}
