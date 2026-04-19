package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/ui"
)

func defaultTestSessionInfo() sessionInfo {
	return sessionInfo{
		SessionID: "sess-123",
		Provider:  "mock",
		Model:     "mock-model",
		Agent:     "",
		Tools:     "shell,read_file",
		MCP:       "",
		Yolo:      false,
		Search:    true,
		Resuming:  false,
	}
}

// captureStreamJSONOutput pipes events into streamJSON and returns stdout bytes.
func captureStreamJSONOutput(t *testing.T, events []ui.StreamEvent, info sessionInfo) string {
	t.Helper()

	ch := make(chan ui.StreamEvent, len(events)+1)
	for _, ev := range events {
		ch <- ev
	}
	close(ch)

	var buf bytes.Buffer
	emitter := newJSONEmitter(&buf)
	stats := ui.NewSessionStats()

	if err := streamJSON(context.Background(), ch, emitter, stats, info); err != nil {
		t.Fatalf("streamJSON returned error: %v", err)
	}
	return buf.String()
}

// parseJSONL splits stdout into decoded events. Trailing empty lines are
// ignored. Fails the test if any line is not valid JSON.
func parseJSONL(t *testing.T, out string) []map[string]any {
	t.Helper()

	var events []map[string]any
	for i, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\nline: %q", i, err, line)
		}
		events = append(events, obj)
	}
	return events
}

func TestStreamJSON_SessionStartedIsFirstEventWithExpectedFields(t *testing.T) {
	out := captureStreamJSONOutput(t, []ui.StreamEvent{
		ui.DoneEvent(0),
	}, sessionInfo{
		SessionID: "abc",
		Provider:  "anthropic",
		Model:     "claude",
		Agent:     "reviewer",
		Tools:     "shell",
		MCP:       "my-mcp",
		Yolo:      true,
		Search:    false,
		Resuming:  true,
	})

	events := parseJSONL(t, out)
	if len(events) == 0 {
		t.Fatalf("expected at least one event, got 0")
	}
	first := events[0]
	if first["type"] != "session.started" {
		t.Fatalf("expected first event type = session.started, got %v", first["type"])
	}
	if first["session_id"] != "abc" {
		t.Errorf("session_id = %v", first["session_id"])
	}
	if first["provider"] != "anthropic" {
		t.Errorf("provider = %v", first["provider"])
	}
	if first["model"] != "claude" {
		t.Errorf("model = %v", first["model"])
	}
	if first["agent"] != "reviewer" {
		t.Errorf("agent = %v", first["agent"])
	}
	if first["tools"] != "shell" {
		t.Errorf("tools = %v", first["tools"])
	}
	if first["mcp"] != "my-mcp" {
		t.Errorf("mcp = %v", first["mcp"])
	}
	if first["yolo"] != true {
		t.Errorf("yolo = %v", first["yolo"])
	}
	if first["search"] != false {
		t.Errorf("search = %v", first["search"])
	}
	if first["resuming"] != true {
		t.Errorf("resuming = %v", first["resuming"])
	}
	if _, ok := first["seq"]; !ok {
		t.Error("missing seq")
	}
	if _, ok := first["ts"]; !ok {
		t.Error("missing ts")
	}
}

func TestStreamJSON_SessionStartedOmitsEmptyAgentAsNull(t *testing.T) {
	out := captureStreamJSONOutput(t, []ui.StreamEvent{ui.DoneEvent(0)}, defaultTestSessionInfo())
	events := parseJSONL(t, out)
	if events[0]["agent"] != nil {
		t.Fatalf("expected agent null for empty string, got %v", events[0]["agent"])
	}
}

