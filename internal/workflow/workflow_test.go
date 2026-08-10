package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type countingExecutor struct {
	mu    sync.Mutex
	calls int
}

func (e *countingExecutor) Execute(context.Context, AgentRequest) (AgentResult, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return AgentResult{Stdout: "unexpected"}, nil
}

type captureExecutor struct {
	request AgentRequest
}

func (e *captureExecutor) Execute(_ context.Context, request AgentRequest) (AgentResult, error) {
	e.request = request
	return AgentResult{Stdout: "ok"}, nil
}

func TestRunAgentBuildsCapabilityScopedDynamicAgent(t *testing.T) {
	root := t.TempDir()
	executor := &captureExecutor{}
	engine := &Engine{Executor: executor}
	result, err := engine.Execute(context.Background(), `workflow { name = "dynamic" }
return await(run_agent {
  label = "tester",
  system = "compile and run the regression test",
  prompt = "work until go test fails for the expected reason",
  provider = "qwen-local",
  tools = { "read_file", "write_file", "shell" },
  read_dirs = { "`+root+`" },
  write_dirs = { "`+root+`" },
  shell_allow = { "go test *", "gofmt *" },
  cwd = "`+root+`",
  max_turns = 12,
})`, ExecuteOptions{
		CWD:          root,
		AllowedTools: []string{"read_file", "write_file", "shell"},
		AllowedRead:  []string{root},
		AllowedWrite: []string{root},
		AllowedShell: []string{"go test *", "gofmt *"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	resultMap, ok := result.(map[string]any)
	if !ok || resultMap["text"] != "ok" {
		t.Fatalf("result = %#v", result)
	}
	request := executor.request
	if request.SystemMessage != "compile and run the regression test" || request.Provider != "qwen-local" || request.MaxTurns != 12 || request.CWD != root {
		t.Fatalf("request metadata = %#v", request)
	}
	if !reflect.DeepEqual(request.Tools, []string{"read_file", "write_file", "shell"}) || !reflect.DeepEqual(request.ShellAllow, []string{"go test *", "gofmt *"}) {
		t.Fatalf("request capabilities = %#v", request)
	}
}

func TestRunAgentRejectsCapabilityEscalation(t *testing.T) {
	root := t.TempDir()
	engine := &Engine{Executor: &captureExecutor{}}
	_, err := engine.Execute(context.Background(), `workflow { name = "denied" }
return await(run_agent { prompt = "escape", tools = { "shell" }, shell_allow = { "rm -rf *" } })`, ExecuteOptions{
		CWD:          root,
		AllowedTools: []string{"shell"},
		AllowedShell: []string{"go test *"},
	})
	if err == nil || !strings.Contains(err.Error(), `shell pattern "rm -rf *" exceeds the workflow capability ceiling`) {
		t.Fatalf("Execute error = %v", err)
	}
}

type selectiveExecutor struct{}

func (selectiveExecutor) Execute(_ context.Context, request AgentRequest) (AgentResult, error) {
	if request.Prompt == "fail" {
		return AgentResult{Stdout: "partial", ExitReason: "agent_error"}, errors.New("deliberate failure")
	}
	return AgentResult{Stdout: request.Prompt, ExitReason: "completed"}, nil
}

func TestParallelSettledPreservesIndependentFailures(t *testing.T) {
	engine := &Engine{Executor: selectiveExecutor{}}
	result, err := engine.Execute(context.Background(), `workflow { name = "settled" }
return parallel_settled {
  agent { prompt = "ok" },
  agent { prompt = "fail" },
}`, ExecuteOptions{CWD: t.TempDir(), Concurrency: 2})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	items, ok := result.([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("result = %#v", result)
	}
	first := items[0].(map[string]any)
	second := items[1].(map[string]any)
	if first["ok"] != true || second["ok"] != false || !strings.Contains(second["error"].(string), "deliberate failure") {
		t.Fatalf("settled outcomes = %#v", items)
	}
}

func TestEmptyParallelCollectionsReturnImmediately(t *testing.T) {
	engine := &Engine{Executor: &countingExecutor{}}
	for _, source := range []string{
		`workflow { name = "empty" }; return parallel {}`,
		`workflow { name = "empty-settled" }; return parallel_settled {}`,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		result, err := engine.Execute(ctx, source, ExecuteOptions{Concurrency: 2})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result, map[string]any{}) && !reflect.DeepEqual(result, []any{}) {
			t.Fatalf("empty parallel result = %#v", result)
		}
	}
}

func TestCapabilityPathResolvesSymlinkedParentForMissingTarget(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if pathWithinAny(filepath.Join(link, "future.txt"), []string{root}) {
		t.Fatal("path through symlinked parent was accepted inside capability root")
	}
}

func TestCreateWorkspaceCopiesWithinCeiling(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	engine := &Engine{Executor: &captureExecutor{}}
	result, err := engine.Execute(context.Background(), `workflow { name = "workspace" }
return create_workspace { source = "`+source+`", root = "`+root+`", name = "specialist" }`, ExecuteOptions{
		CWD:              source,
		AllowedRead:      []string{source},
		AllowedWorkspace: []string{root},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	workspace := result.(string)
	content, err := os.ReadFile(filepath.Join(workspace, "source.txt"))
	if err != nil || string(content) != "content" {
		t.Fatalf("workspace content = %q, %v", content, err)
	}
}

func TestCommandExecutorAcceptsSatisfiedRequirementAfterAgentError(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "fake-term-llm")
	body := `#!/bin/sh
printf '%s\n' '{"type":"text.delta","text":"worked"}'
printf '%s\n' '{"type":"tool.started","call_id":"c1","name":"shell","args":{"command":"test -f generated.txt"}}'
printf '%s\n' '{"type":"tool.completed","call_id":"c1","name":"shell","success":true}'
printf artifact > generated.txt
exit 1
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	executor := CommandExecutor{Executable: script}
	result, err := executor.Execute(context.Background(), AgentRequest{
		Prompt:     "generate",
		CWD:        root,
		Dynamic:    true,
		ShellAllow: []string{"test*"},
		Require: &CommandRequirement{
			Command:      "test -f generated.txt",
			ExitCode:     0,
			Repetitions:  2,
			ArtifactGlob: "generated.txt",
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Stdout != "worked" || result.ExitReason != "requirement_satisfied" || len(result.ToolCalls) != 1 || !result.ToolCalls[0].Success || len(result.Evidence) != 2 || len(result.Artifacts) != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestCommandRequirementRejectsArtifactOutsideWorkingDirectory(t *testing.T) {
	parent := t.TempDir()
	cwd := filepath.Join(parent, "workspace")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := AgentRequest{
		CWD:        cwd,
		ShellAllow: []string{"true"},
		Require: &CommandRequirement{
			Command:      "true",
			ArtifactGlob: "../secret.txt",
		},
	}
	var result AgentResult
	if err := verifyCommandRequirement(context.Background(), request, &result); err == nil || !strings.Contains(err.Error(), "escapes working directory") {
		t.Fatalf("verifyCommandRequirement error = %v", err)
	}
}

func TestParseAgentJSONLRequiresValidTerminalStream(t *testing.T) {
	result, err := parseAgentJSONL("{\"type\":\"text.delta\",\"text\":\"ok\"}\n{\"type\":\"done\"}\n", "")
	if err != nil || result.Stdout != "ok" {
		t.Fatalf("valid stream result = %#v, err = %v", result, err)
	}
	for _, raw := range []string{
		"not-json\n{\"type\":\"done\"}\n",
		"{\"type\":\"text.delta\",\"text\":\"partial\"}\n",
	} {
		if _, err := parseAgentJSONL(raw, ""); err == nil {
			t.Fatalf("parseAgentJSONL accepted %q", raw)
		}
	}
}

func TestAgentTasksRemainLazyUntilAwaited(t *testing.T) {
	executor := &countingExecutor{}
	engine := &Engine{Executor: executor}
	result, err := engine.Execute(context.Background(), `workflow { name = "lazy" }
local unused = agent { prompt = "do not execute" }
return "done"`, ExecuteOptions{Concurrency: 1, CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result != "done" {
		t.Fatalf("result = %#v", result)
	}
	executor.mu.Lock()
	calls := executor.calls
	executor.mu.Unlock()
	if calls != 0 {
		t.Fatalf("executor calls = %d, want 0 for unawaited task", calls)
	}
}

func TestWorkflowLuaSandboxOmitsAmbientCapabilities(t *testing.T) {
	engine := &Engine{Executor: &countingExecutor{}}
	result, err := engine.Execute(context.Background(), `workflow { name = "sandbox" }
return {
  os = os == nil,
  io = io == nil,
  package = package == nil,
  debug = debug == nil,
  dofile = dofile == nil,
  loadfile = loadfile == nil,
  load = load == nil,
  loadstring = loadstring == nil,
  random = math.random == nil,
  table = table.concat({"o", "k"}, "")
}`, ExecuteOptions{CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	values, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T", result)
	}
	for _, key := range []string{"os", "io", "package", "debug", "dofile", "loadfile", "load", "loadstring", "random"} {
		if values[key] != true {
			t.Fatalf("sandbox capability %q remained available: %#v", key, values[key])
		}
	}
	if values["table"] != "ok" {
		t.Fatalf("safe table library unavailable: %#v", values)
	}
}

type blockingExecutor struct{}

func (blockingExecutor) Execute(ctx context.Context, _ AgentRequest) (AgentResult, error) {
	<-ctx.Done()
	return AgentResult{}, ctx.Err()
}

func TestWorkflowContextTimeoutStopsInfiniteLuaLoop(t *testing.T) {
	engine := &Engine{Executor: &countingExecutor{}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := engine.Execute(ctx, `workflow { name = "infinite" }
while true do end`, ExecuteOptions{CWD: t.TempDir()})
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(fmt.Sprint(err), context.DeadlineExceeded.Error()) {
		t.Fatalf("Execute error = %v, want deadline exceeded", err)
	}
}

func TestAgentTimeoutFailsTask(t *testing.T) {
	engine := &Engine{Executor: blockingExecutor{}}
	_, err := engine.Execute(context.Background(), `workflow { name = "timeout" }
return await(agent { prompt = "block" })`, ExecuteOptions{
		CWD:          t.TempDir(),
		AgentTimeout: 10 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(fmt.Sprint(err), context.DeadlineExceeded.Error()) {
		t.Fatalf("Execute error = %v, want deadline exceeded", err)
	}
}
