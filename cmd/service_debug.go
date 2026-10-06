package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	// launchctl print includes environment values. Only expose lifecycle fields.
	for _, line := range strings.Split(status, "\n") {
		trim := strings.TrimSpace(line)
		key, _, ok := strings.Cut(trim, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ActiveState", "SubState", "UnitFileState", "MainPID", "state", "pid", "last exit code", "last terminating signal", "runs":
			serviceDebug(cmd, "%s", trim)
		}
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
