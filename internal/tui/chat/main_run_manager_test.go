package chat

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func unwrapMainRunUIMessage(msg tea.Msg) tea.Msg {
	if envelope, ok := msg.(mainRunUIEnvelope); ok {
		return envelope.message
	}
	return msg
}

func TestNewAndClearRebindMainRunUISinkToNewSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Model)
	}{
		{name: "new", run: func(m *Model) { _, _ = m.cmdNew() }},
		{name: "clear", run: func(m *Model) { _, _ = m.cmdClear() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestChatModel(false)
			manager := NewMainRunManager(context.Background())
			defer manager.Close(time.Second)
			m.SetMainRunManager(manager)
			m.SetProgram(tea.NewProgram(m))
			oldID := m.SessionID()
			m.AttachMainRunUISink(m.program.Send)

			tc.run(m)

			if m.SessionID() == oldID {
				t.Fatal("session ID did not change")
			}
			manager.mu.RLock()
			_, oldAttached := manager.uiSinks[oldID]
			_, newAttached := manager.uiSinks[m.SessionID()]
			manager.mu.RUnlock()
			if oldAttached || !newAttached {
				t.Fatalf("sink bindings old=%v new=%v", oldAttached, newAttached)
			}
			m.DetachMainRunUISink()
		})
	}
}

func TestInlineInteractiveRequestsUseEmbeddedModels(t *testing.T) {
	m := newTestChatModel(false)
	approvalDone := make(chan tools.ApprovalResult, 1)
	_, _ = m.Update(ApprovalRequestMsg{Path: "file.go", DoneCh: approvalDone})
	if m.approvalModel == nil || !m.pausedForExternalUI {
		t.Fatal("inline approval was cancelled instead of rendered")
	}
	select {
	case result := <-approvalDone:
		t.Fatalf("inline approval resolved before user input: %#v", result)
	default:
	}

	m.approvalModel = nil
	m.approvalDoneCh = nil
	m.pausedForExternalUI = false
	askDone := make(chan []tools.AskUserAnswer, 1)
	_, _ = m.Update(AskUserRequestMsg{Questions: []tools.AskUserQuestion{{Question: "Continue?"}}, DoneCh: askDone})
	if m.askUserModel == nil || !m.pausedForExternalUI {
		t.Fatal("inline ask_user was cancelled instead of rendered")
	}
	select {
	case answers := <-askDone:
		t.Fatalf("inline ask_user resolved before user input: %#v", answers)
	default:
	}
}

