package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestEncodeTextDeltaPayloadMatchesJSONMarshalForInvalidUTF8(t *testing.T) {
	delta := string([]byte{'o', 'k', 0xff, '!'})
	data, err := encodeTextDeltaPayload(2, delta, 7)
	if err != nil {
		t.Fatalf("encodeTextDeltaPayload() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("encoded payload is invalid JSON: %v; data=%q", err, data)
	}
	if got["delta"] != "ok�!" {
		t.Fatalf("delta = %q, want replacement-char-normalized string", got["delta"])
	}
	if got["output_index"] != float64(2) || got["sequence_number"] != float64(7) {
		t.Fatalf("payload = %#v, want output_index=2 sequence_number=7", got)
	}
}

func TestResponseRunSubscriberSurvivesUpToBufferLimit(t *testing.T) {
	run := newResponseRun("resp_test1", "sess_test", "", "mock", time.Now().Unix(), func() {})

	sub := run.subscribe(0)
	if sub.ch == nil {
		t.Fatal("expected live channel from subscribe")
	}
	defer run.unsubscribe(sub.ch)

	// Fill the subscriber buffer up to the limit (should not drop)
	for i := 0; i < defaultResponseRunSubscriberBuffer; i++ {
		err := run.appendEvent("response.output_text.delta", map[string]any{
			"delta": "x",
		})
		if err != nil {
			t.Fatalf("appendEvent failed at %d: %v", i, err)
		}
	}

	// Subscriber should still be registered
	run.mu.Lock()
	subCount := len(run.subscribers)
	run.mu.Unlock()

	if subCount != 1 {
		t.Fatalf("expected 1 subscriber, got %d", subCount)
	}

	// Drain all events from the channel
	for i := 0; i < defaultResponseRunSubscriberBuffer; i++ {
		select {
		case ev := <-sub.ch:
			if ev.Sequence != int64(i+1) {
				t.Fatalf("expected sequence %d, got %d", i+1, ev.Sequence)
			}
		default:
			t.Fatalf("expected event at index %d but channel was empty", i)
		}
	}
}

func TestResponseRunSubscriberDroppedWhenBufferFull(t *testing.T) {
	run := newResponseRun("resp_test2", "sess_test", "", "mock", time.Now().Unix(), func() {})

	sub := run.subscribe(0)
	if sub.ch == nil {
		t.Fatal("expected live channel from subscribe")
	}
	defer run.unsubscribe(sub.ch)

	// Fill buffer completely
	for i := 0; i < defaultResponseRunSubscriberBuffer; i++ {
		if err := run.appendEvent("response.output_text.delta", map[string]any{"delta": "x"}); err != nil {
			t.Fatalf("appendEvent failed at %d: %v", i, err)
		}
	}

	// One more should drop the subscriber
	if err := run.appendEvent("response.output_text.delta", map[string]any{"delta": "overflow"}); err != nil {
		t.Fatalf("appendEvent failed on overflow: %v", err)
	}

	run.mu.Lock()
	subCount := len(run.subscribers)
	run.mu.Unlock()
	if subCount != 0 {
		t.Fatalf("expected 0 subscribers after overflow, got %d", subCount)
	}
}

func TestResponseRunCompactionKeepsReplayWindowInOrder(t *testing.T) {
	run := newResponseRun("resp_compact", "sess_test", "", "mock", time.Now().Unix(), func() {})
	run.maxRetainedEvents = 3

	for i := 0; i < 8; i++ {
		if err := run.appendTextDeltaEvent(0, ""); err != nil {
			t.Fatalf("appendTextDeltaEvent failed at %d: %v", i, err)
		}
	}

	run.mu.Lock()
	activeLen := len(run.events) - run.eventStart
	storageLen := len(run.events)
	minReplayAfter := run.minReplayAfter
	run.mu.Unlock()

	if activeLen != 3 {
		t.Fatalf("active retained events = %d, want 3", activeLen)
	}
	if storageLen > 6 {
		t.Fatalf("storage length = %d, want bounded near replay window", storageLen)
	}
	if minReplayAfter != 5 {
		t.Fatalf("minReplayAfter = %d, want 5", minReplayAfter)
	}

	stale := run.subscribe(4)
	if !stale.snapshotRequired || stale.minReplayAfter != 5 {
		t.Fatalf("subscribe before replay window = %#v, want snapshot required at 5", stale)
	}

	fresh := run.subscribe(5)
	if fresh.snapshotRequired {
		t.Fatalf("subscribe at replay window unexpectedly required snapshot: %#v", fresh)
	}
	if len(fresh.replay) != 3 {
		t.Fatalf("replay length = %d, want 3", len(fresh.replay))
	}
	for i, ev := range fresh.replay {
		expected := int64(i + 6)
		if ev.Sequence != expected {
			t.Fatalf("replay[%d].Sequence = %d, want %d", i, ev.Sequence, expected)
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("replay[%d] payload is invalid JSON: %v", i, err)
		}
		if payload["sequence_number"] != float64(expected) {
			t.Fatalf("replay[%d] payload sequence_number = %v, want %d", i, payload["sequence_number"], expected)
		}
	}
	if fresh.ch != nil {
		run.unsubscribe(fresh.ch)
	}
}

func TestResponseRunConcurrentAppendsPreserveOrder(t *testing.T) {
	const totalEvents = 200
	const numWriters = 4
	eventsPerWriter := totalEvents / numWriters

	run := newResponseRun("resp_order", "sess_test", "", "mock", time.Now().Unix(), func() {})

	sub := run.subscribe(0)
	if sub.ch == nil {
		t.Fatal("expected live channel from subscribe")
	}
	defer run.unsubscribe(sub.ch)

	// Launch concurrent writers, each appending eventsPerWriter events.
	var wg sync.WaitGroup
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < eventsPerWriter; i++ {
				_ = run.appendEvent("response.output_text.delta", map[string]any{"delta": "x"})
			}
		}()
	}

	// Collect all events from subscriber in a separate goroutine.
	received := make([]int64, 0, totalEvents)
	done := make(chan struct{})
	go func() {
		for ev := range sub.ch {
			received = append(received, ev.Sequence)
			if len(received) >= totalEvents {
				break
			}
		}
		close(done)
	}()

	wg.Wait()

	// Wait for all events to arrive (or timeout)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for events, got %d/%d", len(received), totalEvents)
	}

	// Verify strictly increasing sequence numbers
	for i := 1; i < len(received); i++ {
		if received[i] <= received[i-1] {
			t.Fatalf("out of order at index %d: seq %d followed by %d", i, received[i-1], received[i])
		}
	}

	// Verify no gaps
	for i, seq := range received {
		expected := int64(i + 1)
		if seq != expected {
			t.Fatalf("gap at index %d: expected seq %d, got %d", i, expected, seq)
		}
	}
}

