package cmd

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/lifecycle"
	"github.com/samsaffron/term-llm/internal/termhost"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

type chatProgramTestMsg struct{ value string }

type lifecycleProgramTestReporter struct {
	reports  []lifecycle.Snapshot
	controls []termhost.Control
}

func (r *lifecycleProgramTestReporter) Report(snapshot lifecycle.Snapshot) termhost.Control {
	r.reports = append(r.reports, snapshot)
	if len(r.controls) == 0 {
		return termhost.Control{}
	}
	control := r.controls[0]
	r.controls = r.controls[1:]
	return control
}

type lifecycleProgramTestModel struct {
	snapshot    lifecycle.Snapshot
	replacement *lifecycleProgramTestModel
}

type lifecycleProgramStartSwitchMsg struct{}
type lifecycleProgramReplacementReadyMsg struct{ next *lifecycleProgramTestModel }
type lifecycleProgramSetSnapshotMsg struct{ snapshot lifecycle.Snapshot }

type uncomparableLifecycleProgramModel []lifecycle.Snapshot

func (m uncomparableLifecycleProgramModel) Init() tea.Cmd { return nil }
func (m uncomparableLifecycleProgramModel) Update(tea.Msg) (tea.Model, tea.Cmd) {
	return m, nil
}
func (m uncomparableLifecycleProgramModel) View() tea.View { return tea.NewView("") }
func (m uncomparableLifecycleProgramModel) LifecycleSnapshot() lifecycle.Snapshot {
	return m[0]
}

func (m *lifecycleProgramTestModel) Init() tea.Cmd { return nil }

func (m *lifecycleProgramTestModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case lifecycleProgramStartSwitchMsg:
		next := m.replacement
		return m, func() tea.Msg { return lifecycleProgramReplacementReadyMsg{next: next} }
	case lifecycleProgramReplacementReadyMsg:
		return message.next, nil
	case lifecycleProgramSetSnapshotMsg:
		m.snapshot = message.snapshot
	}
	return m, nil
}

func (m *lifecycleProgramTestModel) View() tea.View { return tea.NewView("") }

func (m *lifecycleProgramTestModel) LifecycleSnapshot() lifecycle.Snapshot {
	return m.snapshot
}

func TestScopeChatProgramCmdTagsApplicationMessage(t *testing.T) {
	message := scopeChatProgramCmd(func() tea.Msg { return chatProgramTestMsg{value: "old"} }, 7)()
	scoped, ok := message.(chatProgramScopedMsg)
	if !ok {
		t.Fatalf("scoped command returned %T", message)
	}
	if scoped.generation != 7 || scoped.message != (chatProgramTestMsg{value: "old"}) {
		t.Fatalf("scoped message = %#v", scoped)
	}
}

func TestScopeChatProgramCmdRecursesThroughBatchAndSequence(t *testing.T) {
	leaf := func(value string) tea.Cmd {
		return func() tea.Msg { return chatProgramTestMsg{value: value} }
	}
	message := scopeChatProgramCmd(tea.Batch(
		leaf("batch"),
		tea.Sequence(leaf("first"), leaf("second")),
	), 3)()
	batch, ok := message.(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("scoped batch = %T len=%d", message, len(batch))
	}
	if _, ok := batch[0]().(chatProgramScopedMsg); !ok {
		t.Fatalf("batch leaf returned %T", batch[0]())
	}

	sequenceMessage := batch[1]()
	sequenceType := ""
	if sequenceMessage != nil {
		sequenceType = typeName(sequenceMessage)
	}
	if sequenceType != "sequenceMsg" {
		t.Fatalf("sequence control message became %T (%s)", sequenceMessage, sequenceType)
	}
}

func TestScopeChatProgramCmdPreservesBubbleTeaControlMessage(t *testing.T) {
	message := scopeChatProgramCmd(tea.Quit, 5)()
	if _, ok := message.(tea.QuitMsg); !ok {
		t.Fatalf("quit command returned %T", message)
	}
}

