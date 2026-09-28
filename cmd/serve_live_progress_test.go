package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/live"
)

type liveProgressHarness struct {
	t      *testing.T
	now    time.Time
	chunks []live.DelegationChunk
	p      *liveProgress
}

func newLiveProgressHarness(t *testing.T) *liveProgressHarness {
	h := &liveProgressHarness{t: t, now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	h.p = newLiveProgress(func(chunk live.DelegationChunk) { h.chunks = append(h.chunks, chunk) }, func() time.Time { return h.now })
	return h
}

func (h *liveProgressHarness) event(name string, payload map[string]any) {
	h.t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	h.p.observe(responseRunEvent{Event: name, Data: data})
	h.p.tick()
}

func (h *liveProgressHarness) advance(d time.Duration) {
	h.now = h.now.Add(d)
	h.p.tick()
}

// take returns and clears the chunks emitted since the last call.
func (h *liveProgressHarness) take() []live.DelegationChunk {
	chunks := h.chunks
	h.chunks = nil
	return chunks
}

func (h *liveProgressHarness) startTool(callID, name, info string) {
	h.event("response.tool_exec.start", map[string]any{
		"call_id": callID, "tool_name": name, "tool_info": info, "started_at": h.now.UnixMilli(),
	})
}

func quietStatus(text string) live.DelegationChunk {
	return live.DelegationChunk{Channel: live.ChannelQuiet, Text: text, Progress: true}
}

func spokenProgress(text string) live.DelegationChunk {
	return live.DelegationChunk{Channel: live.ChannelSpeakable, Text: text, Progress: true}
}

func assertChunks(t *testing.T, got []live.DelegationChunk, want ...live.DelegationChunk) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("chunks = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chunk %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

// The quiet status is a fixed template filled from event fields, sent when the
// state changes and never more often than the spacing allows; changes inside
// the window coalesce into the next snapshot of the latest state.
func TestLiveProgressStatusIsTemplatedAndCoalesced(t *testing.T) {
	h := newLiveProgressHarness(t)
	h.startTool("call_1", "shell", "Run \"live\"\npackage   tests")
	assertChunks(t, h.take(), quietStatus(`[STATUS] Running shell "Run 'live' package tests" (0.0s). 0.0s elapsed.`))

	h.advance(time.Second)
	h.event("response.tool_exec.end", map[string]any{"call_id": "call_1", "tool_name": "shell", "success": false, "duration_ms": 1000})
	h.advance(time.Second)
	h.startTool("call_2", "read_file", "internal/live/controller.go")
	if chunks := h.take(); len(chunks) != 0 {
		t.Fatalf("status inside the spacing window: %#v", chunks)
	}

	h.advance(2 * time.Second)
	assertChunks(t, h.take(), quietStatus(`[STATUS] Running read_file "internal/live/controller.go" (2.0s). `+
		`Last: shell "Run 'live' package tests" failed after 1.0s. 4.0s elapsed, 1 tool finished (1 failed).`))

	// Nothing changed, so the ticker alone sends nothing more.
	h.advance(10 * time.Second)
	if chunks := h.take(); len(chunks) != 0 {
		t.Fatalf("unchanged state was resent: %#v", chunks)
	}
}

// spoken keeps only the notes the voice model is asked to say.
func spoken(chunks []live.DelegationChunk) []live.DelegationChunk {
	var out []live.DelegationChunk
	for _, chunk := range chunks {
		if chunk.Channel == live.ChannelSpeakable {
			out = append(out, chunk)
		}
	}
	return out
}

// A spoken note fills a silence: the first after liveProgressFirstSilence, then
// no more often than liveProgressRepeatGap, and result text resets the clock.
func TestLiveProgressSpeaksOnlyAfterSilence(t *testing.T) {
	h := newLiveProgressHarness(t)
	h.startTool("call_1", "shell", "Run tests")
	h.take()

	h.advance(liveProgressFirstSilence - time.Second)
	if chunks := spoken(h.take()); len(chunks) != 0 {
		t.Fatalf("spoke before the silence threshold: %#v", chunks)
	}
	h.advance(time.Second)
	assertChunks(t, spoken(h.take()), spokenProgress(`[PROGRESS] Still working, no result yet: running shell "Run tests" (15s). 15s elapsed.`))

	h.advance(liveProgressRepeatGap - time.Second)
	if chunks := spoken(h.take()); len(chunks) != 0 {
		t.Fatalf("repeated before the repeat gap: %#v", chunks)
	}
	h.advance(time.Second)
	assertChunks(t, spoken(h.take()), spokenProgress(`[PROGRESS] Still working, no result yet: running shell "Run tests" (45s). 45s elapsed.`))

	// Result text is speech of its own; the next silence is measured from it,
	// with the shorter first threshold.
	h.event("response.output_text.delta", map[string]any{"delta": "Found it."})
	h.advance(liveProgressFirstSilence - time.Second)
	if chunks := spoken(h.take()); len(chunks) != 0 {
		t.Fatalf("spoke over fresh result text: %#v", chunks)
	}
	h.advance(time.Second)
	if chunks := spoken(h.take()); len(chunks) != 1 {
		t.Fatalf("no note after the result text went quiet: %#v", chunks)
	}
}

// A tool that runs quietly still gets its elapsed time refreshed, at
// liveStatusRefresh rather than every tick; with nothing running, an unchanged
// state is not resent.
func TestLiveProgressRefreshesStatusForRunningTools(t *testing.T) {
	h := newLiveProgressHarness(t)
	h.startTool("call_1", "shell", "Run tests")
	h.take()
	h.advance(liveStatusRefresh - time.Second)
	if chunks := h.take(); len(chunks) != 0 {
		t.Fatalf("refreshed early: %#v", chunks)
	}
	h.advance(time.Second)
	if chunks := h.take(); len(chunks) != 2 || chunks[1] != quietStatus(`[STATUS] Running shell "Run tests" (15s). 15s elapsed.`) {
		t.Fatalf("stale elapsed time was not refreshed: %#v", chunks)
	}

	h.event("response.tool_exec.end", map[string]any{"call_id": "call_1", "tool_name": "shell", "success": true, "duration_ms": 15000})
	h.advance(liveStatusMinSpacing)
	h.take()
	h.advance(time.Minute)
	for _, chunk := range h.take() {
		if chunk.Channel == live.ChannelQuiet {
			t.Fatalf("resent an unchanged status with nothing running: %#v", chunk)
		}
	}
}

// A run that needs the user says so at once, stays quiet while it waits, and
// measures the next silence from the moment the user answered.
func TestLiveProgressAnnouncesInputRequestsImmediately(t *testing.T) {
	h := newLiveProgressHarness(t)
	h.event("response.approval.prompt", map[string]any{"approval_id": "appr_1", "title": "Run rm -rf tmp/build"})
	assertChunks(t, h.take(),
		spokenProgress(`[PROGRESS] Waiting for the user's approval for "Run rm -rf tmp/build" in the app.`),
		quietStatus(`[STATUS] Waiting for the user's approval for "Run rm -rf tmp/build". 0.0s elapsed.`))

	// A replayed prompt is not announced twice.
	h.event("response.approval.prompt", map[string]any{"approval_id": "appr_1", "title": "Run rm -rf tmp/build"})
	h.advance(5 * time.Minute)
	if chunks := h.take(); len(chunks) != 0 {
		t.Fatalf("nagged while waiting on the user: %#v", chunks)
	}

	h.event("response.approval.resolved", map[string]any{"approval_id": "appr_1"})
	h.take()
	h.advance(liveProgressFirstSilence - time.Second)
	if chunks := h.take(); len(chunks) != 0 {
		t.Fatalf("spoke right after the user answered: %#v", chunks)
	}
	h.advance(time.Second)
	assertChunks(t, h.take(), spokenProgress(`[PROGRESS] Still working, no result yet: the agent is thinking. 5m15s elapsed.`))

	h.event("response.ask_user.prompt", map[string]any{"call_id": "ask_1", "questions": []map[string]any{{"header": "Target", "question": "Which branch?"}}})
	if chunks := h.take(); len(chunks) == 0 || chunks[0] != spokenProgress(`[PROGRESS] Waiting for the user to answer a question: "Which branch?" in the app.`) {
		t.Fatalf("question was not announced: %#v", chunks)
	}
}

func TestLiveProgressSummarisesSubagents(t *testing.T) {
	h := newLiveProgressHarness(t)
	h.startTool("call_spawn", "spawn_agent", "Review the diff")
	h.take()
	h.advance(liveStatusMinSpacing)
	h.event("response.tool_exec.progress", map[string]any{
		"call_id": "call_spawn", "state": "running", "calls_started": 14, "current_tool": "grep",
		"children": []map[string]any{{"state": "running"}, {"state": "running"}, {"state": "completed"}},
	})
	assertChunks(t, h.take(), quietStatus(`[STATUS] Running spawn_agent "Review the diff" (4.0s). `+
		`spawn_agent: 2 of 3 agents active, 14 tool calls, now grep. 4.0s elapsed.`))
}

// Every note stays within its byte budget and valid UTF-8, however long or
// multibyte the event text is.
func TestLiveProgressNotesAreBounded(t *testing.T) {
	h := newLiveProgressHarness(t)
	long := strings.Repeat("写", 400)
	h.startTool("call_1", strings.Repeat("tool", 50), long)
	h.event("response.approval.prompt", map[string]any{"approval_id": "a", "title": long})
	h.advance(time.Hour)
	chunks := h.take()
	if len(chunks) == 0 {
		t.Fatal("no notes")
	}
	for _, chunk := range chunks {
		if len(chunk.Text) > liveStatusMaxBytes || !utf8.ValidString(chunk.Text) {
			t.Fatalf("note exceeds %d bytes or is invalid UTF-8 (%d): %q", liveStatusMaxBytes, len(chunk.Text), chunk.Text)
		}
	}
}

// A redelivered end event must not count the tool twice.
func TestLiveProgressCountsEachToolEndOnce(t *testing.T) {
	h := newLiveProgressHarness(t)
	h.startTool("call_1", "grep", "live")
	end := map[string]any{"call_id": "call_1", "tool_name": "grep", "success": true, "duration_ms": 200}
	h.event("response.tool_exec.end", end)
	h.event("response.tool_exec.end", end)
	if h.p.done != 1 || h.p.failed != 0 {
		t.Fatalf("done/failed = %d/%d, want 1/0", h.p.done, h.p.failed)
	}
}

// Waiting on the user ends on any evidence the run moved on, not only on the
// resolution event, so one missed event cannot silence the rest of the call.
func TestLiveProgressWaitEndsWhenTheRunMovesOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt func(h *liveProgressHarness)
		resume func(h *liveProgressHarness)
	}{
		{
			name: "answer text resumes after an approval",
			prompt: func(h *liveProgressHarness) {
				h.event("response.approval.prompt", map[string]any{"approval_id": "appr_1", "title": "Run tests"})
			},
			resume: func(h *liveProgressHarness) {
				h.event("response.output_text.delta", map[string]any{"delta": "Done."})
			},
		},
		{
			name: "the ask_user tool ends",
			prompt: func(h *liveProgressHarness) {
				h.event("response.ask_user.prompt", map[string]any{"call_id": "ask_1", "questions": []map[string]any{{"question": "Which branch?"}}})
			},
			resume: func(h *liveProgressHarness) {
				h.event("response.tool_exec.end", map[string]any{"call_id": "ask_1", "tool_name": "ask_user", "success": true})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLiveProgressHarness(t)
			tc.prompt(h)
			tc.resume(h)
			h.take()
			h.advance(liveProgressFirstSilence)
			if chunks := h.take(); len(chunks) == 0 || !strings.HasPrefix(chunks[0].Text, "[PROGRESS] Still working") {
				t.Fatalf("silence notes did not resume: %#v", chunks)
			}
		})
	}
}

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
	h := newLiveProgressHarness(t)
	queue := make(chan responseRunEvent, 4)
	queue <- event(t, 2, "response.approval.resolved", map[string]any{"approval_id": "appr_1"})
	queue <- event(t, 3, "response.tool_exec.start", map[string]any{"call_id": "c1", "tool_name": "shell", "tool_info": "Run tests"})
	after := int64(0)
	backlog := []responseRunEvent{event(t, 1, "response.approval.prompt", map[string]any{"approval_id": "appr_1", "title": "Run tests"})}
	done, err := (&serveLiveDelegator{}).applyRunBacklog(backlog, queue, &after, h.p.emit, h.p)
	if done || err != nil || after != 3 || len(queue) != 0 {
		t.Fatalf("done=%v err=%v after=%d queued=%d", done, err, after, len(queue))
	}
	h.p.tick()
	for _, chunk := range h.take() {
		if strings.Contains(chunk.Text, "Waiting for") {
			t.Fatalf("announced a prompt answered in the same batch: %#v", chunk)
		}
	}

	// A closed or nil subscription ends the batch without blocking.
	closed := make(chan responseRunEvent)
	close(closed)
	for _, events := range []chan responseRunEvent{closed, nil} {
		if done, err := (&serveLiveDelegator{}).applyRunBacklog(nil, events, &after, h.p.emit, h.p); done || err != nil {
			t.Fatalf("done=%v err=%v", done, err)
		}
	}
}

