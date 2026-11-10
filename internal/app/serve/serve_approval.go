package serve

import (
	"errors"
	"fmt"
	"time"

	"github.com/samsaffron/term-llm/internal/app/serve/approvalprompt"
	"github.com/samsaffron/term-llm/internal/tools"
)

var (
	errServeApprovalNotPending  = errors.New("no pending approval request")
	errServeApprovalAnswered    = errors.New("approval request already answered")
	errServeApprovalNoTransport = errors.New("no approval transport configured")
)

type serveApprovalPrompt = approvalprompt.Prompt
type serveApprovalOption = approvalprompt.Option

type serveApprovalSubmission struct {
	Result tools.ApprovalResult
	Err    error
}

type servePendingApproval struct {
	CellID              string
	ApprovalID          string
	Path                string
	IsWrite             bool
	IsShell             bool
	IsWorkspace         bool
	WorkDir             string
	Scope               string
	Options             []tools.ApprovalOption
	ResumeAutoAvailable bool
	CreatedAt           time.Time
	responseC           chan serveApprovalSubmission
	responded           bool
}

func (p *servePendingApproval) snapshot() serveApprovalPrompt {
	return (approvalprompt.Details{CellID: p.CellID, ApprovalID: p.ApprovalID, Path: p.Path, IsWrite: p.IsWrite, IsShell: p.IsShell, IsWorkspace: p.IsWorkspace, WorkDir: p.WorkDir, Scope: p.Scope, Options: p.Options, ResumeAutoAvailable: p.ResumeAutoAvailable, CreatedAt: p.CreatedAt}).Snapshot()
}

func (rt *serveRuntime) awaitApproval(target string, isWrite bool, isShell bool, workDir string) (tools.ApprovalResult, error) {
	return rt.awaitApprovalRequest(target, isWrite, isShell, false, workDir)
}

func (rt *serveRuntime) awaitSharedShellApproval(command string) (tools.ApprovalResult, error) {
	return rt.awaitApprovalRequestScoped(command, false, true, false, "", "shared_shell")
}

func (rt *serveRuntime) awaitWorkspaceApproval(workspace string) (tools.WorkspaceApprovalResult, error) {
	result, err := rt.awaitApprovalRequest(workspace, true, false, true, "")
	return approvalprompt.WorkspaceResult(result, err)
}

func newServePendingApproval(target string, isWrite bool, isShell bool, isWorkspace bool, workDir string) *servePendingApproval {
	return newServePendingApprovalScoped(target, isWrite, isShell, isWorkspace, workDir, "")
}

func newServePendingApprovalScoped(target string, isWrite bool, isShell bool, isWorkspace bool, workDir, scope string) *servePendingApproval {
	options := approvalprompt.Options(target, isWrite, isShell, isWorkspace, workDir, scope)

	return &servePendingApproval{
		ApprovalID:  "appr_" + randomSuffix(),
		Path:        target,
		IsWrite:     isWrite,
		IsShell:     isShell,
		IsWorkspace: isWorkspace,
		WorkDir:     workDir,
		Scope:       scope,
		Options:     options,
		CreatedAt:   time.Now(),
		responseC:   make(chan serveApprovalSubmission, 1),
	}
}

// prepareApprovalRequest registers an approval without requiring a live stream
// transport. Recovery paths can discover the prompt through session state.
func (rt *serveRuntime) prepareApprovalRequest(target string, isWrite bool, isShell bool, isWorkspace bool, workDir string) (*servePendingApproval, serveApprovalPrompt) {
	pending := newServePendingApproval(target, isWrite, isShell, isWorkspace, workDir)
	rt.approvalMu.Lock()
	defer rt.approvalMu.Unlock()
	if rt.pendingApprovals == nil {
		rt.pendingApprovals = make(map[string]*servePendingApproval)
	}
	if rt.toolMgr != nil && rt.toolMgr.ApprovalMgr != nil {
		pending.ResumeAutoAvailable = rt.toolMgr.ApprovalMgr.GuardianAutoSuspended()
	}
	rt.pendingApprovals[pending.ApprovalID] = pending
	return pending, pending.snapshot()
}

func (rt *serveRuntime) awaitApprovalRequest(target string, isWrite bool, isShell bool, isWorkspace bool, workDir string) (tools.ApprovalResult, error) {
	return rt.awaitApprovalRequestScoped(target, isWrite, isShell, isWorkspace, workDir, "")
}

