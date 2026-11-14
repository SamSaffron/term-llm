// Package approvalprompt builds the shared file, shell and cell approval presentation.
package approvalprompt

import (
	"github.com/term-llm/term-llm/internal/tools"
	"os"
	"time"
)

type Prompt struct {
	CellID              string   `json:"cell_id,omitempty"`
	ApprovalID          string   `json:"approval_id"`
	Path                string   `json:"path"`
	IsWrite             bool     `json:"is_write"`
	IsShell             bool     `json:"is_shell"`
	IsWorkspace         bool     `json:"is_workspace,omitempty"`
	WorkDir             string   `json:"work_dir,omitempty"`
	Scope               string   `json:"scope,omitempty"`
	Title               string   `json:"title"`
	Options             []Option `json:"options"`
	ResumeAutoAvailable bool     `json:"resume_auto_available,omitempty"`
	CreatedAt           int64    `json:"created_at"`
}

type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Index       int    `json:"index"`
	Choice      string `json:"choice"`
}

type Details struct {
	CellID, ApprovalID, Path      string
	IsWrite, IsShell, IsWorkspace bool
	WorkDir, Scope                string
	Options                       []tools.ApprovalOption
	ResumeAutoAvailable           bool
	CreatedAt                     time.Time
}

func (p Details) Snapshot() Prompt {
	options := make([]Option, len(p.Options))
	for i, opt := range p.Options {
		options[i] = Option{
			Label:       opt.Label,
			Description: opt.Description,
			Index:       i,
			Choice:      approvalChoiceName(opt.Choice),
		}
	}

	title := "Access Request"
	switch {
	case p.IsWorkspace:
		title = "Allow workspace access?"
	case p.IsShell:
		title = "Shell Command Request"
	case p.IsWrite:
		title = "Write Access Request"
	default:
		title = "Read Access Request"
	}

	return Prompt{
		CellID:              p.CellID,
		ApprovalID:          p.ApprovalID,
		Path:                p.Path,
		IsWrite:             p.IsWrite,
		IsShell:             p.IsShell,
		IsWorkspace:         p.IsWorkspace,
		WorkDir:             p.WorkDir,
		Scope:               p.Scope,
		Title:               title,
		Options:             options,
		ResumeAutoAvailable: p.ResumeAutoAvailable,
		CreatedAt:           p.CreatedAt.UnixMilli(),
	}
}

func Options(target string, isWrite, isShell, isWorkspace bool, workDir, scope string) []tools.ApprovalOption {
	var options []tools.ApprovalOption
	if isWorkspace {
		options = tools.BuildWorkspaceOptions(target)
	} else if isShell {
		if scope == "shared_shell" {
			options = tools.BuildSharedShellOptions(target)
		} else {
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
		}
	} else {
		repoInfo := tools.DetectGitRepo(target)
		var repoInfoPtr *tools.GitRepoInfo
		if repoInfo.IsRepo {
			repoInfoPtr = &repoInfo
		}
		options = tools.BuildFileOptions(target, repoInfoPtr, isWrite)
	}

	return options
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
	case tools.ApprovalChoiceWorkspaceRemember:
		return "workspace_remember"
	case tools.ApprovalChoiceCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// Payload is shared by turn-owned and idle cell approval transports.
func (p Prompt) Payload() map[string]any {
	payload := map[string]any{
		"approval_id":           p.ApprovalID,
		"path":                  p.Path,
		"is_write":              p.IsWrite,
		"is_shell":              p.IsShell,
		"is_workspace":          p.IsWorkspace,
		"title":                 p.Title,
		"scope":                 p.Scope,
		"options":               p.Options,
		"resume_auto_available": p.ResumeAutoAvailable,
		"created_at":            p.CreatedAt,
	}
	if p.WorkDir != "" {
		payload["work_dir"] = p.WorkDir
	}
	if p.CellID != "" {
		payload["cell_id"] = p.CellID
	}
	return payload
}

// WorkspaceResult preserves the distinct human workspace boundary.
func WorkspaceResult(result tools.ApprovalResult, err error) (tools.WorkspaceApprovalResult, error) {
	if err != nil {
		return tools.WorkspaceApprovalResult{}, err
	}
	choice := result.Choice
	return tools.WorkspaceApprovalResult{
		Approved:  !result.Cancelled && (choice == tools.ApprovalChoiceWorkspace || choice == tools.ApprovalChoiceWorkspaceRemember),
		Remember:  !result.Cancelled && choice == tools.ApprovalChoiceWorkspaceRemember,
		Cancelled: result.Cancelled,
	}, nil
}
