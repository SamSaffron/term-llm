package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
)

// lifecycleProvider scripts one engine run and lets lifecycle tests gate the
// compaction helper (or soft-brief) stream so the active state can be observed
// while compaction is genuinely blocked.
type lifecycleProvider struct {
	mu     sync.Mutex
	calls  []core.Request
	script func(ctx context.Context, call int, req core.Request) (core.Stream, error)
}

func newLifecycleProvider(script func(ctx context.Context, call int, req core.Request) (core.Stream, error)) *lifecycleProvider {
	return &lifecycleProvider{script: script}
}

func (p *lifecycleProvider) Name() string       { return "fake" }
func (p *lifecycleProvider) Credential() string { return "test" }

func (p *lifecycleProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: true, SupportsToolChoice: true}
}

func (p *lifecycleProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	call := len(p.calls) - 1
	p.mu.Unlock()
	return p.script(ctx, call, req)
}

func (p *lifecycleProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// gatedLifecycleStream blocks until release (or context cancellation) before
// yielding any event. Close does not release it: a real compaction helper would
// also stay blocked until its provider call returns.
type gatedLifecycleStream struct {
	ctx     context.Context
	started chan struct{}
	release chan struct{}
	events  []core.Event
	index   int
	once    sync.Once
}

func (s *gatedLifecycleStream) Recv() (core.Event, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
	case <-s.ctx.Done():
		return core.Event{}, s.ctx.Err()
	}
	if s.index >= len(s.events) {
		return core.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *gatedLifecycleStream) Close() error { return nil }

func newLifecycleTestEngine(t *testing.T, model string, limit int, provider core.Provider, registry *ToolRegistry) *Engine {
	t.Helper()
	useTestInputLimit(t, "fake", model, limit)
	e := NewEngine(provider, registry)
	e.ConfigureContextManagement(provider, "fake", model, true)
	return e
}

type compactionStatusWant struct {
	active      bool
	errContains string
}

func collectCompactionStatuses(events []core.Event) []core.Event {
	var statuses []core.Event
	for _, event := range events {
		if event.Type == core.EventCompactionStatus {
			statuses = append(statuses, event)
		}
	}
	return statuses
}

func assertCompactionStatuses(t *testing.T, events []core.Event, want []compactionStatusWant) {
	t.Helper()
	got := collectCompactionStatuses(events)
	if len(got) != len(want) {
		t.Fatalf("compaction status events = %d (%+v), want %d: events=%v", len(got), statusEventsForLog(got), len(want), eventTypes(events))
	}
	for i, expected := range want {
		status := got[i]
		if status.CompactionActive != expected.active {
			t.Fatalf("status[%d].CompactionActive = %v, want %v", i, status.CompactionActive, expected.active)
		}
		if expected.errContains == "" {
			if status.Err != nil {
				t.Fatalf("status[%d].Err = %v, want nil", i, status.Err)
			}
			continue
		}
		if status.Err == nil || !strings.Contains(status.Err.Error(), expected.errContains) {
			t.Fatalf("status[%d].Err = %v, want containing %q", i, status.Err, expected.errContains)
		}
	}
}

func statusEventsForLog(statuses []core.Event) []string {
	out := make([]string, 0, len(statuses))
	for _, status := range statuses {
		text := "active"
		if !status.CompactionActive {
			text = "idle"
		}
		if status.Err != nil {
			text += ": " + status.Err.Error()
		}
		out = append(out, text)
	}
	return out
}

// assertBalancedCompactionStatusSequence requires strict begin/end pairing:
// every begin is followed by an end before another begin, and the run never
// finishes active. A run can retry compaction after a failed attempt, so
// callers should assert the sequence shape rather than a fixed pair count.
func assertBalancedCompactionStatusSequence(t *testing.T, events []core.Event) []core.Event {
	t.Helper()
	statuses := collectCompactionStatuses(events)
	if len(statuses) == 0 {
		t.Fatalf("no compaction status events: events=%v", eventTypes(events))
	}
	for i, status := range statuses {
		wantActive := i%2 == 0
		if status.CompactionActive != wantActive {
			t.Fatalf("status[%d].CompactionActive = %v, want %v: %v", i, status.CompactionActive, wantActive, statusEventsForLog(statuses))
		}
	}
	if statuses[len(statuses)-1].CompactionActive {
		t.Fatalf("run finished with a permanently active compaction: %v", statusEventsForLog(statuses))
	}
	return statuses
}

func eventIndex(events []core.Event, match func(core.Event) bool) int {
	for i, event := range events {
		if match(event) {
			return i
		}
	}
	return -1
}

func recvLifecycleEvent(t *testing.T, stream core.Stream) (core.Event, bool) {
	t.Helper()
	event, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return core.Event{}, false
	}
	if err != nil {
		t.Fatalf("Recv() error: %v", err)
	}
	if event.Type == core.EventError && event.Err != nil {
		t.Fatalf("stream error event: %v", event.Err)
	}
	return event, true
}