func TestNewChatProgramModelPreservesConcreteModelAndPublishesInitialLifecycle(t *testing.T) {
	model := &chat.Model{}
	reporter := &lifecycleProgramTestReporter{}
	host := newChatProgramModel(model, reporter)

	final, err := finalChatModel(host)
	if err != nil {
		t.Fatalf("finalChatModel() error = %v", err)
	}
	if final != model {
		t.Fatalf("final model = %p, want original model %p", final, model)
	}
	want := []lifecycle.Snapshot{{State: lifecycle.Idle}}
	if !reflect.DeepEqual(reporter.reports, want) {
		t.Fatalf("initial reports = %#v, want %#v", reporter.reports, want)
	}
}

func TestFinalChatModelRejectsUnexpectedChild(t *testing.T) {
	host := newChatProgramModelForChild(&lifecycleProgramTestModel{}, nil)
	if _, err := finalChatModel(host); err == nil {
		t.Fatal("finalChatModel() accepted non-chat child")
	}
}

func TestChatProgramModelHandlesUncomparableChild(t *testing.T) {
	child := uncomparableLifecycleProgramModel{{State: lifecycle.Idle}}
	host := newChatProgramModelForChild(child, nil)

	host.Update(struct{}{})
	if host.generation != 2 {
		t.Fatalf("generation = %d, want conservative replacement generation 2", host.generation)
	}
}

func TestChatProgramModelPublishesInitialStateAndDeduplicatesUpdates(t *testing.T) {
	initial := lifecycle.Snapshot{State: lifecycle.Idle, SessionID: "session-a"}
	working := lifecycle.Snapshot{State: lifecycle.Working, SessionID: "session-a", Message: "Generating response"}
	child := &lifecycleProgramTestModel{snapshot: initial}
	reporter := &lifecycleProgramTestReporter{}
	host := newChatProgramModelForChild(child, reporter)

	if !reflect.DeepEqual(reporter.reports, []lifecycle.Snapshot{initial}) {
		t.Fatalf("initial reports = %#v", reporter.reports)
	}
	host.Update(struct{}{})
	if len(reporter.reports) != 1 {
		t.Fatalf("unchanged Update published %d reports, want 1", len(reporter.reports))
	}
	host.Update(lifecycleProgramSetSnapshotMsg{snapshot: working})
	if !reflect.DeepEqual(reporter.reports, []lifecycle.Snapshot{initial, working}) {
		t.Fatalf("reports after state change = %#v", reporter.reports)
	}
	host.Update(struct{}{})
	if len(reporter.reports) != 2 {
		t.Fatalf("second unchanged Update published %d reports, want 2", len(reporter.reports))
	}
}

func TestChatProgramModelTransfersLifecycleOnlyWhenReplacementIsUpdated(t *testing.T) {
	oldSnapshot := lifecycle.Snapshot{State: lifecycle.Working, SessionID: "old", Message: "Generating response"}
	newSnapshot := lifecycle.Snapshot{State: lifecycle.Idle, SessionID: "new"}
	next := &lifecycleProgramTestModel{snapshot: newSnapshot}
	old := &lifecycleProgramTestModel{snapshot: oldSnapshot, replacement: next}
	reporter := &lifecycleProgramTestReporter{}
	host := newChatProgramModelForChild(old, reporter)

	_, switchCmd := host.Update(lifecycleProgramStartSwitchMsg{})
	if switchCmd == nil {
		t.Fatal("switch Update returned no command")
	}
	if !reflect.DeepEqual(reporter.reports, []lifecycle.Snapshot{oldSnapshot}) {
		t.Fatalf("switch preparation transferred authority: %#v", reporter.reports)
	}

	ready := switchCmd()
	if !reflect.DeepEqual(reporter.reports, []lifecycle.Snapshot{oldSnapshot}) {
		t.Fatalf("command goroutine transferred authority: %#v", reporter.reports)
	}
	host.Update(ready)
	if !reflect.DeepEqual(reporter.reports, []lifecycle.Snapshot{oldSnapshot, newSnapshot}) {
		t.Fatalf("replacement reports = %#v", reporter.reports)
	}
	if host.model != next || host.generation != 2 {
		t.Fatalf("visible model=%p generation=%d, want next model and generation 2", host.model, host.generation)
	}
}

