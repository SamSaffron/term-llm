package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func startSteeringTestRun(t *testing.T, sessionID string, bridges ...*steeringTestBridge) (*MainRunManager, *llm.Engine, MainRunSnapshot) {
	t.Helper()
	entered := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) > 1 {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	var provider llm.Provider = llm.NewOpenAICompatProvider(server.URL, "", "test", "compat")
	registry := llm.NewToolRegistry()
	if len(bridges) > 0 {
		bridges[0].Provider = provider
		provider = bridges[0]
		registry.Register(bridges[0].tool)
	}
	engine := llm.NewEngine(provider, registry)
	manager := NewMainRunManager(context.Background())
	t.Cleanup(func() { manager.Close(time.Second) })
	snapshot, err := manager.Start(sessionID, MainRunExecution{Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error {
		stream, err := engine.Stream(ctx, llm.Request{Messages: []llm.Message{llm.UserText("source")}, Tools: []llm.ToolSpec{{Name: "never", Schema: map[string]any{"type": "object"}}}})
		if err != nil {
			return err
		}
		defer stream.Close()
		for {
			if _, err = stream.Recv(); err != nil {
				return err
			}
		}
	}, QueueSteering: func(entry llm.QueuedSteering) llm.SteeringQueueStatus {
		_, status := engine.QueueSteeringWithStatus(entry)
		return status
	}, ListSteering: engine.ListPendingSteering})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("source did not start")
	}
	for _, id := range []string{"first", "second"} {
		engine.QueueSteering(llm.QueuedSteering{ID: id, Message: llm.UserText(id), DisplayText: id, Origin: llm.SteeringOriginUser})
	}
	return manager, engine, snapshot
}

func TestTUICancelRushBeforeAdmissionKeepsGuidanceAndAllowsNewRun(t *testing.T) {
	manager, engine, source := startSteeringTestRun(t, "session")
	command, err := manager.Rush("session", source.RunID, engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.Cancel("session")
	ready := command().(steeringReadyMsg)
	if ready.err == nil {
		t.Fatal("cancelled handoff succeeded")
	}
	// Explicit Stop and coordinator completion share asynchronous cleanup.
	// Wait for its owner to release instead of depending on scheduler order.
	deadline := time.Now().Add(3 * time.Second)
	for manager.steeringCoordinator("session") != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if engine.SteeringTransitioning() || manager.steeringCoordinator("session") != nil {
		t.Fatal("cancelled source left an engine freeze")
	}
	if len(engine.ListPendingSteering()) != 2 {
		t.Fatal("cancelled guidance lost")
	}
	if _, err := manager.Start("session", MainRunExecution{Execute: func(context.Context, func(ui.StreamEvent)) error { return nil }}); err != nil {
		t.Fatalf("session wedged after cancellation: %v", err)
	}
}

func TestTUICancelAfterInputCommitKeepsUnansweredUserRowsVisible(t *testing.T) {
	model := newTestChatModel(false)
	manager, engine, source := startSteeringTestRun(t, model.SessionID())
	model.mainRunManager = manager
	model.engine = engine
	model.mainRunID = source.RunID
	command, err := manager.Rush(model.SessionID(), source.RunID, engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := manager.steeringCoordinator(model.SessionID())
	model.steeringHandoff = coordinator.owner.OperationID
	ready := command().(steeringReadyMsg)
	if ready.err != nil {
		t.Fatal(ready.err)
	}
	manager.Cancel(model.SessionID())
	_, next := model.handleSteeringReady(ready)
	if next != nil {
		t.Fatal("Stop launched a replacement")
	}
	ids := map[string]int{}
	for _, message := range model.messages {
		ids[message.ClientMessageID]++
	}
	if ids["first"] != 1 || ids["second"] != 1 {
		t.Fatalf("committed guidance disappeared: %+v", ids)
	}
	if model.steeringHandoff != "" {
		t.Fatal("handoff flag remained stuck")
	}
	if _, err := manager.Start(model.SessionID(), MainRunExecution{SteeringOperationID: ready.operationID, Execute: func(context.Context, func(ui.StreamEvent)) error { return nil }}); err == nil {
		t.Fatal("stale replacement started after Stop")
	}
}

type steeringTestBridge struct {
	llm.Provider
	tool     llm.Tool
	requests chan llm.Request
	execute  func(context.Context, string, json.RawMessage) (llm.ToolOutput, error)
}

func (p *steeringTestBridge) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	if p.requests != nil {
		p.requests <- req
	}
	return p.Provider.Stream(ctx, req)
}

func (p *steeringTestBridge) SetToolExecutor(execute func(context.Context, string, json.RawMessage) (llm.ToolOutput, error)) {
	p.execute = execute
}

type steeringUncooperativeTool struct{ started, release chan struct{} }

func (t *steeringUncooperativeTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "ignores_cancel", Schema: map[string]any{"type": "object"}}
}
func (t *steeringUncooperativeTool) Preview(json.RawMessage) string { return "" }
func (t *steeringUncooperativeTool) Execute(context.Context, json.RawMessage) (llm.ToolOutput, error) {
	close(t.started)
	<-t.release
	return llm.TextOutput("done"), nil
}

