package cmd

import (
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) handleSessionState(w http.ResponseWriter, r *http.Request, sessionID string) {
	resp := map[string]any{
		"active_run": false,
	}

	var persistedProvider, persistedModel, persistedEffort string

	if s.sessionMgr != nil {
		if rt, ok := s.sessionMgr.Get(sessionID); ok && rt != nil {
			activeRun := rt.hasActiveRun()
			resp["active_run"] = activeRun
			if prompts := rt.pendingAskUserPrompts(); len(prompts) > 0 {
				resp["pending_ask_users"] = prompts
				resp["pending_ask_user"] = prompts[0]
			}
			if approvals := rt.pendingApprovalPrompts(); len(approvals) > 0 {
				resp["pending_approvals"] = approvals
				resp["pending_approval"] = approvals[0]
			}
			if pk := strings.TrimSpace(rt.providerKey); pk != "" {
				persistedProvider = pk
			} else if rt.provider != nil {
				resolved := resolveSessionProviderKey(s.cfgRef, &session.Session{
					Provider: strings.TrimSpace(rt.provider.Name()),
				})
				persistedProvider = resolved
			}
			rt.mu.Lock()
			if rt.sessionMeta != nil {
				persistedModel = strings.TrimSpace(rt.sessionMeta.Model)
				persistedEffort = strings.TrimSpace(rt.sessionMeta.ReasoningEffort)
			}
			rt.mu.Unlock()
			if persistedModel == "" {
				persistedModel = strings.TrimSpace(rt.defaultModel)
			}
			if !activeRun {
				if lastErr := rt.consumeLastUIRunError(); lastErr != "" {
					resp["last_error"] = lastErr
				}
			}
		}
	}

	// Fall back to DB when the runtime is not loaded (e.g. after a page reload).
	if s.store != nil && (persistedProvider == "" || persistedModel == "") {
		if sess, err := s.store.Get(r.Context(), sessionID); err == nil && sess != nil {
			if persistedProvider == "" {
				pk := strings.TrimSpace(sess.ProviderKey)
				if pk == "" {
					pk = resolveSessionProviderKey(s.cfgRef, sess)
				}
				persistedProvider = pk
			}
			if persistedModel == "" {
				persistedModel = strings.TrimSpace(sess.Model)
			}
			if persistedEffort == "" {
				persistedEffort = strings.TrimSpace(sess.ReasoningEffort)
			}
		}
	}

	if persistedProvider != "" {
		resp["provider"] = persistedProvider
	}
	if persistedModel != "" {
		resp["model"] = persistedModel
	}
	if persistedEffort != "" {
		resp["reasoning_effort"] = persistedEffort
	}

	if s.responseRuns != nil {
		if activeResponseID := s.responseRuns.activeRunID(sessionID); activeResponseID != "" {
			resp["active_run"] = true
			resp["active_response_id"] = activeResponseID
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
