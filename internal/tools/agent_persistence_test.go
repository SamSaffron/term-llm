package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type persistentLifecycleRunner struct {
	*lifecycleRunner
	store session.AgentRunStore
}

func (r *persistentLifecycleRunner) AgentRunStore() session.AgentRunStore { return r.store }

func TestAgentLifecycleQueuedShutdownRestartsFreshAfterReload(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := &persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, store: store}
	firstTool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	firstTool.SetRunner(runner)
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	first := lifecycleResult(t, lifecycleCall(t, firstTool, ctx, `{"agent_name":"developer","prompt":"blocking","wait":0}`))
	<-runner.entered
	wait := &agentControlTool{name: WaitAgentToolName, spawn: firstTool}
	lifecycleCall(t, wait, ctx, `{"agent_ids":["`+first.AgentID+`"],"max_wait":0}`)
	before, err := store.GetAgentRun(ctx, first.AgentID)
	if err != nil || !before.CollectedAt.IsZero() {
		t.Fatalf("running agent collected: %+v, %v", before, err)
	}
	queued := lifecycleResult(t, lifecycleCall(t, firstTool, ctx, `{"agent_name":"developer","prompt":"queued","model":"test:model","wait":0}`))
	if queued.Status != "queued" {
		t.Fatalf("queued = %+v", queued)
	}
	if err := firstTool.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	lifecycleCall(t, wait, ctx, `{"agent_ids":["`+first.AgentID+`"],"max_wait":0}`)
	after, err := store.GetAgentRun(ctx, first.AgentID)
	if err != nil || after.CollectedAt.IsZero() {
		t.Fatalf("terminal agent not collected: %+v, %v", after, err)
	}
	stored, err := store.GetAgentRun(ctx, queued.AgentID)
	if err != nil || stored.Status != "interrupted" || stored.Started || stored.Model != "test:model" {
		t.Fatalf("queued admission = %+v, %v", stored, err)
	}
	// Simulate a process restart: the new manager only has persisted admission.
	processAgentEntries.Delete(first.AgentID)
	processAgentEntries.Delete(queued.AgentID)
	fresh := &persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, store: store}
	replacement := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	replacement.SetRunner(fresh)
	continued := lifecycleResult(t, lifecycleCall(t, &agentControlTool{name: ContinueAgentToolName, spawn: replacement}, ctx, `{"agent_id":"`+queued.AgentID+`","wait":0}`))
	if continued.AgentID != queued.AgentID || continued.Status == "failed" {
		t.Fatalf("queued continuation = %+v", continued)
	}
	select {
	case id := <-fresh.entered:
		if id != queued.AgentID {
			t.Fatalf("new child id = %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("queued child never started")
	}
	close(fresh.release)
	if err := replacement.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	if fresh.calls != 1 || fresh.resumed != 0 {
		t.Fatalf("queued child resumed without a session: calls=%d resumed=%d", fresh.calls, fresh.resumed)
	}
}

func TestAgentLifecycleReloadListInterruptedAndResume(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := &persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, store: store}
	firstTool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2, DefaultTimeout: 300}, 0)
	firstTool.SetRunner(runner)
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	first := lifecycleResult(t, lifecycleCall(t, firstTool, ctx, `{"agent_name":"developer","prompt":"work","wait":0}`))
	<-runner.entered
	wait := &agentControlTool{name: WaitAgentToolName, spawn: firstTool}
	lifecycleCall(t, wait, ctx, `{"agent_ids":["`+first.AgentID+`"],"max_wait":0}`)
	before, err := store.GetAgentRun(ctx, first.AgentID)
	if err != nil || !before.CollectedAt.IsZero() {
		t.Fatalf("running agent collected: %+v, %v", before, err)
	}
	if err := firstTool.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	replacement := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2, DefaultTimeout: 300}, 0)
	replacement.SetRunner(&persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, store: store})
	listed := lifecycleCall(t, &agentControlTool{name: ListAgentsToolName, spawn: replacement}, ctx, `{}`)
	if !strings.Contains(listed.Content, `"status":"interrupted"`) || !strings.Contains(listed.Content, first.AgentID) {
		t.Fatalf("reloaded agents = %s", listed.Content)
	}
	continued := lifecycleResult(t, lifecycleCall(t, &agentControlTool{name: ContinueAgentToolName, spawn: replacement}, ctx, `{"agent_id":"`+first.AgentID+`","wait":1}`))
	if continued.Status != "completed" || continued.AgentID != first.AgentID {
		t.Fatalf("resumed record = %+v", continued)
	}
}
