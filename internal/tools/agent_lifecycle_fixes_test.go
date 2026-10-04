package tools

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
)

// mediaRunner emits one media-bearing tool result, then blocks until released.
type mediaRunner struct {
	*lifecycleRunner
	path string
}

func (r *mediaRunner) RunAgentWithCallbackAndOptions(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error) {
	cb(id, SubagentEvent{Type: SubagentEventToolEnd, ToolName: "image_generate", Media: []llm.MediaArtifact{{StoredPath: r.path, MediaType: "image/png"}}})
	return r.lifecycleRunner.RunAgentWithCallbackAndOptions(ctx, name, prompt, depth, id, cb, opts)
}

func (r *mediaRunner) RunAgentWithCallback(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback) (SpawnAgentRunResult, error) {
	return r.RunAgentWithCallbackAndOptions(ctx, name, prompt, depth, id, cb, SpawnAgentRunOptions{})
}

func TestWaitAgentDeliversDetachedChildMedia(t *testing.T) {
	runner := &mediaRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, path: "/tmp/detached-child.png"}
	spawn := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2, DefaultTimeout: 300}, 0)
	spawn.SetRunner(runner)
	ctx := llm.ContextWithSessionID(context.Background(), "media-parent")
	spawned := lifecycleResult(t, lifecycleCall(t, spawn, ctx, `{"agent_name":"artist","prompt":"draw","wait":0}`))
	<-runner.entered
	close(runner.release)
	out := lifecycleCall(t, &agentControlTool{name: WaitAgentToolName, spawn: spawn}, ctx, `{"agent_ids":["`+spawned.AgentID+`"],"max_wait":5}`)
	if !strings.Contains(out.Content, `"status":"completed"`) {
		t.Fatalf("wait = %s", out.Content)
	}
	if len(out.Media) != 1 || out.Media[0].Path() != runner.path {
		t.Fatalf("wait_agent media = %+v, want detached child's image", out.Media)
	}
}

