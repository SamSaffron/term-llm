package config

import (
	"os"
	"strings"
)

// Reasoning display policy values.
const (
	ReasoningDisplayAuto      = "auto"
	ReasoningDisplayOff       = "off"
	ReasoningDisplayStatus    = "status"
	ReasoningDisplayCollapsed = "collapsed"
	ReasoningDisplayExpanded  = "expanded"
	ReasoningDisplayRaw       = "raw"

	ReasoningSourceSummaryOnly           = "summary_only"
	ReasoningSourceSummaryOrProviderSafe = "summary_or_provider_safe"
	ReasoningSourceAll                   = "all"

	ReasoningStatusNone    = "none"
	ReasoningStatusGeneric = "generic"
	ReasoningStatusTitle   = "title"
	ReasoningStatusSummary = "summary"

	ReasoningHistoryNone           = "none"
	ReasoningHistoryCollapsed      = "collapsed"
	ReasoningHistoryExpanded       = "expanded"
	ReasoningHistoryTranscriptOnly = "transcript_only"

	ReasoningExportNever     = "never"
	ReasoningExportAsk       = "ask"
	ReasoningExportSummaries = "summaries"
	ReasoningExportRaw       = "raw"

	ReasoningInherit = "inherit"
)

// ReasoningSurfaceConfig holds optional per-surface overrides for reasoning UI.
type ReasoningSurfaceConfig struct {
	Display string `mapstructure:"display" yaml:"display,omitempty"`
	Status  string `mapstructure:"status" yaml:"status,omitempty"`
	History string `mapstructure:"history" yaml:"history,omitempty"`
	Export  string `mapstructure:"export" yaml:"export,omitempty"`
	Raw     *bool  `mapstructure:"raw" yaml:"raw,omitempty"`
}

// ReasoningConfig controls display/export of provider reasoning summaries and
// raw thinking. It is intentionally separate from provider reasoning effort.
type ReasoningConfig struct {
	Display          string                 `mapstructure:"display" yaml:"display,omitempty"`
	Source           string                 `mapstructure:"source" yaml:"source,omitempty"`
	Status           string                 `mapstructure:"status" yaml:"status,omitempty"`
	History          string                 `mapstructure:"history" yaml:"history,omitempty"`
	Export           string                 `mapstructure:"export" yaml:"export,omitempty"`
	Raw              bool                   `mapstructure:"raw" yaml:"raw,omitempty"`
	MaxSummaryChars  int                    `mapstructure:"max_summary_chars" yaml:"max_summary_chars,omitempty"`
	MaxRawChars      int                    `mapstructure:"max_raw_chars" yaml:"max_raw_chars,omitempty"`
	ExtractTitles    bool                   `mapstructure:"extract_titles" yaml:"extract_titles,omitempty"`
	HiddenLabel      string                 `mapstructure:"hidden_label" yaml:"hidden_label,omitempty"`
	PersistSummaries bool                   `mapstructure:"persist_summaries" yaml:"persist_summaries,omitempty"`
	Chat             ReasoningSurfaceConfig `mapstructure:"chat" yaml:"chat,omitempty"`
	Ask              ReasoningSurfaceConfig `mapstructure:"ask" yaml:"ask,omitempty"`
	Serve            ReasoningSurfaceConfig `mapstructure:"serve" yaml:"serve,omitempty"`
	Jobs             ReasoningSurfaceConfig `mapstructure:"jobs" yaml:"jobs,omitempty"`

	// Presence markers let programmatic configs explicitly set false for boolean
	// options while still allowing partial configs to inherit defaults. Load
	// sets these when the config file has a value; direct callers can set them
	// when they need a false override.
	RawSet              bool `mapstructure:"-" yaml:"-"`
	ExtractTitlesSet    bool `mapstructure:"-" yaml:"-"`
	PersistSummariesSet bool `mapstructure:"-" yaml:"-"`

	// Track schema defaults separately from explicit display settings so serve's
	// off default does not mask an explicitly configured top-level policy.
	displayDefaulted      bool
	serveDisplayDefaulted bool
}

