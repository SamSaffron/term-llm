package chat

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/tools"
)

func TestCodeModeApprovalDoesNotInheritLiveYoloOrToggle(t *testing.T) {
	for _, liveYolo := range []bool{false, true} {
		m := newTestChatModel(true)
		m.yolo = liveYolo
		done := make(chan tools.ApprovalResult, 1)
		m.Update(ApprovalRequestMsg{Context: context.Background(), NoAutoApprove: true, Path: t.TempDir() + "/file", DoneCh: done})
		if m.approvalModel == nil || !m.approvalNoAutoApprove {
			t.Fatal("cell prompt autoapproved or lost captured restriction")
		}
		if !liveYolo {
			m.toggleYoloMode()
		}
		select {
		case result := <-done:
			t.Fatalf("current/toggled yolo widened cell: %+v", result)
		default:
		}
		m.Update(DismissApprovalMsg{DoneCh: done})
		if m.approvalDoneCh != nil || m.approvalModel != nil || m.approvalNoAutoApprove {
			t.Fatal("dismissal retained cell prompt state")
		}
		normal := make(chan tools.ApprovalResult, 1)
		m.Update(ApprovalRequestMsg{Path: "ordinary", DoneCh: normal})
		select {
		case result := <-normal:
			if result.Choice != tools.ApprovalChoiceOnce {
				t.Fatal(result)
			}
		default:
			t.Fatal("stale cell flag suppressed ordinary yolo approval")
		}
	}
}
func TestCodeModeCancelledPromptAndStaleDismissCannotReplaceNewPrompt(t *testing.T) {
	m := newTestChatModel(true)
	newer := make(chan tools.ApprovalResult, 1)
	m.Update(ApprovalRequestMsg{Path: "newer", DoneCh: newer})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stale := make(chan tools.ApprovalResult, 1)
	m.Update(ApprovalRequestMsg{Context: ctx, NoAutoApprove: true, Path: "cancelled", DoneCh: stale})
	m.Update(DismissApprovalMsg{DoneCh: stale})
	if m.approvalDoneCh != newer {
		t.Fatal("cancelled/stale cell prompt replaced current dialog")
	}
	select {
	case result := <-stale:
		if !result.Cancelled {
			t.Fatal(result)
		}
	default:
		t.Fatal("cancelled message was not settled")
	}
}
func TestCodeModePendingTerminalQueueDiscardsCancelledPrompts(t *testing.T) {
	run := &mainRunState{}
	old, newer := make(chan tools.ApprovalResult, 1), make(chan tools.ApprovalResult, 1)
	appendPendingMainRunUI(run, ApprovalRequestMsg{DoneCh: old})
	appendPendingMainRunUI(run, ApprovalRequestMsg{DoneCh: newer})
	appendPendingMainRunUI(run, DismissApprovalMsg{DoneCh: old})
	if len(run.pendingUI) != 1 || run.pendingUI[0].(ApprovalRequestMsg).DoneCh != newer {
		t.Fatal("dismissal was not token matched")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 {
		appendPendingMainRunUI(run, ApprovalRequestMsg{Context: ctx, DoneCh: old})
	}
	if len(run.pendingUI) != 1 {
		t.Fatal("cancelled detached prompts accumulated")
	}
}

func TestCodeModeLateApprovalReplyNeverBlocksUI(t *testing.T) {
	done := make(chan tools.ApprovalResult, 1)
	done <- tools.ApprovalResult{Cancelled: true}
	replyApproval(done, tools.ApprovalResult{Choice: tools.ApprovalChoiceOnce})
	if result := <-done; !result.Cancelled {
		t.Fatal("late answer replaced cancellation")
	}
}
