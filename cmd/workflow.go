package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	workflowpkg "github.com/samsaffron/term-llm/internal/workflow"
	"github.com/spf13/cobra"
)

var (
	workflowRunInputs        []string
	workflowRunInputJSON     string
	workflowRunAgent         string
	workflowRunProvider      string
	workflowRunConcurrency   int
	workflowRunAgentTimeout  time.Duration
	workflowRunTimeout       time.Duration
	workflowRunJSON          bool
	workflowRunAgentTools    []string
	workflowRunAgentRead     []string
	workflowRunAgentWrite    []string
	workflowRunAgentShell    []string
	workflowRunWorkspaceRoot []string
	workflowValidateJSON     bool
)

var workflowCmd = &cobra.Command{
	Use:   "workflow",
	Short: "Run capability-scoped Lua workflows",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var workflowRunCmd = &cobra.Command{
	Use:   "run <path.lua>",
	Short: "Run one explicit Lua workflow file",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkflow,
}

var workflowValidateCmd = &cobra.Command{
	Use:   "validate <path.lua>",
	Short: "Validate workflow syntax and metadata without executing it",
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkflowValidate,
}

func init() {
	workflowRunCmd.Flags().StringArrayVar(&workflowRunInputs, "input", nil, "Workflow input as key=value (repeatable)")
	workflowRunCmd.Flags().StringVar(&workflowRunInputJSON, "input-json", "", "Workflow inputs as a JSON object")
	workflowRunCmd.Flags().StringVar(&workflowRunAgent, "agent", "", "Override the agent for every agent task")
	workflowRunCmd.Flags().StringVar(&workflowRunProvider, "provider", "", "Override the provider for every agent task")
	workflowRunCmd.Flags().IntVar(&workflowRunConcurrency, "concurrency", 4, "Maximum concurrent agent tasks")
	workflowRunCmd.Flags().DurationVar(&workflowRunAgentTimeout, "agent-timeout", 5*time.Minute, "Maximum duration of each agent task")
	workflowRunCmd.Flags().DurationVar(&workflowRunTimeout, "timeout", 30*time.Minute, "Maximum duration of the whole workflow")
	workflowRunCmd.Flags().StringSliceVar(&workflowRunAgentTools, "agent-tool", nil, "Tool capability ceiling for dynamic run_agent tasks")
	workflowRunCmd.Flags().StringSliceVar(&workflowRunAgentRead, "agent-read-dir", nil, "Read-directory capability ceiling for dynamic run_agent tasks")
	workflowRunCmd.Flags().StringSliceVar(&workflowRunAgentWrite, "agent-write-dir", nil, "Write-directory capability ceiling for dynamic run_agent tasks")
	workflowRunCmd.Flags().StringSliceVar(&workflowRunAgentShell, "agent-shell-allow", nil, "Shell pattern capability ceiling for dynamic run_agent tasks")
	workflowRunCmd.Flags().StringSliceVar(&workflowRunWorkspaceRoot, "workspace-root", nil, "Destination root ceiling for create_workspace")
	workflowRunCmd.Flags().BoolVar(&workflowRunJSON, "json", false, "Print the full workflow result as JSON")
	workflowValidateCmd.Flags().BoolVar(&workflowValidateJSON, "json", false, "Print metadata as JSON")
	workflowCmd.AddCommand(workflowRunCmd, workflowValidateCmd)
	rootCmd.AddCommand(workflowCmd)
}

type workflowRunOutput struct {
	Name       string `json:"name"`
	SourcePath string `json:"source_path"`
	SHA256     string `json:"source_sha256"`
	DurationMS int64  `json:"duration_ms"`
	Result     any    `json:"result"`
}

func runWorkflowValidate(cmd *cobra.Command, args []string) error {
	definition, err := workflowpkg.ParseDefinitionFile(args[0])
	if err != nil {
		return err
	}
	if workflowValidateJSON {
		return writeWorkflowJSON(cmd, definition)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s: valid (%s)\n", definition.Name, definition.SHA256)
	return nil
}

func runWorkflow(cmd *cobra.Command, args []string) error {
	if workflowRunConcurrency < 1 {
		return fmt.Errorf("--concurrency must be at least 1")
	}
	if workflowRunAgentTimeout <= 0 {
		return fmt.Errorf("--agent-timeout must be greater than zero")
	}
	if workflowRunTimeout <= 0 {
		return fmt.Errorf("--timeout must be greater than zero")
	}
	inputs, err := parseWorkflowInputs(workflowRunInputJSON, workflowRunInputs)
	if err != nil {
		return err
	}
	path, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolve workflow path: %w", err)
	}
	definition, err := workflowpkg.ParseDefinitionFile(path)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve term-llm executable: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve workflow CWD: %w", err)
	}
	engine := workflowpkg.Engine{Executor: workflowpkg.CommandExecutor{Executable: executable}}
	runCtx, cancel := context.WithTimeout(cmd.Context(), workflowRunTimeout)
	defer cancel()
	started := time.Now()
	result, err := engine.Execute(runCtx, definition.Source, workflowpkg.ExecuteOptions{
		Inputs:           inputs,
		Agent:            workflowRunAgent,
		Provider:         workflowRunProvider,
		Concurrency:      workflowRunConcurrency,
		AgentTimeout:     workflowRunAgentTimeout,
		CWD:              cwd,
		AllowedTools:     workflowRunAgentTools,
		AllowedRead:      workflowRunAgentRead,
		AllowedWrite:     workflowRunAgentWrite,
		AllowedShell:     workflowRunAgentShell,
		AllowedWorkspace: workflowRunWorkspaceRoot,
	})
	if err != nil {
		return err
	}
	output := workflowRunOutput{
		Name:       definition.Name,
		SourcePath: definition.Path,
		SHA256:     definition.SHA256,
		DurationMS: time.Since(started).Milliseconds(),
		Result:     result,
	}
	if workflowRunJSON {
		return writeWorkflowJSON(cmd, output)
	}
	if text, ok := result.(string); ok {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), text)
		return err
	}
	return writeWorkflowJSON(cmd, output)
}

func parseWorkflowInputs(inputJSON string, pairs []string) (map[string]any, error) {
	inputs := make(map[string]any)
	if strings.TrimSpace(inputJSON) != "" {
		decoder := json.NewDecoder(strings.NewReader(inputJSON))
		if err := decoder.Decode(&inputs); err != nil {
			return nil, fmt.Errorf("parse --input-json: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("parse --input-json: trailing JSON value")
			}
			return nil, fmt.Errorf("parse --input-json: %w", err)
		}
		if inputs == nil {
			inputs = make(map[string]any)
		}
	}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid --input %q (expected key=value)", pair)
		}
		inputs[key] = value
	}
	return inputs, nil
}

func writeWorkflowJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
