package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	maxChildRunProjection = 100
	// Child revisions are persisted in browser storage and compared as JavaScript
	// numbers. Keep them below Number.MAX_SAFE_INTEGER even for corrupt/future
	// timestamps.
	maxChildReadRevision int64 = 1<<53 - 1
)

func safeChildRevision(values ...int64) int64 {
	var revision int64
	for _, value := range values {
		if value > revision {
			revision = value
		}
	}
	if revision < 0 {
		return 0
	}
	if revision > maxChildReadRevision {
		return maxChildReadRevision
	}
	return revision
}

func terminalChildRevision(revision int64) int64 {
	revision = safeChildRevision(revision)
	if revision < maxChildReadRevision {
		return revision + 1
	}
	return revision
}

type childRunProjection struct {
	SessionID         string                `json:"session_id"`
	ParentSessionID   string                `json:"parent_session_id"`
	ParentSpawnItemID int64                 `json:"parent_spawn_item_id,omitempty"`
	ParentSpawnCallID string                `json:"parent_spawn_call_id,omitempty"`
	Title             string                `json:"title"`
	Agent             string                `json:"agent,omitempty"`
	TaskSummary       string                `json:"task_summary,omitempty"`
	State             session.SessionStatus `json:"state"`
	Attention         bool                  `json:"attention"`
	ResponseID        string                `json:"response_id,omitempty"`
	RunEpoch          int64                 `json:"run_epoch,omitempty"`
	Revision          int64                 `json:"revision"`
	StartedAt         int64                 `json:"started_at,omitempty"`
	EndedAt           int64                 `json:"ended_at,omitempty"`
	ApproximateTimes  bool                  `json:"approximate_times,omitempty"`
}

type childSpawnProvenance struct {
	ItemID int64
	CallID string
}

type descendingMessageReader interface {
	GetMessagesPageDescending(ctx context.Context, sessionID string, beforeSeq, limit int) ([]session.Message, error)
}

func childSpawnProvenanceForParent(ctx context.Context, store session.Store, parentID string) map[string]childSpawnProvenance {
	reader, ok := store.(descendingMessageReader)
	if !ok {
		return nil
	}
	messages, err := reader.GetMessagesPageDescending(ctx, parentID, 0, 500)
	if err != nil {
		return nil
	}
	calls := make(map[string]int64)
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.ToolCall != nil && part.ToolCall.Name == tools.SpawnAgentToolName {
				calls[part.ToolCall.ID] = message.ID
			}
		}
	}
	provenance := make(map[string]childSpawnProvenance)
	for _, message := range messages {
		for _, part := range message.Parts {
			result := part.ToolResult
			if result == nil || result.Name != tools.SpawnAgentToolName {
				continue
			}
			var spawn tools.SpawnAgentResult
			if json.Unmarshal([]byte(result.Content), &spawn) != nil || strings.TrimSpace(spawn.SessionID) == "" {
				continue
			}
			provenance[spawn.SessionID] = childSpawnProvenance{ItemID: calls[result.ID], CallID: result.ID}
		}
	}
	return provenance
}

