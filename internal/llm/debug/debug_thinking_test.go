package debug

import (
	"context"
	. "github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"io"
	"strings"
	"testing"
)

func TestDebugThinkingSequence(t *testing.T) {
	for _, prompt := range []string{"think", "thinkraw", "think,markdown*20,thinkraw"} {
		actions := parseSequenceDSL(prompt, nil)
		if len(actions) == 0 {
			t.Fatalf("not parsed: %s", prompt)
		}
		provider := NewDebugProvider("fast")
		provider.preset.Delay = 0
		provider.preset.ChunkSize = 5
		stream, err := provider.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: prompt}}}}})
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		ids := map[string]bool{}
		kinds := map[ReasoningKind]bool{}
		chunks := 0
		for {
			ev, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if ev.Type != EventReasoningDelta {
				continue
			}
			chunks++
			text.WriteString(ev.Text)
			ids[ev.ReasoningItemID] = true
			kinds[ev.ReasoningKind] = true
			if len(ev.Text) > 5 || ev.ReasoningItemID == "" {
				t.Fatalf("invalid delta: %#v", ev)
			}
		}
		stream.Close()
		if chunks < 2 || text.Len() == 0 {
			t.Fatalf("no chunked reasoning: %s", prompt)
		}
		if prompt == "think" && (!kinds[ReasoningKindSummary] || !strings.HasPrefix(text.String(), "**Checking the next step**\n\n")) {
			t.Fatalf("summary=%q", text.String())
		}
		if prompt == "thinkraw" && !kinds[ReasoningKindRaw] {
			t.Fatal("wrong raw kind")
		}
		if strings.Contains(prompt, ",") && len(ids) != 2 {
			t.Fatal("bursts reuse item id")
		}
	}
}

func TestDebugThinkingEnginePersistence(t *testing.T) {
	for _, prompt := range []string{"think", "thinkraw"} {
		provider := NewDebugProvider("fast")
		provider.preset.Delay = 0
		runner := engine.NewEngine(provider, engine.NewToolRegistry())
		var persisted Message
		runner.SetTurnCompletedCallback(func(_ context.Context, _ int, messages []Message, _ TurnMetrics) error {
			persisted = messages[0]
			return nil
		})
		stream, err := runner.Stream(context.Background(), Request{Messages: []Message{UserText(prompt)}})
		if err != nil {
			t.Fatal(err)
		}
		for {
			_, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		stream.Close()
		if len(persisted.Parts) != 1 || persisted.Parts[0].ReasoningContent == "" || persisted.Parts[0].ReasoningItemID == "" {
			t.Fatalf("metadata missing: %#v", persisted)
		}
		kind := ReasoningKindSummary
		if prompt == "thinkraw" {
			kind = ReasoningKindRaw
		}
		if persisted.Parts[0].ReasoningKind != kind {
			t.Fatalf("kind=%s", persisted.Parts[0].ReasoningKind)
		}
	}
}
