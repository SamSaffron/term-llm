package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
)

// LocalToolRegistry manages local tools and their registration with the engine.
type LocalToolRegistry struct {
	mu sync.RWMutex

	config      *ToolConfig
	permissions *ToolPermissions
	approval    *ApprovalManager
	limits      OutputLimits
	appConfig   *config.Config

	memoryStore    ImageRecorder
	agent          string
	sessionID      string
	fileRecorder   FileChangeRecorder
	mediaPublisher MediaPublisher

	collaborativeShellController CollaborativeShellController
	shellRoutingMode             ShellRoutingMode

	// Registered tools
	tools map[string]llm.Tool
}

// NewLocalToolRegistry creates a new registry from configuration.
// The approvalMgr parameter is used for interactive permission prompts.
func NewLocalToolRegistry(toolConfig *ToolConfig, appConfig *config.Config, approvalMgr *ApprovalManager) (*LocalToolRegistry, error) {
	// Build permissions from config
	perms, err := toolConfig.BuildPermissions()
	if err != nil {
		return nil, err
	}

	// If no approval manager provided, create one (for backwards compatibility).
	// Otherwise keep the registry and approval manager on the same permissions
	// instance so dynamic updates like SetBaseDir are visible to tool checks.
	if approvalMgr == nil {
		approvalMgr = NewApprovalManager(perms)
	} else if approvalMgr.permissions != nil {
		perms = approvalMgr.permissions
	} else {
		approvalMgr.permissions = perms
	}

	r := &LocalToolRegistry{
		config:           toolConfig,
		permissions:      perms,
		approval:         approvalMgr,
		limits:           DefaultOutputLimits(),
		appConfig:        appConfig,
		shellRoutingMode: ShellRoutingLocalOnly,
		tools:            make(map[string]llm.Tool),
	}

	// Register enabled tools
	if err := r.registerEnabledTools(); err != nil {
		return nil, err
	}
	if primary := strings.TrimSpace(toolConfig.PrimaryWorkspaceValue()); primary != "" {
		if err := approvalMgr.SetPrimaryWorkspace(primary); err != nil {
			return nil, fmt.Errorf("bind primary workspace: %w", err)
		}
	}

	return r, nil
}

// SetImageRecorder wires an image recorder for image generation tracking into
// the already-registered image generation tool.
func (r *LocalToolRegistry) SetImageRecorder(recorder ImageRecorder, agent, sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.memoryStore = recorder
	r.agent = agent
	r.sessionID = sessionID
	r.applyImageRecorderLocked()
}

// applyImageRecorderLocked pushes the recorder and attribution into the
// registered image generation tool. Callers must hold r.mu.
func (r *LocalToolRegistry) applyImageRecorderLocked() {
	if t, ok := r.tools[ImageGenerateToolName]; ok {
		if it, ok := t.(*ImageGenerateTool); ok {
			it.imageRecorder = r.memoryStore
			it.agent = r.agent
			it.sessionID = r.sessionID
		}
	}
}

// SetFileChangeRecorder wires a recorder for file-change tracking into the
// already-registered file-modifying tools. This mutates registered instances
// directly (the SetServeMode pattern) so the recorder takes effect regardless
// of registration order.
func (r *LocalToolRegistry) SetFileChangeRecorder(recorder FileChangeRecorder) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.fileRecorder = recorder
	r.applyFileRecorderLocked()
}

// applyFileRecorderLocked pushes r.fileRecorder into registered tool
// instances. Callers must hold r.mu. SetLimits re-creates some tools, so it
// re-applies the recorder through this helper.
func (r *LocalToolRegistry) applyFileRecorderLocked() {
	if t, ok := r.tools[WriteFileToolName]; ok {
		if wt, ok := t.(*WriteFileTool); ok {
			wt.recorder = r.fileRecorder
		}
	}
	if t, ok := r.tools[EditFileToolName]; ok {
		if et, ok := t.(*EditFileTool); ok {
			et.recorder = r.fileRecorder
		}
	}
	if t, ok := r.tools[UnifiedDiffToolName]; ok {
		if ut, ok := t.(*UnifiedDiffTool); ok {
			ut.recorder = r.fileRecorder
		}
	}
	if t, ok := r.tools[ShellToolName]; ok {
		if st, ok := t.(*ShellTool); ok {
			st.setFileChangeRecorder(r.fileRecorder)
		}
	}
}

