//go:build browserfixture

package llm

import "github.com/samsaffron/term-llm/internal/llm/debug"

// Deprecated: use debug.BrowserGate for new package boundaries.
type DebugBrowserGate = debug.BrowserGate

// Deprecated: use debug.NewBrowserGate.
func NewDebugBrowserGate() *DebugBrowserGate { return debug.NewBrowserGate() }

// Deprecated: use debug.LookupBrowserGate.
func LookupDebugBrowserGate(prompt string) (*DebugBrowserGate, bool) {
	return debug.LookupBrowserGate(prompt)
}