func TestStreamJSON_TextEventProducesTextDelta(t *testing.T) {
	out := captureStreamJSONOutput(t, []ui.StreamEvent{
		ui.TextEvent("hello"),
		ui.TextEvent(" world"),
		ui.DoneEvent(0),
	}, defaultTestSessionInfo())

	events := parseJSONL(t, out)
	deltas := 0
	var collected string
	for _, ev := range events {
		if ev["type"] == "text.delta" {
			deltas++
			collected += ev["text"].(string)
		}
	}
	if deltas != 2 {
		t.Errorf("expected 2 text.delta events, got %d", deltas)
	}
	if collected != "hello world" {
		t.Errorf("concatenated text = %q", collected)
	}
}

func TestStreamJSON_ToolStartAndCompletedEvents(t *testing.T) {
	args := json.RawMessage(`{"path":"/etc/hosts"}`)
	out := captureStreamJSONOutput(t, []ui.StreamEvent{
		ui.ToolStartEvent("call-1", "read_file", "(hosts)", args),
		ui.ToolEndEvent("call-1", "read_file", "(hosts)", true),
		ui.DoneEvent(0),
	}, defaultTestSessionInfo())

	events := parseJSONL(t, out)
	var started, completed map[string]any
	for _, ev := range events {
		switch ev["type"] {
		case "tool.started":
			started = ev
		case "tool.completed":
			completed = ev
		}
	}
	if started == nil {
		t.Fatal("missing tool.started event")
	}
	if started["call_id"] != "call-1" {
		t.Errorf("tool.started.call_id = %v", started["call_id"])
	}
	if started["name"] != "read_file" {
		t.Errorf("tool.started.name = %v", started["name"])
	}
	if started["info"] != "(hosts)" {
		t.Errorf("tool.started.info = %v", started["info"])
	}
	argsObj, ok := started["args"].(map[string]any)
	if !ok {
		t.Fatalf("expected args to decode as object, got %T: %v", started["args"], started["args"])
	}
	if argsObj["path"] != "/etc/hosts" {
		t.Errorf("tool.started.args.path = %v", argsObj["path"])
	}
	if completed == nil {
		t.Fatal("missing tool.completed event")
	}
	if completed["call_id"] != "call-1" {
		t.Errorf("tool.completed.call_id = %v", completed["call_id"])
	}
	if completed["success"] != true {
		t.Errorf("tool.completed.success = %v", completed["success"])
	}
}

func TestStreamJSON_ToolStartWithNilArgsEmitsNull(t *testing.T) {
	out := captureStreamJSONOutput(t, []ui.StreamEvent{
		ui.ToolStartEvent("c1", "shell", "(echo hi)", nil),
		ui.ToolEndEvent("c1", "shell", "(echo hi)", true),
		ui.DoneEvent(0),
	}, defaultTestSessionInfo())

	events := parseJSONL(t, out)
	for _, ev := range events {
		if ev["type"] == "tool.started" {
			if _, ok := ev["args"]; !ok {
				t.Fatal("args key missing from tool.started (should be present as null)")
			}
			if ev["args"] != nil {
				t.Fatalf("expected args=null, got %v", ev["args"])
			}
			return
		}
	}
	t.Fatal("tool.started event not found")
}

func TestStreamJSON_UsageEventAllFourTokenCounts(t *testing.T) {
	out := captureStreamJSONOutput(t, []ui.StreamEvent{
		ui.UsageEvent(100, 50, 20, 5),
		ui.DoneEvent(0),
	}, defaultTestSessionInfo())

	events := parseJSONL(t, out)
	var usage map[string]any
	for _, ev := range events {
		if ev["type"] == "usage" {
			usage = ev
			break
		}
	}
	if usage == nil {
		t.Fatal("missing usage event")
	}
	if usage["input_tokens"].(float64) != 100 {
		t.Errorf("input_tokens = %v", usage["input_tokens"])
	}
	if usage["output_tokens"].(float64) != 50 {
		t.Errorf("output_tokens = %v", usage["output_tokens"])
	}
	if usage["cached_input_tokens"].(float64) != 20 {
		t.Errorf("cached_input_tokens = %v", usage["cached_input_tokens"])
	}
	if usage["cache_write_tokens"].(float64) != 5 {
		t.Errorf("cache_write_tokens = %v", usage["cache_write_tokens"])
	}
}