func recvUntilCompactionBegin(t *testing.T, stream core.Stream, events *[]core.Event) {
	t.Helper()
	for {
		event, ok := recvLifecycleEvent(t, stream)
		if !ok {
			t.Fatalf("stream ended before compaction begin: events=%v", eventTypes(*events))
		}
		*events = append(*events, event)
		if event.Type == core.EventCompactionStatus && event.CompactionActive {
			return
		}
	}
}

func drainLifecycleEvents(t *testing.T, stream core.Stream, events *[]core.Event) {
	t.Helper()
	for {
		event, ok := recvLifecycleEvent(t, stream)
		if !ok {
			return
		}
		*events = append(*events, event)
	}
}

func drainLifecycleEventsAllowError(stream core.Stream, events *[]core.Event) error {
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		*events = append(*events, event)
		if event.Type == core.EventError && event.Err != nil {
			return event.Err
		}
	}
}

func visibleLifecycleText(events []core.Event) string {
	var text strings.Builder
	for _, event := range events {
		if event.Type == core.EventTextDelta {
			text.WriteString(event.Text)
		}
	}
	return text.String()
}

// A hard pre-turn compaction must report active before it calls the summarizer
// and stay active until the summary is applied, even while the helper blocks.
func TestCompactionLifecycleHardSummarizerBlocked(t *testing.T) {
	const model = "lifecycle-hard-blocked"
	release := make(chan struct{})
	started := make(chan struct{})
	provider := newLifecycleProvider(func(ctx context.Context, call int, req core.Request) (core.Stream, error) {
		if req.Ephemeral {
			return &gatedLifecycleStream{ctx: ctx, started: started, release: release, events: []core.Event{{Type: core.EventTextDelta, Text: "hard summary"}}}, nil
		}
		return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "after hard compaction"}}}, nil
	})
	e := newLifecycleTestEngine(t, model, 1000, provider, nil)
	stream, err := e.Stream(context.Background(), core.Request{
		Model: model,
		Messages: []core.Message{
			core.SystemText("system"),
			core.UserText(strings.Repeat("h", 4000)),
			core.AssistantText("prior answer"),
			core.UserText("continue"),
		},
		Tools: []core.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var events []core.Event
	recvUntilCompactionBegin(t, stream, &events)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatalf("summarizer never started: events=%v", eventTypes(events))
	}
	close(release)
	drainLifecycleEvents(t, stream, &events)

	assertCompactionStatuses(t, events, []compactionStatusWant{{active: true}, {active: false}})
	boundary := eventIndex(events, func(event core.Event) bool { return event.Type == core.EventCompaction })
	idle := eventIndex(events, func(event core.Event) bool {
		return event.Type == core.EventCompactionStatus && !event.CompactionActive
	})
	if boundary < 0 || idle < 0 || idle < boundary {
		t.Fatalf("idle status must follow the compaction boundary: events=%v", eventTypes(events))
	}
	if got := visibleLifecycleText(events); got != "after hard compaction" {
		t.Fatalf("visible text = %q, want resumed answer only", got)
	}
	if provider.callCount() != 2 {
		t.Fatalf("provider calls = %d, want summarizer + resumed turn", provider.callCount())
	}
}

