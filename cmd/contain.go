package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/contain"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var containRunner contain.Runner = contain.OSRunner{}
var containInputIsTerminal = func(input any) bool {
	f, ok := input.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
var containNewTemplate string
var containNewSet []string
var containNewNoInput bool
var containImageSyncForce bool
var containShellUser string

var containCmd = &cobra.Command{
	Use:   "contain",
	Short: "Manage Docker Compose backed workspaces",
	Long: `Manage named global workspaces backed by Docker Compose.

Each workspace is stored under ~/.config/term-llm/containers/<name>/ with
compose.yaml as the source of truth. Phase 1 shells out to local docker compose
and does not integrate with LLMs.`,
}

var containNewCmd = &cobra.Command{
	Use:   "new <name>",
	Short: "Create a new Docker Compose workspace from a deterministic template",
	Args:  requireContainNameArg,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		templateValues, err := parseContainSetFlags(containNewSet)
		if err != nil {
			return err
		}
		dir, err := contain.CreateWorkspace(name, contain.CreateOptions{Template: containNewTemplate, CWD: cwd, Values: templateValues, NoInput: containNewNoInput, Stdin: cmd.InOrStdin(), Stdout: cmd.OutOrStdout()})
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Created contain workspace %q at %s\n", name, dir)
		fmt.Fprintf(cmd.OutOrStdout(), "Edit compose.yaml directly.\n")
		started, err := promptAndMaybeStartContainWorkspace(cmd, name)
		if err != nil {
			return err
		}
		printContainNextSteps(cmd, name, started)
		return nil
	},
}

