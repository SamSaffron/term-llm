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
	NativeWebSearch    bool // Provider has native web search capability
	NativeWebFetch     bool // Provider has native URL fetch capability
	ToolCalls          bool
	SupportsToolChoice bool // Provider supports tool_choice to force specific tool use
	ManagesOwnContext  bool // Provider manages its own context window (skip compaction)
}

// Stream yields events until io.EOF.
type Stream interface {
	Recv() (Event, error)
	Close() error
}

// Request represents a single model turn.
type Request struct {
	Model               string
	SessionID           string // Optional session ID for provider-side continuity/caching hints
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
	PartImage      PartType = "image"
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
	Type                      PartType
	Text                      string
	ReasoningContent          string         // Reasoning summary text (or thinking content for OpenRouter)
	ReasoningItemID           string         // Responses API reasoning item ID for replay
	ReasoningEncryptedContent string         // Responses API encrypted reasoning content for replay
	ImageData                 *ToolImageData // User-supplied image (base64-encoded)
	ImagePath                 string         // Local filesystem path to the image (when available, e.g. Telegram uploads)
	ToolCall                  *ToolCall
	ToolResult                *ToolResult
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

// DiffData represents structured diff information from edit tools.
type DiffData struct {
	File string `json:"f"`
	Old  string `json:"o"`
	New  string `json:"n"`
	Line int    `json:"l"` // 1-indexed starting line number
}

// ToolContentPartType identifies a structured tool result content item.
type ToolContentPartType string

const (
	ToolContentPartText      ToolContentPartType = "text"
	ToolContentPartImageData ToolContentPartType = "image_data"
)

// ToolImageData represents base64-encoded image data in tool output.
type ToolImageData struct {
	MediaType string `json:"media_type,omitempty"`
	Base64    string `json:"base64,omitempty"`
}

// ToolContentPart represents one structured piece of tool result content.
// Use a sequence like [text, image_data, text] to preserve multimodal ordering.
type ToolContentPart struct {
	Type      ToolContentPartType `json:"type"`
	Text      string              `json:"text,omitempty"`
	ImageData *ToolImageData      `json:"image_data,omitempty"`
}

// ToolOutput is the structured return type from Tool.Execute().
// Most tools only populate Content. Edit/image tools also populate Diffs/Images.
type ToolOutput struct {
	Content      string            // Text result (sent to LLM)
	ContentParts []ToolContentPart `json:"content_parts,omitempty"` // Structured multimodal tool content for provider formatting
	Diffs        []DiffData        // Structured diff data (for UI rendering)
	Images       []string          // Image paths (for UI rendering)
}

// TextOutput creates a ToolOutput with only text content.
func TextOutput(s string) ToolOutput {
	return ToolOutput{Content: s}
}

// ToolResult is the output from executing a tool call.
type ToolResult struct {
	ID           string
	Name         string
	Content      string            // Clean text sent to LLM
	ContentParts []ToolContentPart `json:"content_parts,omitempty"` // Structured multimodal tool content
	Display      string            // Deprecated: old marker-based output. Kept only for deserializing pre-structured sessions. TODO: remove once no saved sessions use Display-based diff markers.
	Diffs        []DiffData        `json:"diffs,omitempty"`  // Structured diff data
	Images       []string          `json:"images,omitempty"` // Image paths
	IsError      bool              // True if this result represents a tool execution error
	ThoughtSig   []byte            // Gemini 3 thought signature (passed through from ToolCall)
}

// EventType describes streaming events.
type EventType string

const (
	EventTextDelta      EventType = "text_delta"
	EventReasoningDelta EventType = "reasoning_delta" // For thinking models (OpenRouter reasoning_content)
	EventToolCall       EventType = "tool_call"
	EventToolExecStart  EventType = "tool_exec_start" // Emitted when tool execution begins
	EventToolExecEnd    EventType = "tool_exec_end"   // Emitted when tool execution completes
	EventUsage          EventType = "usage"
	EventPhase          EventType = "phase" // Emitted for high-level phase changes (Thinking, Searching, etc.)
	EventDone           EventType = "done"
	EventError          EventType = "error"
	EventRetry          EventType = "retry"        // Emitted when retrying after rate limit
	EventInterjection   EventType = "interjection" // User interjected a message mid-stream
)

// WarningPhasePrefix is the prefix for warning-level phase events.
// Phase events starting with this prefix are rendered as visible warnings
// in both the TUI and plain text output.
const WarningPhasePrefix = "WARNING: "

// ToolExecutionResponse holds the result of a synchronous tool execution.
// Used by claude_bin provider to receive results from the engine.
type ToolExecutionResponse struct {
	Result ToolOutput
	Err    error
}

// Event represents a streamed output update.
type Event struct {
	Type                      EventType
	Text                      string
	ReasoningItemID           string // For EventReasoningDelta: reasoning item ID
	ReasoningEncryptedContent string // For EventReasoningDelta: encrypted reasoning content
	Tool                      *ToolCall
	ToolCallID                string     // For EventToolExecStart/End: unique ID of this tool invocation
	ToolName                  string     // For EventToolExecStart/End: name of tool being executed
	ToolInfo                  string     // For EventToolExecStart/End: additional info (e.g., URL being fetched)
	ToolSuccess               bool       // For EventToolExecEnd: whether tool execution succeeded
	ToolOutput                string     // For EventToolExecEnd: the tool's text content
	ToolDiffs                 []DiffData // For EventToolExecEnd: structured diffs from edit tools
	ToolImages                []string   // For EventToolExecEnd: image paths from image tools
	Use                       *Usage
	Err                       error
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
	CachedInputTokens int // Tokens read from cache (cache_read_input_tokens)
	CacheWriteTokens  int // Tokens written to cache (cache_creation_input_tokens)
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
	ID          string  `json:"id"`
	DisplayName string  `json:"display_name,omitempty"`
	Created     int64   `json:"created,omitempty"`
	OwnedBy     string  `json:"owned_by,omitempty"`
	InputLimit  int     `json:"input_limit,omitempty"` // Max input tokens (0 = unknown)
	InputPrice  float64 `json:"input_price"`           // Pricing per 1M tokens (0 = free, -1 = unknown)
	OutputPrice float64 `json:"output_price"`          // Pricing per 1M tokens (0 = free, -1 = unknown)
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

// UserImageMessage creates a user message with an image and an optional text caption.
func UserImageMessage(mediaType, base64Data, caption string) Message {
	return UserImageMessageWithPath(mediaType, base64Data, "", caption)
}

// UserImageMessageWithPath creates a user message with an image, an optional local
// file path (so tools like image_generate can reference it), and an optional caption.
func UserImageMessageWithPath(mediaType, base64Data, filePath, caption string) Message {
	parts := []Part{{
		Type:      PartImage,
		ImageData: &ToolImageData{MediaType: mediaType, Base64: base64Data},
		ImagePath: filePath,
	}}
	if caption != "" {
		parts = append(parts, Part{Type: PartText, Text: caption})
	}
	return Message{Role: RoleUser, Parts: parts}
}

func AssistantText(text string) Message {
	return Message{
		Role:  RoleAssistant,
		Parts: []Part{{Type: PartText, Text: text}},
	}
}

func ToolResultMessageFromOutput(id, name string, output ToolOutput, thoughtSig []byte) Message {
	return Message{
		Role: RoleTool,
		Parts: []Part{{
			Type: PartToolResult,
			ToolResult: &ToolResult{
				ID:           id,
				Name:         name,
				Content:      output.Content,
				ContentParts: output.ContentParts,
				Diffs:        output.Diffs,
				Images:       output.Images,
				ThoughtSig:   thoughtSig,
			},
		}},
	}
}

// ToolResultMessage creates a tool result message from a plain string.
// Convenience wrapper for callers that only have text content (no diffs/images).
func ToolResultMessage(id, name, content string, thoughtSig []byte) Message {
	return ToolResultMessageFromOutput(id, name, TextOutput(content), thoughtSig)
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
