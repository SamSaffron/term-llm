package llm

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/gateway/protocol"
)

// gatewayRequest is deliberately distinct from Request. In particular it has
// no WorkingDir, approval transcript/metadata, execution filters/budgets/maps,
// debug fields, local paths, or display-only tool metadata. This makes local
// filesystem/process state structurally impossible on the wire.
type gatewayRequest struct {
	Model                          string            `json:"model"`
	SessionID                      string            `json:"session_id,omitempty"`
	Ephemeral                      bool              `json:"ephemeral,omitempty"`
	IncludeDeveloperInContinuation bool              `json:"include_developer_in_continuation,omitempty"`
	Messages                       []Message         `json:"messages"`
	Tools                          []ToolSpec        `json:"tools,omitempty"`
	ToolChoice                     ToolChoice        `json:"tool_choice"`
	LastTurnToolChoice             *ToolChoice       `json:"last_turn_tool_choice,omitempty"`
	ParallelToolCalls              bool              `json:"parallel_tool_calls,omitempty"`
	Search                         bool              `json:"search,omitempty"`
	ForceExternalSearch            bool              `json:"force_external_search,omitempty"`
	DisableExternalWebFetch        bool              `json:"disable_external_web_fetch,omitempty"`
	ReasoningEffort                string            `json:"reasoning_effort,omitempty"`
	Responses                      *ResponsesOptions `json:"responses,omitempty"`
	MaxOutputTokens                int               `json:"max_output_tokens,omitempty"`
	Temperature                    float32           `json:"temperature,omitempty"`
	TemperatureSet                 bool              `json:"temperature_set,omitempty"`
	TopP                           float32           `json:"top_p,omitempty"`
	TopPSet                        bool              `json:"top_p_set,omitempty"`
	ServiceTier                    string            `json:"service_tier,omitempty"`
	ServiceTierSet                 bool              `json:"service_tier_set,omitempty"`
}

func sanitizeGatewayMessages(messages []Message) []Message {
	out := make([]Message, len(messages))
	for i, message := range messages {
		out[i] = message
		// Approval and persisted/UI projection metadata are satellite-local. CacheAnchor
		// remains because it changes provider prompt-cache semantics.
		out[i].ApprovalRole = ""
		out[i].ClientMessageID = ""
		out[i].ResponseID = ""
		out[i].AssistantSegmentOrdinal = 0
		out[i].SegmentStartSequence = 0
		out[i].SegmentEndSequence = 0
		out[i].Parts = make([]Part, 0, len(message.Parts))
		for _, part := range message.Parts {
			if part.Type == PartSkillActivation || part.Type == PartToolActivity {
				continue
			}
			part.ImagePath = ""
			part.FilePath = ""
			if part.ToolCall != nil {
				call := *part.ToolCall
				call.ToolInfo = ""
				part.ToolCall = &call
			}
			if part.ToolResult != nil {
				result := *part.ToolResult
				result.Display = ""
				result.Diffs = nil
				result.Images = nil
				part.ToolResult = &result
			}
			out[i].Parts = append(out[i].Parts, part)
		}
	}
	return out
}

// EncodeGatewayRequest serializes provider-neutral request data while omitting
// satellite-only fields by construction.
func EncodeGatewayRequest(req Request) (json.RawMessage, error) {
	wire := gatewayRequest{
		Model: req.Model, SessionID: req.SessionID, Ephemeral: req.Ephemeral,
		IncludeDeveloperInContinuation: req.IncludeDeveloperInContinuation,
		Messages:                       sanitizeGatewayMessages(req.Messages), Tools: req.Tools,
		ToolChoice: req.ToolChoice, LastTurnToolChoice: req.LastTurnToolChoice,
		ParallelToolCalls: req.ParallelToolCalls, Search: req.Search,
		ForceExternalSearch: req.ForceExternalSearch, DisableExternalWebFetch: req.DisableExternalWebFetch,
		ReasoningEffort: req.ReasoningEffort, Responses: req.Responses,
		MaxOutputTokens: req.MaxOutputTokens, Temperature: req.Temperature,
		TemperatureSet: req.TemperatureSet, TopP: req.TopP, TopPSet: req.TopPSet,
		ServiceTier: req.ServiceTier, ServiceTierSet: req.ServiceTierSet,
	}
	data, err := json.Marshal(wire)
	return data, err
}

