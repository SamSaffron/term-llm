package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

const maxChildRunProjection = 100

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
	sort.SliceStable(children, func(i, j int) bool { return children[i].UpdatedAt.After(children[j].UpdatedAt) })
	items := make([]childRunProjection, 0, min(len(children), maxChildRunProjection))
	for _, child := range children {
		item := childRunProjection{
			SessionID:        child.ID,
			ParentSessionID:  parentID,
			Title:            child.PreferredShortTitle(),
			Agent:            child.Agent,
			State:            child.Status,
			Attention:        child.Status == session.StatusError,
			Revision:         child.UpdatedAt.UnixMicro(),
			StartedAt:        child.CreatedAt.UnixMilli(),
			ApproximateTimes: true,
		}
		if spawn, ok := spawnProvenance[child.ID]; ok {
			item.ParentSpawnItemID = spawn.ItemID
			item.ParentSpawnCallID = spawn.CallID
		}
		if child.Status == session.StatusComplete || child.Status == session.StatusError {
			item.EndedAt = child.UpdatedAt.UnixMilli()
		}
		if messages, messagesErr := s.store.GetMessages(r.Context(), child.ID, 1, 0); messagesErr == nil && len(messages) > 0 {
			item.TaskSummary = strings.TrimSpace(messages[0].TextContent)
			if runes := []rune(item.TaskSummary); len(runes) > 240 {
				item.TaskSummary = string(runes[:240]) + "…"
			}
		}
		if run := s.responseRuns.latestRun(child.ID); run != nil {
			run.mu.Lock()
			item.ResponseID = run.id
			item.RunEpoch = run.runEpoch
			if run.created > 0 {
				item.StartedAt = run.created * 1000
				item.Revision = max(item.Revision, run.created*1_000_000)
				item.ApproximateTimes = false
			}
			if run.endedAt > 0 {
				item.EndedAt = run.endedAt
				item.Revision = max(item.Revision, run.endedAt*1000)
			}
			item.Revision += run.runEpoch*1_000_000 + run.lastSequenceNumber
			run.mu.Unlock()
		}
		if s.sessionMgr != nil {
			if runtime, ok := s.sessionMgr.Get(child.ID); ok && runtime != nil {
				for _, prompt := range runtime.pendingApprovalPrompts() {
					item.Attention = true
					item.Revision = max(item.Revision, prompt.CreatedAt*1000)
				}
				for _, prompt := range runtime.pendingAskUserPrompts() {
					item.Attention = true
					item.Revision = max(item.Revision, prompt.CreatedAt*1000)
				}
			}
		}
		items = append(items, item)
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
	etag := fmt.Sprintf(`"children-%s-%d"`, parentID, revision)
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
