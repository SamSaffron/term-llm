package serve

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/samsaffron/term-llm/internal/app/uploads"
	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	worktreesEnabled := false
	if s.projectsEnabled {
		if projects, ok := s.projectStore(); ok {
			if list, err := projects.ListProjects(r.Context(), session.ProjectListOptions{}); err == nil {
				statusCtx, cancelStatus := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancelStatus()
				for i, p := range list {
					if status := s.cachedProjectStatus(statusCtx, p, i < maxEagerProjectStatusChecks); status.Available && status.Git {
						worktreesEnabled = true
						break
					}
				}
			}
		}
	} else if _, err := s.currentGitRoot(); err == nil {
		worktreesEnabled = true
	}
	shellEnabled := s.cfg.ui && platformServeShellSupported() && s.store != nil
	liveCapability := s.liveCapability(r.Context())
	w.Header().Set("ETag", fmt.Sprintf(`W/"projects-%t-worktrees-%t-shell-%t-live-%t-%s-%v-%v"`,
		s.projectsEnabled, worktreesEnabled, shellEnabled, liveCapability["enabled"], liveCapability["provider"], liveCapability["transport"], liveCapability["version"]))
	payload := map[string]any{
		"projects":  map[string]bool{"enabled": s.projectsEnabled},
		"worktrees": map[string]bool{"enabled": worktreesEnabled},
		"shell": map[string]any{
			"enabled": shellEnabled, "version": 1,
			"transport": "http_sse", "replay_bytes": serveShellReplayBytes,
		},
		"live": liveCapability,
		"event_feed": map[string]any{
			"version": 1, "sse": true, "long_poll": true,
			"heartbeat_ms": serveEventHeartbeat.Milliseconds(), "replay_limit": serveEventReplayLimit,
		},
		"attachments": map[string]any{
			"max_count":  uploads.MaxAttachments,
			"max_bytes":  uploads.MaxAttachmentBytes,
			"mime_types": []string{"*/*"},
			"extensions": []string{},
		},
	}
	if attention, ok := session.AsAttentionStore(s.store); ok {
		if storeID, err := attention.StoreInstanceID(r.Context()); err == nil {
			protocolVersion := 1
			if _, supported := session.AsResponseRunInteractionStore(s.store); supported {
				protocolVersion = 2
			}
			payload["attention"] = map[string]any{"enabled": true, "protocol_version": protocolVersion, "store_instance_id": storeID}
		} else {
			payload["attention"] = map[string]bool{"enabled": false}
		}
	} else {
		payload["attention"] = map[string]bool{"enabled": false}
	}
	if s.responseRuns != nil {
		diagnostics := s.responseRuns.Diagnostics()
		payload["reliability_diagnostics"] = map[string]any{
			"idempotency_replays": diagnostics.IdempotencyReplays,
		}
	}
	payload["attention_diagnostics"] = s.attentionDiagnostics.snapshot()
	writeJSON(w, http.StatusOK, payload)
}
