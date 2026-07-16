package chat

import (
	"fmt"
	"reflect"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// ConversationNavigationMsg requests an in-process conversation switch. CloseID
// is optional and identifies a runtime that must be cancelled and removed.
type ConversationNavigationMsg struct {
	SessionID string
	AutoSend  string
	CloseID   string
}

// ConversationFactory creates an independently owned chat runtime.
type ConversationFactory func(sessionID, autoSend string) (*Model, error)

// RoutedConversationMsg prevents asynchronous messages from one runtime being
// consumed by another runtime with an equal stream generation.
type RoutedConversationMsg struct {
	ConversationID string
	Generation     uint64
	Msg            tea.Msg
}

// ConversationHost owns all chat models participating in one TUI process. Only
// the active model is rendered, but commands emitted by every model remain
// scheduled and are continuously routed back to their owner.
type ConversationHost struct {
	runtimes map[string]*Model
	activeID string
	mainID   string
	factory  ConversationFactory
	width    int
	height   int
	err      error
}

func NewConversationHost(main *Model, factory ConversationFactory) *ConversationHost {
	h := &ConversationHost{runtimes: make(map[string]*Model), factory: factory}
	if main != nil {
		id := main.ConversationID()
		h.mainID, h.activeID = id, id
		h.install(id, main)
	}
	return h
}

func (h *ConversationHost) install(id string, model *Model) {
	if model == nil || strings.TrimSpace(id) == "" {
		return
	}
	model.EnableConversationNavigation(true)
	h.runtimes[id] = model
}

func (h *ConversationHost) Init() tea.Cmd {
	if model := h.runtimes[h.activeID]; model != nil {
		return h.routeCmd(h.activeID, model.streamGeneration, model.Init())
	}
	return nil
}

func (h *ConversationHost) routeCmd(id string, generation uint64, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		return RoutedConversationMsg{ConversationID: id, Generation: generation, Msg: cmd()}
	}
}

func sequenceCommands(msg tea.Msg) ([]tea.Cmd, bool) {
	value := reflect.ValueOf(msg)
	if !value.IsValid() || value.Kind() != reflect.Slice || value.Type().PkgPath() != "charm.land/bubbletea/v2" || value.Type().Name() != "sequenceMsg" {
		return nil, false
	}
	cmds := make([]tea.Cmd, 0, value.Len())
	for i := 0; i < value.Len(); i++ {
		cmd, ok := value.Index(i).Interface().(tea.Cmd)
		if !ok {
			return nil, false
		}
		cmds = append(cmds, cmd)
	}
	return cmds, true
}

func isBubbleTeaControlMessage(msg tea.Msg) bool {
	typeOf := reflect.TypeOf(msg)
	if typeOf == nil {
		return false
	}
	if typeOf.Kind() == reflect.Pointer {
		typeOf = typeOf.Elem()
	}
	return typeOf.PkgPath() == "charm.land/bubbletea/v2"
}

func (h *ConversationHost) updateRuntime(id string, msg tea.Msg) tea.Cmd {
	model := h.runtimes[id]
	if model == nil || msg == nil {
		return nil
	}
	updated, cmd := model.Update(msg)
	if next, ok := updated.(*Model); ok {
		h.runtimes[id] = next
		model = next
	}
	return h.routeCmd(id, model.streamGeneration, cmd)
}

func (h *ConversationHost) navigate(msg ConversationNavigationMsg) tea.Cmd {
	if closeID := strings.TrimSpace(msg.CloseID); closeID != "" {
		if runtime := h.runtimes[closeID]; runtime != nil {
			runtime.Shutdown()
			delete(h.runtimes, closeID)
		}
	}
	target := strings.TrimSpace(msg.SessionID)
	if target == "" {
		return nil
	}
	if existing := h.runtimes[target]; existing != nil {
		h.activeID = target
		return nil
	}
	if h.factory == nil {
		h.err = fmt.Errorf("conversation runtime %s is not available", target)
		return nil
	}
	model, err := h.factory(target, msg.AutoSend)
	if err != nil {
		h.err = err
		return nil
	}
	h.install(target, model)
	h.activeID = target
	if h.width > 0 || h.height > 0 {
		_, _ = model.Update(tea.WindowSizeMsg{Width: h.width, Height: h.height})
	}
	return h.routeCmd(target, model.streamGeneration, model.Init())
}

func (h *ConversationHost) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case RoutedConversationMsg:
		if msg.Msg == nil {
			return h, nil
		}
		if batch, ok := msg.Msg.(tea.BatchMsg); ok {
			cmds := make([]tea.Cmd, 0, len(batch))
			for _, cmd := range batch {
				cmds = append(cmds, h.routeCmd(msg.ConversationID, msg.Generation, cmd))
			}
			return h, tea.Batch(cmds...)
		}
		if sequence, ok := sequenceCommands(msg.Msg); ok {
			cmds := make([]tea.Cmd, 0, len(sequence))
			for _, cmd := range sequence {
				cmds = append(cmds, h.routeCmd(msg.ConversationID, msg.Generation, cmd))
			}
			return h, tea.Sequence(cmds...)
		}
		if nav, ok := msg.Msg.(ConversationNavigationMsg); ok {
			return h, h.navigate(nav)
		}
		if _, ok := msg.Msg.(tea.QuitMsg); ok {
			return h, tea.Quit
		}
		if isBubbleTeaControlMessage(msg.Msg) {
			if msg.ConversationID == h.activeID {
				return h, func() tea.Msg { return msg.Msg }
			}
			return h, nil
		}
		return h, h.updateRuntime(msg.ConversationID, msg.Msg)
	case tea.WindowSizeMsg:
		h.width, h.height = msg.Width, msg.Height
		cmds := make([]tea.Cmd, 0, len(h.runtimes))
		for id := range h.runtimes {
			cmds = append(cmds, h.updateRuntime(id, msg))
		}
		return h, tea.Batch(cmds...)
	case ConversationNavigationMsg:
		return h, h.navigate(msg)
	default:
		return h, h.updateRuntime(h.activeID, msg)
	}
}

func (h *ConversationHost) View() tea.View {
	active := h.runtimes[h.activeID]
	if active == nil {
		return tea.NewView("")
	}
	if h.activeID != h.mainID {
		if parent := h.runtimes[h.mainID]; parent != nil {
			active.SetParentRuntimeStatus(parent.RuntimeStatus())
		}
	}
	if h.err != nil {
		active.SetFooterWarning(h.err.Error())
		h.err = nil
	}
	return active.View()
}

func (h *ConversationHost) ActiveConversationID() string { return h.activeID }

func (h *ConversationHost) Runtime(id string) *Model { return h.runtimes[id] }

func (h *ConversationHost) ActiveRuntime() *Model { return h.runtimes[h.activeID] }

// Shutdown deterministically resolves pending UI callers and cancels every
// conversation without allowing one runtime to tear down another.
func (h *ConversationHost) Shutdown() {
	for id, runtime := range h.runtimes {
		runtime.Shutdown()
		delete(h.runtimes, id)
	}
}