func TestTUICancelRushWaitsForActualBridgedToolWithoutStore(t *testing.T) {
	tool := &steeringUncooperativeTool{started: make(chan struct{}), release: make(chan struct{})}
	released := false
	defer func() {
		if !released {
			close(tool.release)
		}
	}()
	bridge := &steeringTestBridge{tool: tool}
	manager, engine, source := startSteeringTestRun(t, "session", bridge)
	run := manager.runs["session"]
	go func() { _, _ = bridge.execute(run.ctx, tool.Spec().Name, json.RawMessage(`{}`)) }()
	select {
	case <-tool.started:
	case <-time.After(3 * time.Second):
		t.Fatal("tool did not start")
	}
	command, err := manager.Rush("session", source.RunID, engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan steeringReadyMsg, 1)
	go func() { ready <- command().(steeringReadyMsg) }()
	select {
	case <-run.done:
	case <-time.After(3 * time.Second):
		t.Fatal("Rush did not cancel source")
	}
	manager.Cancel("session")
	if !engine.SteeringTransitioning() {
		t.Fatal("Stop released freeze while actual tool still runs")
	}
	if _, err := manager.Start("session", MainRunExecution{Execute: func(context.Context, func(ui.StreamEvent)) error { return nil }}); err == nil {
		t.Fatal("replacement overlapped abandoned tool")
	}
	close(tool.release)
	released = true
	select {
	case msg := <-ready:
		if msg.err == nil {
			t.Fatal("Stop allowed continuation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handoff did not settle after actual completion")
	}
	deadline := time.Now().Add(3 * time.Second)
	for manager.steeringCoordinator("session") != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if engine.SteeringTransitioning() || manager.steeringCoordinator("session") != nil {
		t.Fatal("settled cleanup left the session frozen")
	}
	if len(engine.ListPendingSteering()) != 2 {
		t.Fatal("Stop lost guidance")
	}
}

// Drive both UI delivery orders, not just the manager coordinator: processing
// the source cancellation must not turn into an explicit Stop of the handoff.
func TestTUIRushContinuesThroughSourceTerminalUI(t *testing.T) {
	for _, durable := range []bool{false, true} {
		for _, terminalFirst := range []bool{false, true} {
			name := "memory"
			if durable {
				name = "durable"
			}
			if terminalFirst {
				name += "/terminal-first"
			} else {
				name += "/ready-first"
			}
			t.Run(name, func(t *testing.T) {
				model := newTestChatModel(true)
				tool := &steeringUncooperativeTool{started: make(chan struct{}), release: make(chan struct{})}
				released := false
				defer func() {
					if !released {
						close(tool.release)
					}
				}()
				bridge := &steeringTestBridge{tool: tool, requests: make(chan llm.Request, 4)}
				manager, engine, source := startSteeringTestRun(t, model.SessionID(), bridge)
				<-bridge.requests // Original request.
				model.mainRunManager = manager
				model.engine = engine
				model.provider = bridge
				if durable {
					store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: ":memory:"})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { manager.Close(time.Second); store.Close() })
					if err := store.Create(context.Background(), model.sess); err != nil {
						t.Fatal(err)
					}
					model.store = store
				}
				_ = model.attachMainRun(model.SessionID())
				defer model.detachMainRun()
				model.setTextareaValue("unsent draft")
				run := manager.runs[model.SessionID()]
				go func() { _, _ = bridge.execute(run.ctx, tool.Spec().Name, json.RawMessage(`{}`)) }()
				select {
				case <-tool.started:
				case <-time.After(3 * time.Second):
					t.Fatal("tool did not start")
				}
				_, rush := model.rushPendingSteering()
				if rush == nil {
					t.Fatal("Rush did not start")
				}
				ready := make(chan steeringReadyMsg, 1)
				go func() { ready <- rush().(steeringReadyMsg) }()
				select {
				case <-source.Done:
				case <-time.After(3 * time.Second):
					t.Fatal("source was not cancelled")
				}
				if terminalFirst {
					_, terminalCmd := model.Update(streamEventMsg{event: ui.ErrorEvent(context.Canceled), generation: model.streamGeneration, mainRunID: model.mainRunID, mainRunSubscription: model.mainRunSubscription})
					assertRushDoesNotClearScreen(t, terminalCmd)
				}
				close(tool.release)
				released = true
				var settled steeringReadyMsg
				select {
				case settled = <-ready:
				case <-time.After(3 * time.Second):
					t.Fatal("handoff did not settle")
				}
				if settled.err != nil {
					t.Fatalf("source cancellation aborted Rush: %v", settled.err)
				}
				_, next := model.Update(settled)
				if next == nil {
					t.Fatal("UI did not authorize replacement")
				}
				batch, ok := next().(tea.BatchMsg)
				if !ok || len(batch) == 0 {
					t.Fatal("missing replacement commands")
				}
				started, ok := batch[0]().(mainRunStartedMsg)
				if !ok {
					t.Fatal("replacement start was cancelled by source UI cleanup")
				}
				if started.runID == source.RunID {
					t.Fatal("replacement reused source run")
				}
				_, _ = model.Update(started)
				if model.steeringHandoff != "" {
					t.Fatal("handoff remained stuck")
				}
				if model.textarea.Value() != "unsent draft" {
					t.Fatal("Rush overwrote draft")
				}
				var request llm.Request
				select {
				case request = <-bridge.requests:
				case <-time.After(3 * time.Second):
					t.Fatal("replacement provider was never called")
				}
				for _, id := range []string{"first", "second"} {
					visible, prompt := 0, 0
					for _, row := range model.messages {
						if row.ClientMessageID == id {
							visible++
						}
					}
					for _, row := range request.Messages {
						if row.ClientMessageID == id {
							prompt++
						}
					}
					if visible != 1 || prompt != 1 {
						t.Fatalf("guidance %s visible/prompt counts=%d/%d, want 1/1", id, visible, prompt)
					}
				}
				replacement := manager.runs[model.SessionID()]
				select {
				case <-replacement.done:
				case <-time.After(3 * time.Second):
					t.Fatal("replacement did not finish")
				}
			})
		}
	}
}