func TestStreamJSON_RetryEvent(t *testing.T) {
	out := captureStreamJSONOutput(t, []ui.StreamEvent{
		ui.RetryEvent(2, 5, 3.5),
		ui.DoneEvent(0),
	}, defaultTestSessionInfo())

	events := parseJSONL(t, out)
	var retry map[string]any
	for _, ev := range events {
		if ev["type"] == "retry" {
			retry = ev
			break
		}
	}
	if retry == nil {
		t.Fatal("missing retry event")
	}
	if retry["attempt"].(float64) != 2 {
		t.Errorf("attempt = %v", retry["attempt"])
	}
	if retry["max"].(float64) != 5 {
		t.Errorf("max = %v", retry["max"])
	}
	if retry["wait_seconds"].(float64) != 3.5 {
		t.Errorf("wait_seconds = %v", retry["wait_seconds"])
	}
}

func TestStreamJSON_FinalTwoEventsAreStatsThenDone(t *testing.T) {
	out := captureStreamJSONOutput(t, []ui.StreamEvent{
		ui.TextEvent("hello"),
		ui.DoneEvent(42),
	}, defaultTestSessionInfo())

	events := parseJSONL(t, out)
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(events))
	}
	last := events[len(events)-1]
	second := events[len(events)-2]
	if second["type"] != "stats" {
		t.Fatalf("expected second-to-last = stats, got %v", second["type"])
	}
	if last["type"] != "done" {
		t.Fatalf("expected last = done, got %v", last["type"])
	}
	if last["tokens"].(float64) != 42 {
		t.Errorf("done.tokens = %v", last["tokens"])
	}
	// stats has all token/count fields
	for _, k := range []string{"duration_ms", "llm_ms", "tool_ms", "input_tokens", "output_tokens", "cached_input_tokens", "cache_write_tokens", "tool_calls", "llm_calls"} {
		if _, ok := second[k]; !ok {
			t.Errorf("missing stats field %q", k)
		}
	}
}

func TestStreamJSON_SeqIsStrictlyMonotonic(t *testing.T) {
	out := captureStreamJSONOutput(t, []ui.StreamEvent{
		ui.TextEvent("a"),
		ui.TextEvent("b"),
		ui.UsageEvent(1, 1, 0, 0),
		ui.DoneEvent(0),
	}, defaultTestSessionInfo())

	events := parseJSONL(t, out)
	var prev float64 = -1
	for i, ev := range events {
		seq, ok := ev["seq"].(float64)
		if !ok {
			t.Fatalf("event %d missing seq: %v", i, ev)
		}
		if seq <= prev {
			t.Fatalf("seq not strictly monotonic at event %d (type=%v): prev=%v, current=%v", i, ev["type"], prev, seq)
		}
		prev = seq
	}
	if events[0]["seq"].(float64) != 0 {
		t.Errorf("expected first seq = 0, got %v", events[0]["seq"])
	}
}

func TestStreamJSON_ContextCanceledStillEmitsStatsAndDone(t *testing.T) {
	ch := make(chan ui.StreamEvent) // unbuffered, will block
	ctx, cancel := context.WithCancel(context.Background())

	// signalWriter wraps a buffer and signals once the first write
	// completes so we can cancel after session.started is flushed
	// without relying on timing.
	firstWrite := make(chan struct{}, 1)
	var buf bytes.Buffer
	sw := &signalWriter{w: &buf, ready: firstWrite}
	emitter := newJSONEmitter(sw)
	stats := ui.NewSessionStats()

	done := make(chan error, 1)
	go func() {
		done <- streamJSON(ctx, ch, emitter, stats, defaultTestSessionInfo())
	}()

	<-firstWrite
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("streamJSON returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("streamJSON did not return within 1s after ctx cancel")
	}

	events := parseJSONL(t, buf.String())
	if len(events) < 3 {
		t.Fatalf("expected at least session.started, stats, done, got %d events: %s", len(events), buf.String())
	}
	if events[0]["type"] != "session.started" {
		t.Errorf("first event = %v", events[0]["type"])
	}
	if events[len(events)-2]["type"] != "stats" {
		t.Errorf("second-to-last = %v", events[len(events)-2]["type"])
	}
	if events[len(events)-1]["type"] != "done" {
		t.Errorf("last = %v", events[len(events)-1]["type"])
	}
}

