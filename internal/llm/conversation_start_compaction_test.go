package llm

import (
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
)

func TestCompactionResultPreservesConversationStart(t *testing.T) {
	start := ConversationStartMessage(time.Date(2026, time.September, 9, 13, 42, 0, 0, time.FixedZone("AEST", 10*60*60)))
	platform := PlatformContextMessage("web mode context")
	messages := []core.Message{
		core.SystemText("old system"),
		platform,
		start,
		core.UserText("first question"),
		core.AssistantText("first answer"),
		core.UserText("latest question"),
		core.AssistantText("latest answer"),
	}

	result := CompactionResultFromBrief("new system", "continue the task", messages, engine.CompactionConfig{})
	if result == nil {
		t.Fatal("nil compaction result")
	}
	preserved, ok := ConversationStartFrom(result.NewMessages)
	if !ok {
		t.Fatalf("conversation start missing from compacted history: %#v", result.NewMessages)
	}
	if got, want := core.MessageText(preserved), core.MessageText(start); got != want {
		t.Fatalf("preserved text = %q, want %q", got, want)
	}
	preservedPlatform, ok := PlatformContextFrom(result.NewMessages)
	if !ok || core.MessageText(preservedPlatform) != core.MessageText(platform) {
		t.Fatalf("platform context missing from compacted history: %#v", result.NewMessages)
	}
	if len(result.NewMessages) < 4 || result.NewMessages[0].Role != core.RoleSystem || !IsPlatformContextMessage(result.NewMessages[1]) || !IsConversationStartMessage(result.NewMessages[2]) || result.NewMessages[3].Role != core.RoleUser {
		t.Fatalf("compacted ordering = %#v", result.NewMessages)
	}

	second := CompactionResultFromBrief("newer system", "continue again", result.NewMessages, engine.CompactionConfig{})
	startCount, platformCount := 0, 0
	for _, message := range second.NewMessages {
		if IsConversationStartMessage(message) {
			startCount++
		}
		if IsPlatformContextMessage(message) {
			platformCount++
		}
	}
	if startCount != 1 || platformCount != 1 {
		t.Fatalf("durable context counts after second compaction = (start %d, platform %d), want (1, 1): %#v", startCount, platformCount, second.NewMessages)
	}
}
