package cmd

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/agents/gist"
	"github.com/samsaffron/term-llm/internal/config"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/session"
)

type serveGistCreator interface {
	Create(description string, public bool, files map[string]string) (*gist.Gist, error)
}

type sessionShareRequest struct {
	AnchorMessageID int64              `json:"anchor_message_id"`
	Scope           session.ShareScope `json:"scope"`
	Public          bool               `json:"public"`
}

type sessionShareResponse struct {
	GistID     string `json:"gist_id"`
	GistURL    string `json:"gist_url"`
	PreviewURL string `json:"preview_url"`
	Public     bool   `json:"public"`
}

func (s *serveServer) handleCreateSessionShare(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}
	var req sessionShareRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if req.Scope != session.ShareScopeResponse && req.Scope != session.ShareScopeConversation {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "scope must be response or conversation")
		return
	}
	sess, err := s.store.Get(r.Context(), sessionID)
	if err != nil || sess == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	messages, _, err := session.LoadScrollbackWithBoundary(r.Context(), s.store, sess)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load session transcript")
		return
	}
	selected, err := session.SelectShareMessages(messages, req.AnchorMessageID, req.Scope)
	if errors.Is(err, session.ErrInvalidShareAnchor) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "anchor must identify a visible assistant response in this session")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	reasoningConfig := config.DefaultReasoningConfig()
	if s.cfgRef != nil {
		reasoningConfig = s.cfgRef.ResolveReasoning("chat")
	}
	opts := session.ExportOptions{
		IncludeReasoningSummaries: internalreasoning.ExportSummaries(reasoningConfig),
		Partial:                   req.Scope == session.ShareScopeConversation,
		ResponseOnly:              req.Scope == session.ShareScopeResponse,
		// Point-in-time web shares deliberately never include raw reasoning.
		IncludeRawReasoning: false,
	}
	files, err := session.GistFiles(sess, selected, opts)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to render share")
		return
	}
	factory := s.shareClientFactory
	if factory == nil {
		factory = func() (serveGistCreator, error) { return gist.NewClient() }
	}
	client, err := factory()
	if err != nil {
		message := strings.TrimSpace(err.Error())
		if message == "" {
			message = "GitHub CLI is unavailable"
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "dependency_error", message+". Sharing currently requires the gh CLI on the term-llm server.")
		return
	}
	name := strings.TrimSpace(sess.PreferredShortTitle())
	if name == "" {
		name = session.ShortID(sess.ID)
	}
	description := fmt.Sprintf("term-llm conversation through %s", name)
	if req.Scope == session.ShareScopeResponse {
		description = fmt.Sprintf("term-llm assistant response from %s", name)
	}
	created, err := client.Create(description, req.Public, files)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "dependency_error", "failed to create GitHub Gist: "+strings.TrimSpace(err.Error()))
		return
	}
	if created == nil || strings.TrimSpace(created.ID) == "" {
		writeOpenAIError(w, http.StatusBadGateway, "dependency_error", "GitHub CLI returned an empty Gist result")
		return
	}
	preview := session.GistPreviewURL(created.ID)
	if preview == "" {
		writeOpenAIError(w, http.StatusBadGateway, "dependency_error", "GitHub CLI returned an invalid Gist ID")
		return
	}
	gistURL := strings.TrimSpace(created.URL)
	if gistURL == "" {
		gistURL = gist.GetURL(created.ID)
	}
	writeJSON(w, http.StatusCreated, sessionShareResponse{
		GistID: created.ID, GistURL: gistURL, PreviewURL: preview, Public: req.Public,
	})
}
