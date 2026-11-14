package sessionactivity

import "github.com/term-llm/term-llm/internal/session"

// WebSessionMetrics contains durable counters, not request-level pricing data.
type WebSessionMetrics struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	CacheWriteTokens  int `json:"cache_write_tokens"`
	ToolCalls         int `json:"tool_calls"`
	LLMTurns          int `json:"llm_turns"`
}

type ChildRunProjection struct {
	WebSessionMetrics
	Model             string `json:"model,omitempty"`
	SessionID         string `json:"session_id"`
	ParentSessionID   string `json:"parent_session_id"`
	ParentSpawnItemID int64  `json:"parent_spawn_item_id,omitempty"`
	ParentSpawnCallID string `json:"parent_spawn_call_id,omitempty"`
	Title             string `json:"title"`
	// Kind separates a spawned subagent from an isolated skill run. Both are
	// children of the same parent, and the UI must not call one the other.
	Kind        string                `json:"kind,omitempty"`
	Agent       string                `json:"agent,omitempty"`
	TaskSummary string                `json:"task_summary,omitempty"`
	State       session.SessionStatus `json:"state"`
	Attention   bool                  `json:"attention"`
	ResponseID  string                `json:"response_id,omitempty"`
	RunEpoch    int64                 `json:"run_epoch,omitempty"`
	// Live/Steerable/RunID describe this process's view of a delegated run in
	// flight. A durable-only projection cannot distinguish "finished" from
	// "running somewhere this server cannot see".
	Live             bool   `json:"live,omitempty"`
	Steerable        bool   `json:"steerable,omitempty"`
	ChildRunID       string `json:"child_run_id,omitempty"`
	PendingAsks      int    `json:"pending_asks,omitempty"`
	Revision         int64  `json:"revision"`
	StartedAt        int64  `json:"started_at,omitempty"`
	EndedAt          int64  `json:"ended_at,omitempty"`
	ApproximateTimes bool   `json:"approximate_times,omitempty"`
	// A delegated run spends real money on its own session row. Pricing it here
	// is what lets the parent report what the work actually cost.
	CostUSD     *float64 `json:"cost_usd,omitempty"`
	CostPartial bool     `json:"cost_partial,omitempty"`
}
