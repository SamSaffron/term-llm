//go:build !linux && !darwin

package hostfacts

import "context"

// collectPlatform is intentionally quiet: portable identity from the standard
// library is still rendered, and unknown fields show as n/a.
func collectPlatform(context.Context, collector, *Facts) {}
