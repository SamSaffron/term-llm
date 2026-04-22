package tools

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"golang.org/x/term"
)

// ApprovalUIHooks allows the TUI to coordinate with approval prompts.
// Set these callbacks before running the ask command to pause/resume the UI.
var (
	approvalMu      sync.Mutex
	OnApprovalStart func() // Called before showing prompt (pause TUI)
	OnApprovalEnd   func() // Called after prompt answered (resume TUI)
)

// AskUserUIHooks allows the TUI to coordinate with ask_user tool prompts.
var (
	askUserMu         sync.Mutex
	OnAskUserStart    func()     // Called before showing ask_user UI (pause TUI)
	OnAskUserEnd      func()     // Called after ask_user UI answered (resume TUI)
	lastAskUserResult string     // Stores the last ask_user summary for TUI to display
	askUserResultMu   sync.Mutex // Protects lastAskUserResult

	// AskUserUIFunc allows custom UI rendering for ask_user prompts.
	// When set, this function is called instead of RunAskUser.
	// This is used for inline rendering in alt screen mode.
	AskUserUIFunc func(questions []AskUserQuestion) ([]AskUserAnswer, error)
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

// SetAskUserHooks sets callbacks for TUI coordination during ask_user prompts.
func SetAskUserHooks(onStart, onEnd func()) {
	askUserMu.Lock()
	defer askUserMu.Unlock()
	OnAskUserStart = onStart
	OnAskUserEnd = onEnd
}

// SetAskUserUIFunc sets the function to call for ask_user prompts.
// When set, this replaces the default RunAskUser with custom rendering.
func SetAskUserUIFunc(fn func(questions []AskUserQuestion) ([]AskUserAnswer, error)) {
	askUserMu.Lock()
	defer askUserMu.Unlock()
	AskUserUIFunc = fn
}

// ClearAskUserUIFunc removes the custom ask_user UI function.
func ClearAskUserUIFunc() {
	askUserMu.Lock()
	defer askUserMu.Unlock()
	AskUserUIFunc = nil
}

// ClearAskUserHooks removes the ask_user hooks.
func ClearAskUserHooks() {
	askUserMu.Lock()
	defer askUserMu.Unlock()
	OnAskUserStart = nil
	OnAskUserEnd = nil
}

// SetLastAskUserResult stores the summary from the last ask_user execution.
// This should be called by the ask_user UI before signaling completion.
func SetLastAskUserResult(summary string) {
	askUserResultMu.Lock()
	defer askUserResultMu.Unlock()
	lastAskUserResult = summary
}

// GetAndClearAskUserResult retrieves and clears the last ask_user summary.
// Returns empty string if no result was stored.
func GetAndClearAskUserResult() string {
	askUserResultMu.Lock()
	defer askUserResultMu.Unlock()
	result := lastAskUserResult
	lastAskUserResult = ""
	return result
}

// CreateTUIHooks creates start/end hook functions for TUI coordination.
// flushAndWait is called to flush content before releasing terminal.
// prog is the tea.Program to release/restore.
func CreateTUIHooks(prog *tea.Program, flushAndWait func()) (start, end func()) {
	start = func() {
		if flushAndWait != nil {
			flushAndWait()
		}
		prog.ReleaseTerminal()
	}
	end = func() {
		prog.RestoreTerminal()
	}
	return
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
	).WithShowHelp(false).WithShowErrors(false)

	err := form.Run()

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
