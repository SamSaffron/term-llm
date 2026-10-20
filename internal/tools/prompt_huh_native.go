//go:build !js

package tools

import (
	"os"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
	"github.com/samsaffron/term-llm/internal/terminalpolicy"
)

// HuhApprovalPrompt prompts the user for approval using a huh form.
// This provides a nicer UI than the TTY-based prompt.
func HuhApprovalPrompt(req *ApprovalRequest) (ConfirmOutcome, string) {
	if !terminalpolicy.OutputInteractive(os.Stderr) {
		runtimeoutput.Printf("%s\n", "Approval denied: interactive terminal unavailable")
		return Cancel, ""
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		runtimeoutput.Printf("%s\n", "Approval denied: controlling terminal unavailable")
		return Cancel, ""
	}
	defer tty.Close()

	// Notify TUI to pause (e.g., stop spinner during approval)
	approvalMu.Lock()
	startHook := OnApprovalStart
	endHook := OnApprovalEnd
	approvalMu.Unlock()

	if startHook != nil {
		startHook()
	}

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Key("confirm").
				Title(req.Description).
				Affirmative("Yes").
				Negative("No").
				WithButtonAlignment(lipgloss.Left),
		),
	).WithShowHelp(false).WithShowErrors(false).WithInput(tty).WithOutput(tty)

	err = form.Run()

	// Notify TUI to resume
	if endHook != nil {
		endHook()
	}

	if err != nil {
		return Cancel, ""
	}

	if form.GetBool("confirm") {
		return ProceedAlways, req.Path
	}
	return Cancel, ""
}
