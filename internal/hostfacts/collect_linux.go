//go:build linux

package hostfacts

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func collectPlatform(ctx context.Context, c collector, f *Facts) {
	read := func(path string) []byte {
		b, err := c.readFile(filepath.Join(c.root, path))
		if err != nil {
			return nil
		}
		return b
	}
	if b := read("etc/os-release"); b != nil {
		values := parseKeyValues(string(b))
		f.Distro = strings.Trim(values["PRETTY_NAME"], "\"")
		if f.Distro == "" {
			f.Distro = strings.Trim(values["NAME"], "\"")
		}
		f.DistroVersion = strings.Trim(values["VERSION_ID"], "\"")
	}
	if b := read("proc/sys/kernel/osrelease"); b != nil {
		f.Kernel = strings.TrimSpace(string(b))
	}
	if b := read("proc/1/comm"); b != nil {
		f.Init = detectInit(strings.TrimSpace(string(b)))
	}
	if _, err := c.readFile(filepath.Join(c.root, "run/systemd/system")); err == nil {
		f.Init = "systemd"
	}
	if b := read("proc/uptime"); b != nil {
		fields := strings.Fields(string(b))
		if len(fields) > 0 {
			v, _ := strconv.ParseFloat(fields[0], 64)
			f.UptimeSeconds = int64(v)
		}
	}
	if b := read("proc/loadavg"); b != nil {
		fields := strings.Fields(string(b))
		if len(fields) >= 3 {
			f.Load1, _ = strconv.ParseFloat(fields[0], 64)
			f.Load5, _ = strconv.ParseFloat(fields[1], 64)
			f.Load15, _ = strconv.ParseFloat(fields[2], 64)
			f.LoadKnown = true
		}
	}
	if b := read("proc/meminfo"); b != nil {
		vals := parseMeminfo(string(b))
		f.MemTotalBytes = vals["MemTotal"] * 1024
		f.MemAvailableBytes = vals["MemAvailable"] * 1024
	}
	if _, err := c.readFile(filepath.Join(c.root, ".dockerenv")); err == nil {
		f.Container = "docker"
	} else if _, err := c.readFile(filepath.Join(c.root, "run/.containerenv")); err == nil {
		f.Container = "podman"
	} else if b := read("proc/1/cgroup"); strings.Contains(string(b), "lxc") {
		f.Container = "lxc"
	}
	if b := read("proc/version"); strings.Contains(strings.ToLower(string(b)), "microsoft") {
		f.Container = "wsl"
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(c.root, &st); err == nil {
		f.Disks = []DiskUsage{{Path: "/", Mount: "/", TotalBytes: st.Blocks * uint64(st.Bsize), FreeBytes: st.Bavail * uint64(st.Bsize)}}
	} else {
		f.Warnings = append(f.Warnings, fmt.Sprintf("statfs /: %v", err))
	}
	if ctx.Err() != nil {
		f.Warnings = append(f.Warnings, "platform probes: "+ctx.Err().Error())
	}
}

func parseKeyValues(s string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '='); i > 0 {
			out[line[:i]] = line[i+1:]
		}
	}
	return out
}
func parseMeminfo(s string) map[string]uint64 {
	out := map[string]uint64{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 {
			key := strings.TrimSuffix(fields[0], ":")
			out[key], _ = strconv.ParseUint(fields[1], 10, 64)
		}
	}
	return out
}
func detectInit(v string) string {
	v = strings.ToLower(filepath.Base(v))
	switch {
	case strings.Contains(v, "systemd"):
		return "systemd"
	case v == "runsvdir" || v == "runit":
		return "runit"
	case strings.Contains(v, "openrc"):
		return "openrc"
	case strings.Contains(v, "s6"):
		return "s6"
	case v == "init":
		return "sysvinit"
	default:
		return "unknown"
	}
}
