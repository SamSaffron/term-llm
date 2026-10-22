package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
)

// contextKey is a private type for context keys to prevent collisions.
type contextKey string

// toolCallIDKey is the context key for the current tool call ID.
const toolCallIDKey contextKey = "tool_call_id"
const guardianReviewCollectorKey contextKey = "guardian_review_collector"

// GuardianReview is display-only audit metadata for a Guardian-reviewed tool
// invocation. Providers must continue to receive only the ordinary tool result.
type GuardianReview struct {
	DurationMS float64 `json:"duration_ms"`
	StateBytes int     `json:"state_bytes,omitempty"`
	Outcome    string  `json:"outcome"`
	Message    string  `json:"message"`
	Model      string  `json:"model,omitempty"`
	Tool       string  `json:"tool,omitempty"`
	Command    string  `json:"command,omitempty"`
	Path       string  `json:"path,omitempty"`
	IsWrite    bool    `json:"is_write,omitempty"`
	WorkDir    string  `json:"workdir,omitempty"`
}

type guardianReviewCollector struct {
	mu      sync.Mutex
	reviews []GuardianReview
}

// ContextWithGuardianReviewCapture installs a per-tool review collector and
// returns a snapshot function for the execution owner.
func ContextWithGuardianReviewCapture(ctx context.Context) (context.Context, func() []GuardianReview) {
	collector := &guardianReviewCollector{}
	return context.WithValue(ctx, guardianReviewCollectorKey, collector), collector.snapshot
}

// RecordGuardianReview attaches a correlated review to the active tool call.
func RecordGuardianReview(ctx context.Context, review GuardianReview) {
	collector, _ := ctx.Value(guardianReviewCollectorKey).(*guardianReviewCollector)
	if collector == nil {
		return
	}
	collector.mu.Lock()
	collector.reviews = append(collector.reviews, review)
	collector.mu.Unlock()
}

func (c *guardianReviewCollector) snapshot() []GuardianReview {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]GuardianReview(nil), c.reviews...)
}

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

const toolRunIDKey contextKey = "tool_run_id"

// ContextWithToolRunID binds dynamic schema activation to one engine run.
func ContextWithToolRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, toolRunIDKey, runID)
}

// ToolRunIDFromContext returns the active engine run identity.
func ToolRunIDFromContext(ctx context.Context) string {
	if v := ctx.Value(toolRunIDKey); v != nil {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}

const approvalTranscriptKey contextKey = "approval_transcript"

// TrustedUserInputToolName is the only tool whose output may be promoted to a
// trusted user turn in an approval transcript. Guardian backends treat the user
// role as authorization, so a tool that could set it would be able to author its
// own approval. internal/tools pins this against the ask_user tool name.
const TrustedUserInputToolName = "ask_user"

// ApprovalTurnForTool reports the trusted approval turn a tool result should
// contribute to a policy-review transcript. Only first-party ask_user output,
// which is produced by a human answering a prompt, may speak as the user.
func ApprovalTurnForTool(callName string, output ToolOutput) (role, text string, ok bool) {
	if strings.TrimSpace(callName) != TrustedUserInputToolName {
		return "", "", false
	}
	text = strings.TrimSpace(output.TrustedUserInput)
	if text == "" {
		return "", "", false
	}
	return string(RoleUser), text, true
}

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
	NativeWebSearch         bool // Provider has native web search capability
	NativeWebFetch          bool // Provider has native URL fetch capability
	ToolCalls               bool
	SupportsToolChoice      bool // Provider supports tool_choice to force specific tool use
	ManagesOwnContext       bool // Provider manages its own context window (skip compaction)
	InlineToolLoop          bool // Provider completes its MCP/tool loop inside one Stream invocation
	OrderedInlineToolEvents bool // Provider requires streamed text/tool/text order preserved in persisted assistant parts
}

// NativeToolDiscoverySupport describes a provider/model/transport combination
// whose client-executed discovery protocol has been explicitly verified.
type NativeToolDiscoverySupport struct {
	Supported bool
	Name      string
	Reason    string
}

