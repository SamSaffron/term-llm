package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestSessionMessageEntriesProjectsSuccessfulSpawnAgentResult(t *testing.T) {
	content, err := json.Marshal(tools.SpawnAgentResult{
		AgentName: "reviewer", Output: "durable review", Duration: 42, SessionID: "child-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := []session.Message{
		*session.NewMessage("parent", llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
			Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "spawn-1", Name: tools.SpawnAgentToolName},
		}}}, 0),
		*session.NewMessage("parent", llm.ToolResultMessage("spawn-1", tools.SpawnAgentToolName, string(content), nil), 1),
	}
	store := newServeRuntimeTestStore()
	store.sessions["child-1"] = &session.Session{ID: "child-1", ParentID: "parent"}
	entries := (&serveServer{store: store}).sessionMessageEntries(messages)
	if len(entries) != 2 || len(entries[1].Parts) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	part := entries[1].Parts[0]
	if part.SpawnAgent == nil || part.SpawnAgent.Output != "durable review" || part.SpawnAgent.SessionID != "child-1" || part.ToolError {
		t.Fatalf("spawn projection = %#v", part)
	}
}

func TestSessionMessageEntriesOmitsUnvalidatedSpawnChildLinks(t *testing.T) {
	for _, tc := range []struct {
		name        string
		childID     string
		childParent string
	}{
		{name: "foreign child", childID: "foreign", childParent: "other"},
		{name: "unlinked child", childID: "unlinked", childParent: ""},
		{name: "self reference", childID: "parent", childParent: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, _ := json.Marshal(tools.SpawnAgentResult{Output: "safe output", SessionID: tc.childID})
			messages := []session.Message{
				*session.NewMessage("parent", llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "spawn", Name: tools.SpawnAgentToolName}}}}, 0),
				*session.NewMessage("parent", llm.ToolResultMessage("spawn", tools.SpawnAgentToolName, string(content), nil), 1),
			}
			store := newServeRuntimeTestStore()
			store.sessions[tc.childID] = &session.Session{ID: tc.childID, ParentID: tc.childParent}
			entries := (&serveServer{store: store}).sessionMessageEntries(messages)
			part := entries[1].Parts[0]
			if part.SpawnAgent == nil || part.SpawnAgent.Output != "safe output" || part.SpawnAgent.SessionID != "" {
				t.Fatalf("unvalidated link survived projection: %#v", part.SpawnAgent)
			}
		})
	}
}

func TestSessionMessageEntriesMarksStructuredSpawnErrorAndBoundsOutput(t *testing.T) {
	content, _ := json.Marshal(tools.SpawnAgentResult{Output: strings.Repeat("界", maxWebSpawnAgentOutputBytes/3+100), Error: strings.Repeat("error", 4000), Type: "timeout", SessionID: "child-2"})
	messages := []session.Message{*session.NewMessage("parent", llm.ToolResultMessage("spawn-2", tools.SpawnAgentToolName, string(content), nil), 0)}
	store := newServeRuntimeTestStore()
	store.sessions["child-2"] = &session.Session{ID: "child-2", ParentID: "parent"}
	entries := (&serveServer{store: store}).sessionMessageEntries(messages)
	part := entries[0].Parts[0]
	if part.SpawnAgent == nil || !part.ToolError || !strings.Contains(part.SpawnAgent.Error, "error truncated") {
		t.Fatalf("spawn error projection = %#v", part)
	}
	if len(part.SpawnAgent.Output) > maxWebSpawnAgentOutputBytes+32 || !strings.Contains(part.SpawnAgent.Output, "truncated") || !utf8.ValidString(part.SpawnAgent.Output) {
		t.Fatalf("bounded output length=%d suffix=%q", len(part.SpawnAgent.Output), part.SpawnAgent.Output[len(part.SpawnAgent.Output)-30:])
	}
}
