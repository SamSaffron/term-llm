package session

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ModelUsageKind classifies why a model was billed inside a session.
type ModelUsageKind string

const (
	ModelUsageMain         ModelUsageKind = "main"
	ModelUsageGuardian     ModelUsageKind = "guardian"
	ModelUsageCompaction   ModelUsageKind = "compaction"
	ModelUsageSideQuestion ModelUsageKind = "side_question"
	ModelUsageHandover     ModelUsageKind = "handover"
	ModelUsageSubagent     ModelUsageKind = "subagent"
)

// UnknownUsageModel labels rows recorded before the billing model was resolved.
// Attribution is still useful without it: the kind separates delegated and
// helper spend from the session's own turns.
const UnknownUsageModel = "unknown"

// ModelUsage is durable per-model, per-kind token attribution for one session.
//
// The counters on the sessions row (input_tokens and friends) are a single
// undifferentiated bucket: main-model turns, guardian reviews, compaction,
// side questions and delegated subagent work all accumulate into the same
// columns through UpdateMetrics. Recorded history therefore cannot say which
// model spent what, and a session's own model is invisible in any per-model
// breakdown. These rows carry the attribution that bucket loses.
type ModelUsage struct {
	Model             string         `json:"model"`
	Kind              ModelUsageKind `json:"kind"`
	InputTokens       int            `json:"input_tokens"`
	OutputTokens      int            `json:"output_tokens"`
	CachedInputTokens int            `json:"cached_input_tokens"`
	CacheWriteTokens  int            `json:"cache_write_tokens"`
	LLMTurns          int            `json:"llm_turns"`
	ToolCalls         int            `json:"tool_calls"`
	// LLMMS and ToolMS are the model and tool time this attribution spent.
	// Runtime timing dies with the runtime, so a resumed session can only report
	// how long its work took if the durable rows carry it.
	LLMMS  int64 `json:"llm_ms"`
	ToolMS int64 `json:"tool_ms"`
}

// BillableCountersZero reports whether the entry carries no token counters.
// Turn and tool counts alone never create a row: they would attribute work to a
// model without any evidence of what it cost.
func (u ModelUsage) BillableCountersZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedInputTokens == 0 && u.CacheWriteTokens == 0
}

// RecordsNoWork reports whether the entry is worth a row at all. Elapsed time is
// evidence in its own right: a turn that spent minutes on tool calls without
// reporting tokens still did work, and dropping it would both understate the
// session and strand that slice of the clock.
func (u ModelUsage) RecordsNoWork() bool {
	return u.BillableCountersZero() && u.LLMMS <= 0 && u.ToolMS <= 0
}

// normalized clamps negative counters and supplies the fallback model and kind.
func (u ModelUsage) normalized() ModelUsage {
	u.Model = strings.TrimSpace(u.Model)
	if u.Model == "" {
		u.Model = UnknownUsageModel
	}
	// Trim the kind as well as the model: an untrimmed kind matches no consumer
	// and would sit beside the canonical row, double counting the same spend.
	u.Kind = ModelUsageKind(strings.TrimSpace(string(u.Kind)))
	if u.Kind == "" {
		u.Kind = ModelUsageMain
	}
	u.InputTokens = max(0, u.InputTokens)
	u.OutputTokens = max(0, u.OutputTokens)
	u.CachedInputTokens = max(0, u.CachedInputTokens)
	u.CacheWriteTokens = max(0, u.CacheWriteTokens)
	u.LLMTurns = max(0, u.LLMTurns)
	u.ToolCalls = max(0, u.ToolCalls)
	u.LLMMS = max(0, u.LLMMS)
	u.ToolMS = max(0, u.ToolMS)
	return u
}

