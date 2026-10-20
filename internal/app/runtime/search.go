package runtime

// ResumedSessionSearch decides web search for a resumed session. The saved
// session setting applies unless this invocation is explicit: --no-search
// always disables search (including provider-native search), and --search
// keeps the CLI-resolved value. Explicit flags apply to this invocation only;
// the saved session setting is not rewritten.
func ResumedSessionSearch(resolved, saved, searchFlagSet, noSearch bool) bool {
	switch {
	case noSearch:
		return false
	case searchFlagSet:
		return resolved
	default:
		return saved
	}
}