var containStartCmd = &cobra.Command{
	Use:               "start <name>",
	Short:             "Start an existing contain workspace, creating containers if needed",
	Args:              requireContainNameArg,
	ValidArgsFunction: containWorkspaceNameCompletion,
	RunE: func(cmd *cobra.Command, args []string) error {
		return contain.Start(cmd.Context(), containRunner, args[0], cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

var containRestartCmd = &cobra.Command{
	Use:               "restart <name>",
	Short:             "Restart a contain workspace",
	Args:              requireContainNameArg,
	ValidArgsFunction: containWorkspaceNameCompletion,
	RunE: func(cmd *cobra.Command, args []string) error {
		return contain.Restart(cmd.Context(), containRunner, args[0], cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

var containStopCmd = &cobra.Command{
	Use:               "stop <name>",
	Short:             "Stop a contain workspace without deleting resources",
	Args:              requireContainNameArg,
	ValidArgsFunction: containWorkspaceNameCompletion,
	RunE: func(cmd *cobra.Command, args []string) error {
		return contain.Stop(cmd.Context(), containRunner, args[0], cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

var containRmCmd = &cobra.Command{
	Use:   "rm <name>",
	Short: "Permanently remove a contain workspace, including Compose-managed volumes",
	Long: `Permanently remove a contain workspace.

This runs docker compose down --volumes --remove-orphans for the workspace and
then deletes the workspace config directory under ~/.config/term-llm/containers.
This is destructive and cannot be undone. You must type YES to continue.`,
	Args:              requireContainNameArg,
	ValidArgsFunction: containWorkspaceNameCompletion,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		dir, err := contain.ContainerDir(name)
		if err != nil {
			return err
		}
		project := contain.ProjectName(name)
		fmt.Fprintf(cmd.ErrOrStderr(), "\nWARNING: destructive contain removal requested for %q.\n", name)
		fmt.Fprintf(cmd.ErrOrStderr(), "This will run: docker compose down --volumes --remove-orphans\n")
		fmt.Fprintf(cmd.ErrOrStderr(), "Project: %s\n", project)
		fmt.Fprintf(cmd.ErrOrStderr(), "Config directory to delete: %s\n", dir)
		fmt.Fprintf(cmd.ErrOrStderr(), "Compose-managed volumes for this workspace may be permanently removed.\n")
		fmt.Fprintf(cmd.ErrOrStderr(), "\nType YES to permanently remove %q: ", name)

		line, readErr := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if readErr != nil && strings.TrimSpace(line) == "" {
			return fmt.Errorf("aborted: confirmation required")
		}
		if strings.TrimSpace(line) != "YES" {
			return fmt.Errorf("aborted: confirmation did not match YES")
		}
		return contain.Remove(cmd.Context(), containRunner, name, cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

var containRebuildCmd = &cobra.Command{
	Use:   "rebuild <name>",
	Short: "Rebuild workspace images and recreate containers without deleting volumes",
	Long: `Rebuild workspace images and recreate containers without deleting volumes.

This runs docker compose build --pull --no-cache, then docker compose up -d
--force-recreate. Use it when the managed image Dockerfile changed, base images
changed, or a Dockerfile installs a moving target such as @latest.`,
	Args:              requireContainNameArg,
	ValidArgsFunction: containWorkspaceNameCompletion,
	RunE: func(cmd *cobra.Command, args []string) error {
		return contain.Rebuild(cmd.Context(), containRunner, args[0], cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

var containLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List contain workspaces and labelled Docker containers",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		entries, err := contain.List(cmd.Context(), containRunner, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		return contain.PrintList(entries, cmd.OutOrStdout())
	},
}

var containExecCmd = &cobra.Command{
	Use:                "exec <name> [cmd...]",
	Short:              "Run a command in a contain workspace service",
	Long:               "Run a command in a contain workspace service. Host PRIMARY selection proxying is disabled by default; set TERM_LLM_ENABLE_PRIMARY_SELECTION_PROXY=1 to opt in for trusted interactive commands.",
	DisableFlagParsing: true,
	Args:               requireContainNameArgAllowCommand,
	ValidArgsFunction:  containWorkspaceNameCompletion,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmdArgs := args[1:]
		return contain.Exec(cmd.Context(), containRunner, args[0], cmdArgs, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

var containShellCmd = &cobra.Command{
	Use:   "shell <name>",
	Short: "Open the configured shell in a contain workspace service",
	Long: `Open the configured shell in a contain workspace service. Use --user/-u to
run the shell as a specific container username or UID. If --user is omitted,
term-llm uses the service label org.term-llm.contain.user when present.

By default, host clipboard/PRIMARY selection access is not bridged into the
container. To opt in to middle-click PRIMARY selection proxying for trusted
interactive sessions, set TERM_LLM_ENABLE_PRIMARY_SELECTION_PROXY=1 before
running this command.`,
	Args:              requireContainNameArg,
	ValidArgsFunction: containWorkspaceNameCompletion,
	RunE: func(cmd *cobra.Command, args []string) error {
		user, err := cmd.Flags().GetString("user")
		if err != nil {
			return err
		}
		return contain.ShellWithOptions(cmd.Context(), containRunner, args[0], contain.ShellOptions{User: user}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

var containTemplatesCmd = &cobra.Command{
	Use:   "templates",
	Short: "List built-in contain templates",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		for _, name := range contain.BuiltinTemplateNames() {
			desc := ""
			switch name {
			case "agent":
				desc = "managed term-llm agent runtime workspace"
			case "basic":
				desc = "minimal Alpine Compose workspace"
			}
			if desc == "" {
				fmt.Fprintln(cmd.OutOrStdout(), name)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", name, desc)
			}
		}
		return nil
	},
}

var containImageCmd = &cobra.Command{
	Use:   "image",
	Short: "Manage contain image assets",
}

var containImageSyncCmd = &cobra.Command{
	Use:   "sync [image]",
	Short: "Write managed contain image assets into the config directory",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) > 1 {
			return fmt.Errorf("unexpected extra image argument(s): %s", strings.Join(args[1:], " "))
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		image := "agent"
		if len(args) == 1 {
			image = args[0]
		}
		result, err := contain.SyncImage(image, containImageSyncForce)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Synced contain image %q to %s\n", result.Name, result.Dir)
		for _, file := range result.Files {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", file)
		}
		return nil
	},
}

func requireContainNameArg(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("expecting workspace name: %s <name>", cmd.CommandPath())
	}
	if len(args) > 1 {
		return fmt.Errorf("unexpected extra argument(s) after workspace name: %s", strings.Join(args[1:], " "))
	}
	return nil
}

func requireContainNameArgAllowCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("expecting workspace name: %s <name> [cmd...]", cmd.CommandPath())
	}
	return nil
}

func containWorkspaceNameCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	entries, err := contain.Definitions()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var completions []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name, toComplete) {
			completions = append(completions, entry.Name+"\t"+containWorkspaceCompletionDescription(entry))
		}
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

func containWorkspaceCompletionDescription(entry contain.ListEntry) string {
	if entry.Status == "invalid" {
		return "invalid compose.yaml"
	}
	if strings.TrimSpace(entry.Service) != "" && entry.Service != "-" {
		return "service: " + entry.Service
	}
	return "contain workspace"
}

func containTemplateFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	var completions []string
	for _, name := range contain.BuiltinTemplateNames() {
		if !strings.HasPrefix(name, toComplete) {
			continue
		}
		desc := "built-in template"
		switch name {
		case "agent":
			desc = "managed term-llm agent runtime workspace"
		case "basic":
			desc = "minimal Alpine Compose workspace"
		}
		completions = append(completions, name+"\t"+desc)
	}
	// Do not disable file completion: --template also accepts file and directory paths.
	return completions, cobra.ShellCompDirectiveDefault
}

func parseContainSetFlags(values []string) (map[string]string, error) {
	out := map[string]string{}
	for _, raw := range values {
		key, value, ok := strings.Cut(raw, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid --set value %q: expected key=value", raw)
		}
		out[key] = value
	}
	return out, nil
}

func printContainWebUIInfo(cmd *cobra.Command, name string) {
	dir, err := contain.ContainerDir(name)
	if err != nil {
		return
	}
	envPath := filepath.Join(dir, ".env")
	values, err := readContainEnvFile(envPath)
	if err != nil {
		return
	}
	token := strings.TrimSpace(values["WEB_TOKEN"])
	if token == "" || strings.Contains(token, "{{") {
		return
	}
	port := strings.TrimSpace(values["WEB_PORT"])
	if port == "" || strings.Contains(port, "{{") {
		port = "8081"
	}
	basePath := strings.TrimSpace(values["WEB_BASE_PATH"])
	if basePath == "" || strings.Contains(basePath, "{{") {
		basePath = "/chat"
	}
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	fmt.Fprintf(cmd.OutOrStdout(), "  Web UI: http://localhost:%s%s\n", port, basePath)
	fmt.Fprintf(cmd.OutOrStdout(), "  Web UI bearer token: %s\n", token)
}

func printContainNextSteps(cmd *cobra.Command, name string, started bool) {
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "Next steps:")
	if !started {
		fmt.Fprintf(cmd.OutOrStdout(), "  Start: term-llm contain start %s\n", name)
	}
	if hasContainExecRecipe(name, "agent") {
		fmt.Fprintf(cmd.OutOrStdout(), "  Chat with agent: term-llm contain exec %s agent\n", name)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "  Open shell: term-llm contain shell %s\n", name)
	fmt.Fprintf(cmd.OutOrStdout(), "  Run command/recipe: term-llm contain exec %s <cmd-or-recipe> [args...]\n", name)
	fmt.Fprintf(cmd.OutOrStdout(), "  Force raw command if a recipe collides: term-llm contain exec %s -- <cmd...>\n", name)
	printContainWebUIInfo(cmd, name)
}

func hasContainExecRecipe(name, recipeName string) bool {
	composePath, err := contain.ComposePath(name)
	if err != nil {
		return false
	}
	info, err := contain.ReadComposeInfo(composePath)
	if err != nil || info.Invalid {
		return false
	}
	_, ok := info.Hints.ExecRecipes[recipeName]
	return ok
}

func promptAndMaybeStartContainWorkspace(cmd *cobra.Command, name string) (bool, error) {
	if containNewNoInput || !containCommandInputIsTerminal(cmd) {
		return false, nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Start container now? [Y/n]: ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "" || answer == "y" || answer == "yes" {
		fmt.Fprintf(cmd.OutOrStdout(), "Starting contain workspace %q...\n", name)
		return true, contain.Start(cmd.Context(), containRunner, name, cmd.OutOrStdout(), cmd.ErrOrStderr())
	}
	return false, nil
}

func containCommandInputIsTerminal(cmd *cobra.Command) bool {
	return containInputIsTerminal(cmd.InOrStdin())
}

func readContainEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key != "" {
			values[key] = value
		}
	}
	return values, nil
}

func init() {
	containNewCmd.Flags().StringVar(&containNewTemplate, "template", "agent", "Built-in template name or path to file/directory template")
	if err := containNewCmd.RegisterFlagCompletionFunc("template", containTemplateFlagCompletion); err != nil {
		panic(fmt.Sprintf("failed to register contain template completion: %v", err))
	}
	containNewCmd.Flags().StringArrayVar(&containNewSet, "set", nil, "Set template prompt value (key=value); may be specified multiple times")
	containNewCmd.Flags().BoolVar(&containNewNoInput, "no-input", false, "Do not prompt; use defaults and --set values only")
	containShellCmd.Flags().StringVarP(&containShellUser, "user", "u", "", "Username or UID to use for the shell inside the container")
	containImageSyncCmd.Flags().BoolVar(&containImageSyncForce, "force", false, "Overwrite non-managed existing image files")
	rootCmd.AddCommand(containCmd)
	containCmd.AddCommand(containNewCmd)
	containCmd.AddCommand(containStartCmd)
	containCmd.AddCommand(containRestartCmd)
	containCmd.AddCommand(containStopCmd)
	containCmd.AddCommand(containRmCmd)
	containCmd.AddCommand(containRebuildCmd)
	containCmd.AddCommand(containLsCmd)
	containCmd.AddCommand(containExecCmd)
	containCmd.AddCommand(containShellCmd)
	containCmd.AddCommand(containTemplatesCmd)
	containCmd.AddCommand(containImageCmd)
	containImageCmd.AddCommand(containImageSyncCmd)
}
