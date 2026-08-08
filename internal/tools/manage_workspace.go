package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type manageWorkspaceArgs struct {
	Action  string `json:"action"`
	Path    string `json:"path,omitempty"`
	Access  string `json:"access,omitempty"`
	Reason  string `json:"reason,omitempty"`
	GrantID string `json:"grant_id,omitempty"`
}

// ManageWorkspaceTool grants, lists, and revokes session-scoped local file
// capabilities. Registration is automatic only when a path-capable tool exists.
type ManageWorkspaceTool struct {
	approval *ApprovalManager
	config   *ToolConfig
}

func NewManageWorkspaceTool(approval *ApprovalManager, configs ...*ToolConfig) *ManageWorkspaceTool {
	return &ManageWorkspaceTool{approval: approval, config: optionalToolConfig(configs)}
}

func (t *ManageWorkspaceTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        ManageWorkspaceToolName,
		Description: "Manage session-scoped local workspaces. Use grant with read access for reference/comparison/source repositories (the default), and request write only when the user's task requires mutation. Additional grants receive one Guardian review in auto mode; write elevation is reviewed separately. Use list to inspect current capabilities and revoke to remove an additional grant. Never treat a workspace as shell-command permission, and do not repeat a denied request unless the user provides new explicit authorization.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"action":   map[string]interface{}{"type": "string", "enum": []string{"grant", "revoke", "list"}, "description": "Workspace operation."},
				"path":     map[string]interface{}{"type": "string", "description": "Directory to grant, or exact canonical path to revoke."},
				"access":   map[string]interface{}{"type": "string", "enum": []string{"read", "write"}, "default": "read", "description": "Grant access level. Write implies read."},
				"reason":   map[string]interface{}{"type": "string", "description": "Concise reason grounded in the user's request (required for grant)."},
				"grant_id": map[string]interface{}{"type": "string", "description": "Stable additional grant ID to revoke."},
			},
			"required":             []string{"action"},
			"additionalProperties": false,
		},
	}
}

func (t *ManageWorkspaceTool) Preview(args json.RawMessage) string {
	var input manageWorkspaceArgs
	if json.Unmarshal(args, &input) != nil {
		return ""
	}
	action := strings.ToLower(strings.TrimSpace(input.Action))
	if action == "list" {
		return "list workspaces"
	}
	selector := strings.TrimSpace(input.Path)
	if selector == "" {
		selector = strings.TrimSpace(input.GrantID)
	}
	if selector == "" {
		return action
	}
	return action + " " + selector
}

func (t *ManageWorkspaceTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	if t == nil || t.approval == nil {
		return workspaceToolError(NewToolError(ErrPermissionDenied, "workspace approval manager is unavailable")), nil
	}
	var input manageWorkspaceArgs
	if err := json.Unmarshal(args, &input); err != nil {
		return workspaceToolError(NewToolErrorf(ErrInvalidParams, "invalid manage_workspace arguments: %v", err)), nil
	}
	switch strings.ToLower(strings.TrimSpace(input.Action)) {
	case "list":
		return llm.ToolOutput{Content: formatWorkspaceCapabilities(t.approval.WorkspaceCapabilities())}, nil
	case "grant":
		access := session.WorkspaceAccess(strings.ToLower(strings.TrimSpace(input.Access)))
		if access == "" {
			access = session.WorkspaceAccessRead
		}
		baseDir := ""
		if t.config != nil {
			baseDir = t.config.WorkingDir()
			if t.config.RequiresExplicitWorkingDir() && baseDir == "" && !filepath.IsAbs(strings.TrimSpace(input.Path)) {
				return workspaceToolError(NewToolError(ErrInvalidParams, "relative workspace path requires an absolute path or explicit session working directory")), nil
			}
		}
		root, err := CanonicalWorkspaceRoot(input.Path, baseDir)
		if err != nil {
			return workspaceToolError(err), nil
		}
		result, err := t.approval.GrantWorkspace(ctx, root, access, input.Reason)
		if err != nil {
			return workspaceToolError(err), nil
		}
		status := "already granted"
		if result.Changed {
			status = "granted"
		}
		durability := "session runtime only"
		if result.Persisted {
			durability = "persisted with this session"
		}
		return llm.ToolOutput{Content: fmt.Sprintf("Workspace %s: id=%s path=%s access=%s provenance=%s (%s)", status, result.Capability.ID, result.Capability.Path, result.Capability.Access, result.Capability.Provenance, durability)}, nil
	case "revoke":
		selector := strings.TrimSpace(input.GrantID)
		if selector == "" {
			selector = strings.TrimSpace(input.Path)
		}
		capability, changed, err := t.approval.RevokeWorkspace(ctx, selector)
		if err != nil {
			return workspaceToolError(err), nil
		}
		if !changed {
			return llm.ToolOutput{Content: "No matching additional workspace grant."}, nil
		}
		return llm.ToolOutput{Content: fmt.Sprintf("Revoked workspace grant: id=%s path=%s access=%s", capability.ID, capability.Path, capability.Access)}, nil
	default:
		return workspaceToolError(NewToolError(ErrInvalidParams, "action must be grant, revoke, or list")), nil
	}
}

func workspaceToolError(err error) llm.ToolOutput {
	if err == nil {
		return llm.ToolOutput{}
	}
	return llm.ToolOutput{Content: err.Error(), IsError: true}
}

func formatWorkspaceCapabilities(capabilities []WorkspaceCapability) string {
	if len(capabilities) == 0 {
		return "No session workspace capabilities are bound."
	}
	var b strings.Builder
	b.WriteString("Session workspace capabilities:\n")
	for _, capability := range capabilities {
		kind := "additional"
		if capability.Primary {
			kind = "primary"
		}
		fmt.Fprintf(&b, "- id=%s path=%s access=%s status=%s provenance=%s kind=%s", capability.ID, capability.Path, capability.Access, capability.Status, capability.Provenance, kind)
		if capability.Rationale != "" {
			fmt.Fprintf(&b, " reason=%q", capability.Rationale)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
