package chat

import (
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"

	"github.com/samsaffron/term-llm/internal/ui"
)

const (
	streamPerfEnvSummary = "TERM_LLM_DEBUG_STREAM_PERF"
	streamPerfEnvTrace   = "TERM_LLM_DEBUG_STREAM_TRACE"
)

type streamPerfConfig struct {
	enabled bool
	trace   bool
}

func loadStreamPerfConfigFromEnv(getenv func(string) string) streamPerfConfig {
	enabled := ui.ParseBoolDefault(getenv(streamPerfEnvSummary), false)
	trace := ui.ParseBoolDefault(getenv(streamPerfEnvTrace), false)
	if trace {
		enabled = true
	}
	return streamPerfConfig{
		enabled: enabled,
		trace:   trace,
	}
}

type durationMetric string

const (
	durationMetricStreamEvent  durationMetric = "stream_event"
	durationMetricSmoothTick   durationMetric = "smooth_tick"
	durationMetricSetContent   durationMetric = "set_content"
	durationMetricViewportView durationMetric = "viewport_view"
)

type durationCollector struct {
	// Diagnostic mode keeps all samples for accurate percentile reporting.
	// This grows with stream length by design.
	samplesMicros []int64
	totalMicros   int64
	maxMicros     int64
}

type durationSummary struct {
	Count int
	Total time.Duration
	Mean  time.Duration
	P50   time.Duration
	P95   time.Duration
	Max   time.Duration
}

func (c *durationCollector) Add(d time.Duration) {
	if d < 0 {
		return
	}
	micros := d.Microseconds()
	c.samplesMicros = append(c.samplesMicros, micros)
	c.totalMicros += micros
	if micros > c.maxMicros {
		c.maxMicros = micros
	}
}

func (c durationCollector) Summary() durationSummary {
	if len(c.samplesMicros) == 0 {
		return durationSummary{}
	}

	sorted := append([]int64(nil), c.samplesMicros...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i] < sorted[j]
	})
	p50 := percentileFromSortedMicros(sorted, 0.50)
	p95 := percentileFromSortedMicros(sorted, 0.95)
	mean := c.totalMicros / int64(len(c.samplesMicros))

	return durationSummary{
		Count: len(c.samplesMicros),
		Total: time.Duration(c.totalMicros) * time.Microsecond,
		Mean:  time.Duration(mean) * time.Microsecond,
		P50:   time.Duration(p50) * time.Microsecond,
		P95:   time.Duration(p95) * time.Microsecond,
		Max:   time.Duration(c.maxMicros) * time.Microsecond,
	}
}

