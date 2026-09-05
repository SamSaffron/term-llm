package sidequestion

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestSystemPolicyRequestsUsefulMarkdownWithoutForcedVerbosity(t *testing.T) {
	for _, want := range []string{
		"clear Markdown structure",
		"headings, bullets, tables, or code blocks",
		"enough detail",
		"simple question",
		"one response",
		"no tools",
		"cannot inspect files, run commands, search, delegate, or take actions",
	} {
		if !strings.Contains(SystemPolicy, want) {
			t.Fatalf("SystemPolicy missing %q:\n%s", want, SystemPolicy)
		}
	}
}

func TestPrepareContextSnapshotRemovesDanglingToolsAndPreservesAnchors(t *testing.T) {
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
	if !got[0].CacheAnchor {
		t.Fatal("cache anchor was not retained")
	}
	last := got[len(got)-1]
	if len(last.Parts) != 1 || last.Parts[0].Text != "partial" {
		t.Fatalf("dangling protocol was retained: %#v", last.Parts)
	}
	if !messages[0].CacheAnchor || len(messages[3].Parts) != 3 {
		t.Fatal("snapshot mutated source")
	}
}

func TestCloneMessagesDeepCopiesMutablePartMetadata(t *testing.T) {
	messages := []llm.Message{{Role: llm.RoleAssistant, Parts: []llm.Part{{
		Type:                  llm.PartProviderReplay,
		ReasoningSummaryParts: []string{"summary"},
		ProviderReplay:        &llm.ProviderReplayItem{Raw: []byte(`{"id":"one"}`)},
		ToolCall:              &llm.ToolCall{ID: "call", Arguments: []byte(`{"value":1}`), ThoughtSig: []byte("sig")},
		ToolResult: &llm.ToolResult{ID: "call", ContentParts: []llm.ToolContentPart{{
			Type: llm.ToolContentPartImageData, ImageData: &llm.ToolImageData{Base64: "image"},
		}}, Images: []string{"path"}, ThoughtSig: []byte("result-sig")},
	}}}}
	cloned := CloneMessages(messages)
	cloned[0].Parts[0].ReasoningSummaryParts[0] = "changed"
	cloned[0].Parts[0].ProviderReplay.Raw[0] = 'x'
	cloned[0].Parts[0].ToolCall.Arguments[0] = 'x'
	cloned[0].Parts[0].ToolCall.ThoughtSig[0] = 'x'
	cloned[0].Parts[0].ToolResult.ContentParts[0].ImageData.Base64 = "changed"
	cloned[0].Parts[0].ToolResult.Images[0] = "changed"
	cloned[0].Parts[0].ToolResult.ThoughtSig[0] = 'x'

	part := messages[0].Parts[0]
	if part.ReasoningSummaryParts[0] != "summary" || part.ProviderReplay.Raw[0] == 'x' || part.ToolCall.Arguments[0] == 'x' ||
		part.ToolCall.ThoughtSig[0] == 'x' || part.ToolResult.ContentParts[0].ImageData.Base64 != "image" ||
		part.ToolResult.Images[0] != "path" || part.ToolResult.ThoughtSig[0] == 'x' {
		t.Fatalf("clone mutated source metadata: %#v", part)
	}
}

func TestBuildMessagesRefreshesMainAndKeepsChronologicalHistory(t *testing.T) {
	got, err := buildMessagesWithinBudget([]llm.Message{llm.UserText("new main fact")}, []Entry{
		{Question: "q1", Response: "a1"},
		{Question: "q2", Response: "a2"},
	}, "q3", 10_000)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, msg := range got {
		for _, part := range msg.Parts {
			if part.Type == llm.PartText {
				texts = append(texts, part.Text)
			}
		}
	}
	joined := strings.Join(texts, "|")
	for _, want := range []string{"new main fact", SystemPolicy, "q1", "a1", "q2", "a2", "q3"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("messages %q missing %q", joined, want)
		}
	}
}

