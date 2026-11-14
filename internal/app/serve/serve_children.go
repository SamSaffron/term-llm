package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/term-llm/term-llm/internal/app/serve/sessionactivity"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/tools"
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

type webSessionMetrics = sessionactivity.WebSessionMetrics
type childRunProjection = sessionactivity.ChildRunProjection

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

// delegatedChildKind reads the one durable marker that separates the two child
// kinds. buildChildExecutionRequest names a skill run "/skill @agent: …" and a
// spawned subagent "@agent: …"; Session.IsSubagent is set in memory but has no
// column, so it cannot be used here.
func delegatedChildKind(name string) string {
	if strings.HasPrefix(strings.TrimSpace(name), "/") {
		return "skill_run"
	}
	return "subagent"
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
	projection, err := s.projectSessionChildren(r.Context(), parentID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to list child sessions")
		return
	}
	w.Header().Set("ETag", projection.ETag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == projection.ETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"parent_session_id":  parentID,
		"revision":           projection.Revision,
		"children":           projection.Items,
		"limit":              maxChildRunProjection,
		"children_truncated": projection.Truncated,
	})
}

type childProjection struct {
	Items     []childRunProjection
	Revision  int64
	ETag      string
	Truncated bool
}

// projectSessionChildren is shared by direct-child links and conversation activity.
func (s *serveServer) projectSessionChildren(ctx context.Context, parentID string) (childProjection, error) {
	spawnProvenance := childSpawnProvenanceForParent(ctx, s.store, parentID)
	children, err := s.store.List(ctx, session.ListOptions{
		ParentID:       parentID,
		Limit:          maxChildRunProjection + 1,
		SortByActivity: true,
	})
	if err != nil {
		return childProjection{}, fmt.Errorf("list child sessions: %w", err)
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
			WebSessionMetrics: webSessionMetrics{InputTokens: child.InputTokens, OutputTokens: child.OutputTokens, CachedInputTokens: child.CachedInputTokens, CacheWriteTokens: child.CacheWriteTokens, ToolCalls: child.ToolCalls, LLMTurns: child.LLMTurns},
			Model:             child.Model,
			SessionID:         child.ID,
			ParentSessionID:   parentID,
			Title:             child.PreferredShortTitle(),
			Kind:              delegatedChildKind(child.Name),
			Agent:             child.Agent,
			State:             child.Status,
			Attention:         child.Status == session.StatusError,
			Revision:          safeChildRevision(child.UpdatedAt.UnixMilli()),
			StartedAt:         child.CreatedAt.UnixMilli(),
			ApproximateTimes:  true,
		}
		item.CostUSD, item.CostPartial = aggregateCost(child.Model, item.WebSessionMetrics)
		terminal := child.Status == session.StatusComplete || child.Status == session.StatusError || child.Status == session.StatusInterrupted
		if spawn, ok := spawnProvenance[child.ID]; ok {
			item.ParentSpawnItemID = spawn.ItemID
			item.ParentSpawnCallID = spawn.CallID
		}
		if terminal {
			item.EndedAt = child.UpdatedAt.UnixMilli()
			item.Revision = terminalChildRevision(item.Revision)
		}
		if messages, messagesErr := s.store.GetMessages(ctx, child.ID, 1, 0); messagesErr == nil && len(messages) > 0 {
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
		// The registry supplies spawn provenance and questions routed to the
		// parent; execution state comes from the ordinary response run.
		if handle := s.ensureChildRuns().lookup(child.ID); handle != nil {
			// The parent has no tool result while a spawn is in flight. Use the
			// admitted call identity so its card can already open this child.
			if item.ParentSpawnCallID == "" {
				item.ParentSpawnCallID = handle.callID
			}
			live := handle.state()
			item.Live = live.Live
			item.Steerable = live.Steerable
			item.ChildRunID = live.RunID
			item.PendingAsks = live.PendingAsks
			if live.StartedAt > 0 {
				item.StartedAt = live.StartedAt
				item.ApproximateTimes = false
			}
			if live.PendingAsks > 0 {
				item.Attention = true
			}
			if live.Live {
				terminal = false
				item.EndedAt = 0
			}
			item.Revision = safeChildRevision(item.Revision, live.StartedAt, live.EndedAt)
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
		ParentID  string              `json:"parent_id"`
		Items     []childETagSnapshot `json:"items"`
		Truncated bool                `json:"truncated"`
	}{ParentID: parentID, Items: etagItems, Truncated: len(children) > maxChildRunProjection})
	if err != nil {
		return childProjection{}, fmt.Errorf("encode child projection: %w", err)
	}
	digest := sha256.Sum256(etagPayload)
	etag := fmt.Sprintf(`"children-%s"`, hex.EncodeToString(digest[:16]))
	return childProjection{Items: items, Revision: revision, ETag: etag, Truncated: len(children) > maxChildRunProjection}, nil
}
