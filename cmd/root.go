package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/exitcode"
	pprofserver "github.com/samsaffron/term-llm/internal/pprof"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/samsaffron/term-llm/internal/update"
	"github.com/spf13/cobra"
)

func init() {
	update.SetupUpdateChecks(rootCmd, Version)
	rootCmd.PersistentFlags().BoolVar(&debugRaw, "debug-raw", false, "Emit raw debug logs with timestamps")
	rootCmd.PersistentFlags().BoolVar(&showStats, "stats", false, "Show session statistics (time, tokens, tool calls)")
	rootCmd.PersistentFlags().BoolVar(&noSession, "no-session", false, "Disable session persistence (no reads/writes to sessions database)")
	rootCmd.PersistentFlags().StringVar(&sessionDBPath, "session-db", "", "Override sessions database path (defaults to ~/.local/share/term-llm/sessions.db)")
	rootCmd.PersistentFlags().StringVar(&cpuProfile, "cpuprofile", "", "Write CPU profile to file")
	rootCmd.PersistentFlags().StringVar(&memProfile, "memprofile", "", "Write memory profile to file")
	rootCmd.PersistentFlags().StringVar(&pprofFlag, "pprof", "", "Start pprof debug server (optionally specify port)")
	rootCmd.PersistentFlags().Lookup("pprof").NoOptDefVal = "0"

	rootCmd.AddCommand(memoryCmd)
}

var rootCmd = &cobra.Command{
	Use:   "term-llm",
	Short: "Translate natural language to CLI commands",
	Long: `term-llm uses AI to suggest shell commands based on your description.

Examples:
  term-llm exec "find all go files modified today"
  term-llm exec "compress this folder" --auto-pick
  term-llm exec "show disk usage" -s    # with web search

  term-llm config                       # view configuration
  term-llm config completion zsh        # shell completions`,
	CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		return startProfiling()
	},
	PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
		return stopProfiling()
	},
}

var debugRaw bool
var showStats bool
var noSession bool
var sessionDBPath string
var cpuProfile string
var memProfile string
var cpuProfileFile *os.File
var pprofFlag string
var pprofServer *pprofserver.Server

func startProfiling() error {
	if cpuProfile != "" {
		f, err := os.Create(cpuProfile)
		if err != nil {
			return err
		}
		cpuProfileFile = f
		if err := pprof.StartCPUProfile(f); err != nil {
			f.Close()
			return err
		}
	}

	// Check flag first, then env var for pprof server
	port, err := resolvePprofPort()
	if err != nil {
		return err
	}
	if port >= 0 {
		pprofServer = pprofserver.NewServer()
		actualPort, err := pprofServer.Start(port)
		if err != nil {
			return err
		}
		pprofserver.PrintUsage(os.Stderr, actualPort)
	}

	return nil
}

// resolvePprofPort returns the port for the pprof server.
// Returns -1 if pprof should not be started.
// Returns 0 for random port, or a specific port number.
// Returns an error for invalid port values.
func resolvePprofPort() (int, error) {
	// Check flag first (--pprof or --pprof=PORT)
	if pprofFlag != "" {
		port, err := strconv.Atoi(pprofFlag)
		if err != nil {
			return 0, fmt.Errorf("invalid --pprof value %q: must be a port number", pprofFlag)
		}
		if err := validatePort(port); err != nil {
			return 0, fmt.Errorf("invalid --pprof port: %w", err)
		}
		return port, nil
	}

	// Check environment variable
	if envPort := os.Getenv("TERM_LLM_PPROF"); envPort != "" {
		// Treat "1" or "true" as "enable with random port"
		if envPort == "1" || strings.EqualFold(envPort, "true") {
			return 0, nil
		}
		// Otherwise parse as explicit port number
		port, err := strconv.Atoi(envPort)
		if err != nil {
			return 0, fmt.Errorf("invalid TERM_LLM_PPROF value %q: must be a port number, '1', or 'true'", envPort)
		}
		if err := validatePort(port); err != nil {
			return 0, fmt.Errorf("invalid TERM_LLM_PPROF port: %w", err)
		}
		return port, nil
	}

	return -1, nil // pprof disabled
}

// validatePort checks that a port number is valid.
// Port 0 means "random port", ports 1-65535 are valid explicit ports.
func validatePort(port int) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("port %d out of range (must be 0-65535)", port)
	}
	return nil
}

func stopProfiling() error {
	if cpuProfileFile != nil {
		pprof.StopCPUProfile()
		cpuProfileFile.Close()
	}
	if memProfile != "" {
		f, err := os.Create(memProfile)
		if err != nil {
			return err
		}
		defer f.Close()
		runtime.GC()
		if err := pprof.WriteHeapProfile(f); err != nil {
			return err
		}
	}
	if pprofServer != nil {
		// Use a timeout context to avoid hanging on shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := pprofServer.Stop(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pprof server shutdown error: %v\n", err)
		}
	}
	return nil
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		if exitErr, ok := err.(exitcode.ExitError); ok {
			os.Exit(exitErr.Code)
		}
		os.Exit(1)
	}
}

func detectShell() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		return "bash"
	}
	// Extract shell name from path (e.g., /bin/zsh -> zsh)
	parts := strings.Split(shell, "/")
	return parts[len(parts)-1]
}

func executeCommand(command, shell string) error {
	ui.ShowCommand(command)

	cmd := exec.Command(shell, "-c", command)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}

	return nil
}