func TestBuildMessagesPreservesMainPrefixBeforeSidePolicy(t *testing.T) {
	got, err := buildMessagesWithinBudget([]llm.Message{
		llm.SystemText("system"),
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "platform"}}},
		llm.UserText("main question"),
		llm.AssistantText("main answer"),
	}, []Entry{{Question: "side one", Response: "side answer"}}, "side two", 10_000)
	if err != nil {
		t.Fatal(err)
	}
	wantRoles := []llm.Role{llm.RoleSystem, llm.RoleDeveloper, llm.RoleUser, llm.RoleAssistant, llm.RoleUser, llm.RoleAssistant, llm.RoleDeveloper, llm.RoleUser}
	wantTexts := []string{"system", "platform", "main question", "main answer", "side one", "side answer", SystemPolicy, "side two"}
	if len(got) != len(wantRoles) {
		t.Fatalf("messages = %#v", got)
	}
	for i := range got {
		if got[i].Role != wantRoles[i] || len(got[i].Parts) != 1 || got[i].Parts[0].Text != wantTexts[i] {
			t.Fatalf("message %d = %#v, want role=%s text=%q", i, got[i], wantRoles[i], wantTexts[i])
		}
	}
}

func TestBuildMessagesUsesRuntimeInputLimit(t *testing.T) {
	got, err := BuildMessages([]llm.Message{llm.UserText(strings.Repeat("main ", 2000))}, nil, "side", "unknown", "unknown", 1_000)
	if err != nil {
		t.Fatal(err)
	}
	if tokens := estimateSideMessageTokens(got); tokens > 800 {
		t.Fatalf("tokens = %d, want runtime-derived budget <= 800", tokens)
	}
}

func TestBuildMessagesDropsOldSideHistoryBeforeMainPrefix(t *testing.T) {
	snapshot := []llm.Message{llm.SystemText("system"), llm.UserText("main question"), llm.AssistantText("main answer")}
	history := []Entry{
		{Question: strings.Repeat("old question ", 30), Response: strings.Repeat("old answer ", 30)},
		{Question: "recent side question", Response: "recent side answer"},
	}
	required := []llm.Message{
		llm.UserText(history[1].Question), llm.AssistantText(history[1].Response),
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: SystemPolicy}}},
		llm.UserText("current question"),
	}
	budget := estimateSideMessageTokens(append(CloneMessages(snapshot), required...))
	got, err := buildMessagesWithinBudget(snapshot, history, "current question", budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < len(snapshot) || !reflect.DeepEqual(got[:len(snapshot)], snapshot) {
		t.Fatalf("main cache prefix changed: %#v", got)
	}
	joined := messageText(got)
	if strings.Contains(joined, "old question") || !strings.Contains(joined, "recent side question") {
		t.Fatalf("wrong side history retained: %q", joined)
	}
	if tokens := estimateSideMessageTokens(got); tokens > budget {
		t.Fatalf("tokens = %d, budget = %d", tokens, budget)
	}
}

func TestBuildMessagesTrimsMainAtCompleteUserTurnBoundary(t *testing.T) {
	call := &llm.ToolCall{ID: "read-1", Name: "read"}
	result := &llm.ToolResult{ID: "read-1", Name: "read", Content: "result"}
	snapshot := []llm.Message{
		llm.SystemText("system"),
		llm.UserText(strings.Repeat("old turn ", 80)),
		llm.AssistantText(strings.Repeat("old response ", 80)),
		llm.UserText("recent turn"),
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: call}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: result}}},
		llm.AssistantText("recent answer"),
	}
	required := []llm.Message{
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: SystemPolicy}}},
		llm.UserText("why?"),
	}
	budget := estimateSideMessageTokens(append(append([]llm.Message{snapshot[0]}, snapshot[3:]...), required...))
	got, err := buildMessagesWithinBudget(snapshot, nil, "why?", budget)
	if err != nil {
		t.Fatal(err)
	}
	joined := messageText(got)
	if strings.Contains(joined, "old turn") || !strings.Contains(joined, "recent turn") {
		t.Fatalf("wrong main turn retained: %q", joined)
	}
	if len(got) < 6 || got[0].Role != llm.RoleSystem || got[1].Role != llm.RoleUser {
		t.Fatalf("invalid retained boundary: %#v", got)
	}
	var calls, results int
	for _, msg := range got {
		for _, part := range msg.Parts {
			if part.ToolCall != nil {
				calls++
			}
			if part.ToolResult != nil {
				results++
			}
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("tool cycle split: calls=%d results=%d messages=%#v", calls, results, got)
	}
	if tokens := estimateSideMessageTokens(got); tokens > budget {
		t.Fatalf("tokens = %d, budget = %d", tokens, budget)
	}
}

