package cmd

import (
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) handleSessionState(w http.ResponseWriter, r *http.Request, sessionID string) {
	resp := map[string]any{
		"active_run": false,
	}

	var persistedProvider, persistedModel, persistedEffort, persistedReasoningMode string
	var persistedGoal *session.Goal
	persistedGoalRead := false
	var runtimeDefaultModel string
	runtimeMetaRead := false

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
			if rt.engine != nil {
				if entries := rt.engine.ListPendingInterjections(); len(entries) > 0 {
					items := make([]map[string]any, 0, len(entries))
					for _, entry := range entries {
						text := strings.TrimSpace(entry.DisplayText)
						if text == "" {
							text = strings.TrimSpace(llm.MessageText(entry.Message))
						}
						if text == "" {
							text = strings.TrimSpace(llm.MessageAttachmentSummary(entry.Message))
						}
						item := map[string]any{
							"id":     entry.ID,
							"text":   text,
							"status": string(entry.Status),
						}
						if summary := strings.TrimSpace(llm.MessageAttachmentSummary(entry.Message)); summary != "" {
							item["attachment_summary"] = summary
						}
						items = append(items, item)
					}
					resp["pending_interjections"] = items
					resp["pending_interjection"] = items[0]
				}
			}
			if pk := strings.TrimSpace(rt.providerKey); pk != "" {
				persistedProvider = pk
			} else if rt.provider != nil {
				resolved := resolveSessionProviderKey(s.cfgRef, &session.Session{
					Provider: strings.TrimSpace(rt.provider.Name()),
				})
				persistedProvider = resolved
			}
			runtimeDefaultModel = strings.TrimSpace(rt.defaultModel)
			// rt.mu is held for the entire duration of a run; take it only
			// non-blockingly so state polls never stall a busy session. When
			// the lock is held, fall through to the DB for model/effort.
			if rt.mu.TryLock() {
				if rt.sessionMeta != nil {
					persistedModel = strings.TrimSpace(rt.sessionMeta.Model)
					persistedEffort = strings.TrimSpace(rt.sessionMeta.ReasoningEffort)
					persistedReasoningMode = strings.ToLower(strings.TrimSpace(rt.sessionMeta.ReasoningMode))
					persistedGoal = rt.sessionMeta.Goal.Clone()
					persistedGoalRead = true
				}
				mcpState := rt.mcpStateLocked()
				resp["mcp_servers"] = mcpState.Servers
				resp["mcp_enabled"] = mcpState.Enabled
				rt.mu.Unlock()
				runtimeMetaRead = true
			}
			if !activeRun {
				if lastErr := rt.consumeLastUIRunError(); lastErr != "" {
					resp["last_error"] = lastErr
				}
			}
		}
	}

	// Fall back to the DB when the runtime was not loaded (e.g. after a
	// page reload) or we could not read sessionMeta because a run held
	// rt.mu. The DB has the last persisted model/effort/MCP/goal selection for the session.
	if s.store != nil && (!runtimeMetaRead || persistedProvider == "" || persistedModel == "" || !persistedGoalRead) {
		if sess, err := s.store.Get(r.Context(), sessionID); err == nil && sess != nil {
			if !persistedGoalRead {
				persistedGoal = sess.Goal.Clone()
				persistedGoalRead = true
			}
			if enabled, ok := resp["mcp_enabled"].([]string); !ok || len(enabled) == 0 {
				if persistedMCP := parseServerList(sess.MCP); len(persistedMCP) > 0 {
					resp["mcp_enabled"] = persistedMCP
				}
			}
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
			if persistedReasoningMode == "" {
				persistedReasoningMode = strings.ToLower(strings.TrimSpace(sess.ReasoningMode))
			}
		}
	}

	if persistedModel == "" {
		persistedModel = runtimeDefaultModel
	}
	persistedModel, persistedEffort = normalizeProviderModelEffort(persistedProvider, persistedModel, persistedEffort)

	if persistedProvider != "" {
		resp["provider"] = persistedProvider
	}
	if persistedModel != "" {
		resp["model"] = persistedModel
	}
	if persistedEffort != "" {
		resp["reasoning_effort"] = persistedEffort
	}
	if persistedReasoningMode != "" {
		resp["reasoning_mode"] = persistedReasoningMode
	}
	if persistedGoal != nil && persistedGoal.Exists() {
		resp["goal"] = persistedGoal
	} else {
		resp["goal"] = nil
	}

	if lastResponseID := s.latestDurableResponseIDForSession(r.Context(), sessionID); lastResponseID != "" {
		resp["lastResponseId"] = lastResponseID
	}

	if s.responseRuns != nil {
		if activeResponseID := s.responseRuns.activeRunID(sessionID); activeResponseID != "" {
			resp["active_run"] = true
			resp["active_response_id"] = activeResponseID
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
