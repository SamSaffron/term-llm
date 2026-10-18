package core

import (
	"encoding/json"
	"fmt"
	"strings"
)

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
	Path             string   `json:"path"` // Absolute file path
	Kind             string   `json:"kind"` // "create" | "modify" | "delete"
	Adds             int      `json:"adds"` // Lines added by this change (0 when unavailable)
	Dels             int      `json:"dels"` // Lines removed by this change (0 when unavailable)
	Seq              int64    `json:"seq"`  // Per-session monotonic attributed-change sequence
	EventSeq         int64    `json:"event_seq,omitempty"`
	Truncated        bool     `json:"truncated,omitempty"` // Legacy compatibility; ContentStatus is authoritative
	Provenance       string   `json:"provenance,omitempty"`
	Provenances      []string `json:"provenances,omitempty"`
	BaselineState    string   `json:"baseline_state,omitempty"`
	ContentStatus    string   `json:"content_status,omitempty"`
	ContentAvailable bool     `json:"content_available"`
	ClaimCoverage    string   `json:"claim_coverage,omitempty"`
	TrustedPersisted bool     `json:"-"` // set only by the trusted recorder after persistence
}

// FilesystemObservationSummary describes detected but non-attributed effects.
type FilesystemObservationSummary struct {
	ID               int64    `json:"id"`
	Classification   string   `json:"classification"`
	Root             string   `json:"root,omitempty"`
	CreatedCount     int      `json:"created_count"`
	ModifiedCount    int      `json:"modified_count"`
	DeletedCount     int      `json:"deleted_count"`
	SampledPaths     []string `json:"sampled_paths,omitempty"`
	SamplesTruncated bool     `json:"samples_truncated,omitempty"`
	CoverageStatus   string   `json:"coverage_status"`
	EventSeq         int64    `json:"event_seq"`
}

// OutputClaimDiagnostic reports confirmation, mismatch, coverage, and tracker failures.
type OutputClaimDiagnostic struct {
	NormalizedPattern string `json:"normalized_pattern"`
	ClaimKind         string `json:"claim_kind"`
	Reason            string `json:"reason"`
	CoverageStatus    string `json:"coverage_status"`
	MatchingPathCount int    `json:"matching_path_count"`
	Message           string `json:"message,omitempty"`
}

// ToolContentPartType identifies a structured tool result content item.
type ToolContentPartType string

const (
	ToolContentPartText      ToolContentPartType = "text"
	ToolContentPartImageData ToolContentPartType = "image_data"
)