func TestStreamJSON_ErrorEventReturnsError(t *testing.T) {
	ch := make(chan ui.StreamEvent, 3)
	ch <- ui.TextEvent("hi")
	ch <- ui.ErrorEvent(errors.New("boom"))
	close(ch)

	var buf bytes.Buffer
	emitter := newJSONEmitter(&buf)
	stats := ui.NewSessionStats()

	err := streamJSON(context.Background(), ch, emitter, stats, defaultTestSessionInfo())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected wrapped boom error, got %v", err)
	}

	events := parseJSONL(t, buf.String())
	var sawError bool
	for _, ev := range events {
		if ev["type"] == "error" {
			if ev["message"] != "boom" {
				t.Errorf("error.message = %v", ev["message"])
			}
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("expected error event in output")
	}
	// Still terminates with stats + done.
	if events[len(events)-2]["type"] != "stats" {
		t.Errorf("second-to-last = %v", events[len(events)-2]["type"])
	}
	if events[len(events)-1]["type"] != "done" {
		t.Errorf("last = %v", events[len(events)-1]["type"])
	}
}

func TestStreamJSON_ImageAndDiffAndPhaseEvents(t *testing.T) {
	out := captureStreamJSONOutput(t, []ui.StreamEvent{
		ui.PhaseEvent("WARNING: truncated"),
		ui.ImageEvent("/tmp/out.png"),
		ui.DiffEvent("foo.go", "old", "new", 42),
		ui.DoneEvent(0),
	}, defaultTestSessionInfo())

	events := parseJSONL(t, out)
	seen := map[string]map[string]any{}
	for _, ev := range events {
		typ, _ := ev["type"].(string)
		seen[typ] = ev
	}
	if seen["phase"]["phase"] != "WARNING: truncated" {
		t.Errorf("phase event = %v", seen["phase"])
	}
	if seen["image"]["path"] != "/tmp/out.png" {
		t.Errorf("image event = %v", seen["image"])
	}
	diff := seen["diff"]
	if diff == nil {
		t.Fatal("missing diff event")
	}
	if diff["path"] != "foo.go" || diff["old"] != "old" || diff["new"] != "new" || diff["line"].(float64) != 42 {
		t.Errorf("diff event = %v", diff)
	}
}

func TestEmitProgressiveResult_OmitsEmptyOptionals(t *testing.T) {
	var buf bytes.Buffer
	emitter := newJSONEmitter(&buf)
	err := emitProgressiveResult(emitter, progressiveRunResult{
		ExitReason: "natural",
		Finalized:  true,
	})
	if err != nil {
		t.Fatalf("emitProgressiveResult error: %v", err)
	}

	events := parseJSONL(t, buf.String())
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev["type"] != "progressive.result" {
		t.Errorf("type = %v", ev["type"])
	}
	if ev["exit_reason"] != "natural" {
		t.Errorf("exit_reason = %v", ev["exit_reason"])
	}
	if ev["finalized"] != true {
		t.Errorf("finalized = %v", ev["finalized"])
	}
	for _, k := range []string{"session_id", "sequence", "reason", "message", "progress", "final_response", "fallback_text"} {
		if _, ok := ev[k]; ok {
			t.Errorf("expected %q to be omitted, got %v", k, ev[k])
		}
	}
}

// signalWriter wraps an io.Writer and sends (non-blocking) on ready
// after each successful write. Used in tests to sync on "first flush"
// without relying on time.Sleep.
type signalWriter struct {
	w     io.Writer
	ready chan<- struct{}
}

func (s *signalWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	select {
	case s.ready <- struct{}{}:
	default:
	}
	return n, err
}

