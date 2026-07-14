package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"", 0},
		{"hi", 1},
		{"hello world", 3},          // 11 bytes → (11+3)/4 = 3
		{strings.Repeat("a", 4), 1}, // exactly 4 bytes → 1 token
		{strings.Repeat("a", 5), 2}, // 5 bytes → (5+3)/4 = 2
		{strings.Repeat("a", 8), 2}, // 8 bytes → 2 tokens
		{strings.Repeat("a", 100), 25},
	}

	for _, tt := range tests {
		got := EstimateTokens(tt.input)
		if got != tt.expected {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tt.input, got, tt.expected)
		}
	}
}

func TestEstimateMessageTokens(t *testing.T) {
	msgs := []Message{
		UserText("hello world"),        // 11 bytes → 3 tokens
		AssistantText("goodbye world"), // 13 bytes → 4 tokens (rounded up)
	}
	got := EstimateMessageTokens(msgs)
	// 11 bytes → (11+3)/4 = 3, 13 bytes → (13+3)/4 = 4
	if got != 7 {
		t.Errorf("EstimateMessageTokens = %d, want 7", got)
	}
}

func TestEstimateMessageTokensWithToolParts(t *testing.T) {
	// Tool call arguments should be counted
	msgs := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{{
				Type:     PartToolCall,
				ToolCall: &ToolCall{Name: "read", Arguments: []byte(`{"path":"/foo"}`)},
			}},
		},
		{
			Role: RoleTool,
			Parts: []Part{{
				Type:       PartToolResult,
				ToolResult: &ToolResult{Name: "read", Content: strings.Repeat("x", 40)},
			}},
		},
	}
	got := EstimateMessageTokens(msgs)
	// args: 15 bytes → 4 tokens, content: 40 bytes → 10 tokens
	want := EstimateTokens(`{"path":"/foo"}`) + EstimateTokens(strings.Repeat("x", 40))
	if got != want {
		t.Errorf("EstimateMessageTokens with tools = %d, want %d", got, want)
	}
}

func TestEstimateMessageTokensEmpty(t *testing.T) {
	if got := EstimateMessageTokens(nil); got != 0 {
		t.Errorf("EstimateMessageTokens(nil) = %d, want 0", got)
	}
	if got := EstimateMessageTokens([]Message{}); got != 0 {
		t.Errorf("EstimateMessageTokens([]) = %d, want 0", got)
	}
}

func TestEstimateMessageTokensSkipsEventMessages(t *testing.T) {
	msgs := []Message{
		UserText("hello world"),
		{Role: RoleEvent, Parts: []Part{{Type: PartText, Text: strings.Repeat("x", 100)}}},
		AssistantText("goodbye world"),
	}

	got := EstimateMessageTokens(msgs)
	want := EstimateMessageTokens([]Message{msgs[0], msgs[2]})
	if got != want {
		t.Errorf("EstimateMessageTokens should ignore event messages, got %d want %d", got, want)
	}
}

func TestEstimateMessageTokensCountsReasoningReplay(t *testing.T) {
	msg := Message{Role: RoleAssistant, Parts: []Part{{
		Type:                      PartText,
		Text:                      "visible",
		ReasoningContent:          strings.Repeat("r", 40),
		ReasoningSummaryParts:     []string{strings.Repeat("s", 40)},
		ReasoningEncryptedContent: strings.Repeat("e", 40),
	}}}

	got := EstimateMessageTokens([]Message{msg})
	visibleOnly := EstimateTokens("visible")
	if got <= visibleOnly {
		t.Fatalf("EstimateMessageTokens = %d, want reasoning replay counted beyond visible-only %d", got, visibleOnly)
	}
}

func TestReconstructHistory(t *testing.T) {
	recentUser := []Message{UserText("recent question")}

	result := reconstructHistory("system prompt", "summary of conversation", recentUser)

	// Should be: system + summary + assistant ack + recent user
	if len(result) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(result))
	}

	if result[0].Role != RoleSystem {
		t.Errorf("first message should be system, got %s", result[0].Role)
	}
	if result[0].Parts[0].Text != "system prompt" {
		t.Errorf("system prompt mismatch")
	}

	if result[1].Role != RoleUser {
		t.Errorf("second message should be user (summary), got %s", result[1].Role)
	}
	if !strings.Contains(result[1].Parts[0].Text, "summary of conversation") {
		t.Errorf("summary message should contain summary text")
	}
	if !strings.Contains(result[1].Parts[0].Text, summaryPrefix) {
		t.Errorf("summary message should contain prefix")
	}
	if !result[1].CacheAnchor {
		t.Errorf("summary message should have CacheAnchor=true for stable cache breakpoint")
	}

	if result[2].Role != RoleAssistant {
		t.Errorf("third message should be assistant ack, got %s", result[2].Role)
	}

	if result[3].Role != RoleUser {
		t.Errorf("fourth message should be user, got %s", result[3].Role)
	}
	if result[3].Parts[0].Text != "recent question" {
		t.Errorf("recent user message mismatch")
	}
}

func TestReconstructHistoryNoSystem(t *testing.T) {
	result := reconstructHistory("", "summary", []Message{UserText("q")})

	// Without system prompt: summary + ack + user = 3
	if len(result) != 3 {
		t.Fatalf("expected 3 messages without system prompt, got %d", len(result))
	}
	if result[0].Role != RoleUser {
		t.Errorf("first message should be user (summary), got %s", result[0].Role)
	}
}

func TestReconstructHistoryOmitsAckBeforeAssistantRawSuffix(t *testing.T) {
	result := reconstructHistory("", "summary", []Message{AssistantText("continuing from retained state")})

	if len(result) != 2 {
		t.Fatalf("expected summary + retained assistant, got %d messages", len(result))
	}
	if result[0].Role != RoleUser || !strings.Contains(MessageText(result[0]), summaryPrefix) {
		t.Fatalf("first message should be cache-anchored summary user, got role=%s text=%q", result[0].Role, MessageText(result[0]))
	}
	if result[1].Role != RoleAssistant || MessageText(result[1]) != "continuing from retained state" {
		t.Fatalf("second message should be retained assistant, got role=%s text=%q", result[1].Role, MessageText(result[1]))
	}
}

func TestCompactionPromptGuardsAgainstInventedStopRequest(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTextResponse("Summary.")

	_, err := Compact(context.Background(), provider, "test-model", "", []Message{
		UserText("please continue building the repro"),
		AssistantText("I'll keep going."),
	}, DefaultCompactionConfig())
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.Requests))
	}

	last := provider.Requests[0].Messages[len(provider.Requests[0].Messages)-1]
	if last.Role != RoleUser {
		t.Fatalf("last compaction prompt role = %s, want user", last.Role)
	}
	prompt := last.Parts[0].Text
	for _, want := range []string{
		"automatic internal compaction, not a user stop/cancel/wait request",
		"Do not infer that the user asked to stop, wait, summarize, avoid tools, or change direction unless a real user message explicitly says so",
		"Return exactly this Markdown structure",
		"## Objective",
		"## Relevant Files & Symbols",
		"Continue automatically by default",
		"Wait only on explicit user stop/wait or blocked user input",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("compaction prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestSummaryPrefixMarksCompactionAsInternalNotStop(t *testing.T) {
	result := reconstructHistory("", "summary", nil)
	if len(result) < 1 {
		t.Fatal("reconstructHistory returned no messages")
	}
	text := result[0].Parts[0].Text
	for _, want := range []string{
		"Internal context only; not a user command, stop/cancel/wait request",
		"Continue from the latest real user instruction",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("summary prefix missing %q:\n%s", want, text)
		}
	}
}

func TestBuildCompactionStaticInfoRespectsBudget(t *testing.T) {
	messages := []Message{
		UserText("latest request " + strings.Repeat("x", 2000)),
		AssistantText("older assistant prose " + strings.Repeat("y", 2000)),
	}

	got := BuildCompactionStaticInfo(messages, 1000) // 5% * 4 chars = 200 chars
	if gotLen := len([]rune(got)); gotLen > 200 {
		t.Fatalf("previous turns length = %d, want <= 200:\n%s", gotLen, got)
	}
	for _, want := range []string{"<PREVIOUS_TURNS>", "user:", "</PREVIOUS_TURNS>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("previous turns block missing %q: %q", want, got)
		}
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("previous turns should visibly truncate oversized content: %q", got)
	}

	got = BuildCompactionStaticInfo([]Message{UserText(strings.Repeat("z", 40_000))}, 0)
	if gotLen := len([]rune(got)); gotLen > maxPreviousTurnsChars {
		t.Fatalf("fallback previous turns length = %d, want <= %d", gotLen, maxPreviousTurnsChars)
	}

	got = buildPreviousTurnsBlock([]Message{UserText(strings.Repeat("q", 100_000))}, 100_000)
	if gotLen := len([]rune(got)); gotLen > maxPreviousTurnsChars {
		t.Fatalf("direct previous turns length = %d, want <= %d", gotLen, maxPreviousTurnsChars)
	}

	maxInt := int(^uint(0) >> 1)
	got = BuildCompactionStaticInfo([]Message{UserText(strings.Repeat("w", 100_000))}, maxInt)
	if gotLen := len([]rune(got)); gotLen > maxPreviousTurnsChars {
		t.Fatalf("large-limit previous turns length = %d, want <= %d", gotLen, maxPreviousTurnsChars)
	}
}