func (rt *serveRuntime) awaitApprovalRequestScoped(target string, isWrite bool, isShell bool, isWorkspace bool, workDir, scope string) (tools.ApprovalResult, error) {
	pending := newServePendingApprovalScoped(target, isWrite, isShell, isWorkspace, workDir, scope)

	rt.approvalMu.Lock()

	eventFunc := rt.approvalEventFunc
	ctx := rt.approvalCtx
	pauseResponseTimeout := rt.pauseResponseTimeout

	// Fail fast if no approval transport is configured (e.g. synchronous
	// /v1/responses or /v1/chat/completions paths that don't go through
	// the response-run streaming flow). Without an event func the client
	// has no way to learn about the pending approval, so blocking would
	// hang the request indefinitely.
	if eventFunc == nil || ctx == nil {
		rt.approvalMu.Unlock()
		return tools.ApprovalResult{}, errServeApprovalNoTransport
	}

	if rt.pendingApprovals == nil {
		rt.pendingApprovals = make(map[string]*servePendingApproval)
	}
	if rt.toolMgr != nil && rt.toolMgr.ApprovalMgr != nil {
		pending.ResumeAutoAvailable = rt.toolMgr.ApprovalMgr.GuardianAutoSuspended()
	}
	rt.pendingApprovals[pending.ApprovalID] = pending
	rt.approvalMu.Unlock()

	resumeResponseTimeout := func() {}
	if pauseResponseTimeout != nil {
		resumeResponseTimeout = pauseResponseTimeout()
	}
	defer resumeResponseTimeout()
	defer rt.removePendingApproval(pending.ApprovalID, pending)

	// Emit SSE event — if this fails the client never learns about the
	// pending approval, so return immediately instead of blocking forever.
	snap := pending.snapshot()
	payload := snap.Payload()
	if err := eventFunc("response.approval.prompt", payload); err != nil {
		return tools.ApprovalResult{}, fmt.Errorf("failed to emit approval event: %w", err)
	}

	// Block waiting for response or cancellation
	select {
	case submission := <-pending.responseC:
		return submission.Result, submission.Err
	case <-ctx.Done():
		return tools.ApprovalResult{Cancelled: true, Choice: tools.ApprovalChoiceCancelled}, ctx.Err()
	}
}

func (rt *serveRuntime) approvalOutcome(approvalID string, choiceIndex int, cancelled bool) string {
	if cancelled {
		return "cancelled-by-user"
	}
	rt.approvalMu.Lock()
	defer rt.approvalMu.Unlock()
	pending := rt.pendingApprovals[approvalID]
	if pending != nil && choiceIndex >= 0 && choiceIndex < len(pending.Options) && pending.Options[choiceIndex].Choice == tools.ApprovalChoiceDeny {
		return "denied"
	}
	return "accepted"
}

func (rt *serveRuntime) submitApproval(approvalID string, choiceIndex int, cancelled bool, resumeAuto bool) error {
	rt.approvalMu.Lock()
	pending := rt.pendingApprovals[approvalID]
	if pending == nil {
		rt.approvalMu.Unlock()
		return errServeApprovalNotPending
	}
	if pending.responded {
		rt.approvalMu.Unlock()
		return errServeApprovalAnswered
	}
	if resumeAuto && !pending.ResumeAutoAvailable {
		rt.approvalMu.Unlock()
		return errors.New("auto resume is not available for this approval")
	}

	submission := serveApprovalSubmission{}
	if cancelled {
		submission.Result = tools.ApprovalResult{
			Choice:    tools.ApprovalChoiceCancelled,
			Cancelled: true,
		}
	} else {
		if choiceIndex < 0 || choiceIndex >= len(pending.Options) {
			rt.approvalMu.Unlock()
			return errors.New("choice index out of range")
		}
		opt := pending.Options[choiceIndex]
		submission.Result = tools.ApprovalResult{
			Choice:     opt.Choice,
			Path:       opt.Path,
			Pattern:    opt.Pattern,
			SaveToRepo: opt.SaveToRepo,
		}
	}

	pending.responded = true
	rt.approvalMu.Unlock()

	// Approval and breaker recovery are independent decisions. Only this explicit
	// protocol bit starts a fresh auto epoch; choosing any approval option alone
	// leaves the breaker suspended.
	if resumeAuto && rt.toolMgr != nil && rt.toolMgr.ApprovalMgr != nil {
		rt.toolMgr.ApprovalMgr.ResumeAuto()
	}

	select {
	case pending.responseC <- submission:
		return nil
	default:
		return errServeApprovalAnswered
	}
}

func (rt *serveRuntime) removePendingApproval(approvalID string, pending *servePendingApproval) {
	rt.approvalMu.Lock()
	defer rt.approvalMu.Unlock()
	if current := rt.pendingApprovals[approvalID]; current == pending {
		delete(rt.pendingApprovals, approvalID)
	}
}

func (rt *serveRuntime) clearPendingApprovals() {
	rt.approvalMu.Lock()
	defer rt.approvalMu.Unlock()
	for _, pending := range rt.pendingApprovals {
		if pending != nil && !pending.responded {
			pending.responded = true
			select {
			case pending.responseC <- serveApprovalSubmission{
				Result: tools.ApprovalResult{
					Choice:    tools.ApprovalChoiceCancelled,
					Cancelled: true,
				},
			}:
			default:
			}
		}
	}
	rt.pendingApprovals = nil
}

func (rt *serveRuntime) pendingApprovalPrompts() []serveApprovalPrompt {
	rt.approvalMu.Lock()
	defer rt.approvalMu.Unlock()
	return sortedPendingSnapshots(rt.pendingApprovals,
		func(pending *servePendingApproval) serveApprovalPrompt { return pending.snapshot() },
		func(prompt serveApprovalPrompt) int64 { return prompt.CreatedAt },
	)
}
