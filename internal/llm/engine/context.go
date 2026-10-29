package engine

import (
	"context"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// SetCompaction enables context compaction with the given input token limit
// and configuration. Only enable for models with known input limits.
// Must be called before Stream() or between streams (not during).
func (e *Engine) SetCompaction(inputLimit int, config CompactionConfig) {
	e.callbackMu.Lock()
	e.inputLimit = inputLimit
	e.compactionConfig = &config
	e.callbackMu.Unlock()
}

// SetContextTracking enables token tracking without enabling compaction.
// Use this to track context fullness when auto_compact is disabled.
// Must be called before Stream() or between streams (not during).
func (e *Engine) SetContextTracking(inputLimit int) {
	e.callbackMu.Lock()
	e.inputLimit = inputLimit
	e.compactionConfig = nil
	e.callbackMu.Unlock()
}

// ConfigureContextManagement enables compaction or context tracking based on
// the provider/model's input limit and the autoCompact setting.
// Providers that manage their own context, or models without a known limit,
// clear engine-side tracking/compaction to avoid leaking stale settings.
// Both inputLimit and compactionConfig are set atomically under a single lock.
func (e *Engine) ConfigureContextManagement(provider core.Provider, providerName, modelName string, autoCompact bool) {
	limit := 0
	var compactionConfig *CompactionConfig

	if provider != nil && !provider.Capabilities().ManagesOwnContext {
		limits := env.ModelLimits
		runtimeProvider := env.unwrapProvider(provider)
		// Runtime-aware providers must win over canonical model-prefix tables.
		// Ollama's GGUF metadata can advertise a much larger architectural
		// context than the live Modelfile allocates.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		runtimeLimit, ok, err := limits.ResolveRuntimeInputLimit(ctx, runtimeProvider, providerName, modelName)
		cancel()
		switch {
		case ok && err == nil:
			limit = runtimeLimit
		default:
			if ok {
				runtimeoutput.Debug("failed to inspect provider runtime context limit", "provider", providerName, "model", modelName, "error", err)
			}
			limit = limits.ResolveInputLimit(providerName, modelName)
			if limit == 0 {
				limits.RefreshDynamicLimits(provider, providerName, modelName)
				limit = limits.ResolveInputLimit(providerName, modelName)
			}
		}
		if limit > 0 && autoCompact {
			cfg := DefaultCompactionConfig()
			compactionConfig = &cfg
		}
	}

	e.callbackMu.Lock()
	e.inputLimit = limit
	e.compactionConfig = compactionConfig
	// A provider-reported context reading only describes the provider/model
	// that produced it; drop it when the runtime moves to another one.
	if key := providerName + "\x00" + modelName; key != e.reportedContextKey {
		e.reportedContextKey = key
		e.reportedContext = core.ContextUsage{}
	}
	e.callbackMu.Unlock()
}

// InputLimit returns the configured input token limit (0 if unknown).
func (e *Engine) InputLimit() int {
	e.callbackMu.RLock()
	v := e.inputLimit
	e.callbackMu.RUnlock()
	return v
}

// CompactionThresholds returns the configured soft and hard compaction token
// thresholds. The final value is false when compaction is disabled or the
// input limit is unknown.
func (e *Engine) CompactionThresholds() (soft, hard int, enabled bool) {
	e.callbackMu.RLock()
	inputLimit := e.inputLimit
	var config *CompactionConfig
	if e.compactionConfig != nil {
		copy := *e.compactionConfig
		config = &copy
	}
	e.callbackMu.RUnlock()

	if config == nil || inputLimit <= 0 {
		return 0, 0, false
	}
	softRatio, hardRatio := EffectiveCompactionThresholdRatios(config)
	return int(float64(inputLimit) * softRatio), int(float64(inputLimit) * hardRatio), true
}

// LastTotalTokens returns the total tokens (cached+input+output) from the most
// recent API response, approximating current context fullness.
func (e *Engine) LastTotalTokens() int {
	e.callbackMu.RLock()
	v := e.lastTotalTokens
	e.callbackMu.RUnlock()
	return v
}

// ContextUsage returns the latest known context occupancy for display.
//
// Providers that manage their own context report it via EventContextUsage;
// that reading wins because the engine neither tracks nor compacts their
// context. Otherwise it is the engine-tracked usage baseline and input limit.
// The result never feeds compaction.
func (e *Engine) ContextUsage() core.ContextUsage {
	e.callbackMu.RLock()
	defer e.callbackMu.RUnlock()
	if e.reportedContext.UsedTokens > 0 {
		return e.reportedContext
	}
	return core.ContextUsage{UsedTokens: e.lastTotalTokens, Window: e.inputLimit}
}

// observeReportedContext stores the reading carried by an EventContextUsage
// and ignores every other event. The provider owns window retention: a zero
// window means unknown (e.g. Claude Code switched models mid-run), so it must
// not inherit the previous model's window here.
func (e *Engine) observeReportedContext(event core.Event) {
	reported := event.Context
	if event.Type != core.EventContextUsage || reported == nil || reported.UsedTokens <= 0 {
		return
	}
	e.callbackMu.Lock()
	e.reportedContext = *reported
	e.callbackMu.Unlock()
}

// recordUsageBaseline updates the engine-tracked context baseline from a
// provider usage report when context tracking is enabled. Cache writes are
// part of the prompt (providers report them separately from InputTokens), and
// the output becomes assistant-message input on the next request.
func (e *Engine) recordUsageBaseline(inputLimit int, use *core.Usage, messageCount int, producedOutput bool) {
	if inputLimit <= 0 || use == nil {
		return
	}
	// Retain a compatibility count that includes the in-flight assistant row
	// when this usage event carries provider output.
	if use.OutputTokens > 0 || producedOutput {
		messageCount++
	}
	e.callbackMu.Lock()
	e.lastTotalTokens = use.InputTokens + use.CachedInputTokens + use.CacheWriteTokens + use.OutputTokens
	e.lastMessageCount = messageCount
	e.callbackMu.Unlock()
}

// hasProvisionalOutput reports whether this attempt has produced assistant
// output that will become a transcript row.
func (t *providerTurnFrame) hasProvisionalOutput() bool {
	return t.textBuilder.Len() > 0 || t.reasoningBuilder.Len() > 0 || len(t.toolCalls) > 0 ||
		len(t.syncToolCalls) > 0 || t.reasoningItemID != "" || t.reasoningEncryptedContent != ""
}

// ContextEstimateBaseline returns the persisted context-estimate baseline: the
// last observed exact total tokens plus a legacy message count retained for
// compatibility with existing session metadata.
//
// For providers that manage their own context the engine never tracks usage,
// so the latest provider-reported occupancy is the baseline. Persisting it lets
// a reloaded session show its last known size before the next turn.
func (e *Engine) ContextEstimateBaseline() (int, int) {
	e.callbackMu.RLock()
	defer e.callbackMu.RUnlock()
	if e.reportedContext.UsedTokens > 0 {
		return e.reportedContext.UsedTokens, 0
	}
	return e.lastTotalTokens, e.lastMessageCount
}

// SetContextEstimateBaseline seeds the context-estimate baseline, typically
// from persisted session state on resume. The message count is legacy metadata;
// EstimateTokens recomputes the delta boundary from the transcript shape.
func (e *Engine) SetContextEstimateBaseline(lastTotalTokens, lastMessageCount int) {
	if lastTotalTokens < 0 {
		lastTotalTokens = 0
	}
	if lastMessageCount < 0 || lastTotalTokens == 0 {
		lastMessageCount = 0
	}
	e.callbackMu.Lock()
	e.lastTotalTokens = lastTotalTokens
	e.lastMessageCount = lastMessageCount
	e.callbackMu.Unlock()
}

// EstimateTokens returns the estimated input token count for the next API call
// based on the current message list. When a provider usage baseline is available,
// it treats that exact total as covering the transcript through the last assistant
// message and adds only the structural delta appended after that assistant turn.
func (e *Engine) EstimateTokens(messages []core.Message) int {
	e.callbackMu.RLock()
	lastTotalTokens := e.lastTotalTokens
	e.callbackMu.RUnlock()

	if lastTotalTokens <= 0 {
		return EstimateMessageTokens(messages)
	}

	afterLastAssistant := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == core.RoleAssistant {
			afterLastAssistant = i + 1
			break
		}
	}
	if afterLastAssistant < 0 {
		// A persisted baseline is only valid when it can be anchored to an
		// assistant turn in the current transcript. Summary-only / cleared
		// contexts should not inherit a stale pre-compaction baseline.
		return EstimateMessageTokens(messages)
	}

	return lastTotalTokens + EstimateMessageTokens(messages[afterLastAssistant:])
}