// A long request cannot push another pending request out of either note.
func TestLiveProgressLongWaitsShareTheNote(t *testing.T) {
	h := newLiveProgressHarness(t)
	long := strings.Repeat("very long approval title ", 10)
	h.p.observe(responseRunEvent{Event: "response.approval.prompt", Data: []byte(`{"approval_id":"appr_1","title":"` + long + `"}`)})
	h.p.observe(responseRunEvent{Event: "response.ask_user.prompt", Data: []byte(`{"call_id":"ask_1","questions":[{"question":"` + long + `"}]}`)})
	h.p.tick()
	chunks := h.take()
	if len(chunks) != 2 {
		t.Fatalf("chunks = %#v", chunks)
	}
	for _, chunk := range chunks {
		if !strings.Contains(chunk.Text, "approval") || !strings.Contains(chunk.Text, "to answer a question") || len(chunk.Text) > liveStatusMaxBytes {
			t.Fatalf("a pending request was cut from %q (%d bytes)", chunk.Text, len(chunk.Text))
		}
	}
}

// However many requests are pending, the note names some and counts the rest
// rather than cutting any off unannounced.
func TestLiveProgressCountsWaitsBeyondTheShownFew(t *testing.T) {
	h := newLiveProgressHarness(t)
	long := strings.Repeat("very long approval title ", 10)
	for i := range 8 {
		h.p.observe(responseRunEvent{Event: "response.approval.prompt", Data: fmt.Appendf(nil, `{"approval_id":"appr_%d","title":"%s"}`, i, long)})
	}
	h.p.tick()
	chunks := h.take()
	if len(chunks) != 2 {
		t.Fatalf("chunks = %#v", chunks)
	}
	for _, chunk := range chunks {
		if strings.Count(chunk.Text, "approval for") != liveWaitingShown || !strings.Contains(chunk.Text, ", and 5 more") || len(chunk.Text) > liveStatusMaxBytes {
			t.Fatalf("pending requests not all accounted for in %q (%d bytes)", chunk.Text, len(chunk.Text))
		}
	}
}

