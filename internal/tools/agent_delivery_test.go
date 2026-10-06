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

func TestAgentCompletionCollectionSuppressesWakeAndPreservesMedia(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "session.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	child := &mediaRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, path: filepath.Join(t.TempDir(), "generated.png")}
	tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(&mediaPersistentRunner{mediaRunner: child, store: store})
	wake := make(chan string, 1)
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	ctx = ContextWithQueueAgentOrigin(ctx, QueueAgentOriginContext{Origin: QueueAgentOriginWeb, SessionID: "parent"})
	ctx = ContextWithAgentCompletionWake(ctx, func(parent string) { wake <- parent })
	spawned := lifecycleResult(t, lifecycleCall(t, tool, ctx, `{"agent_name":"developer","prompt":"draw","notify_when_done":true,"wait":0}`))
	<-child.entered
	close(child.release)
	if err := tool.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case parent := <-wake:
		if parent != "parent" {
			t.Fatalf("wrong wake target %q", parent)
		}
	default:
		t.Fatal("completion did not wake parent")
	}
	// Simulate process restart: in-memory media is lost, but the uncollected
	// persisted artifact remains available through wait_agent.
	stored, _ := store.GetAgentRun(ctx, spawned.AgentID)
	if len(stored.Media) != 1 || !stored.CollectedAt.IsZero() {
		t.Fatalf("media not persisted before restart: %+v", stored)
	}
	processAgentEntries.Delete(spawned.AgentID)
	next := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	next.SetRunner(&persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{}, store: store})
	out := lifecycleCall(t, &agentControlTool{name: WaitAgentToolName, spawn: next}, ctx, `{"agent_ids":["`+spawned.AgentID+`"],"max_wait":0}`)
	if len(out.Media) != 1 || out.Media[0].Path() != child.path {
		t.Fatalf("restarted media=%+v", out.Media)
	}
	pending, err := PendingAgentEvents(ctx, store, "parent")
	if err != nil || len(pending) != 0 {
		t.Fatalf("collected completion still pending: %+v %v", pending, err)
	}
}

type mediaPersistentRunner struct {
	*mediaRunner
	store session.AgentRunStore
}

func (r *mediaPersistentRunner) AgentRunStore() session.AgentRunStore { return r.store }

func TestPendingAgentEventsRecoverSameProcessReloadWithoutResumingChild(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "session.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record := session.AgentRun{ID: "reloaded-child", ParentSessionID: "parent", AgentName: "developer", Status: "running", NotifyOrigin: QueueAgentOriginWeb, OwnerInstanceID: processAgentOwner(), UpdatedAt: time.Now()}
	if err := store.PutAgentRun(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	pending, err := PendingAgentEvents(context.Background(), store, "parent")
	if err != nil || len(pending) != 1 || pending[0].Status != "interrupted" {
		t.Fatalf("reloaded events = %+v, %v", pending, err)
	}
	stored, err := store.GetAgentRun(context.Background(), record.ID)
	if err != nil || stored.Status != "running" {
		t.Fatalf("child was auto-resumed: %+v, %v", stored, err)
	}
}

func TestAgentNotifyRequiresTrustedHost(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "session.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(&persistentLifecycleRunner{lifecycleRunner: &lifecycleRunner{}, store: store})
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	for _, origin := range []QueueAgentOriginContext{{}, {Origin: QueueAgentOriginWeb, SessionID: "other"}, {Origin: QueueAgentOriginTelegram, SessionID: "parent"}} {
		requestCtx := ContextWithQueueAgentOrigin(ctx, origin)
		requestCtx = ContextWithAgentCompletionWake(requestCtx, func(string) { t.Fatal("untrusted wake") })
		out := lifecycleCall(t, tool, requestCtx, `{"agent_name":"developer","prompt":"work","notify_when_done":true,"wait":0}`)
		if !out.IsError || !strings.Contains(out.Content, "requires a persistent web parent") {
			t.Fatalf("origin=%+v output=%+v", origin, out)
		}
	}
}

func TestHostLifecycleNoticeCannotResumeChild(t *testing.T) {
	tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(&lifecycleRunner{})
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	ctx = ContextWithAgentRecoveryNotice(ctx)
	out := lifecycleCall(t, &agentControlTool{name: ContinueAgentToolName, spawn: tool}, ctx, `{"agent_id":"interrupted-child"}`)
	if !out.IsError || !strings.Contains(out.Content, "explicit user confirmation") {
		t.Fatalf("host notice could auto-resume: %+v", out)
	}
}