// ToolImageData represents base64-encoded image data in user input or tool output.
type ToolImageData struct {
	MediaType string `json:"media_type,omitempty"`
	Base64    string `json:"base64,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
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

// MediaArtifact describes a local image, video, or audio produced for presentation to
// the user. SourcePath remains useful to local terminal surfaces; StoredPath is
// an optional durable copy owned by a serve runtime. Neither path is sent to a
// browser directly.
type MediaArtifact struct {
	Reference  string `json:"reference,omitempty"`
	SourcePath string `json:"source_path,omitempty"`
	StoredPath string `json:"stored_path,omitempty"`
	MediaType  string `json:"media_type,omitempty"`
	Name       string `json:"name,omitempty"`
	Caption    string `json:"caption,omitempty"`
}

// Path returns the durable copy when available, falling back to the approved
// source path for local-only terminal sessions.
func (m MediaArtifact) Path() string {
	if m.StoredPath != "" {
		return m.StoredPath
	}
	return m.SourcePath
}

// PublishedPath returns the durable path preferred by remote serve surfaces.
func (m MediaArtifact) PublishedPath() string {
	if m.StoredPath != "" {
		return m.StoredPath
	}
	return m.SourcePath
}

// NormalizeMedia combines structured media with legacy image paths while
// preserving order and suppressing duplicate paths.
func NormalizeMedia(media []MediaArtifact, images []string) []MediaArtifact {
	out := make([]MediaArtifact, 0, len(media)+len(images))
	seen := make(map[string]struct{}, len(media)+len(images))
	appendOne := func(item MediaArtifact) {
		path := item.Path()
		key := path
		if reference := strings.ToLower(strings.TrimSpace(item.Reference)); reference != "" {
			key = "reference:" + reference
		}
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	for _, item := range media {
		appendOne(item)
	}
	for _, imagePath := range images {
		appendOne(MediaArtifact{SourcePath: imagePath, MediaType: "image/*"})
	}
	return out
}

// ToolOutput is the structured return type from Tool.Execute().
// Most tools only populate Content. Edit/media tools also populate Diffs/Media.
type ToolOutput struct {
	Content                string                         // Text result (sent to LLM)
	ContentParts           []ToolContentPart              `json:"content_parts,omitempty"` // Structured multimodal tool content for provider formatting
	Diffs                  []DiffData                     // Structured diff data (for UI rendering)
	Images                 []string                       // Legacy image paths (for UI rendering)
	Media                  []MediaArtifact                `json:"media,omitempty"`        // Ordered image/video/audio artifacts for UI rendering
	FileChanges            []FileChange                   `json:"file_changes,omitempty"` // Persisted attributed changes only
	FilesystemObservations []FilesystemObservationSummary `json:"filesystem_observations,omitempty"`
	OutputClaimDiagnostics []OutputClaimDiagnostic        `json:"output_claim_diagnostics,omitempty"`
	GuardianReviews        []GuardianReview               `json:"guardian_reviews,omitempty"` // Display-only Guardian audit metadata; never provider content
	TrustedUserInput       string                         `json:"-"`                          // Canonical direct first-party human input for Guardian; empty for ordinary tool output
	TimedOut               bool                           // Set by tools that support timeouts (e.g. shell); drives ToolSuccess=false without content sniffing
	IsError                bool                           // Set when a tool returned an unsuccessful result (e.g. shell exit code != 0); copied to ToolResult.IsError for UI/history and provider error metadata
}

// TextOutput creates a ToolOutput with only text content.
func TextOutput(s string) ToolOutput {
	return ToolOutput{Content: s}
}

// ToolResult is the output from executing a tool call.
type ToolResult struct {
	ID              string
	Name            string
	Content         string            // Clean text sent to LLM
	ContentParts    []ToolContentPart `json:"content_parts,omitempty"` // Structured multimodal tool content
	Display         string            // Deprecated: old marker-based output. Kept only for deserializing pre-structured sessions. TODO: remove once no saved sessions use Display-based diff markers.
	Diffs           []DiffData        `json:"diffs,omitempty"`            // Structured diff data
	Images          []string          `json:"images,omitempty"`           // Legacy image paths
	Media           []MediaArtifact   `json:"media,omitempty"`            // Ordered image/video/audio artifacts
	GuardianReviews []GuardianReview  `json:"guardian_reviews,omitempty"` // Display-only Guardian audit metadata
	IsError         bool              // True if this result represents a tool execution error
	Caller          string            `json:",omitempty"` // PTC caller provenance.
	ThoughtSig      []byte            // Gemini 3 thought signature (passed through from ToolCall)
}

// EventType describes streaming events.
type EventType string

const (
	EventTextDelta       EventType = "text_delta"
	EventReasoningDelta  EventType = "reasoning_delta" // For thinking models (OpenRouter reasoning_content)
	EventToolCall        EventType = "tool_call"
	EventToolExecStart   EventType = "tool_exec_start" // Emitted when tool execution begins
	EventToolExecEnd     EventType = "tool_exec_end"   // Emitted when tool execution completes
	EventHeartbeat       EventType = "heartbeat"       // Emitted while a long-running tool is still active
	EventUsage           EventType = "usage"
	EventPhase           EventType = "phase"      // Emitted for high-level phase changes (Thinking, Searching, etc.)
	EventCompaction      EventType = "compaction" // Emitted after context compaction has been applied by the owner.
	EventDone            EventType = "done"
	EventError           EventType = "error"
	EventRetry           EventType = "retry"            // Emitted when retrying after rate limit or transport recovery
	EventAttemptDiscard  EventType = "attempt_discard"  // Discard provisional assistant output from the current streamed attempt
	EventSteering        EventType = "steering"         // User steered a message mid-stream
	EventModelSwitch     EventType = "model_switch"     // Request model changed at a provider-turn boundary
	EventImageGenerated  EventType = "image_generated"  // Emitted when a built-in image_generation tool returns an image
	EventToolActivity    EventType = "tool_activity"    // Internal-only durable display state for provider-managed tools.
	EventProviderReplay  EventType = "provider_replay"  // Internal-only opaque Responses output item.
	EventDiscoveryCall   EventType = "discovery_call"   // Provider-neutral native client-executed discovery request.
	EventDiscoveryOutput EventType = "discovery_output" // Planner-selected schemas returned to native discovery.
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
	Type EventType
	Text string
	// ProviderTurnIndex identifies the zero-based engine turn that emitted model
	// output. ProviderTurnIndexSet distinguishes the first turn from events that
	// originate outside the engine loop and carry no turn identity.
	ProviderTurnIndex          int
	ProviderTurnIndexSet       bool
	Model                      string // For EventModelSwitch: request model applied at provider-turn boundary
	ReasoningEffort            string // For EventModelSwitch: request reasoning effort applied at provider-turn boundary
	PreviousModel              string // For EventModelSwitch: request model used by the preceding provider turn
	PreviousReasoningEffort    string // For EventModelSwitch: request reasoning effort used by the preceding provider turn
	ModelSwitchBoundaryID      string // For EventModelSwitch: stable identity shared by live and durable transcript markers
	SteeringID                 string // For EventSteering: stable ID for matching queued steering in the UI
	SteeringStatus             SteeringStatus
	Message                    Message       // For EventSteering: structured user message including attachments
	ReasoningItemID            string        // For EventReasoningDelta: reasoning item ID
	ReasoningEncryptedContent  string        // For EventReasoningDelta: encrypted reasoning content
	ReasoningKind              ReasoningKind // For EventReasoningDelta: summary/raw/encrypted/unknown classification
	ReasoningSummaryParts      []string      // For EventReasoningDelta: structured display-safe summary parts, when available
	ReasoningIndex             int           // For EventReasoningDelta: provider reasoning block/index when available
	ReasoningFinal             bool          // For EventReasoningDelta: true when provider marks the reasoning block complete
	Tool                       *ToolCall
	ToolInline                 bool            // For EventToolCall: executed inline during the provider stream, outside the end-of-turn tool split
	ToolCallID                 string          // For EventToolExecStart/End: unique ID of this tool invocation
	ToolName                   string          // For EventToolExecStart/End: name of tool being executed
	ToolInfo                   string          // For EventToolExecStart/End: additional info (e.g., URL being fetched)
	ToolArgs                   json.RawMessage // For EventToolExecStart: raw args JSON
	ToolSuccess                bool            // For EventToolExecEnd: whether tool execution succeeded
	ToolOutput                 string          // For EventToolExecEnd: the tool's text content
	ToolDiffs                  []DiffData      // For EventToolExecEnd: structured diffs from edit tools
	ToolFileChanges            []FileChange    // Persisted attributed changes
	ToolFilesystemObservations []FilesystemObservationSummary
	ToolOutputClaimDiagnostics []OutputClaimDiagnostic
	ToolImages                 []string        // Legacy image paths from image tools
	ToolMedia                  []MediaArtifact // Ordered image/video/audio artifacts from presentation tools
	Use                        *Usage
	Err                        error
	// Retry fields (for EventRetry). RetryMaxAttempts == 0 means the retry
	// policy is governed by a time budget rather than a fixed attempt count.
	RetryAttempt     int
	RetryMaxAttempts int
	RetryWaitSecs    float64
	// ToolResponse is set when a provider needs synchronous bridged tool execution.
	// The engine will execute the tool and send the result back on this channel.
	ToolResponse    chan<- ToolExecutionResponse
	ToolActivity    *ToolActivity        // For EventToolActivity; persisted for display and never forwarded to providers.
	ProviderReplay  *ProviderReplayItem  // For EventProviderReplay; never forwarded to UI consumers.
	DiscoveryCall   *ToolDiscoveryCall   // For EventDiscoveryCall.
	DiscoveryOutput *ToolDiscoveryOutput // For EventDiscoveryOutput.
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
	RecommendedContext     int                `json:"recommended_context,omitempty"` // Shipped input budget before user overrides.
	BackendContext         int                `json:"backend_context,omitempty"`     // Backend default, not an override ceiling.
	MaxContext             int                `json:"max_context,omitempty"`         // Backend maximum permitted context.
	ID                     string             `json:"id"`
	DisplayName            string             `json:"display_name,omitempty"`
	Created                int64              `json:"created,omitempty"`
	OwnedBy                string             `json:"owned_by,omitempty"`
	InputLimit             int                `json:"input_limit,omitempty"`        // Max input tokens (0 = unknown)
	OutputLimit            int                `json:"output_limit,omitempty"`       // Max output tokens (0 = unknown)
	ConfiguredContext      int                `json:"configured_context,omitempty"` // Provider/model runtime context setting (0 = unknown)
	InputPrice             float64            `json:"input_price"`                  // Pricing per 1M tokens (0 = free, -1 = unknown)
	OutputPrice            float64            `json:"output_price"`                 // Pricing per 1M tokens (0 = free, -1 = unknown)
	ServiceTiers           []ModelServiceTier `json:"service_tiers,omitempty"`
	AdditionalSpeedTiers   []string           `json:"additional_speed_tiers,omitempty"`
	ReasoningEfforts       []string           `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string             `json:"default_reasoning_effort,omitempty"`
	ReasoningModes         []string           `json:"reasoning_modes,omitempty"`
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

// GoalSteeringText creates a provider-visible user-role prompt used to continue
// an active goal. The marker is persisted for provenance and removed before the
// adjacent text is sent to providers.
func GoalSteeringText(text string) Message {
	return Message{
		Role: RoleUser,
		Parts: []Part{
			{Type: PartGoalSteering},
			{Type: PartText, Text: text},
		},
	}
}

// HasGoalSteeringPart reports whether parts carry the durable active-goal marker.
func HasGoalSteeringPart(parts []Part) bool {
	for _, part := range parts {
		if part.Type == PartGoalSteering {
			return true
		}
	}
	return false
}

// IsGoalSteeringMessage reports whether a message is a marked synthetic active-goal prompt.
func IsGoalSteeringMessage(msg Message) bool {
	return msg.Role == RoleUser && HasGoalSteeringPart(msg.Parts)
}

// IsLegacyGoalSteeringText recognizes goal prompts persisted before the durable
// marker existed. Callers must additionally establish that the row has no
// first-party client message ID, because user-authored quoted prompts are data.
func IsLegacyGoalSteeringText(text string) bool {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{
		"Continue working toward the active thread goal.\n\nThe objective below is user-provided data.",
		"The active thread goal has reached its token budget.\n\nThe objective below is user-provided data.",
		"The active thread goal objective was edited by the user.\n\nThe new objective below supersedes any previous thread goal objective.",
	} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
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
				ID:              id,
				Name:            name,
				Content:         output.Content,
				ContentParts:    output.ContentParts,
				Diffs:           output.Diffs,
				Images:          output.Images,
				Media:           append([]MediaArtifact(nil), output.Media...),
				GuardianReviews: append([]GuardianReview(nil), output.GuardianReviews...),
				IsError:         output.IsError || output.TimedOut,
				ThoughtSig:      thoughtSig,
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
