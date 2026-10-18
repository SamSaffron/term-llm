package responses

import (
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/core/stream"
)

// Package-local names for the core contracts this family uses heavily.
type (
	Event                          = core.Event
	Part                           = core.Part
	Message                        = core.Message
	Stream                         = core.Stream
	Usage                          = core.Usage
	ToolSpec                       = core.ToolSpec
	ToolCall                       = core.ToolCall
	ToolChoice                     = core.ToolChoice
	ToolActivity                   = core.ToolActivity
	ToolDiscoveryCall              = core.ToolDiscoveryCall
	ProviderReplayItem             = core.ProviderReplayItem
	ResponsesOptions               = core.ResponsesOptions
	ProgrammaticToolCallingOptions = core.ProgrammaticToolCallingOptions
	PromptCacheOptions             = core.PromptCacheOptions
	MultiAgentOptions              = core.MultiAgentOptions
	StreamIncompleteError          = stream.StreamIncompleteError
	ResponsesIncompleteError       = stream.ResponsesIncompleteError
)

const (
	RoleUser               = core.RoleUser
	RoleDeveloper          = core.RoleDeveloper
	RoleTool               = core.RoleTool
	RoleSystem             = core.RoleSystem
	RoleAssistant          = core.RoleAssistant
	PartText               = core.PartText
	PartProviderReplay     = core.PartProviderReplay
	PartToolCall           = core.PartToolCall
	PartToolResult         = core.PartToolResult
	PartImage              = core.PartImage
	PartFile               = core.PartFile
	PartDiscoveryOutput    = core.PartDiscoveryOutput
	PartDiscoveryCall      = core.PartDiscoveryCall
	EventReasoningDelta    = core.EventReasoningDelta
	EventToolExecStart     = core.EventToolExecStart
	EventTextDelta         = core.EventTextDelta
	EventRetry             = core.EventRetry
	EventUsage             = core.EventUsage
	EventToolExecEnd       = core.EventToolExecEnd
	EventToolCall          = core.EventToolCall
	EventToolActivity      = core.EventToolActivity
	EventProviderReplay    = core.EventProviderReplay
	EventImageGenerated    = core.EventImageGenerated
	EventHeartbeat         = core.EventHeartbeat
	EventError             = core.EventError
	EventDone              = core.EventDone
	EventDiscoveryCall     = core.EventDiscoveryCall
	ToolChoiceRequired     = core.ToolChoiceRequired
	ToolChoiceNone         = core.ToolChoiceNone
	ToolChoiceName         = core.ToolChoiceName
	ToolChoiceAuto         = core.ToolChoiceAuto
	ToolActivityFailed     = core.ToolActivityFailed
	ToolActivityCompleted  = core.ToolActivityCompleted
	ReasoningKindUnknown   = core.ReasoningKindUnknown
	ReasoningKindRaw       = core.ReasoningKindRaw
	ReasoningKindEncrypted = core.ReasoningKindEncrypted
	ReasoningKindSummary   = core.ReasoningKindSummary
)
