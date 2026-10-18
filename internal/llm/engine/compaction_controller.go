package engine

import (
	"context"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"strings"

	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// runCompactionController owns compaction state for exactly one runLoop
// invocation. It borrows the engine, request, context, and event sender; provider
// attempts, turn counters, callbacks, cancellation, and tool ownership remain
// with runLoop.
type runCompactionController struct {
	engine *Engine
	ctx    context.Context
	send   corestream.Sender
	req    *core.Request

	config       *CompactionConfig
	inputLimit   int
	systemPrompt string
	softRatio    float64
	hardRatio    float64

	reactiveDone      bool
	softInjected      bool
	softActive        bool
	softUsage         core.Usage
	softOriginal      []core.Message
	softPrepared      PreparedCompactionContext
	softOriginalCount int
	resumePending     bool
}

func newRunCompactionController(ctx context.Context, e *Engine, req *core.Request, send corestream.Sender) *runCompactionController {
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
			if msg.Role == core.RoleSystem {
				systemPrompt = core.CollectTextParts(msg.Parts)
				break
			}
		}
	}
	soft, hard := EffectiveCompactionThresholdRatios(config)
	return &runCompactionController{engine: e, ctx: ctx, send: send, req: req, config: config, inputLimit: inputLimit, systemPrompt: systemPrompt, softRatio: soft, hardRatio: hard}
}

func (c *runCompactionController) thresholdState(messages []core.Message) (estimate, soft, hard int) {
	if c.config == nil || c.inputLimit <= 0 {
		return 0, 0, 0
	}
	return c.engine.estimatedTokens(messages), int(float64(c.inputLimit) * c.softRatio), int(float64(c.inputLimit) * c.hardRatio)
}
func (c *runCompactionController) softReached(messages []core.Message) bool {
	est, soft, _ := c.thresholdState(messages)
	return soft > 0 && est >= soft
}
func (c *runCompactionController) hardReached(messages []core.Message) bool {
	est, _, hard := c.thresholdState(messages)
	return hard > 0 && est >= hard
}
func (c *runCompactionController) eligible(messages []core.Message) bool {
	return len(nonSystemMessages(messages)) > 1
}

func (c *runCompactionController) apply(result *CompactionResult) bool {
	if result != nil {
		result.NewMessages = restoreToolDiscoveryReplay(result.NewMessages, collectToolDiscoveryReplay(c.req.Messages))
	}
	if err := c.engine.PrepareCompactionContext(c.ctx, c.req.SessionID, c.req.Tools, result); err != nil {
		runtimeoutput.Warn("compaction plan restoration failed; continuing without it", "error", err)
	}
	if cb := c.engine.getCompactionCallback(); cb != nil {
		if err := cb(c.ctx, result); err != nil {
			runtimeoutput.Debug("compaction callback failed", "error", err)
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
	if err := c.send.Send(core.Event{Type: core.EventCompaction}); err != nil {
		runtimeoutput.Debug("send compaction boundary failed", "error", err)
	}
	return true
}

func (c *runCompactionController) resetSoft() {
	c.softActive = false
	c.softUsage = core.Usage{}
	c.softOriginal = nil
	c.softPrepared = PreparedCompactionContext{}
	c.softOriginalCount = 0
}
func (c *runCompactionController) beginSoft() {
	c.softInjected = true
	c.softActive = true
	c.softUsage = core.Usage{}
	c.softOriginal = append([]core.Message(nil), c.req.Messages...)
	nonSystem := nonSystemMessages(c.softOriginal)
	c.softPrepared = env.PrepareCompactionContext(nonSystem, *c.config, "")
	c.softOriginalCount = len(nonSystem)
}
func (c *runCompactionController) restoreSoftFailure(originalTools []core.ToolSpec, originalChoice core.ToolChoice) {
	if len(c.req.Messages) > 0 && strings.TrimSpace(core.MessageText(c.req.Messages[len(c.req.Messages)-1])) == strings.TrimSpace(ContextContinuationBriefPrompt) {
		c.req.Messages = c.req.Messages[:len(c.req.Messages)-1]
	}
	c.req.Tools = append([]core.ToolSpec(nil), originalTools...)
	c.req.ToolChoice = originalChoice
	resetProviderConversation(c.engine.provider)
	c.resetSoft()
}
func messagesWithoutTrailingBriefPrompt(messages []core.Message) []core.Message {
	messages = append([]core.Message(nil), messages...)
	if len(messages) > 0 && strings.TrimSpace(core.MessageText(messages[len(messages)-1])) == strings.TrimSpace(ContextContinuationBriefPrompt) {
		messages = messages[:len(messages)-1]
	}
	return messages
}
func (c *runCompactionController) applySoftHardFallback(originalTools []core.ToolSpec, originalChoice core.ToolChoice) bool {
	if c.config == nil {
		return false
	}
	messages := c.softOriginal
	if len(messages) == 0 {
		messages = messagesWithoutTrailingBriefPrompt(c.req.Messages)
	}
	result, err := env.Compact(c.ctx, c.engine.provider, c.req.Model, c.systemPrompt, nonSystemMessages(messages), *c.config)
	if err != nil {
		runtimeoutput.Debug("soft compaction hard fallback failed", "error", err)
		return false
	}
	if !c.softUsage.IsZero() {
		result.Usage.Add(c.softUsage)
	}
	if !c.apply(result) {
		return false
	}
	c.resetSoft()
	c.req.Messages = append(c.req.Messages, core.UserText(ContextContinuationPrompt))
	c.req.Tools = append([]core.ToolSpec(nil), originalTools...)
	c.req.ToolChoice = originalChoice
	return true
}
func (c *runCompactionController) maybeAfterResponse(pending []core.Message) bool {
	if c.config == nil || !c.eligible(c.req.Messages) {
		return false
	}
	candidate := append(append([]core.Message(nil), c.req.Messages...), pending...)
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
	if err := c.send.Send(core.Event{Type: core.EventPhase, Text: PhaseCompactingSummarizeHistory}); err != nil {
		runtimeoutput.Debug("send compaction phase failed", "error", err)
		return false
	}
	result, err := env.Compact(c.ctx, c.engine.provider, c.req.Model, c.systemPrompt, nonSystemMessages(c.req.Messages), *c.config)
	if err != nil {
		runtimeoutput.Debug("post-response compaction failed", "error", err)
		return false
	}
	return c.apply(result)
}

func (c *runCompactionController) beforeTurn() error {
	if c.config == nil || !c.eligible(c.req.Messages) {
		return nil
	}
	if c.hardReached(c.req.Messages) {
		if err := c.send.Send(core.Event{Type: core.EventPhase, Text: PhaseCompactingSummarizeHistory}); err != nil {
			return err
		}
		result, err := env.Compact(c.ctx, c.engine.provider, c.req.Model, c.systemPrompt, nonSystemMessages(c.req.Messages), *c.config)
		if err == nil {
			c.apply(result)
		}
		return nil
	}
	if !c.softInjected && len(c.req.Tools) > 0 && c.softReached(c.req.Messages) {
		if err := c.send.Send(core.Event{Type: core.EventPhase, Text: PhaseCompactingWriteBrief}); err != nil {
			return err
		}
		c.beginSoft()
		c.req.Messages = append(c.req.Messages, core.UserText(ContextContinuationBriefPrompt))
		if c.engine.provider.Capabilities().SupportsToolChoice {
			c.req.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceNone}
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
	return c.send.Send(core.Event{Type: core.EventPhase, Text: PhaseCompactingResumeTask})
}