func (r *LocalToolRegistry) applyCollaborativeShellLocked() {
	if t, ok := r.tools[ShellToolName].(*ShellTool); ok {
		t.setCollaborativeShell(r.collaborativeShellController, r.shellRoutingMode)
	}
}

// SetCollaborativeShellController configures authoritative shell routing. Web
// runtimes use controller_required; all other callers retain local_only.
func (r *LocalToolRegistry) SetCollaborativeShellController(controller CollaborativeShellController, mode ShellRoutingMode) {
	if r == nil {
		return
	}
	if mode == "" {
		mode = ShellRoutingLocalOnly
	}
	r.mu.Lock()
	r.collaborativeShellController = controller
	r.shellRoutingMode = mode
	r.applyCollaborativeShellLocked()
	r.mu.Unlock()
}

func (r *LocalToolRegistry) CollaborativeShellActivityController() CollaborativeShellActivityController {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	controller, _ := r.collaborativeShellController.(CollaborativeShellActivityController)
	return controller
}

func (r *LocalToolRegistry) CollaborativeShellRouting() (ShellRoutingMode, bool) {
	if r == nil {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.shellRoutingMode, r.collaborativeShellController != nil
}

func (r *LocalToolRegistry) CollaborativeShellMode(ctx context.Context, sessionID string) CollaborativeShellMode {
	if r == nil {
		return CollaborativeShellMode{State: CollaborativeShellUnavailable}
	}
	r.mu.RLock()
	controller := r.collaborativeShellController
	mode := r.shellRoutingMode
	r.mu.RUnlock()
	if mode == ShellRoutingLocalOnly {
		return CollaborativeShellMode{State: CollaborativeShellOff}
	}
	if controller == nil {
		return CollaborativeShellMode{State: CollaborativeShellUnavailable, Reason: "controller unavailable"}
	}
	return controller.Mode(ctx, sessionID)
}

// HasVisibleShell reports whether shell is registered in this registry.
func (r *LocalToolRegistry) HasVisibleShell() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tools[ShellToolName]
	return ok && r.config.IsToolEnabled(ShellToolName)
}

// registerEnabledTools registers all tools that are enabled in config.
func (r *LocalToolRegistry) registerEnabledTools() error {
	hasPathTool := false
	for _, specName := range r.config.Enabled {
		if specName == ManageWorkspaceToolName {
			continue
		}
		if IsPathCapableTool(specName) {
			hasPathTool = true
		}
		if err := r.registerTool(specName); err != nil {
			return err
		}
	}
	if hasPathTool {
		return r.registerTool(ManageWorkspaceToolName)
	}
	return nil
}

// IsPathCapableTool reports whether name is a local file/path operation that
// requires access to manage_workspace so explicit tool lists cannot strand it.
func IsPathCapableTool(name string) bool {
	switch name {
	case ReadFileToolName, WriteFileToolName, EditFileToolName, UnifiedDiffToolName,
		GrepToolName, GlobToolName, ViewImageToolName, ShowImageToolName, ShowMediaToolName, ImageGenerateToolName:
		return true
	default:
		return false
	}
}

