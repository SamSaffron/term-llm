package tools

import (
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	"github.com/samsaffron/term-llm/internal/skills"
)

const codeModeBindingUnavailableCode = "binding_unavailable"

var codeModeAgentControlNames = []string{
	WaitAgentToolName,
	ContinueAgentToolName,
	CancelAgentToolName,
	ListAgentsToolName,
}

// codeModeBindings freezes the configured services a cell cannot reconstruct:
// the shared spawn agent child registry, the jobs client, the session plan
// controller and the skill catalogue. Values are captured once at admission.
// A service that cannot be bound denies only the affected tool inside the cell
// (codeModeUnavailableTool); unrelated script calls still work and no scoped
// effect ever falls back to the mutable live instance.
type codeModeBindings struct {
	spawn     *SpawnAgentTool
	queue     *QueueAgentTool
	waitJobs  *WaitForJobsTool
	plan      *UpdatePlanTool
	runScript *RunAgentScriptTool
	skills    *skills.Registry
	denied    map[string]string
}

func (b *codeModeBindings) deny(name, reason string) {
	if b.denied == nil {
		b.denied = map[string]string{}
	}
	b.denied[name] = reason
}

// captureCodeModeBindingsLocked resolves bindings while the registry read lock
// is held. Registry mutators (SetJobsServer, SetAgentModelAdmission,
// SetCollaborativeShellController, skill/tool registration) take the write lock;
// tool-level mutators take the owning tool's mutex, which the bind helpers below
// also take. Hosts must not assign these fields directly outside those APIs.
func (r *LocalToolRegistry) captureCodeModeBindingsLocked(shadow *LocalToolRegistry, names []string) *codeModeBindings {
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	b := &codeModeBindings{}
	spawnRequested := wanted[SpawnAgentToolName]
	for _, name := range codeModeAgentControlNames {
		spawnRequested = spawnRequested || wanted[name]
	}
	if spawnRequested {
		// Bind controls in two passes: the shared service first, then controls.
		// Never depend on the admitted name order (cancel_agent sorts before
		// spawn_agent) and never construct a second runner-less tool.
		if live, exists := r.tools[SpawnAgentToolName]; exists {
			if spawn, ok := live.(*SpawnAgentTool); ok && spawn != nil && spawn.manager != nil {
				b.spawn = spawn.bindCodeMode()
				if b.spawn.runner == nil {
					// wait/cancel/list still observe the shared child registry, but
					// starting or resuming work without a configured runner must fail
					// as an affected-tool denial rather than a live fallback.
					b.deny(SpawnAgentToolName, "the spawn agent runner is not configured")
					b.deny(ContinueAgentToolName, "the spawn agent runner is not configured")
				}
			} else {
				b.deny(SpawnAgentToolName, "the configured spawn agent service has an unexpected type")
			}
		}
		for _, name := range codeModeAgentControlNames {
			if _, exists := r.tools[name]; !exists {
				continue
			}
			if b.spawn == nil {
				b.deny(name, "the shared spawn agent service is not available")
			}
		}
	}
	if _, exists := r.tools[QueueAgentToolName]; exists && wanted[QueueAgentToolName] {
		if queue, ok := r.tools[QueueAgentToolName].(*QueueAgentTool); ok && queue != nil && queue.client != nil {
			b.queue = queue.bindCodeMode(shadow.approval, shadow.config)
		} else {
			b.deny(QueueAgentToolName, "the configured jobs client is unavailable")
		}
	}
	if _, exists := r.tools[WaitForJobsToolName]; exists && wanted[WaitForJobsToolName] {
		if wait, ok := r.tools[WaitForJobsToolName].(*WaitForJobsTool); ok && wait != nil && wait.client != nil {
			b.waitJobs = wait.bindCodeMode()
		} else {
			b.deny(WaitForJobsToolName, "the configured jobs client is unavailable")
		}
	}
	if _, exists := r.tools[UpdatePlanToolName]; exists && wanted[UpdatePlanToolName] {
		if plan, ok := r.tools[UpdatePlanToolName].(*UpdatePlanTool); ok && plan != nil && plan.controller != nil {
			// Share the session-keyed controller so nested updates persist and
			// are observed by the same request-context projection.
			b.plan = &UpdatePlanTool{controller: plan.controller}
		} else {
			b.deny(UpdatePlanToolName, "the configured plan controller is unavailable")
		}
	}
	if _, exists := r.tools[RunAgentScriptToolName]; exists && wanted[RunAgentScriptToolName] {
		if _, ok := r.tools[RunAgentScriptToolName].(*RunAgentScriptTool); ok {
			// The shadow config freezes AgentDir and the working directory at
			// admission; filename-only containment is enforced by the tool.
			b.runScript = NewRunAgentScriptTool(shadow.config, shadow.limits)
		} else {
			b.deny(RunAgentScriptToolName, "the configured agent script source is unavailable")
		}
	}
	if wanted[SearchSkillsToolName] {
		// The live search tool may be registered directly on the engine rather
		// than in this registry. Prefer its catalogue when present; otherwise use
		// the registry that activation shares, which is the same namespace.
		if search, ok := r.tools[SearchSkillsToolName].(*SearchSkillsTool); ok && search != nil && search.registry != nil {
			b.skills = search.registry
		} else if activate, ok := r.tools[ActivateSkillToolName].(*ActivateSkillTool); ok && activate != nil && activate.registry != nil {
			b.skills = activate.registry
		} else if _, localSearch := r.tools[SearchSkillsToolName]; localSearch {
			b.deny(SearchSkillsToolName, "the configured skill catalogue is unavailable")
		} else if _, localActivate := r.tools[ActivateSkillToolName]; localActivate {
			b.deny(SearchSkillsToolName, "the configured skill catalogue is unavailable")
		}
		// No local registration at all: leave the name to live resolution (for
		// example an engine-registered or remote tool).
	}
	return b
}

