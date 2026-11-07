package mcpstart

import (
	"context"
	"fmt"
	"github.com/samsaffron/term-llm/internal/mcp"
	"strings"
	"time"
)

// WaitForServersReady waits for session-selected servers to settle. Authentication
// required is a settled state: signing in later must not discard the selection.
func WaitForServersReady(ctx context.Context, manager *mcp.Manager, names []string, timeout time.Duration) error {
	if manager == nil || len(names) == 0 {
		return nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	check := func() (bool, error) {
		var failed []string
		var starting []string
		var stopped []string
		for _, name := range names {
			status, err := manager.ServerStatus(name)
			switch status {
			case mcp.StatusReady, mcp.StatusAuthRequired:
				// Settled: sign-in can finish later without discarding selection.
			case mcp.StatusStarting:
				starting = append(starting, name)
			case mcp.StatusFailed:
				msg := "unknown error"
				if err != nil {
					msg = err.Error()
				}
				failed = append(failed, fmt.Sprintf("%s (%s)", name, msg))
			default:
				stopped = append(stopped, name)
			}
		}
		if len(failed) > 0 {
			return false, fmt.Errorf("MCP servers failed to start: %s", strings.Join(failed, "; "))
		}
		if len(stopped) > 0 {
			return false, fmt.Errorf("MCP servers stopped before becoming ready: %s", strings.Join(stopped, ", "))
		}
		return len(starting) == 0, nil
	}

	for {
		ready, err := check()
		if ready || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			var stillStarting []string
			for _, name := range names {
				status, _ := manager.ServerStatus(name)
				if status == mcp.StatusStarting {
					stillStarting = append(stillStarting, name)
				}
			}
			if len(stillStarting) == 0 {
				_, err := check()
				return err
			}
			return fmt.Errorf("timed out waiting for MCP servers to start: %s", strings.Join(stillStarting, ", "))
		case <-ticker.C:
		}
	}
}
