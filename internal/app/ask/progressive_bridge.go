package ask

import (
	"errors"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
	internalreasoning "github.com/samsaffron/term-llm/internal/reasoning"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

var errAskProgressiveBridgeStopped = errors.New("progressive stream consumer stopped")

type ProgressiveRunnerSink struct {
	Bridge     *ProgressiveBridge
	OnGuardian func(tools.GuardianEvent)
}

func (s ProgressiveRunnerSink) GuardianEvent(event tools.GuardianEvent) {
	if s.Bridge != nil {
		_ = s.Bridge.HandleGuardianEvent(event)
	}
	if s.OnGuardian != nil {
		s.OnGuardian(event)
	}
}

func (s ProgressiveRunnerSink) Event(ev llm.Event) {
	_ = s.EventWithError(ev)
}

func (s ProgressiveRunnerSink) EventWithError(ev llm.Event) error {
	if s.Bridge == nil {
		return nil
	}
	return s.Bridge.HandleEvent(ev)
}

type ProgressiveBridge struct {
	events chan ui.StreamEvent
	stats  *ui.SessionStats
	done   chan struct{}

	seenToolStarts map[string]struct{}
	seenToolEnds   map[string]struct{}
	eventMu        sync.Mutex
	stopOnce       sync.Once
	closeOnce      sync.Once

	attemptInput          int
	attemptOutput         int
	attemptCached         int
	attemptCacheWrite     int
	attemptUsageCalls     int
	attemptUsageCommitted bool
}

func NewProgressiveBridge(bufSize int) *ProgressiveBridge {
	if bufSize <= 0 {
		bufSize = ui.DefaultStreamBufferSize
	}
	return &ProgressiveBridge{
		events:         make(chan ui.StreamEvent, bufSize),
		stats:          ui.NewSessionStats(),
		done:           make(chan struct{}),
		seenToolStarts: make(map[string]struct{}),
		seenToolEnds:   make(map[string]struct{}),
	}
}

func (b *ProgressiveBridge) markAttemptCommitted() {
	b.attemptInput, b.attemptOutput, b.attemptCached, b.attemptCacheWrite, b.attemptUsageCalls = 0, 0, 0, 0, 0
	b.attemptUsageCommitted = true
}

func (b *ProgressiveBridge) resetAttemptUsage() {
	b.attemptInput, b.attemptOutput, b.attemptCached, b.attemptCacheWrite, b.attemptUsageCalls = 0, 0, 0, 0, 0
	b.attemptUsageCommitted = false
}

func (b *ProgressiveBridge) Events() <-chan ui.StreamEvent {
	return b.events
}

func (b *ProgressiveBridge) Stats() *ui.SessionStats {
	return b.stats
}

// Stop signals that the event consumer has exited so producers stop
// blocking on sends into the bridge buffer.
func (b *ProgressiveBridge) Stop() {
	b.stopOnce.Do(func() {
		close(b.done)
	})
}

func (b *ProgressiveBridge) send(event ui.StreamEvent) error {
	select {
	case <-b.done:
		return errAskProgressiveBridgeStopped
	case b.events <- event:
		return nil
	}
}

func (b *ProgressiveBridge) HandleGuardianEvent(event tools.GuardianEvent) error {
	if b == nil {
		return nil
	}
	b.eventMu.Lock()
	defer b.eventMu.Unlock()
	addGuardianUsage(b.stats, event)
	return b.send(ui.GuardianReviewEvent(event))
}

func (b *ProgressiveBridge) HandleEvent(event llm.Event) error {
	if b == nil {
		return nil
	}
	b.eventMu.Lock()
	defer b.eventMu.Unlock()
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
		for _, media := range event.ToolMedia {
			if err := b.send(ui.MediaEvent(media)); err != nil {
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
	case llm.EventSteering:
		if event.Text != "" {
			return b.send(ui.SteeringEvent(event.Text, event.SteeringID))
		}
	}
	return nil
}

func (b *ProgressiveBridge) CloseSuccess() {
	b.closeOnce.Do(func() {
		b.eventMu.Lock()
		defer b.eventMu.Unlock()
		_ = b.send(ui.DoneEvent(b.stats.OutputTokens))
		b.Stop()
		close(b.events)
	})
}

func (b *ProgressiveBridge) CloseError(err error) {
	b.closeOnce.Do(func() {
		b.eventMu.Lock()
		defer b.eventMu.Unlock()
		if err != nil {
			_ = b.send(ui.ErrorEvent(err))
		}
		b.Stop()
		close(b.events)
	})
}

func addGuardianUsage(stats *ui.SessionStats, event tools.GuardianEvent) bool {
	if stats == nil || event.Usage.BillableCountersZero() {
		return false
	}
	u := event.Usage
	stats.AddGuardianUsageForModel(event.Model, u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteTokens)
	return true
}

func isProgressToolName(name string) bool {
	switch strings.TrimSpace(name) {
	case tools.UpdateProgressToolName, tools.FinalizeProgressToolName:
		return true
	default:
		return false
	}
}
