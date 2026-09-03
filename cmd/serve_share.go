package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/share"
)

type sessionShareRequest struct {
	AnchorMessageID int64              `json:"anchor_message_id"`
	Scope           session.ShareScope `json:"scope"`
	Visibility      share.Visibility   `json:"visibility,omitempty"`
	Public          *bool              `json:"public,omitempty"`
}

type sessionShareResponse struct {
	Provider   share.ProviderID   `json:"provider"`
	ID         string             `json:"id"`
	URL        string             `json:"url"`
	SourceURL  string             `json:"source_url,omitempty"`
	Visibility share.Visibility   `json:"visibility"`
	Ready      bool               `json:"ready"`
	Scope      session.ShareScope `json:"scope"`

	GistID     string `json:"gist_id,omitempty"`
	GistURL    string `json:"gist_url,omitempty"`
	PreviewURL string `json:"preview_url,omitempty"`
	Public     *bool  `json:"public,omitempty"`
}

type sharingCapabilitiesResponse struct {
	Enabled           bool               `json:"enabled"`
	Provider          share.Provider     `json:"provider"`
	Operations        []share.Operation  `json:"operations"`
	Visibilities      []share.Visibility `json:"visibilities"`
	DefaultVisibility share.Visibility   `json:"default_visibility"`
	Help              string             `json:"help,omitempty"`
	Notes             []string           `json:"notes,omitempty"`
	Limits            map[string]any     `json:"limits,omitempty"`
}

const (
	sharingCacheTTL      = time.Minute
	sharingErrorCacheTTL = 5 * time.Second
)

func (s *serveServer) shareConfigSnapshot() config.ShareConfig {
	if s.cfgRef == nil {
		return config.ShareConfig{}
	}
	return s.cfgRef.Share
}

func shareConfigCacheKey(cfg config.ShareConfig) string {
	encoded, _ := json.Marshal(cfg)
	return string(encoded)
}

func (s *serveServer) buildSharingPublisher() (share.Publisher, error) {
	if s.sharePublisherFactory != nil {
		return s.sharePublisherFactory()
	}
	return share.NewPublisher(s.shareConfigSnapshot())
}

// sharingProvider caches both the publisher and validated capabilities. The
// config-derived key invalidates the cache if an embedding reloads cfgRef in
// place, while the TTL bounds stale provider state. A shared flight prevents
// concurrent modal/API requests from spawning duplicate capability helpers.
func (s *serveServer) sharingProvider(ctx context.Context) (share.Publisher, share.Capabilities, error) {
	key := shareConfigCacheKey(s.shareConfigSnapshot())
	for {
		now := time.Now()
		s.shareMu.Lock()
		if s.shareCacheKey == key && now.Before(s.shareCacheUntil) {
			publisher, capabilities, err := s.shareCachedPublisher, s.shareCachedCapabilities, s.shareCachedErr
			s.shareMu.Unlock()
			return publisher, capabilities, err
		}
		if flight := s.shareBuildFlight; flight != nil {
			s.shareMu.Unlock()
			select {
			case <-ctx.Done():
				return nil, share.Capabilities{}, share.NewError(share.ErrorProvider, "sharing was canceled")
			case <-flight:
				continue
			}
		}
		flight := make(chan struct{})
		s.shareBuildFlight = flight
		s.shareMu.Unlock()

		publisher, err := s.buildSharingPublisher()
		var capabilities share.Capabilities
		if err == nil {
			err = s.withShareInvocation(ctx, func() error {
				var capabilityErr error
				capabilities, capabilityErr = publisher.Capabilities(ctx)
				return capabilityErr
			})
		}
		if err == nil {
			if capabilityErr := share.ValidateCapabilities(capabilities); capabilityErr != nil {
				err = share.NewError(share.ErrorProtocol, "sharing provider returned invalid capabilities")
			}
		}
		ttl := sharingCacheTTL
		if err != nil {
			ttl = sharingErrorCacheTTL
		}
		s.shareMu.Lock()
		s.shareCacheKey = key
		s.shareCachedPublisher = publisher
		s.shareCachedCapabilities = capabilities
		s.shareCachedErr = err
		s.shareCacheUntil = time.Now().Add(ttl)
		close(flight)
		s.shareBuildFlight = nil
		s.shareMu.Unlock()
		return publisher, capabilities, err
	}
}