// NativeToolDiscoveryProvider is implemented only by providers that can
// translate the provider-neutral discovery request and replay parts to their wire
// protocol. Support must be exact rather than inferred from general tool calling.
type NativeToolDiscoveryProvider interface {
	NativeToolDiscoverySupport(model string) NativeToolDiscoverySupport
}

// Stream yields events until io.EOF.
type Stream interface {
	Recv() (Event, error)
	Close() error
}

// ResponsesOptions controls advanced OpenAI Responses API execution features.
// A nil Request.Responses uses provider configuration; a non-nil value overlays
// its explicitly populated fields on those defaults.
type ResponsesOptions struct {
	ReasoningMode           string
	ReasoningContext        string
	MultiAgent              MultiAgentOptions
	ProgrammaticToolCalling ProgrammaticToolCallingOptions
	PromptCache             PromptCacheOptions
}

type MultiAgentOptions struct {
	Enabled                bool
	EnabledSet             bool
	MaxConcurrentSubagents int
}

type ProgrammaticToolCallingOptions struct {
	Enabled    bool
	EnabledSet bool
	Tools      []string
}

type PromptCacheOptions struct {
	Mode string
	TTL  string
}

func (o ResponsesOptions) IsZero() bool {
	return o.ReasoningMode == "" && o.ReasoningContext == "" &&
		!o.MultiAgent.Enabled && !o.MultiAgent.EnabledSet && o.MultiAgent.MaxConcurrentSubagents == 0 &&
		!o.ProgrammaticToolCalling.Enabled && !o.ProgrammaticToolCalling.EnabledSet && len(o.ProgrammaticToolCalling.Tools) == 0 &&
		o.PromptCache.Mode == "" && o.PromptCache.TTL == ""
}

