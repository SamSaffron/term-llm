package engine

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestTrimMessagesToFit(t *testing.T) {
	sys := core.SystemText("system prompt")
	// Each message is ~5 tokens (20 bytes / 4)
	u1 := core.UserText("first user message!")
	a1 := core.AssistantText("first assistant msg")
	u2 := core.UserText("second user message")
	a2 := core.AssistantText("second asst message")
	u3 := core.UserText("third user message!!")

	t.Run("fits within budget", func(t *testing.T) {
		msgs := []core.Message{sys, u1, a1, u2, a2, u3}
		result := trimMessagesToFit(msgs, 10000)
		if len(result) != 6 {
			t.Errorf("expected 6 messages (no trimming), got %d", len(result))
		}
	})

	t.Run("trims oldest first", func(t *testing.T) {
		msgs := []core.Message{sys, u1, a1, u2, a2, u3}
		// Budget for system + ~2 messages
		budget := EstimateMessageTokens([]core.Message{sys}) + EstimateMessageTokens([]core.Message{u3}) + 1
		result := trimMessagesToFit(msgs, budget)
		if result[0].Role != core.RoleSystem {
			t.Error("should preserve system message")
		}
		// Last message should be u3
		last := result[len(result)-1]
		if last.Parts[0].Text != u3.Parts[0].Text {
			t.Errorf("last message should be u3, got %q", last.Parts[0].Text)
		}
	})

	t.Run("skips leading assistant after trim", func(t *testing.T) {
		msgs := []core.Message{sys, u1, a1, u2}
		// Budget fits system + a1 + u2, but a1 is assistant — should be skipped
		budget := EstimateMessageTokens([]core.Message{sys, a1, u2}) + 1
		result := trimMessagesToFit(msgs, budget)
		// After trimming u1 and skipping leading a1, should have sys + u2
		if len(result) < 2 {
			t.Fatalf("expected at least 2 messages, got %d", len(result))
		}
		if result[0].Role != core.RoleSystem {
			t.Error("first should be system")
		}
		if result[1].Role != core.RoleUser {
			t.Errorf("should start with user after system, got %s", result[1].Role)
		}
	})

	t.Run("no system message", func(t *testing.T) {
		msgs := []core.Message{u1, a1, u2}
		budget := EstimateMessageTokens([]core.Message{u2}) + 1
		result := trimMessagesToFit(msgs, budget)
		if len(result) != 1 || result[0].Parts[0].Text != u2.Parts[0].Text {
			t.Errorf("should trim to just u2, got %d messages", len(result))
		}
	})
}

