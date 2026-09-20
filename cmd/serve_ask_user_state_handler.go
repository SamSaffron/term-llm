package cmd

import (
	"context"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	planpkg "github.com/samsaffron/term-llm/internal/plan"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type webCurrentPlan struct {
	Version     int64          `json:"version"`
	Steps       []planpkg.Step `json:"steps"`
	Explanation string         `json:"explanation,omitempty"`
}

type webPlanSummary struct {
	Version        int64  `json:"version"`
	StepCount      int    `json:"step_count"`
	CompletedSteps int    `json:"completed_steps"`
	Position       int    `json:"position"`
	State          string `json:"state"`
}

type webSessionStateData struct {
	response               map[string]any
	persistedProvider      string
	persistedModel         string
	persistedEffort        string
	persistedReasoningMode string
	persistedGoal          *session.Goal
	persistedGoalRead      bool
	runtimeDefaultModel    string
	runtimeMetaRead        bool
	availability           llm.SteeringAvailability
	pendingItems           []map[string]any
	pendingIDs             map[string]struct{}
	pendingAuthoritative   bool
}

func summarizeWebPlan(snapshot planpkg.Snapshot, version int64) *webPlanSummary {
	if version <= 0 || snapshot.NormalizeAndValidate() != nil || len(snapshot.Plan) == 0 {
		return nil
	}
	summary := &webPlanSummary{
		Version:   version,
		StepCount: len(snapshot.Plan),
		Position:  1,
		State:     string(planpkg.StatusPending),
	}
	firstPending := 0
	for index, step := range snapshot.Plan {
		switch step.Status {
		case planpkg.StatusCompleted:
			summary.CompletedSteps++
		case planpkg.StatusInProgress:
			summary.Position = index + 1
			summary.State = string(planpkg.StatusInProgress)
		case planpkg.StatusPending:
			if firstPending == 0 {
				firstPending = index + 1
			}
		}
	}
	if summary.State != string(planpkg.StatusInProgress) {
		if firstPending > 0 {
			summary.Position = firstPending
		} else {
			summary.Position = summary.StepCount
			summary.State = string(planpkg.StatusCompleted)
		}
	}
	return summary
}

func planSnapshotStoreForWeb(store session.Store) (session.PlanSnapshotStore, bool) {
	if store == nil {
		return nil, false
	}
	// LoggingStore implements PlanSnapshotStore so it can preserve logging for
	// capable stores, but its embedded store remains the source of truth for
	// whether the optional capability exists at all.
	if loggingStore, ok := store.(*session.LoggingStore); ok {
		if _, supported := planSnapshotStoreForWeb(loggingStore.Store); !supported {
			return nil, false
		}
	}
	planStore, ok := store.(session.PlanSnapshotStore)
	return planStore, ok
}

func (s *serveServer) handleSessionState(w http.ResponseWriter, r *http.Request, sessionID string) {
	state := webSessionStateData{
		response: map[string]any{
			"active_run": false,
		},
	}
	s.addSessionStatePlan(state.response, r, sessionID)

	state.availability = llm.SteeringAvailability{Protocol: 1, UnavailableReason: "run_not_consuming"}
	s.addSessionStateRush(state.response, r, sessionID)
	state.pendingItems = make([]map[string]any, 0)
	state.pendingIDs = make(map[string]struct{})
	s.loadSessionStatePendingSteering(&state, r, sessionID)

	s.addSessionStateRuntime(&state, sessionID)
	s.addSessionStateResolvedInteractions(state.response, sessionID)
	s.finalizeSessionStateSteering(&state)
	s.loadSessionStatePersistedMetadata(&state, r, sessionID)
	s.addSessionStateMetadata(&state)

	if lastResponseID := s.latestDurableResponseIDForSession(r.Context(), sessionID); lastResponseID != "" {
		state.response["lastResponseId"] = lastResponseID
	}

	s.addSessionStateActiveRun(state.response, sessionID)
	// Sample the transcript revision after active-run state. Run finalization
	// commits transcript rows before clearing active ownership, so an idle sample
	// is paired with a revision that can include that final commit. Sampling in
	// the opposite order could publish idle plus a stale pre-final revision.
	s.addSessionStateTranscriptRev(state.response, r, sessionID)

	writeJSON(w, http.StatusOK, state.response)
}

func (s *serveServer) addSessionStatePlan(resp map[string]any, r *http.Request, sessionID string) {
	if planStore, ok := planSnapshotStoreForWeb(s.store); ok {
		snapshot, version, err := planStore.LoadPlanSnapshot(r.Context(), sessionID)
		switch {
		case err != nil:
			// Omit the field rather than turning a transient read failure into an
			// authoritative clear on the client.
		case version <= 0:
			resp["current_plan"] = nil
		case snapshot.NormalizeAndValidate() == nil && len(snapshot.Plan) > 0:
			resp["current_plan"] = webCurrentPlan{
				Version:     version,
				Steps:       snapshot.Plan,
				Explanation: snapshot.Explanation,
			}
		}
	}
}

func (s *serveServer) addSessionStateRush(resp map[string]any, r *http.Request, sessionID string) {
	if rushStore, ok := session.AsRushStore(s.store); ok {
		if op, err := rushStore.LatestRush(r.Context(), sessionID); err == nil {
			if transition := s.ensureResponseRuns().steeringTransition(sessionID); transition != nil {
				if failed := transition.failure.Load(); failed != nil {
					_ = restart.Default.Go(r.Context(), func(context.Context) { s.finishSteeringRush(rushStore, transition, failed.status, failed.reason) })
				}
			}

			// A missing process-local owner after restart is ambiguous external
			// execution, not permission to start a replacement automatically.
			if op.Status.Active() && s.ensureResponseRuns().steeringTransition(sessionID) == nil {
				running := false
				if s.responseRuns != nil {
					_, running = s.responseRuns.get(op.SourceResponseID)
				}
				if !running {
					op, _ = rushStore.AdvanceRush(r.Context(), op, session.RushBlocked, "settlement_unknown")
				}
			}
			resp["active_rush"] = op
		}
	}
}

func (s *serveServer) loadSessionStatePendingSteering(state *webSessionStateData, r *http.Request, sessionID string) {
	if pendingStore, ok := session.AsPendingSteeringStore(s.store); ok {
		if entries, err := pendingStore.ListPendingSteering(r.Context(), sessionID); err == nil {
			state.pendingAuthoritative = true
			for _, entry := range entries {
				if entry.OwnerKind != "" {
					continue
				}
				item := map[string]any{
					"id":     entry.ID,
					"text":   entry.DisplayText,
					"origin": entry.Origin,
					"status": string(llm.SteeringQueued),
				}
				if entry.AttachmentSummary != "" {
					item["attachment_summary"] = entry.AttachmentSummary
				}
				state.pendingItems = append(state.pendingItems, item)
				state.pendingIDs[entry.ID] = struct{}{}
			}
		}
	}
}

func (s *serveServer) addSessionStateRuntime(state *webSessionStateData, sessionID string) {
	if s.sessionMgr != nil {
		if rt, ok := s.sessionMgr.Get(sessionID); ok && rt != nil {
			activeRun := rt.hasActiveRun()
			state.response["active_run"] = activeRun
			if s.approvalDefault != tools.ModeYolo {
				state.response["approval_policy"] = runtimeApprovalPolicy(rt)
			}
			s.addSessionStateRuntimePrompts(state.response, rt)
			s.addSessionStateRuntimeSteering(state, rt)
			s.addSessionStateRuntimeProvider(state, rt)
			state.runtimeDefaultModel = strings.TrimSpace(rt.defaultModel)
			// rt.mu is held for the entire duration of a run; take it only
			// non-blockingly so state polls never stall a busy session. When
			// the lock is held, fall through to the DB for model/effort.
			s.readSessionStateRuntimeMetadata(state, rt)
			if !activeRun {
				if lastErr := rt.consumeLastUIRunError(); lastErr != "" {
					state.response["last_error"] = lastErr
				}
			}
		}
	}
}

func (s *serveServer) addSessionStateRuntimePrompts(resp map[string]any, rt *serveRuntime) {
	if prompts := rt.pendingAskUserPrompts(); len(prompts) > 0 {
		resp["pending_ask_users"] = prompts
		resp["pending_ask_user"] = prompts[0]
	}
	if approvals := rt.pendingApprovalPrompts(); len(approvals) > 0 {
		resp["pending_approvals"] = approvals
		resp["pending_approval"] = approvals[0]
	}
}

func (s *serveServer) addSessionStateRuntimeSteering(state *webSessionStateData, rt *serveRuntime) {
	if rt.engine != nil {
		state.availability = rt.engine.SteeringAvailability()
		if entries := rt.engine.ListPendingSteering(); len(entries) > 0 {
			for _, entry := range entries {
				if _, exists := state.pendingIDs[entry.ID]; exists {
					continue
				}
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
				state.pendingItems = append(state.pendingItems, item)
				state.pendingIDs[entry.ID] = struct{}{}
			}
		}
	}
}

func (s *serveServer) addSessionStateRuntimeProvider(state *webSessionStateData, rt *serveRuntime) {
	if pk := strings.TrimSpace(rt.providerKey); pk != "" {
		state.persistedProvider = pk
	} else if rt.provider != nil {
		resolved := resolveSessionProviderKey(s.cfgRef, &session.Session{
			Provider: strings.TrimSpace(rt.provider.Name()),
		})
		state.persistedProvider = resolved
	}
}

func (s *serveServer) readSessionStateRuntimeMetadata(state *webSessionStateData, rt *serveRuntime) {
	if rt.mu.TryLock() {
		if rt.sessionMeta != nil {
			state.persistedModel = strings.TrimSpace(rt.sessionMeta.Model)
			state.persistedEffort = strings.TrimSpace(rt.sessionMeta.ReasoningEffort)
			state.persistedReasoningMode = strings.ToLower(strings.TrimSpace(rt.sessionMeta.ReasoningMode))
			state.persistedGoal = rt.sessionMeta.Goal.Clone()
			state.persistedGoalRead = true
		}
		mcpState := rt.mcpStateLocked()
		state.response["mcp_servers"] = mcpState.Servers
		state.response["mcp_enabled"] = mcpState.Enabled
		rt.mu.Unlock()
		state.runtimeMetaRead = true
	}
}

func (s *serveServer) addSessionStateResolvedInteractions(resp map[string]any, sessionID string) {
	if s.responseRuns != nil {
		if run := s.responseRuns.latestRun(sessionID); run != nil {
			if resolved := run.resolvedInteractionsSnapshot(); len(resolved) > 0 {
				resp["resolved_interactions"] = resolved
			}
		}
	}
}

func (s *serveServer) finalizeSessionStateSteering(state *webSessionStateData) {
	if _, ok := session.AsRushStore(s.store); !ok {
		state.availability.CanRush = false
		state.availability.UnavailableReason = "durable_store_unavailable"
	}
	eligiblePending := false
	for _, item := range state.pendingItems {
		if item["origin"] != llm.SteeringOriginJobNotification {
			eligiblePending = true
		}
	}
	if !eligiblePending && state.availability.CanRush {
		state.availability.CanRush = false
		state.availability.UnavailableReason = "no_user_steering"
	}
	state.response["steering"] = state.availability
	if state.pendingAuthoritative || len(state.pendingItems) > 0 {
		state.response["pending_steering"] = state.pendingItems
		if len(state.pendingItems) > 0 {
			state.response["pending_steering_text"] = state.pendingItems[0]
		}
	}
}

func (s *serveServer) loadSessionStatePersistedMetadata(state *webSessionStateData, r *http.Request, sessionID string) {
	// Fall back to the DB when the runtime was not loaded (e.g. after a
	// page reload) or we could not read sessionMeta because a run held
	// rt.mu. The DB has the last persisted model/effort/MCP/goal selection for the session.
	if s.store != nil && (!state.runtimeMetaRead || state.persistedProvider == "" || state.persistedModel == "" || !state.persistedGoalRead) {
		if sess, err := s.store.Get(r.Context(), sessionID); err == nil && sess != nil {
			if !state.persistedGoalRead {
				state.persistedGoal = sess.Goal.Clone()
				state.persistedGoalRead = true
			}
			if enabled, ok := state.response["mcp_enabled"].([]string); !ok || len(enabled) == 0 {
				if persistedMCP := parseServerList(sess.MCP); len(persistedMCP) > 0 {
					state.response["mcp_enabled"] = persistedMCP
				}
			}
			if state.persistedProvider == "" {
				pk := strings.TrimSpace(sess.ProviderKey)
				if pk == "" {
					pk = resolveSessionProviderKey(s.cfgRef, sess)
				}
				state.persistedProvider = pk
			}
			if state.persistedModel == "" {
				state.persistedModel = strings.TrimSpace(sess.Model)
			}
			if state.persistedEffort == "" {
				state.persistedEffort = strings.TrimSpace(sess.ReasoningEffort)
			}
			if state.persistedReasoningMode == "" {
				state.persistedReasoningMode = strings.ToLower(strings.TrimSpace(sess.ReasoningMode))
			}
		}
	}
}

func (s *serveServer) addSessionStateMetadata(state *webSessionStateData) {
	if state.persistedModel == "" {
		state.persistedModel = state.runtimeDefaultModel
	}
	state.persistedModel, state.persistedEffort = normalizeProviderModelEffort(state.persistedProvider, state.persistedModel, state.persistedEffort)

	if state.persistedProvider != "" {
		state.response["provider"] = state.persistedProvider
	}
	if state.persistedModel != "" {
		state.response["model"] = state.persistedModel
	}
	if state.persistedEffort != "" {
		state.response["reasoning_effort"] = state.persistedEffort
	}
	if state.persistedReasoningMode != "" {
		state.response["reasoning_mode"] = state.persistedReasoningMode
	}
	if state.persistedGoal != nil && state.persistedGoal.Exists() {
		state.response["goal"] = state.persistedGoal
	} else {
		state.response["goal"] = nil
	}
}

func (s *serveServer) addSessionStateActiveRun(resp map[string]any, sessionID string) {
	if s.responseRuns != nil {
		if activeResponseID := s.responseRuns.activeRunID(sessionID); activeResponseID != "" {
			resp["active_run"] = true
			resp["active_response_id"] = activeResponseID
			if run, ok := s.responseRuns.get(activeResponseID); ok && run != nil {
				run.mu.Lock()
				resp["started_rev"] = run.startedRev
				resp["run_epoch"] = run.runEpoch
				if run.clientMessageID != "" {
					resp["client_message_id"] = run.clientMessageID
				}
				if run.anchorRowID > 0 {
					resp["anchor_row_id"] = run.anchorRowID
				}
				run.mu.Unlock()
			}
		}
	}
}

func (s *serveServer) addSessionStateTranscriptRev(resp map[string]any, r *http.Request, sessionID string) {
	if indexer, ok := s.transcriptIndexerForWeb(); ok {
		if rev, err := indexer.TranscriptRev(r.Context(), sessionID); err == nil {
			resp["transcript_rev"] = rev
		}
	}
}
