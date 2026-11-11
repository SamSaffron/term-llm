package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
)

func policyMineFixture(t *testing.T, provider *mockprovider.MockProvider) (MineOptions, session.Store) {
	t.Helper()
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions.db")
	writer, err := session.NewStore(session.Config{Enabled: true, Path: sessionPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	env := testEnv(filepath.Join(dir, "memory.db"), "assistant")
	env.LoadConfig = func() (*config.Config, error) {
		return &config.Config{DefaultProvider: "offline", Providers: map[string]config.ProviderConfig{"offline": {Type: "ollama", Model: "fixture"}}}, nil
	}
	env.ApplyAskProviderOverrides = func(*config.Config) error { return nil }
	env.NewEngine = func(core.Provider, *config.Config) *engine.Engine {
		return engine.NewEngine(provider, engine.NewToolRegistry())
	}
	env.SessionStoreConfig = func(*config.Config) session.Config { return session.Config{Enabled: true, Path: sessionPath} }
	env.ActiveModel = func(*config.Config) string { return "fixture" }
	return MineOptions{Env: env, Limit: 1, BatchSize: 10, ReadBytes: 2048, Promote: "never", PromptMaxTokens: 2000, TaxonomyMaxTokens: 500, ToolMaxTurns: 5, MaxOutputTokens: 1024}, writer
}

func testMemoryPolicyMineRejection(t *testing.T, streamError bool) {
	requireMemoryPolicyShell(t)
	ctx := context.Background()
	guard := filepath.Join(t.TempDir(), "guard")
	script := `#!/bin/sh
read -r payload
case "$payload" in
 *'"mode":"input"'*) printf '%s' "$payload";;
 *'"content":"accepted fact"'*) exit 0;;
 *) exit 1;;
esac
`
	if err := os.WriteFile(guard, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(memoryGuardEnv, guard)
	provider := mockprovider.NewMockProvider("policy").
		AddToolCall("accepted", "memory_create_fragment", map[string]string{"path": "fragments/notes/accepted.md", "content": "accepted fact"}).
		AddToolCall("rejected", "memory_create_fragment", map[string]string{"path": "fragments/notes/rejected.md", "content": "rejected fact"})
	if streamError {
		provider.AddError(errors.New("synthetic provider disconnect"))
	} else {
		provider.AddTextResponse("done")
	}
	opts, writer := policyMineFixture(t, provider)
	sess := &session.Session{ID: "synthetic-policy", Agent: "assistant", Status: session.StatusComplete}
	if err := writer.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []core.Message{core.SystemText("boot context"), core.UserText("durable conversation"), core.AssistantText("confirmed")} {
		if err := writer.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
			t.Fatal(err)
		}
	}
	store, err := opts.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertState(ctx, &memorydb.MiningState{SessionID: sess.ID, Agent: "assistant", LastMinedOffset: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Mine(opts); err == nil {
		t.Fatal("policy-rejected mining returned success")
	}
	if len(provider.RecordedRequests()) != 3 {
		t.Fatalf("model did not complete rejected-tool sequence: %d requests", len(provider.RecordedRequests()))
	}
	state, err := store.GetState(ctx, sess.ID)
	if err != nil || state == nil || state.LastMinedOffset != 1 {
		t.Fatal("rejection advanced or removed cursor")
	}
	accepted, err := store.GetFragment(ctx, "assistant", "fragments/notes/accepted.md")
	if err != nil || accepted == nil {
		t.Fatal("accepted write missing")
	}
	rejected, err := store.GetFragment(ctx, "assistant", "fragments/notes/rejected.md")
	if err != nil || rejected != nil {
		t.Fatal("rejected write persisted")
	}
	sources, err := store.GetFragmentSources(ctx, "assistant", "fragments/notes/accepted.md")
	if err != nil || len(sources) != 1 || sources[0].SessionID != sess.ID {
		t.Fatal("accepted partial write lost source provenance")
	}
}

func TestMemoryPolicyMineSkipsEmbedding(t *testing.T) {
	// No executable is needed: with no eligible sessions the policy should not
	// run, nor should an invalid embedding provider be constructed.
	t.Setenv(memoryGuardEnv, filepath.Join(t.TempDir(), "missing-guard"))
	provider := mockprovider.NewMockProvider("policy")
	opts, _ := policyMineFixture(t, provider)
	store, err := opts.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateFragment(context.Background(), &memorydb.Fragment{Agent: "assistant", Path: "fragments/notes/legacy.md", Content: "synthetic legacy unchecked text"}); err != nil {
		t.Fatal(err)
	}
	opts.Embed, opts.EmbedProvider = true, "invalid-policy-test-provider"
	if err := Mine(opts); err != nil {
		t.Fatalf("guarded embedding was not skipped: %v", err)
	}
	if len(provider.RecordedRequests()) != 0 {
		t.Fatal("unexpected model request")
	}
}

func TestMemoryPolicyMineRejectionPreservesCursorAndAcceptedSources(t *testing.T) {
	for _, streamError := range []bool{false, true} {
		name := "done"
		if streamError {
			name = "provider error"
		}
		t.Run(name, func(t *testing.T) { testMemoryPolicyMineRejection(t, streamError) })
	}
}