// registerTool registers a single tool by spec name.
func (r *LocalToolRegistry) registerTool(specName string) error {
	if !ValidToolName(specName) {
		return NewToolErrorf(ErrInvalidParams, "unknown tool: %s", specName)
	}

	var tool llm.Tool

	switch specName {
	case ReadFileToolName:
		tool = NewReadFileTool(r.approval, r.limits, r.config)
	case WriteFileToolName:
		tool = NewWriteFileTool(r.approval, r.config)
	case EditFileToolName:
		tool = NewEditFileTool(r.approval, r.config)
	case UnifiedDiffToolName:
		tool = NewUnifiedDiffTool(r.approval, r.config)
	case ShellToolName:
		tool = NewShellTool(r.approval, r.config, r.limits)
	case GrepToolName:
		tool = NewGrepTool(r.approval, r.limits, r.config)
	case GlobToolName:
		tool = NewGlobTool(r.approval, r.config)
	case ViewImageToolName:
		tool = NewViewImageTool(r.approval, r.config)
	case ShowImageToolName:
		tool = NewShowImageTool(r.approval, r.config)
	case ShowMediaToolName:
		mediaTool := NewShowMediaTool(r.approval, r.config)
		mediaTool.SetPublisher(r.mediaPublisher)
		tool = mediaTool
	case ImageGenerateToolName:
		tool = NewImageGenerateTool(r.approval, r.appConfig, r.config.ImageProvider, r.memoryStore, r.agent, r.sessionID, r.config)
	case AskUserToolName:
		tool = NewAskUserTool()
	case SpawnAgentToolName:
		// SpawnAgentTool requires a runner to be set later via SetRunner
		tool = NewSpawnAgentTool(r.config.Spawn, 0)
	case QueueAgentToolName:
		tool = NewQueueAgentTool(r.config)
	case WaitForJobsToolName:
		tool = NewWaitForJobsTool()
	case HubDelegateToolName:
		tool = NewHubDelegateTool()
	case HubCheckDelegationToolName:
		tool = NewHubCheckDelegationTool()
	case RunAgentScriptToolName:
		tool = NewRunAgentScriptTool(r.config, r.limits)
	case InitiateHandoverToolName:
		tool = NewInitiateHandoverTool()
	case ManageWorkspaceToolName:
		tool = NewManageWorkspaceTool(r.approval, r.config)
	case UpdatePlanToolName:
		controller := NewPlanController(nil)
		controller.SetPromptGuidance(r.config.PlanGuidance)
		tool = NewUpdatePlanTool(controller)
	default:
		return NewToolErrorf(ErrInvalidParams, "unimplemented tool: %s", specName)
	}

	r.tools[specName] = tool
	if specName == ShellToolName {
		r.applyCollaborativeShellLocked()
	}
	return nil
}

// SetViewImageVisionProvider switches view_image into routed-vision mode. If
// view_image was not enabled, it is registered so the primary model can call it.
func (r *LocalToolRegistry) SetViewImageVisionProvider(provider llm.Provider, model string) {
	if r == nil || provider == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[ViewImageToolName] = NewViewImageToolWithVision(r.approval, provider, model, r.config)
	if _, ok := r.tools[ManageWorkspaceToolName]; !ok {
		r.tools[ManageWorkspaceToolName] = NewManageWorkspaceTool(r.approval, r.config)
	}
	if !stringSliceContains(r.config.Enabled, ViewImageToolName) {
		r.config.Enabled = append(r.config.Enabled, ViewImageToolName)
	}
}

func stringSliceContains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// RegisterWithEngine registers all enabled tools with the LLM engine.
func (r *LocalToolRegistry) RegisterWithEngine(engine *llm.Engine) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, tool := range r.tools {
		engine.RegisterTool(tool)
	}
	if lifecycle, ok := r.fileRecorder.(llm.FileTrackingRunLifecycle); ok {
		engine.SetFileTrackingRunLifecycle(lifecycle)
	}
}

// SetPlanStore wires durable latest-snapshot persistence only when update_plan
// was explicitly configured and registered.
func (r *LocalToolRegistry) SetPlanStore(store session.Store) {
	if r == nil || store == nil {
		return
	}
	planStore, ok := store.(session.PlanSnapshotStore)
	if !ok {
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[UpdatePlanToolName].(*UpdatePlanTool)
	if ok {
		tool.controller.SetStore(planStore)
	}
}

// GetSpecs returns tool specs for all enabled tools.
func (r *LocalToolRegistry) GetSpecs() []llm.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()

	specs := make([]llm.ToolSpec, 0, len(r.tools))
	for _, tool := range r.tools {
		specs = append(specs, tool.Spec())
	}
	return specs
}

// Get returns a tool by spec name.
func (r *LocalToolRegistry) Get(specName string) (llm.Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, ok := r.tools[specName]
	return tool, ok
}

// IsEnabled checks if a tool is enabled.
func (r *LocalToolRegistry) IsEnabled(specName string) bool {
	return r.config.IsToolEnabled(specName)
}

