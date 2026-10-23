package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/userservice"
	"github.com/spf13/cobra"
)

func serviceDebug(cmd *cobra.Command, format string, args ...any) {
	enabled, _ := cmd.Flags().GetBool("debug")
	if enabled {
		fmt.Fprintf(cmd.ErrOrStderr(), "[service debug] "+format+"\n", args...)
	}
}

// Diagnostics are best effort and must never replace the original startup error.
func reportServiceDebugFailure(cmd *cobra.Command, e serviceEnvironment, kind string) {
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()
	serviceDebug(cmd, "Startup failed; collecting native status and recent logs (logs may contain sensitive data; review before sharing)")
	status, err := e.native.Status(ctx, kind)
	for _, field := range serviceLifecycleFields(status) {
		serviceDebug(cmd, "%s", field)
	}
	if err != nil {
		serviceDebug(cmd, "Native status unavailable (see command outcome above)")
	}
	// Do not expose full native errors: launchctl output can contain environment secrets.
	logs, err := e.native.RecentLogs(ctx, kind, e.specPath(kind))
	if logs != "" {
		serviceDebug(cmd, "Recent service logs (last 60 lines):")
		fmt.Fprintln(cmd.ErrOrStderr(), strings.TrimSpace(logs))
	}
	if err != nil {
		serviceDebug(cmd, "Could not read recent logs (see command outcome above)")
	}
	serviceDebug(cmd, "More details: term-llm service status %s; term-llm service logs %s --follow", kind, kind)
}

var serviceReadyTimeout = 15 * time.Second

// serviceLifecycleFields selects native lifecycle lines. launchctl print also
// includes environment values, so only allowlisted fields are ever exposed.
func serviceLifecycleFields(status string) []string {
	var fields []string
	for _, line := range strings.Split(status, "\n") {
		trim := strings.TrimSpace(line)
		key, _, ok := strings.Cut(trim, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ActiveState", "SubState", "UnitFileState", "MainPID", "state", "pid", "last exit code", "last terminating signal", "runs":
			fields = append(fields, trim)
		}
	}
	return fields
}

// serviceNotRunningSummary distinguishes a server that exits during startup
// (and is being restarted by the supervisor) from one that is merely slow, so
// a readiness timeout points operators at the service log instead of looking
// like a hang.
func serviceNotRunningSummary(ctx context.Context, kind string, native userservice.Native) string {
	if ctx.Err() != nil || native.Running(ctx, kind) {
		return ""
	}
	status, _ := native.Status(ctx, kind)
	seen := map[string]bool{}
	var facts []string
	for _, field := range serviceLifecycleFields(status) {
		key, _, _ := strings.Cut(field, "=")
		key = strings.TrimSpace(key)
		if key == "UnitFileState" || seen[key] {
			continue
		}
		seen[key] = true
		facts = append(facts, field)
	}
	summary := "; the server process is not running"
	if len(facts) > 0 {
		summary += " (" + strings.Join(facts, ", ") + ")"
	}
	return summary + " and is probably exiting during startup: the cause is in the service log (rerun with --debug to include it)"
}
