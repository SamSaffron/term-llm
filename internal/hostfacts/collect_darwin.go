//go:build darwin

package hostfacts

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"
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
	if out, err := c.run(ctx, "sysctl", "-n", "hw.memsize"); err == nil {
		fmt.Sscan(strings.TrimSpace(string(out)), &f.MemTotalBytes)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err == nil {
		block := uint64(st.Bsize)
		f.Disks = []DiskUsage{{Path: "/", Mount: "/", TotalBytes: uint64(st.Blocks) * block, FreeBytes: uint64(st.Bavail) * block}}
	}
	f.UptimeSeconds = -1
	_ = time.Second
}
