package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/filelock"
	"github.com/samsaffron/term-llm/internal/passkeyauth"
	"github.com/samsaffron/term-llm/internal/userservice"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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
		return installUserService(cmd, kind, launch, serviceInstallOptions{binary: binary, directory: directory, secretsFile: secretsFile, secretNames: secretNames, noStart: noStart, dryRun: dryRun, yes: yes, custom: cmd.ArgsLenAtDash() >= 0})
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
		c := &cobra.Command{Use: action + " [web|hub]", Short: serviceActionDescription(action), Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return manageUserService(cmd, action, args) }}
		if action == "setup" || action == "recover" {
			c.Flags().Bool("print-setup-code", false, "Explicitly print the temporary enrollment code even to redirected output")
		}
		serviceCmd.AddCommand(c)
	}
	var follow bool
	logs := &cobra.Command{Use: "logs [web|hub]", Short: "Show native service logs", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		env, err := newServiceEnvironment()
		if err != nil {
			return err
		}
		kind, err := env.selectKind(args)
		if err != nil {
			return err
		}
		if err = env.native.CheckOwned(kind, env.specPath(kind)); err != nil {
			return err
		}
		return env.native.Logs(cmd.Context(), kind, env.specPath(kind), follow, cmd.OutOrStdout(), cmd.ErrOrStderr())
	}}
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "Follow new log entries")
	serviceCmd.AddCommand(logs)
	var specFile string
	runner := &cobra.Command{Use: "run <web|hub>", Hidden: true, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return runUserService(cmd, args[0], specFile) }}
	runner.Flags().StringVar(&specFile, "spec", "", "Private launch specification")
	serviceCmd.AddCommand(runner)
}
func serviceActionDescription(action string) string {
	return map[string]string{"status": "Show installed Web and Hub services", "open": "Open the service's configured browser URL", "start": "Enable autostart and start", "stop": "Stop and disable autostart", "restart": "Restart an enabled service (interrupts active work)", "uninstall": "Remove service registration, preserving credentials and user data", "setup": "Create a fresh first-passkey setup code and restart", "recover": "Authorize temporary passkey recovery and restart", "token": "Explicitly reveal the managed bearer token"}[action]
}

type serviceEnvironment struct {
	root, home string
	native     userservice.Native
}

