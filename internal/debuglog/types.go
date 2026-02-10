package debuglog

import (
	"encoding/json"
	"time"
)

// Entry is the base type for all log entries
type Entry struct {
	Timestamp time.Time
	SessionID string
	Type      string // "request" or "event"
	Raw       json.RawMessage
}

// RequestEntry represents a logged LLM request
type RequestEntry struct {
	Timestamp time.Time
	SessionID string
	Provider  string
	Model     string
	Request   RequestData
}

// RequestData contains the request details
type RequestData struct {
	SessionID               string      `json:"session_id,omitempty"`
	Messages                []Message   `json:"messages"`
	Tools                   []Tool      `json:"tools,omitempty"`
	ToolChoice              *ToolChoice `json:"tool_choice,omitempty"`
	Search                  bool        `json:"search,omitempty"`
	ForceExternalSearch     bool        `json:"force_external_search,omitempty"`
	ParallelToolCalls       bool        `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens         int         `json:"max_output_tokens,omitempty"`
	Temperature             float32     `json:"temperature,omitempty"`
	TopP                    float32     `json:"top_p,omitempty"`
	ReasoningEffort         string      `json:"reasoning_effort,omitempty"`
	ReasoningReplayParts    int         `json:"reasoning_replay_parts,omitempty"`
	ReasoningEncryptedParts int         `json:"reasoning_encrypted_parts,omitempty"`
	MaxTurns                int         `json:"max_turns,omitempty"`
}

// ToolChoice represents tool choice settings
type ToolChoice struct {
	Mode string `json:"mode"`
	Name string `json:"name,omitempty"`
}

// Message is a simplified message for logging
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []Part
}

// Part represents a message part
type Part struct {
	Type                          string      `json:"type"`
	Text                          string      `json:"text,omitempty"`
	ReasoningContent              string      `json:"reasoning_content,omitempty"`
	ReasoningItemID               string      `json:"reasoning_item_id,omitempty"`
	ReasoningEncryptedContentLen  int         `json:"reasoning_encrypted_content_len,omitempty"`
	ReasoningEncryptedContentHash string      `json:"reasoning_encrypted_content_hash,omitempty"`
	ToolCall                      *ToolCall   `json:"tool_call,omitempty"`
	ToolResult                    *ToolResult `json:"tool_result,omitempty"`
}

// ToolCall is a simplified tool call for logging
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult is a simplified tool result for logging
type ToolResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// Tool is a simplified tool spec for logging
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// EventEntry represents a logged LLM event
type EventEntry struct {
	Timestamp time.Time
	SessionID string
	EventType string
	Data      map[string]any
}

// Session represents a debug session with metadata
type Session struct {
	ID          string
	FilePath    string
	StartTime   time.Time
	EndTime     time.Time
	Provider    string
	Model       string
	Turns       int // Number of request/response cycles
	TotalTokens TokenUsage
	HasErrors   bool
	Command     string   // CLI command that started the session
	Args        []string // CLI arguments
	Cwd         string   // Working directory
	Entries     []any    // RequestEntry or EventEntry
}

// TokenUsage tracks token consumption
type TokenUsage struct {
	Input  int
	Output int
	Cached int
}

// SessionSummary is a lightweight session info for listing
type SessionSummary struct {
	ID        string
	FilePath  string
	StartTime time.Time
	Provider  string
	Model     string
	Calls     int // Number of LLM API calls
	Input     int // Input tokens
	Output    int // Output tokens
	Cached    int // Cached input tokens
	HasErrors bool
	FileSize  int64
}
