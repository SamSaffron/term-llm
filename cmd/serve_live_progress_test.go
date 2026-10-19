package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/app/serve/liveprogress"
	"github.com/samsaffron/term-llm/internal/live"
)

// streamRun folds a replayed backlog before announcing anything: an approval
// answered within the backlog is never spoken as pending, while one still
// pending when the backlog ends is spoken exactly once. The resolution comes
// from the production path that answers an approval.
func TestServeLiveStreamRunAnnouncesOnlyPendingInputAfterReplay(t *testing.T) {
	appendEvent := func(t *testing.T, run *responseRun, name string, payload map[string]any) {
		t.Helper()
		if err := run.appendEvent(name, payload); err != nil {
			t.Fatal(err)
		}
	}
	spokenWaits := func(chunks []live.DelegationChunk) int {
		n := 0
		for _, chunk := range chunks {
			if chunk.Channel == live.ChannelSpeakable && strings.Contains(chunk.Text, "Waiting for") {
				n++
			}
		}
		return n
	}

	t.Run("answered within the backlog", func(t *testing.T) {
		// No answer text follows, so only the resolution event can end the
		// wait, and the run stays open so the post-replay tick runs.
		run := newResponseRun("resp-live-replay", "sess", "", "mock", 1, nil)
		appendEvent(t, run, "response.approval.prompt", map[string]any{"approval_id": "appr_1", "title": "Run tests"})
		run.recordResolvedInteraction("approval", "appr_1", "accepted")

		chunks := make(chan live.DelegationChunk, 16)
		done := make(chan error, 1)
		go func() {
			done <- (&serveLiveDelegator{}).streamRun(t.Context(), run, func(chunk live.DelegationChunk) { chunks <- chunk })
		}()
		var got []live.DelegationChunk
		select {
		case chunk := <-chunks: // the post-replay status snapshot
			got = append(got, chunk)
		case <-time.After(5 * time.Second):
			t.Fatal("no status after the replayed backlog")
		}
		appendEvent(t, run, "response.completed", map[string]any{})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		close(chunks)
		for chunk := range chunks {
			got = append(got, chunk)
		}
		for _, chunk := range got {
			if strings.Contains(chunk.Text, "Waiting for") {
				t.Fatalf("reported an approval already answered as pending: %#v", got)
			}
		}
	})

	t.Run("still pending after the backlog", func(t *testing.T) {
		run := newResponseRun("resp-live-pending", "sess", "", "mock", 1, nil)
		appendEvent(t, run, "response.approval.prompt", map[string]any{"approval_id": "appr_1", "title": "Run tests"})

		chunks := make(chan live.DelegationChunk, 16)
		done := make(chan error, 1)
		go func() {
			done <- (&serveLiveDelegator{}).streamRun(t.Context(), run, func(chunk live.DelegationChunk) { chunks <- chunk })
		}()
		var got []live.DelegationChunk
		for spokenWaits(got) == 0 {
			select {
			case chunk := <-chunks:
				got = append(got, chunk)
			case <-time.After(5 * time.Second):
				t.Fatalf("pending approval was never announced: %#v", got)
			}
		}
		appendEvent(t, run, "response.completed", map[string]any{})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		close(chunks)
		for chunk := range chunks {
			got = append(got, chunk)
		}
		if n := spokenWaits(got); n != 1 {
			t.Fatalf("pending approval announced %d times: %#v", n, got)
		}
	})
}

// Events already queued on the subscription are folded with the batch in hand —
// a received event or a replayed backlog — so a prompt answered before the
// consumer caught up is never announced.
func TestServeLiveAppliesQueuedRunEventsBeforeTicking(t *testing.T) {
	event := func(t *testing.T, sequence int64, name string, payload map[string]any) responseRunEvent {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return responseRunEvent{Sequence: sequence, Event: name, Data: data}
	}
	// streamRun passes a replayed backlog, or a single received event, as the
	// batch in hand; both then fold what the subscription already holds.
	var chunks []live.DelegationChunk
	emit := func(chunk live.DelegationChunk) { chunks = append(chunks, chunk) }
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	progress := liveprogress.New(emit, func() time.Time { return now })
	queue := make(chan responseRunEvent, 4)
	queue <- event(t, 2, "response.approval.resolved", map[string]any{"approval_id": "appr_1"})
	queue <- event(t, 3, "response.tool_exec.start", map[string]any{"call_id": "c1", "tool_name": "shell", "tool_info": "Run tests"})
	after := int64(0)
	backlog := []responseRunEvent{event(t, 1, "response.approval.prompt", map[string]any{"approval_id": "appr_1", "title": "Run tests"})}
	done, err := (&serveLiveDelegator{}).applyRunBacklog(backlog, queue, &after, emit, progress)
	if done || err != nil || after != 3 || len(queue) != 0 {
		t.Fatalf("done=%v err=%v after=%d queued=%d", done, err, after, len(queue))
	}
	progress.Tick()
	for _, chunk := range chunks {
		if strings.Contains(chunk.Text, "Waiting for") {
			t.Fatalf("announced a prompt answered in the same batch: %#v", chunk)
		}
	}

	// A closed or nil subscription ends the batch without blocking.
	closed := make(chan responseRunEvent)
	close(closed)
	for _, events := range []chan responseRunEvent{closed, nil} {
		if done, err := (&serveLiveDelegator{}).applyRunBacklog(nil, events, &after, emit, progress); done || err != nil {
			t.Fatalf("done=%v err=%v", done, err)
		}
	}
}