// DefaultReasoningConfig returns the safe, useful default reasoning display policy.
func DefaultReasoningConfig() ReasoningConfig {
	return ReasoningConfig{
		Display:          defaultString("reasoning.display"),
		Source:           defaultString("reasoning.source"),
		Status:           defaultString("reasoning.status"),
		History:          defaultString("reasoning.history"),
		Export:           defaultString("reasoning.export"),
		Raw:              defaultBool("reasoning.raw"),
		MaxSummaryChars:  defaultInt("reasoning.max_summary_chars"),
		MaxRawChars:      defaultInt("reasoning.max_raw_chars"),
		ExtractTitles:    defaultBool("reasoning.extract_titles"),
		HiddenLabel:      defaultString("reasoning.hidden_label"),
		PersistSummaries: defaultBool("reasoning.persist_summaries"),
		displayDefaulted: true,
	}
}

// ResolveReasoning returns a fully-populated reasoning display policy for a
// surface ("chat", "ask", "serve", or "jobs"), applying optional per-surface
// overrides and the TERM_LLM_SHOW_RAW_REASONING debug override.
func (c *Config) ResolveReasoning(surface string) ReasoningConfig {
	resolved := DefaultReasoningConfig()
	var overrides ReasoningConfig
	if c != nil {
		overrides = c.Reasoning
	}
	resolved = mergeReasoningConfig(resolved, overrides)
	var policy ReasoningSurfaceConfig
	switch strings.ToLower(strings.TrimSpace(surface)) {
	case "chat":
		policy = overrides.Chat
	case "ask":
		policy = overrides.Ask
	case "serve":
		policy = overrides.Serve
		policy.Display = overrides.serveDisplay()
	case "jobs":
		policy = overrides.Jobs
	}
	resolved = applyReasoningSurface(resolved, policy)
	resolved.normalize()
	if parseEnvBool(os.Getenv("TERM_LLM_SHOW_RAW_REASONING")) {
		resolved.Display = ReasoningDisplayRaw
		resolved.Source = ReasoningSourceAll
		resolved.Raw = true
	}
	return resolved
}

// serveDisplay defaults to off unless a display policy was explicitly chosen.
// An explicit serve policy (including inherit) wins over the top-level policy.
func (r ReasoningConfig) serveDisplay() string {
	serve := strings.TrimSpace(r.Serve.Display)
	if serve != "" && (!r.serveDisplayDefaulted || serve != defaultString("reasoning.serve.display")) {
		return serve
	}
	top := strings.TrimSpace(r.Display)
	if top != "" && (!r.displayDefaulted || top != defaultString("reasoning.display")) {
		return ReasoningInherit
	}
	return defaultString("reasoning.serve.display")
}

func mergeReasoningConfig(base, override ReasoningConfig) ReasoningConfig {
	if isZeroReasoningConfig(override) {
		return base
	}
	if override.Display != "" {
		base.Display = override.Display
	}
	if override.Source != "" {
		base.Source = override.Source
	}
	if override.Status != "" {
		base.Status = override.Status
	}
	if override.History != "" {
		base.History = override.History
	}
	if override.Export != "" {
		base.Export = override.Export
	}
	if override.RawSet || override.Raw {
		base.Raw = override.Raw
	}
	if override.MaxSummaryChars != 0 {
		base.MaxSummaryChars = override.MaxSummaryChars
	}
	if override.MaxRawChars != 0 {
		base.MaxRawChars = override.MaxRawChars
	}
	if override.ExtractTitlesSet || override.ExtractTitles {
		base.ExtractTitles = override.ExtractTitles
	}
	if override.PersistSummariesSet || override.PersistSummaries {
		base.PersistSummaries = override.PersistSummaries
	}
	if override.HiddenLabel != "" {
		base.HiddenLabel = override.HiddenLabel
	}
	base.Chat = override.Chat
	base.Ask = override.Ask
	base.Serve = override.Serve
	base.Jobs = override.Jobs
	return base
}

