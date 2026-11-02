package session

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestApplyReasoningPersistencePolicyDropsSummaryContentWhenDisabled(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	cfg.PersistSummaries = false
	msg := core.Message{Role: core.RoleAssistant, Parts: []core.Part{{
		Type:                      core.PartText,
		Text:                      "answer",
		ReasoningContent:          "summary body",
		ReasoningKind:             core.ReasoningKindSummary,
		ReasoningSummaryTitle:     "Summary title",
		ReasoningItemID:           "rs_1",
		ReasoningEncryptedContent: "enc_1",
	}}}

	got := ApplyReasoningPersistencePolicy(msg, cfg)
	part := got.Parts[0]
	if part.ReasoningContent != "" || part.ReasoningSummaryTitle != "" {
		t.Fatalf("summary display content should be stripped, got content=%q title=%q", part.ReasoningContent, part.ReasoningSummaryTitle)
	}
	if part.ReasoningItemID != "rs_1" || part.ReasoningEncryptedContent != "enc_1" || part.ReasoningKind != core.ReasoningKindSummary {
		t.Fatalf("replay metadata should be preserved, got %#v", part)
	}
	if msg.Parts[0].ReasoningContent == "" {
		t.Fatal("policy should not mutate the original message")
	}
}

func TestApplyReasoningPersistencePolicyTreatsLegacyEmptyKindAsSummary(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	cfg.PersistSummaries = false
	msg := core.Message{Role: core.RoleAssistant, Parts: []core.Part{{
		Type:             core.PartText,
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
	msg := core.Message{Role: core.RoleAssistant, Parts: []core.Part{{
		Type:             core.PartText,
		Text:             "answer",
		ReasoningContent: "raw replay text",
		ReasoningKind:    core.ReasoningKindRaw,
	}}}

	got := ApplyReasoningPersistencePolicy(msg, cfg)
	if got.Parts[0].ReasoningContent != "raw replay text" {
		t.Fatalf("raw reasoning should be preserved for replay/debug, got %q", got.Parts[0].ReasoningContent)
	}
}

func TestApplyReasoningPersistencePolicyFiltersOrderedBurstsIndependently(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	cfg.PersistSummaries = false
	for _, kind := range []core.ReasoningKind{core.ReasoningKindSummary, ""} {
		t.Run(string(kind), func(t *testing.T) {
			display := &core.ReasoningDisplay{Content: "burst summary", SummaryParts: []string{"burst summary"}, Kind: kind, SummaryTitle: "Burst"}
			msg := core.Message{Role: core.RoleAssistant, Parts: []core.Part{{
				Type: core.PartText, ReasoningDisplay: display, ReasoningContent: "raw replay", ReasoningKind: core.ReasoningKindRaw,
				ReasoningItemID: "rs_1", ReasoningEncryptedContent: "opaque",
			}}}
			if kept := ApplyReasoningPersistencePolicy(msg, config.DefaultReasoningConfig()).Parts[0]; kept.ReasoningDisplay != display {
				t.Fatalf("enabled persistence changed ordered display: %#v", kept)
			}
			got := ApplyReasoningPersistencePolicy(msg, cfg).Parts[0]
			if got.ReasoningDisplay != nil || !got.ReasoningReplayOnly || core.ReasoningForDisplay(got) != nil {
				t.Fatalf("stripped burst must not expose replay metadata: %#v", got)
			}
			if got.ReasoningContent != "raw replay" || got.ReasoningItemID != "rs_1" || got.ReasoningEncryptedContent != "opaque" {
				t.Fatalf("replay metadata changed: %#v", got)
			}
			if msg.Parts[0].ReasoningDisplay != display || display.Content != "burst summary" || display.SummaryParts[0] != "burst summary" || display.SummaryTitle != "Burst" || msg.Parts[0].ReasoningReplayOnly {
				t.Fatalf("original display metadata was mutated: %#v", msg.Parts[0])
			}
		})
	}
}

func TestApplyReasoningPersistencePolicyKeepsRawOrderedBurst(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	cfg.PersistSummaries = false
	display := &core.ReasoningDisplay{Content: "raw burst", Kind: core.ReasoningKindRaw}
	msg := core.Message{Role: core.RoleAssistant, Parts: []core.Part{{
		Type: core.PartText, ReasoningDisplay: display, ReasoningReplayOnly: true,
		ReasoningContent: "aggregate summary", ReasoningSummaryParts: []string{"aggregate summary"}, ReasoningKind: core.ReasoningKindSummary, ReasoningSummaryTitle: "Aggregate",
	}}}
	got := ApplyReasoningPersistencePolicy(msg, cfg).Parts[0]
	if got.ReasoningDisplay != display || !got.ReasoningReplayOnly {
		t.Fatalf("raw burst and replay marker must survive: %#v", got)
	}
	if got.ReasoningContent != "" || got.ReasoningSummaryParts != nil || got.ReasoningSummaryTitle != "" {
		t.Fatalf("aggregate summary should still be stripped: %#v", got)
	}
	if msg.Parts[0].ReasoningContent != "aggregate summary" || msg.Parts[0].ReasoningSummaryParts[0] != "aggregate summary" {
		t.Fatal("original aggregate metadata was mutated")
	}
}
