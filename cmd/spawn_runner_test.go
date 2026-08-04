package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/tools"
)

type capturingSpawnRunner struct {
	lastAgentName string
	lastPrompt    string
	lastDepth     int
	lastOptions   tools.SpawnAgentRunOptions
	catalogNames  []string
}

func (r *capturingSpawnRunner) ListSpawnAgentNames() ([]string, error) {
	return append([]string(nil), r.catalogNames...), nil
}

func (r *capturingSpawnRunner) ResolveSpawnAgent(name string) error {
	for _, candidate := range r.catalogNames {
		if candidate == name {
			return nil
		}
	}
	return errors.New("definition not found")
}

func (r *capturingSpawnRunner) RunAgent(ctx context.Context, agentName string, prompt string, depth int) (tools.SpawnAgentRunResult, error) {
	return r.RunAgentWithOptions(ctx, agentName, prompt, depth, tools.SpawnAgentRunOptions{})
}

func (r *capturingSpawnRunner) RunAgentWithCallback(ctx context.Context, agentName string, prompt string, depth int, callID string, cb tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	return r.RunAgentWithCallbackAndOptions(ctx, agentName, prompt, depth, callID, cb, tools.SpawnAgentRunOptions{})
}

func (r *capturingSpawnRunner) RunAgentWithOptions(ctx context.Context, agentName string, prompt string, depth int, opts tools.SpawnAgentRunOptions) (tools.SpawnAgentRunResult, error) {
	r.lastAgentName = agentName
	r.lastPrompt = prompt
	r.lastDepth = depth
	r.lastOptions = opts
	return tools.SpawnAgentRunResult{Output: "ok"}, nil
}

func (r *capturingSpawnRunner) RunAgentWithCallbackAndOptions(ctx context.Context, agentName string, prompt string, depth int, callID string, cb tools.SubagentEventCallback, opts tools.SpawnAgentRunOptions) (tools.SpawnAgentRunResult, error) {
	return r.RunAgentWithOptions(ctx, agentName, prompt, depth, opts)
}

func TestSpawnRunnerWaitClosesRunAdmission(t *testing.T) {
	runner := &SpawnAgentRunner{}
	if !runner.beginRun() {
		t.Fatal("beginRun() rejected before shutdown")
	}

	waitDone := make(chan struct{})
	go func() {
		runner.Wait()
		close(waitDone)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		runner.runMu.Lock()
		draining := runner.draining
		runner.runMu.Unlock()
		if draining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Wait() did not close run admission")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-waitDone:
		t.Fatal("Wait() returned while a run was active")
	default:
	}

	runner.endRun()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("Wait() did not return after the active run ended")
	}

	if runner.beginRun() {
		runner.endRun()
		t.Fatal("beginRun() accepted a run after Wait began")
	}
	_, err := runner.runChildInternal(context.Background(), runpkg.ChildRunRequest{}, nil)
	if !errors.Is(err, errSpawnAgentRunnerDraining) {
		t.Fatalf("runChildInternal() error = %v, want %v", err, errSpawnAgentRunnerDraining)
	}
}

func TestSpawnRunnerWaitContendsWithRunAdmission(t *testing.T) {
	const workers = 32

	runner := &SpawnAgentRunner{}
	if !runner.beginRun() {
		t.Fatal("beginRun() rejected before shutdown")
	}

	start := make(chan struct{})
	rejected := make(chan struct{}, workers)
	for range workers {
		go func() {
			<-start
			for runner.beginRun() {
				runtime.Gosched()
				runner.endRun()
			}
			rejected <- struct{}{}
		}()
	}

	waitDone := make(chan struct{})
	go func() {
		<-start
		runner.Wait()
		close(waitDone)
	}()
	close(start)

	for range workers {
		select {
		case <-rejected:
		case <-time.After(time.Second):
			runner.endRun()
			t.Fatal("timed out waiting for run admission to close")
		}
	}

	select {
	case <-waitDone:
		t.Fatal("Wait() returned while the initial run was active")
	default:
	}
	runner.endRun()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("Wait() did not return after all admitted runs ended")
	}
}