// An empty delta says nothing, so it neither ends a wait nor resets silence.
func TestLiveProgressIgnoresEmptyDeltas(t *testing.T) {
	h := newLiveProgressHarness(t)
	h.p.observe(responseRunEvent{Event: "response.approval.prompt", Data: []byte(`{"approval_id":"appr_1","title":"Run tests"}`)})
	h.p.observe(responseRunEvent{Event: "response.output_text.delta", Data: []byte(`{"delta":""}`)})
	h.p.tick()
	if chunks := h.take(); len(chunks) == 0 || !strings.Contains(chunks[0].Text, "Waiting for") {
		t.Fatalf("an empty delta ended the wait: %#v", chunks)
	}
}

// A start time from outside the delegation is not trusted, but a tool that
// started just before the tracker subscribed keeps its real age.
func TestLiveProgressClampsUntrustedStartTimes(t *testing.T) {
	h := newLiveProgressHarness(t)
	h.event("response.tool_exec.start", map[string]any{"call_id": "c1", "tool_name": "shell", "tool_info": "Run tests", "started_at": 1})
	assertChunks(t, h.take(), quietStatus(`[STATUS] Running shell "Run tests" (0.0s). 0.0s elapsed.`))

	h = newLiveProgressHarness(t)
	h.event("response.tool_exec.start", map[string]any{
		"call_id": "c2", "tool_name": "shell", "tool_info": "Run tests", "started_at": h.now.Add(-2 * time.Second).UnixMilli(),
	})
	assertChunks(t, h.take(), quietStatus(`[STATUS] Running shell "Run tests" (2.0s). 0.0s elapsed.`))
}

