package serve

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session"
)

func TestWebHistoryPreservesThinkingAroundInlineTools(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	s := &serveServer{cfgRef: &config.Config{Reasoning: cfg}}
	msg := session.Message{Role: core.RoleAssistant, Parts: []core.Part{
		{Type: core.PartText, ReasoningContent: "before\n\nafter", ReasoningKind: core.ReasoningKindRaw, ReasoningReplayOnly: true,
			ReasoningDisplay: &core.ReasoningDisplay{Content: "before", Kind: core.ReasoningKindRaw}, CreatedAt: 1000},
		{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "read", Name: "read_file"}},
		{Type: core.PartText, ReasoningDisplay: &core.ReasoningDisplay{Content: "after", Kind: core.ReasoningKindRaw}, CreatedAt: 2000},
		{Type: core.PartText, Text: "answer", CreatedAt: 3000},
	}}
	// Session parts are JSON: verify the representation survives save/reload,
	// rather than only working with an in-memory engine message.
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var restored session.Message
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	entries := s.sessionMessageEntries([]session.Message{restored})
	if len(entries) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	var order []string
	for _, part := range entries[0].Parts {
		order = append(order, part.Type+":"+part.Text+part.ToolCallID)
	}
	if want := []string{"reasoning:before", "tool_call:read", "reasoning:after", "text:answer"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %q, want %q", order, want)
	}
	if entries[0].Parts[2].CreatedAt != 2000 {
		t.Fatal("post-tool thought lost its timestamp")
	}
}

func TestWebHistoryNeverDisplaysReplayOnlyThinking(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	s := &serveServer{cfgRef: &config.Config{Reasoning: cfg}}
	for _, kind := range []core.ReasoningKind{core.ReasoningKindSummary, core.ReasoningKindRaw} {
		entry := sessionMessageEntry{}
		part := core.Part{Type: core.PartText, Text: "answer", ReasoningReplayOnly: true, ReasoningContent: "aggregate", ReasoningKind: kind}
		appendSessionMessageTextWithReasoning(&entry, &session.Message{Role: core.RoleAssistant}, part, nil, "", s.cfgRef.ResolveReasoning("serve"))
		if len(entry.Parts) != 1 || entry.Parts[0].Type != "text" {
			t.Fatalf("%s exposed aggregate: %#v", kind, entry.Parts)
		}
	}
}
