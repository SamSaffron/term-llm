package ask

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/mcp"
	mcpoauth "github.com/samsaffron/term-llm/internal/mcp/oauth"
	"github.com/samsaffron/term-llm/internal/terminalpolicy"
	"github.com/samsaffron/term-llm/internal/tooldiscovery"
)

// MCPOptions contains options for enabling MCP servers.
type MCPOptions struct {
	Provider      core.Provider
	Model         string
	YoloMode      bool
	ToolDiscovery config.ToolDiscoveryConfig
}

// EnableMCPServersWithFeedback initializes MCP servers with user feedback.
// Returns the manager (caller must call StopAll) or error if setup failed.
func EnableMCPServersWithFeedback(ctx context.Context, mcpFlag string, engine *engine.Engine, errWriter io.Writer, opts *MCPOptions) (*mcp.Manager, error) {
	serverNames := ParseServerList(mcpFlag)
	if len(serverNames) == 0 {
		return nil, nil
	}

	mcpManager := mcp.NewManager()
	if err := mcpManager.LoadConfig(); err != nil {
		return nil, fmt.Errorf("failed to load MCP config: %w", err)
	}

	// Set up sampling handler if provider is available
	if opts != nil && opts.Provider != nil {
		mcpManager.SetSamplingProvider(opts.Provider, opts.Model, opts.YoloMode)
	}

	// Validate all servers exist before starting any
	available := mcpManager.AvailableServers()
	availableSet := make(map[string]bool)
	for _, s := range available {
		availableSet[s] = true
	}

	var missing []string
	for _, name := range serverNames {
		if !availableSet[name] {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		if len(missing) == 1 {
			return nil, fmt.Errorf("MCP server '%s' not configured. Add it with: term-llm mcp add %s", missing[0], missing[0])
		}
		return nil, fmt.Errorf("MCP servers not configured: %s. Add them with: term-llm mcp add <name>", strings.Join(missing, ", "))
	}

	// Only repaint startup progress when feedback is an interactive terminal.
	feedbackFile, _ := errWriter.(*os.File)
	animate := terminalpolicy.OutputInteractive(feedbackFile)
	if animate {
		fmt.Fprintf(errWriter, "Starting MCP: %s", strings.Join(serverNames, ", "))
	} else {
		fmt.Fprintf(errWriter, "Starting MCP: %s\n", strings.Join(serverNames, ", "))
	}

	ready := false
	defer func() {
		if !ready {
			mcpManager.StopAll()
		}
	}()

	if err := startMCPServersWithFeedback(ctx, mcpManager, serverNames, errWriter, animate); err != nil {
		return nil, err
	}

	if err := CheckMCPServerFailures(mcpManager, serverNames, errWriter, animate); err != nil {
		return nil, err
	}

	// Register every authorised wrapper for execution while the planner owns
	// provider visibility and durable session activation.
	discoveryCfg := config.ToolDiscoveryConfig{}
	if opts != nil {
		discoveryCfg = opts.ToolDiscovery
	}
	if _, err := tooldiscovery.NewPlanner(discoveryCfg, mcpManager, engine); err != nil {
		return nil, fmt.Errorf("configure MCP tool discovery: %w", err)
	}
	tools := mcpManager.AllTools()

	// Show result
	var readyServers []string
	for _, name := range serverNames {
		if status, _ := mcpManager.ServerStatus(name); status == mcp.StatusReady {
			readyServers = append(readyServers, name)
		}
	}
	if len(tools) > 0 {
		if animate {
			fmt.Fprint(errWriter, "\r")
		}
		fmt.Fprintf(errWriter, "✓ MCP ready: %d tools from %s\n\n", len(tools), strings.Join(readyServers, ", "))
	} else {
		if animate {
			fmt.Fprintln(errWriter)
		}
		return nil, fmt.Errorf("MCP servers started but no tools available from: %s", strings.Join(serverNames, ", "))
	}

	ready = true
	return mcpManager, nil
}

func startMCPServersWithFeedback(ctx context.Context, manager *mcp.Manager, serverNames []string, feedback io.Writer, animate bool) error {
	// Enable all servers (async).
	var enableErrors []string
	for _, server := range serverNames {
		if err := manager.Enable(ctx, server); err != nil {
			enableErrors = append(enableErrors, fmt.Sprintf("%s: %v", server, err))
		}
	}
	if len(enableErrors) > 0 {
		if animate {
			fmt.Fprintln(feedback)
		}
		return fmt.Errorf("failed to start MCP servers: %s", strings.Join(enableErrors, "; "))
	}

	// Give the manager's 30-second per-server deadline time to settle, but
	// don't wait forever if a transport ignores cancellation.
	if err := WaitForMCPStartup(ctx, manager, serverNames, feedback, animate, 35*time.Second); err != nil {
		if animate {
			fmt.Fprintln(feedback)
		}
		return err
	}
	return nil
}

func CheckMCPServerFailures(manager *mcp.Manager, serverNames []string, feedback io.Writer, animate bool) error {
	var failedServers []string
	var authRequiredServers []string
	for _, name := range serverNames {
		status, err := manager.ServerStatus(name)
		if status == mcp.StatusAuthRequired {
			authRequiredServers = append(authRequiredServers, name)
			continue
		}
		if status != mcp.StatusFailed {
			continue
		}
		errMsg := "unknown error"
		if err != nil {
			errMsg = err.Error()
			if errors.Is(err, context.DeadlineExceeded) {
				errMsg = "startup timed out: " + errMsg
			}
		}
		failedServers = append(failedServers, fmt.Sprintf("%s (%s)", name, errMsg))
	}
	var problems []string
	if len(failedServers) > 0 {
		problems = append(problems, "MCP servers failed to start: "+strings.Join(failedServers, "; "))
	}
	if len(authRequiredServers) > 0 {
		var hints []string
		authStatuses := manager.AuthStatuses()
		for _, name := range authRequiredServers {
			command := "term-llm mcp login "
			if authStatuses[name].State == mcpoauth.AuthSignedIn {
				command += "--force "
			}
			hints = append(hints, "`"+command+name+"`")
		}
		problems = append(problems, fmt.Sprintf("MCP servers need sign-in: %s (run %s)", strings.Join(authRequiredServers, ", "), strings.Join(hints, ", ")))
	}
	if len(problems) == 0 {
		return nil
	}
	if animate {
		fmt.Fprintln(feedback)
	}
	return errors.New(strings.Join(problems, "; "))
}

// WaitForMCPStartup waits for each requested server to leave the starting state.
// The extra bound protects callers if a transport never returns after its own deadline.
func WaitForMCPStartup(ctx context.Context, manager interface {
	ServerStatus(string) (mcp.ServerStatus, error)
}, serverNames []string, feedback io.Writer, animate bool, maxWait time.Duration) error {
	spinner := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	spinIdx := 0
	for {
		var starting []string
		for _, name := range serverNames {
			if status, _ := manager.ServerStatus(name); status == mcp.StatusStarting {
				starting = append(starting, name)
			}
		}
		if len(starting) == 0 {
			return nil
		}
		if animate {
			fmt.Fprintf(feedback, "\r%s Starting MCP: %s", spinner[spinIdx], strings.Join(serverNames, ", "))
			spinIdx = (spinIdx + 1) % len(spinner)
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("MCP servers still starting: %s (startup timed out: %w)", strings.Join(starting, ", "), ctx.Err())
			}
			return fmt.Errorf("MCP servers still starting: %s: %w", strings.Join(starting, ", "), ctx.Err())
		case <-timer.C:
			return fmt.Errorf("MCP servers still starting after startup timeout: %s", strings.Join(starting, ", "))
		case <-ticker.C:
		}
	}
}

// ParseServerList splits comma-separated server names and trims whitespace.
func ParseServerList(mcpFlag string) []string {
	var servers []string
	for s := range strings.SplitSeq(mcpFlag, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			servers = append(servers, s)
		}
	}
	return servers
}
