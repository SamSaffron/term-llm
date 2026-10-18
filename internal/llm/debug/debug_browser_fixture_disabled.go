//go:build !browserfixture

package debug

import "context"

// Production debug providers have no browser-test gate or trigger surface.
func waitDebugBrowserFixture(context.Context, string) error { return nil }
