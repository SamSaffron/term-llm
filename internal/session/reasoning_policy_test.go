package session

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

func TestApplyReasoningPersistencePolicyDropsSummaryContentWhenDisabled(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	cfg.PersistSummaries = false
	msg := llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
		Type:                      llm.PartText,
		Text:                      "answer",
		ReasoningContent:          "summary body",
		ReasoningKind:             llm.ReasoningKindSummary,
		ReasoningSummaryTitle:     "Summary title",
		ReasoningItemID:           "rs_1",
		ReasoningEncryptedContent: "enc_1",
	}}}

	got := ApplyReasoningPersistencePolicy(msg, cfg)
	part := got.Parts[0]
	if part.ReasoningContent != "" || part.ReasoningSummaryTitle != "" {
		t.Fatalf("summary display content should be stripped, got content=%q title=%q", part.ReasoningContent, part.ReasoningSummaryTitle)
	}
	if part.ReasoningItemID != "rs_1" || part.ReasoningEncryptedContent != "enc_1" || part.ReasoningKind != llm.ReasoningKindSummary {
		t.Fatalf("replay metadata should be preserved, got %#v", part)
	}
	if msg.Parts[0].ReasoningContent == "" {
		t.Fatal("policy should not mutate the original message")
	}
}

func TestApplyReasoningPersistencePolicyTreatsLegacyEmptyKindAsSummary(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	cfg.PersistSummaries = false
	msg := llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
		Type:             llm.PartText,
		Text:             "answer",
		ReasoningContent: "legacy summary body",
	}}}

	got := ApplyReasoningPersistencePolicy(msg, cfg)
	if got.Parts[0].ReasoningContent != "" {
		t.Fatalf("legacy summary content should be stripped when persistence is disabled, got %q", got.Parts[0].ReasoningContent)
	}
	if msg.Parts[0].ReasoningContent == "" {
		t.Fatal("policy should not mutate the original legacy message")
	}
}

func TestApplyReasoningPersistencePolicyKeepsRawReasoning(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	cfg.PersistSummaries = false
	msg := llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{
		Type:             llm.PartText,
		Text:             "answer",
		ReasoningContent: "raw replay text",
		ReasoningKind:    llm.ReasoningKindRaw,
	}}}

	got := ApplyReasoningPersistencePolicy(msg, cfg)
	if got.Parts[0].ReasoningContent != "raw replay text" {
		t.Fatalf("raw reasoning should be preserved for replay/debug, got %q", got.Parts[0].ReasoningContent)
	}
}