// Soft compaction's lifecycle spans the brief provider turn. Phase changes and
// the brief stream itself must not project an idle status before the brief is
// applied.
func TestCompactionLifecycleSoftBriefBlocked(t *testing.T) {
	const model = "lifecycle-soft-blocked"
	release := make(chan struct{})
	started := make(chan struct{})
	provider := newLifecycleProvider(func(ctx context.Context, call int, req core.Request) (core.Stream, error) {
		if call == 0 {
			return &gatedLifecycleStream{
				ctx:     ctx,
				started: started,
				release: release,
				events: []core.Event{
					{Type: core.EventPhase, Text: "Thinking"},
					{Type: core.EventTextDelta, Text: "brief body"},
				},
			}, nil
		}
		return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "after soft compaction"}}}, nil
	})
	e := newLifecycleTestEngine(t, model, 1000, provider, nil)
	stream, err := e.Stream(context.Background(), core.Request{
		Model: model,
		Messages: []core.Message{
			core.SystemText("system"),
			core.UserText(strings.Repeat("s", 3600)),
			core.AssistantText("prior answer"),
			core.UserText("continue"),
		},
		Tools: []core.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var events []core.Event
	recvUntilCompactionBegin(t, stream, &events)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatalf("soft brief turn never started: events=%v", eventTypes(events))
	}
	close(release)
	drainLifecycleEvents(t, stream, &events)

	assertCompactionStatuses(t, events, []compactionStatusWant{{active: true}, {active: false}})
	begin := eventIndex(events, func(event core.Event) bool {
		return event.Type == core.EventCompactionStatus && event.CompactionActive
	})
	briefPhase := eventIndex(events, func(event core.Event) bool {
		return event.Type == core.EventPhase && event.Text == PhaseCompactingWriteBrief
	})
	thinking := eventIndex(events, func(event core.Event) bool {
		return event.Type == core.EventPhase && event.Text == "Thinking"
	})
	boundary := eventIndex(events, func(event core.Event) bool { return event.Type == core.EventCompaction })
	idle := eventIndex(events, func(event core.Event) bool {
		return event.Type == core.EventCompactionStatus && !event.CompactionActive
	})
	if begin < 0 || briefPhase < begin {
		t.Fatalf("compaction begin must precede the brief phase: events=%v", eventTypes(events))
	}
	if thinking < begin || boundary < thinking || idle < boundary {
		t.Fatalf("brief phase streaming must not end the lifecycle early: events=%v", eventTypes(events))
	}
	if got := visibleLifecycleText(events); got != "after soft compaction" {
		t.Fatalf("visible text = %q, want only the resumed answer", got)
	}
}

// A failed or cancelled summarizer must end the lifecycle with its error before
// the normal turn resumes without compacted history.
func TestCompactionLifecycleHardFailureEndsActiveBeforeResume(t *testing.T) {
	for _, tc := range []struct {
		name        string
		helperErr   error
		errContains string
	}{
		{name: "failure", helperErr: errors.New("summarizer unavailable"), errContains: "summarizer unavailable"},
		{name: "cancellation", helperErr: context.Canceled, errContains: "context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := "lifecycle-hard-" + tc.name
			provider := newLifecycleProvider(func(ctx context.Context, call int, req core.Request) (core.Stream, error) {
				if req.Ephemeral {
					return nil, tc.helperErr
				}
				return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "normal turn"}}}, nil
			})
			e := newLifecycleTestEngine(t, model, 1000, provider, nil)
			stream, err := e.Stream(context.Background(), core.Request{
				Model: model,
				Messages: []core.Message{
					core.SystemText("system"),
					core.UserText(strings.Repeat("h", 4000)),
					core.AssistantText("prior answer"),
					core.UserText("continue"),
				},
				Tools: []core.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
			})
			if err != nil {
				t.Fatalf("Stream() error = %v", err)
			}
			defer stream.Close()

			var events []core.Event
			drainLifecycleEvents(t, stream, &events)

			statuses := assertBalancedCompactionStatusSequence(t, events)
			for i, status := range statuses {
				if status.CompactionActive {
					continue
				}
				if status.Err == nil || !strings.Contains(status.Err.Error(), tc.errContains) {
					t.Fatalf("status[%d].Err = %v, want containing %q: %v", i, status.Err, tc.errContains, statusEventsForLog(statuses))
				}
				if tc.name == "cancellation" && !errors.Is(status.Err, context.Canceled) {
					t.Fatalf("cancellation status Err = %v, want context.Canceled", status.Err)
				}
			}
			if got := visibleLifecycleText(events); got != "normal turn" {
				t.Fatalf("visible text = %q, want the normal turn to resume", got)
			}
		})
	}
}

