// Package llm preserves the original public contracts while implementations
// move into their owning packages. New packages should import llm/core directly.
package llm

import (
	"context"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

// Deprecated: use core.Capabilities for new package boundaries.
type Capabilities = core.Capabilities

// Deprecated: use core.CommandSuggestion for new package boundaries.
type CommandSuggestion = core.CommandSuggestion

// Deprecated: use core.DiffComment for new package boundaries.
type DiffComment = core.DiffComment

// Deprecated: use core.DiffCommentContextLine for new package boundaries.
type DiffCommentContextLine = core.DiffCommentContextLine

// Deprecated: use core.DiffData for new package boundaries.
type DiffData = core.DiffData

// Deprecated: use core.DiscoveredTool for new package boundaries.
type DiscoveredTool = core.DiscoveredTool

// Deprecated: use core.EditToolCall for new package boundaries.
type EditToolCall = core.EditToolCall

// Deprecated: use core.Event for new package boundaries.
type Event = core.Event

// Deprecated: use core.EventType for new package boundaries.
type EventType = core.EventType

// Deprecated: use core.FileChange for new package boundaries.
type FileChange = core.FileChange

// Deprecated: use core.FilesystemObservationSummary for new package boundaries.
type FilesystemObservationSummary = core.FilesystemObservationSummary

// Deprecated: use core.GuardianReview for new package boundaries.
type GuardianReview = core.GuardianReview

// Deprecated: use core.MediaArtifact for new package boundaries.
type MediaArtifact = core.MediaArtifact

// Deprecated: use core.Message for new package boundaries.
type Message = core.Message

// Deprecated: use core.ModelInfo for new package boundaries.
type ModelInfo = core.ModelInfo

// Deprecated: use core.MultiAgentOptions for new package boundaries.
type MultiAgentOptions = core.MultiAgentOptions

// Deprecated: use core.NativeToolDiscoveryProvider for new package boundaries.
type NativeToolDiscoveryProvider = core.NativeToolDiscoveryProvider

// Deprecated: use core.NativeToolDiscoveryRequest for new package boundaries.
type NativeToolDiscoveryRequest = core.NativeToolDiscoveryRequest

// Deprecated: use core.NativeToolDiscoverySupport for new package boundaries.
type NativeToolDiscoverySupport = core.NativeToolDiscoverySupport

// Deprecated: use core.OutputClaimDiagnostic for new package boundaries.
type OutputClaimDiagnostic = core.OutputClaimDiagnostic

// Deprecated: use core.Part for new package boundaries.
type Part = core.Part

// Deprecated: use core.PartType for new package boundaries.
type PartType = core.PartType

// Deprecated: use core.PathNoteProvenance for new package boundaries.
type PathNoteProvenance = core.PathNoteProvenance

// Deprecated: use core.ProgrammaticToolCallingOptions for new package boundaries.
type ProgrammaticToolCallingOptions = core.ProgrammaticToolCallingOptions

// Deprecated: use core.PromptCacheOptions for new package boundaries.
type PromptCacheOptions = core.PromptCacheOptions

// Deprecated: use core.Provider for new package boundaries.
type Provider = core.Provider

// Deprecated: use core.ProviderReplayItem for new package boundaries.
type ProviderReplayItem = core.ProviderReplayItem

// Deprecated: use core.ProviderStateExporter for new package boundaries.
type ProviderStateExporter = core.ProviderStateExporter

// Deprecated: use core.ProviderStateImporter for new package boundaries.
type ProviderStateImporter = core.ProviderStateImporter

// Deprecated: use core.ReasoningKind for new package boundaries.
type ReasoningKind = core.ReasoningKind

// Deprecated: use core.Request for new package boundaries.
type Request = core.Request

// Deprecated: use core.ResponsesOptions for new package boundaries.
type ResponsesOptions = core.ResponsesOptions

// Deprecated: use core.Role for new package boundaries.
type Role = core.Role

// Deprecated: use core.SkillActivationProvenance for new package boundaries.
type SkillActivationProvenance = core.SkillActivationProvenance

// Deprecated: use core.Stream for new package boundaries.
type Stream = core.Stream

// Deprecated: use core.ToolActivity for new package boundaries.
type ToolActivity = core.ToolActivity

// Deprecated: use core.ToolCall for new package boundaries.
type ToolCall = core.ToolCall

// Deprecated: use core.ToolChoice for new package boundaries.
type ToolChoice = core.ToolChoice

// Deprecated: use core.ToolChoiceMode for new package boundaries.
type ToolChoiceMode = core.ToolChoiceMode

// Deprecated: use core.ToolContentPart for new package boundaries.
type ToolContentPart = core.ToolContentPart

// Deprecated: use core.ToolContentPartType for new package boundaries.
type ToolContentPartType = core.ToolContentPartType

// Deprecated: use core.ToolDiscoveryCall for new package boundaries.
type ToolDiscoveryCall = core.ToolDiscoveryCall

// Deprecated: use core.ToolDiscoveryOutput for new package boundaries.
type ToolDiscoveryOutput = core.ToolDiscoveryOutput

// Deprecated: use core.ToolExecutionResponse for new package boundaries.
type ToolExecutionResponse = core.ToolExecutionResponse

// Deprecated: use core.ToolFileData for new package boundaries.
type ToolFileData = core.ToolFileData

// Deprecated: use core.ToolImageData for new package boundaries.
type ToolImageData = core.ToolImageData

// Deprecated: use core.ToolNamespaceIdentity for new package boundaries.
type ToolNamespaceIdentity = core.ToolNamespaceIdentity

// Deprecated: use core.ToolOutput for new package boundaries.
type ToolOutput = core.ToolOutput

// Deprecated: use core.ToolResult for new package boundaries.
type ToolResult = core.ToolResult

// Deprecated: use core.ToolSpec for new package boundaries.
type ToolSpec = core.ToolSpec

// Deprecated: use core.Usage for new package boundaries.
type Usage = core.Usage

// Deprecated: use core.ApprovalTranscriptFromContext for new package boundaries.
func ApprovalTranscriptFromContext(ctx context.Context) []Message {
	return core.ApprovalTranscriptFromContext(ctx)
}

// Deprecated: use core.ApprovalTurnForTool for new package boundaries.
func ApprovalTurnForTool(callName string, output ToolOutput) (role, text string, ok bool) {
	return core.ApprovalTurnForTool(callName, output)
}

// Deprecated: use core.AssistantText for new package boundaries.
func AssistantText(text string) Message { return core.AssistantText(text) }

// Deprecated: use core.CallIDFromContext for new package boundaries.
func CallIDFromContext(ctx context.Context) string { return core.CallIDFromContext(ctx) }

// Deprecated: use core.ContextWithApprovalTranscript for new package boundaries.
func ContextWithApprovalTranscript(ctx context.Context, messages []Message) context.Context {
	return core.ContextWithApprovalTranscript(ctx, messages)
}

// Deprecated: use core.ContextWithCallID for new package boundaries.
func ContextWithCallID(ctx context.Context, callID string) context.Context {
	return core.ContextWithCallID(ctx, callID)
}

// Deprecated: use core.ContextWithGuardianReviewCapture for new package boundaries.
func ContextWithGuardianReviewCapture(ctx context.Context) (context.Context, func() []GuardianReview) {
	return core.ContextWithGuardianReviewCapture(ctx)
}

// Deprecated: use core.ContextWithSessionID for new package boundaries.
func ContextWithSessionID(ctx context.Context, sessionID string) context.Context {
	return core.ContextWithSessionID(ctx, sessionID)
}

// Deprecated: use core.ContextWithToolRunID for new package boundaries.
func ContextWithToolRunID(ctx context.Context, runID string) context.Context {
	return core.ContextWithToolRunID(ctx, runID)
}

// Deprecated: use core.GoalSteeringText for new package boundaries.
func GoalSteeringText(text string) Message { return core.GoalSteeringText(text) }

// Deprecated: use core.HasGoalSteeringPart for new package boundaries.
func HasGoalSteeringPart(parts []Part) bool { return core.HasGoalSteeringPart(parts) }

// Deprecated: use core.IsEncryptedReasoningDelta for new package boundaries.
func IsEncryptedReasoningDelta(event Event) bool { return core.IsEncryptedReasoningDelta(event) }

// Deprecated: use core.IsGoalSteeringMessage for new package boundaries.
func IsGoalSteeringMessage(msg Message) bool { return core.IsGoalSteeringMessage(msg) }

// Deprecated: use core.IsLegacyGoalSteeringText for new package boundaries.
func IsLegacyGoalSteeringText(text string) bool { return core.IsLegacyGoalSteeringText(text) }

// Deprecated: use core.MergeReasoningKind for new package boundaries.
func MergeReasoningKind(current, incoming ReasoningKind) ReasoningKind {
	return core.MergeReasoningKind(current, incoming)
}

// Deprecated: use core.MessageAttachmentSummary for new package boundaries.
func MessageAttachmentSummary(msg Message) string { return core.MessageAttachmentSummary(msg) }

// Deprecated: use core.MessageText for new package boundaries.
func MessageText(msg Message) string { return core.MessageText(msg) }

// Deprecated: use core.NormalizeMedia for new package boundaries.
func NormalizeMedia(media []MediaArtifact, images []string) []MediaArtifact {
	return core.NormalizeMedia(media, images)
}

// Deprecated: use core.NormalizeReasoningKind for new package boundaries.
func NormalizeReasoningKind(kind ReasoningKind) ReasoningKind {
	return core.NormalizeReasoningKind(kind)
}

// Deprecated: use core.NormalizeStoredReasoningKind for new package boundaries.
func NormalizeStoredReasoningKind(kind ReasoningKind, hasReasoningContent bool) ReasoningKind {
	return core.NormalizeStoredReasoningKind(kind, hasReasoningContent)
}

// Deprecated: use core.RecordGuardianReview for new package boundaries.
func RecordGuardianReview(ctx context.Context, review GuardianReview) {
	core.RecordGuardianReview(ctx, review)
}

// Deprecated: use core.SessionIDFromContext for new package boundaries.
func SessionIDFromContext(ctx context.Context) string { return core.SessionIDFromContext(ctx) }

// Deprecated: use core.SystemText for new package boundaries.
func SystemText(text string) Message { return core.SystemText(text) }

// Deprecated: use core.TextOutput for new package boundaries.
func TextOutput(s string) ToolOutput { return core.TextOutput(s) }

// Deprecated: use core.ToolErrorMessage for new package boundaries.
func ToolErrorMessage(id, name, errorText string, thoughtSig []byte) Message {
	return core.ToolErrorMessage(id, name, errorText, thoughtSig)
}

// Deprecated: use core.ToolResultMessage for new package boundaries.
func ToolResultMessage(id, name, content string, thoughtSig []byte) Message {
	return core.ToolResultMessage(id, name, content, thoughtSig)
}

// Deprecated: use core.ToolResultMessageFromOutput for new package boundaries.
func ToolResultMessageFromOutput(id, name string, output ToolOutput, thoughtSig []byte) Message {
	return core.ToolResultMessageFromOutput(id, name, output, thoughtSig)
}

// Deprecated: use core.ToolRunIDFromContext for new package boundaries.
func ToolRunIDFromContext(ctx context.Context) string { return core.ToolRunIDFromContext(ctx) }

// Deprecated: use core.UserImageMessage for new package boundaries.
func UserImageMessage(mediaType, base64Data, caption string) Message {
	return core.UserImageMessage(mediaType, base64Data, caption)
}

// Deprecated: use core.UserImageMessageWithPath for new package boundaries.
func UserImageMessageWithPath(mediaType, base64Data, filePath, caption string) Message {
	return core.UserImageMessageWithPath(mediaType, base64Data, filePath, caption)
}

// Deprecated: use core.UserText for new package boundaries.
func UserText(text string) Message { return core.UserText(text) }

// Deprecated: use core.DiffOperationCreate for new package boundaries.
const DiffOperationCreate = core.DiffOperationCreate

// Deprecated: use core.EventAttemptDiscard for new package boundaries.
const EventAttemptDiscard = core.EventAttemptDiscard

// Deprecated: use core.EventCompaction for new package boundaries.
const EventCompaction = core.EventCompaction

// Deprecated: use core.EventDiscoveryCall for new package boundaries.
const EventDiscoveryCall = core.EventDiscoveryCall

// Deprecated: use core.EventDiscoveryOutput for new package boundaries.
const EventDiscoveryOutput = core.EventDiscoveryOutput

// Deprecated: use core.EventDone for new package boundaries.
const EventDone = core.EventDone

// Deprecated: use core.EventError for new package boundaries.
const EventError = core.EventError

// Deprecated: use core.EventHeartbeat for new package boundaries.
const EventHeartbeat = core.EventHeartbeat

// Deprecated: use core.EventImageGenerated for new package boundaries.
const EventImageGenerated = core.EventImageGenerated

// Deprecated: use core.EventModelSwitch for new package boundaries.
const EventModelSwitch = core.EventModelSwitch

// Deprecated: use core.EventPhase for new package boundaries.
const EventPhase = core.EventPhase

// Deprecated: use core.EventProviderReplay for new package boundaries.
const EventProviderReplay = core.EventProviderReplay

// Deprecated: use core.EventReasoningDelta for new package boundaries.
const EventReasoningDelta = core.EventReasoningDelta

// Deprecated: use core.EventRetry for new package boundaries.
const EventRetry = core.EventRetry

// Deprecated: use core.EventSteering for new package boundaries.
const EventSteering = core.EventSteering

// Deprecated: use core.EventTextDelta for new package boundaries.
const EventTextDelta = core.EventTextDelta

// Deprecated: use core.EventToolActivity for new package boundaries.
const EventToolActivity = core.EventToolActivity

// Deprecated: use core.EventToolCall for new package boundaries.
const EventToolCall = core.EventToolCall

// Deprecated: use core.EventToolExecEnd for new package boundaries.
const EventToolExecEnd = core.EventToolExecEnd

// Deprecated: use core.EventToolExecStart for new package boundaries.
const EventToolExecStart = core.EventToolExecStart

// Deprecated: use core.EventUsage for new package boundaries.
const EventUsage = core.EventUsage

// Deprecated: use core.PartAgentMention for new package boundaries.
const PartAgentMention = core.PartAgentMention

// Deprecated: use core.PartConversationStart for new package boundaries.
const PartConversationStart = core.PartConversationStart

// Deprecated: use core.PartDiffComment for new package boundaries.
const PartDiffComment = core.PartDiffComment

// Deprecated: use core.PartDiscoveryCall for new package boundaries.
const PartDiscoveryCall = core.PartDiscoveryCall

// Deprecated: use core.PartDiscoveryOutput for new package boundaries.
const PartDiscoveryOutput = core.PartDiscoveryOutput

// Deprecated: use core.PartDisplayText for new package boundaries.
const PartDisplayText = core.PartDisplayText

// Deprecated: use core.PartFile for new package boundaries.
const PartFile = core.PartFile

// Deprecated: use core.PartGoalSteering for new package boundaries.
const PartGoalSteering = core.PartGoalSteering

// Deprecated: use core.PartImage for new package boundaries.
const PartImage = core.PartImage

// Deprecated: use core.PartPathNote for new package boundaries.
const PartPathNote = core.PartPathNote

// Deprecated: use core.PartPlatformContext for new package boundaries.
const PartPlatformContext = core.PartPlatformContext

// Deprecated: use core.PartProviderReplay for new package boundaries.
const PartProviderReplay = core.PartProviderReplay

// Deprecated: use core.PartSkillActivation for new package boundaries.
const PartSkillActivation = core.PartSkillActivation

// Deprecated: use core.PartText for new package boundaries.
const PartText = core.PartText

// Deprecated: use core.PartToolActivity for new package boundaries.
const PartToolActivity = core.PartToolActivity

// Deprecated: use core.PartToolCall for new package boundaries.
const PartToolCall = core.PartToolCall

// Deprecated: use core.PartToolResult for new package boundaries.
const PartToolResult = core.PartToolResult

// Deprecated: use core.ReasoningKindEncrypted for new package boundaries.
const ReasoningKindEncrypted = core.ReasoningKindEncrypted

// Deprecated: use core.ReasoningKindRaw for new package boundaries.
const ReasoningKindRaw = core.ReasoningKindRaw

// Deprecated: use core.ReasoningKindSummary for new package boundaries.
const ReasoningKindSummary = core.ReasoningKindSummary

// Deprecated: use core.ReasoningKindUnknown for new package boundaries.
const ReasoningKindUnknown = core.ReasoningKindUnknown

// Deprecated: use core.RoleAssistant for new package boundaries.
const RoleAssistant = core.RoleAssistant

// Deprecated: use core.RoleDeveloper for new package boundaries.
const RoleDeveloper = core.RoleDeveloper

// Deprecated: use core.RoleEvent for new package boundaries.
const RoleEvent = core.RoleEvent

// Deprecated: use core.RoleSystem for new package boundaries.
const RoleSystem = core.RoleSystem

// Deprecated: use core.RoleTool for new package boundaries.
const RoleTool = core.RoleTool

// Deprecated: use core.RoleUser for new package boundaries.
const RoleUser = core.RoleUser

// Deprecated: use core.ToolActivityCompleted for new package boundaries.
const ToolActivityCompleted = core.ToolActivityCompleted

// Deprecated: use core.ToolActivityFailed for new package boundaries.
const ToolActivityFailed = core.ToolActivityFailed

// Deprecated: use core.ToolChoiceAuto for new package boundaries.
const ToolChoiceAuto = core.ToolChoiceAuto

// Deprecated: use core.ToolChoiceName for new package boundaries.
const ToolChoiceName = core.ToolChoiceName

// Deprecated: use core.ToolChoiceNone for new package boundaries.
const ToolChoiceNone = core.ToolChoiceNone

// Deprecated: use core.ToolChoiceRequired for new package boundaries.
const ToolChoiceRequired = core.ToolChoiceRequired

// Deprecated: use core.ToolContentPartImageData for new package boundaries.
const ToolContentPartImageData = core.ToolContentPartImageData

// Deprecated: use core.ToolContentPartText for new package boundaries.
const ToolContentPartText = core.ToolContentPartText

// Deprecated: use core.TrustedUserInputToolName for new package boundaries.
const TrustedUserInputToolName = core.TrustedUserInputToolName

// Deprecated: use core.WarningPhasePrefix for new package boundaries.
const WarningPhasePrefix = core.WarningPhasePrefix

// Deprecated: use core.TurnMetrics for new package boundaries.
type TurnMetrics = core.TurnMetrics

// Deprecated: use core.SteeringStatus for new package boundaries.
type SteeringStatus = core.SteeringStatus

// Deprecated: use core.Continuation for new package boundaries.
type Continuation = core.Continuation

// Deprecated: use core.ModelServiceTier for new package boundaries.
type ModelServiceTier = core.ModelServiceTier

const (
	SteeringQueued    = core.SteeringQueued
	SteeringCommitted = core.SteeringCommitted
)

func cloneToolActivity(a *ToolActivity) *ToolActivity { return core.CloneToolActivity(a) }
func cloneToolDiscoveryOutput(o *ToolDiscoveryOutput) *ToolDiscoveryOutput {
	return core.CloneToolDiscoveryOutput(o)
}
