package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func hostTestModels(t *testing.T) (*ConversationHost, *Model, *Model) {
	t.Helper()
	parent := newTestChatModel(true)
	parent.sess = &session.Session{ID: "parent", Kind: session.KindRoot, Status: session.StatusActive}
	parent.streamGeneration = 1
	parent.routedGeneration.Store(1)
	side := newTestChatModel(true)
	side.sess = &session.Session{ID: "side", Kind: session.KindSide, ParentID: "parent", RootID: "parent", SideState: session.SideOpen, Status: session.StatusActive}
	side.streamGeneration = 1
	side.routedGeneration.Store(1)
	host := NewConversationHost(parent, func(id, _ string) (*Model, error) {
		if id == "side" {
			return side, nil
		}
		return nil, context.Canceled
	})
	return host, parent, side
}

func TestConversationHostRoutesSimultaneousEqualGenerationStreams(t *testing.T) {
	host, parent, side := hostTestModels(t)
	_, cmd := host.Update(ConversationNavigationMsg{SessionID: "side"})
	if cmd != nil {
		_ = cmd() // Init is irrelevant to this direct routing test.
	}
	parent.streaming, side.streaming = true, true

	_, _ = host.Update(RoutedConversationMsg{ConversationID: "parent", Generation: 1, Msg: streamEventMsg{generation: 1, event: ui.TextEvent("parent-only")}})
	_, _ = host.Update(RoutedConversationMsg{ConversationID: "side", Generation: 1, Msg: streamEventMsg{generation: 1, event: ui.TextEvent("side-only")}})
	_, _ = host.Update(RoutedConversationMsg{ConversationID: "parent", Generation: 1, Msg: ui.SmoothTickMsg{}})
	_, _ = host.Update(RoutedConversationMsg{ConversationID: "side", Generation: 1, Msg: ui.SmoothTickMsg{}})

	if got := parent.currentResponse.String(); !strings.Contains(got, "parent-only") || strings.Contains(got, "side-only") {
		t.Fatalf("parent stream crossed routes: %q", got)
	}
	if got := side.currentResponse.String(); !strings.Contains(got, "side-only") || strings.Contains(got, "parent-only") {
		t.Fatalf("side stream crossed routes: %q", got)
	}
}

func TestConversationHostDrainsInactiveCompletionAndPersistsStatus(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Path: t.TempDir() + "/sessions.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, sess := range []*session.Session{
		{ID: "parent", Kind: session.KindRoot, Status: session.StatusActive},
		{ID: "side", Kind: session.KindSide, ParentID: "parent", RootID: "parent", SideState: session.SideOpen, Status: session.StatusActive},
	} {
		if err := store.Create(context.Background(), sess); err != nil {
			t.Fatal(err)
		}
	}
	parent := newTestChatModel(true)
	parent.store, parent.sess, parent.streaming, parent.streamGeneration = store, &session.Session{ID: "parent", Kind: session.KindRoot}, true, 3
	parent.routedGeneration.Store(3)
	parent.streamStartTime = time.Now()
	side := newTestChatModel(true)
	side.store, side.sess = store, &session.Session{ID: "side", Kind: session.KindSide, ParentID: "parent", RootID: "parent", SideState: session.SideOpen}
	host := NewConversationHost(parent, func(string, string) (*Model, error) { return side, nil })
	_, _ = host.Update(ConversationNavigationMsg{SessionID: "side"})
	_, _ = host.Update(RoutedConversationMsg{ConversationID: "parent", Generation: 3, Msg: streamEventMsg{generation: 3, event: ui.DoneEvent(0)}})

	if parent.streaming {
		t.Fatal("inactive parent completion was not drained")
	}
	persisted, err := store.Get(context.Background(), "parent")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != session.StatusComplete {
		t.Fatalf("parent status = %q, want complete", persisted.Status)
	}
	if host.ActiveConversationID() != "side" {
		t.Fatal("inactive completion changed active viewport")
	}
}

