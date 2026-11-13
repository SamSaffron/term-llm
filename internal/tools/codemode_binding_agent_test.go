package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools/agentwait"
)

type bindingAgentRunner struct {
	mu       sync.Mutex
	depths   []int
	entered  chan string
	release  chan struct{}
	finished chan string
}

func (r *bindingAgentRunner) RunAgent(ctx context.Context, name, prompt string, depth int) (SpawnAgentRunResult, error) {
	return r.RunAgentWithCallback(ctx, name, prompt, depth, "", nil)
}

func (r *bindingAgentRunner) RunAgentWithCallback(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback) (SpawnAgentRunResult, error) {
	return r.RunAgentWithCallbackAndOptions(ctx, name, prompt, depth, id, cb, SpawnAgentRunOptions{})
}

func (r *bindingAgentRunner) RunAgentWithCallbackAndOptions(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error) {
	r.mu.Lock()
	r.depths = append(r.depths, depth)
	r.mu.Unlock()
	if r.entered != nil {
		r.entered <- opts.ChildSessionID
	}
	select {
	case <-r.release:
		if r.finished != nil {
			r.finished <- opts.ChildSessionID
		}
		return SpawnAgentRunResult{Output: "done", SessionID: opts.ChildSessionID}, nil
	case <-ctx.Done():
		return SpawnAgentRunResult{SessionID: opts.ChildSessionID}, ctx.Err()
	}
}

func (r *bindingAgentRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.depths)
}

