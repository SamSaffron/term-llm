package llm

import (
	"context"
	"encoding/json"
)

// Provider streams model output events for a request.
type Provider interface {
	Name() string
	Credential() string // Returns credential type for debugging (e.g., "api_key", "codex", "claude-code")
	Capabilities() Capabilities
	Stream(ctx context.Context, req Request) (Stream, error)
}

// Capabilities describe optional provider features.
type Capabilities struct {
	NativeSearch bool
	ToolCalls    bool
}

// Stream yields events until io.EOF.
type Stream interface {
	Recv() (Event, error)
	Close() error
}

// Request represents a single model turn.
type Request struct {
	Model             string
	Messages          []Message
	Tools             []ToolSpec
	ToolChoice        ToolChoice
	ParallelToolCalls bool
	Search            bool
	ReasoningEffort   string
	MaxOutputTokens   int
	Temperature       float32
	TopP              float32
	Debug             bool
	DebugRaw          bool
}

// Role identifies a message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartType identifies a message content part.
type PartType string

const (
	PartText       PartType = "text"
	PartToolCall   PartType = "tool_call"
	PartToolResult PartType = "tool_result"
)

// Message holds a role with structured parts.
type Message struct {
	Role  Role
	Parts []Part
}

// Part represents a single content part.
type Part struct {
	Type       PartType
	Text       string
	ToolCall   *ToolCall
	ToolResult *ToolResult
}

// ToolSpec describes a callable tool.
type ToolSpec struct {
	Name        string
	Description string
	Schema      map[string]interface{}
}

// ToolChoiceMode controls tool selection behavior.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceName     ToolChoiceMode = "name"
)

// ToolChoice configures which tool the model should call.
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string
}

// ToolCall is a model-requested tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolResult is the output from executing a tool call.
type ToolResult struct {
	ID      string
	Name    string
	Content string
}

// EventType describes streaming events.
type EventType string

const (
	EventTextDelta EventType = "text_delta"
	EventToolCall  EventType = "tool_call"
	EventUsage     EventType = "usage"
	EventDone      EventType = "done"
	EventError     EventType = "error"
)

// Event represents a streamed output update.
type Event struct {
	Type EventType
	Text string
	Tool *ToolCall
	Use  *Usage
	Err  error
}

// Usage captures token usage if available.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// CommandSuggestion represents a single command suggestion from the LLM.
type CommandSuggestion struct {
	Command     string `json:"command"`
	Explanation string `json:"explanation"`
	Likelihood  int    `json:"likelihood"` // 1-10, how likely this matches user intent
}

// EditToolCall represents a single edit tool call (find/replace).
type EditToolCall struct {
	FilePath  string `json:"file_path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// ModelInfo represents a model available from a provider.
type ModelInfo struct {
	ID          string
	DisplayName string
	Created     int64
	OwnedBy     string
}

func SystemText(text string) Message {
	return Message{
		Role:  RoleSystem,
		Parts: []Part{{Type: PartText, Text: text}},
	}
}

func UserText(text string) Message {
	return Message{
		Role:  RoleUser,
		Parts: []Part{{Type: PartText, Text: text}},
	}
}

func AssistantText(text string) Message {
	return Message{
		Role:  RoleAssistant,
		Parts: []Part{{Type: PartText, Text: text}},
	}
}

func ToolResultMessage(id, name, content string) Message {
	return Message{
		Role: RoleTool,
		Parts: []Part{{
			Type: PartToolResult,
			ToolResult: &ToolResult{
				ID:      id,
				Name:    name,
				Content: content,
			},
		}},
	}
}
