package tools

import (
	"context"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"path/filepath"
	"testing"
)

func TestAgentReviewUncollectedMediaSurvivesLiveContinuation(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "session.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	child := &mediaRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, path: filepath.Join(t.TempDir(), "result.png")}
	tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
	tool.SetRunner(&mediaPersistentRunner{mediaRunner: child, store: store})
	ctx := llm.ContextWithSessionID(context.Background(), "astra-parent")
	spawned := lifecycleResult(t, lifecycleCall(t, tool, ctx, `{"agent_name":"developer","prompt":"draw","wait":0}`))
	<-child.entered
	close(child.release)
	entryAny, _ := processAgentEntries.Load(spawned.AgentID)
	<-entryAny.(*agentEntry).done
	before, _ := store.GetAgentRun(ctx, spawned.AgentID)
	if len(before.Media) != 1 {
		t.Fatalf("initial media=%v", before.Media)
	}
	out := lifecycleCall(t, &agentControlTool{name: ContinueAgentToolName, spawn: tool}, ctx, `{"agent_id":"`+spawned.AgentID+`","instructions":"finish","wait":5}`)
	if len(out.Media) != 1 {
		t.Fatalf("continuation lost uncollected media: output=%s media=%v", out.Content, out.Media)
	}
}
