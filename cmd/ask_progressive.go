package cmd

import (
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
)

type askProgressiveRunResult struct {
	Result progressiveRunResult
	Err    error
}

type askProgressiveBridge struct {
	events chan ui.StreamEvent
	stats  *ui.SessionStats

	seenToolStarts map[string]struct{}
	seenToolEnds   map[string]struct{}
	closeOnce      sync.Once
}

func newAskProgressiveBridge(bufSize int) *askProgressiveBridge {
	if bufSize <= 0 {
		bufSize = ui.DefaultStreamBufferSize
	}
	return &askProgressiveBridge{
		events:         make(chan ui.StreamEvent, bufSize),
		stats:          ui.NewSessionStats(),
		seenToolStarts: make(map[string]struct{}),
		seenToolEnds:   make(map[string]struct{}),
	}
}

func (b *askProgressiveBridge) Events() <-chan ui.StreamEvent {
	return b.events
}

func (b *askProgressiveBridge) Stats() *ui.SessionStats {
	return b.stats
}

func (b *askProgressiveBridge) HandleEvent(event llm.Event) error {
	switch event.Type {
	case llm.EventError:
		if event.Err != nil {
			b.events <- ui.ErrorEvent(event.Err)
		}
	case llm.EventTextDelta:
		if event.Text != "" {
			b.events <- ui.TextEvent(event.Text)
		}
	case llm.EventToolCall:
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
		b.events <- ui.ToolStartEvent(toolCallID, event.Tool.Name, toolInfo, toolArgs)
	case llm.EventToolExecStart:
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
		b.events <- ui.ToolStartEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolArgs)
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
		b.events <- ui.ToolEndEvent(event.ToolCallID, event.ToolName, event.ToolInfo, event.ToolSuccess)
		for _, imagePath := range event.ToolImages {
			b.events <- ui.ImageEvent(imagePath)
		}
		for _, d := range event.ToolDiffs {
			b.events <- ui.DiffEvent(d.File, d.Old, d.New, d.Line)
		}
	case llm.EventUsage:
		if event.Use != nil {
			b.stats.AddUsage(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens, event.Use.CacheWriteTokens)
			b.events <- ui.UsageEvent(event.Use.InputTokens, event.Use.OutputTokens, event.Use.CachedInputTokens, event.Use.CacheWriteTokens)
		}
	case llm.EventPhase:
		if event.Text != "" {
			b.events <- ui.PhaseEvent(event.Text)
		}
	case llm.EventRetry:
		b.events <- ui.RetryEvent(event.RetryAttempt, event.RetryMaxAttempts, event.RetryWaitSecs)
	case llm.EventInterjection:
		if event.Text != "" {
			b.events <- ui.InterjectionEvent(event.Text)
		}
	}
	return nil
}

func (b *askProgressiveBridge) CloseSuccess() {
	b.closeOnce.Do(func() {
		b.events <- ui.DoneEvent(b.stats.OutputTokens)
		close(b.events)
	})
}

func (b *askProgressiveBridge) CloseError(err error) {
	b.closeOnce.Do(func() {
		if err != nil {
			b.events <- ui.ErrorEvent(err)
		}
		close(b.events)
	})
}