func TestBuildCompactionStaticInfoKeepsNewestTurnsWhenBudgetTight(t *testing.T) {
	messages := []Message{
		UserText("old important signal: internal/llm/old.go TestOld failed with error OLD_SIGNAL"),
		AssistantText("older assistant prose that should be safe to drop"),
		UserText("latest user: reverse prev turns."),
		AssistantText("most recent state: editing compaction.go now."),
	}

	got := BuildCompactionStaticInfo(messages, 800) // 5% * 4 chars = 160 chars
	if gotLen := len([]rune(got)); gotLen > 160 {
		t.Fatalf("previous turns length = %d, want <= 160:\n%s", gotLen, got)
	}
	for _, want := range []string{"latest user: reverse prev turns", "most recent state"} {
		if !strings.Contains(got, want) {
			t.Fatalf("previous turns missing recent context %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "OLD_SIGNAL") || strings.Contains(got, "older assistant prose") {
		t.Fatalf("previous turns kept older context before newer context:\n%s", got)
	}
}

func TestBuildCompactionStaticInfoPerEntryCapsKeepBudgetForImportantContext(t *testing.T) {
	messages := []Message{
		UserText("older but important: internal/llm/engine.go TestRunLoop failed with error boom"),
		AssistantText(strings.Repeat("routine assistant prose ", 2000)),
		UserText("latest user says continue " + strings.Repeat("A", 2000)),
	}

	got := BuildCompactionStaticInfo(messages, 5000) // 1000-char budget; latest user cap leaves room for older signals
	if gotLen := len([]rune(got)); gotLen > 1000 {
		t.Fatalf("previous turns length = %d, want <= 1000", gotLen)
	}
	for _, want := range []string{"<PREVIOUS_TURNS>", "user:", "latest user says continue", "internal/llm/engine.go", "TestRunLoop", "error boom"} {
		if !strings.Contains(got, want) {
			t.Fatalf("previous turns missing %q:\n%s", want, got)
		}
	}
}

func TestBuildCompactionStaticInfoPrioritizesSignalsAndOmitsBulkToolOutput(t *testing.T) {
	messages := []Message{
		AssistantText(strings.Repeat("low value prose ", 1000)),
		UserText("Please keep changes minimal. Failing test: TestSoftCompaction in internal/llm/engine_test.go. Error: expected no compactionPrompt."),
		{
			Role: RoleAssistant,
			Parts: []Part{
				{Type: PartText, Text: "I'll inspect the test and update the compaction code."},
				{Type: PartToolCall, ToolCall: &ToolCall{Name: "read_file", Arguments: json.RawMessage(`{"path":"internal/llm/engine_test.go"}`)}},
				{Type: PartToolCall, ToolCall: &ToolCall{Name: "write_file", Arguments: json.RawMessage(`{"path":"internal/llm/compaction.go","content":"large replacement"}`)}},
			},
		},
		{
			Role: RoleTool,
			Parts: []Part{{
				Type:       PartToolResult,
				ToolResult: &ToolResult{ID: "tool-1", Name: "shell", Content: strings.Repeat("ordinary output line\n", 1000)},
			}},
		},
		AssistantText("Decision: use the brief directly. Next action: update tests."),
	}

	got := BuildCompactionStaticInfo(messages, 10000)
	for _, want := range []string{"<PREVIOUS_TURNS>", "user:", "Please keep changes minimal", "TestSoftCompaction", "internal/llm/engine_test.go", "expected no compactionPrompt", "assistant:", "tool calls:", "read_file path=internal/llm/engine_test.go", "write_file path=internal/llm/compaction.go", "tool results:", "Decision: use the brief directly", "Next action: update tests", "bulk output omitted"} {
		if !strings.Contains(got, want) {
			t.Fatalf("previous turns missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "ordinary output line") > 2 {
		t.Fatalf("previous turns included bulk tool output:\n%s", got)
	}
	if got2 := BuildCompactionStaticInfo(messages, 10000); got2 != got {
		t.Fatalf("previous turns block is not deterministic:\nfirst=%q\nsecond=%q", got, got2)
	}
}

func TestBuildCompactionStaticInfoExtractsPriorCompactionSummary(t *testing.T) {
	prior := summaryPrefix + `<PREVIOUS_TURNS>
user: stale raw request that should not be nested
assistant:
  **Considering test modifications**
tool results:
  - read_file completed: 123: noisy line-numbered output
</PREVIOUS_TURNS>

<SUMMARY_AND_NEXT_ACTIONS>
Objective: improve compaction formatting.
Current state: tests passed after editing internal/llm/compaction.go.
Next action: review the final diff.
</SUMMARY_AND_NEXT_ACTIONS>`
	messages := []Message{
		UserText(prior),
		UserText("latest real user request: ok improve it then"),
	}

	got := BuildCompactionStaticInfo(messages, 20000)
	if strings.Count(got, "<PREVIOUS_TURNS>") != 1 || strings.Count(got, "</PREVIOUS_TURNS>") != 1 {
		t.Fatalf("previous turns should not nest old previous-turn tags:\n%s", got)
	}
	for _, want := range []string{"prior compaction summary:", "Objective: improve compaction formatting", "Next action: review the final diff", "user: latest real user request: ok improve it then"} {
		if !strings.Contains(got, want) {
			t.Fatalf("previous turns missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"stale raw request", "Considering test modifications", "line-numbered output", "<SUMMARY_AND_NEXT_ACTIONS>"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("previous turns included stale/nested content %q:\n%s", unwanted, got)
		}
	}
}

func TestBuildCompactionStaticInfoOmitsAssistantReasoningSummaries(t *testing.T) {
	messages := []Message{
		UserText("please continue"),
		{Role: RoleAssistant, Parts: []Part{{
			Type:             PartText,
			Text:             "Implemented the requested change.",
			ReasoningContent: "Considering test modifications that should stay out of the previous-turn transcript.",
			ReasoningKind:    ReasoningKindSummary,
		}}},
	}

	got := BuildCompactionStaticInfo(messages, 10000)
	if !strings.Contains(got, "assistant: Implemented the requested change.") {
		t.Fatalf("previous turns missing visible assistant text:\n%s", got)
	}
	if strings.Contains(got, "Considering test modifications") {
		t.Fatalf("previous turns included assistant reasoning summary:\n%s", got)
	}
}

func TestBuildCompactionStaticInfoSuppressesReadFileToolResultBody(t *testing.T) {
	messages := []Message{
		UserText("inspect the compaction file"),
		{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{Name: "read_file", Arguments: json.RawMessage(`{"path":"internal/llm/compaction.go"}`)}}}},
		{Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{Name: "read_file", Content: "1: package llm\n2: important implementation detail\n3: more line-numbered source"}}}},
	}

	got := BuildCompactionStaticInfo(messages, 10000)
	for _, want := range []string{"tool calls:", "read_file path=internal/llm/compaction.go", "tool results:", "read_file completed: bulk output omitted"} {
		if !strings.Contains(got, want) {
			t.Fatalf("previous turns missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "1: package llm") || strings.Contains(got, "line-numbered source") {
		t.Fatalf("previous turns included raw read_file body:\n%s", got)
	}
}

func TestSoftCompactUsesBriefPromptAndReportsUsage(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTurn(MockTurn{Text: "continue by editing internal/llm/engine.go", Usage: Usage{InputTokens: 12, OutputTokens: 7}})

	result, err := SoftCompact(context.Background(), provider, "test-model", "system", []Message{
		UserText("please continue"),
		AssistantText("working"),
	}, DefaultCompactionConfig())
	if err != nil {
		t.Fatalf("SoftCompact failed: %v", err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.Requests))
	}
	last := provider.Requests[0].Messages[len(provider.Requests[0].Messages)-1]
	prompt := MessageText(last)
	if !strings.Contains(prompt, "compact continuation brief") || !strings.Contains(prompt, "## Objective") || !strings.Contains(prompt, "## Relevant Files & Symbols") || strings.Contains(prompt, "Create a detailed summary") {
		t.Fatalf("soft compaction prompt mismatch:\n%s", prompt)
	}
	if result.Model != "test-model" {
		t.Fatalf("result model = %q, want test-model", result.Model)
	}
	if result.Usage.InputTokens != 12 || result.Usage.OutputTokens != 7 {
		t.Fatalf("result usage = %+v, want 12/7", result.Usage)
	}
	if !strings.Contains(result.Summary, "<PREVIOUS_TURNS>") || !strings.Contains(result.Summary, "<SUMMARY_AND_NEXT_ACTIONS>") || !strings.Contains(result.Summary, "continue by editing internal/llm/engine.go") {
		t.Fatalf("soft compact summary missing previous turns/summary sections:\n%s", result.Summary)
	}
	if strings.Index(result.Summary, "<PREVIOUS_TURNS>") > strings.Index(result.Summary, "<SUMMARY_AND_NEXT_ACTIONS>") {
		t.Fatalf("summary/next actions should appear after previous turns:\n%s", result.Summary)
	}
}

func TestSoftCompactFallsBackToHardWhenBriefEmpty(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTurn(MockTurn{Usage: Usage{InputTokens: 5, OutputTokens: 1}})
	provider.AddTurn(MockTurn{Text: "hard fallback summary", Usage: Usage{InputTokens: 11, OutputTokens: 3}})

	result, err := SoftCompact(context.Background(), provider, "test-model", "system", []Message{
		UserText("please continue"),
		AssistantText("working"),
	}, DefaultCompactionConfig())
	if err != nil {
		t.Fatalf("SoftCompact fallback failed: %v", err)
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("provider requests = %d, want soft brief + hard fallback", len(provider.Requests))
	}
	firstLast := MessageText(provider.Requests[0].Messages[len(provider.Requests[0].Messages)-1])
	if !strings.Contains(firstLast, "compact continuation brief") {
		t.Fatalf("first request did not use brief prompt: %q", firstLast)
	}
	secondLast := MessageText(provider.Requests[1].Messages[len(provider.Requests[1].Messages)-1])
	if !strings.Contains(secondLast, "Create a compact continuation brief") || !strings.Contains(secondLast, "## Objective") {
		t.Fatalf("second request did not use structured hard compaction prompt: %q", secondLast)
	}
	if !strings.Contains(result.Summary, "<PREVIOUS_TURNS>") || !strings.Contains(result.Summary, "<SUMMARY_AND_NEXT_ACTIONS>") || !strings.Contains(result.Summary, "hard fallback summary") {
		t.Fatalf("summary missing structured hard fallback sections/content:\n%s", result.Summary)
	}
	if result.Usage.InputTokens != 16 || result.Usage.OutputTokens != 4 {
		t.Fatalf("usage = %+v, want soft+hard fallback usage 16/4", result.Usage)
	}
}

func TestCompactRetainsRawRecentSuffixOutsideSummaryHelper(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTextResponse("older prefix summary")

	oldAssistant := Message{Role: RoleAssistant, Parts: []Part{{
		Type:                      PartText,
		Text:                      "older assistant state",
		ReasoningContent:          "old hidden reasoning",
		ReasoningSummaryParts:     []string{"old reasoning summary"},
		ReasoningItemID:           "old-reasoning-id",
		ReasoningEncryptedContent: "old-encrypted-reasoning",
		ReasoningKind:             ReasoningKindRaw,
		ReasoningSummaryTitle:     "old title",
	}}}
	messages := []Message{
		UserText("older user goal"),
		oldAssistant,
		UserText("RECENT exact user request"),
		{Role: RoleAssistant, Parts: []Part{{
			Type:                      PartText,
			Text:                      "RECENT exact assistant state",
			ReasoningItemID:           "recent-reasoning-id",
			ReasoningEncryptedContent: "recent-encrypted-reasoning",
			ReasoningKind:             ReasoningKindEncrypted,
		}}},
	}
	config := DefaultCompactionConfig()
	config.RecentRawTurns = 1
	config.RecentRawTokenBudget = 2_000

	result, err := Compact(context.Background(), provider, "test-model", "", messages, config)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.Requests))
	}

	helperText := requestMessagesText(provider.Requests[0].Messages)
	for _, want := range []string{"older user goal", "older assistant state"} {
		if !strings.Contains(helperText, want) {
			t.Fatalf("helper request missing summarized prefix %q:\n%s", want, helperText)
		}
	}
	for _, unwanted := range []string{"RECENT exact user request", "RECENT exact assistant state", "old hidden reasoning", "old-encrypted-reasoning", "old reasoning summary"} {
		if strings.Contains(helperText, unwanted) {
			t.Fatalf("helper request included retained/raw or reasoning-only content %q:\n%s", unwanted, helperText)
		}
	}

	oldAssistantFound := false
	for _, msg := range provider.Requests[0].Messages {
		for _, part := range msg.Parts {
			if part.Text != "older assistant state" {
				continue
			}
			oldAssistantFound = true
			if part.ReasoningContent != "" || len(part.ReasoningSummaryParts) != 0 || part.ReasoningItemID != "" || part.ReasoningEncryptedContent != "" || part.ReasoningKind != "" || part.ReasoningSummaryTitle != "" {
				t.Fatalf("summary helper retained reasoning metadata: %+v", part)
			}
		}
	}
	if !oldAssistantFound {
		t.Fatalf("did not find old assistant message in helper request")
	}

	if len(result.NewMessages) != 4 {
		t.Fatalf("new messages = %d, want summary + ack + retained user + retained assistant", len(result.NewMessages))
	}
	if got := MessageText(result.NewMessages[2]); got != "RECENT exact user request" {
		t.Fatalf("retained user = %q", got)
	}
	retainedAssistant := result.NewMessages[3]
	if got := MessageText(retainedAssistant); got != "RECENT exact assistant state" {
		t.Fatalf("retained assistant = %q", got)
	}
	if retainedAssistant.Parts[0].ReasoningEncryptedContent != "recent-encrypted-reasoning" || retainedAssistant.Parts[0].ReasoningItemID != "recent-reasoning-id" {
		t.Fatalf("retained raw suffix lost reasoning replay metadata: %+v", retainedAssistant.Parts[0])
	}
}