func TestSpawnAgentCompletedWithinBudgetIsCollectedAndReleased(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	child := &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}
	close(child.release)
	spawn := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2, DefaultTimeout: 300}, 0)
	spawn.SetRunner(&persistentLifecycleRunner{lifecycleRunner: child, store: store})
	ctx := llm.ContextWithSessionID(context.Background(), "collect-parent")
	result := lifecycleResult(t, lifecycleCall(t, spawn, ctx, `{"agent_name":"developer","prompt":"work","wait":5}`))
	if result.Status != "completed" || result.Output != "done" {
		t.Fatalf("spawn = %+v", result)
	}
	got, err := store.GetAgentRun(ctx, result.AgentID)
	if err != nil || got.CollectedAt.IsZero() {
		t.Fatalf("persisted run = %+v, %v; want collected", got, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, live := processAgentEntries.Load(result.AgentID); !live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("collected run is still retained in the process registry")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInterruptAgentsForParentStopsOnlyThatParentsChildren(t *testing.T) {
	runner := &lifecycleRunner{entered: make(chan string, 2), release: make(chan struct{})}
	defer close(runner.release)
	spawn := NewSpawnAgentTool(SpawnConfig{MaxParallel: 2, MaxDepth: 2, DefaultTimeout: 300}, 0)
	spawn.SetRunner(runner)
	stopped := llm.ContextWithSessionID(context.Background(), "stopped-parent")
	other := llm.ContextWithSessionID(context.Background(), "other-parent")
	victim := lifecycleResult(t, lifecycleCall(t, spawn, stopped, `{"agent_name":"developer","prompt":"a","wait":0}`))
	survivor := lifecycleResult(t, lifecycleCall(t, spawn, other, `{"agent_name":"developer","prompt":"b","wait":0}`))
	<-runner.entered
	<-runner.entered

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ids := InterruptAgentsForParent(ctx, "stopped-parent")
	if len(ids) != 1 || ids[0] != victim.AgentID {
		t.Fatalf("interrupted = %v, want only %s", ids, victim.AgentID)
	}
	wait := &agentControlTool{name: WaitAgentToolName, spawn: spawn}
	snapshot := lifecycleResult(t, llm.ToolOutput{Content: strings.Trim(lifecycleCall(t, wait, stopped, `{"agent_ids":["`+victim.AgentID+`"]}`).Content, "[]")})
	if snapshot.Status != "interrupted" || !snapshot.Resumable || !strings.Contains(snapshot.Next, "continue_agent") {
		t.Fatalf("stopped child = %+v, want resumable interrupted", snapshot)
	}
	alive := lifecycleCall(t, wait, other, `{"agent_ids":["`+survivor.AgentID+`"]}`)
	if !strings.Contains(alive.Content, `"status":"running"`) {
		t.Fatalf("other parent's child = %s, want running", alive.Content)
	}
	_ = spawn.Shutdown(context.Background())
}

func TestAgentControlFailuresAreToolErrors(t *testing.T) {
	spawn := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	spawn.SetRunner(&lifecycleRunner{})
	ctx := llm.ContextWithSessionID(context.Background(), "error-parent")
	for name, args := range map[string]string{
		WaitAgentToolName:     `{"agent_ids":["missing"]}`,
		CancelAgentToolName:   `{"agent_id":"missing"}`,
		ContinueAgentToolName: `{"agent_id":"missing"}`,
	} {
		out := lifecycleCall(t, &agentControlTool{name: name, spawn: spawn}, ctx, args)
		if !out.IsError || !strings.Contains(out.Content, "not found") {
			t.Fatalf("%s missing agent = %+v, want tool error", name, out)
		}
	}
}

// reloadAwareRunner admits its work like cmdRunner.Run does.
type reloadAwareRunner struct {
	*lifecycleRunner
	coordinator *restart.Coordinator
	gate        chan struct{}
	admitErr    chan error
}

func (r *reloadAwareRunner) RunAgentWithCallbackAndOptions(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error) {
	<-r.gate
	ctx, release, err := r.coordinator.Activity(ctx)
	r.admitErr <- err
	if err != nil {
		return SpawnAgentRunResult{}, err
	}
	defer release()
	return r.lifecycleRunner.RunAgentWithCallbackAndOptions(ctx, name, prompt, depth, id, cb, opts)
}

func (r *reloadAwareRunner) RunAgentWithCallback(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback) (SpawnAgentRunResult, error) {
	return r.RunAgentWithCallbackAndOptions(ctx, name, prompt, depth, id, cb, SpawnAgentRunOptions{})
}

func TestBackgroundSpawnSurvivesReleaseOfSpawningCallAndReloadInterruptsIt(t *testing.T) {
	c := &restart.Coordinator{InterruptAfter: time.Millisecond}
	runner := &reloadAwareRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, coordinator: c, gate: make(chan struct{}), admitErr: make(chan error, 1)}
	defer close(runner.release)
	spawn := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2, DefaultTimeout: 300}, 0)
	spawn.SetRunner(runner)
	callCtx, releaseCall, err := c.Root(llm.ContextWithSessionID(context.Background(), "reload-parent"))
	if err != nil {
		t.Fatal(err)
	}
	spawned := lifecycleResult(t, lifecycleCall(t, spawn, callCtx, `{"agent_name":"developer","prompt":"work","wait":0}`))
	releaseCall() // the tool call's operation ends before the child is admitted
	close(runner.gate)
	if err := <-runner.admitErr; err != nil {
		t.Fatalf("background child rejected: %v", err)
	}
	<-runner.entered

	stop, err := c.Bind(context.Background(), func(context.Context) error { return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	wait := &agentControlTool{name: WaitAgentToolName, spawn: spawn}
	out := lifecycleCall(t, wait, llm.ContextWithSessionID(context.Background(), "reload-parent"), `{"agent_ids":["`+spawned.AgentID+`"],"max_wait":5}`)
	if !strings.Contains(out.Content, `"status":"interrupted"`) || !strings.Contains(out.Content, `"resumable":true`) {
		t.Fatalf("child after reload = %s, want resumable interrupted", out.Content)
	}
}