// TestStreamJSON_StreamErrorReturnsTerminalFlushedSentinel verifies that
// a ui.StreamEventError causes streamJSON to return a terminalFlushedError
// — signaling to runAsk that terminal events (error, stats, done) are
// already written and must not be duplicated via emitFatalError.
func TestStreamJSON_StreamErrorReturnsTerminalFlushedSentinel(t *testing.T) {
	ch := make(chan ui.StreamEvent, 2)
	ch <- ui.ErrorEvent(errors.New("boom"))
	close(ch)

	var buf bytes.Buffer
	emitter := newJSONEmitter(&buf)
	stats := ui.NewSessionStats()

	err := streamJSON(context.Background(), ch, emitter, stats, defaultTestSessionInfo())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isTerminalFlushed(err) {
		t.Fatalf("expected terminalFlushedError, got %T: %v", err, err)
	}
	if errors.Unwrap(err).Error() != "boom" {
		t.Errorf("unwrapped error = %q, want %q", errors.Unwrap(err).Error(), "boom")
	}

	events := parseJSONL(t, buf.String())
	var types []string
	for _, ev := range events {
		types = append(types, ev["type"].(string))
	}
	want := []string{"session.started", "error", "stats", "done"}
	if len(types) != len(want) {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	for i, w := range want {
		if types[i] != w {
			t.Errorf("event %d = %q, want %q", i, types[i], w)
		}
	}
}

// TestStreamJSON_StreamErrorPathDoesNotDuplicateTerminalEvents emulates
// the runAsk error-handling flow: if streamJSON returns an error that
// already reports terminal events flushed, emitFatalError must be
// skipped so the output has exactly one stats+done tail.
func TestStreamJSON_StreamErrorPathDoesNotDuplicateTerminalEvents(t *testing.T) {
	ch := make(chan ui.StreamEvent, 2)
	ch <- ui.ErrorEvent(errors.New("boom"))
	close(ch)

	var buf bytes.Buffer
	emitter := newJSONEmitter(&buf)
	stats := ui.NewSessionStats()

	err := streamJSON(context.Background(), ch, emitter, stats, defaultTestSessionInfo())
	if err != nil && !isTerminalFlushed(err) {
		_ = emitFatalError(emitter, stats, err)
	}

	events := parseJSONL(t, buf.String())
	var statsCount, doneCount, errorCount int
	for _, ev := range events {
		switch ev["type"] {
		case "stats":
			statsCount++
		case "done":
			doneCount++
		case "error":
			errorCount++
		}
	}
	if statsCount != 1 {
		t.Errorf("stats events = %d, want 1", statsCount)
	}
	if doneCount != 1 {
		t.Errorf("done events = %d, want 1", doneCount)
	}
	if errorCount != 1 {
		t.Errorf("error events = %d, want 1", errorCount)
	}

	if events[len(events)-2]["type"] != "stats" || events[len(events)-1]["type"] != "done" {
		t.Errorf("last two events = %v,%v; want stats,done", events[len(events)-2]["type"], events[len(events)-1]["type"])
	}
}

// TestJSONEmitter_SeqMonotonicUnderConcurrency verifies that when many
// goroutines call emit concurrently, seq matches output order. Relies
// on seq assignment + write happening inside a single mutex.
func TestJSONEmitter_SeqMonotonicUnderConcurrency(t *testing.T) {
	var buf bytes.Buffer
	e := newJSONEmitter(&buf)

	const n = 500
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = e.emit("text.delta", map[string]any{"text": "x"})
		}()
	}
	wg.Wait()

	events := parseJSONL(t, buf.String())
	if len(events) != n {
		t.Fatalf("got %d events, want %d", len(events), n)
	}
	for i, ev := range events {
		got, ok := ev["seq"].(float64)
		if !ok || int(got) != i {
			t.Fatalf("event %d seq = %v, want %d (out-of-order write)", i, ev["seq"], i)
		}
	}
}
