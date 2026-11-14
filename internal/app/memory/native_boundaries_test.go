package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	memorydb "github.com/term-llm/term-llm/internal/memory"
	"github.com/term-llm/term-llm/internal/session"
)

func TestNativeLookupsAndTaxonomySanitizeCompleteSources(t *testing.T) {
	ctx := context.Background()
	store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, path := range []string{"fragments/notes/legacy.md", "fragments/credentials/excluded.md"} {
		if err := store.CreateFragment(ctx, &memorydb.Fragment{Agent: "assistant", Path: path, Content: nativeSyntheticSecret + " needle"}); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := newMemoryProtection(config.MemoryConfig{ExcludePaths: []string{"fragments/credentials/**"}})
	if err != nil {
		t.Fatal(err)
	}
	get := &memoryGetFragmentTool{store: store, agent: "assistant", readBytes: 10, protection: policy}
	search := &memorySearchFragmentsTool{store: store, agent: "assistant", readBytes: 10, protection: policy}
	for _, tc := range []struct {
		tool core.Tool
		args json.RawMessage
	}{
		{get, json.RawMessage(`{"path":"fragments/notes/legacy.md"}`)},
		{search, json.RawMessage(`{"query":"needle"}`)},
		{&memoryListFragmentsTool{store: store, agent: "assistant", protection: policy}, json.RawMessage(`{}`)},
	} {
		out, err := tc.tool.Execute(ctx, tc.args)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.Content, "sk-synth") || strings.Contains(out.Content, "credentials/") {
			t.Fatal("clipped secret prefix or excluded path leaked")
		}
	}
	taxonomy := policy.taxonomy([]memorydb.Fragment{{Path: "fragments/" + nativeSyntheticSecret + "/note.md"}, {Path: "fragments/credentials/excluded.md"}}, 1000)
	if strings.Contains(taxonomy, "sk-synth") || strings.Contains(taxonomy, "credentials/") {
		t.Fatal("taxonomy summarized an unsanitized path")
	}
}

func TestNativeInsightFieldsRejectBeforeAnyWriteOrCheckpoint(t *testing.T) {
	for _, field := range []string{"rule", "compact", "trigger", "category"} {
		t.Run(field, func(t *testing.T) {
			item := map[string]any{"rule": "Prefer terse status updates", "compact": "Be terse", "trigger": "When giving status", "category": "workflow", "confidence": 0.8}
			item[field] = nativeSyntheticSecret
			data, _ := json.Marshal(map[string]any{"insights": []any{item}})
			provider := mockprovider.NewMockProvider("native").AddTextResponse(string(data))
			opts, _ := nativeMineFixture(t, provider)
			opts.PromptMaxTokens = 12000
			ctx := context.Background()
			store, err := opts.OpenStore()
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			candidate := memoryMineCandidate{Session: &session.Session{ID: "synthetic-insight"}, Agent: "assistant"}
			messages := []session.Message{}
			for i := 0; i < 4; i++ {
				messages = append(messages, session.Message{Sequence: i, Role: core.RoleUser, TextContent: "Prefer terse status updates"})
			}
			source := &trackingMemoryMineStore{messages: map[string][]session.Message{candidate.Session.ID: messages}}
			_, _, err = opts.runInsightExtractionPass(ctx, &config.Config{}, engine.NewEngine(provider, nil), source, store, []memoryMineCandidate{candidate})
			if !errors.Is(err, errMemorySafety) {
				t.Fatal("insight secret accepted")
			}
			insights, _ := store.ListInsights(ctx, "assistant", 100)
			mined, _ := store.InsightMinedAt(ctx, candidate.Session.ID)
			if len(insights) != 0 || !mined.IsZero() {
				t.Fatal("unsafe insight wrote or advanced marker")
			}
		})
	}
}