func percentileFromSortedMicros(sorted []int64, pct float64) int64 {
	if len(sorted) == 0 {
		return 0
	}

	if pct <= 0 {
		return sorted[0]
	}
	if pct >= 1 {
		return sorted[len(sorted)-1]
	}

	rank := int(math.Ceil(pct*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

type streamPerfSnapshot struct {
	TurnSequence int
	SessionID    string
	StartedAt    time.Time
	EndedAt      time.Time
	Duration     time.Duration

	TextEvents int
	TextBytes  int

	SmoothTicksScheduled int
	SmoothTicksHandled   int
	SmoothTicksWithText  int
	MaxSmoothTickBacklog int
	MaxSmoothBufferBytes int

	TrackerVersionBumps int
	ContentVersionBumps int
	MaxViewInterval     time.Duration

	StreamEventDurations durationSummary
	SmoothTickDurations  durationSummary
	SetContentDurations  durationSummary
	ViewportDurations    durationSummary
}

type streamPerfTelemetry struct {
	cfg streamPerfConfig
	out io.Writer

	active       bool
	turnSequence int
	sessionID    string
	startedAt    time.Time
	lastFrameAt  time.Time

	textEvents int
	textBytes  int

	smoothTicksScheduled int
	smoothTicksHandled   int
	smoothTicksWithText  int
	maxSmoothTickBacklog int
	maxSmoothBufferBytes int

	trackerVersionBumps int
	contentVersionBumps int
	lastTrackerVersion  uint64
	hasTrackerVersion   bool

	maxViewInterval time.Duration

	streamEventDurations durationCollector
	smoothTickDurations  durationCollector
	setContentDurations  durationCollector
	viewportDurations    durationCollector
}

func newStreamPerfTelemetry(cfg streamPerfConfig, out io.Writer) *streamPerfTelemetry {
	if out == nil {
		out = io.Discard
	}
	return &streamPerfTelemetry{
		cfg:                  cfg,
		out:                  out,
		streamEventDurations: newDurationCollector(),
		smoothTickDurations:  newDurationCollector(),
		setContentDurations:  newDurationCollector(),
		viewportDurations:    newDurationCollector(),
	}
}

func newStreamPerfTelemetryFromEnv() *streamPerfTelemetry {
	cfg := loadStreamPerfConfigFromEnv(os.Getenv)
	if !cfg.enabled {
		return nil
	}
	return newStreamPerfTelemetry(cfg, os.Stderr)
}

func (t *streamPerfTelemetry) Enabled() bool {
	return t != nil && t.cfg.enabled
}

func (t *streamPerfTelemetry) Active() bool {
	return t.Enabled() && t.active
}

func (t *streamPerfTelemetry) StartTurn(sessionID string, startedAt time.Time) {
	if !t.Enabled() {
		return
	}
	t.turnSequence++
	t.active = true
	t.sessionID = sessionID
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	t.startedAt = startedAt
	t.lastFrameAt = time.Time{}

	t.textEvents = 0
	t.textBytes = 0
	t.smoothTicksScheduled = 0
	t.smoothTicksHandled = 0
	t.smoothTicksWithText = 0
	t.maxSmoothTickBacklog = 0
	t.maxSmoothBufferBytes = 0
	t.trackerVersionBumps = 0
	t.contentVersionBumps = 0
	t.lastTrackerVersion = 0
	t.hasTrackerVersion = false
	t.maxViewInterval = 0

	t.streamEventDurations = newDurationCollector()
	t.smoothTickDurations = newDurationCollector()
	t.setContentDurations = newDurationCollector()
	t.viewportDurations = newDurationCollector()
}

func (t *streamPerfTelemetry) RecordFrameAt(ts time.Time) {
	if !t.Active() {
		return
	}
	if t.lastFrameAt.IsZero() {
		t.lastFrameAt = ts
		return
	}
	gap := ts.Sub(t.lastFrameAt)
	if gap > t.maxViewInterval {
		t.maxViewInterval = gap
	}
	t.lastFrameAt = ts
}

func (t *streamPerfTelemetry) RecordTextEvent(bufferLen int) {
	if !t.Active() {
		return
	}
	t.textEvents++
	if bufferLen > t.maxSmoothBufferBytes {
		t.maxSmoothBufferBytes = bufferLen
	}
}

func (t *streamPerfTelemetry) RecordTextDelta(text string, bufferLen int) {
	if !t.Active() {
		return
	}
	t.RecordTextEvent(bufferLen)
	t.textBytes += len(text)
	if t.cfg.trace {
		t.tracef("text_event events=%d bytes=%d smooth_buffer=%d", t.textEvents, t.textBytes, bufferLen)
	}
}

func (t *streamPerfTelemetry) RecordSmoothTickScheduled() {
	if !t.Active() {
		return
	}
	t.smoothTicksScheduled++
	backlog := t.smoothTicksScheduled - t.smoothTicksHandled
	if backlog > t.maxSmoothTickBacklog {
		t.maxSmoothTickBacklog = backlog
	}
}

func (t *streamPerfTelemetry) RecordSmoothTickHandled(withText bool, bufferLen int) {
	if !t.Active() {
		return
	}
	t.smoothTicksHandled++
	if withText {
		t.smoothTicksWithText++
	}
	if bufferLen > t.maxSmoothBufferBytes {
		t.maxSmoothBufferBytes = bufferLen
	}
	if t.cfg.trace {
		backlog := t.smoothTicksScheduled - t.smoothTicksHandled
		if backlog < 0 {
			backlog = 0
		}
		t.tracef("smooth_tick handled=%d/%d with_text=%t backlog=%d smooth_buffer=%d",
			t.smoothTicksHandled, t.smoothTicksScheduled, withText, backlog, bufferLen)
	}
}

func (t *streamPerfTelemetry) RecordTrackerVersion(version uint64) {
	if !t.Active() {
		return
	}
	if !t.hasTrackerVersion {
		t.lastTrackerVersion = version
		t.hasTrackerVersion = true
		return
	}
	if version > t.lastTrackerVersion {
		t.trackerVersionBumps += int(version - t.lastTrackerVersion)
	}
	t.lastTrackerVersion = version
}

func (t *streamPerfTelemetry) RecordContentVersionBump() {
	if !t.Active() {
		return
	}
	t.contentVersionBumps++
}

func (t *streamPerfTelemetry) RecordDuration(metric durationMetric, d time.Duration) {
	if !t.Active() {
		return
	}
	switch metric {
	case durationMetricStreamEvent:
		t.streamEventDurations.Add(d)
	case durationMetricSmoothTick:
		t.smoothTickDurations.Add(d)
	case durationMetricSetContent:
		t.setContentDurations.Add(d)
	case durationMetricViewportView:
		t.viewportDurations.Add(d)
	}
}

func (t *streamPerfTelemetry) SnapshotAt(endedAt time.Time) streamPerfSnapshot {
	if endedAt.IsZero() {
		endedAt = time.Now()
	}
	duration := endedAt.Sub(t.startedAt)
	if duration < 0 {
		duration = 0
	}
	return streamPerfSnapshot{
		TurnSequence: t.turnSequence,
		SessionID:    t.sessionID,
		StartedAt:    t.startedAt,
		EndedAt:      endedAt,
		Duration:     duration,

		TextEvents: t.textEvents,
		TextBytes:  t.textBytes,

		SmoothTicksScheduled: t.smoothTicksScheduled,
		SmoothTicksHandled:   t.smoothTicksHandled,
		SmoothTicksWithText:  t.smoothTicksWithText,
		MaxSmoothTickBacklog: t.maxSmoothTickBacklog,
		MaxSmoothBufferBytes: t.maxSmoothBufferBytes,

		TrackerVersionBumps: t.trackerVersionBumps,
		ContentVersionBumps: t.contentVersionBumps,
		MaxViewInterval:     t.maxViewInterval,

		StreamEventDurations: t.streamEventDurations.Summary(),
		SmoothTickDurations:  t.smoothTickDurations.Summary(),
		SetContentDurations:  t.setContentDurations.Summary(),
		ViewportDurations:    t.viewportDurations.Summary(),
	}
}

func (t *streamPerfTelemetry) EmitSummaryIfActive(endedAt time.Time) {
	if !t.Active() {
		return
	}

	snapshot := t.SnapshotAt(endedAt)
	fmt.Fprintf(t.out,
		"[stream-perf] turn=%d session=%s duration=%s text_events=%d text_bytes=%d smooth_ticks_scheduled=%d smooth_ticks_handled=%d smooth_tick_text=%d backlog_max=%d smooth_buffer_max=%dB view_interval_max=%s tracker_bumps=%d content_bumps=%d stream_event_p95=%s smooth_tick_p95=%s set_content_p95=%s viewport_p95=%s\n",
		snapshot.TurnSequence,
		snapshot.SessionID,
		snapshot.Duration.Round(time.Millisecond),
		snapshot.TextEvents,
		snapshot.TextBytes,
		snapshot.SmoothTicksScheduled,
		snapshot.SmoothTicksHandled,
		snapshot.SmoothTicksWithText,
		snapshot.MaxSmoothTickBacklog,
		snapshot.MaxSmoothBufferBytes,
		snapshot.MaxViewInterval.Round(time.Millisecond),
		snapshot.TrackerVersionBumps,
		snapshot.ContentVersionBumps,
		snapshot.StreamEventDurations.P95.Round(time.Microsecond),
		snapshot.SmoothTickDurations.P95.Round(time.Microsecond),
		snapshot.SetContentDurations.P95.Round(time.Microsecond),
		snapshot.ViewportDurations.P95.Round(time.Microsecond),
	)

	t.active = false
}

func (t *streamPerfTelemetry) tracef(format string, args ...any) {
	if !t.Active() || !t.cfg.trace {
		return
	}
	fmt.Fprintf(t.out, "[stream-perf-trace] "+format+"\n", args...)
}

func newDurationCollector() durationCollector {
	return durationCollector{
		samplesMicros: make([]int64, 0, 1024),
	}
}
