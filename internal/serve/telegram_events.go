package serve

import (
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
)

// telegramEventAccumulator owns presentation state reduced from one Telegram
// stream. It has one event-consumer writer and coordinator snapshot readers;
// snapshots never expose its mutable maps or slices.
type telegramEventAccumulator struct {
	mu                                                             sync.Mutex
	text                                                           strings.Builder
	activeTools                                                    map[string]string
	activePhase                                                    string
	toolsRan                                                       bool
	images                                                         []string
	media                                                          []llm.MediaArtifact
	textDeltas, reasoningDeltas                                    int
	toolStarts, toolEnds, toolCalls                                int
	phaseEvents, usageEvents, doneEvents, retryEvents, errorEvents int
	otherEvents                                                    int
	otherTypes                                                     map[llm.EventType]int
}

type telegramEventSnapshot struct {
	Text                                                           string
	ToolDisplay                                                    string
	Phase                                                          string
	ToolsRan                                                       bool
	Images                                                         []string
	Media                                                          []llm.MediaArtifact
	TextDeltas, ReasoningDeltas                                    int
	ToolStarts, ToolEnds, ToolCalls                                int
	PhaseEvents, UsageEvents, DoneEvents, RetryEvents, ErrorEvents int
	OtherEvents                                                    int
	OtherTypes                                                     map[llm.EventType]int
}

func newTelegramEventAccumulator(resume *telegramContinuation) *telegramEventAccumulator {
	a := &telegramEventAccumulator{activeTools: make(map[string]string), otherTypes: make(map[llm.EventType]int)}
	if resume != nil {
		a.text.WriteString(resume.Text)
		a.images = append(a.images, resume.Images...)
		a.media = append(a.media, resume.Media...)
	}
	return a
}

// Apply reduces one event and returns the accumulated prose byte length.
func (a *telegramEventAccumulator) Apply(ev llm.Event) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch ev.Type {
	case llm.EventTextDelta:
		a.text.WriteString(ev.Text)
		a.textDeltas++
	case llm.EventReasoningDelta:
		a.reasoningDeltas++
	case llm.EventToolExecStart:
		a.activeTools[ev.ToolCallID] = ev.ToolName
		a.toolsRan = true
		a.toolStarts++
	case llm.EventToolExecEnd:
		delete(a.activeTools, ev.ToolCallID)
		a.toolEnds++
		a.images = append(a.images, ev.ToolImages...)
		a.media = append(a.media, ev.ToolMedia...)
	case llm.EventHeartbeat:
	case llm.EventPhase:
		a.activePhase = ev.Text
		a.phaseEvents++
	case llm.EventToolCall:
		a.toolCalls++
	case llm.EventUsage:
		a.usageEvents++
	case llm.EventDone:
		a.doneEvents++
	case llm.EventRetry:
		a.retryEvents++
		a.activePhase = ui.FormatRetryStatus("Retrying", ev.RetryAttempt, ev.RetryMaxAttempts, ev.RetryWaitSecs, 0, "")
	case llm.EventSteering:
		a.activePhase = "📝 Considering: " + tailRunes(strings.TrimSpace(ev.Text), 80)
	case llm.EventError:
		a.errorEvents++
	default:
		a.otherEvents++
		a.otherTypes[ev.Type]++
	}
	return a.text.Len()
}

func (a *telegramEventAccumulator) Snapshot() telegramEventSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	other := make(map[llm.EventType]int, len(a.otherTypes))
	for k, v := range a.otherTypes {
		other[k] = v
	}
	return telegramEventSnapshot{
		Text: a.text.String(), ToolDisplay: activeToolDisplay(a.activeTools), Phase: a.activePhase, ToolsRan: a.toolsRan,
		Images: append([]string(nil), a.images...), Media: append([]llm.MediaArtifact(nil), a.media...),
		TextDeltas: a.textDeltas, ReasoningDeltas: a.reasoningDeltas, ToolStarts: a.toolStarts, ToolEnds: a.toolEnds, ToolCalls: a.toolCalls,
		PhaseEvents: a.phaseEvents, UsageEvents: a.usageEvents, DoneEvents: a.doneEvents, RetryEvents: a.retryEvents, ErrorEvents: a.errorEvents,
		OtherEvents: a.otherEvents, OtherTypes: other,
	}
}

func (a *telegramEventAccumulator) Text() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.text.String()
}
