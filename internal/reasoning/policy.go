package reasoning

import (
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

const (
	KindUnknown   = "unknown"
	KindSummary   = "summary"
	KindRaw       = "raw"
	KindEncrypted = "encrypted"
)

// EffectiveDisplay resolves display=auto to the current concrete behavior and
// applies the raw safety gate. If raw display is requested without raw=true, it
// falls back to collapsed summary display.
func EffectiveDisplay(cfg config.ReasoningConfig) string {
	display := strings.ToLower(strings.TrimSpace(cfg.Display))
	switch display {
	case "", config.ReasoningDisplayAuto:
		return config.ReasoningDisplayCollapsed
	case config.ReasoningDisplayRaw:
		if cfg.Raw {
			return config.ReasoningDisplayRaw
		}
		return config.ReasoningDisplayCollapsed
	case config.ReasoningDisplayOff,
		config.ReasoningDisplayStatus,
		config.ReasoningDisplayCollapsed,
		config.ReasoningDisplayExpanded:
		return display
	default:
		return config.ReasoningDisplayCollapsed
	}
}

// RawDisplayBlocked reports display=raw was requested without the explicit raw
// gate. Chat uses this to show a one-time warning while falling back to the
// normal collapsed interactive thought UI.
func RawDisplayBlocked(cfg config.ReasoningConfig) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Display), config.ReasoningDisplayRaw) && !cfg.Raw
}

func NormalizeKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case KindSummary:
		return KindSummary
	case KindRaw:
		return KindRaw
	case KindEncrypted:
		return KindEncrypted
	default:
		return KindUnknown
	}
}

func SourceAllowsSummary(cfg config.ReasoningConfig) bool {
	source := strings.ToLower(strings.TrimSpace(cfg.Source))
	return source == "" || source == config.ReasoningSourceSummaryOnly || source == config.ReasoningSourceSummaryOrProviderSafe || source == config.ReasoningSourceAll
}

func SourceAllowsRaw(cfg config.ReasoningConfig) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Source), config.ReasoningSourceAll)
}

// SourceAllowsProviderThinking reports whether interactive surfaces may show
// provider-marked non-encrypted thinking blocks. This is intentionally broader
// than SourceAllowsRaw: raw export/replay still requires source=all, while the
// default summary_or_provider_safe source permits provider-classified thinking
// to render as a normal collapsed Thought/Thinking block.
func SourceAllowsProviderThinking(cfg config.ReasoningConfig) bool {
	source := strings.ToLower(strings.TrimSpace(cfg.Source))
	return source == "" || source == config.ReasoningSourceSummaryOrProviderSafe || source == config.ReasoningSourceAll
}

// IsDisplayable reports whether classified, non-encrypted reasoning can be
// shown for the resolved interactive UI policy. Encrypted and unknown content
// remain hidden. Export has a separate stricter raw gate.
func IsDisplayable(kind string, cfg config.ReasoningConfig) bool {
	switch NormalizeKind(kind) {
	case KindSummary:
		return EffectiveDisplay(cfg) != config.ReasoningDisplayOff && SourceAllowsSummary(cfg)
	case KindRaw:
		return EffectiveDisplay(cfg) != config.ReasoningDisplayOff && SourceAllowsProviderThinking(cfg)
	default:
		return false
	}
}

func StatusEnabled(cfg config.ReasoningConfig) bool {
	if EffectiveDisplay(cfg) == config.ReasoningDisplayOff {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(cfg.Status))
	return s != config.ReasoningStatusNone
}

func HistoryMode(cfg config.ReasoningConfig) string {
	display := EffectiveDisplay(cfg)
	if display == config.ReasoningDisplayOff || display == config.ReasoningDisplayStatus {
		return config.ReasoningHistoryNone
	}
	history := strings.ToLower(strings.TrimSpace(cfg.History))
	switch history {
	case config.ReasoningHistoryNone, config.ReasoningHistoryCollapsed, config.ReasoningHistoryExpanded, config.ReasoningHistoryTranscriptOnly:
		return history
	default:
		if display == config.ReasoningDisplayExpanded || display == config.ReasoningDisplayRaw {
			return config.ReasoningHistoryExpanded
		}
		return config.ReasoningHistoryCollapsed
	}
}

func HistoryVisible(kind string, cfg config.ReasoningConfig) bool {
	if !cfg.PersistSummaries && NormalizeKind(kind) == KindSummary {
		return false
	}
	if !IsDisplayable(kind, cfg) {
		return false
	}
	mode := HistoryMode(cfg)
	return mode == config.ReasoningHistoryCollapsed || mode == config.ReasoningHistoryExpanded
}

func HistoryExpanded(kind string, cfg config.ReasoningConfig) bool {
	if !HistoryVisible(kind, cfg) {
		return false
	}
	display := EffectiveDisplay(cfg)
	return HistoryMode(cfg) == config.ReasoningHistoryExpanded || display == config.ReasoningDisplayExpanded || (NormalizeKind(kind) == KindRaw && display == config.ReasoningDisplayRaw)
}

func ExportSummaries(cfg config.ReasoningConfig) bool {
	// Provider summaries are display-safe by definition; source only gates raw
	// thinking export via ExportRaw.
	return strings.EqualFold(strings.TrimSpace(cfg.Export), config.ReasoningExportSummaries) || strings.EqualFold(strings.TrimSpace(cfg.Export), config.ReasoningExportRaw)
}

func ExportRaw(cfg config.ReasoningConfig) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Export), config.ReasoningExportRaw) && cfg.Raw && SourceAllowsRaw(cfg)
}

func TruncateRunes(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return text
	}
	count := 0
	for i := range text {
		if count >= maxRunes {
			var b strings.Builder
			b.Grow(i)
			written := 0
			for _, prefixRune := range text[:i] {
				if written >= maxRunes {
					break
				}
				b.WriteRune(prefixRune)
				written++
			}
			return b.String()
		}
		count++
	}
	return text
}

func LimitReasoningText(kind string, text string, cfg config.ReasoningConfig) string {
	switch NormalizeKind(kind) {
	case KindSummary:
		return TruncateRunes(text, cfg.MaxSummaryChars)
	case KindRaw:
		return TruncateRunes(text, cfg.MaxRawChars)
	default:
		return text
	}
}

func SummaryTitle(text string, cfg config.ReasoningConfig) string {
	if !cfg.ExtractTitles {
		return ""
	}
	return ParseReasoningSummary(text).Title
}

func SummaryBody(text string, cfg config.ReasoningConfig) string {
	if !cfg.ExtractTitles {
		return strings.TrimSpace(text)
	}
	parsed := ParseReasoningSummary(text)
	if strings.TrimSpace(parsed.Body) != "" {
		return parsed.Body
	}
	return strings.TrimSpace(text)
}