func TestSoftCompactAvoidsPreviousTurnsOverlapWithRawSuffix(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTurn(MockTurn{Text: "continue from durable summary", Usage: Usage{InputTokens: 9, OutputTokens: 3}})

	messages := []Message{
		UserText("older durable user goal"),
		AssistantText("older durable assistant state"),
		UserText("RECENT raw user instruction"),
		AssistantText("RECENT raw assistant progress"),
	}
	config := DefaultCompactionConfig()
	config.RecentRawTurns = 1
	config.RecentRawTokenBudget = 2_000

	result, err := SoftCompact(context.Background(), provider, "test-model", "system", messages, config)
	if err != nil {
		t.Fatalf("SoftCompact failed: %v", err)
	}
	helperText := requestMessagesText(provider.Requests[0].Messages)
	if !strings.Contains(helperText, "older durable user goal") || !strings.Contains(helperText, "older durable assistant state") {
		t.Fatalf("helper request missing older prefix:\n%s", helperText)
	}
	if strings.Contains(helperText, "RECENT raw user instruction") || strings.Contains(helperText, "RECENT raw assistant progress") {
		t.Fatalf("helper request included retained raw suffix:\n%s", helperText)
	}
	if strings.Contains(result.Summary, "RECENT raw user instruction") || strings.Contains(result.Summary, "RECENT raw assistant progress") {
		t.Fatalf("summary duplicated retained raw suffix:\n%s", result.Summary)
	}
	if got := MessageText(result.NewMessages[len(result.NewMessages)-2]); got != "RECENT raw user instruction" {
		t.Fatalf("retained raw user = %q", got)
	}
	if got := MessageText(result.NewMessages[len(result.NewMessages)-1]); got != "RECENT raw assistant progress" {
		t.Fatalf("retained raw assistant = %q", got)
	}
}

func TestCompactSplitRawSuffixDoesNotStartWithToolResult(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTextResponse("summary including giant latest user prefix")

	messages := []Message{
		UserText("older context"),
		AssistantText("older answer"),
		UserText("latest huge request " + strings.Repeat("x", 2_000)),
		{Role: RoleAssistant, Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "call-1", Name: "shell", Arguments: json.RawMessage(`{"command":"go test ./..."}`)}}}},
		{Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{ID: "call-1", Name: "shell", Content: "ok"}}}},
	}
	config := DefaultCompactionConfig()
	config.RecentRawTurns = 1
	config.RecentRawTokenBudget = 80

	result, err := Compact(context.Background(), provider, "test-model", "", messages, config)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if len(result.NewMessages) < 3 {
		t.Fatalf("new messages too short: %#v", result.NewMessages)
	}
	firstRaw := result.NewMessages[1]
	if firstRaw.Role == RoleTool {
		t.Fatalf("raw suffix started with orphan tool result: %#v", result.NewMessages)
	}
	if firstRaw.Role != RoleAssistant {
		t.Fatalf("first raw suffix message = %s, want assistant split point", firstRaw.Role)
	}
	if len(result.NewMessages) < 3 || result.NewMessages[2].Role != RoleTool {
		t.Fatalf("tool result should be retained after its assistant call, got %#v", result.NewMessages)
	}
}

func TestCompactRawSuffixCanBeDisabled(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTextResponse("summary only")

	config := DefaultCompactionConfig()
	config.RecentRawTokenBudget = -1
	result, err := Compact(context.Background(), provider, "test-model", "", []Message{
		UserText("older user"),
		AssistantText("older answer"),
		UserText("recent user should not be raw"),
	}, config)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if len(result.NewMessages) != 2 {
		t.Fatalf("new messages = %d, want summary + ack when raw suffix disabled", len(result.NewMessages))
	}
	for i, msg := range result.NewMessages[1:] {
		if strings.Contains(MessageText(msg), "recent user should not be raw") {
			t.Fatalf("disabled raw suffix retained recent message outside summary at new message %d: %#v", i+1, result.NewMessages)
		}
	}
}