// Every pending request is announced, together, and the status lists them
// all; answering one leaves the other reported.
func TestLiveProgressAnnouncesEveryPendingRequest(t *testing.T) {
	h := newLiveProgressHarness(t)
	h.p.observe(responseRunEvent{Event: "response.approval.prompt", Data: []byte(`{"approval_id":"appr_1","title":"Run tests"}`)})
	h.p.observe(responseRunEvent{Event: "response.ask_user.prompt", Data: []byte(`{"call_id":"ask_1","questions":[{"question":"Which branch?"}]}`)})
	// A prompt without an id could never be resolved; it must not latch.
	h.p.observe(responseRunEvent{Event: "response.approval.prompt", Data: []byte(`{"title":"Unnamed"}`)})
	h.p.tick()
	assertChunks(t, h.take(),
		spokenProgress(`[PROGRESS] Waiting for the user's approval for "Run tests", and for the user to answer a question: "Which branch?" in the app.`),
		quietStatus(`[STATUS] Waiting for the user's approval for "Run tests", and for the user to answer a question: "Which branch?". 0.0s elapsed.`))

	h.advance(liveStatusMinSpacing)
	h.event("response.ask_user.resolved", map[string]any{"call_id": "ask_1"})
	assertChunks(t, h.take(), quietStatus(`[STATUS] Waiting for the user's approval for "Run tests". 4.0s elapsed.`))
}

