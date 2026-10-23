package service

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
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

// Hooks are the cmd-owned behaviours the service launcher shares with serve.
type Hooks struct {
	// LaunchFlags returns the flag sets that define the serve <kind> command.
	LaunchFlags                 func(kind string) []*pflag.FlagSet
	ResolveWebAuthMode          func(authSet bool, mode string, noAuthSet, noAuth bool) (string, error)
	ResolveHubAuthMode          func(mode string) (string, error)
	ValidateBind                func(host string, port int, auth bool) error
	IsLoopbackHost              func(host string) bool
	ValidateWebPasskeyTransport func(hubURL string, register bool, connect string, cors []string) error
	NormalizeBasePath           func(string) (string, error)
	NormalizeHubBasePath        func(string) (string, error)
	GenerateToken               func() (string, error)
	OpenBrowser                 func(string) error
	ReplaceProcess              func(executable string, args, env []string) error
	OutputIsTerminal            func(any) bool
	RegistrationTokenEnv        string
}

// InstallOptions are the values bound to the install command's flags.
type InstallOptions struct {
	Binary, Directory, SecretsFile string
	SecretNames                    []string
	NoStart, DryRun, Yes, Custom   bool
}

// App implements the per-user service commands; Cobra wiring stays in cmd.
type App struct{ Hooks }

// InstallOptions are the values bound to the install command's flags.

// Install installs or reconciles one managed service.
func (a App) Install(cmd *cobra.Command, kind string, args []string, opts InstallOptions) error {
	return a.installUserService(cmd, kind, args, opts)
}

// Manage runs a lifecycle action (status, open, start, ...).
func (a App) Manage(cmd *cobra.Command, action string, args []string) error {
	return a.manageUserService(cmd, action, args)
}

// Run replaces this process with the supervised serve command.
func (a App) Run(cmd *cobra.Command, kind, specFile string) error {
	return a.runUserService(cmd, kind, specFile)
}

// Logs shows native service logs.
func Logs(cmd *cobra.Command, args []string, follow bool) error {
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
}