func requestMessagesText(messages []Message) string {
	var b strings.Builder
	for _, msg := range messages {
		if text := MessageText(msg); text != "" {
			b.WriteString(text)
			b.WriteByte('\n')
		}
		for _, part := range msg.Parts {
			if part.ToolCall != nil {
				b.WriteString(part.ToolCall.Name)
				b.WriteString(" ")
				b.Write(part.ToolCall.Arguments)
				b.WriteByte('\n')
			}
			if part.ToolResult != nil {
				b.WriteString(part.ToolResult.Name)
				b.WriteString(" ")
				b.WriteString(part.ToolResult.Content)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

func TestContinuationBriefPrefersTextOverReasoningSummary(t *testing.T) {
	msg := Message{Role: RoleAssistant, Parts: []Part{{
		Type:             PartText,
		Text:             "text brief",
		ReasoningContent: "reasoning summary should not be included",
		ReasoningKind:    ReasoningKindSummary,
	}}}
	if got := continuationBriefFromAssistantMessage(msg); got != "text brief" {
		t.Fatalf("brief = %q, want text brief", got)
	}

	msg.Parts[0].Text = ""
	if got := continuationBriefFromAssistantMessage(msg); got != "reasoning summary should not be included" {
		t.Fatalf("reasoning fallback = %q", got)
	}
}

func TestCompactionResultFromBriefDoesNotDuplicateInternalBriefTurn(t *testing.T) {
	brief := "Continue by editing internal/llm/engine.go."
	result := CompactionResultFromBrief("", brief, []Message{
		UserText("latest user request"),
		UserText(contextContinuationPrompt),
		UserText(contextContinuationBriefPrompt),
		AssistantText(brief),
	}, CompactionConfig{InputLimit: 5000})

	if strings.Contains(result.Summary, contextContinuationBriefPrompt) || strings.Contains(result.Summary, contextContinuationPrompt) {
		t.Fatalf("summary duplicated internal continuation prompt:\n%s", result.Summary)
	}
	if strings.Count(result.Summary, brief) != 1 {
		t.Fatalf("summary should contain brief exactly once, got %d:\n%s", strings.Count(result.Summary, brief), result.Summary)
	}
	if !strings.Contains(result.Summary, "latest user request") {
		t.Fatalf("summary missing deterministic user context:\n%s", result.Summary)
	}
}

func TestTruncateToolResult(t *testing.T) {
	t.Run("under limit", func(t *testing.T) {
		short := "hello"
		if got := TruncateToolResult(short, 100); got != short {
			t.Errorf("short content should not be truncated")
		}
	})

	t.Run("over limit", func(t *testing.T) {
		long := strings.Repeat("x", 1000)
		result := TruncateToolResult(long, 100)
		if len(result) >= len(long) {
			t.Errorf("truncated result should be shorter than original")
		}
		if !strings.Contains(result, "900 chars truncated") {
			t.Errorf("truncated result should report 900 chars truncated, got: %s", result)
		}
		if !strings.Contains(result, "1 lines") {
			t.Errorf("single-line truncated middle should report 1 line, got: %s", result)
		}
		// First 50 and last 50 chars should be preserved
		if !strings.HasPrefix(result, strings.Repeat("x", 50)) {
			t.Errorf("truncated result should preserve first half")
		}
		if !strings.HasSuffix(result, strings.Repeat("x", 50)) {
			t.Errorf("truncated result should preserve last half")
		}
	})

	t.Run("odd limit", func(t *testing.T) {
		// With odd limit 101: head=50, tail=51
		content := strings.Repeat("a", 50) + strings.Repeat("b", 51) + strings.Repeat("c", 99)
		result := TruncateToolResult(content, 101)
		runes := []rune(result)
		// Head should be 50 'a's, tail should be 51 'c's (last 51 of the 99)
		headPart := string(runes[:50])
		if headPart != strings.Repeat("a", 50) {
			t.Errorf("head should be 50 'a's, got %q", headPart)
		}
		tailPart := string(runes[len(runes)-51:])
		if tailPart != strings.Repeat("c", 51) {
			t.Errorf("tail should be 51 'c's, got %q", tailPart)
		}
	})

	t.Run("line count accuracy", func(t *testing.T) {
		// Create content with known line structure in the middle
		head := strings.Repeat("H", 50)
		middle := "line1\nline2\nline3\n" + strings.Repeat("x", 100)
		tail := strings.Repeat("T", 50)
		content := head + middle + tail
		result := TruncateToolResult(content, 100)
		// Middle has 3 newlines → 4 lines
		if !strings.Contains(result, "4 lines") {
			t.Errorf("expected 4 lines in truncation marker, got: %s", result)
		}
	})

	t.Run("multibyte UTF-8", func(t *testing.T) {
		// Each emoji is a multi-byte rune; ensure we don't split them
		content := strings.Repeat("\U0001f600", 200) // 200 smiley faces (4 bytes each)
		result := TruncateToolResult(content, 100)
		// Should contain truncation marker
		if !strings.Contains(result, "chars truncated") {
			t.Errorf("should truncate multi-byte content")
		}
		// Head and tail should be valid UTF-8 with intact runes
		runes := []rune(result)
		// First 50 runes should be smiley faces
		for i := 0; i < 50; i++ {
			if runes[i] != '\U0001f600' {
				t.Errorf("rune %d in head should be smiley, got %U", i, runes[i])
				break
			}
		}
		// Last 50 runes should be smiley faces
		for i := len(runes) - 50; i < len(runes); i++ {
			if runes[i] != '\U0001f600' {
				t.Errorf("rune %d in tail should be smiley, got %U", i, runes[i])
				break
			}
		}
	})

	t.Run("shell exit_code preserved in tail", func(t *testing.T) {
		// Simulate shell output: large body + exit_code on last line
		body := strings.Repeat("output line\n", 2000)
		content := body + "exit_code: 0"
		result := TruncateToolResult(content, 1000)
		if !strings.HasSuffix(result, "exit_code: 0") {
			t.Errorf("exit_code should be preserved in tail, result ends with: %q",
				result[len(result)-50:])
		}
	})

	t.Run("empty string", func(t *testing.T) {
		if got := TruncateToolResult("", 100); got != "" {
			t.Errorf("empty string should return empty, got %q", got)
		}
	})

	t.Run("exact boundary", func(t *testing.T) {
		exact := strings.Repeat("a", 100)
		if got := TruncateToolResult(exact, 100); got != exact {
			t.Errorf("content exactly at limit should not be truncated")
		}
	})
}

func TestIsContextOverflowError(t *testing.T) {
	tests := []struct {
		err      error
		expected bool
	}{
		{nil, false},
		{fmt.Errorf("network timeout"), false},
		{fmt.Errorf("max_tokens must be at most 4096"), false}, // output token config error, not context overflow
		{fmt.Errorf("context length exceeded"), true},
		{fmt.Errorf("maximum context length is 128000"), true},
		{fmt.Errorf("context_length_exceeded"), true},
		{fmt.Errorf("too many tokens in request"), true},
		{fmt.Errorf("Request Too Large"), true},
		{fmt.Errorf("prompt is too long"), true},
		{fmt.Errorf("the input is too long for this model"), true},
		{fmt.Errorf("exceeds the model's maximum context"), true},
	}

	for _, tt := range tests {
		got := isContextOverflowError(tt.err)
		if got != tt.expected {
			errStr := "<nil>"
			if tt.err != nil {
				errStr = tt.err.Error()
			}
			t.Errorf("isContextOverflowError(%q) = %v, want %v", errStr, got, tt.expected)
		}
	}
}

func TestInputLimitForModel(t *testing.T) {
	tests := []struct {
		model    string
		expected int
	}{
		// Claude 1M models: 1M - 20K practical output reserve = 980K
		{"claude-opus-4-8", 980_000},
		{"claude-sonnet-4-6", 980_000},
		{"claude-opus-4-6", 980_000},
		// Claude 200K models: 200K - 20K practical output reserve = 180K
		{"claude-sonnet-4-5-20250929", 180_000},
		{"claude-sonnet-4-20250514", 180_000},
		{"claude-opus-4-20250514", 180_000},
		// Claude 3.x: small max output, full deduction
		{"claude-3.5-sonnet-20241022", 192_000}, // 200K - 8K
		{"gpt-4o-2024-05-13", 112_000},          // 128K - 16K
		{"gpt-4.1-2025-04-14", 1_014_808},       // 1047K - 32K
		{"gpt-4", 8_192},
		{"gpt-4-32k", 32_768},
		{"gpt-5.3-codex", 272_000},       // explicit input=272K
		{"gpt-5.3-codex-spark", 100_000}, // explicit input=100K
		{"gpt-5.2-codex", 272_000},
		{"gpt-5.2-chat-latest", 112_000}, // 128K - 16K
		{"gpt-5.1", 272_000},
		{"gpt-5.1-chat-latest", 112_000},
		{"gpt-5", 272_000},
		{"gpt-5.4-mini", 272_000},
		{"gpt-5.4-nano", 272_000},
		{"gpt-5-mini", 272_000},
		{"o1-mini", 62_000},                 // 128K - 65K
		{"o1-pro", 100_000},                 // 200K - 100K
		{"o3-mini", 100_000},                // 200K - 100K
		{"gpt-4-turbo-2024-04-09", 124_000}, // 128K - 4K
		{"gpt-3.5-turbo-0125", 12_000},      // 16K - 4K
		{"gemini-2.5-pro-latest", 983_000},  // 1M - 65K
		{"gemini-3-pro-preview", 936_000},   // 1M - 64K
		{"gemini-3-flash-preview", 983_000}, // 1M - 65K
		{"grok-4-1-fast", 1_970_000},        // 2M - 30K
		{"grok-3-mini-fast", 123_000},       // 131K - 8K
		{"deepseek-chat", 128_000},
		{"unknown-model-xyz", 0},
		{"", 0},
		// Case insensitivity
		{"GPT-5", 272_000},
		{"Claude-Sonnet-4-5-20250929", 180_000},
	}

	for _, tt := range tests {
		got := InputLimitForModel(tt.model)
		if got != tt.expected {
			t.Errorf("InputLimitForModel(%q) = %d, want %d", tt.model, got, tt.expected)
		}
	}
}

func TestInputLimitForProviderModel(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	RefreshCopilotCacheSync([]ModelInfo{
		{ID: "gpt-5.5", InputLimit: 1_030_000},
		{ID: "gpt-5.4", InputLimit: 1_030_000},
		{ID: "gpt-5.4-mini", InputLimit: 380_000},
		{ID: "gpt-5.3-codex", InputLimit: 380_000},
		{ID: "gpt-5.2-codex", InputLimit: 252_000},
		{ID: "gpt-5.2", InputLimit: 108_000},
		{ID: "gpt-5.1-codex", InputLimit: 108_000},
		{ID: "gpt-5.1", InputLimit: 108_000},
		{ID: "gpt-5", InputLimit: 108_000},
		{ID: "gemini-3-pro", InputLimit: 108_000},
		{ID: "gpt-4.1", InputLimit: 48_000},
		{ID: "gpt-4o", InputLimit: 48_000},
	})

	tests := []struct {
		provider string
		model    string
		expected int
	}{
		// Copilot-specific effective input limits come from the live model cache.
		// The Copilot /models parser applies term-llm's practical 20K output
		// reserve only when Copilot reports context+output but no explicit prompt
		// limit.
		{"copilot", "gpt-5.5", 1_030_000},
		{"copilot", "gpt-5.5-medium", 1_030_000},
		{"copilot", "gpt-5.4", 1_030_000},
		{"copilot", "gpt-5.4-mini", 380_000},
		{"copilot", "gpt-5.3-codex", 380_000},
		{"copilot", "gpt-5.2-codex", 252_000},
		{"copilot", "gpt-5.2", 108_000},
		{"copilot", "gpt-5.1-codex", 108_000},
		{"copilot", "gpt-5.1", 108_000},
		{"copilot", "gpt-5", 108_000},
		{"copilot", "gemini-3-pro", 108_000},
		{"copilot", "gpt-4.1", 48_000},
		{"copilot", "gpt-4o", 48_000},
		// Copilot does not fall back to canonical limits; use live cache or config.
		{"copilot", "gpt-3.5-turbo", 0},
		// OpenAI direct uses canonical input limits
		{"openai", "gpt-5.2-codex", 272_000},
		{"openai", "gpt-5.4-mini", 272_000},
		{"openai", "gpt-5.4-nano", 272_000},
		{"openai", "gpt-5", 272_000},
		{"openai", "gpt-4.1", 1_014_808},
		// ChatGPT uses canonical input limits
		{"chatgpt", "gpt-5.2-codex", 272_000},
		{"chatgpt", "gpt-5.4-mini", 272_000},
		// Unknown provider falls back to canonical
		{"", "gpt-5", 272_000},
		{"unknown", "gpt-5", 272_000},
	}

	for _, tt := range tests {
		got := InputLimitForProviderModel(tt.provider, tt.model)
		if got != tt.expected {
			t.Errorf("InputLimitForProviderModel(%q, %q) = %d, want %d", tt.provider, tt.model, got, tt.expected)
		}
	}
}

func TestOutputLimitForModel(t *testing.T) {
	tests := []struct {
		model    string
		expected int
	}{
		{"claude-sonnet-4-6", 64_000},
		{"claude-3.5-sonnet-20241022", 8_192},
		{"claude-3-opus-20240229", 4_096},
		{"gpt-4-turbo-2024-04-09", 4_096},
		{"gpt-3.5-turbo-0125", 4_096},
		{"gpt-4o-2024-05-13", 16_384},
		{"gpt-5", 128_000},
		{"gpt-5.4-mini", 128_000},
		{"gpt-5.4-nano", 128_000},
		{"unknown-model-xyz", 0},
	}

	for _, tt := range tests {
		got := OutputLimitForModel(tt.model)
		if got != tt.expected {
			t.Errorf("OutputLimitForModel(%q) = %d, want %d", tt.model, got, tt.expected)
		}
	}
}

func TestClampOutputTokens(t *testing.T) {
	tests := []struct {
		name      string
		requested int
		model     string
		expected  int
	}{
		{"within limit", 10_000, "claude-sonnet-4-6", 10_000},
		{"exceeds limit", 10_000, "gpt-3.5-turbo", 4_096},
		{"exactly at limit", 4_096, "gpt-3.5-turbo", 4_096},
		{"unknown model no clamp", 10_000, "unknown-model", 10_000},
		{"zero requested", 0, "gpt-3.5-turbo", 0},
		{"negative requested", -1, "gpt-3.5-turbo", -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClampOutputTokens(tt.requested, tt.model)
			if got != tt.expected {
				t.Errorf("ClampOutputTokens(%d, %q) = %d, want %d", tt.requested, tt.model, got, tt.expected)
			}
		})
	}
}

func TestTrimMessagesToFit(t *testing.T) {
	sys := SystemText("system prompt")
	// Each message is ~5 tokens (20 bytes / 4)
	u1 := UserText("first user message!")
	a1 := AssistantText("first assistant msg")
	u2 := UserText("second user message")
	a2 := AssistantText("second asst message")
	u3 := UserText("third user message!!")

	t.Run("fits within budget", func(t *testing.T) {
		msgs := []Message{sys, u1, a1, u2, a2, u3}
		result := trimMessagesToFit(msgs, 10000)
		if len(result) != 6 {
			t.Errorf("expected 6 messages (no trimming), got %d", len(result))
		}
	})

	t.Run("trims oldest first", func(t *testing.T) {
		msgs := []Message{sys, u1, a1, u2, a2, u3}
		// Budget for system + ~2 messages
		budget := EstimateMessageTokens([]Message{sys}) + EstimateMessageTokens([]Message{u3}) + 1
		result := trimMessagesToFit(msgs, budget)
		if result[0].Role != RoleSystem {
			t.Error("should preserve system message")
		}
		// Last message should be u3
		last := result[len(result)-1]
		if last.Parts[0].Text != u3.Parts[0].Text {
			t.Errorf("last message should be u3, got %q", last.Parts[0].Text)
		}
	})

	t.Run("skips leading assistant after trim", func(t *testing.T) {
		msgs := []Message{sys, u1, a1, u2}
		// Budget fits system + a1 + u2, but a1 is assistant — should be skipped
		budget := EstimateMessageTokens([]Message{sys, a1, u2}) + 1
		result := trimMessagesToFit(msgs, budget)
		// After trimming u1 and skipping leading a1, should have sys + u2
		if len(result) < 2 {
			t.Fatalf("expected at least 2 messages, got %d", len(result))
		}
		if result[0].Role != RoleSystem {
			t.Error("first should be system")
		}
		if result[1].Role != RoleUser {
			t.Errorf("should start with user after system, got %s", result[1].Role)
		}
	})

	t.Run("no system message", func(t *testing.T) {
		msgs := []Message{u1, a1, u2}
		budget := EstimateMessageTokens([]Message{u2}) + 1
		result := trimMessagesToFit(msgs, budget)
		if len(result) != 1 || result[0].Parts[0].Text != u2.Parts[0].Text {
			t.Errorf("should trim to just u2, got %d messages", len(result))
		}
	})
}

func TestTrimMessagesToFitPreservesAnchor(t *testing.T) {
	sys := SystemText("system")
	summary := Message{
		Role:        RoleUser,
		Parts:       []Part{{Type: PartText, Text: summaryPrefix + "previous summary"}},
		CacheAnchor: true,
	}
	ack := AssistantText("I've reviewed the context summary.")
	u1 := UserText("first user message!")
	a1 := AssistantText("first assistant msg")
	u2 := UserText("second user message")

	t.Run("preserves anchor block", func(t *testing.T) {
		msgs := []Message{sys, summary, ack, u1, a1, u2}
		// Budget fits system + summary + ack + u2 but not u1+a1
		budget := EstimateMessageTokens([]Message{sys, summary, ack, u2}) + 1
		result := trimMessagesToFit(msgs, budget)
		// Should have: sys, summary, ack, u2
		if len(result) < 4 {
			t.Fatalf("expected at least 4 messages, got %d", len(result))
		}
		if result[0].Role != RoleSystem {
			t.Error("first should be system")
		}
		if !result[1].CacheAnchor {
			t.Error("second should be cache-anchored summary")
		}
		if result[2].Role != RoleAssistant {
			t.Error("third should be assistant ack")
		}
		if result[len(result)-1].Parts[0].Text != u2.Parts[0].Text {
			t.Errorf("last should be u2, got %q", result[len(result)-1].Parts[0].Text)
		}
	})

	t.Run("drops anchor when too large", func(t *testing.T) {
		bigSummary := Message{
			Role:        RoleUser,
			Parts:       []Part{{Type: PartText, Text: strings.Repeat("x", 10000)}},
			CacheAnchor: true,
		}
		msgs := []Message{sys, bigSummary, ack, u1, u2}
		// Budget only fits system + u2
		budget := EstimateMessageTokens([]Message{sys, u2}) + 1
		result := trimMessagesToFit(msgs, budget)
		if result[0].Role != RoleSystem {
			t.Error("first should be system")
		}
		// Should have dropped the anchor since it doesn't fit
		if len(result) == 2 && result[1].Parts[0].Text == u2.Parts[0].Text {
			// OK: sys + u2
		} else {
			for _, m := range result {
				if m.CacheAnchor {
					t.Error("anchor should have been dropped when too large")
				}
			}
		}
	})
}

func TestTrimOversizedSingleMessage(t *testing.T) {
	sys := SystemText("system")
	// A single user message that's way too large
	bigMsg := UserText(strings.Repeat("x", 40000))
	msgs := []Message{sys, bigMsg}

	// Budget that fits system + some content but not all of bigMsg
	budget := EstimateMessageTokens([]Message{sys}) + 500
	result := trimMessagesToFit(msgs, budget)

	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0].Role != RoleSystem {
		t.Error("first should be system")
	}
	// The big message should have been truncated
	if len(result[1].Parts[0].Text) >= 40000 {
		t.Error("oversized message should have been truncated")
	}
	if !strings.Contains(result[1].Parts[0].Text, "chars truncated") {
		t.Error("truncated message should contain truncation marker")
	}
}

func TestCompactReportsUsage(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTurn(MockTurn{
		Text: "Summary.",
		Usage: Usage{
			InputTokens:       484,
			OutputTokens:      1200,
			CachedInputTokens: 200000,
			CacheWriteTokens:  32,
		},
	})

	result, err := Compact(context.Background(), provider, "test-model", "", []Message{UserText("hello")}, DefaultCompactionConfig())
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if result.Usage.InputTokens != 484 || result.Usage.OutputTokens != 1200 || result.Usage.CachedInputTokens != 200000 || result.Usage.CacheWriteTokens != 32 {
		t.Fatalf("usage = %+v, want compaction helper usage", result.Usage)
	}
}

func TestCompactUsesProviderInputLimit(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTextResponse("Summary.")

	messages := []Message{
		UserText("hello"),
		AssistantText("hi"),
	}

	config := DefaultCompactionConfig()
	config.InputLimit = 50_000 // Provider-specific limit

	_, err := Compact(context.Background(), provider, "test-model", "", messages, config)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	// Verify it didn't crash — the InputLimit was used instead of canonical
}

func TestCompactClampsOutputTokens(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTextResponse("Summary.")

	messages := []Message{
		UserText("hello"),
		AssistantText("hi"),
	}

	config := DefaultCompactionConfig()
	config.SummaryTokenBudget = 10_000

	// Use a model with small output limit (GPT-3.5: 4096)
	_, err := Compact(context.Background(), provider, "gpt-3.5-turbo", "", messages, config)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	// The budget should have been clamped to the model's output limit
	if provider.Requests[0].MaxOutputTokens != 4_096 {
		t.Errorf("MaxOutputTokens = %d, want 4096 (clamped)", provider.Requests[0].MaxOutputTokens)
	}
}

func TestCompactRecompactsAlreadyCompacted(t *testing.T) {
	// Simulate re-compacting a conversation that was already compacted.
	// The previous summary (CacheAnchor=true) should be visible to the LLM.
	provider := NewMockProvider("test")
	provider.AddTextResponse("New combined summary.")

	messages := []Message{
		// Previous compaction output
		{
			Role:        RoleUser,
			Parts:       []Part{{Type: PartText, Text: summaryPrefix + "Previous summary content."}},
			CacheAnchor: true,
		},
		AssistantText("I've reviewed the context summary."),
		// New messages since last compaction
		UserText("new question"),
		AssistantText("new answer"),
		UserText("follow up"),
	}

	config := DefaultCompactionConfig()
	result, err := Compact(context.Background(), provider, "test-model", "sys", messages, config)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	// The LLM request should have included the previous summary
	foundPrevSummary := false
	for _, msg := range provider.Requests[0].Messages {
		if msg.CacheAnchor && strings.Contains(msg.Parts[0].Text, "Previous summary content") {
			foundPrevSummary = true
			break
		}
	}
	if !foundPrevSummary {
		t.Error("re-compaction request should include previous summary")
	}

	// Result should have new summary with CacheAnchor
	if !result.NewMessages[1].CacheAnchor {
		t.Error("new summary should have CacheAnchor=true")
	}
	if !strings.Contains(result.NewMessages[1].Parts[0].Text, "New combined summary") {
		t.Error("new summary should contain the LLM's response")
	}
}

func newIsolatedResponsesProvider(t *testing.T, responseText, responseID string) (Provider, *OpenAIProvider, <-chan struct {
	PreviousResponseID string
	Input              []ResponsesInputItem
}) {
	t.Helper()

	captured := make(chan struct {
		PreviousResponseID string
		Input              []ResponsesInputItem
	}, 1)

	sse := strings.Join([]string{
		"event: response.output_text.delta",
		fmt.Sprintf(`data: {"delta":%q}`, responseText),
		"",
		"event: response.completed",
		fmt.Sprintf(`data: {"response":{"id":%q}}`, responseID),
		"",
		"data: [DONE]",
		"",
	}, "\n")

	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			defer r.Body.Close()

			var payload struct {
				PreviousResponseID string               `json:"previous_response_id"`
				Input              []ResponsesInputItem `json:"input"`
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return nil, err
			}
			captured <- struct {
				PreviousResponseID string
				Input              []ResponsesInputItem
			}{
				PreviousResponseID: payload.PreviousResponseID,
				Input:              payload.Input,
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(strings.NewReader(sse)),
			}, nil
		}),
	}

	provider := &OpenAIProvider{
		apiKey: "test-key",
		model:  "gpt-5.2",
		responsesClient: &ResponsesClient{
			BaseURL:        "https://example.test/v1/responses",
			GetAuthHeader:  func() string { return "Bearer test-key" },
			HTTPClient:     httpClient,
			LastResponseID: "resp_live",
		},
	}

	return WrapWithRetry(provider, DefaultRetryConfig()), provider, captured
}

