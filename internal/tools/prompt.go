package tools

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// ApprovalUIHooks allows the TUI to coordinate with approval prompts.
// Set these callbacks before running the ask command to pause/resume the UI.
var (
	approvalMu       sync.Mutex
	OnApprovalStart  func() // Called before showing prompt (pause TUI)
	OnApprovalEnd    func() // Called after prompt answered (resume TUI)
)

// SetApprovalHooks sets callbacks for TUI coordination during approval prompts.
func SetApprovalHooks(onStart, onEnd func()) {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	OnApprovalStart = onStart
	OnApprovalEnd = onEnd
}

// ClearApprovalHooks removes the approval hooks.
func ClearApprovalHooks() {
	approvalMu.Lock()
	defer approvalMu.Unlock()
	OnApprovalStart = nil
	OnApprovalEnd = nil
}

// TTYApprovalPrompt prompts the user for directory access approval via /dev/tty.
// This allows prompting even when stdin is piped.
func TTYApprovalPrompt(req *ApprovalRequest) (ConfirmOutcome, string) {
	// Open /dev/tty directly for both reading and writing
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// No TTY available - deny access
		return Cancel, ""
	}
	defer tty.Close()

	// Notify TUI to pause
	approvalMu.Lock()
	startHook := OnApprovalStart
	endHook := OnApprovalEnd
	approvalMu.Unlock()

	if startHook != nil {
		startHook()
	}

	// Restore terminal to cooked mode to ensure proper echo
	fd := int(tty.Fd())
	oldState, err := term.MakeRaw(fd)
	if err == nil {
		// Restore immediately - we just want to reset any weird state
		term.Restore(fd, oldState)
	}

	// Display the prompt and loop until valid response
	reader := bufio.NewReader(tty)
	// Reset terminal state to ensure prompt is visible after TUI releases terminal:
	// \033[?25h = show cursor, \033[0m = reset attributes, \033[r = reset scroll region
	fmt.Fprintf(tty, "\033[?25h\033[0m\033[r\n\033[K%s [y/n]: ", req.Description)

	var outcome ConfirmOutcome
	var resultPath string

	for {
		response, err := reader.ReadString('\n')
		if err != nil {
			outcome = Cancel
			break
		}

		response = strings.TrimSpace(strings.ToLower(response))

		switch response {
		case "y", "yes":
			outcome = ProceedAlways
			resultPath = req.Path
		case "n", "no":
			outcome = Cancel
		default:
			// Invalid input - prompt again (swallow and re-prompt)
			fmt.Fprintf(tty, "\033[K%s [y/n]: ", req.Description)
			continue
		}
		break
	}

	// Notify TUI to resume
	if endHook != nil {
		endHook()
	}

	return outcome, resultPath
}

// HuhApprovalPrompt prompts the user for approval using a huh form.
// This provides a nicer UI than the TTY-based prompt.
func HuhApprovalPrompt(req *ApprovalRequest) (ConfirmOutcome, string) {
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Key("confirm").
				Title(req.Description).
				Affirmative("Yes").
				Negative("No").
				WithButtonAlignment(lipgloss.Left),
		),
	).WithShowHelp(false).WithShowErrors(false)

	if err := form.Run(); err != nil {
		return Cancel, ""
	}

	if form.GetBool("confirm") {
		return ProceedAlways, req.Path
	}
	return Cancel, ""
}
