package sidequestion

import (
	"context"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestPrepareContextSnapshotRemovesDanglingToolsAndAnchors(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleUser, CacheAnchor: true, Parts: []llm.Part{{Type: llm.PartText, Text: "completed"}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "complete", Name: "read"}}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "complete"}}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "partial"}, {Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "dangling", Name: "write"}}, {Type: llm.PartProviderReplay}}},
		{Role: llm.RoleEvent, Parts: []llm.Part{{Type: llm.PartText, Text: "ui only"}}},
	}
	got := PrepareContextSnapshot(messages)
	if len(got) != 4 {
		t.Fatalf("snapshot len = %d, want 4: %#v", len(got), got)
	}
	if got[0].CacheAnchor {
		t.Fatal("cache anchor was retained")
	}
	last := got[len(got)-1]
	if len(last.Parts) != 1 || last.Parts[0].Text != "partial" {
		t.Fatalf("dangling protocol was retained: %#v", last.Parts)
	}
	if !messages[0].CacheAnchor || len(messages[3].Parts) != 3 {
		t.Fatal("snapshot mutated source")
	}
}

func TestBuildMessagesRefreshesMainAndKeepsChronologicalHistory(t *testing.T) {
	got := BuildMessages([]llm.Message{llm.UserText("new main fact")}, []Entry{
		{Question: "q1", Response: "a1"},
		{Question: "q2", Response: "a2"},
	}, "q3")
	var texts []string
	for _, msg := range got {
		for _, part := range msg.Parts {
			if part.Type == llm.PartText {
				texts = append(texts, part.Text)
			}
		}
	}
	joined := strings.Join(texts, "|")
	for _, want := range []string{"new main fact", SystemPolicy, "q1|a1|q2|a2|q3"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("messages %q missing %q", joined, want)
		}
	}
}

func TestBuildMessagesPutsPolicyInLeadingDeveloperPosition(t *testing.T) {
	got := BuildMessages([]llm.Message{
		llm.SystemText("system"),
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "platform"}}},
		llm.UserText("main question"),
		llm.AssistantText("main answer"),
	}, []Entry{{Question: "side one", Response: "side answer"}}, "side two")
	wantRoles := []llm.Role{llm.RoleSystem, llm.RoleDeveloper, llm.RoleDeveloper, llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant, llm.RoleUser}
	wantTexts := []string{"system", "platform", SystemPolicy, "main question", "main answer", "side one", "side answer", "side two"}
	if len(got) != len(wantRoles) {
		t.Fatalf("messages = %#v", got)
	}
	for i := range got {
		if got[i].Role != wantRoles[i] || len(got[i].Parts) != 1 || got[i].Parts[0].Text != wantTexts[i] {
			t.Fatalf("message %d = %#v, want role=%s text=%q", i, got[i], wantRoles[i], wantTexts[i])
		}
	}
}

func TestPrepareContextSnapshotRejectsMalformedToolOrdering(t *testing.T) {
	messages := []llm.Message{
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "same"}}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "same", Name: "read"}}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "same"}}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "same", Name: "duplicate"}}}},
	}
	got := PrepareContextSnapshot(messages)
	if len(got) != 2 || got[0].Parts[0].ToolCall == nil || got[1].Parts[0].ToolResult == nil {
		t.Fatalf("malformed ordering was globally matched: %#v", got)
	}
}

func TestAppendHistoryCapsAtTwenty(t *testing.T) {
	var history []Entry
	for i := 0; i < 25; i++ {
		history = AppendHistory(history, Entry{Question: string(rune('a' + i)), Response: "ok"})
	}
	if len(history) != HistoryLimit || history[0].Question != "f" {
		t.Fatalf("history = len %d first %q", len(history), history[0].Question)
	}
}

func TestRunDisablesCapabilitiesAndRejectsToolCall(t *testing.T) {
	provider := llm.NewMockProvider("mock").AddToolCall("call-1", "danger", map[string]any{"path": "/tmp/x"})
	result, err := Run(context.Background(), provider, llm.Request{
		Search:   true,
		Tools:    []llm.ToolSpec{{Name: "danger"}},
		Messages: []llm.Message{llm.UserText("do it")},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Synthetic || result.Response != ToolAttemptResponse {
		t.Fatalf("result = %#v", result)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("requests = %d", len(provider.Requests))
	}
	req := provider.Requests[0]
	if !req.Ephemeral || req.Search || len(req.Tools) != 0 || req.MaxTurns != 1 || req.SessionID != "" {
		t.Fatalf("unsafe request: %#v", req)
	}
	if req.Responses == nil || !req.Responses.MultiAgent.EnabledSet || req.Responses.MultiAgent.Enabled || !req.Responses.ProgrammaticToolCalling.EnabledSet || req.Responses.ProgrammaticToolCalling.Enabled {
		t.Fatalf("native controls not disabled: %#v", req.Responses)
	}
}
