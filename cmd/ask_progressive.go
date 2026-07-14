package cmd

import (
	"errors"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/ui"
)

var errAskProgressiveBridgeStopped = errors.New("progressive stream consumer stopped")

type askProgressiveRunResult struct {
	Result progressiveRunResult
	Err    error
}

type askProgressiveRunnerSink struct {
	bridge *askProgressiveBridge
}

func (s askProgressiveRunnerSink) Event(ev llm.Event) {
	_ = s.EventWithError(ev)
}

func (s askProgressiveRunnerSink) EventWithError(ev llm.Event) error {
	if s.bridge == nil {
		return nil
	}
	return s.bridge.HandleEvent(ev)
}

type askProgressiveBridge struct {
	events chan ui.StreamEvent
	stats  *ui.SessionStats
	done   chan struct{}

	seenToolStarts map[string]struct{}
	seenToolEnds   map[string]struct{}
	stopOnce       sync.Once
	closeOnce      sync.Once

	attemptInput          int
	attemptOutput         int
	attemptCached         int
	attemptCacheWrite     int
	attemptUsageCalls     int
	attemptUsageCommitted bool
}

func newAskProgressiveBridge(bufSize int) *askProgressiveBridge {
	if bufSize <= 0 {
		bufSize = ui.DefaultStreamBufferSize
	}
	return &askProgressiveBridge{
		events:         make(chan ui.StreamEvent, bufSize),
		stats:          ui.NewSessionStats(),
		done:           make(chan struct{}),
		seenToolStarts: make(map[string]struct{}),
		seenToolEnds:   make(map[string]struct{}),
	}
}

func (b *askProgressiveBridge) markAttemptCommitted() {
	b.attemptInput, b.attemptOutput, b.attemptCached, b.attemptCacheWrite, b.attemptUsageCalls = 0, 0, 0, 0, 0
	b.attemptUsageCommitted = true
}

func (b *askProgressiveBridge) resetAttemptUsage() {
	b.attemptInput, b.attemptOutput, b.attemptCached, b.attemptCacheWrite, b.attemptUsageCalls = 0, 0, 0, 0, 0
	b.attemptUsageCommitted = false
}

func (b *askProgressiveBridge) Events() <-chan ui.StreamEvent {
	return b.events
}

func (b *askProgressiveBridge) Stats() *ui.SessionStats {
	return b.stats
}

// Stop signals that the event consumer has exited so producers stop
// blocking on sends into the bridge buffer.
func (b *askProgressiveBridge) Stop() {
	b.stopOnce.Do(func() {
		close(b.done)
	})
}

func (b *askProgressiveBridge) send(event ui.StreamEvent) error {
	select {
	case <-b.done:
		return errAskProgressiveBridgeStopped
	case b.events <- event:
		return nil
	}
}

