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

type persistedBlockingChildRunner struct {
	*blockingChildRunner
	store session.AgentRunStore
}

func (r *persistedBlockingChildRunner) AgentRunStore() session.AgentRunStore { return r.store }

// Each parent turn builds a fresh cmdRunner and session handle. The child may
// complete after the first turn returns, before wait_agent is called on turn 2.
func TestFreshCmdRunnerWaitsForDetachedResultOnNextTurn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "sessions.db")
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}, Sessions: config.SessionsConfig{Enabled: true, Path: path}}
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	firstStore, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	owner := &agentHostOwner{}
	prepare := func(store session.Store, own *agentHostOwner) *cmdRunEnvironment {
		t.Helper()
		runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store, Tools: "spawn_agent", ToolsSet: true, AgentOwner: own}).(*cmdRunner)
		env, err := runner.prepare(ctx, runpkg.Request{Platform: runpkg.PlatformConsole, SessionID: "parent", ProviderInstance: llm.NewMockProvider("mock"), Cwd: t.TempDir(), DeferSession: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return env
	}
	first := prepare(firstStore, owner)
	child := &blockingChildRunner{entered: make(chan struct{}), release: make(chan struct{})}
	first.runtime.toolMgr.GetSpawnAgentTool().SetRunner(&persistedBlockingChildRunner{blockingChildRunner: child, store: firstStore})
	spawn, err := first.runtime.toolMgr.GetSpawnAgentTool().Execute(ctx, json.RawMessage(`{"agent_name":"developer","prompt":"work","wait":0}`))
	if err != nil {
		t.Fatal(err)
	}
	var result tools.SpawnAgentResult
	if err := json.Unmarshal([]byte(spawn.Content), &result); err != nil || result.AgentID == "" || result.Status != "running" && result.Status != "queued" {
		t.Fatalf("first turn = %s, %v", spawn.Content, err)
	}
	<-child.entered
	first.Close() // host handoff must not block the first turn
	close(child.release)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := owner.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}
	secondStore, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	second := prepare(secondStore, nil)
	defer second.Close()
	wait, ok := second.runtime.toolMgr.Registry.Get(tools.WaitAgentToolName)
	if !ok {
		t.Fatal("wait_agent missing on fresh runner")
	}
	output, err := wait.Execute(ctx, json.RawMessage(`{"agent_ids":["`+result.AgentID+`"],"max_wait":0}`))
	if err != nil || !strings.Contains(output.Content, `"status":"completed"`) || !strings.Contains(output.Content, `"output":"done"`) {
		t.Fatalf("second turn result = %s, %v", output.Content, err)
	}
	record, err := secondStore.GetAgentRun(ctx, result.AgentID)
	if err != nil || record.CollectedAt.IsZero() {
		t.Fatalf("second turn did not collect result: %+v, %v", record, err)
	}
}