func TestNativeFreshenRejectsBeforeRecentAndOffsets(t *testing.T) {
	ctx := context.Background()
	provider := mockprovider.NewMockProvider("native").AddTextResponse("# Bad\n\napi_key: " + nativeSyntheticSecret)
	opts, writer := nativeMineFixture(t, provider)
	sess := &session.Session{ID: "freshen-fixture", Agent: "assistant", Status: session.StatusComplete, CreatedAt: time.Now().Add(-time.Hour)}
	if err := writer.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := writer.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, core.UserText("new activity"), -1)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "recent.md")
	if err := os.WriteFile(file, []byte("known good"), 0600); err != nil {
		t.Fatal(err)
	}
	err := UpdateRecent(UpdateRecentOptions{Env: opts.Env, File: file, TargetTokens: 50, MaxInputChars: 5000, FragmentChars: 1000})
	if !errors.Is(err, errMemorySafety) {
		t.Fatalf("unsafe freshen output accepted: %v", err)
	}
	data, _ := os.ReadFile(file)
	store, err := opts.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	meta, _ := store.GetMeta(ctx, memoryUpdateRecentMetaKey("assistant"))
	offset, _ := readUpdateRecentOffset(ctx, store, sess.ID)
	if string(data) != "known good" || meta != "" || offset != 0 {
		t.Fatal("freshen rejection changed recent or offsets")
	}
}

func TestNativeEvidenceSkipsPrivilegedRolesBeforeBudget(t *testing.T) {
	opts := MineOptions{BatchSize: 1, MaxMessages: 1, PromptMaxTokens: 2000}
	candidate := memoryMineCandidate{Session: &session.Session{ID: "evidence"}, Agent: "assistant"}
	messages := []session.Message{{Sequence: 0, Role: core.RoleSystem, TextContent: strings.Repeat("privileged ", 100000)}, {Sequence: 3, Role: core.RoleDeveloper, TextContent: "privileged"}, {Sequence: 7, Role: core.RoleEvent, TextContent: "privileged"}, {Sequence: 9, Role: core.RoleUser, TextContent: nativeSyntheticSecret + " durable fact"}}
	source := &trackingMemoryMineStore{messages: map[string][]session.Message{"evidence": messages}}
	got, err := opts.loadMessagesForMining(context.Background(), source, candidate, 0, "")
	if err != nil || got.NextOffset != 10 || len(got.Messages) != 1 || got.TruncatedMessages != 0 || strings.Contains(got.Messages[0].TextContent, "sk-synth") {
		t.Fatal("privileged context affected evidence budget or source was clipped before sanitation")
	}
	for _, transcript := range [][]transcriptMessage{buildTranscript(messages), buildInsightTranscript(messages)} {
		for _, msg := range transcript {
			if strings.Contains(msg.Text, "privileged") {
				t.Fatal("privileged context entered evidence")
			}
		}
	}
}

func TestNativeAnchoredRulesApplyBeforeCompositionAndClipping(t *testing.T) {
	source := "BEGIN_PRIVATE/" + strings.Repeat("X", 150) + "/END"
	cfg := config.MemoryConfig{Secrets: config.MemorySecretsConfig{Patterns: []string{`^BEGIN_PRIVATE/[X]+/END$`}}}
	p, err := newMemoryProtection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.fragment("fragments/notes/test.md", source) == nil || p.check(source) == nil {
		t.Fatal("field composition bypassed anchored detector")
	}
	prompt := p.promotePrompt([]memorydb.Fragment{{Path: "fragments/notes/test.md", Content: source}}, source)
	if strings.Contains(prompt, "BEGIN_PRIVATE") {
		t.Fatal("promotion composed raw fields before sanitation")
	}
	msg := session.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{Name: "read_file", ToolInfo: source}}}}
	safe := p.message(msg)
	transcript := buildInsightTranscript([]session.Message{{Role: core.RoleUser, TextContent: strings.Repeat("useful conversation ", 40)}, safe})
	for _, entry := range transcript {
		if strings.Contains(entry.Text, "BEGIN_PRIVATE") {
			t.Fatal("tool metadata clipped before sanitation")
		}
	}
	if msg.Parts[0].ToolCall.ToolInfo != source {
		t.Fatal("source message mutated")
	}
}

