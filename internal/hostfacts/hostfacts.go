package hostfacts

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/buildinfo"
)

const (
	probeBudget = 2 * time.Second
	cacheTTL    = 60 * time.Second
)

type DiskUsage struct {
	Path, Mount           string
	TotalBytes, FreeBytes uint64
}

type Facts struct {
	Hostname, OS, Distro, DistroVersion, Kernel, Arch string
	Init                                              string
	PackageManagers                                   []string
	User                                              string
	UID                                               int
	IsRoot                                            bool
	SudoNoPassword                                    *bool
	Container, Virtualization                         string
	UptimeSeconds                                     int64
	Load1, Load5, Load15                              float64
	LoadKnown                                         bool
	MemTotalBytes, MemAvailableBytes                  uint64
	Disks                                             []DiskUsage
	Shell, TermLLMVersion                             string
	CollectedAt                                       time.Time
	Warnings                                          []string
}

type collector struct {
	root        string
	now         func() time.Time
	readFile    func(string) ([]byte, error)
	lookPath    func(string) (string, error)
	run         func(context.Context, string, ...string) ([]byte, error)
	hostname    func() (string, error)
	currentUser func() (*user.User, error)
}

func productionCollector() collector {
	return collector{root: "/", now: time.Now, readFile: os.ReadFile, lookPath: exec.LookPath,
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdin = nil
			cmd.Env = withoutEnv(os.Environ(), "SUDO_ASKPASS")
			return cmd.Output()
		}, hostname: os.Hostname, currentUser: user.Current}
}

func withoutEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, value := range env {
		if !strings.HasPrefix(value, prefix) {
			out = append(out, value)
		}
	}
	return out
}

func Collect(ctx context.Context) Facts { return collect(ctx, productionCollector()) }

func collect(ctx context.Context, c collector) (facts Facts) {
	facts.OS, facts.Arch, facts.TermLLMVersion, facts.CollectedAt = runtime.GOOS, runtime.GOARCH, buildinfo.Version, c.now()
	facts.Init = "unknown"
	facts.UID = -1
	if h, err := c.hostname(); err == nil {
		facts.Hostname = h
	} else {
		facts.Warnings = append(facts.Warnings, "hostname: "+err.Error())
	}
	if u, err := c.currentUser(); err == nil {
		facts.User = u.Username
		if uid, e := strconv.Atoi(u.Uid); e == nil {
			facts.UID = uid
			facts.IsRoot = uid == 0
		}
	}
	facts.Shell = os.Getenv("SHELL")
	budgetCtx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	collectPlatform(budgetCtx, c, &facts)
	for _, name := range []string{"pacman", "apt", "dnf", "zypper", "apk", "brew", "nix", "port", "flatpak", "snap"} {
		if _, err := c.lookPath(name); err == nil {
			facts.PackageManagers = append(facts.PackageManagers, name)
		}
	}
	if !facts.IsRoot {
		if _, err := c.lookPath("sudo"); err == nil {
			probeCtx, stop := context.WithTimeout(budgetCtx, time.Second)
			_, err = c.run(probeCtx, "sudo", "-n", "true")
			stop()
			v := err == nil
			facts.SudoNoPassword = &v
			if err != nil && probeCtx.Err() != nil {
				facts.Warnings = append(facts.Warnings, "sudo probe: "+probeCtx.Err().Error())
			}
		}
	}
	if _, err := c.lookPath("systemd-detect-virt"); err == nil {
		probeCtx, stop := context.WithTimeout(budgetCtx, time.Second)
		if out, err := c.run(probeCtx, "systemd-detect-virt"); err == nil {
			facts.Virtualization = strings.TrimSpace(string(out))
		}
		stop()
	}
	if err := budgetCtx.Err(); err != nil {
		facts.Warnings = append(facts.Warnings, "probe budget: "+err.Error())
	}
	return facts
}

func (f Facts) Render() string {
	unknown := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "n/a"
		}
		return s
	}
	root, sudo := "no", "n/a"
	if f.IsRoot {
		root = "yes"
	}
	if f.SudoNoPassword != nil {
		if *f.SudoNoPassword {
			sudo = "yes"
		} else {
			sudo = "no"
		}
	}
	load := "n/a"
	if f.LoadKnown {
		load = fmt.Sprintf("%.2f %.2f %.2f", f.Load1, f.Load5, f.Load15)
	}
	mem := "n/a"
	if f.MemTotalBytes > 0 {
		mem = fmt.Sprintf("%s avail / %s", formatBytes(f.MemAvailableBytes), formatBytes(f.MemTotalBytes))
	}
	lines := []string{
		fmt.Sprintf("host: %s  (%s %s, %s)  distro: %s %s", unknown(f.Hostname), unknown(f.OS), unknown(f.Kernel), unknown(f.Arch), unknown(f.Distro), f.DistroVersion),
		fmt.Sprintf("init: %s  pkg: %s  container: %s  virtualization: %s", unknown(f.Init), unknown(strings.Join(f.PackageManagers, ",")), unknown(f.Container), unknown(f.Virtualization)),
		fmt.Sprintf("user: %s (uid %d)  root: %s  sudo -n: %s", unknown(f.User), f.UID, root, sudo),
		fmt.Sprintf("up: %s  load: %s  mem: %s", formatDuration(f.UptimeSeconds), load, mem),
	}
	for _, d := range f.Disks {
		used := 0.0
		if d.TotalBytes > 0 {
			used = 100 * float64(d.TotalBytes-d.FreeBytes) / float64(d.TotalBytes)
		}
		lines = append(lines, fmt.Sprintf("disk: %s %.0f%% used (%s free)", d.Path, used, formatBytes(d.FreeBytes)))
	}
	lines = append(lines, fmt.Sprintf("term-llm: %s  collected: %s", unknown(f.TermLLMVersion), f.CollectedAt.Format(time.RFC3339)))
	if len(f.Warnings) > 0 {
		w := append([]string(nil), f.Warnings...)
		sort.Strings(w)
		lines = append(lines, "warnings: "+strings.Join(w, "; "))
	}
	return "```text\n" + strings.Join(lines, "\n") + "\n```"
}

func formatBytes(n uint64) string {
	const g = uint64(1 << 30)
	if n >= g {
		return fmt.Sprintf("%.1fG", float64(n)/float64(g))
	}
	return fmt.Sprintf("%dM", n/(1<<20))
}
func formatDuration(seconds int64) string {
	if seconds < 0 {
		return "n/a"
	}
	d := seconds / 86400
	h := (seconds % 86400) / 3600
	return fmt.Sprintf("%dd %dh", d, h)
}

var factCache struct {
	sync.Mutex
	at    time.Time
	value string
}
var cacheNow = time.Now
var cacheCollect = Collect

func RenderCached(ctx context.Context) string {
	factCache.Lock()
	defer factCache.Unlock()
	now := cacheNow()
	if factCache.value != "" && now.Sub(factCache.at) < cacheTTL {
		return factCache.value
	}
	factCache.value = cacheCollect(ctx).Render()
	factCache.at = now
	return factCache.value
}
