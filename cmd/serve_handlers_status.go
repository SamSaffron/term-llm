package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
)

func listStatusAttention(ctx context.Context, store session.AttentionStore, kind session.AttentionKind) map[string]session.AttentionItem {
	for attempt := 0; attempt < 2; attempt++ {
		candidate := make(map[string]session.AttentionItem)
		cursor := ""
		var snapshotVersion int64
		complete := true
		for {
			page, err := store.ListAttention(ctx, session.AttentionListOptions{
				Kind: kind, Limit: 500, Cursor: cursor, SnapshotVersion: snapshotVersion,
			})
			if err != nil {
				complete = false
				if !errors.Is(err, session.ErrAttentionConflict) || attempt == 1 {
					log.Printf("[serve] durable %s status projection unavailable: %v", kind, err)
				}
				break
			}
			if snapshotVersion == 0 {
				snapshotVersion = page.SnapshotVersion
			}
			for _, item := range page.Items {
				candidate[item.SessionID] = item
			}
			if !page.HasMore || page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		if complete {
			return candidate
		}
	}
	return map[string]session.AttentionItem{}
}

func (s *serveServer) handleSessionsStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if s.store == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", "session store not available")
		return
	}

	categories, err := parseSidebarSessionCategories(r.URL.Query().Get("categories"), false)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	includeArchived := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "1") ||
		strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "true")

	sessions, err := s.store.List(r.Context(), session.ListOptions{
		// Bound the polling projection independently of the total session history.
		// The grouped sidebar pages older rows on demand; status polling is only a
		// freshness hint and must remain cheap for long-lived stores.
		Limit:            200,
		Archived:         includeArchived,
		Categories:       categories,
		SortByActivity:   true,
		ExcludeSubagents: true,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to list sessions")
		return
	}

	// Collect active session IDs from in-memory state without touching runtimes.
	activeIDs := s.activeSessionIDs()
	attentionStore, attentionSupported := session.AsAttentionStore(s.store)
	_, interactionProjectionSupported := session.AsResponseRunInteractionStore(s.store)
	durableRunning := make(map[string]session.AttentionItem)
	durableInputRequired := make(map[string]session.AttentionItem)
	if attentionSupported {
		durableRunning = listStatusAttention(r.Context(), attentionStore, session.AttentionKindRunning)
		for id := range durableRunning {
			activeIDs[id] = true
		}
		if interactionProjectionSupported {
			durableInputRequired = listStatusAttention(r.Context(), attentionStore, session.AttentionKindInputRequired)
		}
	}

	runtimeInteractions := make(map[string]serveInteractionSummary)
	if s.sessionMgr != nil {
		runtimeInteractions = s.sessionMgr.UnresolvedInteractionSummaries()
	}
	criticalIDs := make(map[string]bool, len(activeIDs)+1)
	if selected := strings.TrimSpace(r.URL.Query().Get("selected_session")); selected != "" {
		criticalIDs[selected] = true
	}
	for id := range activeIDs {
		criticalIDs[id] = true
	}
	for id := range runtimeInteractions {
		criticalIDs[id] = true
	}
	listed := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		listed[sess.ID] = true
	}
	missing := make([]string, 0, len(criticalIDs))
	for id := range criticalIDs {
		if !listed[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		critical, listErr := s.store.List(r.Context(), session.ListOptions{
			IDs:            missing,
			Limit:          -1,
			Archived:       true,
			SortByActivity: true,
		})
		if listErr != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load active sessions")
			return
		}
		for _, sess := range critical {
			if criticalIDs[sess.ID] && !listed[sess.ID] {
				sessions = append(sessions, sess)
				listed[sess.ID] = true
			}
		}
	}

	attentionBySession := make(map[string]session.AttentionState)
	attentionBatchSupported := false
	if attentionSupported {
		ids := make([]string, 0, len(sessions))
		for _, sess := range sessions {
			ids = append(ids, sess.ID)
		}
		if batch, ok := session.AsAttentionBatchStore(s.store); ok {
			attentionBatchSupported = true
			if states, batchErr := batch.GetAttentionBatch(r.Context(), ids); batchErr == nil {
				attentionBySession = states
			} else {
				log.Printf("[serve] status attention projection unavailable: %v", batchErr)
			}
		}
	}

	// transcript_updated_at remains for older cached clients and hub dashboards.
	// Revision-aware clients use transcript_rev as the correctness signal.
	type statusEntry struct {
		ID                       string   `json:"id"`
		Number                   int64    `json:"number,omitempty"`
		ProjectID                string   `json:"project_id,omitempty"`
		ProjectName              string   `json:"project_name,omitempty"`
		ShortTitle               string   `json:"short_title"`
		LongTitle                string   `json:"long_title"`
		ActiveRun                bool     `json:"active_run,omitempty"`
		ActiveResponseID         string   `json:"active_response_id,omitempty"`
		RunEpoch                 int64    `json:"run_epoch,omitempty"`
		StartedRev               int64    `json:"started_rev,omitempty"`
		StartedAt                int64    `json:"started_at,omitempty"`
		ClientMessageID          string   `json:"client_message_id,omitempty"`
		AnchorRowID              int64    `json:"anchor_row_id,omitempty"`
		TranscriptRev            int64    `json:"transcript_rev"`
		MsgCount                 int      `json:"message_count"`
		LastMessageAt            int64    `json:"last_message_at"`
		TranscriptUpdatedAt      int64    `json:"transcript_updated_at"`
		AttentionStoreInstanceID string   `json:"attention_store_instance_id,omitempty"`
		AttentionSeq             int64    `json:"attention_seq,omitempty"`
		AttentionResponseID      string   `json:"attention_response_id,omitempty"`
		AttentionFinalRev        int64    `json:"attention_final_rev,omitempty"`
		SeenThroughSeq           int64    `json:"seen_through_seq,omitempty"`
		AttentionUnseen          bool     `json:"attention_unseen,omitempty"`
		AttentionOutcome         string   `json:"attention_outcome,omitempty"`
		AttentionTerminalAt      int64    `json:"attention_terminal_at,omitempty"`
		InteractionRequired      bool     `json:"interaction_required"`
		InteractionResponseID    string   `json:"interaction_response_id,omitempty"`
		InteractionStateRev      int64    `json:"interaction_state_rev,omitempty"`
		PendingInteractionCount  int      `json:"pending_interaction_count,omitempty"`
		PendingInteractionKinds  []string `json:"pending_interaction_kinds,omitempty"`
		InteractionRequiredSince int64    `json:"interaction_required_since,omitempty"`
	}

	result := make([]statusEntry, 0, len(sessions))
	indexer, revisioned := s.transcriptIndexerForWeb()
	summaryRevisions := false
	if reporter, ok := s.store.(session.SessionSummaryTranscriptRevisionReporter); ok {
		summaryRevisions = reporter.SessionSummariesIncludeTranscriptRev()
	}
	for _, sess := range sessions {
		lastMessageAt := sess.LastMessageAt
		if lastMessageAt.IsZero() {
			lastMessageAt = sess.CreatedAt
		}
		transcriptUpdatedAt := sess.UpdatedAt
		if transcriptUpdatedAt.IsZero() {
			transcriptUpdatedAt = sess.CreatedAt
		}
		transcriptRev := sess.TranscriptRev
		if revisioned && (!summaryRevisions || transcriptRev == 0) {
			if rev, revErr := indexer.TranscriptRev(r.Context(), sess.ID); revErr == nil {
				transcriptRev = rev
			}
		}
		activeResponseID, startedRev, runEpoch, clientMessageID, anchorRowID := s.activeTranscriptRun(sess.ID)
		startedAt := int64(0)
		if s.responseRuns != nil {
			if run := s.responseRuns.activeRun(sess.ID); run != nil && run.created > 0 {
				startedAt = run.created * 1000
			}
		}
		if durable, ok := durableRunning[sess.ID]; ok && activeResponseID == "" {
			// A durable row owned by another/restarted process is authoritative for
			// the running indicator, but this process cannot serve its event stream.
			// Do not invite the browser to attach to a response ID it does not own.
			startedRev = durable.StartedRev
			startedAt = durable.StartedAt.UnixMilli()
		}
		var attention session.AttentionState
		attentionTerminalAt := int64(0)
		if attentionSupported {
			attention = attentionBySession[sess.ID]
			if !attentionBatchSupported {
				attention, _ = attentionStore.GetAttention(r.Context(), sess.ID)
			}
			if !attention.TerminalAt.IsZero() {
				attentionTerminalAt = attention.TerminalAt.UnixMilli()
			}
		}
		var interactionRequired bool
		var interactionResponseID string
		var interactionStateRev int64
		var pendingInteractionCount int
		var pendingInteractionKinds []string
		var interactionRequiredSince int64
		if durable, ok := durableInputRequired[sess.ID]; ok {
			interactionRequired = durable.InteractionRequired
			interactionResponseID = durable.ResponseID
			interactionStateRev = durable.InteractionStateRev
			pendingInteractionCount = durable.PendingInteractionCount
			pendingInteractionKinds = append([]string(nil), durable.PendingInteractionKinds...)
			if !durable.InteractionRequiredSince.IsZero() {
				interactionRequiredSince = durable.InteractionRequiredSince.UnixMilli()
			}
		}
		if runtimeState, ok := runtimeInteractions[sess.ID]; ok {
			interactionRequired = runtimeState.Count > 0
			interactionResponseID = activeResponseID
			interactionStateRev = 0
			pendingInteractionCount = runtimeState.Count
			pendingInteractionKinds = append([]string(nil), runtimeState.Kinds...)
			if !runtimeState.RequiredSince.IsZero() {
				interactionRequiredSince = runtimeState.RequiredSince.UnixMilli()
			}
		}
		if s.responseRuns != nil {
			if activeRun := s.responseRuns.activeRun(sess.ID); activeRun != nil {
				state := activeRun.interactionState()
				interactionRequired = state.Count > 0
				interactionResponseID = state.ResponseID
				interactionStateRev = state.Revision
				pendingInteractionCount = state.Count
				pendingInteractionKinds = append([]string(nil), state.Kinds...)
				interactionRequiredSince = 0
				if !state.RequiredSince.IsZero() {
					interactionRequiredSince = state.RequiredSince.UnixMilli()
				}
			}
		}
		result = append(result, statusEntry{
			ID:                       sess.ID,
			Number:                   sess.Number,
			ProjectID:                sess.ProjectID,
			ProjectName:              sess.ProjectName,
			ShortTitle:               sess.PreferredShortTitle(),
			LongTitle:                sess.PreferredLongTitle(),
			ActiveRun:                activeIDs[sess.ID],
			ActiveResponseID:         activeResponseID,
			RunEpoch:                 runEpoch,
			StartedRev:               startedRev,
			StartedAt:                startedAt,
			ClientMessageID:          clientMessageID,
			AnchorRowID:              anchorRowID,
			TranscriptRev:            transcriptRev,
			MsgCount:                 sess.MessageCount,
			LastMessageAt:            lastMessageAt.UnixMilli(),
			TranscriptUpdatedAt:      transcriptUpdatedAt.UnixMilli(),
			AttentionStoreInstanceID: attention.StoreInstanceID,
			AttentionSeq:             attention.LatestAttentionSeq,
			AttentionResponseID:      attention.ResponseID,
			AttentionFinalRev:        attention.FinalRev,
			SeenThroughSeq:           attention.SeenThroughSeq,
			AttentionUnseen:          attention.Unseen,
			AttentionOutcome:         string(attention.Outcome),
			AttentionTerminalAt:      attentionTerminalAt,
			InteractionRequired:      interactionRequired,
			InteractionResponseID:    interactionResponseID,
			InteractionStateRev:      interactionStateRev,
			PendingInteractionCount:  pendingInteractionCount,
			PendingInteractionKinds:  pendingInteractionKinds,
			InteractionRequiredSince: interactionRequiredSince,
		})
	}

	body, _ := json.Marshal(map[string]any{"sessions": result})

	// ETag for conditional requests — avoids re-transmitting unchanged data.
	hash := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(hash[:8]) + `"`

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// activeSessionIDs returns the set of session IDs that have an active run,
// either via the session manager (in-memory runtimes) or via detached
// response runs. It does NOT touch runtimes (no TTL refresh).
func (s *serveServer) activeSessionIDs() map[string]bool {
	result := make(map[string]bool)
	if s.sessionMgr != nil {
		for id, active := range s.sessionMgr.ActiveSessionIDs() {
			if active {
				result[id] = true
			}
		}
	}
	if s.responseRuns != nil {
		for id := range s.responseRuns.ActiveSessionIDs() {
			result[id] = true
		}
	}
	return result
}