// modelUsageSchemaV58 is applied as the v58 migration, as part of the canonical
// schema for fresh databases, and again by the v59 migration for databases that
// reached 58 without it. It carries the v59 timing columns too, so the v59
// migration only has to ALTER a table that predates them.
const modelUsageSchemaV58 = `
CREATE TABLE IF NOT EXISTS session_model_usage (
 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
 model TEXT NOT NULL,
 kind TEXT NOT NULL,
 input_tokens INTEGER NOT NULL DEFAULT 0,
 output_tokens INTEGER NOT NULL DEFAULT 0,
 cached_input_tokens INTEGER NOT NULL DEFAULT 0,
 cache_write_tokens INTEGER NOT NULL DEFAULT 0,
 llm_turns INTEGER NOT NULL DEFAULT 0,
 tool_calls INTEGER NOT NULL DEFAULT 0,
 llm_ms INTEGER NOT NULL DEFAULT 0,
 tool_ms INTEGER NOT NULL DEFAULT 0,
 updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
 PRIMARY KEY(session_id, model, kind)
);
`

// RecordModelUsage accumulates one model's share of a session's spend.
// It is additive and idempotent per call, never a replacement, so concurrent
// helper and subagent callbacks cannot clobber each other.
func (s *SQLiteStore) RecordModelUsage(ctx context.Context, sessionID string, usage ModelUsage) error {
	sessionID = strings.TrimSpace(sessionID)
	// Clamp first: an entry whose only counters are negative carries no work,
	// and would otherwise insert a row of zeros.
	usage = usage.normalized()
	if sessionID == "" || usage.RecordsNoWork() {
		return nil
	}
	return retryOnBusy(ctx, 5, func() error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO session_model_usage (
			       session_id, model, kind,
			       input_tokens, output_tokens, cached_input_tokens, cache_write_tokens,
			       llm_turns, tool_calls, llm_ms, tool_ms, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(session_id, model, kind) DO UPDATE SET
			       input_tokens = input_tokens + excluded.input_tokens,
			       output_tokens = output_tokens + excluded.output_tokens,
			       cached_input_tokens = cached_input_tokens + excluded.cached_input_tokens,
			       cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens,
			       llm_turns = llm_turns + excluded.llm_turns,
			       tool_calls = tool_calls + excluded.tool_calls,
			       llm_ms = llm_ms + excluded.llm_ms,
			       tool_ms = tool_ms + excluded.tool_ms,
			       updated_at = excluded.updated_at`,
			sessionID, usage.Model, string(usage.Kind),
			usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens, usage.CacheWriteTokens,
			usage.LLMTurns, usage.ToolCalls, usage.LLMMS, usage.ToolMS, time.Now())
		if err != nil {
			return fmt.Errorf("record model usage: %w", err)
		}
		return nil
	})
}

// ListModelUsage returns the recorded per-model attribution for a session,
// ordered by total tokens descending so the dominant model reads first.
func (s *SQLiteStore) ListModelUsage(ctx context.Context, sessionID string) ([]ModelUsage, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	// The primary key's leading column serves the session lookup, so no extra
	// index is carried for it.
	rows, err := s.db.QueryContext(ctx, `
		SELECT model, kind, input_tokens, output_tokens, cached_input_tokens,
		       cache_write_tokens, llm_turns, tool_calls, llm_ms, tool_ms
		FROM session_model_usage
		WHERE session_id = ?
		ORDER BY (input_tokens + output_tokens + cached_input_tokens + cache_write_tokens) DESC,
		         model ASC, kind ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list model usage: %w", err)
	}
	defer rows.Close()

	var out []ModelUsage
	for rows.Next() {
		var entry ModelUsage
		var kind string
		if err := rows.Scan(&entry.Model, &kind, &entry.InputTokens, &entry.OutputTokens,
			&entry.CachedInputTokens, &entry.CacheWriteTokens, &entry.LLMTurns, &entry.ToolCalls,
			&entry.LLMMS, &entry.ToolMS); err != nil {
			return nil, fmt.Errorf("scan model usage: %w", err)
		}
		entry.Kind = ModelUsageKind(kind)
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read model usage: %w", err)
	}
	return out, nil
}