// ActionDescription is the short help for a lifecycle action command.
func ActionDescription(action string) string {
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

func (a App) installUserService(cmd *cobra.Command, kind string, args []string, opts InstallOptions) error {
	e, err := newServiceEnvironment()
	if err != nil {
		return err
	}
	path := e.specPath(kind)
	if !opts.DryRun {
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
	if !opts.Custom && oldErr == nil {
		args = old.Args
	}
	var oldSpec *userservice.Spec
	if oldErr == nil {
		oldSpec = &old
	}
	spec, err := a.buildServiceInstallSpec(e, kind, args, opts, oldSpec)
	if err != nil {
		return err
	}
	if err = spec.Validate(); err != nil {
		return err
	}
	if err = e.native.CheckOwned(kind, path); err != nil {
		return err
	}
	if opts.DryRun {
		return printServiceDryRun(cmd, e, kind, spec, path)
	}

	if !opts.NoStart {
		if err = e.native.Check(cmd.Context()); err != nil {
			return err
		}
		if err = e.native.CheckLoaded(cmd.Context(), kind, path); err != nil {
			return err
		}
	}
	// Probe whenever there cannot be a known managed listener on this bind.
	if shouldProbeServiceInstallPort(cmd, e, kind, spec, old, oldErr, opts) {
		if err := checkUserServicePort(spec); err != nil {
			return err
		}
	}

	count, err := countServiceInstallCredentials(spec)
	if err != nil {
		return err
	}
	imported, names, err := a.collectServiceSecrets(cmd, e, kind, spec, opts)
	if err != nil {
		return err
	}
	if err = storeServiceSecrets(cmd, e, kind, &spec, imported, names); err != nil {
		return err
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
	definitionReplaced, err := e.native.Install(spec, path)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Installed %s user service. Configuration: %s\n", kind, path)
	if opts.NoStart {
		a.reportServiceNotStarted(cmd, spec, kind, code)
		return nil
	}
	if err = e.native.Reconcile(cmd.Context(), kind, serviceInstallNeedsRestart(oldErr, old, spec, imported, code, definitionReplaced)); err != nil {
		return err
	}
	if err = waitUserService(cmd.Context(), spec, e.native); err != nil {
		return fmt.Errorf("service installed but not ready; inspect 'term-llm service logs %s': %w", kind, err)
	}
	a.reportServiceInstalled(cmd, e, spec, kind, code, opts)
	return nil
}

func (a App) buildServiceInstallSpec(e serviceEnvironment, kind string, args []string, opts InstallOptions, old *userservice.Spec) (userservice.Spec, error) {
	spec, err := a.parseServiceLaunch(kind, args)
	if err != nil {
		return userservice.Spec{}, err
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
			return userservice.Spec{}, fmt.Errorf("%s must be absolute", key)
		}
	}
	spec.Directory = e.home
	if old != nil {
		spec.Directory = old.Directory
		spec.Secrets = append([]string(nil), old.Secrets...)
		spec.Environment = old.Environment
		spec.Binary = old.Binary
	}
	if opts.Directory != "" {
		spec.Directory, err = filepath.Abs(opts.Directory)
		if err != nil {
			return userservice.Spec{}, err
		}
	}
	// A bare reinstall keeps the recorded executable. Silently adopting the
	// invoking binary (often a checkout build) would change the service's code
	// identity and, on macOS, its Local Network permission.
	if opts.Binary != "" {
		spec.Binary, err = filepath.Abs(opts.Binary)
	} else if spec.Binary == "" {
		spec.Binary, err = os.Executable()
	}
	if err != nil {
		return userservice.Spec{}, err
	}
	if info, err := os.Stat(spec.Binary); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return userservice.Spec{}, fmt.Errorf("service binary must be an executable file: %s (select one with --binary)", spec.Binary)
	}
	if info, err := os.Stat(spec.Directory); err != nil || !info.IsDir() {
		return userservice.Spec{}, fmt.Errorf("working directory does not exist: %s", spec.Directory)
	}
	defaultServiceAuthFile(&spec, kind)
	return spec, nil
}

func defaultServiceAuthFile(spec *userservice.Spec, kind string) {
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
}

func printServiceDryRun(cmd *cobra.Command, e serviceEnvironment, kind string, spec userservice.Spec, path string) error {
	data, err := e.native.Render(spec, path)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Service: %s\nAuth: %s\nURL: %s\nWorking directory: %s\nNative file: %s\n\n%s", kind, spec.Auth, spec.URL, spec.Directory, e.native.Path(kind), data)
	return nil
}

func shouldProbeServiceInstallPort(cmd *cobra.Command, e serviceEnvironment, kind string, spec, old userservice.Spec, oldErr error, opts InstallOptions) bool {
	return !opts.NoStart && (oldErr != nil || old.Host != spec.Host || old.Port != spec.Port || !e.native.Running(cmd.Context(), kind))
}

func countServiceInstallCredentials(spec userservice.Spec) (int, error) {
	if spec.Auth == "passkey" {
		return serviceCredentialCount(spec)
	}
	return 0, nil
}

func (a App) collectServiceSecrets(cmd *cobra.Command, e serviceEnvironment, kind string, spec userservice.Spec, opts InstallOptions) (map[string]string, map[string]bool, error) {
	imported := map[string]string{}
	if opts.SecretsFile != "" {
		var err error
		imported, err = userservice.ImportSecrets(opts.SecretsFile)
		if err != nil {
			return nil, nil, err
		}
	}
	for _, name := range opts.SecretNames {
		if !userservice.SecretName(name) {
			return nil, nil, fmt.Errorf("unsupported credential name %q", name)
		}
		value, err := promptServiceSecret(cmd, name)
		if err != nil {
			return nil, nil, err
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
			return nil, nil, fmt.Errorf("Web passkeys cannot use a bearer credential")
		}
		delete(names, tokenName)
	}
	if spec.Auth == "bearer" && !names[tokenName] && imported[tokenName] == "" {
		value, err := a.GenerateToken()
		if err != nil {
			return nil, nil, err
		}
		imported[tokenName] = value
		fmt.Fprintln(cmd.OutOrStdout(), "Generated a stable bearer token (not printed; use service token "+kind+").")
	}
	if spec.Register && !names[a.RegistrationTokenEnv] && imported[a.RegistrationTokenEnv] == "" {
		value, err := promptServiceSecret(cmd, a.RegistrationTokenEnv)
		if err != nil {
			return nil, nil, fmt.Errorf("reverse registration requires --secret %s or --secrets-file: %w", a.RegistrationTokenEnv, err)
		}
		imported[a.RegistrationTokenEnv] = value
	}
	return imported, names, nil
}

func storeServiceSecrets(cmd *cobra.Command, e serviceEnvironment, kind string, spec *userservice.Spec, imported map[string]string, names map[string]bool) error {
	for name, value := range imported {
		if err := e.credentials(kind).Put(cmd.Context(), name, value); err != nil {
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
	return nil
}

func (a App) reportServiceNotStarted(cmd *cobra.Command, spec userservice.Spec, kind, code string) {
	fmt.Fprintf(cmd.OutOrStdout(), "Not started. Run: term-llm service start %s\n", kind)
	if code != "" {
		a.printServiceEnrollment(cmd, spec, code, false)
	}
}

func serviceInstallNeedsRestart(oldErr error, old, spec userservice.Spec, imported map[string]string, code string, definitionReplaced bool) bool {
	// A replaced native definition (e.g. a new launchd key after an upgrade)
	// only takes effect once the loaded job is reloaded.
	return definitionReplaced || (oldErr == nil && (!reflect.DeepEqual(old, spec) || len(imported) > 0 || code != ""))
}

func (a App) reportServiceInstalled(cmd *cobra.Command, e serviceEnvironment, spec userservice.Spec, kind, code string, opts InstallOptions) {
	fmt.Fprintf(cmd.OutOrStdout(), "Running: %s\nAutostart: at login\n", spec.URL)
	if e.native.OS == "linux" {
		fmt.Fprintln(cmd.OutOrStdout(), `For availability after logout/before login, optionally run: sudo loginctl enable-linger "$USER"`)
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "LaunchAgent availability requires your logged-in session; sleeping Macs are not continuously available.")
	}
	fmt.Fprintln(cmd.OutOrStdout(), "HTTP backend checked; provider credentials and remote HTTPS have not been verified.")
	if code != "" {
		a.printServiceEnrollment(cmd, spec, code, false)
	}
	if !opts.Yes && serviceInteractive(cmd) && a.OutputIsTerminal(cmd.OutOrStdout()) {
		fmt.Fprint(cmd.OutOrStdout(), "Open in your browser? [Y/n] ")
		var answer string
		_, _ = fmt.Fscanln(cmd.InOrStdin(), &answer)
		if answer == "" || strings.EqualFold(answer, "y") {
			target := spec.URL
			if code != "" {
				target = strings.TrimRight(target, "/") + "/auth/setup"
			}
			if err := a.OpenBrowser(target); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Open %s manually: %v\n", target, err)
			}
		}
	}
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
	timer := time.NewTimer(serviceReadyTimeout)
	defer timer.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	last := "no health response"
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
		resp, err := client.Do(req)
		if err != nil {
			last = err.Error()
		} else {
			last = resp.Status
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && native.Running(ctx, s.Kind) {
				return nil
			}
			if resp.StatusCode == http.StatusOK {
				last += "; native manager reports no running PID"
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("backend did not answer health checks at %s (last observation: %s)%s", target, last, serviceNotRunningSummary(ctx, s.Kind, native))
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
func (a App) printServiceEnrollment(cmd *cobra.Command, s userservice.Spec, code string, recovery bool) {
	explicit, _ := cmd.Flags().GetBool("print-setup-code")
	if !a.OutputIsTerminal(cmd.OutOrStdout()) && !explicit {
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
