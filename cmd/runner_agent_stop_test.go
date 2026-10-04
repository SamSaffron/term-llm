package cmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// stopChildRunner blocks every child until its context ends or it is released.
type stopChildRunner struct {
	entered chan struct{}
	release chan struct{}
}

func (r *stopChildRunner) RunAgent(ctx context.Context, name, prompt string, depth int) (tools.SpawnAgentRunResult, error) {
	return r.RunAgentWithCallback(ctx, name, prompt, depth, "", nil)
}

func (r *stopChildRunner) RunAgentWithCallback(ctx context.Context, _, _ string, _ int, _ string, _ tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	r.entered <- struct{}{}
	select {
	case <-r.release:
		return tools.SpawnAgentRunResult{Output: "done"}, nil
	case <-ctx.Done():
		return tools.SpawnAgentRunResult{}, ctx.Err()
	}
}

type persistedStopChildRunner struct {
	*stopChildRunner
	store session.AgentRunStore
}

func (r *persistedStopChildRunner) AgentRunStore() session.AgentRunStore { return r.store }

// stopScriptProvider spawns a background child, then either finishes the turn
// or blocks in a second, waited spawn so the test can stop the turn mid-tool.
type stopScriptProvider struct {
	*llm.MockProvider
	turn      int
	blockTurn bool
}

func (p *stopScriptProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.turn++
	switch {
	case p.turn == 1:
		p.AddToolCall("bg", tools.SpawnAgentToolName, map[string]any{"agent_name": "developer", "prompt": "background", "wait": 0})
	case p.turn == 2 && p.blockTurn:
		p.AddToolCall("fg", tools.SpawnAgentToolName, map[string]any{"agent_name": "developer", "prompt": "foreground", "wait": 600})
	default:
		p.AddTextResponse("turn finished")
	}
	return p.MockProvider.Stream(ctx, req)
}

func runStopScenario(t *testing.T, stop bool) (string, *session.SQLiteStore, *stopChildRunner) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "sessions.db")
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}, Sessions: config.SessionsConfig{Enabled: true, Path: path}}
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	parentID := "stop-parent"
	if !stop {
		parentID = "finish-parent"
	}
	provider := &stopScriptProvider{MockProvider: llm.NewMockProvider("mock"), blockTurn: stop}
	runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store, Tools: "spawn_agent", ToolsSet: true, AgentOwner: &agentHostOwner{}}).(*cmdRunner)
	baseCtx := llm.ContextWithSessionID(context.Background(), parentID)
	env, err := runner.prepare(baseCtx, runpkg.Request{Platform: runpkg.PlatformConsole, SessionID: parentID, ProviderInstance: provider, Cwd: t.TempDir(), DeferSession: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	child := &stopChildRunner{entered: make(chan struct{}, 2), release: make(chan struct{})}
	t.Cleanup(func() {
		close(child.release)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = env.runtime.spawnRunner.Shutdown(ctx)
	})
	env.runtime.toolMgr.GetSpawnAgentTool().SetRunner(&persistedStopChildRunner{stopChildRunner: child, store: store})

	turnCtx, stopTurn := context.WithCancel(baseCtx)
	defer stopTurn()
	if stop {
		go func() {
			<-child.entered // background child
			<-child.entered // foreground child: the parent is now blocked in spawn_agent
			stopTurn()
		}()
	}
	req := env.llmReq
	req.SessionID = parentID
	req.MaxTurns = 5
	onEvent := func(ev llm.Event) error {
		// A client that disconnects as soon as the turn's final event arrives
		// cancels the request context after the turn completed normally.
		if !stop && ev.Type == llm.EventDone {
			stopTurn()
		}
		return nil
	}
	_, runErr := env.runtime.RunWithEvents(turnCtx, false, false, []llm.Message{llm.UserText("delegate")}, req, onEvent)
	if stop && runErr == nil {
		t.Fatal("stopped turn returned no error")
	}
	if !stop && runErr != nil {
		t.Fatalf("finished turn: %v", runErr)
	}
	return parentID, store, child
}

func agentRunsByPrompt(t *testing.T, store *session.SQLiteStore, parent string) map[string]session.AgentRun {
	t.Helper()
	runs, err := store.ListAgentRuns(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	byPrompt := make(map[string]session.AgentRun, len(runs))
	for _, run := range runs {
		byPrompt[run.Prompt] = run
	}
	return byPrompt
}

func TestStoppedParentTurnInterruptsDetachedChildren(t *testing.T) {
	parent, store, _ := runStopScenario(t, true)
	runs := agentRunsByPrompt(t, store, parent)
	for _, prompt := range []string{"background", "foreground"} {
		run, ok := runs[prompt]
		if !ok {
			t.Fatalf("no %s run in %+v", prompt, runs)
		}
		if run.Status != "interrupted" {
			t.Fatalf("%s child after stop = %q, want interrupted (resumable)", prompt, run.Status)
		}
	}
}

func TestFinishedParentTurnLeavesDetachedChildRunning(t *testing.T) {
	parent, store, child := runStopScenario(t, false)
	select {
	case <-child.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("background child never started")
	}
	run, ok := agentRunsByPrompt(t, store, parent)["background"]
	if !ok || run.Status != "running" {
		data, _ := json.Marshal(run)
		t.Fatalf("background child after a normal turn = %s, want running", strings.TrimSpace(string(data)))
	}
}
