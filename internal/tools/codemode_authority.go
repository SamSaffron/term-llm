package tools

import (
	"context"
	"maps"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

// CodeModePrompts supplies cell-owned cancellation and presentation, never authority.
type CodeModePrompts struct {
	Serial      bool // single-foreground transports share the root prompt lock
	Prompt      func(string, bool, bool, string) (ApprovalResult, error)
	Workspace   func(string) (WorkspaceApprovalResult, error)
	SharedShell func(string) (ApprovalResult, error)
}
type codeModeAuthority struct{ tools map[string]core.Tool }

func (a *codeModeAuthority) Tool(name string) (core.Tool, bool) { t, ok := a.tools[name]; return t, ok }
func (a *codeModeAuthority) Close()                             {}

// Snapshot helpers require the non-nil config and permissions of a constructed registry.
func (c *ToolConfig) snapshot() *ToolConfig {
	mu := c.mutex()
	mu.RLock()
	defer mu.RUnlock()
	next := *c
	next.mu = &sync.RWMutex{}
	next.Enabled = append([]string(nil), c.Enabled...)
	next.ReadDirs = append([]string(nil), c.ReadDirs...)
	next.WriteDirs = append([]string(nil), c.WriteDirs...)
	next.ShellAllow = append([]string(nil), c.ShellAllow...)
	next.ScriptCommands = append([]string(nil), c.ScriptCommands...)
	return &next
}
func (p *ToolPermissions) clone() *ToolPermissions {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return &ToolPermissions{ReadDirs: append([]string(nil), p.ReadDirs...), WriteDirs: append([]string(nil), p.WriteDirs...), ShellAllow: append([]string(nil), p.ShellAllow...), ScriptCommands: append([]string(nil), p.ScriptCommands...)}
}
func (r *LocalToolRegistry) captureCodeModeAuthority(_ context.Context, ref core.CodeModeCellRef, names []string, binder func(core.CodeModeCellRef) CodeModePrompts) (core.CodeModeAuthority, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names = r.ownedCodeModeNames(ref, names)
	permissions := r.permissions.clone()
	approval := r.approval.CloneForAgentRun(ref.SessionID)
	approval.permissions = permissions
	approval.promptShared = r.approval.PromptLock()
	approval.liveRoot = r.approval.root()
	// A cell's grants are local. A later request may rebind the root store/session.
	approval.workspaceStore = nil
	root := r.approval.root()
	root.toolAllowMu.RLock()
	approval.extensionDirectory = root.extensionDirectory
	approval.readFiles = maps.Clone(root.readFiles)
	approval.toolReadDirs = map[string][]string{}
	for k, v := range root.toolReadDirs {
		approval.toolReadDirs[k] = append([]string(nil), v...)
	}
	root.toolAllowMu.RUnlock()
	if binder != nil {
		prompts := binder(ref)
		if !prompts.Serial {
			approval.promptShared = &sync.Mutex{}
		}
		approval.PromptUIFunc = prompts.Prompt
		approval.WorkspacePromptFunc = prompts.Workspace
		approval.SharedShellPromptUIFunc = prompts.SharedShell
	}
	shadow := &LocalToolRegistry{config: r.config.snapshot(), permissions: permissions, approval: approval, limits: r.limits, appConfig: r.appConfig, grepReranker: r.grepReranker, memoryStore: r.memoryStore, agent: r.agent, sessionID: ref.SessionID, fileRecorder: r.fileRecorder, mediaPublisher: r.mediaPublisher, collaborativeShellController: r.collaborativeShellController, shellRoutingMode: r.shellRoutingMode, tools: map[string]core.Tool{}}
	// Configured services (shared child registry, jobs client, plan controller,
	// skill catalogue, script source) are frozen before any name is considered.
	// A missing service denies only its affected tool; scoped effects never fall
	// back to a mutable live instance.
	bindings := r.captureCodeModeBindingsLocked(shadow, names)
	bindings.bindCodeModeServices(shadow, names)
	for _, name := range names {
		if _, bound := shadow.tools[name]; bound {
			continue
		}
		if IsCodeModeTool(name) || codeModeBindingManagedName(name) {
			continue
		}
		d := lookupBuiltin(name)
		if d == nil {
			// Custom agent, skill or other locally registered script tool.
			if original, ok := r.tools[name]; ok {
				shadow.tools[name] = bindCodeModeScriptTool(original, shadow)
			}
			continue
		}
		if d.nonLocal {
			continue
		}
		tool := d.construct(shadow)
		// Preserve runtime adapters that constructors alone cannot reproduce.
		switch original := r.tools[name].(type) {
		case *ViewImageTool:
			tool = NewViewImageToolWithVision(approval, original.visionProvider, original.visionModel, shadow.config)
		case *ImageGenerateTool:
			copy := *original
			copy.approval = approval
			copy.toolConfig = shadow.config
			tool = &copy
		case *ShowImageTool:
			copy := *original
			copy.approval = approval
			copy.config = shadow.config
			tool = &copy
		}
		shadow.tools[name] = tool
	}
	shadow.applyFileRecorderLocked()
	shadow.applyCollaborativeShellLocked()
	return &codeModeAuthority{tools: shadow.tools}, nil
}

func (m *ApprovalManager) requestedMode() ApprovalMode {
	if m == nil {
		return ModePrompt
	}
	for cur := m; cur != nil; cur = cur.parent {
		cur.modeMu.RLock()
		mode := cur.mode
		cur.modeMu.RUnlock()
		if cur.liveRoot != nil {
			return min(mode, cur.liveRoot.ApprovalMode()) // prompt < auto < yolo
		}
		if mode != ModePrompt {
			return mode
		}
	}
	return ModePrompt
}
func (m *ApprovalManager) codeModeGrantLive(grant session.WorkspaceGrant) bool {
	if m.liveRoot == nil {
		return true
	}
	// Only admission-time grants are subject to the original root's revocation.
	root := m.liveRoot
	root.workspaceMu.RLock()
	defer root.workspaceMu.RUnlock()
	baseline, overlay := root.workspaceGrants[grant.Path], root.workspaceYoloGrants[grant.Path]
	return grant.ID != "" && ((baseline.ID == grant.ID && baseline.Access == grant.Access) || (overlay.ID == grant.ID && overlay.Access == grant.Access))
}

func (m *ApprovalManager) primaryWorkspaceConfirmedLocked() bool {
	root := m.root()
	provenance := root.primaryWorkspaceGrant.Provenance
	return root.primaryWorkspace != "" &&
		root.primaryWorkspaceGrant.ID == primaryWorkspaceID &&
		root.primaryWorkspaceGrant.Path == root.primaryWorkspace &&
		root.primaryWorkspaceGrant.Access == session.WorkspaceAccessWrite &&
		(provenance == primaryWorkspaceProvenanceConfirmed || provenance == primaryWorkspaceProvenanceMainInherited)
}
