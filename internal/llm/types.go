package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// sessionIDKey is the context key for the current session ID.
const sessionIDKey contextKey = "session_id"

const approvalTranscriptKey contextKey = "approval_transcript"

// ContextWithApprovalTranscript returns a new context carrying the conversation
// messages that should be used by policy reviewers for tool approval decisions.
func ContextWithApprovalTranscript(ctx context.Context, messages []Message) context.Context {
	copied := make([]Message, len(messages))
	copy(copied, messages)
	return context.WithValue(ctx, approvalTranscriptKey, copied)
}

// ApprovalTranscriptFromContext extracts the policy-review transcript messages.
func ApprovalTranscriptFromContext(ctx context.Context) []Message {
	if v := ctx.Value(approvalTranscriptKey); v != nil {
		if msgs, ok := v.([]Message); ok {
			copied := make([]Message, len(msgs))
			copy(copied, msgs)
			return copied
		}
	}
	return nil
}

// ContextWithSessionID returns a new context with the session ID set.
// Used by the engine so tools (e.g. file-change recording) know which
// session a tool execution belongs to.
func ContextWithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey, sessionID)
}

// SessionIDFromContext extracts the session ID from context, or returns empty string.
func SessionIDFromContext(ctx context.Context) string {
	if v := ctx.Value(sessionIDKey); v != nil {
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

// ProviderStateExporter is implemented by stateful providers that can persist
// opaque conversation transport state outside the user-visible transcript.
// The returned bytes must be safe to store in the session database.
type ProviderStateExporter interface {
	ExportProviderState() ([]byte, bool)
}

// ProviderStateImporter restores state previously returned by
// ProviderStateExporter. Providers should validate and ignore unusable state by
// returning an error rather than partially applying it.
type ProviderStateImporter interface {
	ImportProviderState([]byte) error
}

// Capabilities describe optional provider features.
type Capabilities struct {
	NativeWebSearch    bool // Provider has native web search capability
	NativeWebFetch     bool // Provider has native URL fetch capability
	ToolCalls          bool
	SupportsToolChoice bool // Provider supports tool_choice to force specific tool use
	ManagesOwnContext  bool // Provider manages its own context window (skip compaction)
	InlineToolLoop     bool // Provider completes its MCP/tool loop inside one Stream invocation
}

// Stream yields events until io.EOF.
type Stream interface {
	Recv() (Event, error)
	Close() error
}

// Request represents a single model turn.
type Request struct {
	Model     string
	SessionID string // Optional session ID for provider-side continuity/caching hints
	// Ephemeral marks one-shot internal requests (title generation, summaries,
	// vision helpers) that must not participate in provider-side conversation/session
	// state. Stateful providers should avoid resuming an existing session and must
	// not update their stored session boundary.
	Ephemeral                bool
	Messages                 []Message
	ApprovalTranscriptPrefix []Message // Optional policy-review-only evidence prepended to tool approval transcripts; never sent to providers.
	Tools                    []ToolSpec
	ToolChoice               ToolChoice
	LastTurnToolChoice       *ToolChoice // If set, force this tool choice on the last agentic turn
	ParallelToolCalls        bool
	Search                   bool
	ForceExternalSearch      bool // If true, use external search even if provider supports native
	DisableExternalWebFetch  bool // If true, do not inject external read_url even when provider lacks native fetch
	ReasoningEffort          string
	MaxOutputTokens          int
	Temperature              float32
	TemperatureSet           bool // If true, Temperature was explicitly provided, including zero
	TopP                     float32
	TopPSet                  bool              // If true, TopP was explicitly provided, including zero
	ServiceTier              string            // Optional Responses API service tier; "priority" enables ChatGPT fast mode
	ServiceTierSet           bool              // If true, ServiceTier overrides any provider-level default; empty clears it
	MaxTurns                 int               // Max agentic turns for tool execution (0 = use default)
	ToolMap                  map[string]string // Maps client tool names to server tool names (e.g. "WebSearch" → "search")
	Debug                    bool
	DebugRaw                 bool
}

// Role identifies a message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	// RoleEvent is a durable UI/session timeline marker. It is never sent to
	// providers as conversation context.
	RoleEvent Role = "event"
	// RoleDeveloper is a privileged instruction role injected by the platform layer.
	// OpenAI/Responses API providers send it as a "developer" role message; Anthropic
	// providers have no native equivalent, so the text is prepended into the next user turn
	// wrapped in <developer>…</developer> tags.
	RoleDeveloper Role = "developer"
)

// PartType identifies a message content part.
type PartType string

const (
	PartText       PartType = "text"
	PartImage      PartType = "image"
	PartFile       PartType = "file"
	PartToolCall   PartType = "tool_call"
	PartToolResult PartType = "tool_result"
)

// Message holds a role with structured parts.
type Message struct {
	Role         Role
	Parts        []Part
	CacheAnchor  bool   // provider should apply cache_control to this message (Anthropic-specific)
	ApprovalRole string `json:",omitempty"` // Optional role override for guardian/policy-review transcripts only.
}

// ReasoningKind classifies provider reasoning/thinking payloads for safe display.
type ReasoningKind string

const (
	ReasoningKindUnknown   ReasoningKind = "unknown"
	ReasoningKindSummary   ReasoningKind = "summary"
	ReasoningKindRaw       ReasoningKind = "raw"
	ReasoningKindEncrypted ReasoningKind = "encrypted"
)

// NormalizeReasoningKind returns a conservative non-empty reasoning kind.
func NormalizeReasoningKind(kind ReasoningKind) ReasoningKind {
	switch ReasoningKind(strings.ToLower(strings.TrimSpace(string(kind)))) {
	case ReasoningKindSummary:
		return ReasoningKindSummary
	case ReasoningKindRaw:
		return ReasoningKindRaw
	case ReasoningKindEncrypted:
		return ReasoningKindEncrypted
	default:
		return ReasoningKindUnknown
	}
}

// NormalizeStoredReasoningKind preserves the historical meaning of persisted
// assistant parts from before ReasoningKind existed. Those parts stored only
// display-safe summaries in ReasoningContent, so an empty kind with content is
// treated as a summary when rendering/exporting stored sessions. Live provider
// events should continue using NormalizeReasoningKind.
func NormalizeStoredReasoningKind(kind ReasoningKind, hasReasoningContent bool) ReasoningKind {
	if strings.TrimSpace(string(kind)) == "" && hasReasoningContent {
		return ReasoningKindSummary
	}
	return NormalizeReasoningKind(kind)
}

// MergeReasoningKind combines accumulated and incoming reasoning classifications.
// Empty incoming kinds mean "no provider signal". Unknown is not a positive
// classification signal, encrypted is replay-only until displayable summary/raw
// text arrives, and raw wins over summary when both appear in the same block.
func MergeReasoningKind(current, incoming ReasoningKind) ReasoningKind {
	if strings.TrimSpace(string(incoming)) == "" {
		return current
	}

	incoming = NormalizeReasoningKind(incoming)
	if incoming == ReasoningKindUnknown {
		return current
	}
	if current == "" {
		return incoming
	}

	current = NormalizeReasoningKind(current)
	if current == ReasoningKindEncrypted {
		return incoming
	}
	if incoming == ReasoningKindEncrypted {
		return current
	}
	if current == incoming {
		return current
	}
	if current == ReasoningKindUnknown {
		return incoming
	}
	if current == ReasoningKindRaw || incoming == ReasoningKindRaw {
		return ReasoningKindRaw
	}
	return ReasoningKindSummary
}

// IsEncryptedReasoningDelta reports whether a reasoning delta only carries
// encrypted replay metadata and should be withheld from interactive UI streams.
func IsEncryptedReasoningDelta(event Event) bool {
	kind := NormalizeReasoningKind(event.ReasoningKind)
	return kind == ReasoningKindEncrypted || (event.ReasoningEncryptedContent != "" && event.Text == "")
}

// Part represents a single content part.
type Part struct {
	Type                      PartType
	Text                      string
	ReasoningContent          string         // Reasoning summary text or provider thinking content (classified by ReasoningKind)
	ReasoningSummaryParts     []string       // Display-safe Responses reasoning summary array elements, when provider supplies structure
	ReasoningItemID           string         // Responses API reasoning item ID for replay
	ReasoningEncryptedContent string         // Provider encrypted reasoning/signature content for replay; never displayed
	ReasoningKind             ReasoningKind  // Classification for ReasoningContent/replay metadata
	ReasoningSummaryTitle     string         // Optional parsed display title for summary reasoning
	ImageData                 *ToolImageData // User-supplied image (base64-encoded)
	ImagePath                 string         // Local filesystem path to the image (when available, e.g. Telegram uploads)
	FileData                  *ToolFileData  // User-supplied file (base64-encoded)
	FilePath                  string         // Local filesystem path to the file (when available)
	ToolCall                  *ToolCall
	ToolResult                *ToolResult
}

// ToolSpec describes a callable tool.
//
// Tool specs are treated as immutable after registration. In particular, Schema
// maps returned from registries may be shared across calls; provider code that
// needs to rewrite a schema must copy it first.
type ToolSpec struct {
	Name        string
	Description string
	Schema      map[string]interface{}
	// Strict opts this tool into OpenAI strict function-parameter schemas.
	// Default is false to match Codex/OpenAI flagship behavior for broad MCP
	// schemas. When enabled, all object properties are required and free-form maps
	// are converted to strict-compatible key/value arrays.
	Strict bool
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
	ToolInfo   string `json:",omitempty"` // Persisted display text for TUI/history (already formatted, e.g. "(main.go)")
	ThoughtSig []byte // Gemini 3 thought signature (must be passed back in result)
}

// Diff operation identifiers for structured diff rendering.
const (
	DiffOperationCreate = "create"
)

// DiffData represents structured diff information from edit/write tools.
type DiffData struct {
	File      string `json:"f"`
	Old       string `json:"o"`
	New       string `json:"n"`
	Line      int    `json:"l"`            // 1-indexed starting line number
	Operation string `json:"op,omitempty"` // Optional operation hint, e.g. "create" for new files
}

// FileChange describes one recorded file modification made by a tool.
// Emitted on EventToolExecEnd when file-change tracking is enabled.
// Contents are never carried here — only metadata; the recorded blobs
// are served on demand by the session file-changes endpoints.
type FileChange struct {
	Path      string `json:"path"`                // Absolute file path
	Kind      string `json:"kind"`                // "create" | "modify" | "delete"
	Adds      int    `json:"adds"`                // Lines added by this change (0 when truncated)
	Dels      int    `json:"dels"`                // Lines removed by this change (0 when truncated)
	Seq       int64  `json:"seq"`                 // Per-session monotonic change sequence
	Truncated bool   `json:"truncated,omitempty"` // Content not retained (size cap, budget, binary, unknown before)
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
	Detail    string `json:"detail,omitempty"`
}

// ToolFileData represents base64-encoded file data in user input.
type ToolFileData struct {
	MediaType string `json:"media_type,omitempty"`
	Base64    string `json:"base64,omitempty"`
	Filename  string `json:"filename,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
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
	FileChanges  []FileChange      `json:"file_changes,omitempty"` // Recorded file changes (when file tracking is enabled)
	TimedOut     bool              // Set by tools that support timeouts (e.g. shell); drives ToolSuccess=false without content sniffing
	IsError      bool              // Set when a tool returned an unsuccessful result (e.g. shell exit code != 0); copied to ToolResult.IsError for UI/history and provider error metadata
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
	EventHeartbeat      EventType = "heartbeat"       // Emitted while a long-running tool is still active
	EventUsage          EventType = "usage"
	EventPhase          EventType = "phase" // Emitted for high-level phase changes (Thinking, Searching, etc.)
	EventDone           EventType = "done"
	EventError          EventType = "error"
	EventRetry          EventType = "retry"           // Emitted when retrying after rate limit or transport recovery
	EventAttemptDiscard EventType = "attempt_discard" // Discard provisional assistant output from the current streamed attempt
	EventInterjection   EventType = "interjection"    // User interjected a message mid-stream
	EventModelSwitch    EventType = "model_switch"    // Request model changed at a provider-turn boundary
	EventImageGenerated EventType = "image_generated" // Emitted when a built-in image_generation tool returns an image
)

// WarningPhasePrefix is the prefix for warning-level phase events.
// Phase events starting with this prefix are rendered as visible warnings
// in both the TUI and plain text output.
const WarningPhasePrefix = "WARNING: "

// ToolExecutionResponse holds the result of a synchronous tool execution.
// Used by CLI bridge providers to receive synchronous tool results from the engine.
type ToolExecutionResponse struct {
	Result ToolOutput
	Err    error
}

// Event represents a streamed output update.
type Event struct {
	Type                      EventType
	Text                      string
	Model                     string // For EventModelSwitch: request model applied at provider-turn boundary
	ReasoningEffort           string // For EventModelSwitch: request reasoning effort applied at provider-turn boundary
	InterjectionID            string // For EventInterjection: stable ID for matching queued interjections in the UI
	InterjectionStatus        InterjectionStatus
	Message                   Message       // For EventInterjection: structured user message including attachments
	ReasoningItemID           string        // For EventReasoningDelta: reasoning item ID
	ReasoningEncryptedContent string        // For EventReasoningDelta: encrypted reasoning content
	ReasoningKind             ReasoningKind // For EventReasoningDelta: summary/raw/encrypted/unknown classification
	ReasoningSummaryParts     []string      // For EventReasoningDelta: structured display-safe summary parts, when available
	ReasoningIndex            int           // For EventReasoningDelta: provider reasoning block/index when available
	ReasoningFinal            bool          // For EventReasoningDelta: true when provider marks the reasoning block complete
	Tool                      *ToolCall
	ToolCallID                string          // For EventToolExecStart/End: unique ID of this tool invocation
	ToolName                  string          // For EventToolExecStart/End: name of tool being executed
	ToolInfo                  string          // For EventToolExecStart/End: additional info (e.g., URL being fetched)
	ToolArgs                  json.RawMessage // For EventToolExecStart: raw args JSON
	ToolSuccess               bool            // For EventToolExecEnd: whether tool execution succeeded
	ToolOutput                string          // For EventToolExecEnd: the tool's text content
	ToolDiffs                 []DiffData      // For EventToolExecEnd: structured diffs from edit tools
	ToolFileChanges           []FileChange    // For EventToolExecEnd: recorded file changes (file tracking)
	ToolImages                []string        // For EventToolExecEnd: image paths from image tools
	Use                       *Usage
	Err                       error
	// Retry fields (for EventRetry). RetryMaxAttempts == 0 means the retry
	// policy is governed by a time budget rather than a fixed attempt count.
	RetryAttempt     int
	RetryMaxAttempts int
	RetryWaitSecs    float64
	// ToolResponse is set when a provider needs synchronous bridged tool execution.
	// The engine will execute the tool and send the result back on this channel.
	ToolResponse chan<- ToolExecutionResponse
	// Image fields (for EventImageGenerated)
	ImageData     []byte // Raw decoded image bytes
	ImageMimeType string // e.g. "image/png"
	RevisedPrompt string // Model's revised prompt, if any
}

// Usage captures token usage if available.
//
// InputTokens is the count of non-cached input tokens — i.e. the portion that was
// freshly processed. CachedInputTokens is the portion served from cache. The two
// are always additive: InputTokens + CachedInputTokens = total prompt/context size.
//
// All providers must normalise to this convention:
//   - Anthropic already reports input_tokens (non-cached) + cache_read_input_tokens separately.
//   - OpenAI/ChatGPT reports prompt_tokens (inclusive of cached); providers must subtract
//     the cached portion before populating InputTokens.
type Usage struct {
	InputTokens       int // Non-cached input tokens (newly processed this turn)
	OutputTokens      int
	CachedInputTokens int // Tokens read from cache (additive with InputTokens, NOT a subset)
	CacheWriteTokens  int // Tokens written to cache (cache_creation_input_tokens)

	// ProviderRawInputTokens is the provider-reported input/prompt token count
	// before term-llm normalization. For OpenAI-family APIs this includes cached
	// tokens; for providers that already report non-cached input separately this
	// may be zero or equal to InputTokens.
	ProviderRawInputTokens int
	// ProviderTotalTokens is the provider-reported total_tokens when available.
	// For OpenAI Responses/Chat Completions this is input_tokens + output_tokens.
	ProviderTotalTokens int
	// ReasoningTokens is provider-reported reasoning output tokens when available.
	ReasoningTokens int
}

// Add accumulates another usage value into u.
func (u *Usage) Add(other Usage) {
	if u == nil {
		return
	}
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CachedInputTokens += other.CachedInputTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.ProviderRawInputTokens += other.ProviderRawInputTokens
	u.ProviderTotalTokens += other.ProviderTotalTokens
	u.ReasoningTokens += other.ReasoningTokens
}

// IsZero reports whether no token usage was reported.
func (u Usage) IsZero() bool {
	return u.InputTokens == 0 &&
		u.OutputTokens == 0 &&
		u.CachedInputTokens == 0 &&
		u.CacheWriteTokens == 0 &&
		u.ProviderRawInputTokens == 0 &&
		u.ProviderTotalTokens == 0 &&
		u.ReasoningTokens == 0
}

// BillableCountersZero reports whether the normalized token counters term-llm
// persists for usage/cost displays are all zero.
func (u Usage) BillableCountersZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedInputTokens == 0 && u.CacheWriteTokens == 0
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
	ID                   string             `json:"id"`
	DisplayName          string             `json:"display_name,omitempty"`
	Created              int64              `json:"created,omitempty"`
	OwnedBy              string             `json:"owned_by,omitempty"`
	InputLimit           int                `json:"input_limit,omitempty"` // Max input tokens (0 = unknown)
	InputPrice           float64            `json:"input_price"`           // Pricing per 1M tokens (0 = free, -1 = unknown)
	OutputPrice          float64            `json:"output_price"`          // Pricing per 1M tokens (0 = free, -1 = unknown)
	ServiceTiers         []ModelServiceTier `json:"service_tiers,omitempty"`
	AdditionalSpeedTiers []string           `json:"additional_speed_tiers,omitempty"`
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

// MessageText returns the concatenated text parts of a message.
func MessageText(msg Message) string {
	var b strings.Builder
	for _, part := range msg.Parts {
		if (part.Type == PartText || part.Type == PartFile) && part.Text != "" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// MessageAttachmentSummary returns a compact summary of non-text content.
func MessageAttachmentSummary(msg Message) string {
	images := 0
	files := 0
	for _, part := range msg.Parts {
		switch part.Type {
		case PartImage:
			images++
		case PartFile:
			files++
		}
	}
	var summaries []string
	if images == 1 {
		summaries = append(summaries, "1 image")
	} else if images > 1 {
		summaries = append(summaries, fmt.Sprintf("%d images", images))
	}
	if files == 1 {
		summaries = append(summaries, "1 file")
	} else if files > 1 {
		summaries = append(summaries, fmt.Sprintf("%d files", files))
	}
	if len(summaries) == 0 {
		return ""
	}
	return "[" + strings.Join(summaries, ", ") + " attached]"
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
				IsError:      output.IsError || output.TimedOut,
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
