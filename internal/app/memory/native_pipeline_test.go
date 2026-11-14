package memory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	memorydb "github.com/term-llm/term-llm/internal/memory"
	"github.com/term-llm/term-llm/internal/session"
)

func nativeMineFixture(t *testing.T, provider *mockprovider.MockProvider) (MineOptions, session.Store) {
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

func TestNativeMineRejectionPreservesCursorAndAcceptedSources(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "done", true: "provider failure"}[disconnect], func(t *testing.T) {
			ctx := context.Background()
			provider := mockprovider.NewMockProvider("native").AddToolCall("good", "memory_create_fragment", map[string]string{"path": "fragments/notes/good.md", "content": "durable safe fact"}).AddToolCall("bad", "memory_create_fragment", map[string]string{"path": "fragments/notes/bad.md", "content": "api_key: " + nativeSyntheticSecret})
			if disconnect {
				provider.AddError(errors.New(nativeSyntheticSecret))
			} else {
				provider.AddTextResponse("done")
			}
			opts, writer := nativeMineFixture(t, provider)
			sess := &session.Session{ID: "native-fixture", Agent: "assistant", Status: session.StatusComplete}
			if err := writer.Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			for _, msg := range []core.Message{core.SystemText("privileged context"), core.UserText("durable fact"), core.AssistantText("confirmed")} {
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
			if err := Mine(opts); !errors.Is(err, errMemorySafety) || strings.Contains(err.Error(), nativeSyntheticSecret) {
				t.Fatal("native rejection masked or disclosed")
			}
			state, err := store.GetState(ctx, sess.ID)
			if err != nil || state.LastMinedOffset != 1 {
				t.Fatal("rejection advanced cursor")
			}
			sources, err := store.GetFragmentSources(ctx, "assistant", "fragments/notes/good.md")
			if err != nil || len(sources) != 1 || sources[0].SessionID != sess.ID {
				t.Fatal("accepted partial write lost provenance")
			}
			bad, err := store.GetFragment(ctx, "assistant", "fragments/notes/bad.md")
			if err != nil || bad != nil {
				t.Fatal("rejected write persisted")
			}
		})
	}
}

func TestNativeInvalidPolicyFailsBeforeProviderConstruction(t *testing.T) {
	for _, pattern := range []string{"private-literal[", "a*", ""} {
		opts, _ := nativeMineFixture(t, mockprovider.NewMockProvider("native"))
		load := opts.LoadConfig
		opts.LoadConfig = func() (*config.Config, error) {
			cfg, err := load()
			cfg.Memory.Secrets.Patterns = []string{pattern}
			return cfg, err
		}
		opts.ApplyAskProviderOverrides = func(*config.Config) error { t.Fatal("invalid policy reached provider configuration"); return nil }
		if err := Mine(opts); err == nil || (pattern != "" && strings.Contains(err.Error(), pattern)) {
			t.Fatal("invalid policy was accepted or leaked")
		}
	}
	p, err := newMemoryProtection(config.MemoryConfig{Secrets: config.MemorySecretsConfig{Patterns: []string{`private-local-[0-9]+`}}})
	if err != nil || p.sanitize("private-local-123456") == "private-local-123456" || p.check("private-local-123456") == nil {
		t.Fatal("declarative detector ineffective")
	}
}

func TestNativeEmbeddingSanitizesWithoutDisablingFeature(t *testing.T) {
	requests := make(chan []string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- body.Input
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{{1, 0}}, "prompt_eval_count": 1})
	}))
	defer server.Close()
	opts, _ := nativeMineFixture(t, mockprovider.NewMockProvider("native"))
	cfg := &config.Config{Memory: config.MemoryConfig{Secrets: config.MemorySecretsConfig{Patterns: []string{`private-local-[0-9]+`}}, ExcludePaths: []string{"fragments/credentials/**"}}, Embed: config.EmbedConfig{Provider: "ollama:fixture", Ollama: config.EmbedOllamaConfig{BaseURL: server.URL}}}
	store, err := opts.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, path := range []string{"fragments/notes/legacy.md", "fragments/credentials/excluded.md"} {
		if err := store.CreateFragment(context.Background(), &memorydb.Fragment{Agent: "assistant", Path: path, Content: "api_key: " + nativeSyntheticSecret + "\nprivate-local-123456"}); err != nil {
			t.Fatal(err)
		}
	}
	count, err := opts.runMemoryEmbedPhase(context.Background(), cfg, store)
	if err != nil || count != 1 {
		t.Fatalf("embedding disabled or failed: %v", err)
	}
	input := <-requests
	if len(input) != 1 || strings.Contains(input[0], nativeSyntheticSecret) || strings.Contains(input[0], "private-local-123456") {
		t.Fatal("embedding provider received detected or excluded content")
	}
}