func isZeroReasoningConfig(r ReasoningConfig) bool {
	return r.Display == "" &&
		r.Source == "" &&
		r.Status == "" &&
		r.History == "" &&
		r.Export == "" &&
		!r.Raw &&
		r.MaxSummaryChars == 0 &&
		r.MaxRawChars == 0 &&
		!r.ExtractTitles &&
		r.HiddenLabel == "" &&
		!r.PersistSummaries &&
		!r.RawSet &&
		!r.ExtractTitlesSet &&
		!r.PersistSummariesSet &&
		r.Chat == (ReasoningSurfaceConfig{}) &&
		r.Ask == (ReasoningSurfaceConfig{}) &&
		r.Serve == (ReasoningSurfaceConfig{}) &&
		r.Jobs == (ReasoningSurfaceConfig{})
}

func applyReasoningSurface(base ReasoningConfig, surface ReasoningSurfaceConfig) ReasoningConfig {
	if v := strings.TrimSpace(surface.Display); v != "" && !strings.EqualFold(v, ReasoningInherit) {
		base.Display = v
	}
	if v := strings.TrimSpace(surface.Status); v != "" && !strings.EqualFold(v, ReasoningInherit) {
		base.Status = v
	}
	if v := strings.TrimSpace(surface.History); v != "" && !strings.EqualFold(v, ReasoningInherit) {
		base.History = v
	}
	if v := strings.TrimSpace(surface.Export); v != "" && !strings.EqualFold(v, ReasoningInherit) {
		base.Export = v
	}
	if surface.Raw != nil {
		base.Raw = *surface.Raw
	}
	return base
}

func (r *ReasoningConfig) normalize() {
	if r == nil {
		return
	}
	defaults := DefaultReasoningConfig()
	r.Display = normalizeOneOf(r.Display, defaults.Display, ReasoningDisplayAuto, ReasoningDisplayOff, ReasoningDisplayStatus, ReasoningDisplayCollapsed, ReasoningDisplayExpanded, ReasoningDisplayRaw)
	r.Source = normalizeOneOf(r.Source, defaults.Source, ReasoningSourceSummaryOnly, ReasoningSourceSummaryOrProviderSafe, ReasoningSourceAll)
	r.Status = normalizeOneOf(r.Status, defaults.Status, ReasoningStatusNone, ReasoningStatusGeneric, ReasoningStatusTitle, ReasoningStatusSummary)
	r.History = normalizeOneOf(r.History, defaults.History, ReasoningHistoryNone, ReasoningHistoryCollapsed, ReasoningHistoryExpanded, ReasoningHistoryTranscriptOnly)
	r.Export = normalizeOneOf(r.Export, defaults.Export, ReasoningExportNever, ReasoningExportAsk, ReasoningExportSummaries, ReasoningExportRaw)
	if r.MaxSummaryChars <= 0 {
		r.MaxSummaryChars = defaults.MaxSummaryChars
	}
	if r.MaxRawChars <= 0 {
		r.MaxRawChars = defaults.MaxRawChars
	}
	if strings.TrimSpace(r.HiddenLabel) == "" {
		r.HiddenLabel = defaults.HiddenLabel
	}
}

func normalizeOneOf(value, fallback string, allowed ...string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" || v == ReasoningInherit {
		return fallback
	}
	for _, allow := range allowed {
		if v == allow {
			return v
		}
	}
	return fallback
}

func parseEnvBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func markReasoningConfigPresence(reasoning *ReasoningConfig, file *fileSettings) {
	if reasoning == nil || file == nil {
		return
	}
	reasoning.displayDefaulted = !file.has("reasoning.display")
	reasoning.serveDisplayDefaulted = !file.has("reasoning.serve.display")
	reasoning.RawSet = file.has("reasoning.raw")
	reasoning.ExtractTitlesSet = file.has("reasoning.extract_titles")
	reasoning.PersistSummariesSet = file.has("reasoning.persist_summaries")
}
