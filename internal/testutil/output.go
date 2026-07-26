package testutil

import (
	"bytes"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// ByteCapture records exact output bytes and the boundaries between Write calls.
// It is safe for renderers and delayed output callbacks to use concurrently.
type ByteCapture struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	writes [][]byte
}

// NewByteCapture creates an empty byte-level output capture.
func NewByteCapture() *ByteCapture {
	return &ByteCapture{}
}

// Write records p as one output write.
func (c *ByteCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.writes = append(c.writes, append([]byte(nil), p...))
	return c.buffer.Write(p)
}

// Bytes returns a copy of all captured bytes.
func (c *ByteCapture) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buffer.Bytes()...)
}

// Writes returns copies of the bytes passed to each Write call, in order.
func (c *ByteCapture) Writes() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	writes := make([][]byte, len(c.writes))
	for i, write := range c.writes {
		writes[i] = append([]byte(nil), write...)
	}
	return writes
}

// Reset clears all captured output.
func (c *ByteCapture) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buffer.Reset()
	c.writes = nil
}

type terminalOutputModel struct {
	commands []tea.Cmd
}

func (m terminalOutputModel) Init() tea.Cmd {
	commands := append([]tea.Cmd(nil), m.commands...)
	return tea.Sequence(append(commands, tea.Quit)...)
}

func (m terminalOutputModel) Update(tea.Msg) (tea.Model, tea.Cmd) {
	return m, nil
}

func (m terminalOutputModel) View() tea.View {
	return tea.NewView("")
}

// TerminalOutputHarness runs Bubble Tea with deterministic terminal settings and
// captures both the complete byte stream and individual output writes.
type TerminalOutputHarness struct {
	capture *ByteCapture
}

// NewTerminalOutputHarness creates a deterministic byte-level terminal harness.
func NewTerminalOutputHarness() *TerminalOutputHarness {
	return &TerminalOutputHarness{capture: NewByteCapture()}
}

// Run executes model with fixed terminal dimensions and environment.
func (h *TerminalOutputHarness) Run(model tea.Model) (tea.Model, error) {
	h.capture.Reset()
	program := tea.NewProgram(
		model,
		tea.WithInput(nil),
		tea.WithOutput(h.capture),
		tea.WithWindowSize(80, 24),
		tea.WithEnvironment([]string{"TERM=xterm-256color"}),
		tea.WithoutSignalHandler(),
	)
	return program.Run()
}

// RunCommands executes commands in sequence and then exits the program.
func (h *TerminalOutputHarness) RunCommands(commands ...tea.Cmd) error {
	_, err := h.Run(terminalOutputModel{commands: commands})
	return err
}

// Bytes returns a copy of the complete terminal byte stream.
func (h *TerminalOutputHarness) Bytes() []byte {
	return h.capture.Bytes()
}

// Writes returns copies of individual terminal writes in output order.
func (h *TerminalOutputHarness) Writes() [][]byte {
	return h.capture.Writes()
}