func TestRedactLiveProgress(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`curl -H "Authorization: Bearer abc.def-ghi" https://x`, `curl -H "Authorization: [redacted]" https://x`},
		{`curl -H 'authorization: token ghp_abcdefgh1234' x`, `curl -H 'authorization: [redacted]' x`},
		{`export OPENAI_API_KEY=sk-proj-abcdefgh12345678`, `export OPENAI_API_KEY=[redacted]`},
		{`psql postgres://sam:hunter2@db/app`, `psql postgres://[redacted]@db/app`},
		{`gh api -H x ghp_0123456789abcdefghij`, `gh api -H x [redacted]`},
		{`deploy --password "s3cret value" --force`, `deploy --password [redacted] --force`},
		{`deploy --password hunter2`, `deploy --password [redacted]`},
		{`curl --token abc123 https://x`, `curl --token [redacted] https://x`},
		{`deploy --password=s3cret`, `deploy --password=[redacted]`},
		{`curl -d '{"password": "hunter2", "user": "sam"}'`, `curl -d '{"password": [redacted], "user": "sam"}'`},
		{`curl -d '{"api_key":"x1"}'`, `curl -d '{"api_key":[redacted]}'`},
		{`curl -H '"authorization": "Bearer y"'`, `curl -H '"authorization": [redacted]'`},
		{`curl -H "Cookie: session=abc123; theme=dark" x`, `curl -H "Cookie: [redacted]" x`},
		{`curl --cookie "sid=secret123" https://x`, `curl --cookie [redacted] https://x`},
		{`curl --cookie sid=secret123 https://x`, `curl --cookie [redacted] https://x`},
		{`curl -s -b 'sid=secret123' https://x`, `curl -s -b [redacted] https://x`},
		{`curl -b sid=secret123 https://x`, `curl -b [redacted] https://x`},
		{`git checkout -b live-progress`, `git checkout -b live-progress`},
		{`curl -u alice:topsecret https://example.com`, `curl -u [redacted] https://example.com`},
		{`curl --user 'alice:top secret' https://x`, `curl --user [redacted] https://x`},
		{`curl --user=alice:topsecret https://x`, `curl --user=[redacted] https://x`},
		{`mysqladmin --user root:hunter2 status`, `mysqladmin --user [redacted] status`},
		{`adduser --user sam`, `adduser --user sam`},
		{`curl -H "X: Bearer eyJhbGciOi.payload.sig" x`, `curl -H "X: Bearer [redacted]" x`},
		{`post xoxp-1234567890-abc`, `post [redacted]`},
		{`echo 9f8e7d6c5b4a39281706f5e4d3c2b1a0ffeeddcc`, `echo [redacted]`},
		// Ordinary previews survive.
		{`Run live package tests`, `Run live package tests`},
		{`Count token budget in internal/live/controller_test.go`, `Count token budget in internal/live/controller_test.go`},
		{`go test -run TestControllerProgressNeverOvertakesBufferedResultText`, `go test -run TestControllerProgressNeverOvertakesBufferedResultText`},
		{`Fix basic auth handling`, `Fix basic auth handling`},
		{`Fix bearer handling`, `Fix bearer handling`},
		{`Fix Bearer Token handling`, `Fix Bearer Token handling`},
		{`git log --author Alice --author=Bob --co-author Eve`, `git log --author Alice --author=Bob --co-author Eve`},
		{`login --author-token x9 author_secret=abc`, `login --author-token [redacted] author_secret=[redacted]`},
		{`curl -H "X: Bearer 'abc123def456'" x`, `curl -H "X: Bearer '[redacted]'" x`},
		{`curl -H "X: Bearer abcdefghijklmnop" x`, `curl -H "X: Bearer [redacted]" x`},
		{`curl -H "X: Bearer abc123" x`, `curl -H "X: Bearer [redacted]" x`},
		{`curl -H "X: Bearer hUnTeR" x`, `curl -H "X: Bearer [redacted]" x`},
		{`login --auth-token x9 --auth abc`, `login --auth-token [redacted] --auth [redacted]`},
		{`check authorization handling`, `check authorization handling`},
		{`go test ./internal/auth/... -run TestTokenRefresh`, `go test ./internal/auth/... -run TestTokenRefresh`},
	} {
		if got := redactLiveProgress(tc.in); got != tc.want {
			t.Errorf("redactLiveProgress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatProgressDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{-time.Second, "0.0s"},
		{250 * time.Millisecond, "0.2s"},
		{9*time.Second + 940*time.Millisecond, "9.9s"},
		{42 * time.Second, "42s"},
		{65 * time.Second, "1m05s"},
		{2*time.Hour + 3*time.Minute, "2h03m"},
	} {
		if got := formatProgressDuration(tc.in); got != tc.want {
			t.Errorf("formatProgressDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