// SetCompactionCallback sets the callback for context compaction events.
// Thread-safe: can be called while streaming is in progress.
func (e *Engine) SetCompactionCallback(cb CompactionCallback) {
	e.callbackMu.Lock()
	e.onCompaction = cb
	e.callbackMu.Unlock()
}

// SetMaxToolOutputChars sets the global maximum characters for tool output.
// Tool results exceeding this limit are truncated with head+tail preservation.
// Pass 0 to disable global truncation.
func (e *Engine) SetMaxToolOutputChars(n int) {
	e.callbackMu.Lock()
	e.maxToolOutputChars = n
	e.callbackMu.Unlock()
}

// QueueRequestModelSwitch requests a same-provider model change for the next
// provider turn in an active agentic loop. This is intended for reasoning-effort
// suffix changes while tools are running: the Engine cannot be replaced safely
// mid-stream, but req.Model can be updated before the next provider.Stream call.
func (e *Engine) QueueRequestModelSwitch(model string) {
	e.QueueRequestRuntimeSwitch(model, "")
}

// QueueRequestRuntimeSwitch requests a same-provider model/effort change for
// the next provider turn in an active agentic loop.
func (e *Engine) QueueRequestRuntimeSwitch(model, reasoningEffort string) {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	e.pendingRequestRuntime = pendingRequestRuntimeSwitch{
		model:           strings.TrimSpace(model),
		reasoningEffort: strings.TrimSpace(reasoningEffort),
	}
}