func TestBuildMessagesPreservesCompactionAnchorWhenItFits(t *testing.T) {
	anchor := llm.Message{Role: llm.RoleUser, CacheAnchor: true, Parts: []llm.Part{{Type: llm.PartText, Text: "compaction summary"}}}
	snapshot := []llm.Message{
		llm.SystemText("system"), anchor, llm.AssistantText("summary acknowledged"),
		llm.UserText(strings.Repeat("old ", 100)), llm.AssistantText(strings.Repeat("answer ", 100)),
		llm.UserText("recent"), llm.AssistantText("current"),
	}
	required := []llm.Message{
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: SystemPolicy}}}, llm.UserText("side"),
	}
	budget := estimateSideMessageTokens(append(append(CloneMessages(snapshot[:3]), snapshot[5:]...), required...))
	got, err := buildMessagesWithinBudget(snapshot, nil, "side", budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 5 || !got[1].CacheAnchor || got[1].Parts[0].Text != "compaction summary" {
		t.Fatalf("compaction anchor lost: %#v", got)
	}
}

func TestBuildMessagesTruncatesOversizedQuestionToBudget(t *testing.T) {
	budget := estimateSideMessageTokens([]llm.Message{
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: SystemPolicy}}},
		llm.UserText(""),
	}) + 20
	got, err := buildMessagesWithinBudget(nil, nil, strings.Repeat("question ", 200), budget)
	if err != nil {
		t.Fatal(err)
	}
	if tokens := estimateSideMessageTokens(got); tokens > budget {
		t.Fatalf("tokens = %d, budget = %d", tokens, budget)
	}
	if len(got) != 2 || got[1].Role != llm.RoleUser || strings.TrimSpace(got[1].Parts[0].Text) == "" {
		t.Fatalf("required question missing: %#v", got)
	}
}

func messageText(messages []llm.Message) string {
	var texts []string
	for _, msg := range messages {
		for _, part := range msg.Parts {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "|")
}

func TestBuildMessagesResanitizesSteeredToolCycleAfterTrim(t *testing.T) {
	call := &llm.ToolCall{ID: "call-1", Name: "read"}
	result := &llm.ToolResult{ID: "call-1", Name: "read", Content: "tool result"}
	snapshot := []llm.Message{
		llm.UserText(strings.Repeat("large old turn ", 100)),
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: call}}},
		llm.UserText("steering"),
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: result}}},
		llm.AssistantText("answer after tool"),
	}
	required := []llm.Message{
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: SystemPolicy}}},
		llm.UserText("side question"),
	}
	budget := estimateSideMessageTokens(append([]llm.Message{snapshot[2], snapshot[4]}, required...))
	got, err := buildMessagesWithinBudget(snapshot, nil, "side question", budget)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range got {
		for _, part := range msg.Parts {
			if part.ToolResult != nil {
				t.Fatalf("trim retained orphaned tool result: %#v", got)
			}
		}
	}
}

func TestBuildMessagesTruncatesOversizedRecentTurnBeforeDroppingIt(t *testing.T) {
	snapshot := []llm.Message{
		llm.UserText("inspect the output"),
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-1", Name: "read"}}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call-1", Name: "read", Content: strings.Repeat("large result ", 500)}}}},
		llm.AssistantText("the useful conclusion"),
	}
	budget := estimateSideMessageTokens([]llm.Message{
		llm.UserText("inspect the output"),
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: snapshot[1].Parts[0].ToolCall}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call-1", Name: "read", Content: "short"}}}},
		llm.AssistantText("the useful conclusion"),
		{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: SystemPolicy}}},
		llm.UserText("what matters?"),
	}) + 20
	got, err := buildMessagesWithinBudget(snapshot, nil, "what matters?", budget)
	if err != nil {
		t.Fatal(err)
	}
	joined := messageText(got)
	if !strings.Contains(joined, "inspect the output") || !strings.Contains(joined, "the useful conclusion") {
		t.Fatalf("oversized recent turn was discarded instead of truncated: %q", joined)
	}
	if tokens := estimateSideMessageTokens(got); tokens > budget {
		t.Fatalf("tokens = %d, budget = %d", tokens, budget)
	}
}

func TestTruncateTextToTokensPreservesValidUTF8(t *testing.T) {
	got := truncateTextToTokens("日本語の質問です", 2)
	if !utf8.ValidString(got) || got == "" {
		t.Fatalf("truncated text = %q, want non-empty valid UTF-8", got)
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
