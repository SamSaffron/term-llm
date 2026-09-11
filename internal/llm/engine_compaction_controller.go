package llm

import (
	"context"
	"log/slog"
	"strings"
)

// runCompactionController owns compaction state for exactly one runLoop
// invocation. It borrows the engine, request, context, and event sender; provider
// attempts, turn counters, callbacks, cancellation, and tool ownership remain
// with runLoop.
type runCompactionController struct {
	engine *Engine
	ctx    context.Context
	send   eventSender
	req    *Request

	config       *CompactionConfig
	inputLimit   int
	systemPrompt string
	softRatio    float64
	hardRatio    float64

	reactiveDone      bool
	softInjected      bool
	softActive        bool
	softUsage         Usage
	softOriginal      []Message
	softPrepared      preparedCompactionContext
	softOriginalCount int
	resumePending     bool
}

func newRunCompactionController(ctx context.Context, e *Engine, req *Request, send eventSender) *runCompactionController {
	e.callbackMu.RLock()
	config := e.compactionConfig
	inputLimit := e.inputLimit
	e.callbackMu.RUnlock()
	if config != nil && inputLimit > 0 {
		copy := *config
		copy.InputLimit = inputLimit
		config = &copy
	}
	var systemPrompt string
	if inputLimit > 0 {
		for _, msg := range req.Messages {
			if msg.Role == RoleSystem {
				systemPrompt = collectTextParts(msg.Parts)
				break
			}
		}
	}
	soft, hard := effectiveCompactionThresholdRatios(config)
	return &runCompactionController{engine: e, ctx: ctx, send: send, req: req, config: config, inputLimit: inputLimit, systemPrompt: systemPrompt, softRatio: soft, hardRatio: hard}
}

func (c *runCompactionController) thresholdState(messages []Message) (estimate, soft, hard int) {
	if c.config == nil || c.inputLimit <= 0 {
		return 0, 0, 0
	}
	return c.engine.estimatedTokens(messages), int(float64(c.inputLimit) * c.softRatio), int(float64(c.inputLimit) * c.hardRatio)
}
func (c *runCompactionController) softReached(messages []Message) bool {
	est, soft, _ := c.thresholdState(messages)
	return soft > 0 && est >= soft
}
func (c *runCompactionController) hardReached(messages []Message) bool {
	est, _, hard := c.thresholdState(messages)
	return hard > 0 && est >= hard
}
func (c *runCompactionController) eligible(messages []Message) bool {
	return len(nonSystemMessages(messages)) > 1
}

func (c *runCompactionController) apply(result *CompactionResult) bool {
	if result != nil {
		result.NewMessages = restoreToolDiscoveryReplay(result.NewMessages, collectToolDiscoveryReplay(c.req.Messages))
	}
	if err := c.engine.PrepareCompactionContext(c.ctx, c.req.SessionID, c.req.Tools, result); err != nil {
		slog.Warn("compaction plan restoration failed; continuing without it", "error", err)
	}
	if cb := c.engine.getCompactionCallback(); cb != nil {
		if err := cb(c.ctx, result); err != nil {
			slog.Debug("compaction callback failed", "error", err)
			return false
		}
	}
	resetProviderConversation(c.engine.provider)
	c.req.Messages = result.ActiveMessages()
	c.resumePending = true
	c.engine.callbackMu.Lock()
	c.engine.lastTotalTokens = 0
	c.engine.lastMessageCount = 0
	c.engine.callbackMu.Unlock()
	if err := c.send.Send(Event{Type: EventCompaction}); err != nil {
		slog.Debug("send compaction boundary failed", "error", err)
	}
	return true
}