func (s *serveServer) withShareInvocation(ctx context.Context, fn func() error) error {
	s.shareMu.Lock()
	if s.shareInvocation == nil {
		s.shareInvocation = make(chan struct{}, 1)
	}
	slot := s.shareInvocation
	s.shareMu.Unlock()
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
		return fn()
	case <-ctx.Done():
		return share.NewError(share.ErrorProvider, "sharing was canceled")
	}
}

func (s *serveServer) handleSharingCapabilities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if s.store == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	_, capabilities, err := s.sharingProvider(r.Context())
	if err != nil {
		writeShareError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sharingCapabilitiesResponse{
		Enabled: true, Provider: capabilities.Provider, Operations: capabilities.Operations,
		Visibilities: capabilities.Visibilities, DefaultVisibility: capabilities.DefaultVisibility,
		Help: capabilities.Provider.Help, Notes: capabilities.Notes, Limits: capabilities.Limits,
	})
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

	publisher, capabilities, err := s.sharingProvider(r.Context())
	if err != nil {
		writeShareError(w, err)
		return
	}
	visibility := req.Visibility
	if visibility == "" {
		if req.Public != nil {
			if *req.Public {
				visibility = share.VisibilityPublic
			} else {
				visibility = share.VisibilityUnlisted
			}
		} else {
			visibility = capabilities.DefaultVisibility
		}
	}
	if !capabilities.SupportsVisibility(visibility) {
		writeShareError(w, share.NewError(share.ErrorUnsupportedVisibility, fmt.Sprintf("%s visibility is not supported by %s", visibility, capabilities.Provider.Name)))
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
		// Point-in-time web shares deliberately never include raw reasoning and
		// are never persisted, so a later whole-session update cannot widen them.
		IncludeRawReasoning: false,
	}
	files, err := session.ShareFiles(sess, selected, opts)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to render share")
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
	var created share.Result
	err = s.withShareInvocation(r.Context(), func() error {
		var createErr error
		created, createErr = publisher.Create(r.Context(), share.Request{
			RequestID: share.NewRequestID(), Title: name, Description: description,
			Visibility: visibility, Entrypoint: "index.html", Files: share.TranscriptFiles(files),
		})
		return createErr
	})
	if err != nil {
		writeShareError(w, err)
		return
	}
	if created.Provider == "" {
		created.Provider = capabilities.Provider.ID
	}
	if created.Provider != capabilities.Provider.ID {
		writeShareError(w, share.NewError(share.ErrorProtocol, "sharing provider returned a mismatched provider identity"))
		return
	}
	response := sessionShareResponse{
		Provider: created.Provider, ID: created.ID, URL: created.URL, SourceURL: created.SourceURL,
		Visibility: created.Visibility, Ready: created.Ready, Scope: req.Scope,
	}
	if created.Provider == share.ProviderGitHub {
		public := created.Visibility == share.VisibilityPublic
		response.GistID, response.GistURL, response.PreviewURL, response.Public = created.ID, created.SourceURL, created.URL, &public
	}
	writeJSON(w, http.StatusCreated, response)
}

func writeShareError(w http.ResponseWriter, err error) {
	typed := share.AsError(err)
	status := http.StatusBadGateway
	switch typed.Code {
	case share.ErrorDependencyMissing, share.ErrorAuthRequired:
		status = http.StatusServiceUnavailable
	case share.ErrorTimeout:
		status = http.StatusGatewayTimeout
	case share.ErrorUnsupportedVisibility:
		status = http.StatusBadRequest
	}
	writeProjectError(w, status, string(typed.Code), typed.Error())
}