func newServiceEnvironment() (serviceEnvironment, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return serviceEnvironment{}, err
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(cfg) {
		return serviceEnvironment{}, fmt.Errorf("XDG_CONFIG_HOME must be absolute for service management")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return serviceEnvironment{}, fmt.Errorf("services require Linux/systemd or macOS/launchd")
	}
	if os.Getuid() == 0 {
		return serviceEnvironment{}, fmt.Errorf("use service as your normal user, not root/sudo")
	}
	return serviceEnvironment{root: filepath.Join(cfg, "term-llm", "services"), home: home, native: userservice.Native{OS: runtime.GOOS, Home: home, ConfigHome: cfg, UID: os.Getuid()}}, nil
}
func (e serviceEnvironment) dir(kind string) string { return filepath.Join(e.root, kind) }
func (e serviceEnvironment) specPath(kind string) string {
	return filepath.Join(e.dir(kind), "service.json")
}
func (e serviceEnvironment) credentials(kind string) userservice.Credentials {
	return userservice.Credentials{OS: e.native.OS, Dir: e.dir(kind), Kind: kind}
}
func (e serviceEnvironment) selectKind(args []string) (string, error) {
	if len(args) > 0 {
		if userservice.ValidKind(args[0]) {
			return args[0], nil
		}
		return "", fmt.Errorf("choose web or hub")
	}
	var kinds []string
	for _, kind := range []string{"web", "hub"} {
		if _, err := os.Stat(e.native.Path(kind)); err == nil {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) != 1 {
		return "", fmt.Errorf("specify web or hub (%d installed services)", len(kinds))
	}
	return kinds[0], nil
}
func (e serviceEnvironment) lock(kind string) (func() error, error) {
	if err := userservice.PrivateDir(e.dir(kind)); err != nil {
		return nil, err
	}
	return filelock.TryLock(filepath.Join(e.dir(kind), "install.lock"))
}

type serviceInstallOptions struct {
	binary, directory, secretsFile string
	secretNames                    []string
	noStart, dryRun, yes, custom   bool
}

func installUserService(cmd *cobra.Command, kind string, args []string, opts serviceInstallOptions) error {
	e, err := newServiceEnvironment()
	if err != nil {
		return err
	}
	path := e.specPath(kind)
	if !opts.dryRun {
		unlock, err := e.lock(kind)
		if err != nil {
			return err
		}
		defer unlock()
	}
	old, oldErr := userservice.Load(path)
	if oldErr != nil && !os.IsNotExist(oldErr) {
		return oldErr
	}
	if !opts.custom && oldErr == nil {
		args = old.Args
	}
	spec, err := parseServiceLaunch(kind, args)
	if err != nil {
		return err
	}
	spec.Environment = map[string]string{}
	for _, entry := range os.Environ() {
		k, v, _ := strings.Cut(entry, "=")
		if userservice.EnvironmentKey(k) {
			spec.Environment[k] = v
		}
	}
	spec.Environment["HOME"] = e.home
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		v := spec.Environment[key]
		if v != "" && !filepath.IsAbs(v) {
			return fmt.Errorf("%s must be absolute", key)
		}
	}
	spec.Directory = e.home
	if oldErr == nil {
		spec.Directory = old.Directory
		spec.Secrets = append([]string(nil), old.Secrets...)
		spec.Environment = old.Environment
		spec.Binary = old.Binary
	}
	if opts.directory != "" {
		spec.Directory, err = filepath.Abs(opts.directory)
		if err != nil {
			return err
		}
	}
	if opts.binary != "" {
		spec.Binary, err = filepath.Abs(opts.binary)
	} else {
		spec.Binary, err = os.Executable()
	}
	if err != nil {
		return err
	}
	if info, err := os.Stat(spec.Binary); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("service binary must be an executable file: %s", spec.Binary)
	}
	if info, err := os.Stat(spec.Directory); err != nil || !info.IsDir() {
		return fmt.Errorf("working directory does not exist: %s", spec.Directory)
	}
	if spec.Auth == "passkey" && spec.AuthFile == "" {
		data := spec.Environment["XDG_DATA_HOME"]
		if data == "" {
			data = filepath.Join(spec.Environment["HOME"], ".local", "share")
		}
		sub := "web-auth"
		if kind == "hub" {
			sub = "hub"
		}
		spec.AuthFile = filepath.Join(data, "term-llm", sub, "auth.json")
		spec.Args = append(spec.Args, "--passkey-auth-file", spec.AuthFile)
	}
	if err = spec.Validate(); err != nil {
		return err
	}
	if err = e.native.CheckOwned(kind, path); err != nil {
		return err
	}
	if opts.dryRun {
		data, err := e.native.Render(spec, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Service: %s\nAuth: %s\nURL: %s\nWorking directory: %s\nNative file: %s\n\n%s", kind, spec.Auth, spec.URL, spec.Directory, e.native.Path(kind), data)
		return nil
	}

	if !opts.noStart {
		if err = e.native.Check(cmd.Context()); err != nil {
			return err
		}
		if err = e.native.CheckLoaded(cmd.Context(), kind, path); err != nil {
			return err
		}
	}
	// Probe whenever there cannot be a known managed listener on this bind.
	if !opts.noStart && (oldErr != nil || old.Host != spec.Host || old.Port != spec.Port || !e.native.Running(cmd.Context(), kind)) {
		if err := checkUserServicePort(spec); err != nil {
			return err
		}
	}

	count := 0
	if spec.Auth == "passkey" {
		count, err = serviceCredentialCount(spec)
		if err != nil {
			return err
		}
	}
	imported := map[string]string{}
	if opts.secretsFile != "" {
		imported, err = userservice.ImportSecrets(opts.secretsFile)
		if err != nil {
			return err
		}
	}
	for _, name := range opts.secretNames {
		if !userservice.SecretName(name) {
			return fmt.Errorf("unsupported credential name %q", name)
		}
		value, err := promptServiceSecret(cmd, name)
		if err != nil {
			return err
		}
		imported[name] = value
	}
	names := map[string]bool{}
	for _, name := range spec.Secrets {
		names[name] = true
	}
	tokenName := serviceTokenName(kind)
	if spec.Auth == "passkey" && kind == "web" {
		if _, ok := imported[tokenName]; ok {
			return fmt.Errorf("Web passkeys cannot use a bearer credential")
		}
		delete(names, tokenName)
	}
	if spec.Auth == "bearer" && !names[tokenName] && imported[tokenName] == "" {
		value, err := generateServeToken()
		if err != nil {
			return err
		}
		imported[tokenName] = value
		fmt.Fprintln(cmd.OutOrStdout(), "Generated a stable bearer token (not printed; use service token "+kind+").")
	}
	if spec.Register && !names[hubRegistrationTokenEnv] && imported[hubRegistrationTokenEnv] == "" {
		value, err := promptServiceSecret(cmd, hubRegistrationTokenEnv)
		if err != nil {
			return fmt.Errorf("reverse registration requires --secret %s or --secrets-file: %w", hubRegistrationTokenEnv, err)
		}
		imported[hubRegistrationTokenEnv] = value
	}
	for name, value := range imported {
		if err = e.credentials(kind).Put(cmd.Context(), name, value); err != nil {
			return err
		}
		names[name] = true
	}
	spec.Secrets = nil
	for name := range names {
		spec.Secrets = append(spec.Secrets, name)
	}
	sort.Strings(spec.Secrets)
	var ambient []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if userservice.SecretName(name) && !names[name] {
			ambient = append(ambient, name)
		}
	}
	if len(ambient) > 0 {
		sort.Strings(ambient)
		fmt.Fprintf(cmd.ErrOrStderr(), "Terminal credentials not imported: %s. If needed, use --secret NAME or --secrets-file.\n", strings.Join(ambient, ", "))
	}
	if _, err = e.credentials(kind).Load(cmd.Context(), spec.Secrets); err != nil {
		return err
	}
	if err = userservice.Save(path, spec); err != nil {
		return err
	}
	code := ""
	if spec.Auth == "passkey" {
		if count == 0 {
			code, err = e.newEnrollment(spec, false, count)
			if err != nil {
				return err
			}
		}
	}
	if err = e.native.Install(spec, path); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Installed %s user service. Configuration: %s\n", kind, path)
	if opts.noStart {
		fmt.Fprintf(cmd.OutOrStdout(), "Not started. Run: term-llm service start %s\n", kind)
		if code != "" {
			printServiceEnrollment(cmd, spec, code, false)
		}
		return nil
	}
	if err = e.native.Reconcile(cmd.Context(), kind, oldErr == nil && (!reflect.DeepEqual(old, spec) || len(imported) > 0 || code != "")); err != nil {
		return err
	}
	if err = waitUserService(cmd.Context(), spec, e.native); err != nil {
		return fmt.Errorf("service installed but not ready; inspect 'term-llm service logs %s': %w", kind, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Running: %s\nAutostart: at login\n", spec.URL)
	if e.native.OS == "linux" {
		fmt.Fprintln(cmd.OutOrStdout(), `For availability after logout/before login, optionally run: sudo loginctl enable-linger "$USER"`)
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "LaunchAgent availability requires your logged-in session; sleeping Macs are not continuously available.")
	}
	fmt.Fprintln(cmd.OutOrStdout(), "HTTP backend checked; provider credentials and remote HTTPS have not been verified.")
	if code != "" {
		printServiceEnrollment(cmd, spec, code, false)
	}
	if !opts.yes && serviceInteractive(cmd) && hubOutputIsTerminal(cmd.OutOrStdout()) {
		fmt.Fprint(cmd.OutOrStdout(), "Open in your browser? [Y/n] ")
		var answer string
		_, _ = fmt.Fscanln(cmd.InOrStdin(), &answer)
		if answer == "" || strings.EqualFold(answer, "y") {
			target := spec.URL
			if code != "" {
				target = strings.TrimRight(target, "/") + "/auth/setup"
			}
			if err = openBrowser(target); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Open %s manually: %v\n", target, err)
			}
		}
	}
	return nil
}
func serviceInteractive(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
func promptServiceSecret(cmd *cobra.Command, name string) (string, error) {
	if !serviceInteractive(cmd) {
		return "", fmt.Errorf("%s needs interactive masked input or --secrets-file", name)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s (stored for this service): ", name)
	f := cmd.InOrStdin().(*os.File)
	value, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", err
	}
	if len(value) == 0 {
		return "", fmt.Errorf("credential must not be empty")
	}
	return string(value), nil
}
func serviceTokenName(kind string) string {
	if kind == "hub" {
		return "TERM_LLM_HUB_TOKEN"
	}
	return "TERM_LLM_SERVE_TOKEN"
}

func checkUserServicePort(s userservice.Spec) error {
	listener, err := net.Listen("tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port)))
	if err != nil {
		return fmt.Errorf("port %d is occupied; choose -- --port <port>: %w", s.Port, err)
	}
	return listener.Close()
}

func waitUserService(ctx context.Context, s userservice.Spec, native userservice.Native) error {
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	target := "http://" + net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) + s.BasePath + "/healthz"
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && native.Running(ctx, s.Kind) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("backend did not answer health checks at %s", target)
		case <-tick.C:
		}
	}
}

