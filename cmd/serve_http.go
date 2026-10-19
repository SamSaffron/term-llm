package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/usage"
)

func (s *serveServer) Start() (startErr error) {
	defer func() {
		if startErr != nil && s.reloadRunsUnregister != nil {
			s.reloadRunsUnregister()
			s.reloadRunsUnregister = nil
		}
	}()
	s.shutdownCh = make(chan struct{})
	s.shutdownOnce = sync.Once{}
	if err := s.installWebReload(); err != nil {
		return fmt.Errorf("restore response continuations: %w", err)
	}
	s.autoTitleMu.Lock()
	if s.autoTitleCancel != nil {
		s.autoTitleCancel()
	}
	s.autoTitleCtx, s.autoTitleCancel = context.WithCancel(context.Background())
	s.autoTitleStopping = false
	s.autoTitleMu.Unlock()
	s.skillRunsMu.Lock()
	s.skillRunsStopping = false
	s.skillRunsMu.Unlock()
	s.resetEventBroker()
	s.skillsCacheMu.Lock()
	s.skillsByDir = nil
	s.skillsCacheMu.Unlock()
	s.mentionsCacheMu.Lock()
	s.mentionsByRoot = nil
	s.mentionsCacheMu.Unlock()
	if s.cfg.ui {
		s.providerCreds.Warm(s.cfgRef)
	}
	s.server = &http.Server{
		Addr:              net.JoinHostPort(s.cfg.host, strconv.Itoa(s.cfg.port)),
		Handler:           s.httpHandler(),
		ConnContext:       s.reloadTransport().ConnContext,
		ConnState:         s.reloadTransport().ConnState,
		ReadHeaderTimeout: serveReadHeaderTimeout,
		IdleTimeout:       serveIdleTimeout,
		// Do not set server-wide WriteTimeout: long-lived SSE streams are valid.
		// Streaming handlers apply per-write deadlines instead.
	}

	if s.cfg.ui {
		s.prewarmUIAssetCache()
	}
	// Cost estimates read a cached rate card; refreshing it here keeps a
	// long-running server from pricing months-old rates.
	usage.RefreshDirectoryInBackground(context.Background())

	errCh := make(chan error, 1)
	go func() {
		var err error
		if s.listener != nil {
			err = s.server.Serve(s.listener)
		} else {
			err = s.server.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("start server: %w", err)
		}
		return nil
	case <-time.After(50 * time.Millisecond):
		s.startEventWatcher()
		s.startResponseLifecycle()
		s.startCompletionPushDispatcher()
		return nil
	}
}

var registerServeBrowserFixtureRoutes func(*http.ServeMux, *serveServer)

