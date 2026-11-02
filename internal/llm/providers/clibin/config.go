package clibin

// HooksEnabled reports whether Claude Code's user-defined hooks are allowed.
func (p *ClaudeBinProvider) HooksEnabled() bool { return p.enableHooks }

// ThinkingSummariesEnabled reports whether Claude's thinking summaries are requested.
func (p *ClaudeBinProvider) ThinkingSummariesEnabled() bool { return p.thinkingSummaries }

// ConfiguredModel reports the model and effort passed to the Grok CLI.
func (p *GrokBinProvider) ConfiguredModel() (model, effort string) {
	return p.model, p.effort
}

// ConfiguredModel reports the model, effort and fast flag sent to Cursor.
func (p *CursorBinProvider) ConfiguredModel() (model, effort string, fast bool) {
	return p.model, p.effort, p.fast
}