type blockingResponseWriter struct {
	header http.Header
	gate   <-chan struct{}
	buf    bytes.Buffer
}

func (w *blockingResponseWriter) Header() http.Header {
	return w.header
}

func (w *blockingResponseWriter) WriteHeader(statusCode int) {}

func (w *blockingResponseWriter) Write(p []byte) (int, error) {
	<-w.gate
	return w.buf.Write(p)
}

func (w *blockingResponseWriter) Flush() {}

func waitForResponseRunCondition(t *testing.T, timeout time.Duration, fn func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(message)
}

func TestStreamResponseRunEventsDoesNotWriteDoneWhenSubscriberOverflows(t *testing.T) {
	srv := &serveServer{shutdownCh: make(chan struct{})}
	run := newResponseRun("resp_overflow", "sess_test", "", "mock", time.Now().Unix(), func() {})
	gate := make(chan struct{})
	w := &blockingResponseWriter{header: make(http.Header), gate: gate}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamDone := make(chan struct{})
	go func() {
		srv.streamResponseRunEvents(ctx, w, run, 0)
		close(streamDone)
	}()

	waitForResponseRunCondition(t, time.Second, func() bool {
		run.mu.Lock()
		defer run.mu.Unlock()
		return len(run.subscribers) == 1
	}, "timed out waiting for stream subscriber")

	for i := 0; i < defaultResponseRunSubscriberBuffer+16; i++ {
		if err := run.appendEvent("response.output_text.delta", map[string]any{"delta": "x"}); err != nil {
			t.Fatalf("appendEvent failed at %d: %v", i, err)
		}
	}

	waitForResponseRunCondition(t, time.Second, func() bool {
		run.mu.Lock()
		defer run.mu.Unlock()
		return len(run.subscribers) == 0
	}, "timed out waiting for subscriber drop")

	if err := run.complete(map[string]any{
		"response": map[string]any{"id": run.id},
	}, llm.Usage{}, llm.Usage{}); err != nil {
		t.Fatalf("complete failed: %v", err)
	}

	close(gate)

	select {
	case <-streamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for overflowed stream to finish")
	}

	body := w.buf.String()
	if !strings.Contains(body, "event: response.output_text.delta\n") {
		t.Fatalf("stream body missing replayed delta events: %q", body)
	}
	if strings.Contains(body, "data: [DONE]\n\n") {
		t.Fatalf("overflowed stream should not emit [DONE], got: %q", body)
	}
}
