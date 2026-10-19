package inspect

import (
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

// Options supplies provenance, session metadata, and an optional detached runtime snapshot.
// ChildSessionIDs must validate candidates against the parent session before returning IDs.
type Options struct {
	SessionID       string
	TranscriptRev   int64
	Scope           string
	Session         *session.Session
	Runtime         RuntimeOptions
	ChildSessionIDs func(name, content string) []string
}

// RuntimeOptions must be captured by the caller without warming a runtime.
type RuntimeOptions struct {
	Loaded            bool
	UnavailableReason string
	SystemPrompt      string
	Tools             []core.ToolSpec
	ToolDiscovery     *core.ToolDiscoveryDiagnostics
}

type Document struct {
	SchemaVersion int     `json:"schema_version"`
	SessionID     string  `json:"session_id"`
	TranscriptRev int64   `json:"transcript_rev"`
	Scope         string  `json:"scope"`
	Header        Header  `json:"header"`
	Blocks        []Block `json:"blocks"`
	bodies        map[string]Body
}

// ModelInfo describes one observed model configuration, without guessing missing history.
type ModelInfo struct {
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	Effort        string `json:"effort,omitempty"`
	ReasoningMode string `json:"reasoning_mode,omitempty"`
	Agent         string `json:"agent,omitempty"`
}

type ModelChange struct {
	From     ModelInfo `json:"from"`
	To       ModelInfo `json:"to"`
	Strategy string    `json:"strategy,omitempty"`
	Status   string    `json:"status,omitempty"`
}

type Handover struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

type Header struct {
	InitialModel    ModelInfo `json:"initial_model"`
	CurrentModel    ModelInfo `json:"current_model"`
	Provider        string    `json:"provider,omitempty"`
	Model           string    `json:"model,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	Agent           string    `json:"agent,omitempty"`
	CompactionSeq   int       `json:"compaction_seq"`
	CompactionCount int       `json:"compaction_count"`
	Runtime         Runtime   `json:"runtime"`
}

type Runtime struct {
	Loaded                       bool                           `json:"loaded"`
	UnavailableReason            string                         `json:"unavailable_reason,omitempty"`
	SystemPromptBlockID          string                         `json:"system_prompt_block_id,omitempty"`
	SystemPromptMatchesPersisted *bool                          `json:"system_prompt_matches_persisted,omitempty"`
	ToolDiscovery                *core.ToolDiscoveryDiagnostics `json:"tool_discovery,omitempty"`
	Tools                        []RuntimeTool                  `json:"tools,omitempty"`
}

type RuntimeTool struct {
	BlockID     string `json:"block_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	SchemaBytes int    `json:"schema_bytes"`
}

type Block struct {
	ModelChange         *ModelChange       `json:"model_change,omitempty"`
	Handover            *Handover          `json:"handover,omitempty"`
	Synthetic           bool               `json:"synthetic,omitempty"`
	ID                  string             `json:"id"`
	Kind                string             `json:"kind"`
	Role                core.Role          `json:"role,omitempty"`
	MessageID           string             `json:"message_id,omitempty"`
	Sequence            int                `json:"sequence"`
	PartIndex           int                `json:"part_index"`
	CreatedAt           int64              `json:"created_at,omitempty"`
	Title               string             `json:"title,omitempty"`
	Preview             string             `json:"preview,omitempty"`
	TotalBytes          int                `json:"total_bytes"`
	TotalLines          int                `json:"total_lines"`
	HasBody             bool               `json:"has_body"`
	CompactionTail      bool               `json:"compaction_tail,omitempty"`
	CompactionSummaryID string             `json:"compaction_summary_id,omitempty"`
	Tool                *Tool              `json:"tool,omitempty"`
	ReasoningKind       core.ReasoningKind `json:"reasoning_kind,omitempty"`
	EncryptedBytes      int                `json:"encrypted_bytes,omitempty"`
	Compaction          *Compaction        `json:"compaction,omitempty"`
	ChildSessionIDs     []string           `json:"child_session_ids,omitempty"`
}

type Tool struct {
	CallID  string `json:"call_id,omitempty"`
	Name    string `json:"name,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

type Compaction struct {
	Ordinal        int  `json:"ordinal"`
	ActiveBoundary bool `json:"active_boundary"`
	TailCount      int  `json:"tail_count"`
}

type Body struct {
	ID         string `json:"id"`
	Format     string `json:"format"`
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
	TotalBytes int    `json:"total_bytes"`
}

type BodiesResponse struct {
	TranscriptRev int64    `json:"transcript_rev"`
	Bodies        []Body   `json:"bodies"`
	Missing       []string `json:"missing"`
	Deferred      []string `json:"deferred"`
}