func TestTrimMessagesToFitPreservesAnchor(t *testing.T) {
	sys := core.SystemText("system")
	summary := core.Message{
		Role:        core.RoleUser,
		Parts:       []core.Part{{Type: core.PartText, Text: SummaryPrefix + "previous summary"}},
		CacheAnchor: true,
	}
	ack := core.AssistantText("I've reviewed the context summary.")
	u1 := core.UserText("first user message!")
	a1 := core.AssistantText("first assistant msg")
	u2 := core.UserText("second user message")

	t.Run("preserves anchor block", func(t *testing.T) {
		msgs := []core.Message{sys, summary, ack, u1, a1, u2}
		// Budget fits system + summary + ack + u2 but not u1+a1
		budget := EstimateMessageTokens([]core.Message{sys, summary, ack, u2}) + 1
		result := trimMessagesToFit(msgs, budget)
		// Should have: sys, summary, ack, u2
		if len(result) < 4 {
			t.Fatalf("expected at least 4 messages, got %d", len(result))
		}
		if result[0].Role != core.RoleSystem {
			t.Error("first should be system")
		}
		if !result[1].CacheAnchor {
			t.Error("second should be cache-anchored summary")
		}
		if result[2].Role != core.RoleAssistant {
			t.Error("third should be assistant ack")
		}
		if result[len(result)-1].Parts[0].Text != u2.Parts[0].Text {
			t.Errorf("last should be u2, got %q", result[len(result)-1].Parts[0].Text)
		}
	})

	t.Run("drops anchor when too large", func(t *testing.T) {
		bigSummary := core.Message{
			Role:        core.RoleUser,
			Parts:       []core.Part{{Type: core.PartText, Text: strings.Repeat("x", 10000)}},
			CacheAnchor: true,
		}
		msgs := []core.Message{sys, bigSummary, ack, u1, u2}
		// Budget only fits system + u2
		budget := EstimateMessageTokens([]core.Message{sys, u2}) + 1
		result := trimMessagesToFit(msgs, budget)
		if result[0].Role != core.RoleSystem {
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
	sys := core.SystemText("system")
	// A single user message that's way too large
	bigMsg := core.UserText(strings.Repeat("x", 40000))
	msgs := []core.Message{sys, bigMsg}

	// Budget that fits system + some content but not all of bigMsg
	budget := EstimateMessageTokens([]core.Message{sys}) + 500
	result := trimMessagesToFit(msgs, budget)

	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0].Role != core.RoleSystem {
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

func TestReconstructHistory(t *testing.T) {
	recentUser := []core.Message{core.UserText("recent question")}

	result := reconstructHistory("system prompt", "summary of conversation", recentUser)

	// Should be: system + summary + assistant ack + recent user
	if len(result) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(result))
	}

	if result[0].Role != core.RoleSystem {
		t.Errorf("first message should be system, got %s", result[0].Role)
	}
	if result[0].Parts[0].Text != "system prompt" {
		t.Errorf("system prompt mismatch")
	}

	if result[1].Role != core.RoleUser {
		t.Errorf("second message should be user (summary), got %s", result[1].Role)
	}
	if !strings.Contains(result[1].Parts[0].Text, "summary of conversation") {
		t.Errorf("summary message should contain summary text")
	}
	if !strings.Contains(result[1].Parts[0].Text, SummaryPrefix) {
		t.Errorf("summary message should contain prefix")
	}
	if !result[1].CacheAnchor {
		t.Errorf("summary message should have CacheAnchor=true for stable cache breakpoint")
	}

	if result[2].Role != core.RoleAssistant {
		t.Errorf("third message should be assistant ack, got %s", result[2].Role)
	}

	if result[3].Role != core.RoleUser {
		t.Errorf("fourth message should be user, got %s", result[3].Role)
	}
	if result[3].Parts[0].Text != "recent question" {
		t.Errorf("recent user message mismatch")
	}
}

func TestConversationEndsAtSettledAssistantIgnoresHistoricalOrphanedToolCall(t *testing.T) {
	messages := []core.Message{
		core.UserText("old turn"),
		{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "orphan", Name: "shell"}}}},
		core.UserText("new turn"),
		core.AssistantText("settled answer"),
	}
	if !conversationEndsAtSettledAssistant(messages) {
		t.Fatal("an orphaned call before the latest user turn should not hide the current settled boundary")
	}
}

func TestConversationEndsAtSettledAssistantIgnoresTrailingEvents(t *testing.T) {
	messages := []core.Message{
		core.UserText("question"),
		core.AssistantText("answer"),
		{Role: core.RoleEvent, Parts: []core.Part{{Type: core.PartText, Text: "timeline marker"}}},
	}
	if !conversationEndsAtSettledAssistant(messages) {
		t.Fatal("trailing event should not hide a settled assistant boundary")
	}
}

