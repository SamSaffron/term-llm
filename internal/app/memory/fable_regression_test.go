package memory

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	memorydb "github.com/term-llm/term-llm/internal/memory"
	"github.com/term-llm/term-llm/internal/session"
)

func TestFableCorrectedWriteCanCompleteBatch(t *testing.T) {
	provider := mockprovider.NewMockProvider("fable-regression").AddToolCall("bad", "memory_create_fragment", map[string]string{"path": "fragments/retry.md", "content": nativeSyntheticSecret}).AddToolCall("good", "memory_create_fragment", map[string]string{"path": "fragments/retry.md", "content": "Safe corrected fact"}).AddTextResponse("done")
	opts, writer := nativeMineFixture(t, provider)
	ctx := context.Background()
	sess := &session.Session{ID: "corrected", Agent: "assistant", Status: session.StatusComplete}
	if err := writer.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []core.Message{core.UserText("A durable fact"), core.AssistantText("Confirmed")} {
		if err := writer.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := Mine(opts); err != nil {
		t.Fatalf("corrected retry still failed: %v", err)
	}
	store, err := opts.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, _ := store.GetState(ctx, sess.ID)
	if state == nil || state.LastMinedOffset != 2 {
		t.Fatal("corrected batch did not advance")
	}
}

func TestFableOrdinaryToolValidationIsNotSecretRejection(t *testing.T) {
	for _, args := range []string{`{"path":"../bad.md","content":"safe"}`, `{"path":"fragments/test.md","content":""}`, `{"path":"fragments/test.md","content":"` + strings.Repeat("x", 8193) + `"}`} {
		_, err := (&memoryCreateFragmentTool{collector: newMemoryExtractionCollector()}).Execute(context.Background(), json.RawMessage(args))
		if err == nil || errors.Is(err, errMemorySafety) {
			t.Fatal("ordinary model validation classified as terminal secret rejection")
		}
	}
}

func TestFableCustomMatchPreservesOtherRecentFacts(t *testing.T) {
	p, err := newMemoryProtection(config.MemoryConfig{Secrets: config.MemorySecretsConfig{Patterns: []string{`private-local-[0-9]+`}}})
	if err != nil {
		t.Fatal(err)
	}
	prior := "# Projects\n\nUnrelated retained fact.\n\n# Credentials\n\nprivate-local-123456\n\n# Preferences\n\nAnother retained fact."
	prompt := p.promotePrompt(nil, prior)
	if !strings.Contains(prompt, "Unrelated retained fact") || !strings.Contains(prompt, "Another retained fact") || strings.Contains(prompt, "private-local-123456") {
		t.Fatal("one custom match erased unrelated prior summary")
	}
}

func TestFableImageInputIsRedactedAndConsumed(t *testing.T) {
	opts, _ := nativeMineFixture(t, mockprovider.NewMockProvider("fable-regression"))
	ctx := context.Background()
	store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RecordImage(ctx, &memorydb.ImageRecord{Agent: "assistant", Prompt: "A landscape " + nativeSyntheticSecret, OutputPath: "/synthetic/image.png", Provider: "fixture"}); err != nil {
		t.Fatal(err)
	}
	count, err := opts.Env.mineGeneratedImages(ctx, store)
	if err != nil || count != 1 {
		t.Fatalf("image detector permanently blocks progress: %v", err)
	}
	meta, _ := store.GetMeta(ctx, opts.Env.memoryImageMineMetaKey("assistant"))
	if meta == "" {
		t.Fatal("processed redacted image not checkpointed")
	}
	fragments, _ := store.ListFragments(ctx, memorydb.ListOptions{Agent: "assistant"})
	if len(fragments) != 1 || strings.Contains(fragments[0].Content, nativeSyntheticSecret) {
		t.Fatal("image was not safely projected")
	}
}

func TestFableUnresolvedSessionDoesNotBlockOtherSessionsOrImages(t *testing.T) {
	provider := mockprovider.NewMockProvider("isolation").AddToolCall("bad", "memory_create_fragment", map[string]string{"path": "fragments/bad.md", "content": nativeSyntheticSecret}).AddTextResponse("done").AddToolCall("good", "memory_create_fragment", map[string]string{"path": "fragments/good.md", "content": "Later safe fact"}).AddTextResponse("done")
	opts, writer := nativeMineFixture(t, provider)
	opts.Limit = 0
	ctx := context.Background()
	// Session number descending means bad (created second) is visited first.
	for _, id := range []string{"good-session", "bad-session"} {
		sess := &session.Session{ID: id, Agent: "assistant", Status: session.StatusComplete}
		if err := writer.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		for _, msg := range []core.Message{core.UserText("Durable evidence"), core.AssistantText("Confirmed")} {
			if err := writer.AddMessage(ctx, id, session.NewMessage(id, msg, -1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	store, err := opts.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RecordImage(ctx, &memorydb.ImageRecord{Agent: "assistant", Prompt: "An unrelated safe image", OutputPath: "/synthetic/image.png", Provider: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := Mine(opts); err == nil {
		t.Fatal("unresolved rejection was silently reported as complete")
	}
	bad, _ := store.GetState(ctx, "bad-session")
	good, _ := store.GetState(ctx, "good-session")
	imageMark, _ := store.GetMeta(ctx, opts.Env.memoryImageMineMetaKey("assistant"))
	if bad != nil || good == nil || good.LastMinedOffset != 2 || imageMark == "" {
		t.Fatal("failed evidence was skipped or unrelated work starved")
	}
}

func TestFableRejectedCountAndSamePathCorrection(t *testing.T) {
	ctx := context.Background()
	store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c := newMemoryExtractionCollector()
	tool := &memoryCreateFragmentTool{store: store, agent: "assistant", collector: c, protection: defaultProtection}
	bad, _ := json.Marshal(map[string]string{"path": "fragments/retry.md", "content": nativeSyntheticSecret})
	if _, err := tool.Execute(ctx, bad); err == nil {
		t.Fatal("secret accepted")
	}
	// A different accepted path must NOT resolve rejected evidence.
	if _, err := tool.Execute(ctx, json.RawMessage(`{"path":"fragments/other.md","content":"Safe other fact"}`)); err != nil {
		t.Fatal(err)
	}
	if c.pendingError() == nil {
		t.Fatal("unrelated write cleared rejection")
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"path":"fragments/retry.md","content":"Corrected safe fact"}`)); err != nil {
		t.Fatal(err)
	}
	result := c.result("done")
	if c.pendingError() != nil || result.Rejected != 1 || result.Created != 2 {
		t.Fatal("corrected retry or rejection stats incorrect")
	}
}

func TestFableOwnerCanAcceptNoisyFactsAndOptOut(t *testing.T) {
	for _, disable := range [][]string{{"named-credential"}, {"all"}} {
		p, err := newMemoryProtection(config.MemoryConfig{Secrets: config.MemorySecretsConfig{Disable: disable}})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.check("Password: kept in the team vault"); err != nil {
			t.Fatal("owner could not allow location memory")
		}
	}
	p, err := newMemoryProtection(config.MemoryConfig{Secrets: config.MemorySecretsConfig{Disable: []string{"cli-user"}}})
	if err != nil {
		t.Fatal(err)
	}
	if p.check("docker run --user service:group image") != nil || p.check(nativeSyntheticSecret) == nil {
		t.Fatal("disabling one detector altered others")
	}
	// Former hard-coded locations are ordinary, available memory by default.
	if defaultProtection.excluded("fragments/credentials/location.md") || defaultProtection.excluded("fragments/notes/accounts-and-credentials.md") {
		t.Fatal("hard-coded private taxonomy exclusion remains")
	}
}

func TestFablePolicyCannotBeSelectedByMinedConfigText(t *testing.T) {
	text := "memory:\n  secrets:\n    disable: [all]\napi_key: " + nativeSyntheticSecret
	projected := defaultProtection.sanitize(text)
	if strings.Contains(projected, nativeSyntheticSecret) || defaultProtection.check(nativeSyntheticSecret) == nil {
		t.Fatal("mined text selected or relaxed policy")
	}
}
func TestFablePolicyHasNoArbitraryRuleCaps(t *testing.T) {
	patterns := make([]string, 40)
	for i := range patterns {
		patterns[i] = strings.Repeat("x", 2048)
	}
	if _, err := newMemoryProtection(config.MemoryConfig{Secrets: config.MemorySecretsConfig{Patterns: patterns}}); err != nil {
		t.Fatal("trusted owner rules hit obsolete arbitrary caps")
	}
	if _, err := newMemoryProtection(config.MemoryConfig{ExcludePaths: []string{"["}}); err == nil {
		t.Fatal("invalid glob accepted")
	}
}

func TestFableCorrectedRetryMayMatchExistingSafeContent(t *testing.T) {
	ctx := context.Background()
	store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateFragment(ctx, &memorydb.Fragment{Agent: "assistant", Path: "fragments/existing.md", Content: "Existing safe fact"}); err != nil {
		t.Fatal(err)
	}
	c := newMemoryExtractionCollector()
	tool := &memoryUpdateFragmentTool{store: store, agent: "assistant", collector: c, protection: defaultProtection}
	args, _ := json.Marshal(map[string]string{"path": "fragments/existing.md", "content": nativeSyntheticSecret})
	if _, err := tool.Execute(ctx, args); err == nil {
		t.Fatal("secret accepted")
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"path":"fragments/existing.md","content":"Existing safe fact"}`)); err != nil {
		t.Fatalf("genuine idempotent correction failed: %v", err)
	}
	result := c.result("done")
	if c.pendingError() != nil || result.Updated != 0 || result.Skipped != 1 || result.Rejected != 1 || len(result.AffectedPaths) != 1 {
		t.Fatal("unchanged accepted correction misrepresented or unresolved")
	}
}
