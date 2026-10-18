package sessions

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/terminalpolicy"
	tuisessions "github.com/samsaffron/term-llm/internal/tui/sessions"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func (a App) Browse(cmd *cobra.Command, args []string) error {
	if !terminalpolicy.Interactive(os.Stdin, os.Stdout) {
		return a.List(cmd, args)
	}
	closeLog, logErr := a.StartInteractiveDiagnostics(cmd.ErrOrStderr())
	if logErr != nil {
		return logErr
	}
	defer closeLog()

	store, err := a.openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	// Get terminal size
	width, height := 80, 24
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		width, height = w, h
	}

	styles := ui.DefaultStyles()
	model := tuisessions.New(store, width, height, styles)

	p := tea.NewProgram(model)
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("failed to run browser: %w", err)
	}

	// Check if user wants to chat with a session
	if m, ok := finalModel.(*tuisessions.Model); ok && m.ChatSessionID() != "" {
		closeLog() // The browser has quit; chat owns a new terminal UI lifecycle.
		return a.ResumeChat(m.ChatSessionID())
	}

	return nil
}
