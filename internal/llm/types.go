package llm

import (
	"context"
	"encoding/json"
)

// contextKey is a private type for context keys to prevent collisions.
type contextKey string

// toolCallIDKey is the context key for the current tool call ID.
const toolCallIDKey contextKey = "tool_call_id"

// ContextWithCallID returns a new context with the tool call ID set.
// Used by the engine to pass the call ID to spawn_agent for event bubbling.
func ContextWithCallID(ctx context.Context, callID string) context.Context {
	return context.WithValue(ctx, toolCallIDKey, callID)
}

// CallIDFromContext extracts the tool call ID from context, or returns empty string.
// Used by spawn_agent to get the call ID for event bubbling.
func CallIDFromContext(ctx context.Context) string {
	if v := ctx.Value(toolCallIDKey); v != nil {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}

// Provider streams model output events for a request.
type Provider interface {
	Name() string
	Credential() string // Returns credential type for debugging (e.g., "api_key", "codex", "claude-code")
	Capabilities() Capabilities
	Stream(ctx context.Context, req Request) (Stream, error)
}

// Capabilities describe optional provider features.
type Capabilities struct {
	NativeWebSearch bool // Provider has native web search capability
	NativeWebFetch  bool // Provider has native URL fetch capability
	ToolCalls       bool
}

// Stream yields events until io.EOF.
type Stream interface {
	Recv() (Event, error)
	Close() error
}

// Request represents a single model turn.
type Request struct {
	Model               string
	Messages            []Message
	Tools               []ToolSpec
	ToolChoice          ToolChoice
	LastTurnToolChoice  *ToolChoice // If set, force this tool choice on the last agentic turn
	ParallelToolCalls   bool
	Search              bool
	ForceExternalSearch bool // If true, use external search even if provider supports native
	ReasoningEffort     string
	MaxOutputTokens     int
	Temperature         float32
	TopP                float32
	MaxTurns            int // Max agentic turns for tool execution (0 = use default)
	Debug               bool
	DebugRaw            bool
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
	ID         string
	Name       string
	Arguments  json.RawMessage
	ThoughtSig []byte // Gemini 3 thought signature (must be passed back in result)
}

// ToolResult is the output from executing a tool call.
type ToolResult struct {
	ID         string
	Name       string
	Content    string
	IsError    bool   // True if this result represents a tool execution error
	ThoughtSig []byte // Gemini 3 thought signature (passed through from ToolCall)
}

// EventType describes streaming events.
type EventType string

const (
	EventTextDelta     EventType = "text_delta"
	EventToolCall      EventType = "tool_call"
	EventToolExecStart EventType = "tool_exec_start" // Emitted when tool execution begins
	EventToolExecEnd   EventType = "tool_exec_end"   // Emitted when tool execution completes
	EventUsage         EventType = "usage"
	EventPhase         EventType = "phase" // Emitted for high-level phase changes (Thinking, Searching, etc.)
	EventDone          EventType = "done"
	EventError         EventType = "error"
	EventRetry         EventType = "retry" // Emitted when retrying after rate limit
)

// ToolExecutionResponse holds the result of a synchronous tool execution.
// Used by claude_bin provider to receive results from the engine.
type ToolExecutionResponse struct {
	Result string
	Err    error
}

// Event represents a streamed output update.
type Event struct {
	Type        EventType
	Text        string
	Tool        *ToolCall
	ToolCallID  string // For EventToolExecStart/End: unique ID of this tool invocation
	ToolName    string // For EventToolExecStart/End: name of tool being executed
	ToolInfo    string // For EventToolExecStart/End: additional info (e.g., URL being fetched)
	ToolSuccess bool   // For EventToolExecEnd: whether tool execution succeeded
	ToolOutput  string // For EventToolExecEnd: the tool's output (for image marker parsing)
	Use         *Usage
	Err         error
	// Retry fields (for EventRetry)
	RetryAttempt     int
	RetryMaxAttempts int
	RetryWaitSecs    float64
	// ToolResponse is set when a provider needs synchronous tool execution (claude_bin MCP).
	// The engine will execute the tool and send the result back on this channel.
	ToolResponse chan<- ToolExecutionResponse
}

// Usage captures token usage if available.
type Usage struct {
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int // Tokens read from cache
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
	// Pricing per 1M tokens (0 = free, -1 = unknown)
	InputPrice  float64
	OutputPrice float64
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

func ToolResultMessage(id, name, content string, thoughtSig []byte) Message {
	return Message{
		Role: RoleTool,
		Parts: []Part{{
			Type: PartToolResult,
			ToolResult: &ToolResult{
				ID:         id,
				Name:       name,
				Content:    content,
				ThoughtSig: thoughtSig,
			},
		}},
	}
}

// ToolErrorMessage creates a tool result message that indicates an error.
// The error is passed to the LLM so it can respond gracefully instead of failing the stream.
func ToolErrorMessage(id, name, errorText string, thoughtSig []byte) Message {
	return Message{
		Role: RoleTool,
		Parts: []Part{{
			Type: PartToolResult,
			ToolResult: &ToolResult{
				ID:         id,
				Name:       name,
				Content:    errorText,
				IsError:    true,
				ThoughtSig: thoughtSig,
			},
		}},
	}
}