// Request represents a single model turn.
type Request struct {
	Resume     *Continuation `json:"-"` // Internal same-invocation execution checkpoint.
	Model      string
	SessionID  string // Optional session ID for provider-side continuity/caching hints
	WorkingDir string // Optional working directory for local subprocess providers
	// Ephemeral marks one-shot internal requests (title generation, summaries,
	// vision helpers) that must not participate in provider-side conversation/session
	// state. Stateful providers should avoid resuming an existing session and must
	// not update their stored session boundary.
	Ephemeral bool
	// IncludeDeveloperInContinuation keeps a developer policy immediately before
	// the latest user turn when building a server-state continuation suffix.
	IncludeDeveloperInContinuation bool
	Messages                       []Message
	ApprovalTranscriptPrefix       []Message // Optional policy-review-only evidence prepended to tool approval transcripts; never sent to providers.
	Tools                          []ToolSpec
	ToolChoice                     ToolChoice
	LastTurnToolChoice             *ToolChoice // If set, force this tool choice on the last agentic turn
	// EnableToolDiscovery opts this request into an attached dynamic tool planner.
	// It is internal orchestration state and is not provider metadata.
	EnableToolDiscovery bool
	// NativeToolDiscovery asks a capable provider adapter to expose its native
	// client-executed discovery control tool. Deferred schemas remain in neutral
	// discovery output parts, never in the ordinary top-level Tools slice.
	NativeToolDiscovery *NativeToolDiscoveryRequest
	ParallelToolCalls   bool
	// AllowedToolsPresent applies an internal, request-scoped execution filter.
	// It is consumed by runtimes before calling Engine.Stream and is not provider metadata.
	// A present empty AllowedTools slice intentionally blocks every callable tool.
	AllowedTools            []string
	AllowedToolsPresent     bool
	Search                  bool
	ForceExternalSearch     bool // If true, use external search even if provider supports native
	DisableExternalWebFetch bool // If true, do not inject external read_url even when provider lacks native fetch
	ReasoningEffort         string
	Responses               *ResponsesOptions // Advanced Responses API controls; nil uses provider defaults.
	MaxOutputTokens         int
	Temperature             float32
	TemperatureSet          bool // If true, Temperature was explicitly provided, including zero
	TopP                    float32
	TopPSet                 bool              // If true, TopP was explicitly provided, including zero
	ServiceTier             string            // Optional Responses API service tier; "priority" enables ChatGPT fast mode
	ServiceTierSet          bool              // If true, ServiceTier overrides any provider-level default; empty clears it
	MaxTurns                int               // Max agentic turns for tool execution (0 = use default)
	ToolMap                 map[string]string // Maps client tool names to server tool names (e.g. "WebSearch" → "search")
	Debug                   bool
	DebugRaw                bool
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
	PartText              PartType = "text"
	PartImage             PartType = "image"
	PartFile              PartType = "file"
	PartToolCall          PartType = "tool_call"
	PartToolResult        PartType = "tool_result"
	PartToolActivity      PartType = "tool_activity"      // Persisted display-only provider-managed tool activity; never sent to providers.
	PartProviderReplay    PartType = "provider_replay"    // Hidden provider protocol state; never rendered/exported.
	PartDiscoveryCall     PartType = "discovery_call"     // Provider-neutral native discovery call, translated only by capable adapters.
	PartDiscoveryOutput   PartType = "discovery_output"   // Trusted schemas selected by the local planner for a discovery call.
	PartSkillActivation   PartType = "skill_activation"   // Persisted direct-activation provenance; never sent to providers.
	PartAgentMention      PartType = "agent_mention"      // Provider-visible delegation instruction; excluded from human-visible text surfaces.
	PartPathNote          PartType = "path_note"          // Persisted branch-context provenance; adjacent text is sent as developer context.
	PartDiffComment       PartType = "diff_comment"       // Persisted inline-diff anchor metadata; never sent to providers.
	PartGoalSteering      PartType = "goal_steering"      // Persisted marker for synthetic active-goal user prompts; adjacent text is sent to providers but hidden from human transcripts.
	PartJobNotification   PartType = "job_notification"   // Persisted marker for queued-job completion notices; adjacent text is sent to providers, the marker is not.
	PartConversationStart PartType = "conversation_start" // Persisted immutable time anchor for the conversation; marker is never sent to providers.
	PartDisplayText       PartType = "display_text"       // Persistence-only visible text; never sent to providers.
	PartPlatformContext   PartType = "platform_context"   // Persisted active interaction-mode context; marker is never sent to providers.
)

// Message holds a role with structured parts.
type Message struct {
	Role                    Role
	Parts                   []Part
	CacheAnchor             bool   // provider should apply cache_control to this message (Anthropic-specific)
	ApprovalRole            string `json:",omitempty"` // Optional role override for guardian/policy-review transcripts only.
	ApprovalText            string `json:",omitempty"` // Optional canonical Guardian evidence text; providers ignore it.
	ClientMessageID         string `json:",omitempty"` // Stable identity for first-party user intent; providers ignore it.
	DisplayText             string `json:",omitempty"` // Optional persistence/UI text when provider-only context is present.
	ResponseID              string `json:",omitempty"` // Stable response owner for persisted/UI projection identity; providers ignore it.
	AssistantSegmentOrdinal int    `json:",omitempty"` // Response-scoped assistant segment identity; -1 when not applicable.
	SegmentStartSequence    int64  `json:",omitempty"` // First response event sequence for this assistant segment, when known.
	SegmentEndSequence      int64  `json:",omitempty"` // Last response event sequence covered by this persisted segment, when known.
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
	CreatedAt                 int64          `json:",omitempty"` // Display-only first-observed Unix milliseconds for an ordered inline part.
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
	ToolActivity              *ToolActivity
	ProviderReplay            *ProviderReplayItem        // Opaque Responses output item used only for stateless continuation.
	DiscoveryCall             *ToolDiscoveryCall         // Native discovery request emitted by a capable provider.
	DiscoveryOutput           *ToolDiscoveryOutput       // Planner-selected trusted schemas paired to a discovery call.
	SkillActivation           *SkillActivationProvenance // Direct user activation metadata; persisted but not provider content.
	PathNote                  *PathNoteProvenance        // Branch-context metadata; persisted but not sent to providers.
	DiffComment               *DiffComment               // Inline-diff comment anchor; persisted but not sent to providers.
	JobNotification           *JobNotificationProvenance // Queued-job completion metadata; persisted but not sent to providers.
}