func TestConversationHostKeepsSimultaneousPendingInteractionsAddressed(t *testing.T) {
	host, parent, side := hostTestModels(t)
	_, _ = host.Update(ConversationNavigationMsg{SessionID: "side"})
	approvalDone := make(chan tools.ApprovalResult, 1)
	askDone := make(chan []tools.AskUserAnswer, 1)
	_, _ = host.Update(RoutedConversationMsg{ConversationID: "parent", Generation: 1, Msg: ApprovalRequestMsg{Path: "/tmp/parent", IsWrite: true, DoneCh: approvalDone}})
	_, _ = host.Update(RoutedConversationMsg{ConversationID: "side", Generation: 1, Msg: AskUserRequestMsg{Questions: []tools.AskUserQuestion{{Header: "Side", Question: "Pick", Options: []tools.AskUserOption{{Label: "A", Description: "a"}, {Label: "B", Description: "b"}}}}, DoneCh: askDone}})

	if parent.approvalModel == nil || side.askUserModel == nil {
		t.Fatalf("pending interactions were lost: parent=%v side=%v", parent.approvalModel != nil, side.askUserModel != nil)
	}
	if got := parent.RuntimeStatus(); got != "needs approval" {
		t.Fatalf("parent status = %q", got)
	}
	_ = host.View()
	if side.parentRuntimeStatus != "needs approval" {
		t.Fatalf("side parent status = %q", side.parentRuntimeStatus)
	}
	_, _ = host.Update(ConversationNavigationMsg{SessionID: "parent"})
	if host.ActiveRuntime().approvalModel == nil {
		t.Fatal("switching back did not surface parent approval")
	}
	_ = host.View()
	if parent.sideRuntimeStatus != "needs input" {
		t.Fatalf("main did not surface side attention: %q", parent.sideRuntimeStatus)
	}

	host.Shutdown()
	select {
	case result := <-approvalDone:
		if !result.Cancelled {
			t.Fatalf("approval shutdown result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("approval caller remained blocked on exit")
	}
	select {
	case answers := <-askDone:
		if answers != nil {
			t.Fatalf("ask shutdown answers = %#v", answers)
		}
	case <-time.After(time.Second):
		t.Fatal("ask_user caller remained blocked on exit")
	}
}

func TestConversationHostCloseSideOnlyAndNavigationDoesNotImplicitlyClose(t *testing.T) {
	host, parent, side := hostTestModels(t)
	parentCancel, sideCancel := false, false
	parent.runtimeCancel = func() { parentCancel = true }
	side.runtimeCancel = func() { sideCancel = true }
	_, _ = host.Update(ConversationNavigationMsg{SessionID: "side"})
	_, _ = host.Update(ConversationNavigationMsg{SessionID: "parent"})
	if sideCancel || host.Runtime("side") == nil {
		t.Fatal("/main-style navigation implicitly closed side")
	}
	_, _ = host.Update(ConversationNavigationMsg{SessionID: "parent", CloseID: "side"})
	if !sideCancel || parentCancel {
		t.Fatalf("close cancellation side=%v parent=%v", sideCancel, parentCancel)
	}
	if host.Runtime("side") != nil || host.ActiveConversationID() != "parent" {
		t.Fatal("side runtime was not removed and parent restored")
	}
}

func TestConversationHostPreservesRoutedSequences(t *testing.T) {
	msg := tea.Sequence(func() tea.Msg { return "first" }, func() tea.Msg { return "second" })()
	cmds, ok := sequenceCommands(msg)
	if !ok || len(cmds) != 2 {
		t.Fatalf("sequence extraction = %d, %v", len(cmds), ok)
	}
}

func TestConversationHostDropsInactiveTerminalControlMessages(t *testing.T) {
	host, parent, _ := hostTestModels(t)
	_, _ = host.Update(ConversationNavigationMsg{SessionID: "side"})
	for _, msg := range []tea.Msg{tea.Quit(), tea.Println("inactive flush")()} {
		_, cmd := host.Update(RoutedConversationMsg{ConversationID: parent.RuntimeRoutingID(), Generation: parent.StreamGeneration(), Msg: msg})
		if cmd != nil {
			t.Fatalf("inactive control message %T escaped routing", msg)
		}
	}
}

func TestConversationHostPostFrameImagesFollowActiveRuntime(t *testing.T) {
	host, parent, side := hostTestModels(t)
	parent.postFrameImageSeq = "main-image"
	side.postFrameImageSeq = "side-image"
	_, _ = host.Update(ConversationNavigationMsg{SessionID: "side"})
	if got := host.TakePostFrameImageSequence(); got != "side-image" {
		t.Fatalf("active image sequence = %q", got)
	}
	if parent.postFrameImageSeq != "main-image" {
		t.Fatal("inactive main image sequence was drained")
	}
}

func TestConversationHostRoutesAfterClearAndNewSessionReplacement(t *testing.T) {
	for _, command := range []struct {
		name string
		run  func(*Model) (tea.Model, tea.Cmd)
	}{
		{name: "clear", run: func(m *Model) (tea.Model, tea.Cmd) { return m.cmdClear() }},
		{name: "new", run: func(m *Model) (tea.Model, tea.Cmd) { return m.cmdNew() }},
	} {
		t.Run(command.name, func(t *testing.T) {
			store, err := session.NewSQLiteStore(session.Config{Path: t.TempDir() + "/sessions.db"})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			old := &session.Session{ID: "original", Kind: session.KindRoot, Status: session.StatusActive}
			if err := store.Create(context.Background(), old); err != nil {
				t.Fatal(err)
			}
			model := newTestChatModel(true)
			model.store, model.sess = store, old
			host := NewConversationHost(model, nil)
			routeID := model.RuntimeRoutingID()
			oldGeneration := model.StreamGeneration()

			_, _ = command.run(model)
			if model.ConversationID() == routeID {
				t.Fatal("command did not replace persisted session ID")
			}
			_, _ = host.Update(RoutedConversationMsg{ConversationID: routeID, Generation: oldGeneration, Msg: ApprovalRequestMsg{Path: "/tmp/stale", IsWrite: true, DoneCh: make(chan tools.ApprovalResult, 1)}})
			if model.approvalModel != nil {
				t.Fatal("session replacement accepted a stale interactive event")
			}
			approvalDone := make(chan tools.ApprovalResult, 1)
			_, _ = host.Update(RoutedConversationMsg{ConversationID: routeID, Generation: model.StreamGeneration(), Msg: ApprovalRequestMsg{Path: "/tmp/write", IsWrite: true, DoneCh: approvalDone}})
			if model.approvalModel == nil {
				t.Fatal("approval routed with stable runtime identity was lost")
			}
			askDone := make(chan []tools.AskUserAnswer, 1)
			_, _ = host.Update(RoutedConversationMsg{ConversationID: routeID, Generation: model.StreamGeneration(), Msg: AskUserRequestMsg{Questions: []tools.AskUserQuestion{{Header: "Pick", Question: "Choose", Options: []tools.AskUserOption{{Label: "A"}, {Label: "B"}}}}, DoneCh: askDone}})
			if model.askUserModel == nil {
				t.Fatal("ask_user routed after session replacement was lost")
			}
			host.Shutdown()
		})
	}
}

func TestConversationHostRoutingIDIsRaceSafeAcrossSessionReplacement(t *testing.T) {
	model := newTestChatModel(true)
	model.sess = &session.Session{ID: "original", Kind: session.KindRoot}
	_ = NewConversationHost(model, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			if model.RuntimeRoutingID() != "original" {
				t.Errorf("routing ID changed")
				return
			}
		}
	}()
	_, _ = model.cmdClear()
	<-done
}

func TestConversationHostAutoSendsWhenSideRuntimeAlreadyExists(t *testing.T) {
	host, _, side := hostTestModels(t)
	_, initCmd := host.Update(ConversationNavigationMsg{SessionID: "side"})
	if initCmd != nil {
		_ = initCmd()
	}
	_, cmd := host.Update(ConversationNavigationMsg{SessionID: "side", AutoSend: "follow up"})
	if cmd == nil || side.textarea.Value() != "follow up" {
		t.Fatalf("existing side auto-send was not queued: value=%q cmd=%v", side.textarea.Value(), cmd != nil)
	}
	routed, ok := cmd().(RoutedConversationMsg)
	if !ok {
		t.Fatalf("auto-send command was not routed: %T", cmd())
	}
	if _, ok := routed.Msg.(autoSendMsg); !ok {
		t.Fatalf("auto-send message = %T", routed.Msg)
	}
}

func TestConversationHostRejectsStaleInteractiveGeneration(t *testing.T) {
	host, parent, _ := hostTestModels(t)
	parent.routedGeneration.Store(2)
	_, _ = host.Update(RoutedConversationMsg{ConversationID: "parent", Generation: 1, Msg: ApprovalRequestMsg{Path: "/tmp/stale", IsWrite: true, DoneCh: make(chan tools.ApprovalResult, 1)}})
	if parent.approvalModel != nil {
		t.Fatal("stale approval event reached a newer runtime generation")
	}
}

func TestConversationFactoryCanOwnIndependentRuntimeGraphs(t *testing.T) {
	parent, side := newTestChatModel(true), newTestChatModel(true)
	parent.sess, side.sess = &session.Session{ID: "parent"}, &session.Session{ID: "side", Kind: session.KindSide}
	parent.provider, side.provider = llm.NewMockProvider("parent"), llm.NewMockProvider("side")
	parent.engine, side.engine = llm.NewEngine(parent.provider, nil), llm.NewEngine(side.provider, nil)
	parent.mcpManager, side.mcpManager = mcp.NewManager(), mcp.NewManager()
	parent.toolMgr = &tools.ToolManager{}
	side.toolMgr = &tools.ToolManager{}
	if parent.provider == side.provider || parent.engine == side.engine || parent.engine.Tools() == side.engine.Tools() || parent.mcpManager == side.mcpManager || parent.toolMgr == side.toolMgr {
		t.Fatal("conversation runtimes share mutable provider/engine/registry/MCP/tool state")
	}
}