// When the soft brief is unusable and the fallback summary fails, the pending
// soft span ends exactly once, with a failure, before the original turn retries.
func TestCompactionLifecycleSoftFallbackFailureEndsActive(t *testing.T) {
	const model = "lifecycle-soft-fallback-failure"
	provider := newLifecycleProvider(func(ctx context.Context, call int, req core.Request) (core.Stream, error) {
		switch {
		case call == 0:
			// The brief turn produced no usable assistant content.
			return &sliceStream{}, nil
		case req.Ephemeral:
			return nil, errors.New("fallback summarizer unavailable")
		default:
			return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "after restored soft compaction"}}}, nil
		}
	})
	e := newLifecycleTestEngine(t, model, 1000, provider, nil)
	stream, err := e.Stream(context.Background(), core.Request{
		Model: model,
		Messages: []core.Message{
			core.SystemText("system"),
			core.UserText(strings.Repeat("s", 3600)),
			core.AssistantText("prior answer"),
			core.UserText("continue"),
		},
		Tools: []core.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var events []core.Event
	drainLifecycleEvents(t, stream, &events)

	statuses := assertBalancedCompactionStatusSequence(t, events)
	softFailureSeen := false
	for _, status := range statuses {
		if status.CompactionActive {
			continue
		}
		if status.Err == nil {
			t.Fatalf("every failed soft attempt must end with an error: %v", statusEventsForLog(statuses))
		}
		if errors.Is(status.Err, errCompactionBriefUnusable) {
			softFailureSeen = true
		}
	}
	if !softFailureSeen {
		t.Fatalf("missing soft-brief failure status: %v", statusEventsForLog(statuses))
	}
	summarize := eventIndex(events, func(event core.Event) bool {
		return event.Type == core.EventPhase && event.Text == PhaseCompactingSummarizeHistory
	})
	if summarize < 0 {
		t.Fatalf("events = %v, want the preserved summarize-history phase", eventTypes(events))
	}
	if got := visibleLifecycleText(events); got != "after restored soft compaction" {
		t.Fatalf("visible text = %q", got)
	}
}

// A reactive overflow during the soft brief can resolve through a hard
// fallback. That fallback belongs to the existing soft span: its successful
// apply must end the lifecycle before resumed generation, without leaving an
// unmatched span that the run-exit cleanup would report as unfinished.
func TestCompactionLifecycleSoftReactiveFallbackEndsBeforeResume(t *testing.T) {
	overflow := errors.New("Responses API error (context_length_exceeded): input exceeds the context window")
	for _, tc := range []struct {
		name  string
		brief func(context.Context) (core.Stream, error)
	}{
		{
			name: "stream_open",
			brief: func(context.Context) (core.Stream, error) {
				return nil, overflow
			},
		},
		{
			name: "stream_event",
			brief: func(context.Context) (core.Stream, error) {
				return &sliceStream{events: []core.Event{{Type: core.EventError, Err: overflow}}}, nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := "lifecycle-soft-reactive-" + tc.name
			release := make(chan struct{})
			started := make(chan struct{})
			provider := newLifecycleProvider(func(ctx context.Context, call int, req core.Request) (core.Stream, error) {
				switch {
				case call == 0:
					return tc.brief(ctx)
				case req.Ephemeral:
					return &gatedLifecycleStream{
						ctx:     ctx,
						started: started,
						release: release,
						events:  []core.Event{{Type: core.EventTextDelta, Text: "hard fallback summary"}},
					}, nil
				default:
					return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "resumed generation"}}}, nil
				}
			})
			e := newLifecycleTestEngine(t, model, 1000, provider, nil)
			stream, err := e.Stream(context.Background(), core.Request{
				Model: model,
				Messages: []core.Message{
					core.SystemText("system"),
					core.UserText(strings.Repeat("s", 3600)),
					core.AssistantText("prior answer"),
					core.UserText("continue"),
				},
				Tools: []core.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
			})
			if err != nil {
				t.Fatalf("Stream() error = %v", err)
			}
			defer stream.Close()

			var events []core.Event
			recvUntilCompactionBegin(t, stream, &events)
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatalf("hard fallback never started: events=%v", eventTypes(events))
			}
			close(release)
			drainLifecycleEvents(t, stream, &events)

			assertCompactionStatuses(t, events, []compactionStatusWant{{active: true}, {active: false}})
			boundary := eventIndex(events, func(event core.Event) bool { return event.Type == core.EventCompaction })
			idle := eventIndex(events, func(event core.Event) bool {
				return event.Type == core.EventCompactionStatus && !event.CompactionActive
			})
			resumed := eventIndex(events, func(event core.Event) bool {
				return event.Type == core.EventTextDelta && event.Text == "resumed generation"
			})
			if boundary < 0 || idle < boundary {
				t.Fatalf("idle status must follow the fallback boundary: events=%v", eventTypes(events))
			}
			if resumed < 0 || resumed < idle {
				t.Fatalf("lifecycle must end before resumed generation: events=%v", eventTypes(events))
			}
			if got := visibleLifecycleText(events); got != "resumed generation" {
				t.Fatalf("visible text = %q", got)
			}
			if provider.callCount() != 3 {
				t.Fatalf("provider calls = %d, want brief + fallback + resumed generation", provider.callCount())
			}
		})
	}
}

