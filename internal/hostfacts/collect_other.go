//go:build !linux && !darwin

package hostfacts

import "context"

func collectPlatform(ctx context.Context, c collector, f *Facts) {
	f.UptimeSeconds = -1
	f.Warnings = append(f.Warnings, "detailed host probes are unavailable on this platform")
}
