package cmd

import (
	"os"
	"os/exec"
	"runtime"
	"runtime/pprof"
	"strings"

	"github.com/samsaffron/term-llm/internal/exitcode"
	"github.com/samsaffron/term-llm/internal/ui"
	"github.com/samsaffron/term-llm/internal/update"
	"github.com/spf13/cobra"
)

func init() {
	update.SetupUpdateChecks(rootCmd, Version)
	rootCmd.PersistentFlags().BoolVar(&debugRaw, "debug-raw", false, "Emit raw debug logs with timestamps")
	rootCmd.PersistentFlags().BoolVar(&showStats, "stats", false, "Show session statistics (time, tokens, tool calls)")
	rootCmd.PersistentFlags().StringVar(&cpuProfile, "cpuprofile", "", "Write CPU profile to file")
	rootCmd.PersistentFlags().StringVar(&memProfile, "memprofile", "", "Write memory profile to file")
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
var cpuProfile string
var memProfile string
var cpuProfileFile *os.File

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