func TestNativeInsightFeatureStillPersistsSafeRules(t *testing.T) {
	provider := mockprovider.NewMockProvider("native").AddTextResponse(`{"insights":[{"rule":"Prefer terse status updates","compact":"Be terse","trigger":"When giving status","category":"workflow","confidence":0.8}]}`)
	opts, _ := nativeMineFixture(t, provider)
	opts.PromptMaxTokens = 12000
	ctx := context.Background()
	store, err := opts.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	candidate := memoryMineCandidate{Session: &session.Session{ID: "safe-insight"}, Agent: "assistant"}
	source := &trackingMemoryMineStore{messages: map[string][]session.Message{candidate.Session.ID: {
		{Sequence: 0, Role: core.RoleUser, TextContent: "Prefer terse status updates"},
		{Sequence: 1, Role: core.RoleAssistant, TextContent: "Acknowledged"},
		{Sequence: 2, Role: core.RoleUser, TextContent: "Always keep status concise"},
		{Sequence: 3, Role: core.RoleAssistant, TextContent: "Understood"},
	}}}
	count, _, err := opts.runInsightExtractionPass(ctx, &config.Config{}, engine.NewEngine(provider, nil), source, store, []memoryMineCandidate{candidate})
	insights, _ := store.ListInsights(ctx, "assistant", 100)
	mined, _ := store.InsightMinedAt(ctx, candidate.Session.ID)
	if err != nil || count != 1 || len(insights) != 1 || mined.IsZero() {
		t.Fatal("safe insight feature was disabled")
	}
}