func TestReconstructHistoryNoSystem(t *testing.T) {
	result := reconstructHistory("", "summary", []core.Message{core.UserText("q")})

	// Without system prompt: summary + ack + user = 3
	if len(result) != 3 {
		t.Fatalf("expected 3 messages without system prompt, got %d", len(result))
	}
	if result[0].Role != core.RoleUser {
		t.Errorf("first message should be user (summary), got %s", result[0].Role)
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

func TestReconstructHistoryOmitsAckBeforeAssistantRawSuffix(t *testing.T) {
	result := reconstructHistory("", "summary", []core.Message{core.AssistantText("continuing from retained state")})

	if len(result) != 2 {
		t.Fatalf("expected summary + retained assistant, got %d messages", len(result))
	}
	if result[0].Role != core.RoleUser || !strings.Contains(core.MessageText(result[0]), SummaryPrefix) {
		t.Fatalf("first message should be cache-anchored summary user, got role=%s text=%q", result[0].Role, core.MessageText(result[0]))
	}
	if result[1].Role != core.RoleAssistant || core.MessageText(result[1]) != "continuing from retained state" {
		t.Fatalf("second message should be retained assistant, got role=%s text=%q", result[1].Role, core.MessageText(result[1]))
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

func TestAudioMediaSummarizesKinds(t *testing.T) {
	audio := core.MediaArtifact{Reference: "0123456789abcdef0123456789abcdef", SourcePath: "/tmp/speech.mp3", MediaType: "audio/mpeg", Name: "speech.mp3"}
	toolResult := &core.ToolResult{ID: "audio-call", Name: "show_media", Content: "ready", Media: []core.MediaArtifact{audio}}
	summary := compactToolResultSummary(toolResult)
	if !strings.Contains(summary, "1 audio(s)") || strings.Contains(summary, "image(s)") {
		t.Fatalf("summary=%q", summary)
	}
	mixed := compactMediaSummary([]core.MediaArtifact{audio, {MediaType: "image/png"}, {MediaType: "video/mp4"}})
	if strings.Join(mixed, ",") != "1 image(s),1 video(s),1 audio(s)" {
		t.Fatalf("mixed=%v", mixed)
	}
}

func TestBuildCompactionStaticInfoRespectsBudget(t *testing.T) {
	messages := []core.Message{
		core.UserText("latest request " + strings.Repeat("x", 2000)),
		core.AssistantText("older assistant prose " + strings.Repeat("y", 2000)),
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

	got = BuildCompactionStaticInfo([]core.Message{core.UserText(strings.Repeat("z", 40_000))}, 0)
	if gotLen := len([]rune(got)); gotLen > maxPreviousTurnsChars {
		t.Fatalf("fallback previous turns length = %d, want <= %d", gotLen, maxPreviousTurnsChars)
	}

	got = buildPreviousTurnsBlock([]core.Message{core.UserText(strings.Repeat("q", 100_000))}, 100_000)
	if gotLen := len([]rune(got)); gotLen > maxPreviousTurnsChars {
		t.Fatalf("direct previous turns length = %d, want <= %d", gotLen, maxPreviousTurnsChars)
	}

	maxInt := int(^uint(0) >> 1)
	got = BuildCompactionStaticInfo([]core.Message{core.UserText(strings.Repeat("w", 100_000))}, maxInt)
	if gotLen := len([]rune(got)); gotLen > maxPreviousTurnsChars {
		t.Fatalf("large-limit previous turns length = %d, want <= %d", gotLen, maxPreviousTurnsChars)
	}
}

func TestParseHandoverDocument(t *testing.T) {
	for _, tt := range []struct {
		name, text, source, target, body string
		ok                               bool
	}{
		{"agents", handoverPrefix("dev", "reviewer") + "briefing\n\nverbatim\n", "dev", "reviewer", "briefing\n\nverbatim\n", true},
		{"models", handoverPrefix("openai:gpt-5-high", "anthropic:claude-max") + "doc", "openai:gpt-5-high", "anthropic:claude-max", "doc", true},
		{"empty body", handoverPrefix("a", "b"), "a", "b", "", true},
		{name: "ordinary", text: "briefing"},
		{name: "not prefix", text: "intro\n" + handoverPrefix("a", "b")},
		{name: "missing separator", text: "[Agent Handover: @a -> @b]\ndoc"},
		{name: "missing source", text: handoverPrefix("", "b")},
		{name: "missing target", text: handoverPrefix("a", "")},
		{name: "multiline name", text: handoverPrefix("a\nb", "c")},
		{name: "ambiguous names", text: handoverPrefix("a", "b -> @c")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source, target, body, ok := ParseHandoverDocument(tt.text)
			if source != tt.source || target != tt.target || body != tt.body || ok != tt.ok {
				t.Fatalf("parse = %q, %q, %q, %v", source, target, body, ok)
			}
		})
	}
}

func TestIsHandoverAcknowledgement(t *testing.T) {
	for _, tt := range []struct {
		text string
		want bool
	}{
		{core.MessageText(ReconstructHandoverHistory("", "doc", "dev", "reviewer")[1]), true},
		{handoverAcknowledgement("openai:gpt-5-high"), true},
		{handoverAcknowledgement(""), false},
		{handoverAcknowledgement("a\nb"), false},
		{"ordinary response", false},
		{handoverAcknowledgement("dev") + "\nextra", false},
		{"prefix " + handoverAcknowledgement("dev"), false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			if got := IsHandoverAcknowledgement(tt.text); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