func TestChatProgramModelOldModelCannotPublishAfterReplacement(t *testing.T) {
	oldSnapshot := lifecycle.Snapshot{State: lifecycle.Idle, SessionID: "old"}
	newSnapshot := lifecycle.Snapshot{State: lifecycle.Idle, SessionID: "new"}
	next := &lifecycleProgramTestModel{snapshot: newSnapshot}
	old := &lifecycleProgramTestModel{snapshot: oldSnapshot, replacement: next}
	reporter := &lifecycleProgramTestReporter{}
	host := newChatProgramModelForChild(old, reporter)

	_, cmd := host.Update(lifecycleProgramStartSwitchMsg{})
	host.Update(cmd())
	want := []lifecycle.Snapshot{oldSnapshot, newSnapshot}

	stale := lifecycle.Snapshot{State: lifecycle.Blocked, SessionID: "old", Message: "Waiting for approval"}
	old.Update(lifecycleProgramSetSnapshotMsg{snapshot: stale})
	host.Update(chatProgramScopedMsg{generation: 1, message: lifecycleProgramSetSnapshotMsg{snapshot: stale}})
	if !reflect.DeepEqual(reporter.reports, want) {
		t.Fatalf("old model published after replacement: %#v, want %#v", reporter.reports, want)
	}
}

func TestChatProgramModelDropsStaleOSCRefreshTick(t *testing.T) {
	child := &lifecycleProgramTestModel{snapshot: lifecycle.Snapshot{State: lifecycle.Idle}}
	reporter := &lifecycleProgramTestReporter{}
	host := newChatProgramModelForChild(child, reporter)
	oldGeneration := host.lifecycleOSCGeneration
	host.Update(lifecycleProgramSetSnapshotMsg{snapshot: lifecycle.Snapshot{State: lifecycle.Working}})
	if host.lifecycleOSCGeneration == oldGeneration {
		t.Fatal("lifecycle state change did not advance OSC generation")
	}
	reportCount := len(reporter.reports)
	_, command := host.Update(lifecycleOSCRefreshMsg{generation: oldGeneration})
	if command != nil || len(reporter.reports) != reportCount {
		t.Fatalf("stale refresh command=%v reports=%d, want nil/%d", command != nil, len(reporter.reports), reportCount)
	}
}

func TestChatProgramModelReturnsLifecycleOSCThroughTeaRawAndNeverView(t *testing.T) {
	child := &lifecycleProgramTestModel{snapshot: lifecycle.Snapshot{State: lifecycle.Working}}
	reporter := &lifecycleProgramTestReporter{controls: []termhost.Control{{Sequence: "\x1b]9;4;3\x07"}}}
	host := newChatProgramModelForChild(child, reporter)
	raw := strings.Join(rawLifecycleStrings(host.Init()), "")
	if raw != "\x1b]9;4;3\x07" {
		t.Fatalf("Init lifecycle raw = %q", raw)
	}
	if strings.Contains(host.View().Content, "\x1b]9;4;") {
		t.Fatalf("View contains lifecycle OSC: %q", host.View().Content)
	}
}

func rawLifecycleStrings(command tea.Cmd) []string {
	if command == nil {
		return nil
	}
	message := command()
	switch typed := message.(type) {
	case tea.RawMsg:
		return []string{fmt.Sprint(typed.Msg)}
	case tea.BatchMsg:
		var values []string
		for _, nested := range typed {
			values = append(values, rawLifecycleStrings(nested)...)
		}
		return values
	default:
		return nil
	}
}

func typeName(value any) string {
	typeInfo := reflect.TypeOf(value)
	if typeInfo == nil {
		return ""
	}
	return typeInfo.Name()
}