// codeModeBindingManagedName reports names owned by the binding passes. The
// generic descriptor constructor must not synthesize a local tool for them: a
// remote or host-registered tool with the same name keeps live resolution.
func codeModeBindingManagedName(name string) bool {
	switch name {
	case SpawnAgentToolName, QueueAgentToolName, WaitForJobsToolName, UpdatePlanToolName,
		SearchSkillsToolName, RunAgentScriptToolName:
		return true
	}
	for _, control := range codeModeAgentControlNames {
		if control == name {
			return true
		}
	}
	return false
}

// bindCodeModeServices installs the frozen service tools and per-tool denials
// that are callable in the admitted name list. A name that is not admitted is
// never added.
func (b *codeModeBindings) bindCodeModeServices(shadow *LocalToolRegistry, names []string) {
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	if b.spawn != nil && wanted[SpawnAgentToolName] {
		shadow.tools[SpawnAgentToolName] = b.spawn
	}
	for _, name := range codeModeAgentControlNames {
		if wanted[name] && b.spawn != nil {
			shadow.tools[name] = &agentControlTool{name: name, spawn: b.spawn}
		}
	}
	if b.queue != nil && wanted[QueueAgentToolName] {
		shadow.tools[QueueAgentToolName] = b.queue
	}
	if b.waitJobs != nil && wanted[WaitForJobsToolName] {
		shadow.tools[WaitForJobsToolName] = b.waitJobs
	}
	if b.plan != nil && wanted[UpdatePlanToolName] {
		shadow.tools[UpdatePlanToolName] = b.plan
	}
	if b.runScript != nil && wanted[RunAgentScriptToolName] {
		shadow.tools[RunAgentScriptToolName] = b.runScript
	}
	if b.skills != nil && wanted[SearchSkillsToolName] {
		// search_skills reads the catalogue only; activation remains a direct
		// tool and is never implied by a search.
		shadow.tools[SearchSkillsToolName] = NewSearchSkillsTool(b.skills)
	}
	for name, reason := range b.denied {
		if wanted[name] {
			shadow.tools[name] = &codeModeUnavailableTool{name: name, reason: codeModeBindingUnavailableCode + ": " + reason}
		}
	}
}

// bindCodeMode returns a cell-bound view of the configured spawn service. It
// shares the live child manager (so host shutdown, slots and the process-wide
// entry registry stay authoritative) while freezing the parent's policy. The
// struct is constructed field-by-field: copying SpawnAgentTool would copy its
// mutex, and calling SetRunner would mutate the shared manager and runner
// lifecycle wiring.
func (t *SpawnAgentTool) bindCodeMode() *SpawnAgentTool {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return &SpawnAgentTool{
		runner:         t.runner,
		mediaPublisher: t.mediaPublisher,
		config:         cloneSpawnConfig(t.config),
		depth:          t.depth,
		remainingDepth: t.remainingDepth,
		manager:        t.manager,
		// Host display callbacks are request-scoped; nested progress uses the
		// cell-owned execution callback carried on the cell context.
		eventCallback: nil,
	}
}

func cloneSpawnConfig(config SpawnConfig) SpawnConfig {
	config.AllowedAgents = append([]string(nil), config.AllowedAgents...)
	if config.AgentModels != nil {
		models := make(map[string]string, len(config.AgentModels))
		for name, model := range config.AgentModels {
			models[name] = model
		}
		config.AgentModels = models
	}
	return config
}

// bindCodeMode freezes the configured jobs client and the request's model
// admission policy. A later turn replacing the live tool cannot retarget an
// already-admitted cell.
func (t *QueueAgentTool) bindCodeMode(approval *ApprovalManager, config *ToolConfig) *QueueAgentTool {
	if t == nil {
		return nil
	}
	bound := &QueueAgentTool{
		approval:                approval,
		client:                  t.client,
		config:                  config,
		triggerReconcileTimeout: t.triggerReconcileTimeout,
		admission:               t.admission,
	}
	if t.parentState != nil {
		policy, parent := t.parentState()
		bound.parentState = func() (modelpolicy.Policy, modelpolicy.ParentModel) {
			return policy, parent
		}
	}
	return bound
}

func (t *WaitForJobsTool) bindCodeMode() *WaitForJobsTool {
	if t == nil {
		return nil
	}
	return &WaitForJobsTool{client: t.client, pollIntervalOverride: t.pollIntervalOverride}
}