// DecodeGatewayRequest strictly decodes the current top-level request schema.
// Nested provider-neutral structs retain Go's forward-compatible unknown-field
// behavior so additive message metadata can cross mixed patch versions.
func DecodeGatewayRequest(data []byte) (Request, error) {
	var wire gatewayRequest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return Request{}, fmt.Errorf("decode gateway request: %w", err)
	}
	return Request{
		Model: wire.Model, SessionID: wire.SessionID, Ephemeral: wire.Ephemeral,
		IncludeDeveloperInContinuation: wire.IncludeDeveloperInContinuation,
		Messages:                       wire.Messages, Tools: wire.Tools, ToolChoice: wire.ToolChoice,
		LastTurnToolChoice: wire.LastTurnToolChoice, ParallelToolCalls: wire.ParallelToolCalls,
		Search: wire.Search, ForceExternalSearch: wire.ForceExternalSearch,
		DisableExternalWebFetch: wire.DisableExternalWebFetch, ReasoningEffort: wire.ReasoningEffort,
		Responses: wire.Responses, MaxOutputTokens: wire.MaxOutputTokens,
		Temperature: wire.Temperature, TemperatureSet: wire.TemperatureSet,
		TopP: wire.TopP, TopPSet: wire.TopPSet, ServiceTier: wire.ServiceTier,
		ServiceTierSet: wire.ServiceTierSet,
	}, nil
}

type gatewayEvent struct {
	Type                      EventType           `json:"type"`
	Text                      string              `json:"text,omitempty"`
	Model                     string              `json:"model,omitempty"`
	ReasoningEffort           string              `json:"reasoning_effort,omitempty"`
	InterjectionID            string              `json:"interjection_id,omitempty"`
	InterjectionStatus        InterjectionStatus  `json:"interjection_status,omitempty"`
	Message                   *Message            `json:"message,omitempty"`
	ReasoningItemID           string              `json:"reasoning_item_id,omitempty"`
	ReasoningEncryptedContent string              `json:"reasoning_encrypted_content,omitempty"`
	ReasoningKind             ReasoningKind       `json:"reasoning_kind,omitempty"`
	ReasoningSummaryParts     []string            `json:"reasoning_summary_parts,omitempty"`
	ReasoningIndex            int                 `json:"reasoning_index,omitempty"`
	ReasoningFinal            bool                `json:"reasoning_final,omitempty"`
	Tool                      *ToolCall           `json:"tool,omitempty"`
	ToolCallID                string              `json:"tool_call_id,omitempty"`
	ToolName                  string              `json:"tool_name,omitempty"`
	ToolInfo                  string              `json:"tool_info,omitempty"`
	ToolArgs                  json.RawMessage     `json:"tool_args,omitempty"`
	ToolSuccess               bool                `json:"tool_success,omitempty"`
	ToolOutput                string              `json:"tool_output,omitempty"`
	ToolDiffs                 []DiffData          `json:"tool_diffs,omitempty"`
	ToolFileChanges           []FileChange        `json:"tool_file_changes,omitempty"`
	ToolImages                []string            `json:"tool_images,omitempty"`
	Use                       *Usage              `json:"usage,omitempty"`
	Error                     *protocol.Error     `json:"error,omitempty"`
	RetryAttempt              int                 `json:"retry_attempt,omitempty"`
	RetryMaxAttempts          int                 `json:"retry_max_attempts,omitempty"`
	RetryWaitSecs             float64             `json:"retry_wait_secs,omitempty"`
	ToolActivity              *ToolActivity       `json:"tool_activity,omitempty"`
	ProviderReplay            *ProviderReplayItem `json:"provider_replay,omitempty"`
	ImageData                 []byte              `json:"image_data,omitempty"`
	ImageMimeType             string              `json:"image_mime_type,omitempty"`
	RevisedPrompt             string              `json:"revised_prompt,omitempty"`
}

// EncodeGatewayEvent serializes every provider-neutral event field. Callback
// channels and concrete error values never cross the boundary.
func EncodeGatewayEvent(event Event, publicError *protocol.Error) (json.RawMessage, error) {
	var message *Message
	if event.Message.Role != "" || len(event.Message.Parts) > 0 || event.Message.CacheAnchor || event.Message.ApprovalRole != "" || event.Message.ClientMessageID != "" || event.Message.ResponseID != "" || event.Message.AssistantSegmentOrdinal != 0 || event.Message.SegmentStartSequence != 0 || event.Message.SegmentEndSequence != 0 {
		copy := event.Message
		message = &copy
	}
	wire := gatewayEvent{
		Type: event.Type, Text: event.Text, Model: event.Model, ReasoningEffort: event.ReasoningEffort,
		InterjectionID: event.InterjectionID, InterjectionStatus: event.InterjectionStatus, Message: message,
		ReasoningItemID: event.ReasoningItemID, ReasoningEncryptedContent: event.ReasoningEncryptedContent,
		ReasoningKind: event.ReasoningKind, ReasoningSummaryParts: event.ReasoningSummaryParts,
		ReasoningIndex: event.ReasoningIndex, ReasoningFinal: event.ReasoningFinal,
		Tool: event.Tool, ToolCallID: event.ToolCallID, ToolName: event.ToolName, ToolInfo: event.ToolInfo,
		ToolArgs: event.ToolArgs, ToolSuccess: event.ToolSuccess, ToolOutput: event.ToolOutput,
		ToolDiffs: event.ToolDiffs, ToolFileChanges: event.ToolFileChanges, ToolImages: event.ToolImages,
		Use: event.Use, Error: publicError, RetryAttempt: event.RetryAttempt,
		RetryMaxAttempts: event.RetryMaxAttempts, RetryWaitSecs: event.RetryWaitSecs,
		ToolActivity: event.ToolActivity, ProviderReplay: event.ProviderReplay,
		ImageData: event.ImageData, ImageMimeType: event.ImageMimeType, RevisedPrompt: event.RevisedPrompt,
	}
	return json.Marshal(wire)
}