func TestNativeImagesRedactAndStillMineSafeImages(t *testing.T) {
	for _, secret := range []bool{false, true} {
		t.Run(map[bool]string{false: "safe", true: "secret"}[secret], func(t *testing.T) {
			opts, _ := nativeMineFixture(t, mockprovider.NewMockProvider("native"))
			ctx := context.Background()
			store, err := opts.OpenStore()
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			prompt := "A safe landscape"
			if secret {
				prompt = "A landscape " + nativeSyntheticSecret
			}
			if err := store.RecordImage(ctx, &memorydb.ImageRecord{Agent: "assistant", Prompt: prompt, OutputPath: "/synthetic/image.png", Provider: "fixture"}); err != nil {
				t.Fatal(err)
			}
			count, err := opts.Env.mineGeneratedImages(ctx, store)
			meta, _ := store.GetMeta(ctx, opts.Env.memoryImageMineMetaKey("assistant"))
			if err != nil || count != 1 || meta == "" {
				t.Fatal("image did not make safe progress")
			}
			fragments, _ := store.ListFragments(ctx, memorydb.ListOptions{Agent: "assistant"})
			if len(fragments) != 1 || strings.Contains(fragments[0].Content, nativeSyntheticSecret) {
				t.Fatal("image source not redacted")
			}

		})
	}
}

func TestNativeFragmentRawAndStoredFormsBothChecked(t *testing.T) {
	for _, update := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "update"}[update], func(t *testing.T) {
			provider := mockprovider.NewMockProvider("native")
			name := "memory_create_fragment"
			if update {
				name = "memory_update_fragment"
			}
			provider.AddToolCall("write", name, map[string]string{"path": "fragments/notes/padded.md", "content": " sensitive-value "}).AddTextResponse("done")
			opts, writer := nativeMineFixture(t, provider)
			load := opts.LoadConfig
			opts.LoadConfig = func() (*config.Config, error) {
				cfg, err := load()
				cfg.Memory.Secrets.Patterns = []string{`^ sensitive-value $`}
				return cfg, err
			}
			ctx := context.Background()
			sess := &session.Session{ID: "padded-fragment", Agent: "assistant", Status: session.StatusComplete}
			if err := writer.Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			for _, msg := range []core.Message{core.SystemText("boot"), core.UserText("durable fact"), core.AssistantText("confirmed")} {
				if err := writer.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
					t.Fatal(err)
				}
			}
			store, err := opts.OpenStore()
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if update {
				if err := store.CreateFragment(ctx, &memorydb.Fragment{Agent: "assistant", Path: "fragments/notes/padded.md", Content: "known good"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.UpsertState(ctx, &memorydb.MiningState{SessionID: sess.ID, Agent: "assistant", LastMinedOffset: 1}); err != nil {
				t.Fatal(err)
			}
			if err := Mine(opts); !errors.Is(err, errMemorySafety) {
				t.Fatal("raw model field normalized before validation")
			}
			state, _ := store.GetState(ctx, sess.ID)
			frag, _ := store.GetFragment(ctx, "assistant", "fragments/notes/padded.md")
			if state.LastMinedOffset != 1 {
				t.Fatal("raw-field rejection advanced cursor")
			}
			if (!update && frag != nil) || (update && (frag == nil || frag.Content != "known good")) {
				t.Fatal("raw-field rejection changed fragment")
			}
		})
	}
}
