package usage

import "time"

// Provider constants for identifying data sources
const (
	ProviderClaudeCode = "claude-code"
	ProviderCodex      = "codex"
	ProviderGeminiCLI  = "gemini-cli"
	ProviderTermLLM    = "term-llm"
)

// UsageEntry represents a single usage event from any provider
type UsageEntry struct {
	Timestamp           time.Time
	SessionID           string
	Model               string
	InputTokens         int
	OutputTokens        int
	CacheWriteTokens    int     // cache_creation for Claude
	CacheReadTokens     int     // cache_read for Claude, cached for Gemini
	ReasoningTokens     int     // Codex reasoning_output, Gemini thoughts
	CostUSD             float64 // Pre-calculated cost if available
	Provider            string  // ProviderClaudeCode, ProviderCodex, ProviderGeminiCLI, or ProviderTermLLM
	TrackedExternallyBy string  // For term-llm entries: "claude-code", "codex", "gemini-cli", or "" for direct API
}

// TotalTokens returns the sum of all token types
func (e UsageEntry) TotalTokens() int {
	return e.InputTokens + e.OutputTokens + e.CacheWriteTokens + e.CacheReadTokens + e.ReasoningTokens
}

// DailyUsage represents aggregated usage for a single day
type DailyUsage struct {
	Date             string // YYYY-MM-DD format
	InputTokens      int
	OutputTokens     int
	CacheWriteTokens int
	CacheReadTokens  int
	ReasoningTokens  int
	TotalCost        float64
	ModelsUsed       []string
	Entries          []UsageEntry // Raw entries for this day (used for breakdown)
}

// TotalTokens returns the sum of all token types for the day
func (d DailyUsage) TotalTokens() int {
	return d.InputTokens + d.OutputTokens + d.CacheWriteTokens + d.CacheReadTokens + d.ReasoningTokens
}

// ModelBreakdown represents usage breakdown by model
type ModelBreakdown struct {
	Model            string
	InputTokens      int
	OutputTokens     int
	CacheWriteTokens int
	CacheReadTokens  int
	ReasoningTokens  int
	Cost             float64
}

// LoadResult contains the result of loading usage data from a provider
type LoadResult struct {
	Entries            []UsageEntry
	MissingDirectories []string
	Errors             []error
}

// Merge combines another LoadResult into this one
func (r *LoadResult) Merge(other LoadResult) {
	r.Entries = append(r.Entries, other.Entries...)
	r.MissingDirectories = append(r.MissingDirectories, other.MissingDirectories...)
	r.Errors = append(r.Errors, other.Errors...)
}

// FilterOptions contains options for filtering usage data
type FilterOptions struct {
	Since           time.Time // Include entries on or after this time
	Until           time.Time // Include entries on or before this time
	Provider        string    // Filter to specific provider, or empty for all
	IncludeExternal bool      // Include term-llm entries with TrackedExternallyBy set (opt-in)
}

// Filter returns entries matching the filter options
func (r LoadResult) Filter(opts FilterOptions) []UsageEntry {
	var result []UsageEntry
	for _, e := range r.Entries {
		// Check provider filter
		if opts.Provider != "" && e.Provider != opts.Provider {
			continue
		}
		// Check date range
		if !opts.Since.IsZero() && e.Timestamp.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && e.Timestamp.After(opts.Until) {
			continue
		}
		// Handle term-llm externally-tracked entries
		if e.Provider == ProviderTermLLM && e.TrackedExternallyBy != "" {
			// When showing all providers (no filter), exclude externally-tracked term-llm entries
			// to avoid double-counting with the external provider's data
			if opts.Provider == "" {
				continue
			}
			// When filtering to term-llm specifically, only include if IncludeExternal is set
			if opts.Provider == ProviderTermLLM && !opts.IncludeExternal {
				continue
			}
		}
		result = append(result, e)
	}
	return result
}