func (c *runCompactionController) resetSoft() {
	c.softActive = false
	c.softUsage = Usage{}
	c.softOriginal = nil
	c.softPrepared = preparedCompactionContext{}
	c.softOriginalCount = 0
}
func (c *runCompactionController) beginSoft() {
	c.softInjected = true
	c.softActive = true
	c.softUsage = Usage{}
	c.softOriginal = append([]Message(nil), c.req.Messages...)
	nonSystem := nonSystemMessages(c.softOriginal)
	c.softPrepared = prepareCompactionContext(nonSystem, *c.config, "")
	c.softOriginalCount = len(nonSystem)
}
func (c *runCompactionController) restoreSoftFailure(originalTools []ToolSpec, originalChoice ToolChoice) {
	if len(c.req.Messages) > 0 && strings.TrimSpace(MessageText(c.req.Messages[len(c.req.Messages)-1])) == strings.TrimSpace(contextContinuationBriefPrompt) {
		c.req.Messages = c.req.Messages[:len(c.req.Messages)-1]
	}
	c.req.Tools = append([]ToolSpec(nil), originalTools...)
	c.req.ToolChoice = originalChoice
	resetProviderConversation(c.engine.provider)
	c.resetSoft()
}
func messagesWithoutTrailingBriefPrompt(messages []Message) []Message {
	messages = append([]Message(nil), messages...)
	if len(messages) > 0 && strings.TrimSpace(MessageText(messages[len(messages)-1])) == strings.TrimSpace(contextContinuationBriefPrompt) {
		messages = messages[:len(messages)-1]
	}
	return messages
}
func (c *runCompactionController) applySoftHardFallback(originalTools []ToolSpec, originalChoice ToolChoice) bool {
	if c.config == nil {
		return false
	}
	messages := c.softOriginal
	if len(messages) == 0 {
		messages = messagesWithoutTrailingBriefPrompt(c.req.Messages)
	}
	result, err := Compact(c.ctx, c.engine.provider, c.req.Model, c.systemPrompt, nonSystemMessages(messages), *c.config)
	if err != nil {
		slog.Debug("soft compaction hard fallback failed", "error", err)
		return false
	}
	if !c.softUsage.IsZero() {
		result.Usage.Add(c.softUsage)
	}
	if !c.apply(result) {
		return false
	}
	c.resetSoft()
	c.req.Messages = append(c.req.Messages, UserText(contextContinuationPrompt))
	c.req.Tools = append([]ToolSpec(nil), originalTools...)
	c.req.ToolChoice = originalChoice
	return true
}
func (c *runCompactionController) maybeAfterResponse(pending []Message) bool {
	if c.config == nil || !c.eligible(c.req.Messages) {
		return false
	}
	candidate := append(append([]Message(nil), c.req.Messages...), pending...)
	pendingTool := false
	for _, msg := range pending {
		for _, part := range msg.Parts {
			if part.ToolCall != nil {
				pendingTool = true
				break
			}
		}
		if pendingTool {
			break
		}
	}
	should := c.softReached(candidate)
	if pendingTool {
		should = c.hardReached(candidate)
	}
	if !should {
		return false
	}
	if err := c.send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); err != nil {
		slog.Debug("send compaction phase failed", "error", err)
		return false
	}
	result, err := Compact(c.ctx, c.engine.provider, c.req.Model, c.systemPrompt, nonSystemMessages(c.req.Messages), *c.config)
	if err != nil {
		slog.Debug("post-response compaction failed", "error", err)
		return false
	}
	return c.apply(result)
}

func (c *runCompactionController) beforeTurn() error {
	if c.config == nil || !c.eligible(c.req.Messages) {
		return nil
	}
	if c.hardReached(c.req.Messages) {
		if err := c.send.Send(Event{Type: EventPhase, Text: PhaseCompactingSummarizeHistory}); err != nil {
			return err
		}
		result, err := Compact(c.ctx, c.engine.provider, c.req.Model, c.systemPrompt, nonSystemMessages(c.req.Messages), *c.config)
		if err == nil {
			c.apply(result)
		}
		return nil
	}
	if !c.softInjected && len(c.req.Tools) > 0 && c.softReached(c.req.Messages) {
		if err := c.send.Send(Event{Type: EventPhase, Text: PhaseCompactingWriteBrief}); err != nil {
			return err
		}
		c.beginSoft()
		c.req.Messages = append(c.req.Messages, UserText(contextContinuationBriefPrompt))
		if c.engine.provider.Capabilities().SupportsToolChoice {
			c.req.ToolChoice = ToolChoice{Mode: ToolChoiceNone}
		} else {
			c.req.Tools = nil
		}
	}
	return nil
}
func (c *runCompactionController) emitResumePhase() error {
	if !c.resumePending || c.softActive {
		return nil
	}
	c.resumePending = false
	return c.send.Send(Event{Type: EventPhase, Text: PhaseCompactingResumeTask})
}
