//go:build darwin

package hostfacts

import (
	"context"
	"strings"
)

func collectPlatform(ctx context.Context, c collector, f *Facts) {
	f.Init = "launchd"
	if out, err := c.run(ctx, "sw_vers", "-productVersion"); err == nil {
		f.Distro = "macOS"
		f.DistroVersion = strings.TrimSpace(string(out))
	} else {
		f.Warnings = append(f.Warnings, "sw_vers: "+err.Error())
	}
	if out, err := c.run(ctx, "sysctl", "-n", "kern.osrelease"); err == nil {
		f.Kernel = strings.TrimSpace(string(out))
	}
}