// QueueRequestServiceTier changes the service tier at the next provider request
// boundary without modifying an in-flight request. An empty tier explicitly
// clears the provider default. Safe to call while the engine is streaming.
func (e *Engine) QueueRequestServiceTier(tier string) {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	e.pendingServiceTier = &tier
}

// ClearPendingRequestServiceTier discards an override left by a completed run.
// Callers starting a new run should put their current preference in Request.
func (e *Engine) ClearPendingRequestServiceTier() {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	e.pendingServiceTier = nil
}

func (e *Engine) applyPendingServiceTier(req *core.Request) {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.pendingServiceTier != nil {
		req.ServiceTier = *e.pendingServiceTier
		req.ServiceTierSet = true
		e.pendingServiceTier = nil
	}
}

// ClearPendingRequestModelSwitch cancels any queued same-provider model change.
func (e *Engine) ClearPendingRequestModelSwitch() {
	e.QueueRequestRuntimeSwitch("", "")
}

func (e *Engine) drainPendingRequestRuntimeSwitch() pendingRequestRuntimeSwitch {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	pending := pendingRequestRuntimeSwitch{
		model:           strings.TrimSpace(e.pendingRequestRuntime.model),
		reasoningEffort: strings.TrimSpace(e.pendingRequestRuntime.reasoningEffort),
	}
	e.pendingRequestRuntime = pendingRequestRuntimeSwitch{}
	return pending
}
