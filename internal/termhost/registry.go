// Package termhost publishes the visible chat lifecycle to terminal hosts.
// Host adapters are deliberately thin; Manager owns ordering, fanout,
// coalescing, timeouts, and shutdown.
package termhost

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/lifecycle"
)

// Event aliases the dependency-free public lifecycle event contract.
type Event = lifecycle.Event

const (
	defaultCommandTimeout = 2 * time.Second
	defaultReleaseTimeout = 750 * time.Millisecond
	defaultCloseTimeout   = time.Second
	commandWaitDelay      = 250 * time.Millisecond
)

type commandRunner func(context.Context, string, []string, []byte) error

type runtimeContext struct {
	getenv     func(string) string
	lookPath   func(string) (string, error)
	run        commandRunner
	stderr     io.Writer
	now        func() time.Time
	goos       string
	pid        int
	cwd        string
	executable string
}

func defaultRuntimeContext() runtimeContext {
	cwd, _ := os.Getwd()
	executable, _ := os.Executable()
	return runtimeContext{
		getenv:     os.Getenv,
		lookPath:   exec.LookPath,
		run:        runCommand,
		stderr:     os.Stderr,
		now:        time.Now,
		goos:       runtime.GOOS,
		pid:        os.Getpid(),
		cwd:        cwd,
		executable: executable,
	}
}

// Adapter is the small integration boundary implemented by first-party hosts
// and generic command sinks.
type Adapter interface {
	Name() string
	Send(context.Context, Event) error
}

// Status describes why an adapter is or is not active. It is safe to expose to
// users: command arguments and environment values are never included.
type Status struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Detected bool   `json:"detected"`
	Enabled  bool   `json:"enabled"`
	Reason   string `json:"reason"`
}

// StatusReport is returned by lifecycle status and uses deterministic ordering.
type StatusReport struct {
	Enabled  bool     `json:"enabled"`
	Reason   string   `json:"reason"`
	Adapters []Status `json:"adapters"`
	OSC      Status   `json:"osc"`
}

type discoveredAdapter struct {
	status  Status
	adapter Adapter
}

type adapterFactory struct {
	name        string
	adapterType string
	discover    func(runtimeContext) discoveredAdapter
}

var adapterFactories = []adapterFactory{
	{name: "herdr", adapterType: "native", discover: discoverHerdr},
	{name: "cmux", adapterType: "sidebar", discover: discoverCMUX},
}

func discoverAll(cfg config.LifecycleConfig, legacyProgress bool, rt runtimeContext) ([]discoveredAdapter, StatusReport, error) {
	globalEnabled := cfg.Enabled && strings.TrimSpace(rt.getenv("TERM_LLM_LIFECYCLE")) != "0"
	report := StatusReport{Enabled: globalEnabled}
	switch {
	case !cfg.Enabled:
		report.Reason = "disabled by lifecycle.enabled"
	case strings.TrimSpace(rt.getenv("TERM_LLM_LIFECYCLE")) == "0":
		report.Reason = "disabled by TERM_LLM_LIFECYCLE=0"
	default:
		report.Reason = "enabled"
	}

	selected := make(map[string]bool)
	for _, raw := range cfg.Adapters {
		selected[strings.ToLower(strings.TrimSpace(raw))] = true
	}
	auto := selected["auto"]
	var found []discoveredAdapter
	for _, factory := range adapterFactories {
		allowed := auto || selected[factory.name]
		if !allowed {
			report.Adapters = append(report.Adapters, Status{
				Name: factory.name, Type: factory.adapterType,
				Reason: "not selected by lifecycle.adapters; detection skipped",
			})
			continue
		}
		if !globalEnabled {
			report.Adapters = append(report.Adapters, Status{
				Name: factory.name, Type: factory.adapterType,
				Reason: report.Reason + "; detection skipped",
			})
			continue
		}

		entry := factory.discover(rt)
		if entry.status.Enabled && entry.adapter != nil {
			found = append(found, entry)
		}
		report.Adapters = append(report.Adapters, entry.status)
	}

	for _, sinkCfg := range cfg.Commands {
		sink, err := newCommandSink(sinkCfg, rt.run)
		if err != nil {
			return nil, StatusReport{}, err
		}
		status := Status{
			Name:     sink.Name(),
			Type:     "command",
			Detected: true,
			Enabled:  globalEnabled,
			Reason:   "explicitly configured",
		}
		if !globalEnabled {
			status.Reason = report.Reason
		} else {
			found = append(found, discoveredAdapter{status: status, adapter: sink})
		}
		report.Adapters = append(report.Adapters, status)
	}

	policy := strings.ToLower(strings.TrimSpace(cfg.OSC))
	legacy := false
	if policy == "off" && legacyProgress {
		policy = "auto"
		legacy = true
	}
	if globalEnabled {
		report.OSC = inspectOSC(policy, rt)
		if legacy && report.OSC.Reason != "disabled" {
			report.OSC.Reason += "; enabled by deprecated chat.terminal_progress"
		}
	} else {
		report.OSC = Status{
			Name: "osc", Type: "terminal",
			Reason: report.Reason + "; detection skipped",
		}
	}

	sort.Slice(report.Adapters, func(i, j int) bool {
		if report.Adapters[i].Type != report.Adapters[j].Type {
			return report.Adapters[i].Type < report.Adapters[j].Type
		}
		return report.Adapters[i].Name < report.Adapters[j].Name
	})
	sort.Slice(found, func(i, j int) bool { return found[i].status.Name < found[j].status.Name })
	return found, report, nil
}

// Inspect reports discovery and policy without publishing state or executing a
// custom command.
func Inspect(cfg config.LifecycleConfig, legacyProgress bool) (StatusReport, error) {
	_, report, err := discoverAll(cfg, legacyProgress, defaultRuntimeContext())
	return report, err
}
