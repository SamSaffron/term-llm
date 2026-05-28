package reasoning

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestReasoningPolicyDefaultDisplaysNonEncryptedProviderReasoning(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	if got := EffectiveDisplay(cfg); got != config.ReasoningDisplayCollapsed {
		t.Fatalf("EffectiveDisplay(default) = %q", got)
	}
	if !IsDisplayable(KindSummary, cfg) {
		t.Fatal("summary should be displayable by default")
	}
	if !IsDisplayable(KindRaw, cfg) {
		t.Fatal("provider-marked raw thinking should be displayable by default")
	}
	if IsDisplayable(KindUnknown, cfg) || IsDisplayable(KindEncrypted, cfg) {
		t.Fatal("unknown/encrypted reasoning must not be displayable by default")
	}

	summaryOnly := cfg
	summaryOnly.Source = config.ReasoningSourceSummaryOnly
	if IsDisplayable(KindRaw, summaryOnly) {
		t.Fatal("source=summary_only should hide raw provider thinking")
	}
}

func TestReasoningPolicyRawVisibleInUIButRawModeStillNeedsGate(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	if !IsDisplayable(KindRaw, cfg) {
		t.Fatal("provider-marked raw thinking should be visible in the default UI")
	}
	if !HistoryVisible(KindRaw, cfg) {
		t.Fatal("provider-marked raw thinking should render as collapsed history by default")
	}
	if HistoryExpanded(KindRaw, cfg) {
		t.Fatal("default raw thinking history should be collapsed until details are expanded")
	}

	cfg.Display = config.ReasoningDisplayRaw
	cfg.Source = config.ReasoningSourceAll
	if got := EffectiveDisplay(cfg); got != config.ReasoningDisplayCollapsed {
		t.Fatalf("raw display without raw gate should fall back to collapsed, got %q", got)
	}
	if !RawDisplayBlocked(cfg) {
		t.Fatal("raw display without raw gate should be reported as blocked")
	}
	if !IsDisplayable(KindRaw, cfg) {
		t.Fatal("raw thinking remains visible even when raw mode is blocked")
	}

	cfg.Raw = true
	if got := EffectiveDisplay(cfg); got != config.ReasoningDisplayRaw {
		t.Fatalf("raw display with raw gate = %q", got)
	}
	if !HistoryExpanded(KindRaw, cfg) {
		t.Fatal("display=raw with raw gate should expand raw thinking")
	}
}

func TestReasoningPolicyExportRawRequiresSourceAndRawGate(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	cfg.Export = config.ReasoningExportRaw
	cfg.Raw = true
	cfg.Source = config.ReasoningSourceSummaryOnly
	if ExportRaw(cfg) {
		t.Fatal("raw export should require source=all")
	}

	cfg.Source = config.ReasoningSourceAll
	if !ExportRaw(cfg) {
		t.Fatal("raw export should be allowed when export=raw, raw=true, source=all")
	}
}

func TestReasoningPolicyTranscriptOnlyDoesNotRenderHistory(t *testing.T) {
	cfg := config.DefaultReasoningConfig()
	cfg.History = config.ReasoningHistoryTranscriptOnly
	if HistoryVisible(KindSummary, cfg) {
		t.Fatal("transcript_only summaries should not render in live history")
	}
	if !ExportSummaries(config.ReasoningConfig{Export: config.ReasoningExportSummaries}) {
		t.Fatal("export=summaries should include summaries")
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxRunes int
		want     string
	}{
		{name: "disabled", text: "abcdef", maxRunes: 0, want: "abcdef"},
		{name: "short", text: "abc", maxRunes: 5, want: "abc"},
		{name: "exact", text: "abc", maxRunes: 3, want: "abc"},
		{name: "ascii", text: "abcdef", maxRunes: 3, want: "abc"},
		{name: "unicode", text: "a🙂bc", maxRunes: 2, want: "a🙂"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TruncateRunes(tt.text, tt.maxRunes); got != tt.want {
				t.Fatalf("TruncateRunes(%q, %d) = %q, want %q", tt.text, tt.maxRunes, got, tt.want)
			}
		})
	}
}
