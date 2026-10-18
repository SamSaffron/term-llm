package llm

import (
	"context"

	"github.com/samsaffron/term-llm/internal/llm/engine"
)

// CompactionAckText is the internal assistant acknowledgement inserted after a
// compaction summary. See engine.CompactionAckText.
// Deprecated: use engine.CompactionAckText in new packages.
const CompactionAckText = engine.CompactionAckText

type (
	// CompactionConfig controls when and how context compaction occurs.
	// Deprecated: use engine.CompactionConfig in new packages.
	CompactionConfig = engine.CompactionConfig
	// CompactionResult describes what happened during compaction.
	// Deprecated: use engine.CompactionResult in new packages.
	CompactionResult = engine.CompactionResult
	// HandoverResult describes the outcome of a handover compression.
	// Deprecated: use engine.HandoverResult in new packages.
	HandoverResult = engine.HandoverResult
	// HandoverOptions controls optional handover behaviour.
	// Deprecated: use engine.HandoverOptions in new packages.
	HandoverOptions = engine.HandoverOptions
)

// compactionEnv adapts root-owned model limits, provider isolation, and
// conversation anchors to the engine package's compaction implementation.
var compactionEnv = engine.CompactionEnv{
	InputLimitForModel:      InputLimitForModel,
	ClampOutputTokens:       ClampOutputTokens,
	IsolateProvider:         isolatedConversationProvider,
	ForkProvider:            forkConversationProvider,
	ConversationStartFrom:   ConversationStartFrom,
	InsertConversationStart: InsertConversationStart,
	PlatformContextFrom:     PlatformContextFrom,
	InsertPlatformContext:   InsertPlatformContext,
}

const (
	defaultThresholdRatio          = engine.DefaultThresholdRatio
	compactionPrompt               = engine.CompactionPrompt
	summaryPrefix                  = engine.SummaryPrefix
	contextContinuationBriefPrompt = engine.ContextContinuationBriefPrompt
	handoverTriggerPrompt          = engine.HandoverTriggerPrompt
)

type preparedCompactionContext = engine.PreparedCompactionContext

func prepareCompactionContext(messages []Message, config CompactionConfig, skipBrief string) preparedCompactionContext {
	return compactionEnv.PrepareCompactionContext(messages, config, skipBrief)
}

func compactionResultFromBriefPrepared(systemPrompt, brief string, prepared preparedCompactionContext, originalCount int, config CompactionConfig) *CompactionResult {
	return compactionEnv.CompactionResultFromBriefPrepared(systemPrompt, brief, prepared, originalCount, config)
}

func effectiveCompactionThresholdRatios(config *CompactionConfig) (soft, hard float64) {
	return engine.EffectiveCompactionThresholdRatios(config)
}

func continuationBriefFromAssistantMessage(msg Message) string {
	return engine.ContinuationBriefFromAssistantMessage(msg)
}

func isContextOverflowError(err error) bool { return engine.IsContextOverflowError(err) }

// DefaultCompactionConfig returns a CompactionConfig with sensible defaults.
// Deprecated: use engine.DefaultCompactionConfig in new packages.
func DefaultCompactionConfig() CompactionConfig { return engine.DefaultCompactionConfig() }

// EstimateTokens approximates the token count of text.
// Deprecated: use engine.EstimateTokens in new packages.
func EstimateTokens(text string) int { return engine.EstimateTokens(text) }

// EstimateMessageTokens approximates the token count of msgs.
// Deprecated: use engine.EstimateMessageTokens in new packages.
func EstimateMessageTokens(msgs []Message) int { return engine.EstimateMessageTokens(msgs) }

// BuildCompactionStaticInfo returns a deterministic, bounded <PREVIOUS_TURNS> block.
// Deprecated: use engine.BuildCompactionStaticInfo in new packages.
func BuildCompactionStaticInfo(messages []Message, inputLimit int) string {
	return engine.BuildCompactionStaticInfo(messages, inputLimit)
}

// CompactionResultFromBrief builds a normal CompactionResult from a brief.
// Deprecated: use engine.CompactionResultFromBrief in new packages.
func CompactionResultFromBrief(systemPrompt, brief string, messages []Message, config CompactionConfig) *CompactionResult {
	return compactionEnv.CompactionResultFromBrief(systemPrompt, brief, messages, config)
}

// SoftCompact performs the one LLM call needed for manual soft compaction.
// Deprecated: use engine.SoftCompact in new packages.
func SoftCompact(ctx context.Context, provider Provider, model, systemPrompt string, messages []Message, config CompactionConfig) (*CompactionResult, error) {
	return compactionEnv.SoftCompact(ctx, provider, model, systemPrompt, messages, config)
}

// Compact generates a structured continuation brief for the older prefix.
// Deprecated: use engine.Compact in new packages.
func Compact(ctx context.Context, provider Provider, model, systemPrompt string, messages []Message, config CompactionConfig) (*CompactionResult, error) {
	return compactionEnv.Compact(ctx, provider, model, systemPrompt, messages, config)
}

// Handover produces a structured briefing for a different agent.
// Deprecated: use engine.Handover in new packages.
func Handover(ctx context.Context, provider Provider, model, currentSystemPrompt, newSystemPrompt string, messages []Message, sourceAgent, targetAgent string, config CompactionConfig, options HandoverOptions) (*HandoverResult, error) {
	return compactionEnv.Handover(ctx, provider, model, currentSystemPrompt, newSystemPrompt, messages, sourceAgent, targetAgent, config, options)
}

// HandoverFromFile creates a HandoverResult from an existing handover document.
// Deprecated: use engine.HandoverFromFile in new packages.
func HandoverFromFile(content, newSystemPrompt, sourceAgent, targetAgent string) *HandoverResult {
	return engine.HandoverFromFile(content, newSystemPrompt, sourceAgent, targetAgent)
}

// ReconstructHandoverHistory builds the new agent's history from a handover document.
// Deprecated: use engine.ReconstructHandoverHistory in new packages.
func ReconstructHandoverHistory(systemPrompt, document, sourceAgent, targetAgent string) []Message {
	return engine.ReconstructHandoverHistory(systemPrompt, document, sourceAgent, targetAgent)
}

// ParseHandoverDocument separates a reconstructed briefing from its provenance.
// Deprecated: use engine.ParseHandoverDocument in new packages.
func ParseHandoverDocument(text string) (source, target, body string, ok bool) {
	return engine.ParseHandoverDocument(text)
}

// IsHandoverAcknowledgement recognizes the synthetic handover acknowledgement.
// Deprecated: use engine.IsHandoverAcknowledgement in new packages.
func IsHandoverAcknowledgement(text string) bool { return engine.IsHandoverAcknowledgement(text) }

// TruncateToolResult truncates tool-result text to maxChars.
// Deprecated: use engine.TruncateToolResult in new packages.
func TruncateToolResult(content string, maxChars int) string {
	return engine.TruncateToolResult(content, maxChars)
}

// IsInternalCompactionSummaryText reports whether text is a compaction summary.
// Deprecated: use engine.IsInternalCompactionSummaryText in new packages.
func IsInternalCompactionSummaryText(text string) bool {
	return engine.IsInternalCompactionSummaryText(text)
}

// CompactionSummaryDisplayText returns the user-facing text of a compaction summary.
// Deprecated: use engine.CompactionSummaryDisplayText in new packages.
func CompactionSummaryDisplayText(text string) string {
	return engine.CompactionSummaryDisplayText(text)
}