// DiffCommentContextLine preserves one exact nearby line from the diff that the
// user saw when submitting an inline instruction.
type DiffCommentContextLine struct {
	Side string `json:"side"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// DiffComment is durable, display-only metadata for an instruction anchored to
// a particular diff snapshot. The adjacent text part carries the actual
// provider-facing instruction.
type DiffComment struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id,omitempty"`
	Path     string `json:"path"`
	// Scope is the diff view captured by the anchor. Empty means last_turn for
	// comments persisted before scoped Git diffs supported inline comments.
	Scope string `json:"scope,omitempty"`
	Side  string `json:"side"`
	Line  int    `json:"line"`
	// FileChangeSeq is the retained snapshot sequence for last_turn and zero
	// for Git scopes, which do not have a session file-tracker snapshot.
	FileChangeSeq int64                    `json:"file_change_seq"`
	LineText      string                   `json:"line_text"`
	ContextBefore []DiffCommentContextLine `json:"context_before,omitempty"`
	ContextAfter  []DiffCommentContextLine `json:"context_after,omitempty"`
	Instruction   string                   `json:"instruction"`
}

// PathNoteProvenance identifies generated context carried from the suffix that
// was left behind when a conversation branch was created.
type PathNoteProvenance struct {
	SourceSessionID string   `json:"source_session_id"`
	AnchorMessageID int64    `json:"anchor_message_id,omitempty"`
	SourceMessages  int      `json:"source_messages,omitempty"`
	OmittedMessages int      `json:"omitted_messages,omitempty"`
	ReadFiles       []string `json:"read_files,omitempty"`
	ModifiedFiles   []string `json:"modified_files,omitempty"`
	Model           string   `json:"model,omitempty"`
	Focus           string   `json:"focus,omitempty"`
}

// SkillActivationProvenance records the exact direct invocation and resolved
// skill source used for a historical turn. The expanded instructions are stored
// in the containing developer message's text part, so later on-disk edits cannot
// rewrite session history.
type SkillActivationProvenance struct {
	Name                string   `json:"name"`
	Source              string   `json:"source"`
	SourcePath          string   `json:"source_path"`
	Origin              string   `json:"origin"`
	Execution           string   `json:"execution"`
	RawArguments        string   `json:"raw_arguments,omitempty"`
	Agent               string   `json:"agent,omitempty"`
	Model               string   `json:"model,omitempty"`
	AllowedTools        []string `json:"allowed_tools,omitempty"`
	AllowedToolsPresent bool     `json:"allowed_tools_present,omitempty"`
	RunID               string   `json:"run_id,omitempty"`
	ChildSessionID      string   `json:"child_session_id,omitempty"`
	Status              string   `json:"status,omitempty"`
	StartedAt           string   `json:"started_at,omitempty"`
	CompletedAt         string   `json:"completed_at,omitempty"`
	ActivatedAt         string   `json:"activated_at"`
}

const (
	ToolActivityCompleted = "completed"
	ToolActivityFailed    = "failed"
)

