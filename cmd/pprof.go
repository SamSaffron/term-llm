package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"

	pprofserver "github.com/samsaffron/term-llm/internal/pprof"
	"github.com/spf13/cobra"
)

var pprofDuration int

func init() {
	rootCmd.AddCommand(pprofCmd)
	pprofCmd.AddCommand(pprofCPUCmd)
	pprofCmd.AddCommand(pprofHeapCmd)
	pprofCmd.AddCommand(pprofGoroutineCmd)
	pprofCmd.AddCommand(pprofBlockCmd)
	pprofCmd.AddCommand(pprofMutexCmd)
	pprofCmd.AddCommand(pprofWebCmd)

	pprofCPUCmd.Flags().IntVarP(&pprofDuration, "duration", "d", 30, "Profile duration in seconds")
}

var pprofCmd = &cobra.Command{
	Use:   "pprof",
	Short: "Connect to running pprof debug server",
	Long: `Connect to a running term-llm pprof debug server.

First, start term-llm with profiling enabled:
  term-llm chat --pprof           # random port (recommended)
  term-llm chat --pprof=6060      # specific port
  TERM_LLM_PPROF=1 term-llm chat  # via environment variable

Then, from another terminal, use these commands to profile:
  term-llm pprof cpu              # 30 second CPU profile
  term-llm pprof cpu -d 10        # 10 second CPU profile
  term-llm pprof heap             # Memory allocation profile
  term-llm pprof goroutine        # Goroutine stack dump
  term-llm pprof web              # Open browser to pprof index

The port is auto-discovered from the registry file, or specify explicitly:
  term-llm pprof cpu 6060`,
}

var pprofCPUCmd = &cobra.Command{
	Use:   "cpu [PORT]",
	Short: "Capture CPU profile",
	Long:  "Capture a CPU profile using go tool pprof. Default duration is 30 seconds.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := resolvePort(args)
		if err != nil {
			return err
		}

		url := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/profile?seconds=%d", port, pprofDuration)
		fmt.Fprintf(os.Stderr, "Capturing %d second CPU profile from %s...\n", pprofDuration, url)

		return runGoToolPprof(url)
	},
}

var pprofHeapCmd = &cobra.Command{
	Use:   "heap [PORT]",
	Short: "Capture heap profile",
	Long:  "Capture a memory allocation profile using go tool pprof.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := resolvePort(args)
		if err != nil {
			return err
		}

		url := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/heap", port)
		return runGoToolPprof(url)
	},
}

var pprofGoroutineCmd = &cobra.Command{
	Use:   "goroutine [PORT]",
	Short: "Dump goroutine stacks",
	Long:  "Dump all goroutine stack traces in human-readable format.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := resolvePort(args)
		if err != nil {
			return err
		}

		url := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/goroutine?debug=1", port)
		return runCurl(url)
	},
}

var pprofBlockCmd = &cobra.Command{
	Use:   "block [PORT]",
	Short: "Capture blocking profile",
	Long:  "Capture a blocking profile using go tool pprof.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := resolvePort(args)
		if err != nil {
			return err
		}

		url := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/block", port)
		return runGoToolPprof(url)
	},
}

var pprofMutexCmd = &cobra.Command{
	Use:   "mutex [PORT]",
	Short: "Capture mutex contention profile",
	Long:  "Capture a mutex contention profile using go tool pprof.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := resolvePort(args)
		if err != nil {
			return err
		}

		url := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/mutex", port)
		return runGoToolPprof(url)
	},
}

var pprofWebCmd = &cobra.Command{
	Use:   "web [PORT]",
	Short: "Open pprof index in browser",
	Long:  "Open the pprof index page in your default web browser.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		port, err := resolvePort(args)
		if err != nil {
			return err
		}

		url := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/", port)
		return openBrowser(url)
	},
}

// resolvePort gets the port from args or auto-discovers it.
func resolvePort(args []string) (int, error) {
	if len(args) > 0 {
		port, err := strconv.Atoi(args[0])
		if err != nil {
			return 0, fmt.Errorf("invalid port: %s", args[0])
		}
		if port < 1 || port > 65535 {
			return 0, fmt.Errorf("port %d out of range (must be 1-65535)", port)
		}
		return port, nil
	}

	// Auto-discover from registry
	port, running := pprofserver.IsServerRunning()
	if !running {
		return 0, fmt.Errorf("no pprof server running (start term-llm with --pprof)")
	}
	return port, nil
}

// runGoToolPprof runs "go tool pprof" with the given URL.
func runGoToolPprof(url string) error {
	cmd := exec.Command("go", "tool", "pprof", url)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runCurl fetches a URL and prints to stdout.
func runCurl(url string) error {
	cmd := exec.Command("curl", "-s", url)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// openBrowser opens the given URL in the default browser.
func openBrowser(url string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	return cmd.Start()
}
