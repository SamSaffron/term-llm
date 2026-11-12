package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestMessageWindowPaginationAndBounds(t *testing.T) {
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	candidate := memoryMineCandidate{Session: &session.Session{ID: "window"}, Agent: "assistant"}
	messages := []session.Message{
		{Sequence: 0, Role: core.RoleUser, CreatedAt: at.Add(-time.Hour), TextContent: "old"},
		{Sequence: 1, Role: core.RoleSystem, CreatedAt: at, TextContent: "system"},
		{Sequence: 2, Role: core.RoleUser, CreatedAt: at, TextContent: "at lower"},
		// Out-of-order lower timestamps are explicit skips, not early-stop assumptions.
		{Sequence: 3, Role: core.RoleUser, CreatedAt: at.Add(-time.Minute), TextContent: "older"},
		{Sequence: 4, Role: core.RoleTool, CreatedAt: at, TextContent: "raw tool body must never reach extraction"},
		{Sequence: 5, Role: core.RoleAssistant, CreatedAt: at.Add(time.Minute), TextContent: "fact"},
		{Sequence: 6, Role: core.RoleUser, CreatedAt: at.Add(time.Hour), TextContent: "at upper"},
		{Sequence: 7, Role: core.RoleUser, CreatedAt: at.Add(time.Second), TextContent: "out of order later sequence"},
	}
	for _, offset := range []int{0, 2, 5, 6, 8} {
		t.Run(string(rune('A'+offset)), func(t *testing.T) {
			opts := MineOptions{MessageSince: at, MessageBefore: at.Add(time.Hour), BatchSize: 2, PromptMaxTokens: 4000}
			source := &trackingMemoryMineStore{messages: map[string][]session.Message{"window": messages}}
			r, err := opts.loadMessagesForMining(context.Background(), source, candidate, offset, "empty")
			if err != nil {
				t.Fatal(err)
			}
			if offset < 6 && r.NextOffset != 6 {
				t.Fatalf("offset %d advanced to %d", offset, r.NextOffset)
			}
			if offset >= 6 && r.NextOffset != offset {
				t.Fatal("moved existing cursor backwards or consumed upper-bound evidence")
			}
			if (offset <= 6) != r.UpperBoundary {
				t.Fatal("upper barrier not reported")
			}
			for _, msg := range r.Messages {
				if msg.Sequence < offset || msg.Sequence == 0 || msg.Sequence == 1 || msg.Sequence == 3 || msg.Sequence >= 6 {
					t.Fatal("out-of-scope message loaded")
				}
			}
			prompt := opts.extractionPrompt(candidate, offset, r, "empty").Prompt
			if strings.Contains(prompt, "raw tool body") || strings.Contains(prompt, "at upper") {
				t.Fatal("out-of-scope/tool body reached prompt")
			}
			if offset == 0 && (r.SkippedBefore != 2 || len(r.EvidenceRanges) != 2 || r.EvidenceRanges[0] != (memoryEvidenceRange{2, 3}) || r.EvidenceRanges[1] != (memoryEvidenceRange{4, 6})) {
				t.Fatalf("incorrect evidence spans %+v", r)
			}
			// Removing upper bound resumes at the barrier, including the later in-window
			// out-of-order message. No unseen sequence was discarded.
			if r.UpperBoundary {
				opts.MessageBefore = time.Time{}
				next, err := opts.loadMessagesForMining(context.Background(), source, candidate, r.NextOffset, "empty")
				if err != nil || len(next.Messages) != 2 || next.NextOffset != 8 {
					t.Fatal("upper-bound resume lost unread evidence")
				}
			}
		})
	}
}

func TestMessageWindowMissingDateAndLosslessBudget(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	candidate := memoryMineCandidate{Session: &session.Session{ID: "window"}, Agent: "assistant"}
	opts := MineOptions{MessageSince: at, BatchSize: 1, PromptMaxTokens: 2000}
	source := &trackingMemoryMineStore{messages: map[string][]session.Message{"window": {{Sequence: 0, Role: core.RoleUser, TextContent: "unknown date"}}}}
	if _, err := opts.loadMessagesForMining(context.Background(), source, candidate, 0, "empty"); err == nil {
		t.Fatal("accepted unknown timestamp")
	}
	source.messages["window"] = []session.Message{{Sequence: 0, CreatedAt: at, Role: core.RoleUser, TextContent: strings.Repeat("user fact ", 10000)}}
	if _, err := opts.loadMessagesForMining(context.Background(), source, candidate, 0, "empty"); err == nil {
		t.Fatal("silently clipped single message")
	}
	source.messages["window"] = append([]session.Message{{Sequence: 0, CreatedAt: at, Role: core.RoleUser, TextContent: "small first fact"}}, session.Message{Sequence: 1, CreatedAt: at, Role: core.RoleUser, TextContent: strings.Repeat("later fact ", 10000)})
	r, err := opts.loadMessagesForMining(context.Background(), source, candidate, 0, "empty")
	if err != nil || r.NextOffset != 1 || len(r.Messages) != 1 || !r.PromptBudgetHit || r.TruncatedMessages != 0 || r.UserMessagesCut != 0 {
		t.Fatalf("budget did not defer losslessly: %+v %v", r, err)
	}
	opts.PromptMaxTokens = 100000
	r, err = opts.loadMessagesForMining(context.Background(), source, candidate, 1, "empty")
	if err != nil || r.NextOffset != 2 || len(r.Messages) != 1 || r.Messages[0].TextContent != source.messages["window"][1].TextContent {
		t.Fatal("increased budget did not preserve entire deferred message")
	}
}