func TestCompactUsesIsolatedProviderConversationState(t *testing.T) {
	provider, liveProvider, captured := newIsolatedResponsesProvider(t, "summary", "resp_compact")

	result, err := Compact(context.Background(), provider, "gpt-5.2", "sys", []Message{
		UserText("hello"),
		AssistantText("hi"),
		UserText("please summarize"),
	}, DefaultCompactionConfig())
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if !strings.Contains(result.Summary, "<PREVIOUS_TURNS>") || !strings.Contains(result.Summary, "<SUMMARY_AND_NEXT_ACTIONS>") || !strings.Contains(result.Summary, "summary") {
		t.Fatalf("summary missing structured compaction content:\n%s", result.Summary)
	}

	if liveProvider.responsesClient.LastResponseID != "resp_live" {
		t.Fatalf("live LastResponseID = %q, want %q", liveProvider.responsesClient.LastResponseID, "resp_live")
	}

	select {
	case req := <-captured:
		if req.PreviousResponseID != "" {
			t.Fatalf("compaction previous_response_id = %q, want empty", req.PreviousResponseID)
		}
		if len(req.Input) < 4 {
			t.Fatalf("compaction input length = %d, want full history plus prompt", len(req.Input))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for compaction request capture")
	}
}

func TestHandoverUsesIsolatedProviderConversationState(t *testing.T) {
	provider, liveProvider, captured := newIsolatedResponsesProvider(t, "handover doc", "resp_handover")

	result, err := Handover(context.Background(), provider, "gpt-5.2", "current sys", "new sys", []Message{
		UserText("hello"),
		AssistantText("hi"),
		UserText("prepare handover"),
	}, "source", "target", DefaultCompactionConfig())
	if err != nil {
		t.Fatalf("Handover failed: %v", err)
	}
	if result.Document != "handover doc" {
		t.Fatalf("document = %q, want %q", result.Document, "handover doc")
	}

	if liveProvider.responsesClient.LastResponseID != "resp_live" {
		t.Fatalf("live LastResponseID = %q, want %q", liveProvider.responsesClient.LastResponseID, "resp_live")
	}

	select {
	case req := <-captured:
		if req.PreviousResponseID != "" {
			t.Fatalf("handover previous_response_id = %q, want empty", req.PreviousResponseID)
		}
		if len(req.Input) < 4 {
			t.Fatalf("handover input length = %d, want full history plus prompt", len(req.Input))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handover request capture")
	}
}

func TestFormatTokenCount(t *testing.T) {
	tests := []struct {
		tokens   int
		expected string
	}{
		{0, ""},
		{-1, ""},
		{128_000, "128K"},
		{200_000, "200K"},
		{400_000, "400K"},
		{1_047_576, "1M"},
		{1_048_576, "1M"},
		{2_097_152, "2.1M"},
		{2_000_000, "2M"},
		{16_385, "16K"},
		{8_192, "8K"},
		{32_768, "33K"},
		{131_072, "131K"},
		{256_000, "256K"},
	}

	for _, tt := range tests {
		got := FormatTokenCount(tt.tokens)
		if got != tt.expected {
			t.Errorf("FormatTokenCount(%d) = %q, want %q", tt.tokens, got, tt.expected)
		}
	}
}

func TestCompactSanitizesOrphanedToolCalls(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTextResponse("Summary of tool call.")

	messages := []Message{
		UserText("Please run the tool."),
		{
			Role: RoleAssistant,
			Parts: []Part{{
				Type: PartToolCall,
				ToolCall: &ToolCall{
					ID:        "call-1",
					Name:      "orphaned_tool",
					Arguments: []byte(`{"path":"/tmp/foo"}`),
				},
			}},
		},
		UserText("Thanks."),
	}

	result, err := Compact(context.Background(), provider, "test-model", "system prompt", messages, DefaultCompactionConfig())
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	if len(provider.Requests) == 0 {
		t.Fatal("expected provider request to be recorded")
	}

	// The request messages should not contain any orphaned tool calls
	// (sanitizeToolHistory removes them before building the request).
	for _, msg := range provider.Requests[0].Messages {
		for _, part := range msg.Parts {
			if part.Type == PartToolCall && part.ToolCall != nil && part.ToolCall.Name == "orphaned_tool" {
				t.Errorf("request should not contain orphaned tool call 'orphaned_tool'")
			}
		}
	}

	for _, msg := range result.NewMessages {
		for _, part := range msg.Parts {
			if part.Type == PartToolCall {
				t.Fatalf("unexpected tool call in compacted history")
			}
		}
	}
}

func TestCompactEndToEnd(t *testing.T) {
	// Set up mock provider that returns a summary
	provider := NewMockProvider("test")
	provider.AddTextResponse("## Summary\nUser was debugging a Go test. Fixed the nil pointer in main.go:42.")

	messages := []Message{
		UserText("Help me debug this Go test"),
		AssistantText("I'll look at the test file."),
		UserText("The test is in main_test.go"),
		AssistantText("I see the issue - nil pointer at line 42."),
		UserText("Can you fix it?"),
		AssistantText("Fixed the nil pointer by adding a nil check."),
	}

	config := DefaultCompactionConfig()
	result, err := Compact(context.Background(), provider, "test-model", "You are a helpful assistant.", messages, config)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	if result.Summary == "" {
		t.Error("summary should not be empty")
	}
	if result.OriginalCount != 6 {
		t.Errorf("original count = %d, want 6", result.OriginalCount)
	}
	if len(result.NewMessages) == 0 {
		t.Error("new messages should not be empty")
	}

	// First message should be system
	if result.NewMessages[0].Role != RoleSystem {
		t.Errorf("first message should be system, got %s", result.NewMessages[0].Role)
	}

	// Second should be user with summary
	if result.NewMessages[1].Role != RoleUser {
		t.Errorf("second message should be user (summary), got %s", result.NewMessages[1].Role)
	}
	if !strings.Contains(result.NewMessages[1].Parts[0].Text, "Summary") {
		t.Errorf("summary message should contain the summary text")
	}

	// Provider should receive original messages + compaction instruction (not a text blob)
	req := provider.Requests[0]
	// Last message should be the compaction prompt
	lastMsg := req.Messages[len(req.Messages)-1]
	if lastMsg.Role != RoleUser {
		t.Errorf("last request message should be user (compaction prompt), got %s", lastMsg.Role)
	}
	if !strings.Contains(lastMsg.Parts[0].Text, "Create a compact continuation brief") || !strings.Contains(lastMsg.Parts[0].Text, "## Objective") {
		t.Errorf("last request message should contain structured compaction prompt")
	}
	// Request should include original conversation messages (not serialized text)
	foundOriginal := false
	for _, msg := range req.Messages {
		if msg.Role == RoleUser && len(msg.Parts) > 0 && msg.Parts[0].Text == "Help me debug this Go test" {
			foundOriginal = true
			break
		}
	}
	if !foundOriginal {
		t.Error("request should contain original conversation messages")
	}
}

func TestCompactEmptyMessages(t *testing.T) {
	provider := NewMockProvider("test")
	config := DefaultCompactionConfig()

	_, err := Compact(context.Background(), provider, "test-model", "", nil, config)
	if err == nil {
		t.Error("Compact with nil messages should return error")
	}
}

func TestCompactProviderError(t *testing.T) {
	// When the provider stream errors, Compact should return an error
	// (either the stream error or "empty summary" if the stream closes cleanly)
	provider := NewMockProvider("test")
	provider.AddTurn(MockTurn{Error: fmt.Errorf("rate limited")})
	config := DefaultCompactionConfig()

	_, err := Compact(context.Background(), provider, "test-model", "", []Message{UserText("hi")}, config)
	if err == nil {
		t.Error("Compact should return error when provider fails")
	}
}

func TestCompactProviderNoTurns(t *testing.T) {
	// When provider has no turns configured, Stream itself returns an error
	provider := NewMockProvider("test")
	config := DefaultCompactionConfig()

	_, err := Compact(context.Background(), provider, "test-model", "", []Message{UserText("hi")}, config)
	if err == nil {
		t.Error("Compact should return error when provider has no turns")
	}
	if !strings.Contains(err.Error(), "stream failed") {
		t.Errorf("expected stream failed error, got: %v", err)
	}
}

func TestCompactNoSystemPrompt(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTextResponse("Summary of conversation.")

	messages := []Message{
		UserText("question"),
		AssistantText("answer"),
	}

	config := DefaultCompactionConfig()
	result, err := Compact(context.Background(), provider, "test-model", "", messages, config)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	// Without system prompt the compacted transcript still starts with the summary.
	if result.NewMessages[0].Role != RoleUser {
		t.Errorf("first message should be user (summary) when no system prompt, got %s", result.NewMessages[0].Role)
	}
}

func TestDefaultCompactionConfig(t *testing.T) {
	config := DefaultCompactionConfig()
	if config.ThresholdRatio != 0.90 {
		t.Errorf("ThresholdRatio = %f, want 0.90", config.ThresholdRatio)
	}
	if config.MaxToolResultChars != 80_000 {
		t.Errorf("MaxToolResultChars = %d, want 80000", config.MaxToolResultChars)
	}
	if config.SummaryTokenBudget != 10_000 {
		t.Errorf("SummaryTokenBudget = %d, want 10000", config.SummaryTokenBudget)
	}
	if config.RecentRawTurns != defaultRecentRawTurns {
		t.Errorf("RecentRawTurns = %d, want %d", config.RecentRawTurns, defaultRecentRawTurns)
	}
	if config.RecentRawTokenBudget != 0 {
		t.Errorf("RecentRawTokenBudget = %d, want 0 for auto", config.RecentRawTokenBudget)
	}
}

func TestEffectiveRecentRawTokenBudgetAutoIsModest(t *testing.T) {
	tests := []struct {
		name      string
		config    CompactionConfig
		wantToken int
	}{
		{name: "disabled", config: CompactionConfig{RecentRawTokenBudget: -1, InputLimit: 128_000}, wantToken: 0},
		{name: "explicit", config: CompactionConfig{RecentRawTokenBudget: 12_345, InputLimit: 128_000}, wantToken: 12_345},
		{name: "unknown limit uses cap", config: CompactionConfig{}, wantToken: 8_000},
		{name: "small limit uses floor", config: CompactionConfig{InputLimit: 16_000}, wantToken: 1_000},
		{name: "32k uses five percent", config: CompactionConfig{InputLimit: 32_000}, wantToken: 1_600},
		{name: "64k uses five percent", config: CompactionConfig{InputLimit: 64_000}, wantToken: 3_200},
		{name: "128k uses five percent", config: CompactionConfig{InputLimit: 128_000}, wantToken: 6_400},
		{name: "large limit caps", config: CompactionConfig{InputLimit: 200_000}, wantToken: 8_000},
		{name: "huge limit caps", config: CompactionConfig{InputLimit: 922_000}, wantToken: 8_000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveRecentRawTokenBudget(tt.config); got != tt.wantToken {
				t.Fatalf("effectiveRecentRawTokenBudget(%+v) = %d, want %d", tt.config, got, tt.wantToken)
			}
		})
	}
}

func TestCompactMaxOutputTokens(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTextResponse("Summary.")

	messages := []Message{
		UserText("hello"),
		AssistantText("hi"),
	}

	config := DefaultCompactionConfig()
	config.SummaryTokenBudget = 5_000

	_, err := Compact(context.Background(), provider, "test-model", "", messages, config)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	if len(provider.Requests) == 0 {
		t.Fatal("expected provider request to be recorded")
	}
	if provider.Requests[0].MaxOutputTokens != 5_000 {
		t.Errorf("MaxOutputTokens = %d, want 5000", provider.Requests[0].MaxOutputTokens)
	}
}

func TestCompactMaxOutputTokensDefault(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTextResponse("Summary.")

	messages := []Message{
		UserText("hello"),
		AssistantText("hi"),
	}

	config := DefaultCompactionConfig()
	_, err := Compact(context.Background(), provider, "test-model", "", messages, config)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	if provider.Requests[0].MaxOutputTokens != 10_000 {
		t.Errorf("MaxOutputTokens = %d, want 10000 (default)", provider.Requests[0].MaxOutputTokens)
	}
}

func TestCompactAppendsSafeUserMessage(t *testing.T) {
	config := DefaultCompactionConfig()
	config.RecentRawTokenBudget = -1

	// When last message is assistant, no separator needed
	t.Run("last message assistant", func(t *testing.T) {
		provider := NewMockProvider("test")
		provider.AddTextResponse("Summary.")

		messages := []Message{
			UserText("hello"),
			AssistantText("hi there"),
		}

		_, err := Compact(context.Background(), provider, "test-model", "", messages, config)
		if err != nil {
			t.Fatalf("Compact failed: %v", err)
		}

		req := provider.Requests[0]
		// Last message should be the compaction prompt (user)
		lastMsg := req.Messages[len(req.Messages)-1]
		if lastMsg.Role != RoleUser {
			t.Errorf("last message should be user, got %s", lastMsg.Role)
		}
		// Second-to-last should be the original assistant message (no separator inserted)
		prevMsg := req.Messages[len(req.Messages)-2]
		if prevMsg.Role != RoleAssistant {
			t.Errorf("second-to-last should be assistant, got %s", prevMsg.Role)
		}
		if prevMsg.Parts[0].Text != "hi there" {
			t.Errorf("second-to-last should be original assistant message, got %q", prevMsg.Parts[0].Text)
		}
	})

	// When last message is user, separator assistant message is inserted
	t.Run("last message user", func(t *testing.T) {
		provider := NewMockProvider("test")
		provider.AddTextResponse("Summary.")

		messages := []Message{
			UserText("hello"),
			AssistantText("hi"),
			UserText("thanks"),
		}

		_, err := Compact(context.Background(), provider, "test-model", "", messages, config)
		if err != nil {
			t.Fatalf("Compact failed: %v", err)
		}

		req := provider.Requests[0]
		lastMsg := req.Messages[len(req.Messages)-1]
		if lastMsg.Role != RoleUser {
			t.Errorf("last message should be user (compaction prompt), got %s", lastMsg.Role)
		}
		// Second-to-last should be the separator assistant message
		prevMsg := req.Messages[len(req.Messages)-2]
		if prevMsg.Role != RoleAssistant {
			t.Errorf("separator should be assistant, got %s", prevMsg.Role)
		}
		if !strings.Contains(prevMsg.Parts[0].Text, "summarize") {
			t.Errorf("separator should mention summarize, got %q", prevMsg.Parts[0].Text)
		}
	})

	// When last message is tool result, separator assistant message is inserted
	t.Run("last message tool", func(t *testing.T) {
		provider := NewMockProvider("test")
		provider.AddTextResponse("Summary.")

		messages := []Message{
			UserText("run it"),
			{
				Role: RoleAssistant,
				Parts: []Part{{
					Type:     PartToolCall,
					ToolCall: &ToolCall{ID: "c1", Name: "shell", Arguments: []byte(`{"cmd":"ls"}`)},
				}},
			},
			{
				Role: RoleTool,
				Parts: []Part{{
					Type:       PartToolResult,
					ToolResult: &ToolResult{ID: "c1", Name: "shell", Content: "file.txt"},
				}},
			},
		}

		_, err := Compact(context.Background(), provider, "test-model", "", messages, config)
		if err != nil {
			t.Fatalf("Compact failed: %v", err)
		}

		req := provider.Requests[0]
		lastMsg := req.Messages[len(req.Messages)-1]
		if lastMsg.Role != RoleUser {
			t.Errorf("last message should be user (compaction prompt), got %s", lastMsg.Role)
		}
		prevMsg := req.Messages[len(req.Messages)-2]
		if prevMsg.Role != RoleAssistant {
			t.Errorf("separator should be assistant before compaction prompt, got %s", prevMsg.Role)
		}
	})
}

func TestEstimatedTokensUsesStructuralDeltaAfterLastAssistant(t *testing.T) {
	e := NewEngine(NewMockProvider("test"), nil)

	checkpoint := []Message{
		SystemText("system prompt"),
		UserText("hello world"),
		AssistantText(strings.Repeat("response from model ", 100)),
	}
	e.lastTotalTokens = 150
	e.lastMessageCount = len(checkpoint)

	msgs := append([]Message(nil), checkpoint...)
	msgs = append(msgs, UserText("follow-up question"))

	got := e.estimatedTokens(msgs)
	want := 150 + EstimateMessageTokens([]Message{UserText("follow-up question")})
	if got != want {
		t.Errorf("estimatedTokens (structural delta) = %d, want %d", got, want)
	}
}

func TestEstimatedTokens(t *testing.T) {
	e := NewEngine(NewMockProvider("test"), nil)

	msgs := []Message{
		SystemText("system prompt"),
		UserText("hello world"),
	}

	// With no API data yet, should fall back to pure heuristic
	got := e.estimatedTokens(msgs)
	want := EstimateMessageTokens(msgs)
	if got != want {
		t.Errorf("estimatedTokens (no API data) = %d, want %d", got, want)
	}

	// Simulate API response: 100 input + 50 output. The provider baseline already
	// includes the assistant response, so only structurally trailing messages after
	// the last assistant turn should be estimated heuristically.
	e.lastTotalTokens = 150
	e.lastMessageCount = 2

	// Now add an assistant response and a new user message after the API call.
	msgs = append(msgs, AssistantText("response from model"))
	msgs = append(msgs, UserText("follow-up question"))

	got = e.estimatedTokens(msgs)
	newMsgsEstimate := EstimateMessageTokens([]Message{msgs[len(msgs)-1]})
	want = 150 + newMsgsEstimate
	if got != want {
		t.Errorf("estimatedTokens (with API data) = %d, want %d (150 + %d)", got, want, newMsgsEstimate)
	}
}

func TestEstimatedTokensIgnoresStaleBaselineWithoutAssistantAnchor(t *testing.T) {
	e := NewEngine(NewMockProvider("test"), nil)

	// A provider baseline is only valid if it can be anchored to an assistant turn
	// in the current transcript. Summary-only or cleared histories should not reuse
	// a stale pre-compaction baseline.
	e.lastTotalTokens = 100
	e.lastMessageCount = 5

	msgs := []Message{UserText("short")}
	want := EstimateMessageTokens(msgs)
	got := e.estimatedTokens(msgs)
	if got != want {
		t.Errorf("estimatedTokens (stale unanchored baseline) = %d, want heuristic %d", got, want)
	}
}

func TestEstimatedTokensUsesBaselineWhenMessageCountMatches(t *testing.T) {
	e := NewEngine(NewMockProvider("test"), nil)
	e.lastTotalTokens = 336_000
	e.lastMessageCount = 3

	msgs := []Message{
		SystemText("system"),
		UserText("hello"),
		AssistantText("world"),
	}
	got := e.estimatedTokens(msgs)
	if got != 336_000 {
		t.Fatalf("estimatedTokens (matching baseline) = %d, want %d", got, 336_000)
	}
}

func TestConfigFallbackInputLimit(t *testing.T) {
	// Register config limits for a custom model
	RegisterConfigLimits([]ConfigModelLimit{
		{Provider: "cdck", Model: "custom-model-abc", InputLimit: 150_000, OutputLimit: 50_000},
	})
	defer RegisterConfigLimits(nil) // cleanup

	// Custom model should use config fallback
	if got := InputLimitForModel("custom-model-abc"); got != 150_000 {
		t.Errorf("InputLimitForModel(custom) = %d, want 150000", got)
	}

	// Hardcoded model should still use hardcoded table (not config)
	if got := InputLimitForModel("claude-sonnet-4-6"); got != 980_000 {
		t.Errorf("InputLimitForModel(claude) = %d, want 980000", got)
	}

	// Unknown model with no config should return 0
	if got := InputLimitForModel("totally-unknown"); got != 0 {
		t.Errorf("InputLimitForModel(unknown) = %d, want 0", got)
	}
}

func TestConfigFallbackOutputLimit(t *testing.T) {
	RegisterConfigLimits([]ConfigModelLimit{
		{Provider: "cdck", Model: "custom-model-abc", InputLimit: 150_000, OutputLimit: 50_000},
	})
	defer RegisterConfigLimits(nil)

	if got := OutputLimitForModel("custom-model-abc"); got != 50_000 {
		t.Errorf("OutputLimitForModel(custom) = %d, want 50000", got)
	}

	// Hardcoded model still uses hardcoded table
	if got := OutputLimitForModel("claude-sonnet-4-6"); got != 64_000 {
		t.Errorf("OutputLimitForModel(claude) = %d, want 64000", got)
	}
}

func TestConfigFallbackClampOutputTokens(t *testing.T) {
	RegisterConfigLimits([]ConfigModelLimit{
		{Provider: "cdck", Model: "custom-model-abc", OutputLimit: 50_000},
	})
	defer RegisterConfigLimits(nil)

	// Should clamp to config output limit
	if got := ClampOutputTokens(100_000, "custom-model-abc"); got != 50_000 {
		t.Errorf("ClampOutputTokens(100000, custom) = %d, want 50000", got)
	}

	// Within limit should pass through
	if got := ClampOutputTokens(30_000, "custom-model-abc"); got != 30_000 {
		t.Errorf("ClampOutputTokens(30000, custom) = %d, want 30000", got)
	}
}

func TestConfigFallbackProviderModel(t *testing.T) {
	RegisterConfigLimits([]ConfigModelLimit{
		{Provider: "my-custom-provider", Model: "custom-model-abc", InputLimit: 150_000},
	})
	defer RegisterConfigLimits(nil)

	// Provider model lookup should fall back to config (provider-scoped)
	if got := InputLimitForProviderModel("my-custom-provider", "custom-model-abc"); got != 150_000 {
		t.Errorf("InputLimitForProviderModel(custom) = %d, want 150000", got)
	}
}

func TestConfigFallbackCaseInsensitive(t *testing.T) {
	RegisterConfigLimits([]ConfigModelLimit{
		{Provider: "cdck", Model: "Qwen/Qwen3.5-122B-A10B", InputLimit: 150_000, OutputLimit: 50_000},
	})
	defer RegisterConfigLimits(nil)

	// Exact case match via lowering
	if got := InputLimitForModel("Qwen/Qwen3.5-122B-A10B"); got != 150_000 {
		t.Errorf("InputLimitForModel(mixed case) = %d, want 150000", got)
	}
	if got := InputLimitForModel("qwen/qwen3.5-122b-a10b"); got != 150_000 {
		t.Errorf("InputLimitForModel(lower case) = %d, want 150000", got)
	}
}

func TestConfigFallbackNilCleanup(t *testing.T) {
	RegisterConfigLimits([]ConfigModelLimit{
		{Provider: "cdck", Model: "temp-model", InputLimit: 100_000},
	})
	// Should be registered
	if got := InputLimitForModel("temp-model"); got != 100_000 {
		t.Errorf("before cleanup: InputLimitForModel = %d, want 100000", got)
	}
	// Clear
	RegisterConfigLimits(nil)
	if got := InputLimitForModel("temp-model"); got != 0 {
		t.Errorf("after cleanup: InputLimitForModel = %d, want 0", got)
	}
}

func TestConfigFallbackProviderCollision(t *testing.T) {
	// Two providers use the same model with different limits
	RegisterConfigLimits([]ConfigModelLimit{
		{Provider: "fast-host", Model: "qwen3-coder", InputLimit: 100_000, OutputLimit: 8_000},
		{Provider: "big-host", Model: "qwen3-coder", InputLimit: 200_000, OutputLimit: 50_000},
	})
	defer RegisterConfigLimits(nil)

	// Model-only lookups should return 0 (ambiguous — no wrong answer is safe)
	if got := InputLimitForModel("qwen3-coder"); got != 0 {
		t.Errorf("InputLimitForModel(collision) = %d, want 0 (ambiguous)", got)
	}
	if got := OutputLimitForModel("qwen3-coder"); got != 0 {
		t.Errorf("OutputLimitForModel(collision) = %d, want 0 (ambiguous)", got)
	}

	// Provider-scoped lookups should return the correct value
	if got := InputLimitForProviderModel("fast-host", "qwen3-coder"); got != 100_000 {
		t.Errorf("InputLimitForProviderModel(fast-host) = %d, want 100000", got)
	}
	if got := InputLimitForProviderModel("big-host", "qwen3-coder"); got != 200_000 {
		t.Errorf("InputLimitForProviderModel(big-host) = %d, want 200000", got)
	}

	// Unknown provider with collision should return 0 (falls through to model-only)
	if got := InputLimitForProviderModel("unknown", "qwen3-coder"); got != 0 {
		t.Errorf("InputLimitForProviderModel(unknown, collision) = %d, want 0", got)
	}
}

func TestConfigFallbackProviderNoCollision(t *testing.T) {
	// Two providers with same model and same limits — no collision
	RegisterConfigLimits([]ConfigModelLimit{
		{Provider: "host-a", Model: "shared-model", InputLimit: 150_000, OutputLimit: 50_000},
		{Provider: "host-b", Model: "shared-model", InputLimit: 150_000, OutputLimit: 50_000},
	})
	defer RegisterConfigLimits(nil)

	// Model-only lookups should work (no conflict)
	if got := InputLimitForModel("shared-model"); got != 150_000 {
		t.Errorf("InputLimitForModel(no collision) = %d, want 150000", got)
	}
	if got := OutputLimitForModel("shared-model"); got != 50_000 {
		t.Errorf("OutputLimitForModel(no collision) = %d, want 50000", got)
	}
}

func TestEffortVariantsFor(t *testing.T) {
	tests := []struct {
		model    string
		expected int // number of variants (0 = nil)
	}{
		{"gpt-5", 5},
		{"gpt-5.3-codex", 5},
		{"gpt-5-mini", 5},
		{"gpt-5.2-chat", 5},
		{"claude-sonnet-4-5", 0},
		{"gpt-4o", 0},
		{"o3-mini", 0},
		{"", 0},
	}

	for _, tt := range tests {
		got := EffortVariantsFor(tt.model)
		if len(got) != tt.expected {
			t.Errorf("EffortVariantsFor(%q) returned %d variants, want %d", tt.model, len(got), tt.expected)
		}
	}

	// Check the actual variant values
	variants := EffortVariantsFor("gpt-5")
	expected := []string{"minimal", "low", "medium", "high", "xhigh"}
	for i, v := range variants {
		if v != expected[i] {
			t.Errorf("EffortVariantsFor variant[%d] = %q, want %q", i, v, expected[i])
		}
	}
}

func TestExpandWithEffortVariants(t *testing.T) {
	models := []string{"gpt-5", "claude-sonnet-4-5"}
	expanded := ExpandWithEffortVariants(models)

	// gpt-5 + 5 variants + claude-sonnet-4-5 (no variants) = 7
	if len(expanded) != 7 {
		t.Errorf("ExpandWithEffortVariants returned %d entries, want 7", len(expanded))
	}
	if expanded[0] != "gpt-5" {
		t.Errorf("first entry should be base model, got %q", expanded[0])
	}
	if expanded[1] != "gpt-5-minimal" {
		t.Errorf("second entry should be gpt-5-minimal, got %q", expanded[1])
	}
	if expanded[6] != "claude-sonnet-4-5" {
		t.Errorf("last entry should be claude-sonnet-4-5, got %q", expanded[6])
	}
}

func TestHandoverEndToEnd(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTextResponse("## Objective\nBuild feature X\n\n## Pending Tasks\n1. Implement handler\n2. Add tests")

	messages := []Message{
		UserText("I want to build feature X"),
		AssistantText("Let me explore the codebase first."),
		UserText("Focus on the handler"),
		AssistantText("I found the handler in cmd/handler.go. Here's my plan..."),
	}

	config := DefaultCompactionConfig()
	result, err := Handover(context.Background(), provider, "test-model", "You are a planner.", "You are a developer.", messages, "planner", "developer", config)
	if err != nil {
		t.Fatalf("Handover failed: %v", err)
	}

	if result.Document == "" {
		t.Error("handover document should not be empty")
	}
	if result.SourceAgent != "planner" {
		t.Errorf("source agent = %q, want planner", result.SourceAgent)
	}
	if result.TargetAgent != "developer" {
		t.Errorf("target agent = %q, want developer", result.TargetAgent)
	}

	// NewMessages should have: [system] + [handover doc] + [assistant ack]
	if len(result.NewMessages) != 3 {
		t.Fatalf("new messages count = %d, want 3", len(result.NewMessages))
	}
	if result.NewMessages[0].Role != RoleSystem {
		t.Errorf("first message should be system, got %s", result.NewMessages[0].Role)
	}
	if result.NewMessages[1].Role != RoleUser {
		t.Errorf("second message should be user (handover doc), got %s", result.NewMessages[1].Role)
	}
	if !result.NewMessages[1].CacheAnchor {
		t.Error("handover doc message should have CacheAnchor=true")
	}
	if !strings.Contains(result.NewMessages[1].Parts[0].Text, "[Agent Handover: @planner -> @developer]") {
		t.Error("handover doc should contain handover prefix")
	}
	if result.NewMessages[2].Role != RoleAssistant {
		t.Errorf("third message should be assistant (ack), got %s", result.NewMessages[2].Role)
	}
	if !strings.Contains(result.NewMessages[2].Parts[0].Text, "@planner") {
		t.Error("assistant ack should reference source agent")
	}

	// System prompt should be the NEW agent's system prompt
	if !strings.Contains(result.NewMessages[0].Parts[0].Text, "developer") {
		t.Error("system prompt should be the new agent's prompt")
	}

	// Verify the handover prompt was sent to the provider
	req := provider.Requests[0]
	lastMsg := req.Messages[len(req.Messages)-1]
	if !strings.Contains(lastMsg.Parts[0].Text, "handing over") {
		t.Error("last request message should contain handover prompt")
	}
}

func TestHandoverFromFile(t *testing.T) {
	content := "## Objective\nBuild feature X\n\n## Next Steps\n1. Do the thing"
	result := HandoverFromFile(content, "You are a developer.", "planner", "developer")

	if result.Document != content {
		t.Errorf("document should match input content")
	}
	if result.SourceAgent != "planner" {
		t.Errorf("source agent = %q, want planner", result.SourceAgent)
	}
	if len(result.NewMessages) != 3 {
		t.Fatalf("new messages count = %d, want 3", len(result.NewMessages))
	}
	if !strings.Contains(result.NewMessages[1].Parts[0].Text, content) {
		t.Error("handover doc message should contain file content")
	}
	if !result.NewMessages[1].CacheAnchor {
		t.Error("handover doc should have CacheAnchor=true")
	}
}

func TestHandoverEmptyMessages(t *testing.T) {
	provider := NewMockProvider("test")
	config := DefaultCompactionConfig()

	_, err := Handover(context.Background(), provider, "test-model", "", "", nil, "planner", "developer", config)
	if err == nil {
		t.Error("Handover with nil messages should return error")
	}
}