func newAgentBindingRegistry(t *testing.T, enabled ...string) *ToolManager {
	t.Helper()
	cfg := DefaultToolConfig()
	cfg.Enabled = enabled
	manager, err := NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func captureBindingAuthority(t *testing.T, registry *LocalToolRegistry, sessionID string, names []string) core.CodeModeAuthority {
	t.Helper()
	authority, err := registry.captureCodeModeAuthority(context.Background(), core.CodeModeCellRef{SessionID: sessionID, CellID: "cell", Ctx: context.Background()}, names, nil)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func TestCodeModeAgentAuthoritySharesConfiguredServiceAndBindsControls(t *testing.T) {
	manager := newAgentBindingRegistry(t, SpawnAgentToolName, WaitAgentToolName, CancelAgentToolName, ListAgentsToolName)
	registry := manager.Registry
	live := registry.GetSpawnAgentTool()
	if live == nil {
		t.Fatal("missing configured spawn tool")
	}
	runner := &bindingAgentRunner{entered: make(chan string, 1), release: make(chan struct{})}
	live.SetRunner(runner)

	// The engine admits names sorted, so cancel_agent precedes spawn_agent.
	names := []string{CancelAgentToolName, ListAgentsToolName, SpawnAgentToolName, WaitAgentToolName}
	authority := captureBindingAuthority(t, registry, "binding-agent-parent", names)
	defer authority.Close()

	spawnTool, ok := authority.Tool(SpawnAgentToolName)
	if !ok {
		t.Fatal("spawn_agent was not bound")
	}
	bound := spawnTool.(*SpawnAgentTool)
	if bound == live {
		t.Fatal("authority reused the mutable live spawn tool")
	}
	if bound.manager != live.manager {
		t.Fatal("bound spawn tool does not share the configured child registry")
	}
	for _, name := range []string{WaitAgentToolName, CancelAgentToolName, ListAgentsToolName} {
		control, ok := authority.Tool(name)
		if !ok {
			t.Fatalf("%s was not bound", name)
		}
		if control.(*agentControlTool).spawn != bound {
			t.Fatalf("%s bound a different spawn service", name)
		}
	}

	ctx := core.ContextWithSessionID(context.Background(), "binding-agent-parent")
	spawned := lifecycleResult(t, lifecycleCall(t, spawnTool, ctx, `{"agent_name":"developer","prompt":"work","wait":0}`))
	if spawned.AgentID == "" {
		t.Fatalf("spawn = %+v", spawned)
	}
	select {
	case <-runner.entered:
	case <-time.After(time.Second):
		t.Fatal("configured runner never received the nested spawn")
	}
	// The child is registered in the host's manager, so shutdown/slots remain
	// authoritative even though the script used a frozen policy view.
	live.manager.mu.Lock()
	_, shared := live.manager.agents[spawned.AgentID]
	live.manager.mu.Unlock()
	if !shared {
		t.Fatal("nested child was not registered in the shared child registry")
	}
	if current, ok := processAgentEntries.Load(spawned.AgentID); !ok || current.(*agentEntry).manager != live.manager {
		t.Fatal("nested child was not registered in the process child registry")
	}

	var observed []session.AgentRun
	waitCtx := agentwait.WithObserver(ctx, func(row session.AgentRun) { observed = append(observed, row) })
	wait := bindingTool(t, authority, WaitAgentToolName)
	out := lifecycleCall(t, wait, waitCtx, `{"agent_ids":["`+spawned.AgentID+`"],"max_wait":1}`)
	if !strings.Contains(out.Content, `"status":"running"`) {
		t.Fatalf("nested wait = %s", out.Content)
	}
	// A positive nested wait must satisfy agentwait bookkeeping for the host's
	// turn finalization, using the originating session lineage.
	if len(observed) != 1 || observed[0].ID != spawned.AgentID || observed[0].ParentSessionID != "binding-agent-parent" {
		t.Fatalf("agentwait observation = %+v", observed)
	}

	list := bindingTool(t, authority, ListAgentsToolName)
	if out := lifecycleCall(t, list, ctx, `{}`); !strings.Contains(out.Content, spawned.AgentID) {
		t.Fatalf("nested list = %s", out.Content)
	}

	cancel := bindingTool(t, authority, CancelAgentToolName)
	if out := lifecycleCall(t, cancel, ctx, `{"agent_id":"`+spawned.AgentID+`"}`); !strings.Contains(out.Content, `"status":"cancelled"`) {
		t.Fatalf("nested cancel = %s", out.Content)
	}
	close(runner.release)

	authority.Close()
	live.manager.mu.Lock()
	draining := live.manager.draining
	live.manager.mu.Unlock()
	if draining {
		t.Fatal("ending one cell closed the shared child registry")
	}
}

func bindingTool(t *testing.T, authority core.CodeModeAuthority, name string) core.Tool {
	t.Helper()
	tool, ok := authority.Tool(name)
	if !ok {
		t.Fatalf("%s was not bound", name)
	}
	return tool
}

func TestCodeModeAgentAuthorityPinsRunnerAndDepthAcrossHostRewiring(t *testing.T) {
	manager := newAgentBindingRegistry(t, SpawnAgentToolName)
	registry := manager.Registry
	live := registry.GetSpawnAgentTool()
	first := &bindingAgentRunner{entered: make(chan string, 1), release: make(chan struct{})}
	live.SetRunner(first)
	live.SetDepth(0)
	live.SetRemainingDepth(2)

	authority := captureBindingAuthority(t, registry, "pinned-parent", []string{SpawnAgentToolName})
	defer authority.Close()
	bound := bindingTool(t, authority, SpawnAgentToolName)

	// A later turn/request rebinds the live tool. The admitted cell must keep
	// the runner and parent depth it captured.
	second := &bindingAgentRunner{entered: make(chan string, 1), release: make(chan struct{})}
	live.SetRunner(second)
	live.SetDepth(5)

	ctx := core.ContextWithSessionID(context.Background(), "pinned-parent")
	spawned := lifecycleResult(t, lifecycleCall(t, bound, ctx, `{"agent_name":"developer","prompt":"work","wait":0}`))
	select {
	case <-first.entered:
	case <-time.After(time.Second):
		t.Fatal("frozen runner did not receive the nested spawn")
	}
	if second.callCount() != 0 {
		t.Fatal("nested spawn was retargeted to the later runner")
	}
	first.mu.Lock()
	depth := first.depths[0]
	first.mu.Unlock()
	if depth != 1 {
		t.Fatalf("nested spawn depth = %d, want pinned depth 1", depth)
	}
	close(first.release)
	if err := live.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = spawned
}

func TestCodeModeBindingsFailClosedPerToolWithoutConfiguredService(t *testing.T) {
	// A registered control without its configured spawn service denies only that
	// tool: unrelated calls in the same cell still work and never fall back to a
	// mutable live instance.
	cfg := DefaultToolConfig()
	cfg.Enabled = []string{SpawnAgentToolName, ReadFileToolName}
	manager, err := NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := manager.Registry
	registry.mu.Lock()
	registry.tools[WaitAgentToolName] = &agentControlTool{name: WaitAgentToolName}
	delete(registry.tools, SpawnAgentToolName)
	registry.mu.Unlock()
	authority := captureBindingAuthority(t, registry, "denied-parent", []string{WaitAgentToolName, ReadFileToolName})
	defer authority.Close()
	control, ok := authority.Tool(WaitAgentToolName)
	if !ok {
		t.Fatal("denied control is absent instead of failing closed per tool")
	}
	if _, ok := control.(*codeModeUnavailableTool); !ok {
		t.Fatalf("denied control type = %T", control)
	}
	out := lifecycleCall(t, control, context.Background(), `{"agent_ids":["child"]}`)
	if !out.IsError || !strings.Contains(out.Content, codeModeBindingUnavailableCode) {
		t.Fatalf("denied control = %#v", out)
	}
	if _, ok := authority.Tool(ReadFileToolName); !ok {
		t.Fatal("unrelated tool was excluded with the denied service")
	}

	// update_plan must share the configured controller, never synthesize a new
	// empty one that reports a no-op success.
	cfg = DefaultToolConfig()
	cfg.Enabled = []string{UpdatePlanToolName}
	manager, err = NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry = manager.Registry
	registry.mu.Lock()
	registry.tools[UpdatePlanToolName] = struct{ core.Tool }{Tool: registry.tools[UpdatePlanToolName]}
	registry.mu.Unlock()
	authority = captureBindingAuthority(t, registry, "denied-plan", []string{UpdatePlanToolName})
	defer authority.Close()
	plan, ok := authority.Tool(UpdatePlanToolName)
	if !ok {
		t.Fatal("unavailable update_plan is absent instead of failing closed")
	}
	out = lifecycleCall(t, plan, context.Background(), `{"plan":[{"step":"fake","status":"in_progress"}]}`)
	if !out.IsError || !strings.Contains(out.Content, codeModeBindingUnavailableCode) {
		t.Fatalf("unavailable plan reported success: %#v", out)
	}
}

func TestCodeModeUpdatePlanAuthoritySharesConfiguredController(t *testing.T) {
	cfg := DefaultToolConfig()
	cfg.Enabled = []string{UpdatePlanToolName}
	manager, err := NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := manager.Registry
	live, ok := registry.Get(UpdatePlanToolName)
	if !ok {
		t.Fatal("missing update_plan")
	}
	livePlan := live.(*UpdatePlanTool)
	store := newTestPlanStore()
	livePlan.controller.SetStore(store)

	authority := captureBindingAuthority(t, registry, "plan-session", []string{UpdatePlanToolName})
	defer authority.Close()
	bound := bindingTool(t, authority, UpdatePlanToolName).(*UpdatePlanTool)
	if bound == livePlan {
		t.Fatal("authority reused the mutable live plan tool")
	}
	if bound.controller != livePlan.controller {
		t.Fatal("bound update_plan did not share the session controller")
	}

	ctx := core.ContextWithSessionID(context.Background(), "plan-session")
	out := lifecycleCall(t, bound, ctx, `{"plan":[{"step":"Inspect binding","status":"in_progress"}]}`)
	if out.Content != "Plan updated" || out.IsError {
		t.Fatalf("nested update = %#v", out)
	}
	if store.saves != 1 || store.snapshots["plan-session"].Plan[0].Step != "Inspect binding" {
		t.Fatalf("nested plan did not persist through the configured controller: saves=%d", store.saves)
	}
	// The live request-context projection observes the same state, proving the
	// nested call did not succeed into a detached controller.
	messages, err := livePlan.PrepareRequestContext(ctx, "plan-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, message := range messages {
		text.WriteString(core.MessageText(message))
	}
	if !strings.Contains(text.String(), "<current_execution_plan>") || !strings.Contains(text.String(), "Inspect binding") {
		t.Fatalf("live plan projection missing nested update: %q", text.String())
	}
}