func (s *serveServer) handleSessionChildren(w http.ResponseWriter, r *http.Request, parentID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}
	if parent, err := s.store.Get(r.Context(), parentID); err != nil || parent == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "parent session not found")
		return
	}
	spawnProvenance := childSpawnProvenanceForParent(r.Context(), s.store, parentID)
	children, err := s.store.List(r.Context(), session.ListOptions{
		ParentID:       parentID,
		Limit:          maxChildRunProjection,
		SortByActivity: true,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to list child sessions")
		return
	}
	sort.SliceStable(children, func(i, j int) bool {
		if children[i].UpdatedAt.Equal(children[j].UpdatedAt) {
			return children[i].ID < children[j].ID
		}
		return children[i].UpdatedAt.After(children[j].UpdatedAt)
	})
	type childETagSnapshot struct {
		Projection       childRunProjection `json:"projection"`
		DurableUpdatedAt int64              `json:"durable_updated_at"`
		LastSequence     int64              `json:"last_sequence"`
	}
	items := make([]childRunProjection, 0, min(len(children), maxChildRunProjection))
	etagItems := make([]childETagSnapshot, 0, min(len(children), maxChildRunProjection))
	for _, child := range children {
		durableUpdatedAt := child.UpdatedAt.UnixNano()
		item := childRunProjection{
			SessionID:        child.ID,
			ParentSessionID:  parentID,
			Title:            child.PreferredShortTitle(),
			Agent:            child.Agent,
			State:            child.Status,
			Attention:        child.Status == session.StatusError,
			Revision:         safeChildRevision(child.UpdatedAt.UnixMilli()),
			StartedAt:        child.CreatedAt.UnixMilli(),
			ApproximateTimes: true,
		}
		terminal := child.Status == session.StatusComplete || child.Status == session.StatusError
		if spawn, ok := spawnProvenance[child.ID]; ok {
			item.ParentSpawnItemID = spawn.ItemID
			item.ParentSpawnCallID = spawn.CallID
		}
		if terminal {
			item.EndedAt = child.UpdatedAt.UnixMilli()
			item.Revision = terminalChildRevision(item.Revision)
		}
		if messages, messagesErr := s.store.GetMessages(r.Context(), child.ID, 1, 0); messagesErr == nil && len(messages) > 0 {
			item.TaskSummary = strings.TrimSpace(messages[0].TextContent)
			if runes := []rune(item.TaskSummary); len(runes) > 240 {
				item.TaskSummary = string(runes[:240]) + "…"
			}
		}
		var lastSequence int64
		if run := s.responseRuns.latestRun(child.ID); run != nil {
			run.mu.Lock()
			item.ResponseID = run.id
			item.RunEpoch = run.runEpoch
			lastSequence = run.lastSequenceNumber
			if run.created > 0 {
				item.StartedAt = run.created * 1000
				item.Revision = safeChildRevision(item.Revision, item.StartedAt)
				item.ApproximateTimes = false
			}
			if run.endedAt > 0 {
				item.EndedAt = run.endedAt
				item.Revision = terminalChildRevision(safeChildRevision(item.Revision, run.endedAt))
				terminal = true
			}
			run.mu.Unlock()
		}
		if s.sessionMgr != nil {
			if runtime, ok := s.sessionMgr.Get(child.ID); ok && runtime != nil {
				for _, prompt := range runtime.pendingApprovalPrompts() {
					item.Attention = true
					item.Revision = safeChildRevision(item.Revision, prompt.CreatedAt*1000)
				}
				for _, prompt := range runtime.pendingAskUserPrompts() {
					item.Attention = true
					item.Revision = safeChildRevision(item.Revision, prompt.CreatedAt*1000)
				}
			}
		}
		if terminal {
			// Attention timestamps may have advanced the projection after the first
			// terminal bump. Preserve the terminal marker without encoding event
			// sequence arithmetic into the browser read marker.
			item.Revision = terminalChildRevision(item.Revision)
		}
		items = append(items, item)
		etagItems = append(etagItems, childETagSnapshot{
			Projection:       item,
			DurableUpdatedAt: durableUpdatedAt,
			LastSequence:     lastSequence,
		})
		if len(items) == maxChildRunProjection {
			break
		}
	}
	var revision int64
	for _, item := range items {
		if item.Revision > revision {
			revision = item.Revision
		}
	}
	etagPayload, err := json.Marshal(struct {
		ParentID string              `json:"parent_id"`
		Items    []childETagSnapshot `json:"items"`
	}{ParentID: parentID, Items: etagItems})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to encode child projection")
		return
	}
	digest := sha256.Sum256(etagPayload)
	etag := fmt.Sprintf(`"children-%s"`, hex.EncodeToString(digest[:16]))
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"parent_session_id": parentID,
		"revision":          revision,
		"children":          items,
	})
}
