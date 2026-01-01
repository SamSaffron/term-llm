package ui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/prompt"
	"golang.org/x/term"
)

// ShowCommandHelp renders scrollable help for a command
func ShowCommandHelp(command, shell string, provider llm.Provider) error {
	ctx := context.Background()

	// Build the help prompt
	helpPrompt := prompt.HelpPrompt(command, shell)

	req := llm.AskRequest{
		Question:     helpPrompt,
		EnableSearch: false,
		Debug:        false,
	}

	// Get TTY for shell integration
	tty, err := getTTY()
	if err != nil {
		return fmt.Errorf("cannot open TTY: %w", err)
	}
	defer tty.Close()

	width, height := getTerminalSize()

	// Create the model
	m := newHelpModel(width, height)

	// Create program with TTY
	p := tea.NewProgram(m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithInput(tty),
		tea.WithOutput(tty),
	)

	// Stream content in background, sending chunks to the program
	go func() {
		output := make(chan string)
		errChan := make(chan error, 1)

		go func() {
			errChan <- provider.StreamResponse(ctx, req, output)
		}()

		for chunk := range output {
			p.Send(contentMsg(chunk))
		}

		if err := <-errChan; err != nil {
			p.Send(errorMsg{err})
		} else {
			p.Send(doneMsg{})
		}
	}()

	_, err = p.Run()
	return err
}

// Messages
type contentMsg string
type doneMsg struct{}
type errorMsg struct{ err error }

// helpModel is the bubbletea model for streaming help
type helpModel struct {
	viewport   viewport.Model
	spinner    spinner.Model
	content    *strings.Builder // pointer to avoid copy issues
	rendered   string
	width      int
	height     int
	loading    bool
	err        error
}

func newHelpModel(width, height int) helpModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	vp := viewport.New(width, height-1) // -1 for footer

	return helpModel{
		viewport: vp,
		spinner:  sp,
		content:  &strings.Builder{},
		width:    width,
		height:   height,
		loading:  true,
	}
}

func (m helpModel) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m helpModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "Q", "esc", "enter":
			return m, tea.Quit
		case "ctrl+c":
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewport.Width = msg.Width
		m.viewport.Height = msg.Height - 1
		// Re-render content for new width
		if m.content.Len() > 0 {
			m.rendered = renderMarkdown(m.content.String(), m.width)
			m.viewport.SetContent(m.rendered)
		}

	case contentMsg:
		m.content.WriteString(string(msg))
		// Re-render and update viewport
		m.rendered = renderMarkdown(m.content.String(), m.width)
		m.viewport.SetContent(m.rendered)
		// Auto-scroll to bottom while streaming
		m.viewport.GotoBottom()

	case doneMsg:
		m.loading = false

	case errorMsg:
		m.loading = false
		m.err = msg.err

	case spinner.TickMsg:
		if m.loading {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	// Update viewport for scrolling
	var vpCmd tea.Cmd
	m.viewport, vpCmd = m.viewport.Update(msg)
	cmds = append(cmds, vpCmd)

	return m, tea.Batch(cmds...)
}

func (m helpModel) View() string {
	var footer string

	if m.err != nil {
		footer = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Render(fmt.Sprintf("Error: %v • q to exit", m.err))
	} else if m.loading && m.content.Len() == 0 {
		footer = m.spinner.View() + " Generating help..."
	} else if m.loading {
		footer = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			Render("↑/↓ scroll • streaming...")
	} else {
		footer = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			Render("↑/↓ scroll • q/Esc/Enter to exit")
	}

	return m.viewport.View() + "\n" + footer
}

// renderMarkdown renders content with glamour
func renderMarkdown(content string, width int) string {
	style := styles.DraculaStyleConfig
	style.Document.Margin = uintPtr(0)
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	style.CodeBlock.Margin = uintPtr(0)

	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return content
	}

	rendered, err := renderer.Render(content)
	if err != nil {
		return content
	}

	result := strings.TrimSpace(rendered)
	if result != "" && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}

	return wordwrap.String(result, width)
}

// getTerminalSize returns terminal width and height
func getTerminalSize() (int, int) {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		width = 80
	}
	if err != nil || height <= 0 {
		height = 24
	}
	return width, height
}

func uintPtr(v uint) *uint {
	return &v
}