// If the soft brief's hard fallback also fails, the existing soft span still
// ends exactly once with the original stream failure and no unfinished tail.
func TestCompactionLifecycleSoftReactiveFallbackFailureEndsActive(t *testing.T) {
	const model = "lifecycle-soft-reactive-failure"
	provider := newLifecycleProvider(func(ctx context.Context, call int, req core.Request) (core.Stream, error) {
		switch {
		case call == 0:
			return &sliceStream{events: []core.Event{{Type: core.EventError, Err: errors.New("Responses API error (context_length_exceeded): input exceeds the context window")}}}, nil
		case req.Ephemeral:
			return nil, errors.New("hard fallback summarizer down")
		default:
			return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "unexpected"}}}, nil
		}
	})
	e := newLifecycleTestEngine(t, model, 1000, provider, nil)
	stream, err := e.Stream(context.Background(), core.Request{
		Model: model,
		Messages: []core.Message{
			core.SystemText("system"),
			core.UserText(strings.Repeat("s", 3600)),
			core.AssistantText("prior answer"),
			core.UserText("continue"),
		},
		Tools: []core.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var events []core.Event
	runErr := drainLifecycleEventsAllowError(stream, &events)
	if runErr == nil || !IsContextOverflowError(runErr) {
		t.Fatalf("run error = %v, want the original context overflow", runErr)
	}
	statuses := assertBalancedCompactionStatusSequence(t, events)
	if len(statuses) != 2 {
		t.Fatalf("compaction statuses = %v, want one soft begin/end pair", statusEventsForLog(statuses))
	}
	if statuses[1].Err == nil || !IsContextOverflowError(statuses[1].Err) {
		t.Fatalf("ending status Err = %v, want the original overflow failure", statuses[1].Err)
	}
	if strings.Contains(statuses[1].Err.Error(), errCompactionUnfinished.Error()) {
		t.Fatalf("ending status reported an unfinished span: %v", statuses[1].Err)
	}
}

// Reactive overflow compaction reports one begin/end pair around the helper and
// boundary when it recovers the turn.
func TestCompactionLifecycleReactiveOverflow(t *testing.T) {
	const model = "lifecycle-reactive"
	provider := newLifecycleProvider(func(ctx context.Context, call int, req core.Request) (core.Stream, error) {
		switch {
		case req.Ephemeral:
			return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "reactive summary"}}}, nil
		case call == 0:
			return &sliceStream{events: []core.Event{{Type: core.EventError, Err: errors.New("Responses API error (context_length_exceeded): input exceeds the context window")}}}, nil
		default:
			return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "after reactive compaction"}}}, nil
		}
	})
	e := newLifecycleTestEngine(t, model, 1000, provider, nil)
	stream, err := e.Stream(context.Background(), core.Request{
		Model: model,
		Messages: []core.Message{
			core.SystemText("system"),
			core.UserText("small history"),
			core.AssistantText("prior answer"),
			core.UserText("follow up"),
		},
		Tools: []core.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var events []core.Event
	drainLifecycleEvents(t, stream, &events)

	assertCompactionStatuses(t, events, []compactionStatusWant{{active: true}, {active: false}})
	boundary := eventIndex(events, func(event core.Event) bool { return event.Type == core.EventCompaction })
	idle := eventIndex(events, func(event core.Event) bool {
		return event.Type == core.EventCompactionStatus && !event.CompactionActive
	})
	if boundary < 0 || idle < boundary {
		t.Fatalf("idle status must follow the reactive boundary: events=%v", eventTypes(events))
	}
	if got := visibleLifecycleText(events); got != "after reactive compaction" {
		t.Fatalf("visible text = %q", got)
	}
}

// A reactive compaction whose helper fails still ends the lifecycle; the run
// then terminates with the original overflow error rather than staying active.
func TestCompactionLifecycleReactiveFailureEndsActive(t *testing.T) {
	const model = "lifecycle-reactive-failure"
	provider := newLifecycleProvider(func(ctx context.Context, call int, req core.Request) (core.Stream, error) {
		switch {
		case req.Ephemeral:
			return nil, errors.New("reactive summarizer down")
		case call == 0:
			return &sliceStream{events: []core.Event{{Type: core.EventError, Err: errors.New("Responses API error (context_length_exceeded): input exceeds the context window")}}}, nil
		default:
			return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "unexpected"}}}, nil
		}
	})
	e := newLifecycleTestEngine(t, model, 1000, provider, nil)
	stream, err := e.Stream(context.Background(), core.Request{
		Model: model,
		Messages: []core.Message{
			core.SystemText("system"),
			core.UserText("small history"),
			core.AssistantText("prior answer"),
			core.UserText("follow up"),
		},
		Tools: []core.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var events []core.Event
	runErr := drainLifecycleEventsAllowError(stream, &events)
	if runErr == nil || !IsContextOverflowError(runErr) {
		t.Fatalf("run error = %v, want the original context overflow", runErr)
	}
	assertCompactionStatuses(t, events, []compactionStatusWant{{active: true}, {active: false, errContains: "reactive summarizer down"}})
}

// Post-response compaction before a tool call reports the same balanced
// lifecycle as pre-turn and reactive compaction.
func TestCompactionLifecyclePostResponse(t *testing.T) {
	const model = "lifecycle-post-response"
	registry := NewToolRegistry()
	tool := &countingTool{}
	registry.Register(tool)
	provider := newLifecycleProvider(func(ctx context.Context, call int, req core.Request) (core.Stream, error) {
		switch {
		case req.Ephemeral:
			return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "post-response summary"}}}, nil
		case call == 0:
			return &sliceStream{events: []core.Event{
				{Type: core.EventToolCall, Tool: &core.ToolCall{ID: "call-1", Name: tool.Spec().Name, Arguments: json.RawMessage(`{}`)}},
				{Type: core.EventUsage, Use: &core.Usage{InputTokens: 950, OutputTokens: 1}},
			}}, nil
		default:
			return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "after post-response compaction"}}}, nil
		}
	})
	e := newLifecycleTestEngine(t, model, 1000, provider, registry)
	stream, err := e.Stream(context.Background(), core.Request{
		Model: model,
		Messages: []core.Message{
			core.SystemText("system"),
			core.UserText("use the tool"),
			core.AssistantText("prior answer"),
			core.UserText("continue"),
		},
		Tools: registry.AllSpecs(),
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var events []core.Event
	drainLifecycleEvents(t, stream, &events)

	assertCompactionStatuses(t, events, []compactionStatusWant{{active: true}, {active: false}})
	boundary := eventIndex(events, func(event core.Event) bool { return event.Type == core.EventCompaction })
	idle := eventIndex(events, func(event core.Event) bool {
		return event.Type == core.EventCompactionStatus && !event.CompactionActive
	})
	if boundary < 0 || idle < boundary {
		t.Fatalf("idle status must follow the post-response boundary: events=%v", eventTypes(events))
	}
	if tool.calls.Load() != 1 {
		t.Fatalf("tool calls = %d, want the turn to continue past compaction", tool.calls.Load())
	}
	if got := visibleLifecycleText(events); got != "after post-response compaction" {
		t.Fatalf("visible text = %q", got)
	}
}

// Post-response compaction can also trigger from the soft threshold on a text
// reply; it must use the same single begin/end pair as the tool-call branch.
func TestCompactionLifecyclePostResponseText(t *testing.T) {
	const model = "lifecycle-post-response-text"
	provider := newLifecycleProvider(func(ctx context.Context, call int, req core.Request) (core.Stream, error) {
		if req.Ephemeral {
			return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "post-response text summary"}}}, nil
		}
		return &sliceStream{events: []core.Event{
			{Type: core.EventTextDelta, Text: "final answer"},
			{Type: core.EventUsage, Use: &core.Usage{InputTokens: 920, OutputTokens: 1}},
		}}, nil
	})
	e := newLifecycleTestEngine(t, model, 1000, provider, nil)
	stream, err := e.Stream(context.Background(), core.Request{
		Model: model,
		Messages: []core.Message{
			core.SystemText("system"),
			core.UserText("answer the question"),
			core.AssistantText("prior answer"),
			core.UserText("continue"),
		},
		Tools: []core.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var events []core.Event
	drainLifecycleEvents(t, stream, &events)

	assertCompactionStatuses(t, events, []compactionStatusWant{{active: true}, {active: false}})
	boundary := eventIndex(events, func(event core.Event) bool { return event.Type == core.EventCompaction })
	idle := eventIndex(events, func(event core.Event) bool {
		return event.Type == core.EventCompactionStatus && !event.CompactionActive
	})
	if boundary < 0 || idle < boundary {
		t.Fatalf("idle status must follow the post-response text boundary: events=%v", eventTypes(events))
	}
	if got := visibleLifecycleText(events); got != "final answer" {
		t.Fatalf("visible text = %q", got)
	}
}

// Ordinary runs below the thresholds must not emit spurious status events.
func TestCompactionLifecycleIdleRunEmitsNoStatus(t *testing.T) {
	const model = "lifecycle-idle"
	provider := newLifecycleProvider(func(ctx context.Context, call int, req core.Request) (core.Stream, error) {
		return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "no compaction"}}}, nil
	})
	e := newLifecycleTestEngine(t, model, 100000, provider, nil)
	stream, err := e.Stream(context.Background(), core.Request{
		Model: model,
		Messages: []core.Message{
			core.SystemText("system"),
			core.UserText("small task"),
		},
		Tools: []core.ToolSpec{{Name: "dummy", Schema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	var events []core.Event
	drainLifecycleEvents(t, stream, &events)
	assertCompactionStatuses(t, events, nil)
}

// The tracker only crosses the idle/active wire boundary: nested spans stay
// active, finish closes every remaining span, and extra ends are inert.
func TestCompactionLifecycleTrackerBalancesNestedSpans(t *testing.T) {
	events := make(chan core.Event, 8)
	lifecycle := newCompactionLifecycle(corestream.NewSender(context.Background(), events))
	lifecycle.begin()
	lifecycle.begin()
	lifecycle.end(nil)
	lifecycle.finish(context.Canceled)
	lifecycle.end(nil)
	lifecycle.finish(nil)
	close(events)

	var collected []core.Event
	for event := range events {
		collected = append(collected, event)
	}
	assertCompactionStatuses(t, collected, []compactionStatusWant{{active: true}, {active: false, errContains: "context canceled"}})
}