// Permissions returns the underlying permissions manager.
func (r *LocalToolRegistry) Permissions() *ToolPermissions {
	return r.permissions
}

// SetLimits updates the output limits.
func (r *LocalToolRegistry) SetLimits(limits OutputLimits) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.limits = limits
	// Re-register tools that use limits
	for _, specName := range r.config.Enabled {
		switch specName {
		case ReadFileToolName:
			r.tools[specName] = NewReadFileTool(r.approval, r.limits, r.config)
		case ShellToolName:
			r.tools[specName] = NewShellTool(r.approval, r.config, r.limits)
		case GrepToolName:
			r.tools[specName] = NewGrepTool(r.approval, r.limits, r.config)
		case RunAgentScriptToolName:
			r.tools[specName] = NewRunAgentScriptTool(r.config, r.limits)
		}
	}
	// Re-created tools start without runtime wiring; push it back in.
	r.applyFileRecorderLocked()
	r.applyCollaborativeShellLocked()
}

// AddReadDir adds a directory to the read allowlist at runtime.
func (r *LocalToolRegistry) AddReadDir(dir string) error {
	return r.permissions.AddReadDir(dir)
}

// AddWriteDir adds a directory to the write allowlist at runtime.
func (r *LocalToolRegistry) AddWriteDir(dir string) error {
	return r.permissions.AddWriteDir(dir)
}

// AddShellPattern adds a shell pattern to the allowlist at runtime.
func (r *LocalToolRegistry) AddShellPattern(pattern string) error {
	return r.permissions.AddShellPattern(pattern)
}

// SetBaseDir updates the registry's per-session working directory and records a
// pending primary workspace proposal. Dynamic grants are preserved and shell
// approval remains independent.
func (r *LocalToolRegistry) SetBaseDir(dir string) error {
	return r.SetBaseDirWithContext(context.Background(), dir)
}

// SetBaseDirWithContext is the context-aware session/worktree rebinding path.
func (r *LocalToolRegistry) SetBaseDirWithContext(ctx context.Context, dir string) error {
	if r == nil || r.config == nil {
		return nil
	}
	if r.config.RequiresExplicitWorkingDir() && r.config.WorkingDir() == "" && !filepath.IsAbs(strings.TrimSpace(dir)) {
		return NewToolError(ErrInvalidParams, "relative base directory requires an absolute path when the session is unbound")
	}
	resolved := r.config.ResolveDir(dir)
	if resolved == "" {
		return NewToolError(ErrInvalidParams, "base directory is empty")
	}
	canonical, err := canonicalWorkspaceDirectory(resolved)
	if err != nil {
		return err
	}
	if err := r.approval.SetPrimaryWorkspaceWithContext(ctx, canonical); err != nil {
		return err
	}

	r.mu.Lock()
	r.config.UpdateBaseDir(canonical)
	r.mu.Unlock()
	return nil
}

// BaseDir returns the current per-session base directory, if any.
func (r *LocalToolRegistry) BaseDir() string {
	if r == nil || r.config == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.config.BaseDirValue()
}

// SetServeMode marks tools as running in serve (web/telegram) mode.
// This strips terminal-only params like copy_to_clipboard and show_image
// from tool specs and disables clipboard operations during execution.
// imageBaseURL is retained for compatibility with older callers; generated
// images are now reported through ToolOutput.Images and served by the
// response-stream/session layers.
func (r *LocalToolRegistry) SetServeMode(enabled bool, imageBaseURL string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if t, ok := r.tools[ImageGenerateToolName]; ok {
		if ig, ok := t.(*ImageGenerateTool); ok {
			ig.serveMode = enabled
			ig.serveImageBaseURL = imageBaseURL
		}
	}
	if t, ok := r.tools[ShowImageToolName]; ok {
		if si, ok := t.(*ShowImageTool); ok {
			si.serveMode = enabled
		}
	}
}

// SetMediaPublisher installs durable media publication for show_media. It is
// intentionally separate from SetServeMode so legacy show_image behavior is
// unchanged.
func (r *LocalToolRegistry) SetMediaPublisher(publisher MediaPublisher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mediaPublisher = publisher
	if tool, ok := r.tools[ShowMediaToolName].(*ShowMediaTool); ok {
		tool.SetPublisher(publisher)
	}
	if tool, ok := r.tools[SpawnAgentToolName].(*SpawnAgentTool); ok {
		tool.SetMediaPublisher(publisher)
	}
}

