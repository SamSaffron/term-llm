package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestAgentReviewUncollectedMediaSurvivesLiveContinuation(t *testing.T) {
	for _, mime := range []string{"image/png", "audio/mpeg"} {
		t.Run(mime, func(t *testing.T) {
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "session.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			child := &mediaRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, path: filepath.Join(t.TempDir(), "result"), mediaType: mime, reference: "0123456789abcdef0123456789abcdef"}
			tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2}, 0)
			tool.SetRunner(&mediaPersistentRunner{mediaRunner: child, store: store})
			ctx := core.ContextWithSessionID(context.Background(), "astra-parent")
			spawned := lifecycleResult(t, lifecycleCall(t, tool, ctx, `{"agent_name":"developer","prompt":"draw","wait":0}`))
			<-child.entered
			close(child.release)
			entryAny, _ := processAgentEntries.Load(spawned.AgentID)
			<-entryAny.(*agentEntry).done
			before, _ := store.GetAgentRun(ctx, spawned.AgentID)
			if len(before.Media) != 1 || before.Media[0].MediaType != mime || before.Media[0].Reference != child.reference {
				t.Fatalf("initial media=%v", before.Media)
			}
			out := lifecycleCall(t, &agentControlTool{name: ContinueAgentToolName, spawn: tool}, ctx, `{"agent_id":"`+spawned.AgentID+`","instructions":"finish","wait":5}`)
			if len(out.Media) != 1 || out.Media[0] != before.Media[0] {
				t.Fatalf("continuation lost uncollected media: output=%s media=%v", out.Content, out.Media)
			}
		})
	}
}
