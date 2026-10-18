package cmd

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/app/service"
	"github.com/samsaffron/term-llm/internal/userservice"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var serviceCmd = &cobra.Command{Use: "service", Short: "Install and manage per-user Web and Hub services", Long: `Manage one Web service and one Hub service through systemd (Linux) or launchd
(macOS). New installs default to loopback and passkey authentication. Server
arguments after -- use the existing serve web/hub flags. No sudo is required.

Examples:
  term-llm service install web
  term-llm service install hub
  term-llm service install web -- --auth bearer --port 8081
  term-llm service install hub -- --public-url https://hub.example.com/hub/
  term-llm service status
  term-llm service open web`, PersistentPreRunE: func(*cobra.Command, []string) error { return nil }, PersistentPostRunE: func(*cobra.Command, []string) error { return nil }}

func init() {
	rootCmd.AddCommand(serviceCmd)
	var noStart, dryRun, yes bool
	var binary, directory, secretsFile string
	var secretNames []string
	install := &cobra.Command{Use: "install [web|hub] [-- serve flags]", Short: "Install or reconcile a service (defaults to web)", RunE: func(cmd *cobra.Command, args []string) error {
		kind := "web"
		var launch []string
		before := args
		if n := cmd.ArgsLenAtDash(); n >= 0 {
			before = args[:n]
			launch = args[n:]
		}
		if len(before) > 1 {
			return fmt.Errorf("use install [web|hub] -- <serve flags>")
		}
		if len(before) == 1 {
			kind = before[0]
		}
		if !userservice.ValidKind(kind) {
			return fmt.Errorf("choose web or hub")
		}
		return serviceApp.Install(cmd, kind, launch, service.InstallOptions{Binary: binary, Directory: directory, SecretsFile: secretsFile, SecretNames: secretNames, NoStart: noStart, DryRun: dryRun, Yes: yes, Custom: cmd.ArgsLenAtDash() >= 0})
	}}
	install.Flags().BoolVar(&noStart, "no-start", false, "Write managed files without enabling or starting the service")
	install.Flags().BoolVar(&dryRun, "dry-run", false, "Print the launch plan and native definition without writing files or secrets")
	install.Flags().BoolVarP(&yes, "yes", "y", false, "Do not prompt to open the browser")
	install.Flags().StringVar(&binary, "binary", "", "Executable to supervise (default: this executable)")
	install.Flags().StringVar(&directory, "working-directory", "", "Working directory (default: home; preserved on reinstall)")
	install.Flags().StringVar(&secretsFile, "secrets-file", "", "Import literal KEY=value credentials from a private file")
	install.Flags().StringArrayVar(&secretNames, "secret", nil, "Securely prompt for a named credential (repeatable; never a value on argv)")
	install.Flags().Bool("print-setup-code", false, "Explicitly print the temporary enrollment code even to redirected output")
	serviceCmd.AddCommand(install)
	for _, action := range []string{"status", "open", "start", "stop", "restart", "uninstall", "setup", "recover", "token"} {
		action := action
		c := &cobra.Command{Use: action + " [web|hub]", Short: service.ActionDescription(action), Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return serviceApp.Manage(cmd, action, args) }}
		if action == "start" || action == "restart" {
			c.Flags().Bool("debug", false, "Trace startup checks and native commands; include status and recent logs on failure")
		}
		if action == "setup" || action == "recover" {
			c.Flags().Bool("print-setup-code", false, "Explicitly print the temporary enrollment code even to redirected output")
		}
		serviceCmd.AddCommand(c)
	}
	var follow bool
	logs := &cobra.Command{Use: "logs [web|hub]", Short: "Show native service logs", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return service.Logs(cmd, args, follow)
	}}
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "Follow new log entries")
	serviceCmd.AddCommand(logs)
	var specFile string
	runner := &cobra.Command{Use: "run <web|hub>", Hidden: true, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return serviceApp.Run(cmd, args[0], specFile) }}
	runner.Flags().StringVar(&specFile, "spec", "", "Private launch specification")
	serviceCmd.AddCommand(runner)
}

// serviceApp binds the service application to serve's flags and validators.
var serviceApp = service.App{Hooks: service.Hooks{
	LaunchFlags: func(kind string) []*pflag.FlagSet {
		command := serveCmd
		if kind == "hub" {
			command = serveHubCmd
		}
		return []*pflag.FlagSet{command.Flags(), command.InheritedFlags(), rootCmd.PersistentFlags()}
	},
	ResolveWebAuthMode:          resolveServeAuthMode,
	ResolveHubAuthMode:          resolveHubAuthMode,
	ValidateBind:                validateHubBind,
	IsLoopbackHost:              isLoopbackHost,
	ValidateWebPasskeyTransport: validateWebPasskeyTransport,
	NormalizeBasePath:           normalizeBasePath,
	NormalizeHubBasePath:        normalizeHubBasePath,
	GenerateToken:               generateServeToken,
	OpenBrowser:                 openBrowser,
	ReplaceProcess:              replaceProcess,
	OutputIsTerminal:            func(w any) bool { return hubOutputIsTerminal(w) },
	RegistrationTokenEnv:        hubRegistrationTokenEnv,
}}