// MediaPublisher returns the publisher configured for this registry, including
// registries whose own enabled tool set does not contain show_media.
func (r *LocalToolRegistry) MediaPublisher() MediaPublisher {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.mediaPublisher
}

// ToolManager provides a high-level interface for tool management in commands.
type ToolManager struct {
	Registry    *LocalToolRegistry
	ApprovalMgr *ApprovalManager
}

// NewToolManager creates a new tool manager from config.
func NewToolManager(toolConfig *ToolConfig, appConfig *config.Config) (*ToolManager, error) {
	// Build permissions first to create ApprovalManager
	perms, err := toolConfig.BuildPermissions()
	if err != nil {
		return nil, err
	}

	// Create approval manager first so it can be shared with tools
	approvalMgr := NewApprovalManager(perms)

	// Create registry, passing the approval manager
	registry, err := NewLocalToolRegistry(toolConfig, appConfig, approvalMgr)
	if err != nil {
		return nil, err
	}

	return &ToolManager{
		Registry:    registry,
		ApprovalMgr: approvalMgr,
	}, nil
}

// SetBaseDir updates the per-manager base directory used by all local tools.
func (m *ToolManager) SetBaseDir(dir string) error {
	return m.SetBaseDirWithContext(context.Background(), dir)
}

// SetBaseDirWithContext is the context-aware session/worktree rebinding path.
func (m *ToolManager) SetBaseDirWithContext(ctx context.Context, dir string) error {
	if m == nil || m.Registry == nil {
		return nil
	}
	return m.Registry.SetBaseDirWithContext(ctx, dir)
}

// ClearPrimaryWorkspace removes an explicit session binding and its pending or
// confirmed primary capability while preserving dynamic grants.
func (m *ToolManager) ClearPrimaryWorkspace(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if m.ApprovalMgr != nil {
		if err := m.ApprovalMgr.ClearPrimaryWorkspace(ctx); err != nil {
			return err
		}
	}
	if m.Registry != nil && m.Registry.config != nil {
		m.Registry.config.ClearBaseDir()
	}
	return nil
}

// BaseDir returns the current per-session base directory, if any.
func (m *ToolManager) BaseDir() string {
	if m == nil || m.Registry == nil {
		return ""
	}
	return m.Registry.BaseDir()
}

// ConfigureWorkspacePersistence installs optional session grant persistence and
// rehydrates additional grants before tool execution.
func (m *ToolManager) ConfigureWorkspacePersistence(ctx context.Context, store session.Store, sessionID string) error {
	if m == nil || m.ApprovalMgr == nil {
		return nil
	}
	return m.ApprovalMgr.ConfigureWorkspacePersistence(ctx, store, sessionID)
}

// IsReadPathApproved reports whether path may be read without prompting. It is
// intentionally read-only: it neither invokes approval UI nor adds permissions.
func (m *ToolManager) IsReadPathApproved(path string) bool {
	if m == nil || m.ApprovalMgr == nil {
		return false
	}
	if m.ApprovalMgr.YoloEnabled() {
		return true
	}
	absPath, err := canonicalApprovalPath(path, false)
	if err != nil {
		return false
	}
	outcome, decided, err := m.ApprovalMgr.checkPathApprovalNoPrompt(ReadFileToolName, absPath, absPath, false)
	return err == nil && decided && outcome != Cancel
}

// SetupEngine registers tools with the engine.
func (m *ToolManager) SetupEngine(engine *llm.Engine) {
	m.Registry.RegisterWithEngine(engine)
}

// FilterToolSpecsForApprovalMode removes tools that have no model-facing use in
// the effective approval mode. Executors stay registered so an in-flight call
// issued before a mode change can still complete safely.
func FilterToolSpecsForApprovalMode(specs []llm.ToolSpec, approval *ApprovalManager) []llm.ToolSpec {
	if approval == nil || !approval.YoloEnabled() {
		return specs
	}
	filtered := make([]llm.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if spec.Name != ManageWorkspaceToolName {
			filtered = append(filtered, spec)
		}
	}
	return filtered
}