func serviceCredentialCount(s userservice.Spec) (int, error) {
	if s.Auth != "passkey" {
		return 0, nil
	}
	if _, err := os.Lstat(s.AuthFile); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	endpoint, err := passkeyauth.ParseEndpoint(passkeyauth.EndpointOptions{PublicURL: s.URL})
	if err != nil {
		return 0, err
	}
	// Opening an existing store validates it but never rewrites or creates it.
	store, err := passkeyauth.OpenStore(passkeyauth.StoreOptions{Path: s.AuthFile, RPID: endpoint.RPID, ReadOnly: true})
	if err != nil {
		return 0, err
	}
	return store.CredentialCount(), nil
}

type serviceEnrollment struct {
	Recovery        bool   `json:"recovery"`
	CredentialCount int    `json:"credential_count"`
	Secret          string `json:"secret"`
}

func (e serviceEnvironment) newEnrollment(s userservice.Spec, recovery bool, count int) (string, error) {
	secret, display, err := passkeyauth.GenerateBootstrapSecret(nil)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(serviceEnrollment{Recovery: recovery, CredentialCount: count, Secret: string(secret)})
	if err != nil {
		return "", err
	}
	return display, userservice.WritePrivate(filepath.Join(e.dir(s.Kind), "enrollment.json"), data)
}
func printServiceEnrollment(cmd *cobra.Command, s userservice.Spec, code string, recovery bool) {
	explicit, _ := cmd.Flags().GetBool("print-setup-code")
	if !hubOutputIsTerminal(cmd.OutOrStdout()) && !explicit {
		action := "setup"
		if recovery {
			action = "recover"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Enrollment is pending. The temporary code was not printed to redirected output. Run 'term-llm service %s %s' in a terminal (or explicitly add --print-setup-code).\n", action, s.Kind)
		return
	}

	purpose := "setup"
	if recovery {
		purpose = "recover"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nOpen %s/auth/%s\nOne-time %s code: %s\nThe code is valid for 10 minutes after server startup.\n", strings.TrimRight(s.URL, "/"), purpose, purpose, code)
	fmt.Fprintf(cmd.OutOrStdout(), "If unfinished or expired: term-llm service %s %s\n", map[bool]string{false: "setup", true: "recover"}[recovery], s.Kind)
}