func assertRushDoesNotClearScreen(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	if reflect.DeepEqual(msg, tea.ClearScreen()) {
		t.Fatal("Rush source cleanup blanks the terminal")
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			assertRushDoesNotClearScreen(t, child)
		}
	}
}

func TestTUIRushKeepsPendingVisibleUntilReadyIsPresented(t *testing.T) {
	model := newTestChatModel(true)
	_, _ = model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	manager, engine, source := startSteeringTestRun(t, model.SessionID())
	model.mainRunManager = manager
	model.engine = engine
	_ = model.attachMainRun(model.SessionID())
	defer model.detachMainRun()
	model.messages = append(model.messages, *session.NewMessage(model.SessionID(), llm.UserText("sleep 20"), -1))
	model.invalidateHistoryCache()
	_, rush := model.rushPendingSteering()
	if rush == nil {
		t.Fatal("Rush unavailable")
	}
	ready := rush().(steeringReadyMsg)
	if ready.err != nil {
		t.Fatal(ready.err)
	}
	// The worker has committed/transferred its queue, but Bubble Tea has not
	// delivered the ready message. A late source terminal event must not drop it.
	_, terminalCmd := model.Update(streamEventMsg{event: ui.ErrorEvent(context.Canceled), generation: model.streamGeneration, mainRunID: source.RunID, mainRunSubscription: model.mainRunSubscription})
	view := ui.StripANSI(model.View().Content)
	for _, text := range []string{"sleep 20", "first", "second"} {
		if !strings.Contains(view, text) {
			t.Fatalf("%q vanished before ready presentation: %s", text, view)
		}
	}
	assertRushDoesNotClearScreen(t, terminalCmd)
}

func TestRushInitialRowsImmediatelyInvalidateRenderedHistory(t *testing.T) {
	model := newTestChatModel(true)
	_, _ = model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model.messages = append(model.messages, *session.NewMessage(model.SessionID(), llm.UserText("sleep 20"), -1))
	model.invalidateHistoryCache()
	_ = model.View()
	row := session.NewMessage(model.SessionID(), llm.UserText("testing a steer"), -1)
	row.ClientMessageID = "guidance"
	model.appendSteeringInitialRows([]*session.Message{row})
	view := ui.StripANSI(model.View().Content)
	for _, text := range []string{"sleep 20", "testing a steer"} {
		if !strings.Contains(view, text) {
			t.Fatalf("%q absent before provider output: %s", text, view)
		}
	}
}