func (b *askProgressiveBridge) HandleEvent(event llm.Event) error {
	if b == nil {
		return nil
	}
	switch event.Type {
	case llm.EventError:
		if event.Err != nil {
			return b.send(ui.ErrorEvent(event.Err))
		}
	case llm.EventTextDelta:
		b.attemptUsageCommitted = false
		if event.Text != "" {
			b.stats.ObserveOutput()
			return b.send(ui.TextEvent(event.Text))
		}
	case llm.EventReasoningDelta:
		b.attemptUsageCommitted = false
		kind := llm.NormalizeReasoningKind(event.ReasoningKind)
		if llm.IsEncryptedReasoningDelta(event) {
			b.stats.ObserveOutput()
			break
		}
		if event.Text == "" && event.ReasoningItemID == "" && !event.ReasoningFinal {
			break
		}
		if event.Text != "" {
			b.stats.ObserveOutput()
		}
		title := ""
		displayable := kind == llm.ReasoningKindSummary
		if kind == llm.ReasoningKindSummary && event.Text != "" {
			title = internalreasoning.ParseReasoningSummary(event.Text).Title
		}
		return b.send(ui.ReasoningEvent(kind, event.Text, title, event.ReasoningItemID, event.ReasoningFinal, displayable))
	case llm.EventToolCall:
		b.markAttemptCommitted()
		if event.Tool == nil {
			break
		}
		if isProgressToolName(event.Tool.Name) {
			break
		}
		toolCallID := event.ToolCallID
		if toolCallID == "" {
			toolCallID = event.Tool.ID
		}
		if toolCallID != "" {
			if _, ok := b.seenToolStarts[toolCallID]; ok {
				break
			}
			b.seenToolStarts[toolCallID] = struct{}{}
		}
		toolInfo := event.ToolInfo
		if toolInfo == "" {
			toolInfo = llm.ExtractToolInfo(*event.Tool)
		}
		toolArgs := event.ToolArgs
		if len(toolArgs) == 0 {
			toolArgs = event.Tool.Arguments
		}
		b.stats.ToolStart()
		return b.send(ui.ToolStartEvent(toolCallID, event.Tool.Name, toolInfo, toolArgs))
	case llm.EventToolExecStart:
		b.markAttemptCommitted()
		if isProgressToolName(event.ToolName) {
			break
		}
		if event.ToolCallID != "" {
			if _, ok := b.seenToolStarts[event.ToolCallID]; ok {
				break
			}
			b.seenToolStarts[event.ToolCallID] = struct{}{}
		}
		b.stats.ToolStart()
		return b.send(ui.ToolStartEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolArgs))
	case llm.EventToolExecEnd:
		if isProgressToolName(event.ToolName) {
			break
		}
		if event.ToolCallID != "" {
			if _, ok := b.seenToolEnds[event.ToolCallID]; ok {
				break
			}
			b.seenToolEnds[event.ToolCallID] = struct{}{}
		}
		b.stats.ToolEnd()
		b.resetAttemptUsage()
		if err := b.send(ui.ToolEndEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolSuccess)); err != nil {
			return err
		}
		for _, imagePath := range event.ToolImages {
			if err := b.send(ui.ImageEvent(imagePath)); err != nil {
				return err
			}
		}
		for _, d := range event.ToolDiffs {
			if err := b.send(ui.DiffEventWithOperation(d.File, d.Old, d.New, d.Line, d.Operation)); err != nil {
				return err
			}
		}
	case llm.EventUsage:
		b.stats.GenerationEnd()
		if event.Use != nil {
			b.stats.AddUsage(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens, event.Use.CacheWriteTokens)
			if !event.Use.BillableCountersZero() && !b.attemptUsageCommitted {
				b.attemptInput += event.Use.InputTokens
				b.attemptOutput += event.Use.OutputTokens
				b.attemptCached += event.Use.CachedInputTokens
				b.attemptCacheWrite += event.Use.CacheWriteTokens
				b.attemptUsageCalls++
			}
			return b.send(ui.UsageEvent(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens, event.Use.CacheWriteTokens))
		}
	case llm.EventPhase:
		if event.Text != "" {
			return b.send(ui.PhaseEvent(event.Text))
		}
	case llm.EventRetry:
		b.stats.ScheduleRetryStart(event.RetryWaitSecs)
		return b.send(ui.RetryEvent(event.RetryAttempt, event.RetryMaxAttempts, event.RetryWaitSecs))
	case llm.EventAttemptDiscard:
		b.stats.DiscardUsage(b.attemptInput, b.attemptOutput, b.attemptCached, b.attemptCacheWrite, b.attemptUsageCalls)
		b.resetAttemptUsage()
		return b.send(ui.AttemptDiscardEvent())
	case llm.EventModelSwitch:
		model := event.Model
		if model == "" {
			model = event.Text
		}
		b.stats.SetModel(model)
	case llm.EventInterjection:
		if event.Text != "" {
			return b.send(ui.InterjectionEvent(event.Text, event.InterjectionID))
		}
	}
	return nil
}

func (b *askProgressiveBridge) CloseSuccess() {
	b.closeOnce.Do(func() {
		_ = b.send(ui.DoneEvent(b.stats.OutputTokens))
		b.Stop()
		close(b.events)
	})
}

func (b *askProgressiveBridge) CloseError(err error) {
	b.closeOnce.Do(func() {
		if err != nil {
			_ = b.send(ui.ErrorEvent(err))
		}
		b.Stop()
		close(b.events)
	})
}
