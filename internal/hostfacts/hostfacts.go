package hostfacts

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// probeBudget bounds the few subprocess probes. Rendered facts are
// deliberately limited to host identity that does not change while a process
// runs, so a system prompt that embeds them renders identically when a session
// is resumed and does not needlessly refresh stored prompts. Volatile state (load, memory, disk, uptime, sudo
// credential caching) is left to live commands.
const probeBudget = 2 * time.Second

type Facts struct {
	Hostname, OS, Distro, DistroVersion, Kernel, Arch string
	Init                                              string
	PackageManagers                                   []string
	User                                              string
	UID                                               int
	IsRoot                                            bool
	Container, Virtualization                         string
	Warnings                                          []string
}

type collector struct {
	root        string
	readFile    func(string) ([]byte, error)
	lookPath    func(string) (string, error)
	run         func(context.Context, string, ...string) ([]byte, error)
	hostname    func() (string, error)
	currentUser func() (*user.User, error)
}

func productionCollector() collector {
	return collector{root: "/", readFile: os.ReadFile, lookPath: exec.LookPath,
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdin = nil
			return cmd.Output()
		}, hostname: os.Hostname, currentUser: user.Current}
}

func Collect(ctx context.Context) Facts { return collect(ctx, productionCollector()) }

func collect(ctx context.Context, c collector) (facts Facts) {
	facts.OS, facts.Arch = runtime.GOOS, runtime.GOARCH
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
	budgetCtx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	collectPlatform(budgetCtx, c, &facts)
	for _, name := range []string{"pacman", "apt", "dnf", "zypper", "apk", "brew", "nix", "port", "flatpak", "snap"} {
		if _, err := c.lookPath(name); err == nil {
			facts.PackageManagers = append(facts.PackageManagers, name)
		}
	}
	if _, err := c.lookPath("systemd-detect-virt"); err == nil {
		if out, err := c.run(budgetCtx, "systemd-detect-virt"); err == nil {
			facts.Virtualization = strings.TrimSpace(string(out))
		}
	}
	if err := budgetCtx.Err(); err != nil {
		facts.Warnings = append(facts.Warnings, "probe budget: "+err.Error())
	}
	return facts
}

// Render returns a deterministic description of the host. It contains no
// timestamps or resource measurements.
func (f Facts) Render() string {
	unknown := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "n/a"
		}
		return s
	}
	root := "no"
	if f.IsRoot {
		root = "yes"
	}
	lines := []string{
		fmt.Sprintf("host: %s  (%s %s, %s)  distro: %s %s", unknown(f.Hostname), unknown(f.OS), unknown(f.Kernel), unknown(f.Arch), unknown(f.Distro), f.DistroVersion),
		fmt.Sprintf("init: %s  pkg: %s  container: %s  virtualization: %s", unknown(f.Init), unknown(strings.Join(f.PackageManagers, ",")), unknown(f.Container), unknown(f.Virtualization)),
		fmt.Sprintf("user: %s (uid %d)  root: %s", unknown(f.User), f.UID, root),
	}
	return "```text\n" + strings.Join(lines, "\n") + "\n```"
}

var factCache struct {
	sync.Mutex
	value string
}
var cacheCollect = Collect

// RenderCached collects once per process. A degraded collection (probe
// timeouts or errors) is not cached, so a later render can complete it.
func RenderCached(ctx context.Context) string {
	factCache.Lock()
	defer factCache.Unlock()
	if factCache.value != "" {
		return factCache.value
	}
	facts := cacheCollect(ctx)
	rendered := facts.Render()
	if len(facts.Warnings) == 0 {
		factCache.value = rendered
	}
	return rendered
}
