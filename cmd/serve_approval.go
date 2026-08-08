package cmd

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/samsaffron/term-llm/internal/tools"
)

var (
	errServeApprovalNotPending  = errors.New("no pending approval request")
	errServeApprovalAnswered    = errors.New("approval request already answered")
	errServeApprovalNoTransport = errors.New("no approval transport configured")
)

type serveApprovalPrompt struct {
	ApprovalID          string                `json:"approval_id"`
	Path                string                `json:"path"`
	IsWrite             bool                  `json:"is_write"`
	IsShell             bool                  `json:"is_shell"`
	IsWorkspace         bool                  `json:"is_workspace,omitempty"`
	WorkDir             string                `json:"work_dir,omitempty"`
	Title               string                `json:"title"`
	Options             []serveApprovalOption `json:"options"`
	ResumeAutoAvailable bool                  `json:"resume_auto_available,omitempty"`
	CreatedAt           int64                 `json:"created_at"`
}

type serveApprovalOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Index       int    `json:"index"`
	Choice      string `json:"choice"`
}

type serveApprovalSubmission struct {
	Result tools.ApprovalResult
	Err    error
}

type servePendingApproval struct {
	ApprovalID          string
	Path                string
	IsWrite             bool
	IsShell             bool
	IsWorkspace         bool
	WorkDir             string
	Options             []tools.ApprovalOption
	ResumeAutoAvailable bool
	CreatedAt           time.Time
	responseC           chan serveApprovalSubmission
	responded           bool
}

func (p *servePendingApproval) snapshot() serveApprovalPrompt {
	options := make([]serveApprovalOption, len(p.Options))
	for i, opt := range p.Options {
		options[i] = serveApprovalOption{
			Label:       opt.Label,
			Description: opt.Description,
			Index:       i,
			Choice:      approvalChoiceName(opt.Choice),
		}
	}

	title := "Access Request"
	switch {
	case p.IsWorkspace:
		title = "Primary Workspace Access Request"
	case p.IsShell:
		title = "Shell Command Request"
	case p.IsWrite:
		title = "Write Access Request"
	default:
		title = "Read Access Request"
	}

	return serveApprovalPrompt{
		ApprovalID:          p.ApprovalID,
		Path:                p.Path,
		IsWrite:             p.IsWrite,
		IsShell:             p.IsShell,
		IsWorkspace:         p.IsWorkspace,
		WorkDir:             p.WorkDir,
		Title:               title,
		Options:             options,
		ResumeAutoAvailable: p.ResumeAutoAvailable,
		CreatedAt:           p.CreatedAt.UnixMilli(),
	}
}

func (rt *serveRuntime) awaitApproval(target string, isWrite bool, isShell bool, workDir string) (tools.ApprovalResult, error) {
	return rt.awaitApprovalRequest(target, isWrite, isShell, false, workDir)
}

func (rt *serveRuntime) awaitWorkspaceApproval(workspace string) (tools.WorkspaceApprovalResult, error) {
	result, err := rt.awaitApprovalRequest(workspace, true, false, true, "")
	if err != nil {
		return tools.WorkspaceApprovalResult{}, err
	}
	return tools.WorkspaceApprovalResult{
		Approved:  !result.Cancelled && result.Choice == tools.ApprovalChoiceWorkspace,
		Cancelled: result.Cancelled,
	}, nil
}

func (rt *serveRuntime) awaitApprovalRequest(target string, isWrite bool, isShell bool, isWorkspace bool, workDir string) (tools.ApprovalResult, error) {
	approvalID := "appr_" + randomSuffix()

	var options []tools.ApprovalOption
	if isWorkspace {
		options = tools.BuildWorkspaceOptions(target)
	} else if isShell {
		dir := workDir
		if dir == "" {
			dir, _ = os.Getwd()
		}
		repoInfo := tools.DetectGitRepo(dir)
		var repoInfoPtr *tools.GitRepoInfo
		if repoInfo.IsRepo {
			repoInfoPtr = &repoInfo
		}
		options = tools.BuildShellOptions(target, repoInfoPtr)
	} else {
		repoInfo := tools.DetectGitRepo(target)
		var repoInfoPtr *tools.GitRepoInfo
		if repoInfo.IsRepo {
			repoInfoPtr = &repoInfo
		}
		options = tools.BuildFileOptions(target, repoInfoPtr, isWrite)
	}

	rt.approvalMu.Lock()

	eventFunc := rt.approvalEventFunc
	ctx := rt.approvalCtx

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
	resumeAutoAvailable := rt.toolMgr != nil && rt.toolMgr.ApprovalMgr != nil && rt.toolMgr.ApprovalMgr.GuardianAutoSuspended()
	pending := &servePendingApproval{
		ApprovalID:          approvalID,
		Path:                target,
		IsWrite:             isWrite,
		IsShell:             isShell,
		IsWorkspace:         isWorkspace,
		WorkDir:             workDir,
		Options:             options,
		ResumeAutoAvailable: resumeAutoAvailable,
		CreatedAt:           time.Now(),
		responseC:           make(chan serveApprovalSubmission, 1),
	}
	rt.pendingApprovals[approvalID] = pending
	rt.approvalMu.Unlock()

	defer rt.removePendingApproval(approvalID, pending)

	// Emit SSE event — if this fails the client never learns about the
	// pending approval, so return immediately instead of blocking forever.
	snap := pending.snapshot()
	payload := map[string]any{
		"approval_id":           snap.ApprovalID,
		"path":                  snap.Path,
		"is_write":              snap.IsWrite,
		"is_shell":              snap.IsShell,
		"is_workspace":          snap.IsWorkspace,
		"title":                 snap.Title,
		"options":               snap.Options,
		"resume_auto_available": snap.ResumeAutoAvailable,
		"created_at":            snap.CreatedAt,
	}
	if snap.WorkDir != "" {
		payload["work_dir"] = snap.WorkDir
	}
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

func approvalChoiceName(c tools.ApprovalChoice) string {
	switch c {
	case tools.ApprovalChoiceDeny:
		return "deny"
	case tools.ApprovalChoiceOnce:
		return "once"
	case tools.ApprovalChoiceFile:
		return "file"
	case tools.ApprovalChoiceDirectory:
		return "directory"
	case tools.ApprovalChoiceRepoRead:
		return "repo_read"
	case tools.ApprovalChoiceRepoWrite:
		return "repo_write"
	case tools.ApprovalChoicePattern:
		return "pattern"
	case tools.ApprovalChoiceCommand:
		return "command"
	case tools.ApprovalChoiceWorkspace:
		return "workspace"
	case tools.ApprovalChoiceCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}