func TestStartStreamCancelRoutesToManagerBeforeAttachment(t *testing.T) {
	m := newTestChatModel(false)
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	m.SetMainRunManager(manager)
	if _, err := manager.Start(m.SessionID(), MainRunExecution{Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error {
		<-ctx.Done()
		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}
	_ = m.startStream("pending")
	m.streamCancelFunc()
	select {
	case <-manager.runs[m.SessionID()].done:
	case <-time.After(time.Second):
		t.Fatal("pre-attachment cancellation did not stop manager run")
	}
}

func TestMainRunEventCoalescerPreservesOrderAndLastSequence(t *testing.T) {
	ch := make(chan MainRunEvent, 4)
	ch <- MainRunEvent{Sequence: 1, Event: ui.StreamEvent{Type: ui.StreamEventText, Text: "a"}}
	ch <- MainRunEvent{Sequence: 2, Event: ui.StreamEvent{Type: ui.StreamEventText, Text: "b"}}
	ch <- MainRunEvent{Sequence: 3, Event: ui.StreamEvent{Type: ui.StreamEventToolStart}}
	close(ch)
	coalescer := &mainRunEventCoalescer{ch: ch}
	first, ok := coalescer.next()
	if !ok || first.Sequence != 2 || first.Event.Text != "ab" {
		t.Fatalf("coalesced event = %#v, ok=%v", first, ok)
	}
	second, ok := coalescer.next()
	if !ok || second.Sequence != 3 || second.Event.Type != ui.StreamEventToolStart {
		t.Fatalf("ordered event = %#v, ok=%v", second, ok)
	}
}

func TestBeginUserResponseResetsMainRunSequenceForNextTurn(t *testing.T) {
	m := newTestChatModel(false)
	m.mainRunID = "old-run"
	m.mainRunLastSeq = 80
	m.mainRunReplay = []MainRunEvent{{Sequence: 80}}
	m.mainRunLive = make(chan MainRunEvent)

	_, _ = m.beginUserResponse("next", "next", nil)

	if m.mainRunID != "" || m.mainRunLastSeq != 0 || len(m.mainRunReplay) != 0 || m.mainRunLive != nil {
		t.Fatalf("new turn retained old run cursor: id=%q seq=%d replay=%d live=%v", m.mainRunID, m.mainRunLastSeq, len(m.mainRunReplay), m.mainRunLive != nil)
	}
}

func TestStaleMainRunSubscriberClosureDoesNotReplaceCurrentSubscription(t *testing.T) {
	m := newTestChatModel(false)
	m.mainRunID = "run-2"
	m.mainRunSubscription = 4
	m.mainRunLastSeq = 12

	updated, cmd := m.Update(mainRunSubscriberClosedMsg{sessionID: m.SessionID(), runID: "run-2", subscription: 3})
	if updated != m || cmd != nil {
		t.Fatal("stale subscriber closure was not ignored")
	}
}

func TestRejectedMainRunUIEnvelopeIsRetainedForOwningSession(t *testing.T) {
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	if _, err := manager.Start("owner", MainRunExecution{Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error {
		<-ctx.Done()
		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}
	oldSink := make(chan tea.Msg, 1)
	detach := manager.AttachUISink("owner", func(msg tea.Msg) { oldSink <- msg })
	defer detach()
	if err := manager.DeliverUI("owner", BackgroundRunsMsg{Count: 9}); err != nil {
		t.Fatal(err)
	}
	envelope := (<-oldSink).(mainRunUIEnvelope)

	visible := newTestChatModel(false)
	visible.SetMainRunManager(manager)
	_, cmd := visible.Update(envelope)
	if cmd == nil {
		t.Fatal("wrong-session envelope did not schedule retention")
	}
	_ = cmd()

	redelivered := make(chan tea.Msg, 1)
	detachOwner := manager.AttachUISink("owner", func(msg tea.Msg) { redelivered <- msg })
	defer detachOwner()
	select {
	case got := <-redelivered:
		if unwrapMainRunUIMessage(got) != (BackgroundRunsMsg{Count: 9}) {
			t.Fatalf("retained message = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("rejected prompt was not retained for its owner")
	}
}

func TestMainRunManagerDetachDoesNotBackpressureExecutionAndReplayIsOrdered(t *testing.T) {
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	release := make(chan struct{})
	startedSnapshot, err := manager.Start("session-a", MainRunExecution{Execute: func(ctx context.Context, emit func(ui.StreamEvent)) error {
		for i := 0; i < 600; i++ {
			emit(ui.StreamEvent{Type: ui.StreamEventText, Text: "x"})
		}
		close(release)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-release:
	case <-time.After(time.Second):
		t.Fatal("detached execution blocked without a subscriber")
	}
	select {
	case <-startedSnapshot.Done:
	case <-time.After(time.Second):
		t.Fatal("detached execution did not finish")
	}

	replay, live, snapshotRequired, snapshot, detach := manager.Subscribe("session-a", 0)
	defer detach()
	if snapshotRequired || live != nil || snapshot.Active || len(replay) != 600 {
		t.Fatalf("replay=%d live=%v required=%v snapshot=%+v", len(replay), live != nil, snapshotRequired, snapshot)
	}
	for i, event := range replay {
		if event.Sequence != uint64(i+1) {
			t.Fatalf("event %d sequence=%d", i, event.Sequence)
		}
	}
}

func TestMainRunManagerConcurrentSessionsAndSessionExclusivity(t *testing.T) {
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	block := make(chan struct{})
	var started sync.WaitGroup
	started.Add(2)
	execute := func(ctx context.Context, emit func(ui.StreamEvent)) error {
		started.Done()
		select {
		case <-block:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if _, err := manager.Start("one", MainRunExecution{Execute: execute}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start("two", MainRunExecution{Execute: execute}); err != nil {
		t.Fatal(err)
	}
	started.Wait()
	if got := manager.ActiveCount(); got != 2 {
		t.Fatalf("active count=%d", got)
	}
	if _, err := manager.Start("one", MainRunExecution{Execute: execute}); err == nil {
		t.Fatal("duplicate session run was accepted")
	}
	close(block)
}

func TestMainRunManagerRoutesInterjectionsToOwningSession(t *testing.T) {
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	block := make(chan struct{})
	queued := make(chan llm.QueuedInterjection, 1)
	cancelled := make(chan string, 1)
	discarded := make(chan struct{}, 1)
	if _, err := manager.Start("steered", MainRunExecution{
		Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error {
			select {
			case <-block:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		QueueInterjection: func(entry llm.QueuedInterjection) llm.InterjectionQueueStatus {
			queued <- entry
			return llm.InterjectionQueueQueued
		},
		CancelInterjection:   func(id string) bool { cancelled <- id; return true },
		DiscardInterjections: func() { discarded <- struct{}{} },
	}); err != nil {
		t.Fatal(err)
	}
	entry := llm.QueuedInterjection{ID: "steer-1", Message: llm.UserText("change course")}
	if status := manager.QueueInterjection("steered", entry); status != llm.InterjectionQueueQueued {
		t.Fatalf("queue status = %q", status)
	}
	if got := <-queued; got.ID != entry.ID {
		t.Fatalf("queued interjection = %#v", got)
	}
	if !manager.CancelInterjection("steered", entry.ID) || <-cancelled != entry.ID {
		t.Fatal("cancel was not routed to owning run")
	}
	manager.DiscardInterjections("steered")
	select {
	case <-discarded:
	case <-time.After(time.Second):
		t.Fatal("discard was not routed to owning run")
	}
	close(block)
}

func TestMainRunManagerQueuesInteractiveRequestUntilSessionAttaches(t *testing.T) {
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	block := make(chan struct{})
	if _, err := manager.Start("prompt-session", MainRunExecution{Execute: func(ctx context.Context, emit func(ui.StreamEvent)) error {
		select {
		case <-block:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}); err != nil {
		t.Fatal(err)
	}
	message := BackgroundRunsMsg{Count: 3}
	if err := manager.DeliverUI("prompt-session", message); err != nil {
		t.Fatal(err)
	}
	got := make(chan tea.Msg, 1)
	detach := manager.AttachUISink("prompt-session", func(msg tea.Msg) { got <- msg })
	defer detach()
	select {
	case delivered := <-got:
		if unwrapMainRunUIMessage(delivered) != message {
			t.Fatalf("delivered=%#v", delivered)
		}
	case <-time.After(time.Second):
		t.Fatal("queued interactive request was not delivered on attach")
	}
	close(block)
}

func TestMainRunManagerDeliverUIDoesNotHoldRunLockDuringSinkSend(t *testing.T) {
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	if _, err := manager.Start("busy", MainRunExecution{Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error {
		<-ctx.Done()
		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}
	// Model a blocked Bubble Tea Send: the sink cannot make progress until the
	// update loop drains, and the update loop needs run-scoped manager calls.
	sinkEntered := make(chan struct{})
	release := make(chan struct{})
	detach := manager.AttachUISink("busy", func(tea.Msg) {
		close(sinkEntered)
		<-release
	})
	defer detach()
	defer close(release)
	go func() { _ = manager.DeliverUI("busy", BackgroundRunsMsg{Count: 1}) }()
	<-sinkEntered
	probed := make(chan bool, 1)
	go func() { probed <- manager.HasActive("busy") }()
	select {
	case active := <-probed:
		if !active {
			t.Fatal("run unexpectedly inactive")
		}
	case <-time.After(time.Second):
		t.Fatal("run lock held while delivering to a blocked UI sink")
	}
}

func TestMainRunManagerAttachUISinkDoesNotBlockOnPendingDelivery(t *testing.T) {
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	block := make(chan struct{})
	if _, err := manager.Start("pending-session", MainRunExecution{Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error {
		select {
		case <-block:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.DeliverUI("pending-session", BackgroundRunsMsg{Count: 7}); err != nil {
		t.Fatal(err)
	}

	// An in-process session switch attaches the sink from the Bubble Tea
	// update loop, where a synchronous flush through Program.Send would
	// deadlock. AttachUISink must return before pending prompts are consumed.
	release := make(chan struct{})
	got := make(chan tea.Msg, 1)
	attached := make(chan func(), 1)
	go func() {
		attached <- manager.AttachUISink("pending-session", func(msg tea.Msg) {
			<-release
			got <- msg
		})
	}()
	var detach func()
	select {
	case detach = <-attached:
	case <-time.After(time.Second):
		t.Fatal("AttachUISink blocked while flushing retained prompts")
	}
	defer detach()
	close(release)
	select {
	case msg := <-got:
		if delivered, ok := unwrapMainRunUIMessage(msg).(BackgroundRunsMsg); !ok || delivered.Count != 7 {
			t.Fatalf("delivered %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("retained prompt was not delivered after attach")
	}
	close(block)
}

func TestMainRunManagerUISinkAttachedBeforeStartAndStaleDetachIsSafe(t *testing.T) {
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	first := make(chan tea.Msg, 1)
	second := make(chan tea.Msg, 1)
	detachFirst := manager.AttachUISink("prompt-session", func(msg tea.Msg) { first <- msg })
	block := make(chan struct{})
	if _, err := manager.Start("prompt-session", MainRunExecution{Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error {
		select {
		case <-block:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}); err != nil {
		t.Fatal(err)
	}
	detachSecond := manager.AttachUISink("prompt-session", func(msg tea.Msg) { second <- msg })
	defer detachSecond()
	detachFirst()
	message := BackgroundRunsMsg{Count: 2}
	if err := manager.DeliverUI("prompt-session", message); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-second:
		if unwrapMainRunUIMessage(got) != message {
			t.Fatalf("second sink got %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement sink did not receive request")
	}
	select {
	case got := <-first:
		t.Fatalf("stale sink received %#v", got)
	default:
	}
	close(block)
}

func TestAttachMainRunReconstructsAtLatestSequenceAndRestoresSafeAnchor(t *testing.T) {
	m := newTestChatModel(false)
	goal := session.NewGoal("finish the migration", 0, time.Now().Add(-time.Hour))
	goal.TimeUsedSeconds = 900
	m.sess.Goal = goal
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	started := make(chan struct{})
	const anchorID int64 = 42
	startSnapshot, err := manager.Start(m.SessionID(), MainRunExecution{
		Execute: func(ctx context.Context, emit func(ui.StreamEvent)) error {
			emit(ui.StreamEvent{Type: ui.StreamEventText, Text: "persisted prefix"})
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
		AnchorMessageID: anchorID,
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	m.SetMainRunManager(manager)
	if cmd := m.attachMainRun(m.SessionID()); cmd == nil {
		t.Fatal("active run did not install a listener")
	}
	defer m.detachMainRun()
	if !m.streaming || m.streamCancelFunc == nil {
		t.Fatalf("reattached state streaming=%v cancel=%v", m.streaming, m.streamCancelFunc != nil)
	}
	if m.mainRunLastSeq != 1 || len(m.mainRunReplay) != 0 {
		t.Fatalf("reattach sequence=%d replay=%d", m.mainRunLastSeq, len(m.mainRunReplay))
	}
	if m.activeBranchAnchorID != anchorID {
		t.Fatalf("safe anchor=%d, want %d", m.activeBranchAnchorID, anchorID)
	}
	if !m.streamStartTime.Equal(startSnapshot.StartedAt) {
		t.Fatalf("run clock restarted: streamStartTime=%v, want run start %v", m.streamStartTime, startSnapshot.StartedAt)
	}
	wantVisibleElapsed := time.Since(startSnapshot.StartedAt) + 15*time.Minute
	if got := m.visibleStreamElapsed(); got < wantVisibleElapsed-time.Second || got > wantVisibleElapsed+time.Second {
		t.Fatalf("visible elapsed = %v, want %v", got, wantVisibleElapsed)
	}
	m.streamCancelFunc()
}

func TestAttachMainRunRestoresPresentationAndReplaysDetachedSuffix(t *testing.T) {
	old := newTestChatModel(true)
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)

	started := make(chan struct{})
	emitDetachedSuffix := make(chan struct{})
	detachedSuffixEmitted := make(chan struct{})
	_, err := manager.Start(old.SessionID(), MainRunExecution{Execute: func(ctx context.Context, emit func(ui.StreamEvent)) error {
		emit(ui.StreamEvent{
			Type: ui.StreamEventToolStart, ToolCallID: "agent-call", ToolName: tools.SpawnAgentToolName,
			ToolInfo: "reviewer", ToolArgs: []byte(`{"agent_name":"reviewer","prompt":"review this"}`),
		})
		close(started)
		select {
		case <-emitDetachedSuffix:
			emit(ui.StreamEvent{Type: ui.StreamEventToolEnd, ToolCallID: "agent-call", ToolSuccess: true})
			close(detachedSuffixEmitted)
		case <-ctx.Done():
			return ctx.Err()
		}
		<-ctx.Done()
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	old.SetMainRunManager(manager)
	old.streaming = true
	old.mainRunViewComplete = true
	listen := old.attachMainRun(old.SessionID())
	if listen == nil {
		t.Fatal("originating model did not attach to active run")
	}
	message := listen()
	if message == nil {
		t.Fatal("originating model did not receive retained tool start")
	}
	_, _ = old.Update(message)
	if old.mainRunLastSeq != 1 || !old.tracker.HasPending() {
		t.Fatalf("originating presentation seq=%d pending=%v", old.mainRunLastSeq, old.tracker.HasPending())
	}
	ui.HandleSubagentProgress(old.tracker, old.subagentTracker, "agent-call", tools.SubagentEvent{Type: tools.SubagentEventInit})
	retainedTracker := old.tracker
	retainedSubagents := old.subagentTracker
	old.detachMainRun()

	close(emitDetachedSuffix)
	<-detachedSuffixEmitted

	fresh := newTestChatModel(true)
	fresh.sess.ID = old.SessionID()
	fresh.SetMainRunManager(manager)
	listen = fresh.attachMainRun(fresh.SessionID())
	if listen == nil {
		t.Fatal("fresh model did not attach to active run")
	}
	defer fresh.detachMainRun()
	if fresh.tracker != retainedTracker || fresh.subagentTracker != retainedSubagents {
		t.Fatal("fresh model did not adopt the detached presentation")
	}
	if fresh.subagentTracker.Get("agent-call") == nil {
		t.Fatal("fresh model lost pre-detach subagent progress")
	}
	if !fresh.mainRunViewComplete {
		t.Fatal("restored presentation was incorrectly marked incomplete")
	}
	if fresh.mainRunLastSeq != 1 || len(fresh.mainRunReplay) != 1 || fresh.mainRunReplay[0].Sequence != 2 {
		t.Fatalf("restored cursor=%d replay=%#v", fresh.mainRunLastSeq, fresh.mainRunReplay)
	}
	if len(fresh.tracker.Segments) != 1 || fresh.tracker.Segments[0].ToolCallID != "agent-call" || fresh.tracker.Segments[0].ToolName != tools.SpawnAgentToolName {
		t.Fatalf("restored spawn-agent presentation missing: %#v", fresh.tracker.Segments)
	}

	message = listen()
	if message == nil {
		t.Fatal("fresh model did not receive detached suffix")
	}
	_, _ = fresh.Update(message)
	if fresh.mainRunLastSeq != 2 || fresh.tracker.HasPending() {
		t.Fatalf("detached suffix was not applied: seq=%d pending=%v", fresh.mainRunLastSeq, fresh.tracker.HasPending())
	}
}

func TestAttachCompletedMainRunConsumesVisibleCompletionAttention(t *testing.T) {
	m := newTestChatModel(false)
	manager := NewMainRunManager(t.Context())
	defer manager.Close(time.Second)
	snapshot, err := manager.Start(m.SessionID(), MainRunExecution{Execute: func(context.Context, func(ui.StreamEvent)) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	<-snapshot.Done
	if !manager.Status(m.SessionID()).Unvisited {
		t.Fatal("completion did not request attention")
	}
	m.SetMainRunManager(manager)
	if cmd := m.attachMainRun(m.SessionID()); cmd != nil {
		t.Fatal("completed non-streaming run unexpectedly installed a listener")
	}
	if manager.Status(m.SessionID()).Unvisited {
		t.Fatal("visible completed run retained attention")
	}
}

func TestIncompleteMainRunPresentationDoesNotMaskDurableHistoryOnDone(t *testing.T) {
	m := newTestChatModel(true)
	m.streaming = true
	m.mainRunViewComplete = false
	m.viewCache.completedStream = "stale partial stream"
	m.tracker.HandleToolStart("late-call", "shell", "late tool", nil)

	_, _ = m.Update(streamEventMsg{event: ui.DoneEvent(0)})

	if m.viewCache.completedStream != "" {
		t.Fatalf("partial completed stream masked durable history: %q", m.viewCache.completedStream)
	}
}

func TestMainRunManagerCancelResolvesDetachedApproval(t *testing.T) {
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	blocked := make(chan struct{})
	if _, err := manager.Start("approval-session", MainRunExecution{Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error {
		select {
		case <-blocked:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan tools.ApprovalResult, 1)
	if err := manager.DeliverUI("approval-session", ApprovalRequestMsg{DoneCh: done}); err != nil {
		t.Fatal(err)
	}
	if !manager.Cancel("approval-session") {
		t.Fatal("cancel did not find approval run")
	}
	select {
	case result := <-done:
		if !result.Cancelled || result.Choice != tools.ApprovalChoiceCancelled {
			t.Fatalf("approval result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("detached approval was not resolved on cancellation")
	}
}

func TestMainRunManagerCloseCancelsAndCleansEveryRun(t *testing.T) {
	manager := NewMainRunManager(context.Background())
	var cleaned sync.WaitGroup
	cleaned.Add(2)
	for _, sessionID := range []string{"a", "b"} {
		_, err := manager.Start(sessionID, MainRunExecution{
			Execute: func(ctx context.Context, emit func(ui.StreamEvent)) error {
				<-ctx.Done()
				return ctx.Err()
			},
			Cleanup: cleaned.Done,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	manager.Close(time.Second)
	done := make(chan struct{})
	go func() { cleaned.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run cleanup did not complete")
	}
}

func TestMainRunManagerPropagatesExecutionErrorInSnapshot(t *testing.T) {
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	want := errors.New("provider failed")
	if _, err := manager.Start("failed", MainRunExecution{Execute: func(context.Context, func(ui.StreamEvent)) error { return want }}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, _, _, snapshot, detach := manager.Subscribe("failed", 0)
		detach()
		if !snapshot.Active {
			if !errors.Is(snapshot.Err, want) {
				t.Fatalf("snapshot error=%v", snapshot.Err)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("run did not become terminal")
}

func TestMainRunManagerMarksEveryBackgroundTerminalOutcomeUnvisited(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "error", err: errors.New("provider failed")},
		{name: "interrupted", err: context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewMainRunManager(t.Context())
			defer manager.Close(time.Second)
			release := make(chan struct{})
			snapshot, err := manager.Start("session", MainRunExecution{Execute: func(context.Context, func(ui.StreamEvent)) error {
				<-release
				return tc.err
			}})
			if err != nil {
				t.Fatal(err)
			}
			close(release)
			<-snapshot.Done
			status := manager.Status("session")
			if status.RunID != snapshot.RunID || status.Active || !status.Unvisited {
				t.Fatalf("terminal status = %#v", status)
			}
			if !manager.Visit("session", snapshot.RunID) || manager.Status("session").Unvisited {
				t.Fatal("terminal visit did not clear attention")
			}
		})
	}
}

func TestMainRunManagerCancellationRequestsAttention(t *testing.T) {
	manager := NewMainRunManager(t.Context())
	defer manager.Close(time.Second)
	snapshot, err := manager.Start("session", MainRunExecution{Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !manager.Cancel("session") {
		t.Fatal("cancel did not find active run")
	}
	<-snapshot.Done
	if status := manager.Status("session"); status.Active || !status.Unvisited {
		t.Fatalf("cancelled terminal status = %#v", status)
	}
}

func TestMainRunManagerVisibleCompletionIsAlreadyVisited(t *testing.T) {
	manager := NewMainRunManager(t.Context())
	defer manager.Close(time.Second)
	detach := manager.AttachUISink("session", func(tea.Msg) {})
	defer detach()
	release := make(chan struct{})
	snapshot, err := manager.Start("session", MainRunExecution{Execute: func(context.Context, func(ui.StreamEvent)) error {
		<-release
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	<-snapshot.Done
	if status := manager.Status("session"); status.Active || status.Unvisited {
		t.Fatalf("visible terminal status = %#v", status)
	}
}

func TestMainRunManagerActiveVisitDoesNotConsumeFutureResult(t *testing.T) {
	manager := NewMainRunManager(t.Context())
	defer manager.Close(time.Second)
	detach := manager.AttachUISink("session", func(tea.Msg) {})
	release := make(chan struct{})
	snapshot, err := manager.Start("session", MainRunExecution{Execute: func(context.Context, func(ui.StreamEvent)) error {
		<-release
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if manager.Visit("session", snapshot.RunID) {
		t.Fatal("active run was marked visited before it had a result")
	}
	detach()
	close(release)
	<-snapshot.Done
	if status := manager.Status("session"); status.Active || !status.Unvisited {
		t.Fatalf("background completion after active visit = %#v", status)
	}
}

func TestMainRunManagerVisitIsRunAware(t *testing.T) {
	manager := NewMainRunManager(t.Context())
	defer manager.Close(time.Second)

	first, err := manager.Start("session", MainRunExecution{Execute: func(context.Context, func(ui.StreamEvent)) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	<-first.Done
	if !manager.Status("session").Unvisited {
		t.Fatal("first completion was not marked unvisited")
	}

	second, err := manager.Start("session", MainRunExecution{Execute: func(context.Context, func(ui.StreamEvent)) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	<-second.Done
	if manager.Visit("session", first.RunID) {
		t.Fatal("stale visit matched the replacement run")
	}
	if !manager.Status("session").Unvisited {
		t.Fatal("stale visit cleared the replacement completion")
	}
	if !manager.Visit("session", second.RunID) || manager.Status("session").Unvisited {
		t.Fatal("current completion visit did not clear attention")
	}
}