func TestNativeAdditiveRulesSeeOriginalOverlappingFields(t *testing.T) {
	source := "private-id=example; password=example"
	p, err := newMemoryProtection(config.MemoryConfig{Secrets: config.MemorySecretsConfig{Patterns: []string{regexp.QuoteMeta(source)}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.sanitize(source); strings.Contains(got, "private-id") || strings.Contains(got, "example") {
		t.Fatal("built-in redaction hid context needed by configured detector")
	}
	if p.check(source) == nil {
		t.Fatal("overlapping detector output accepted")
	}
	p, err = newMemoryProtection(config.MemoryConfig{Secrets: config.MemorySecretsConfig{Patterns: []string{`private-id=`, regexp.QuoteMeta(source)}}})
	if err != nil || strings.Contains(p.sanitize(source), "example") {
		t.Fatal("one configured rule hid another's original match")
	}
}

func TestNativeInsightValidatesExactNormalizedFields(t *testing.T) {
	for _, field := range []string{"rule", "compact", "trigger", "category"} {
		t.Run(field, func(t *testing.T) {
			item := map[string]any{"rule": "Prefer terse status updates", "compact": "Be terse", "trigger": "When giving status", "category": "workflow", "confidence": 0.8}
			item[field] = " private-local-123456 "
			data, _ := json.Marshal(map[string]any{"insights": []any{item}})
			provider := mockprovider.NewMockProvider("native").AddTextResponse(string(data))
			opts, _ := nativeMineFixture(t, provider)
			opts.PromptMaxTokens = 12000
			cfg := &config.Config{Memory: config.MemoryConfig{Secrets: config.MemorySecretsConfig{Patterns: []string{`^private-local-[0-9]+$`}}}}
			ctx := context.Background()
			store, err := opts.OpenStore()
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			candidate := memoryMineCandidate{Session: &session.Session{ID: "normalized-insight"}, Agent: "assistant"}
			messages := []session.Message{}
			for i := 0; i < 4; i++ {
				messages = append(messages, session.Message{Sequence: i, Role: core.RoleUser, TextContent: "Prefer terse status updates"})
			}
			source := &trackingMemoryMineStore{messages: map[string][]session.Message{candidate.Session.ID: messages}}
			_, _, err = opts.runInsightExtractionPass(ctx, cfg, engine.NewEngine(provider, nil), source, store, []memoryMineCandidate{candidate})
			if !errors.Is(err, errMemorySafety) {
				t.Fatal("normalization bypassed configured detector")
			}
			insights, _ := store.ListInsights(ctx, "assistant", 100)
			mined, _ := store.InsightMinedAt(ctx, candidate.Session.ID)
			if len(insights) != 0 || !mined.IsZero() {
				t.Fatal("normalized detected field persisted or checkpoint advanced")
			}
		})
	}
}

func TestNativeRecentOwnersCheckRawModelTextBeforeTrimming(t *testing.T) {
	for _, freshen := range []bool{false, true} {
		t.Run(map[bool]string{false: "promote", true: "update-recent"}[freshen], func(t *testing.T) {
			response := " # Good\n\nprivate-local-123456 "
			provider := mockprovider.NewMockProvider("native").AddTextResponse(response)
			opts, writer := nativeMineFixture(t, provider)
			load := opts.LoadConfig
			opts.LoadConfig = func() (*config.Config, error) {
				cfg, err := load()
				cfg.Memory.Secrets.Patterns = []string{`^ # Good\n\nprivate-local-[0-9]+ $`}
				return cfg, err
			}
			ctx := context.Background()
			dir := t.TempDir()
			agent := filepath.Join(dir, "agent")
			if err := os.MkdirAll(filepath.Join(agent, "memory"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(agent, "agent.yaml"), []byte("name: fixture\ndescription: fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
			recent := filepath.Join(agent, "memory", "recent.md")
			if err := os.WriteFile(recent, []byte("known good"), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := opts.OpenStore()
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if freshen {
				sess := &session.Session{ID: "raw-recent", Agent: "assistant", Status: session.StatusComplete}
				if err := writer.Create(ctx, sess); err != nil {
					t.Fatal(err)
				}
				if err := writer.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, core.UserText("new activity"), -1)); err != nil {
					t.Fatal(err)
				}
				err = UpdateRecent(UpdateRecentOptions{Env: opts.Env, File: recent, TargetTokens: 50, MaxInputChars: 5000, FragmentChars: 1000})
			} else {
				if err := store.CreateFragment(ctx, &memorydb.Fragment{Agent: agent, Path: "fragments/notes/safe.md", Content: "safe fact"}); err != nil {
					t.Fatal(err)
				}
				cfg, loadErr := opts.LoadConfig()
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				_, err = runMemoryPromoteFlow(ctx, testEnv("", ""), cfg, engine.NewEngine(provider, nil), store, memoryPromoteOptions{Agent: agent, RecentMaxBytes: 200})
			}
			if !errors.Is(err, errMemorySafety) {
				t.Fatal("raw model text lost detector context before recent validation")
			}
			data, _ := os.ReadFile(recent)
			if string(data) != "known good" {
				t.Fatal("raw-model rejection changed recent file")
			}
		})
	}
}

func TestFableImageFailuresRetainBoundaryButDoNotDropLaterRows(t *testing.T) {
	opts, _ := nativeMineFixture(t, mockprovider.NewMockProvider("image-retry"))
	ctx := context.Background()
	store, err := opts.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Temporary fixture trigger forces exactly one image write to fail. No live DB.
	db, err := sql.Open("sqlite", opts.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_poison BEFORE INSERT ON memory_fragments WHEN NEW.path LIKE 'images/%poison%' BEGIN SELECT RAISE(FAIL,'synthetic image write failure'); END`); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"poison", "later safe"} {
		if err := store.RecordImage(ctx, &memorydb.ImageRecord{Agent: "assistant", Prompt: prompt, OutputPath: "/synthetic/image.png", Provider: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE generated_images SET created_at = '2026-01-02 03:04:05'`); err != nil {
		t.Fatal(err)
	}
	count, err := opts.Env.mineGeneratedImages(ctx, store)
	mark, _ := store.GetMeta(ctx, opts.Env.memoryImageMineMetaKey("assistant"))
	if err == nil || count != 1 || mark != "" {
		t.Fatal("failed timestamp group was crossed or later image starved")
	}
	if _, err := db.Exec(`DROP TRIGGER fail_poison`); err != nil {
		t.Fatal(err)
	}
	count, err = opts.Env.mineGeneratedImages(ctx, store)
	mark, _ = store.GetMeta(ctx, opts.Env.memoryImageMineMetaKey("assistant"))
	if err != nil || count != 2 || mark == "" {
		t.Fatal("failed image was not retried with original watermark")
	}
	// Include the exact timestamp boundary: a row created in that same second
	// after a previous snapshot must not become permanently invisible.
	boundary, err := time.Parse(time.RFC3339Nano, mark)
	if err != nil {
		t.Fatal(err)
	}
	images, err := store.ListImagesSince(ctx, "assistant", boundary)
	if err != nil || len(images) != 2 {
		t.Fatal("inclusive boundary replay lost tied image rows")
	}
	if err := store.RecordImage(ctx, &memorydb.ImageRecord{ID: "late-same-timestamp", Agent: "assistant", Prompt: "arrived after snapshot", OutputPath: "/synthetic/late.png", Provider: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE generated_images SET created_at = '2026-01-02 03:04:05' WHERE id='late-same-timestamp'`); err != nil {
		t.Fatal(err)
	}
	count, err = opts.Env.mineGeneratedImages(ctx, store)
	if err != nil || count != 3 {
		t.Fatal("row arriving on checkpoint boundary was lost")
	}

}

func TestFablePromoteCompactsOnceThenPreservesOldFile(t *testing.T) {
	for _, fits := range []bool{false, true} {
		t.Run(map[bool]string{false: "still-over", true: "fits"}[fits], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			agent := filepath.Join(dir, "agent")
			if err := os.MkdirAll(filepath.Join(agent, "memory"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(agent, "agent.yaml"), []byte("name: fixture\ndescription: fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
			recent := filepath.Join(agent, "memory", "recent.md")
			if err := os.WriteFile(recent, []byte("known good"), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(dir, "memory.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.CreateFragment(ctx, &memorydb.Fragment{Agent: agent, Path: "fragments/fact.md", Content: "Safe fact"}); err != nil {
				t.Fatal(err)
			}
			compact := strings.Repeat("long ", 100)
			if fits {
				compact = "Safe compact summary."
			}
			provider := mockprovider.NewMockProvider("compaction").AddTextResponse(strings.Repeat("long ", 100)).AddTextResponse(compact)
			_, err = runMemoryPromoteFlow(ctx, testEnv("", ""), &config.Config{}, engine.NewEngine(provider, nil), store, memoryPromoteOptions{Agent: agent, RecentMaxBytes: 100})
			if len(provider.RecordedRequests()) != 2 {
				t.Fatal("expected exactly one compaction retry")
			}
			data, _ := os.ReadFile(recent)
			meta, _ := store.GetMeta(ctx, memoryPromoteMetaKey(agent))
			if fits {
				if err != nil || string(data) != compact || meta == "" {
					t.Fatal("valid compaction not persisted")
				}
			} else if !errors.Is(err, errRecentSize) || string(data) != "known good" || meta != "" {
				t.Fatal("oversize result mutated old summary or watermark")
			}
		})
	}
}

func TestFableImageFailureDoesNotPreventEmbeddingPhase(t *testing.T) {
	opts, _ := nativeMineFixture(t, mockprovider.NewMockProvider("later-phase"))
	opts.Embed = true
	opts.EmbedProvider = "invalid-owner-test-provider"
	ctx := context.Background()
	store, err := opts.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db, err := sql.Open("sqlite", opts.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_images BEFORE INSERT ON memory_fragments WHEN NEW.path LIKE 'images/%' BEGIN SELECT RAISE(FAIL,'synthetic image failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordImage(ctx, &memorydb.ImageRecord{Agent: "assistant", Prompt: "A safe image", OutputPath: "/synthetic/image.png", Provider: "fixture"}); err != nil {
		t.Fatal(err)
	}
	err = Mine(opts)
	if err == nil || !strings.Contains(err.Error(), "synthetic image failure") || !strings.Contains(err.Error(), "unknown embedding provider") {
		t.Fatal("image failure prevented later independent phase or was hidden")
	}
}