func TestMessageWindowNativeMiningProvenanceAndEmptyBatch(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "mixed", true: "old only"}[empty], func(t *testing.T) {
			at := time.Now().Add(-time.Hour)
			provider := mockprovider.NewMockProvider("window").AddToolCall("create", "memory_create_fragment", map[string]string{"path": "fragments/window.md", "content": "Safe included fact"}).AddTextResponse("done")
			opts, writer := nativeMineFixture(t, provider)
			opts.MessageSince = at
			opts.PromptMaxTokens = 4000
			opts.BatchSize = 1
			ctx := context.Background()
			id := "window-source"
			if err := writer.Create(ctx, &session.Session{ID: id, Agent: "assistant", Status: session.StatusComplete}); err != nil {
				t.Fatal(err)
			}
			msgs := []*session.Message{session.NewMessage(id, core.UserText("OLD OMITTED"), 0)}
			msgs[0].CreatedAt = at.Add(-time.Hour)
			if !empty {
				m := session.NewMessage(id, core.UserText("included"), 1)
				m.CreatedAt = at
				msgs = append(msgs, m)
				m = session.NewMessage(id, core.UserText("OLD OMITTED AGAIN"), 2)
				m.CreatedAt = at.Add(-time.Second)
				msgs = append(msgs, m)
				m = session.NewMessage(id, core.AssistantText("included too"), 3)
				m.CreatedAt = at
				msgs = append(msgs, m)
			}
			for _, m := range msgs {
				if err := writer.AddMessage(ctx, id, m); err != nil {
					t.Fatal(err)
				}
			}
			if err := Mine(opts); err != nil {
				t.Fatal(err)
			}
			store, err := opts.OpenStore()
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			state, err := store.GetState(ctx, id)
			if err != nil || state == nil || state.LastMinedOffset != len(msgs) {
				t.Fatal("raw cursor accounting failed")
			}
			reqs := provider.RecordedRequests()
			if empty {
				if len(reqs) != 0 {
					t.Fatal("model invoked for out-of-scope-only session")
				}
				return
			}
			if strings.Contains(reqs[0].Messages[1].Parts[0].Text, "OLD OMITTED") {
				t.Fatal("old evidence reached provider")
			}
			links, err := store.GetFragmentSources(ctx, "assistant", "fragments/window.md")
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(links)
			if len(links) != 2 || links[0].TurnStart != 1 || links[0].TurnEnd != 2 || links[1].TurnStart != 3 || links[1].TurnEnd != 4 {
				t.Fatalf("expected exact separate evidence spans: %s", data)
			}
		})
	}
}

func TestMessageWindowValidation(t *testing.T) {
	at := time.Now()
	for _, opts := range []MineOptions{{MessageSince: at, MessageBefore: at}, {MessageSince: at, MessageBefore: at.Add(-time.Second)}, {MessageSince: at, Insights: true}} {
		if err := Mine(opts); err == nil {
			t.Fatal("invalid temporal invocation accepted")
		}
	}
}

func TestMessageWindowProviderUsageIsCacheAdditive(t *testing.T) {
	stats := memoryExtractionStats{}
	stats.noteUsage(&core.Usage{InputTokens: 40, CachedInputTokens: 60, OutputTokens: 7, ProviderRawInputTokens: 100})
	stats.noteUsage(&core.Usage{InputTokens: 30, CachedInputTokens: 90, OutputTokens: 5, ProviderRawInputTokens: 120})
	if stats.InputTokens+stats.CachedInputTokens != 220 || stats.OutputTokens != 12 || stats.ToolTurns != 2 {
		t.Fatalf("wrong resubmitted-context accounting: %+v", stats)
	}
}