// ToolActivity is a display-only record of a provider-managed tool invocation.
// It is persisted with assistant history but ignored by provider request builders.
type ToolActivity struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Info      string          `json:"info,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Status    string          `json:"status"` // "completed" or "failed"
}

// CloneToolActivity copies a tool activity without sharing its arguments.
func CloneToolActivity(activity *ToolActivity) *ToolActivity {
	if activity == nil {
		return nil
	}
	cloned := *activity
	cloned.Arguments = append(json.RawMessage(nil), activity.Arguments...)
	return &cloned
}

// ProviderReplayItem preserves a completed Responses output item byte-for-byte.
// Raw is persisted for provider continuation but omitted from display and exports.
type ProviderReplayItem struct {
	Raw json.RawMessage `json:"raw"`
}

// NativeToolDiscoveryRequest is provider-neutral orchestration metadata. Search
// is the planner-owned input contract; provider adapters translate it to their
// native discovery tool only when NativeToolDiscoverySupport succeeds.
type NativeToolDiscoveryRequest struct {
	Search ToolSpec `json:"search"`
}

// ToolDiscoveryCall records a provider-native request for local catalogue search.
type ToolDiscoveryCall struct {
	ID        string          `json:"id"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolNamespaceIdentity carries an explicit provider-neutral namespace route for
// a callable tool. Name and ChildName are model-visible native identities;
// ToolSpec.Name remains the canonical flattened executable name used by the
// registry and function-only providers.
type ToolNamespaceIdentity struct {
	Name        string `json:"name"`
	ChildName   string `json:"child_name"`
	Description string `json:"description,omitempty"`
}

// DiscoveredTool is a trusted catalogue schema selected by the local planner.
// SchemaHash binds replay to the authorised catalogue generation that supplied it.
type DiscoveredTool struct {
	Spec       ToolSpec `json:"spec"`
	SchemaHash string   `json:"schema_hash"`
}

// ToolDiscoveryOutput pairs trusted schemas with one native discovery call.
type ToolDiscoveryOutput struct {
	CallID        string           `json:"call_id"`
	CatalogueHash string           `json:"catalogue_hash"`
	CatalogueGen  uint64           `json:"catalogue_generation"`
	Tools         []DiscoveredTool `json:"tools"`
}

// CloneToolDiscoveryOutput copies discovery schemas before mutation.
func CloneToolDiscoveryOutput(output *ToolDiscoveryOutput) *ToolDiscoveryOutput {
	if output == nil {
		return nil
	}
	cloned := *output
	cloned.Tools = make([]DiscoveredTool, len(output.Tools))
	for i, selected := range output.Tools {
		selected.Spec.Schema = DeepCopyMap(selected.Spec.Schema)
		selected.Spec.OutputSchema = DeepCopyMap(selected.Spec.OutputSchema)
		selected.Spec.AllowedCallers = append([]string(nil), selected.Spec.AllowedCallers...)
		if selected.Spec.Namespace != nil {
			namespace := *selected.Spec.Namespace
			selected.Spec.Namespace = &namespace
		}
		cloned.Tools[i] = selected
	}
	return &cloned
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
	// Namespace is optional provider-neutral identity metadata. Portable and
	// function-only providers continue to use Name; native namespace-capable
	// adapters may expose ChildName under the explicit namespace.
	Namespace *ToolNamespaceIdentity `json:",omitempty"`
	// Strict opts this tool into OpenAI strict function-parameter schemas.
	// Default is false to match Codex/OpenAI flagship behavior for broad MCP
	// schemas. When enabled, all object properties are required and free-form maps
	// are converted to strict-compatible key/value arrays.
	Strict         bool
	AllowedCallers []string               // Immutable caller allow-list (for example, "programmatic").
	OutputSchema   map[string]interface{} // Optional structured output schema for PTC callers.
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
	ID   string
	Name string // Canonical executable name after engine routing.
	// Namespace and ChildName preserve an explicit native provider identity.
	// They are never reconstructed by splitting Name.
	Namespace  string `json:",omitempty"`
	ChildName  string `json:",omitempty"`
	Arguments  json.RawMessage
	Caller     string `json:",omitempty"` // PTC caller provenance; copied to function_call_output.
	ToolInfo   string `json:",omitempty"` // Persisted display text for TUI/history (already formatted, e.g. "(main.go)")
	ThoughtSig []byte // Gemini 3 thought signature (must be passed back in result)
}
