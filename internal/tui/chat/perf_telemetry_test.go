package chat

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestLoadStreamPerfConfigFromEnv(t *testing.T) {
	cfg := loadStreamPerfConfigFromEnv(func(key string) string {
		switch key {
		case streamPerfEnvSummary:
			return "1"
		case streamPerfEnvTrace:
			return "true"
		default:
			return ""
		}
	})

	if !cfg.enabled {
		t.Fatalf("expected enabled=true")
	}
	if !cfg.trace {
		t.Fatalf("expected trace=true")
	}

	disabled := loadStreamPerfConfigFromEnv(func(key string) string {
		switch key {
		case streamPerfEnvSummary:
			return "0"
		case streamPerfEnvTrace:
			return "false"
		default:
			return ""
		}
	})

	if disabled.enabled {
		t.Fatalf("expected enabled=false")
	}
	if disabled.trace {
		t.Fatalf("expected trace=false")
	}
}

func TestStreamPerfTelemetryAggregatesDurationsAndBacklog(t *testing.T) {
	var out bytes.Buffer
	telemetry := newStreamPerfTelemetry(streamPerfConfig{enabled: true}, &out)

	start := time.Unix(1700000000, 0)
	telemetry.StartTurn("sess-1", start)

	telemetry.RecordTextEvent(120)
	telemetry.RecordSmoothTickScheduled()
	telemetry.RecordSmoothTickScheduled()
	telemetry.RecordSmoothTickHandled(true, 90)

	telemetry.RecordFrameAt(start)
	telemetry.RecordFrameAt(start.Add(100 * time.Millisecond))
	telemetry.RecordFrameAt(start.Add(380 * time.Millisecond))

	telemetry.RecordDuration(durationMetricStreamEvent, 10*time.Millisecond)
	telemetry.RecordDuration(durationMetricStreamEvent, 20*time.Millisecond)
	telemetry.RecordDuration(durationMetricStreamEvent, 30*time.Millisecond)
	telemetry.RecordDuration(durationMetricSetContent, 7*time.Millisecond)

	summary := telemetry.SnapshotAt(start.Add(2 * time.Second))

	if summary.TextEvents != 1 {
		t.Fatalf("TextEvents=%d, want 1", summary.TextEvents)
	}
	if summary.SmoothTicksScheduled != 2 {
		t.Fatalf("SmoothTicksScheduled=%d, want 2", summary.SmoothTicksScheduled)
	}
	if summary.SmoothTicksHandled != 1 {
		t.Fatalf("SmoothTicksHandled=%d, want 1", summary.SmoothTicksHandled)
	}
	if summary.MaxSmoothTickBacklog != 2 {
		t.Fatalf("MaxSmoothTickBacklog=%d, want 2", summary.MaxSmoothTickBacklog)
	}
	if summary.MaxViewInterval != 280*time.Millisecond {
		t.Fatalf("MaxViewInterval=%v, want %v", summary.MaxViewInterval, 280*time.Millisecond)
	}
	if summary.StreamEventDurations.P50 != 20*time.Millisecond {
		t.Fatalf("StreamEventDurations.P50=%v, want %v", summary.StreamEventDurations.P50, 20*time.Millisecond)
	}
	if summary.StreamEventDurations.P95 != 30*time.Millisecond {
		t.Fatalf("StreamEventDurations.P95=%v, want %v", summary.StreamEventDurations.P95, 30*time.Millisecond)
	}
}

func TestModelStreamPerfSummaryEmitsOnceOnDone(t *testing.T) {
	model := newTestChatModel(false)

	var out bytes.Buffer
	model.streamPerf = newStreamPerfTelemetry(streamPerfConfig{enabled: true}, &out)
	model.streamPerf.StartTurn("sess-test", time.Now())

	model.streaming = true
	_, _ = model.Update(streamEventMsg{event: ui.TextEvent("hello world")})
	_, _ = model.Update(ui.SmoothTickMsg{})
	_, _ = model.Update(streamEventMsg{event: ui.DoneEvent(42)})
	_, _ = model.Update(streamEventMsg{event: ui.DoneEvent(0)})

	got := out.String()
	if strings.Count(got, "[stream-perf]") != 1 {
		t.Fatalf("expected one summary emission, got output: %q", got)
	}
}

func TestModelCoalescesSmoothTickSchedulingForBurstTextEvents(t *testing.T) {
	model := newTestChatModel(false)

	var out bytes.Buffer
	model.streamPerf = newStreamPerfTelemetry(streamPerfConfig{enabled: true}, &out)
	model.streamPerf.StartTurn("sess-test", time.Now())
	model.streaming = true

	_, _ = model.Update(streamEventMsg{event: ui.TextEvent("hello")})
	_, _ = model.Update(streamEventMsg{event: ui.TextEvent(" world")})

	if model.streamPerf.smoothTicksScheduled != 1 {
		t.Fatalf("expected exactly one scheduled smooth tick before first tick fires, got %d", model.streamPerf.smoothTicksScheduled)
	}
}

func TestViewAltScreenThrottlesSetContentDuringStreaming(t *testing.T) {
	model := newTestChatModel(true)

	model.streaming = true
	model.streamRenderMinInterval = time.Hour

	// Prime first render.
	model.tracker.AddTextSegment("first", model.width)
	_ = model.View()
	firstRenderedVersion := model.viewCache.lastRenderedVersion

	// New streaming content arrives immediately after.
	model.tracker.AddTextSegment(" second", model.width)
	_ = model.View()

	if model.viewCache.lastRenderedVersion != firstRenderedVersion {
		t.Fatalf("expected lastRenderedVersion to remain %d when throttled, got %d", firstRenderedVersion, model.viewCache.lastRenderedVersion)
	}
}

func TestModelSchedulesRenderTickWhenThrottledContentPending(t *testing.T) {
	model := newTestChatModel(true)
	model.streaming = true
	model.streamRenderMinInterval = time.Second
	model.viewCache.lastSetContentAt = time.Now()
	model.viewCache.contentVersion = 2
	model.viewCache.lastRenderedVersion = 1

	cmd := model.maybeScheduleStreamRenderTick()
	if cmd == nil {
		t.Fatal("expected a render tick command when throttled content is pending")
	}
	if !model.streamRenderTickPending {
		t.Fatal("expected streamRenderTickPending=true after scheduling render tick")
	}
}

func newTestChatModel(altScreen bool) *Model {
	provider := llm.NewMockProvider("mock")
	engine := llm.NewEngine(provider, nil)

	return New(
		&config.Config{DefaultProvider: "mock"},
		provider,
		engine,
		"mock-model",
		nil,
		20,
		false,
		false,
		nil,
		"",
		"",
		false,
		"",
		nil,
		nil,
		altScreen,
		nil,
		false,
		false,
		"",
	)
}