func TestSpawnRunnerCatalogReturnsValidatedRegistryLookupKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	writeAgent := func(dir, body string) {
		t.Helper()
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "agent.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeAgent("lookup-key", "name: Display Name\ndescription: valid\n")
	writeAgent("name with spaces", "name: Name With Spaces\ndescription: valid\n")
	writeAgent("invalid", "name: invalid\ntools:\n  enabled: [shell]\n  disabled: [read_file]\n")
	writeAgent("malformed", "name: [unterminated\n")

	registry, err := agents.NewRegistry(agents.RegistryConfig{UseBuiltin: false, SearchPaths: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &SpawnAgentRunner{registry: registry}
	names, err := runner.ListSpawnAgentNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "lookup-key" || names[1] != "name with spaces" {
		t.Fatalf("catalog names = %#v, want validated registry lookup keys", names)
	}
	if err := runner.ResolveSpawnAgent("lookup-key"); err != nil {
		t.Fatalf("ResolveSpawnAgent(valid) = %v", err)
	}
	if err := runner.ResolveSpawnAgent("name with spaces"); err != nil {
		t.Fatalf("ResolveSpawnAgent(spaced lookup key) = %v", err)
	}
	if err := runner.ResolveSpawnAgent("Display Name"); err == nil {
		t.Fatal("YAML display name unexpectedly acted as a registry alias")
	}
	if err := runner.ResolveSpawnAgent("invalid"); err == nil || !strings.Contains(err.Error(), "invalid agent") {
		t.Fatalf("ResolveSpawnAgent(invalid) = %v", err)
	}
}

func TestCompleteChildAgentUsesOutputToolAndRunsHookInChildDirectory(t *testing.T) {
	baseDir := t.TempDir()
	outputTool := tools.NewSetOutputTool("set_commit_message", "message", "Set the commit message", nil)
	args, err := json.Marshal(map[string]string{"message": "feat: show isolated skill output"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outputTool.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	engine.RegisterTool(outputTool)
	agent := &agents.Agent{
		OutputTool: agents.OutputToolConfig{Name: "set_commit_message", Param: "message"},
		OnComplete: "cat > child-output.txt",
	}

	output, err := completeChildAgent(agent, runpkg.Result{Engine: engine, Response: "ignored prose"}, "streamed prose", baseDir)
	if err != nil {
		t.Fatalf("completeChildAgent() error = %v", err)
	}
	if output != "feat: show isolated skill output" {
		t.Fatalf("output = %q, want captured output-tool value", output)
	}
	written, err := os.ReadFile(filepath.Join(baseDir, "child-output.txt"))
	if err != nil {
		t.Fatalf("read on_complete output: %v", err)
	}
	if string(written) != output {
		t.Fatalf("on_complete input = %q, want %q", written, output)
	}
}

func TestSpawnRunnerBuildRunRequestInheritsParentBaseDir(t *testing.T) {
	runner := &SpawnAgentRunner{}
	runner.SetBaseDir("  /tmp/parent-worktree  ")

	req := runner.buildRunRequest(context.Background(), "reviewer", "review this", "child-session", 1, false, tools.SpawnAgentRunOptions{})
	if req.Cwd != "/tmp/parent-worktree" {
		t.Fatalf("Cwd = %q, want inherited parent BaseDir", req.Cwd)
	}
}

func TestSpawnRunnerBuildRunRequestUsesCurrentParentContext(t *testing.T) {
	baseDir := "/tmp/first-worktree"
	runner := &SpawnAgentRunner{parentSessionID: "stale-parent"}
	runner.SetBaseDirFunc(func() string { return baseDir })

	baseDir = "/tmp/current-worktree"
	ctx := llm.ContextWithSessionID(context.Background(), "current-parent")
	req := runner.buildRunRequest(ctx, "reviewer", "review this", "child-session", 1, false, tools.SpawnAgentRunOptions{})
	if req.Cwd != "/tmp/current-worktree" {
		t.Fatalf("Cwd = %q, want current parent BaseDir", req.Cwd)
	}
	if req.ParentSessionID != "current-parent" {
		t.Fatalf("ParentSessionID = %q, want current context session", req.ParentSessionID)
	}
}

func TestSpawnRunnerBuildRunRequestFallsBackToConfiguredContext(t *testing.T) {
	runner := &SpawnAgentRunner{parentSessionID: "configured-parent"}
	runner.SetBaseDir("/tmp/configured-worktree")
	runner.SetBaseDirFunc(func() string { return "" })

	req := runner.buildRunRequest(context.Background(), "reviewer", "review this", "child-session", 1, false, tools.SpawnAgentRunOptions{})
	if req.Cwd != "/tmp/configured-worktree" {
		t.Fatalf("Cwd = %q, want configured BaseDir fallback", req.Cwd)
	}
	if req.ParentSessionID != "configured-parent" {
		t.Fatalf("ParentSessionID = %q, want configured parent fallback", req.ParentSessionID)
	}
}

func TestWireSpawnAgentRunnerTracksToolManagerBaseDir(t *testing.T) {
	first := t.TempDir()
	current := t.TempDir()
	cfg := &config.Config{}
	toolMgr, err := tools.NewToolManager(&tools.ToolConfig{Enabled: []string{tools.SpawnAgentToolName}}, cfg)
	if err != nil {
		t.Fatalf("NewToolManager: %v", err)
	}
	if err := toolMgr.SetBaseDir(first); err != nil {
		t.Fatalf("SetBaseDir first: %v", err)
	}
	runner, err := WireSpawnAgentRunnerWithStore(cfg, toolMgr, false, nil, "parent-session")
	if err != nil {
		t.Fatalf("WireSpawnAgentRunnerWithStore: %v", err)
	}
	if err := toolMgr.SetBaseDir(current); err != nil {
		t.Fatalf("SetBaseDir current: %v", err)
	}

	req := runner.buildRunRequest(context.Background(), "reviewer", "review this", "child-session", 1, false, tools.SpawnAgentRunOptions{})
	if req.Cwd != current {
		t.Fatalf("Cwd = %q, want current tool manager BaseDir %q", req.Cwd, current)
	}
}

func TestSpawnRunnerSetupAgentToolsUsesCurrentBaseDir(t *testing.T) {
	first := t.TempDir()
	current := t.TempDir()
	runner := &SpawnAgentRunner{cfg: &config.Config{}}
	runner.SetBaseDir(first)
	runner.SetBaseDirFunc(func() string { return current })
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	agent := &agents.Agent{
		Name:  "parent",
		Tools: agents.ToolsConfig{Enabled: []string{tools.SpawnAgentToolName}},
	}

	toolMgr, err := runner.setupAgentTools(runner.cfg, engine, agent, 0, "child-session")
	if err != nil {
		t.Fatalf("setupAgentTools() error = %v", err)
	}
	if got := toolMgr.BaseDir(); got != current {
		t.Fatalf("BaseDir = %q, want current BaseDir %q", got, current)
	}
}

func TestSpawnRunnerSetupAgentToolsPropagatesAgentModels(t *testing.T) {
	cfg := &config.Config{}
	runner := &SpawnAgentRunner{cfg: cfg}
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	agent := &agents.Agent{
		Name: "parent",
		Tools: agents.ToolsConfig{
			Enabled: []string{tools.SpawnAgentToolName},
		},
		Spawn: agents.SpawnConfig{
			AgentModels: map[string]string{
				"codebase": "fast",
			},
		},
	}

	toolMgr, err := runner.setupAgentTools(cfg, engine, agent, 0, "child-session")
	if err != nil {
		t.Fatalf("setupAgentTools() error = %v", err)
	}
	spawnTool := toolMgr.GetSpawnAgentTool()
	if spawnTool == nil {
		t.Fatal("spawn_agent tool was not enabled")
	}

	capturingRunner := &capturingSpawnRunner{}
	spawnTool.SetRunner(capturingRunner)
	out, err := spawnTool.Execute(context.Background(), json.RawMessage(`{"agent_name":"codebase","prompt":"inspect"}`))
	if err != nil {
		t.Fatalf("spawn_agent Execute() error = %v", err)
	}
	if capturingRunner.lastOptions.ModelOverride != "fast" {
		t.Fatalf("ModelOverride = %q, want fast (output %q)", capturingRunner.lastOptions.ModelOverride, out.Content)
	}
}
