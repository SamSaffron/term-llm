package memory

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	memorydb "github.com/term-llm/term-llm/internal/memory"
	"github.com/term-llm/term-llm/internal/session"
)

func rereviewSession(t *testing.T, writer session.Store, id, text string) {
	t.Helper()
	ctx := context.Background()
	if err := writer.Create(ctx, &session.Session{ID: id, Agent: "assistant", Status: session.StatusComplete}); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []core.Message{core.UserText(text), core.AssistantText("Confirmed")} {
		if err := writer.AddMessage(ctx, id, session.NewMessage(id, msg, -1)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFableR1OrdinaryErrorsDoNotPoisonBatch(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		args       json.RawMessage
	}{
		{"traversal", "memory_create_fragment", json.RawMessage(`{"path":"../x.md","content":"safe"}`)},
		{"absolute", "memory_create_fragment", json.RawMessage(`{"path":"/abs/x.md","content":"safe"}`)},
		{"malformed", "memory_create_fragment", json.RawMessage(`{`)},
		{"empty path", "memory_create_fragment", json.RawMessage(`{"content":"safe"}`)},
		{"empty content", "memory_create_fragment", json.RawMessage(`{"path":"fragments/empty.md","content":""}`)},
		{"mistyped update", "memory_update_fragment", json.RawMessage(`{"path":"fragments/typo.md","content":"safe"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mockprovider.NewMockProvider("r1").AddTurn(mockprovider.MockTurn{ToolCalls: []core.ToolCall{{ID: "bad", Name: tc.tool, Arguments: tc.args}}}).AddToolCall("good", "memory_create_fragment", map[string]string{"path": "fragments/x.md", "content": "Safe durable fact"}).AddTextResponse("done")
			opts, writer := nativeMineFixture(t, p)
			rereviewSession(t, writer, "ordinary", "Durable evidence")
			if err := Mine(opts); err != nil {
				t.Fatalf("ordinary mistake poisoned batch: %v", err)
			}
			store, err := opts.OpenStore()
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			state, err := store.GetState(context.Background(), "ordinary")
			if err != nil || state == nil || state.LastMinedOffset != 2 {
				t.Fatal("corrected batch did not advance")
			}
		})
	}
}

func TestFableR2StructuredEvidencePreservedAndR3ParallelCalls(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "additional pattern"}[custom], func(t *testing.T) {
			p := mockprovider.NewMockProvider("r2").AddToolCall("lookup", "memory_get_fragment", map[string]string{"path": "fragments/source.md"}).AddTextResponse("done")
			opts, writer := nativeMineFixture(t, p)
			cfg := config.MemoryConfig{}
			if custom {
				cfg.Secrets.Patterns = []string{`PRIVATE_SENTINEL_[0-9]+`}
			}
			policy, err := newMemoryProtection(cfg)
			if err != nil {
				t.Fatal(err)
			}
			opts.protection = policy
			text := "The DB password: synthetic-value-1\nWe decided to use Postgres 16 for the new service; deploy Friday.\n```yaml\npassword: synthetic-value-2\nengine: Postgres16\nreplicas: 3\n```\nPRIVATE_SENTINEL_123\nKeep this custom following fact."
			rereviewSession(t, writer, "structured", text)
			store, err := opts.OpenStore()
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.CreateFragment(context.Background(), &memorydb.Fragment{Agent: "assistant", Path: "fragments/source.md", Content: text}); err != nil {
				t.Fatal(err)
			}
			if err := Mine(opts); err != nil {
				t.Fatal(err)
			}
			reqs := p.RecordedRequests()
			if len(reqs) != 2 {
				t.Fatalf("got %d requests", len(reqs))
			}
			if !reqs[0].ParallelToolCalls {
				t.Error("provider parallel calls disabled")
			}
			prompt := reqs[0].Messages[1].Parts[0].Text
			lookup := ""
			for _, msg := range reqs[1].Messages {
				for _, part := range msg.Parts {
					if part.ToolResult != nil && part.ToolResult.Name == "memory_get_fragment" {
						lookup = part.ToolResult.Content
					}
				}
			}
			for name, value := range map[string]string{"extraction": prompt, "lookup": lookup} {
				for _, fact := range []string{"We decided to use Postgres 16", "engine: Postgres16", "replicas: 3", "Keep this custom following fact."} {
					if !strings.Contains(value, fact) {
						t.Errorf("%s lost %q", name, fact)
					}
				}
				for _, secret := range []string{"synthetic-value-1", "synthetic-value-2"} {
					if strings.Contains(value, secret) {
						t.Errorf("%s leaked synthetic credential", name)
					}
				}
				if custom && strings.Contains(value, "PRIVATE_SENTINEL_123") {
					t.Errorf("%s leaked custom match", name)
				}
			}
		})
	}
}

func TestFableR4OwnerSeesSafeDetectorIDs(t *testing.T) {
	for _, corrected := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "corrected"}[corrected], func(t *testing.T) {
			p := mockprovider.NewMockProvider("r4").AddToolCall("bad", "memory_create_fragment", map[string]string{"path": "fragments/retry.md", "content": "Password: synthetic-value-3\nPRIVATE_SENTINEL_123"})
			if corrected {
				p.AddToolCall("good", "memory_create_fragment", map[string]string{"path": "fragments/retry.md", "content": "Safe fact"})
			}
			p.AddTextResponse("done")
			opts, writer := nativeMineFixture(t, p)
			policy, err := newMemoryProtection(config.MemoryConfig{Secrets: config.MemorySecretsConfig{Patterns: []string{`PRIVATE_SENTINEL_[0-9]+`}}})
			if err != nil {
				t.Fatal(err)
			}
			opts.protection = policy
			rereviewSession(t, writer, "diagnostic", "Durable evidence")
			// A temporary regular file avoids pipe buffer deadlocks and captures both owner streams.
			f, err := os.CreateTemp(t.TempDir(), "output")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			oldOut, oldErr := os.Stdout, os.Stderr
			func() {
				os.Stdout, os.Stderr = f, f
				defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
				err = Mine(opts)
			}()
			if (err == nil) != corrected {
				t.Fatalf("unexpected batch outcome: %v", err)
			}
			if _, err := f.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(f)
			if err != nil {
				t.Fatal(err)
			}
			output := string(data)
			for _, id := range []string{"named-credential", "pattern #1"} {
				if !strings.Contains(output, id) {
					t.Errorf("owner output missing %s: %s", id, output)
				}
			}
			for _, unsafe := range []string{"synthetic-value-3", "PRIVATE_SENTINEL_123", `PRIVATE_SENTINEL_[0-9]+`} {
				if strings.Contains(output, unsafe) {
					t.Fatal("owner diagnostic leaked content or pattern")
				}
			}
		})
	}
}

func TestFableR2MetadataAndLookupFieldsSanitizedBeforeEncoding(t *testing.T) {
	ctx := context.Background()
	opts, _ := nativeMineFixture(t, mockprovider.NewMockProvider("metadata"))
	store, err := opts.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := defaultProtection
	agent := "owner-" + nativeSyntheticSecret
	fragPath := "fragments/" + nativeSyntheticSecret + "/note.md"
	if err := store.CreateFragment(ctx, &memorydb.Fragment{Agent: agent, Path: fragPath, Content: "needle safe fact"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tool core.Tool
		args map[string]string
	}{
		{&memorySearchFragmentsTool{store: store, agent: agent, protection: p}, map[string]string{"query": "needle OR " + nativeSyntheticSecret}},
		{&memoryListFragmentsTool{store: store, agent: agent, protection: p}, map[string]string{"prefix": "fragments/" + nativeSyntheticSecret}},
		{&memoryGetFragmentTool{store: store, agent: agent, protection: p}, map[string]string{"path": fragPath}},
		{&memoryGetFragmentTool{store: store, agent: agent, protection: p}, map[string]string{"path": fragPath + "absent"}},
	} {
		args, _ := json.Marshal(tc.args)
		out, err := tc.tool.Execute(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.Content, nativeSyntheticSecret) {
			t.Fatal("lookup leaked original metadata")
		}
		if !json.Valid([]byte(out.Content)) {
			t.Fatal("lookup is not valid JSON")
		}
	}
	candidate := memoryMineCandidate{Session: &session.Session{ID: nativeSyntheticSecret}, Agent: agent}
	safe := p.candidate(candidate)
	prompt := buildExtractionPromptParts(safe, 0, 1, nil, "empty").Prompt
	if strings.Contains(prompt, nativeSyntheticSecret) {
		t.Fatal("extraction metadata leaked")
	}
	if candidate.Session.ID != nativeSyntheticSecret || candidate.Agent != agent {
		t.Fatal("provider projection changed persistent identity")
	}
}

func TestFableR2InsightPreservesEvidenceAfterCredential(t *testing.T) {
	ctx := context.Background()
	provider := mockprovider.NewMockProvider("insight-evidence").AddTextResponse(`{"insights":[]}`)
	opts, _ := nativeMineFixture(t, provider)
	store, err := opts.OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	candidate := memoryMineCandidate{Session: &session.Session{ID: "insight-evidence"}, Agent: "assistant"}
	text := "password: synthetic-value-4\nWe chose Postgres 16\nreplicas: 3"
	messages := []session.Message{}
	for i := 0; i < 4; i++ {
		messages = append(messages, session.Message{Sequence: i, Role: core.RoleUser, TextContent: text})
	}
	source := &trackingMemoryMineStore{messages: map[string][]session.Message{candidate.Session.ID: messages}}
	if _, _, err := opts.runInsightExtractionPass(ctx, &config.Config{}, engine.NewEngine(provider, nil), source, store, []memoryMineCandidate{candidate}); err != nil {
		t.Fatal(err)
	}
	reqs := provider.RecordedRequests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests", len(reqs))
	}
	prompt := reqs[0].Messages[1].Parts[0].Text
	if strings.Contains(prompt, "synthetic-value-4") || !strings.Contains(prompt, "We chose Postgres 16") || !strings.Contains(prompt, "replicas: 3") {
		t.Fatal("insight prompt leaked credential or lost following evidence")
	}
}
