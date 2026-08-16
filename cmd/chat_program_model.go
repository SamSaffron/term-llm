package cmd

import (
	"reflect"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

// chatProgramModel keeps one Bubble Tea program alive while its visible chat
// model changes sessions. Commands are scoped to the child model that created
// them: without this boundary, a delayed stream/timer result from the outgoing
// session can be delivered to the replacement model and mutate the wrong
// conversation.
type chatProgramModel struct {
	model      *chat.Model
	generation uint64
}

type chatProgramScopedMsg struct {
	generation uint64
	message    tea.Msg
}

func newChatProgramModel(model *chat.Model) *chatProgramModel {
	return &chatProgramModel{model: model, generation: 1}
}

func (m *chatProgramModel) Init() tea.Cmd {
	if m == nil || m.model == nil {
		return nil
	}
	return scopeChatProgramCmd(m.model.Init(), m.generation)
}

func (m *chatProgramModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if m == nil || m.model == nil {
		return m, nil
	}
	if scoped, ok := message.(chatProgramScopedMsg); ok {
		if scoped.generation != m.generation {
			return m, nil
		}
		message = scoped.message
	}
	updated, cmd := m.model.Update(message)
	next, ok := updated.(*chat.Model)
	if !ok || next == nil {
		return m, scopeChatProgramCmd(cmd, m.generation)
	}
	if next != m.model {
		m.model = next
		m.generation++
	}
	return m, scopeChatProgramCmd(cmd, m.generation)
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