func (s *serveServer) httpHandler() http.Handler {
	// Inner mux: all routes registered at their natural paths.
	// basePath is stripped by http.StripPrefix on the outer mux when mounted,
	// so handlers see /v1/..., /images/..., / etc. without the prefix.
	inner := http.NewServeMux()
	if s.browserAuth != nil {
		s.browserAuth.registerPasskeyRoutes(inner)
		inner.HandleFunc("/auth/security", s.browserAuth.handleSecurityPage)
		inner.HandleFunc("/dist/hub.js", handleBrowserAuthAsset)
		inner.HandleFunc("/dist/hub.css", handleBrowserAuthAsset)
	}

	inner.HandleFunc("/healthz", s.handleHealth)
	inner.HandleFunc("/v1/providers", s.auth(s.cors(s.handleProviders)))
	inner.HandleFunc("/v1/models", s.auth(s.cors(s.handleModels)))
	inner.HandleFunc("/v1/mentions/search", s.auth(s.cors(s.handleMentionSearch)))
	inner.HandleFunc("/v1/responses", s.auth(s.cors(s.handleResponses)))
	inner.HandleFunc("/v1/responses/", s.auth(s.cors(s.handleResponseByID)))
	inner.HandleFunc("/v1/chat/completions", s.auth(s.cors(s.handleChatCompletions)))
	inner.HandleFunc("/v1/messages", s.auth(s.cors(s.handleAnthropicMessages)))
	inner.HandleFunc("/v1/transcribe", s.auth(s.cors(s.handleTranscribe)))
	inner.HandleFunc("GET /v1/live/sessions/{liveID}/audio", s.auth(s.handleLiveSessionAudioRoute))
	inner.HandleFunc("/v1/live/sessions", s.auth(s.cors(s.handleLiveSessions)))
	inner.HandleFunc("/v1/live/sessions/", s.auth(s.cors(s.handleLiveSessionByID)))
	if s.jobsV2 != nil {
		inner.HandleFunc("/v2/jobs", s.auth(s.cors(s.handleJobsV2)))
		inner.HandleFunc("/v2/jobs/", s.auth(s.cors(s.handleJobV2ByID)))
		inner.HandleFunc("/v2/runs", s.auth(s.cors(s.handleRunsV2)))
		inner.HandleFunc("/v2/runs/", s.auth(s.cors(s.handleRunV2ByID)))
	}

	inner.HandleFunc("/images/", s.auth(s.cors(s.handleImage)))
	inner.HandleFunc("/media/", s.auth(s.cors(s.handleMedia)))
	inner.HandleFunc("/uploads/", s.auth(s.cors(s.handleUpload)))
	if s.cfg.filesDir != "" {
		inner.HandleFunc("/files/", s.auth(s.cors(s.handleFile)))
	}
	if s.widgetsMgr != nil {
		s.registerWidgetRoutes(inner)
	}
	if s.extensions != nil {
		s.registerExtensionRoutes(inner)
	}
	inner.HandleFunc("/v1/capabilities", s.auth(s.cors(s.handleCapabilities)))
	inner.HandleFunc("/v1/sharing/capabilities", s.auth(s.cors(s.handleSharingCapabilities)))
	inner.HandleFunc("/v1/project-directories", s.auth(s.cors(s.handleProjectDirectories)))
	inner.HandleFunc("/v1/projects", s.auth(s.cors(s.handleProjects)))
	inner.HandleFunc("/v1/projects/order", s.auth(s.cors(s.handleProjectsOrder)))
	inner.HandleFunc("/v1/projects/", s.auth(s.cors(s.handleProjectByID)))
	inner.HandleFunc("/v1/sidebar", s.auth(s.cors(s.handleSidebar)))
	inner.HandleFunc("/v1/events", s.auth(s.cors(s.handleEvents)))
	inner.HandleFunc("/v1/events/poll", s.auth(s.cors(s.handleEventPoll)))
	inner.HandleFunc("/v1/mcp/oauth/flows/", s.auth(s.cors(s.handleMCPOAuthFlow)))
	inner.HandleFunc("/v1/mcp/catalogue", s.auth(s.cors(s.handleMCPCatalogue)))
	inner.HandleFunc("/v1/mcp/servers", s.auth(s.cors(s.handleMCPServers)))
	inner.HandleFunc("/v1/mcp/servers/", s.auth(s.cors(s.handleMCPServerByName)))
	// OAuth callbacks cannot carry the serve bearer token. A high-entropy,
	// single-use SDK state value is the capability checked by this handler.
	inner.HandleFunc("/v1/mcp/oauth/callback", s.handleMCPOAuthCallback)
	inner.HandleFunc("/v1/sessions/status", s.auth(s.cors(s.handleSessionsStatus)))
	inner.HandleFunc("/v1/attention", s.auth(s.cors(s.handleAttention)))
	inner.HandleFunc("/v1/sessions/search", s.auth(s.cors(s.handleSessionsSearch)))
	inner.HandleFunc("/v1/sessions/pinned-order", s.auth(s.cors(s.handleSessionsPinnedOrder)))
	inner.HandleFunc("/v1/worktrees/diff", s.auth(s.cors(s.handleWorktreeDiff)))
	inner.HandleFunc("/v1/worktrees/merge", s.auth(s.cors(s.handleWorktreeMerge)))
	inner.HandleFunc("/v1/worktrees/assisted-merge", s.auth(s.cors(s.handleWorktreeAssistedMerge)))
	inner.HandleFunc("/v1/worktrees/promote", s.auth(s.cors(s.handleWorktreePromote)))
	inner.HandleFunc("/v1/worktrees", s.auth(s.cors(s.handleWorktrees)))
	inner.HandleFunc("/v1/sessions/", s.auth(s.cors(s.handleSessionByID)))
	inner.HandleFunc("/api/sessions/", s.auth(s.cors(s.handleSideQuestion)))
	inner.HandleFunc("/v1/push/subscribe", s.auth(s.cors(s.handlePushSubscribe)))

	if s.store != nil {
		inner.HandleFunc("/v1/sessions", s.auth(s.cors(s.handleSessions)))
	}
	if registerServeBrowserFixtureRoutes != nil {
		registerServeBrowserFixtureRoutes(inner, s)
	}

	if s.cfg.ui {
		inner.HandleFunc("/", s.cors(s.handleUI)) // catch-all SPA
	}

	// Jobs-only serve instances have no UI surface, so mount at root and keep
	// the canonical /v2/* API paths. The shared base-path wrapper is still used
	// for web/UI surfaces where the browser and API must live under one prefix.
	if s.jobsV2 != nil && !s.cfg.ui && !s.cfg.api {
		return s.reloadTransport().Handler(inner)
	}

	// Outer mux: mount everything under basePath.
	// Requests to basePath/ are handled by StripPrefix → inner.
	// Go's ServeMux auto-redirects basePath (no slash) → basePath/.
	prefix := s.cfg.basePath
	mux := http.NewServeMux()
	var handler http.Handler = inner
	if s.browserAuth != nil {
		handler = s.browserAuth.passkeyAuth(handler)
	}
	mux.Handle(prefix+"/", http.StripPrefix(prefix, handler))

	if s.cfg.ui {
		mux.HandleFunc("/", s.cors(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, prefix+"/", http.StatusTemporaryRedirect)
		}))
	}

	return s.reloadTransport().Handler(mux)
}
