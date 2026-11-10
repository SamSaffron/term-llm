package chat

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"github.com/samsaffron/term-llm/internal/tools"
)

// ApprovalRequestMsg triggers an inline approval prompt.
type ApprovalRequestMsg struct {
	Context       context.Context
	NoAutoApprove bool
	Path          string
	IsWrite       bool
	IsShell       bool
	IsWorkspace   bool
	WorkDir       string // directory where a shell command will execute (may be empty)
	DoneCh        chan<- tools.ApprovalResult
}

// DismissApprovalMsg removes only the cell-owned prompt with this result token.
type DismissApprovalMsg struct{ DoneCh chan<- tools.ApprovalResult }

func (m *Model) clearApprovalPrompt() {
	m.approvalModel = nil
	m.approvalDoneCh = nil
	m.approvalIsWorkspace = false
	m.approvalNoAutoApprove = false
}
func (m *Model) dismissApproval(msg DismissApprovalMsg) (tea.Model, tea.Cmd) {
	if m.approvalDoneCh != msg.DoneCh {
		return m, nil
	}
	m.clearApprovalPrompt()
	m.pausedForExternalUI = false
	m.bumpContentVersion()
	return m, m.terminalTitleCmd()
}

// appendPendingMainRunUI is called with run.mu held. Canceled cell prompts must
// not accumulate in an unattached session, including late rejected deliveries.
func appendPendingMainRunUI(run *mainRunState, message tea.Msg) {
	if dismiss, ok := message.(DismissApprovalMsg); ok {
		for i := 0; i < len(run.pendingUI); {
			if prompt, ok := run.pendingUI[i].(ApprovalRequestMsg); ok && prompt.DoneCh == dismiss.DoneCh {
				copy(run.pendingUI[i:], run.pendingUI[i+1:])
				run.pendingUI[len(run.pendingUI)-1] = nil
				run.pendingUI = run.pendingUI[:len(run.pendingUI)-1]
			} else {
				i++
			}
		}
		return
	}
	if prompt, ok := message.(ApprovalRequestMsg); ok && prompt.Context != nil && prompt.Context.Err() != nil {
		select {
		case prompt.DoneCh <- tools.ApprovalResult{Choice: tools.ApprovalChoiceCancelled, Cancelled: true}:
		default:
		}
		return
	}
	run.pendingUI = append(run.pendingUI, message)
}

// Approval channels are buffered by their transport. Ignore an already-settled
// token instead of blocking the UI loop after cell cancellation or late replay.
func replyApproval(done chan<- tools.ApprovalResult, result tools.ApprovalResult) {
	select {
	case done <- result:
	default:
	}
}
