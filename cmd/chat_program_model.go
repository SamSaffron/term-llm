package cmd

import (
	"fmt"
	"reflect"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/lifecycle"
	"github.com/samsaffron/term-llm/internal/termhost"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

// chatProgramChild is the lifecycle-bearing Bubble Tea model currently visible
// inside the stable chat program host.
type chatProgramChild interface {
	tea.Model
	LifecycleSnapshot() lifecycle.Snapshot
}

type lifecycleSnapshotReporter interface {
	Report(lifecycle.Snapshot) termhost.Control
}

type chatLifecycleReporter interface {
	lifecycleSnapshotReporter
	RestoreOSC() string
	Close()
}

// chatProgramModel keeps one Bubble Tea program alive while its visible chat
// model changes sessions. Commands are scoped to the child model that created
// them: without this boundary, a delayed stream/timer result from the outgoing
// session can be delivered to the replacement model and mutate the wrong
// conversation.
type chatProgramModel struct {
	model      chatProgramChild
	generation uint64
	reporter   lifecycleSnapshotReporter

	lifecycleReported      bool
	lifecycleSnapshot      lifecycle.Snapshot
	lifecycleOSCGeneration uint64
	initialLifecycleCmd    tea.Cmd
}

type lifecycleOSCRefreshMsg struct{ generation uint64 }

type chatProgramScopedMsg struct {
	generation uint64
	message    tea.Msg
}

func newChatProgramModel(model *chat.Model, reporter lifecycleSnapshotReporter) *chatProgramModel {
	return newChatProgramModelForChild(model, reporter)
}

// newChatProgramModelForChild keeps the host testable without weakening the
// production constructor's concrete *chat.Model identity.
func newChatProgramModelForChild(model chatProgramChild, reporter lifecycleSnapshotReporter) *chatProgramModel {
	host := &chatProgramModel{model: model, generation: 1, reporter: reporter}
	host.initialLifecycleCmd = host.publishLifecycle(false)
	return host
}

func finalChatModel(model tea.Model) (*chat.Model, error) {
	// The concrete production constructor installs a stable *chatProgramModel
	// containing only *chat.Model children. Reaching another type means Bubble
	// Tea's session-switch invariant was violated; failing hard is safer than
	// silently reading relaunch state from a stale or unknown model.
	host, ok := model.(*chatProgramModel)
	if !ok || host == nil {
		return nil, fmt.Errorf("chat program returned unexpected final host %T", model)
	}
	visible, ok := host.model.(*chat.Model)
	if !ok || visible == nil {
		return nil, fmt.Errorf("chat program host returned unexpected final child %T", host.model)
	}
	return visible, nil
}

func (m *chatProgramModel) Init() tea.Cmd {
	if m == nil || m.model == nil {
		return nil
	}
	return batchChatProgramCmds(scopeChatProgramCmd(m.model.Init(), m.generation), m.initialLifecycleCmd)
}

func (m *chatProgramModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if m == nil || m.model == nil {
		return m, nil
	}
	if refresh, ok := message.(lifecycleOSCRefreshMsg); ok {
		if refresh.generation != m.lifecycleOSCGeneration {
			return m, nil
		}
		return m, m.publishLifecycle(true)
	}
	if scoped, ok := message.(chatProgramScopedMsg); ok {
		if scoped.generation != m.generation {
			return m, nil
		}
		message = scoped.message
	}
	updated, cmd := m.model.Update(message)
	next, ok := updated.(chatProgramChild)
	if !ok || next == nil {
		return m, batchChatProgramCmds(scopeChatProgramCmd(cmd, m.generation), m.publishLifecycle(false))
	}
	if !sameChatProgramChild(next, m.model) {
		m.model = next
		m.generation++
	}
	return m, batchChatProgramCmds(scopeChatProgramCmd(cmd, m.generation), m.publishLifecycle(false))
}

func sameChatProgramChild(left, right chatProgramChild) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	if leftValue.Type() != rightValue.Type() || !leftValue.Comparable() {
		return false
	}
	return leftValue.Equal(rightValue)
}

func (m *chatProgramModel) publishLifecycle(forceOSC bool) tea.Cmd {
	if m == nil || m.model == nil || m.reporter == nil {
		return nil
	}
	snapshot := m.model.LifecycleSnapshot()
	if !forceOSC && m.lifecycleReported && snapshot == m.lifecycleSnapshot {
		return nil
	}
	if !forceOSC {
		m.lifecycleReported = true
		m.lifecycleSnapshot = snapshot
		m.lifecycleOSCGeneration++
	}
	return lifecycleControlCmd(m.reporter.Report(snapshot), m.lifecycleOSCGeneration)
}

func lifecycleControlCmd(control termhost.Control, generation uint64) tea.Cmd {
	var commands []tea.Cmd
	if control.Sequence != "" {
		commands = append(commands, tea.Raw(control.Sequence))
	}
	if control.RefreshAfter > 0 {
		commands = append(commands, tea.Tick(control.RefreshAfter, func(time.Time) tea.Msg {
			return lifecycleOSCRefreshMsg{generation: generation}
		}))
	}
	return batchChatProgramCmds(commands...)
}

func batchChatProgramCmds(commands ...tea.Cmd) tea.Cmd {
	filtered := commands[:0]
	for _, command := range commands {
		if command != nil {
			filtered = append(filtered, command)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return tea.Batch(filtered...)
	}
}

func (m *chatProgramModel) View() tea.View {
	if m == nil || m.model == nil {
		return tea.NewView("")
	}
	view := m.model.View()
	generation := m.generation
	if postFrame := view.PostFrameMsg; postFrame != nil {
		view.PostFrameMsg = func(err error) tea.Msg {
			message := postFrame(err)
			if message == nil {
				return nil
			}
			return chatProgramScopedMsg{generation: generation, message: message}
		}
	}
	if onMouse := view.OnMouse; onMouse != nil {
		view.OnMouse = func(message tea.MouseMsg) tea.Cmd {
			return scopeChatProgramCmd(onMouse(message), generation)
		}
	}
	return view
}

// scopeChatProgramCmd recursively preserves Bubble Tea's control messages
// (Batch, Sequence, Quit, Exec, renderer commands) while tagging application
// messages with their originating child generation.
func scopeChatProgramCmd(cmd tea.Cmd, generation uint64) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		message := cmd()
		if message == nil {
			return nil
		}
		if batch, ok := message.(tea.BatchMsg); ok {
			scoped := make(tea.BatchMsg, 0, len(batch))
			for _, nested := range batch {
				if nested != nil {
					scoped = append(scoped, scopeChatProgramCmd(nested, generation))
				}
			}
			return scoped
		}

		messageType := reflect.TypeOf(message)
		if messageType != nil && messageType.PkgPath() == "charm.land/bubbletea/v2" {
			// Sequence's concrete message type is intentionally unexported. Rebuild
			// it with scoped child commands; all other Bubble Tea-owned messages
			// must remain visible to the framework unchanged.
			if messageType.Name() == "sequenceMsg" {
				value := reflect.ValueOf(message)
				nested := make([]tea.Cmd, 0, value.Len())
				for i := 0; i < value.Len(); i++ {
					child, ok := value.Index(i).Interface().(tea.Cmd)
					if ok && child != nil {
						nested = append(nested, scopeChatProgramCmd(child, generation))
					}
				}
				return tea.Sequence(nested...)()
			}
			return message
		}
		return chatProgramScopedMsg{generation: generation, message: message}
	}
}
