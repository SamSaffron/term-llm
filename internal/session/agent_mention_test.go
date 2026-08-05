package session

import (
	"context"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestAgentMentionPartRoundTripKeepsProviderContextOffVisibleSurfaces(t *testing.T) {
	const visible = "ask @agent:codebase to inspect this"
	const hidden = "<term_llm_agent_mentions>hidden-delegation-sentinel</term_llm_agent_mentions>"
	input := llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		{Type: llm.PartText, Text: visible},
		{Type: llm.PartAgentMention, Text: hidden},
	}}
	stored := NewMessage("session", input, 0)
	if stored.TextContent != visible || strings.Contains(stored.ExtractTextContent(), "hidden-delegation-sentinel") {
		t.Fatalf("visible text leaked provider context: text=%q extracted=%q", stored.TextContent, stored.ExtractTextContent())
	}

	partsJSON, err := stored.PartsJSON()
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Message
	if err := roundTrip.SetPartsFromJSON(partsJSON); err != nil {
		t.Fatal(err)
	}
	roundTrip.TextContent = roundTrip.ExtractTextContent()
	if roundTrip.TextContent != visible || len(roundTrip.Parts) != 2 || roundTrip.Parts[1].Type != llm.PartAgentMention {
		t.Fatalf("session round trip = %#v", roundTrip)
	}
	providerMessage := roundTrip.ToLLMMessage()
	if got := llm.MessageText(providerMessage); !strings.Contains(got, visible) || !strings.Contains(got, hidden) {
		t.Fatalf("provider conversion lost delegation context: %q", got)
	}
	if providerMessage.Parts[1].Type != llm.PartText || roundTrip.Parts[1].Type != llm.PartAgentMention {
		t.Fatalf("provider conversion mutated structural storage: provider=%#v stored=%#v", providerMessage.Parts, roundTrip.Parts)
	}

	sess := &Session{ID: NewID(), Name: "agent mentions", Provider: "mock", Model: "mock", Mode: ModeChat}
	store, err := NewSQLiteStore(Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	stored.SessionID = sess.ID
	if err := store.AddMessage(ctx, sess.ID, stored); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil || len(loaded) != 1 || loaded[0].TextContent != visible || loaded[0].Parts[1].Type != llm.PartAgentMention {
		t.Fatalf("SQLite round trip = %#v, err=%v", loaded, err)
	}
	if results, err := store.Search(ctx, SearchOptions{Query: "hidden-delegation-sentinel", Limit: 10}); err != nil || len(results) != 0 {
		t.Fatalf("hidden provider context entered history search: %#v, err=%v", results, err)
	}
	if results, err := store.Search(ctx, SearchOptions{Query: "codebase", Limit: 10}); err != nil || len(results) != 1 {
		t.Fatalf("visible text missing from history search: %#v, err=%v", results, err)
	}

	markdown := ExportToMarkdown(sess, loaded, ExportOptions{})
	html, err := ExportToHTML(sess, loaded, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{"markdown": markdown, "html": html} {
		if strings.Contains(output, "hidden-delegation-sentinel") || !strings.Contains(output, "@agent:codebase") {
			t.Fatalf("%s export leaked hidden context or lost visible text: %q", name, output)
		}
	}
}
