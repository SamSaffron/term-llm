package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type persistentLifecycleRunner struct {
	*lifecycleRunner
	store session.AgentRunStore
}

func (r *persistentLifecycleRunner) AgentRunStore() session.AgentRunStore { return r.store }

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