// GetSpecs returns all model-facing tool specs for the effective approval mode.
func (m *ToolManager) GetSpecs() []llm.ToolSpec {
	if m == nil || m.Registry == nil {
		return nil
	}
	return FilterToolSpecsForApprovalMode(m.Registry.GetSpecs(), m.ApprovalMgr)
}

// GetSpawnAgentTool returns the spawn_agent tool if enabled, for runner configuration.
func (m *ToolManager) GetSpawnAgentTool() *SpawnAgentTool {
	if m == nil {
		return nil
	}
	return m.Registry.GetSpawnAgentTool()
}

// GetSpawnAgentTool returns the spawn_agent tool if enabled.
func (r *LocalToolRegistry) GetSpawnAgentTool() *SpawnAgentTool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[SpawnAgentToolName]
	if !ok {
		return nil
	}
	if spawnTool, ok := tool.(*SpawnAgentTool); ok {
		return spawnTool
	}
	return nil
}

// RegisterOutputTool adds an output tool to the local registry.
func (r *LocalToolRegistry) RegisterOutputTool(tool *SetOutputTool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tools[tool.Name()] = tool
}

// GetOutputTool returns the output tool by name if it exists and is a SetOutputTool.
func (r *LocalToolRegistry) GetOutputTool(name string) *SetOutputTool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, ok := r.tools[name]
	if !ok {
		return nil
	}
	if outputTool, ok := tool.(*SetOutputTool); ok {
		return outputTool
	}
	return nil
}

// RegisterSkillTool registers the activate_skill tool with the given registry.
// This must be called after the skills registry is created.
func (r *LocalToolRegistry) RegisterSkillTool(skillRegistry *skills.Registry) *ActivateSkillTool {
	r.mu.Lock()
	defer r.mu.Unlock()

	tool := NewActivateSkillTool(skillRegistry, r.approval)
	r.tools[ActivateSkillToolName] = tool
	return tool
}

// RegisterSkillTools registers script-backed tools declared in a skill's frontmatter.
// skillDir is the skill's SourcePath (absolute directory containing SKILL.md).
// Tools are resolved relative to skillDir and executed from there.
// Duplicate registrations (same name) overwrite the previous entry — activating
// a skill twice is idempotent. Name collisions with built-in tools are rejected.
func (r *LocalToolRegistry) RegisterSkillTools(defs []skills.SkillToolDef, skillDir string) error {
	// Convert SkillToolDef → agents.CustomToolDef
	agentDefs := make([]agents.CustomToolDef, 0, len(defs))
	for _, d := range defs {
		if d.Name == "" {
			return fmt.Errorf("skill tool: name is required")
		}
		if d.Description == "" {
			return fmt.Errorf("skill tool %q: description is required", d.Name)
		}
		if d.Script == "" {
			return fmt.Errorf("skill tool %q: script is required", d.Name)
		}
		agentDefs = append(agentDefs, agents.CustomToolDef{
			Name:           d.Name,
			Description:    d.Description,
			Script:         d.Script,
			Input:          d.Input,
			TimeoutSeconds: d.TimeoutSeconds,
			Env:            d.Env,
			Call:           d.Call,
		})
	}

	// Warn if the skill dir doesn't exist (non-fatal, matches existing behaviour)
	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: skill directory not found: %s\n", skillDir)
	}

	return r.RegisterCustomTools(agentDefs, skillDir)
}

// RestoreSkillTools removes tools registered for a skill turn and restores any
// same-named tools that were present before that turn.
func (r *LocalToolRegistry) RestoreSkillTools(names []string, previous map[string]llm.Tool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range names {
		if tool := previous[name]; tool != nil {
			r.tools[name] = tool
		} else {
			delete(r.tools, name)
		}
	}
}

// GetSkillTool returns the activate_skill tool if registered.
func (r *LocalToolRegistry) GetSkillTool() *ActivateSkillTool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, ok := r.tools[ActivateSkillToolName]
	if !ok {
		return nil
	}
	if skillTool, ok := tool.(*ActivateSkillTool); ok {
		return skillTool
	}
	return nil
}
