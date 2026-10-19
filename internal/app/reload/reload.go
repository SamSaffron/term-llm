// Package reload owns private successor state and process replacement.
package reload

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/process"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
	"github.com/samsaffron/term-llm/internal/tools"
)

// Capture the installed pathname before an updater replaces/unlinks the image.
// Retain the invocation symlink when it really names this executable, so updating
// that symlink loads the new version rather than its obsolete resolved target.
var reloadExecutable = func() string {
	current, err := os.Executable()
	if err != nil {
		return ""
	}
	invoked, err := exec.LookPath(os.Args[0])
	if err != nil {
		return current
	}
	invoked, err = filepath.Abs(invoked)
	if err != nil {
		return current
	}
	a, ea := os.Stat(current)
	b, eb := os.Stat(invoked)
	if ea == nil && eb == nil && os.SameFile(a, b) {
		return invoked
	}
	return current
}()

// Replacement-only hints are scrubbed at package initialization, before any
// command can launch tools. They are never credentials for a tool child.
var mcpReloadToken = takeReloadEnv("TERM_LLM_MCP_RELOAD_TOKEN")
var serveReloadToken = takeReloadEnv("TERM_LLM_SERVE_RELOAD_TOKEN")

func takeReloadEnv(key string) string {
	value := os.Getenv(key)
	_ = os.Unsetenv(key)
	return value
}

func Environ(extra ...string) []string {
	env := append(os.Environ(), tools.HubDelegationEnviron()...)
	for _, provider := range environProviders {
		env = append(env, provider()...)
	}
	env = append(env, process.HandoffEnviron()...)
	return append(env, extra...)
}

func Bind(ctx context.Context, extraEnv ...string) (func(), error) {
	// Server logs only; never write diagnostics into a live TUI or the restart
	// CLI's progress table. Fast idle restarts produce no extra output.
	restart.Default.Diagnostic = func(phase string, blockers []restart.Blocker) {
		entries := make([]string, 0, len(blockers))
		for _, blocker := range blockers {
			entries = append(entries, fmt.Sprintf("%d at %s (oldest %s)", blocker.Count, blocker.Source, blocker.Oldest.Round(time.Second)))
		}
		action := "waiting for"
		if phase == "cancelling" {
			action = "interrupting; still waiting for"
		}
		runtimeoutput.Logf("[reload] %s %s", action, strings.Join(entries, "; "))
	}
	return restart.Default.Bind(ctx, func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return ReplaceProcess(reloadExecutable, append([]string(nil), os.Args...), Environ(extraEnv...))
	})
}

// Executable returns the installed pathname captured at process startup.
func Executable() string { return reloadExecutable }

// MCPToken returns the private MCP bearer token captured from a predecessor.
func MCPToken() string { return mcpReloadToken }

// ServeToken returns the private serve bearer token captured from a predecessor.
func ServeToken() string { return serveReloadToken }

// Providers are installed by host initialization, before commands can reload.
var environProviders []func() []string

// RegisterEnviron installs a host handoff provider in registration order.
func RegisterEnviron(provider func() []string) { environProviders = append(environProviders, provider) }