func DecodeGatewayEvent(data []byte) (Event, error) {
	var wire gatewayEvent
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return Event{}, fmt.Errorf("decode gateway event: %w", err)
	}
	event := Event{
		Type: wire.Type, Text: wire.Text, Model: wire.Model, ReasoningEffort: wire.ReasoningEffort,
		InterjectionID: wire.InterjectionID, InterjectionStatus: wire.InterjectionStatus,
		ReasoningItemID: wire.ReasoningItemID, ReasoningEncryptedContent: wire.ReasoningEncryptedContent,
		ReasoningKind: wire.ReasoningKind, ReasoningSummaryParts: wire.ReasoningSummaryParts,
		ReasoningIndex: wire.ReasoningIndex, ReasoningFinal: wire.ReasoningFinal,
		Tool: wire.Tool, ToolCallID: wire.ToolCallID, ToolName: wire.ToolName, ToolInfo: wire.ToolInfo,
		ToolArgs: wire.ToolArgs, ToolSuccess: wire.ToolSuccess, ToolOutput: wire.ToolOutput,
		ToolDiffs: wire.ToolDiffs, ToolFileChanges: wire.ToolFileChanges, ToolImages: wire.ToolImages,
		Use: wire.Use, RetryAttempt: wire.RetryAttempt, RetryMaxAttempts: wire.RetryMaxAttempts,
		RetryWaitSecs: wire.RetryWaitSecs, ToolActivity: wire.ToolActivity,
		ProviderReplay: wire.ProviderReplay, ImageData: wire.ImageData,
		ImageMimeType: wire.ImageMimeType, RevisedPrompt: wire.RevisedPrompt,
	}
	if wire.Message != nil {
		event.Message = *wire.Message
	}
	if wire.Error != nil {
		event.Err = &GatewayError{Code: wire.Error.Code, Message: wire.Error.Message, RequestID: wire.Error.RequestID}
	}
	return event, nil
}

// GatewayError is a safe, structured gateway failure. Its message is prepared
// by the gateway and never contains raw upstream bodies, secrets, or paths.
type GatewayError struct {
	Code      string
	Message   string
	RequestID string
}

func (e *GatewayError) Error() string {
	if e == nil {
		return "gateway request failed"
	}
	if e.Code == "" {
		return "gateway: " + e.Message
	}
	return fmt.Sprintf("gateway %s: %s", e.Code, e.Message)
}

func EncodeGatewayToolResponse(response ToolExecutionResponse) (json.RawMessage, error) {
	response.Result.Diffs = nil
	response.Result.Images = nil
	response.Result.FileChanges = nil
	payload := struct {
		Result ToolOutput `json:"result"`
		Error  string     `json:"error,omitempty"`
	}{Result: response.Result}
	if response.Err != nil {
		payload.Error = response.Err.Error()
	}
	return json.Marshal(payload)
}

func DecodeGatewayToolResponse(data []byte) (ToolExecutionResponse, error) {
	var payload struct {
		Result ToolOutput `json:"result"`
		Error  string     `json:"error,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return ToolExecutionResponse{}, fmt.Errorf("decode gateway tool response: %w", err)
	}
	response := ToolExecutionResponse{Result: payload.Result}
	if payload.Error != "" {
		response.Err = fmt.Errorf("%s", payload.Error)
	}
	return response, nil
}

func CapabilitiesToGatewayProtocol(c Capabilities) protocol.Capabilities {
	return protocol.Capabilities{
		NativeWebSearch: c.NativeWebSearch, NativeWebFetch: c.NativeWebFetch,
		ToolCalls: c.ToolCalls, SupportsToolChoice: c.SupportsToolChoice,
		ManagesOwnContext: c.ManagesOwnContext, InlineToolLoop: c.InlineToolLoop,
		OrderedInlineToolEvents: c.OrderedInlineToolEvents,
	}
}

func capabilitiesFromProtocol(c protocol.Capabilities) Capabilities {
	return Capabilities{
		NativeWebSearch: c.NativeWebSearch, NativeWebFetch: c.NativeWebFetch,
		ToolCalls: c.ToolCalls, SupportsToolChoice: c.SupportsToolChoice,
		ManagesOwnContext: c.ManagesOwnContext, InlineToolLoop: c.InlineToolLoop,
		OrderedInlineToolEvents: c.OrderedInlineToolEvents,
	}
}
