package cmd

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/contain"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The unit lives in an editable .service file so it keeps editor support
// while the binary stays a single go:embed artifact, matching the hub
// dashboard convention.
//
//go:embed templates/term-llm-hub.service
var serveHubUnitTemplate string

var serveHubUnitTmpl = template.Must(template.New("hub-unit").Parse(serveHubUnitTemplate))

const (
	serveHubSystemdUnitName = "term-llm-hub.service"
	serveHubSystemdUnitPath = "/etc/systemd/system/" + serveHubSystemdUnitName
	serveHubSystemdEnvPath  = "/etc/term-llm-hub.env"
	serveHubTokenEnv        = "TERM_LLM_HUB_TOKEN"

	// serveHubSystemdSettleDelay is how long install waits after starting the
	// service before confirming it is still running: long enough to catch a
	// service that execs fine but exits moments later on bad config.
	serveHubSystemdSettleDelay = 1500 * time.Millisecond
)

var serveHubSystemdInstall bool

var serveHubSystemdCmd = &cobra.Command{
	Use:   "systemd",
	Short: "Generate a systemd unit that runs the Hub as a background service",
	Long: `Generate a systemd unit that keeps the term-llm Hub running in the background.

Without --install the unit is printed to stdout for review or manual
installation. With --install (Linux, run with sudo) the unit is written to
/etc/systemd/system/term-llm-hub.service, the bearer and registration tokens
are stored in /etc/term-llm-hub.env (mode 0600), and the service is enabled
and started via systemctl.

Hub flags set on this command (for example --host or --port) are baked into
the unit's ExecStart line. --token and --registration-token never appear in
the unit: they are written to the environment file, which the Hub reads via
TERM_LLM_HUB_TOKEN and TERM_LLM_HUB_REGISTRATION_TOKEN. On reinstall, tokens
already present in the environment file are kept unless overridden (pass an
explicitly empty --registration-token to disable self-registration), and a
bearer token is generated when none is configured anywhere.

Examples:
  # Print the unit for review
  term-llm serve hub systemd --host 0.0.0.0

  # Install and start the Hub service (the bearer token is generated and printed)
  sudo term-llm serve hub systemd --host 0.0.0.0 --install

  # Install with explicit tokens; pass them as env vars after sudo so they
  # stay out of the command line (a plain VAR=x prefix is stripped by sudo)
  sudo TERM_LLM_HUB_TOKEN=S3CR3T TERM_LLM_HUB_REGISTRATION_TOKEN=R3G term-llm serve hub systemd --host 0.0.0.0 --install`,
	Args: cobra.NoArgs,
	RunE: runServeHubSystemd,
}

func runServeHubSystemd(cmd *cobra.Command, args []string) error {
	authMode, err := resolveServeAuthMode(cmd.Flags().Changed("auth"), serveHubAuthMode, false, false)
	if err != nil {
		return err
	}
	requireAuth := authMode != "none"
	if err := validateHubBind(serveHubHost, serveHubPort, requireAuth); err != nil {
		return err
	}
	if _, err := normalizeHubBasePath(serveHubBasePath); err != nil {
		return err
	}

	exe, err := serveHubSystemdExecutable()
	if err != nil {
		return err
	}
	execStart, err := buildServeHubSystemdExecStart(exe, cmd.Flags())
	if err != nil {
		return err
	}
	unit, err := renderServeHubSystemdUnit(execStart, serveHubSystemdEnvPath)
	if err != nil {
		return err
	}

	if !serveHubSystemdInstall {
		if _, err := cmd.OutOrStdout().Write(unit); err != nil {
			return err
		}
		errOut := cmd.ErrOrStderr()
		fmt.Fprintf(errOut, "\nTo install: save as %s, then run `systemctl daemon-reload` and `systemctl enable --now %s`.\n", serveHubSystemdUnitPath, serveHubSystemdUnitName)
		fmt.Fprintf(errOut, "Tokens are read from %s (%s, %s); or re-run with --install to write both files and start the service.\n", serveHubSystemdEnvPath, serveHubTokenEnv, hubRegistrationTokenEnv)
		return nil
	}

	installer := serveHubSystemdInstaller{
		unitPath:  serveHubSystemdUnitPath,
		envPath:   serveHubSystemdEnvPath,
		goos:      runtime.GOOS,
		settle:    serveHubSystemdSettleDelay,
		systemctl: runSystemctl,
	}
	return installer.install(cmd.Context(), unit, requireAuth, serveHubToken, serveHubRegistrationTokenFlag, cmd.Flags().Changed("registration-token"), cmd.OutOrStdout())
}

func serveHubSystemdExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("determine executable path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != "" {
		exe = resolved
	}
	return exe, nil
}

// serveHubSystemdBakedFlags are the hub flags copied into the generated
// unit's ExecStart line, in the order they appear there. --token and
// --registration-token are deliberately absent: they reach the Hub via the
// environment file so secrets never land in the unit.
var serveHubSystemdBakedFlags = []string{"host", "port", "auth", "base-path", "config", "contain", "nodes-file"}

// buildServeHubSystemdExecStart bakes the hub flags the user explicitly set
// into the ExecStart line, so the service reproduces exactly the invocation
// that was reviewed. Path flags are absolutized because the service does not
// run from the caller's cwd.
func buildServeHubSystemdExecStart(exe string, flags *pflag.FlagSet) (string, error) {
	quotedExe, err := systemdQuoteExecPath(exe)
	if err != nil {
		return "", fmt.Errorf("executable path: %w", err)
	}
	parts := []string{quotedExe, "serve", "hub"}
	for _, name := range serveHubSystemdBakedFlags {
		if !flags.Changed(name) {
			continue
		}
		value := flags.Lookup(name).Value.String()
		if name == "config" || name == "nodes-file" {
			abs, err := filepath.Abs(value)
			if err != nil {
				return "", fmt.Errorf("resolve --%s path: %w", name, err)
			}
			value = abs
		}
		quoted, err := systemdQuoteArg("--" + name + "=" + value)
		if err != nil {
			return "", fmt.Errorf("--%s: %w", name, err)
		}
		parts = append(parts, quoted)
	}
	return strings.Join(parts, " "), nil
}

// systemdQuoteArg renders one ExecStart argument word per systemd's quoting
// rules: $ and % are expanded even inside double quotes so they are doubled,
// and words containing whitespace or quote characters are double-quoted with
// backslash escapes. Control characters cannot be represented safely in a
// unit file and are rejected.
func systemdQuoteArg(s string) (string, error) {
	return systemdQuote(s, true)
}

// systemdQuoteExecPath renders the ExecStart command word. Unlike argument
// words it is never subject to $VAR expansion (systemd resolves the path
// verbatim), so $ must stay single or the exec'd path would be wrong.
func systemdQuoteExecPath(s string) (string, error) {
	return systemdQuote(s, false)
}

func systemdQuote(s string, escapeDollar bool) (string, error) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("value %q must not contain control characters", s)
		}
	}
	s = strings.ReplaceAll(s, "%", "%%")
	if escapeDollar {
		s = strings.ReplaceAll(s, "$", "$$")
	}
	if s != "" && !strings.ContainsAny(s, " \"'\\;") {
		return s, nil
	}
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
	return `"` + escaped + `"`, nil
}

type serveHubUnitView struct {
	ExecStart string
	EnvFile   string
}

func renderServeHubSystemdUnit(execStart, envPath string) ([]byte, error) {
	var buf bytes.Buffer
	if err := serveHubUnitTmpl.Execute(&buf, serveHubUnitView{ExecStart: execStart, EnvFile: envPath}); err != nil {
		return nil, fmt.Errorf("render hub unit template: %w", err)
	}
	return buf.Bytes(), nil
}

// resolveServeHubSystemdTokens resolves the tokens destined for the
// environment file. Bearer precedence: --token flag > TERM_LLM_HUB_TOKEN env
// > existing environment file (so reinstalls keep working tokens) >
// generated; with --auth none the configured token is kept but never minted.
// The registration token follows the same flag > env > existing-file chain,
// except an explicitly-set flag wins verbatim so --registration-token ""
// disables self-registration on reinstall.
func resolveServeHubSystemdTokens(requireAuth bool, tokenFlag, tokenEnv, regTokenFlag string, regTokenSet bool, existing map[string]string) (string, bool, string, error) {
	var regToken string
	if regTokenSet {
		regToken = strings.TrimSpace(regTokenFlag)
	} else {
		regToken = resolveServeHubRegistrationToken(regTokenFlag)
		if regToken == "" {
			regToken = strings.TrimSpace(existing[hubRegistrationTokenEnv])
		}
	}
	envVal := strings.TrimSpace(tokenEnv)
	if envVal == "" {
		envVal = strings.TrimSpace(existing[serveHubTokenEnv])
	}
	if !requireAuth {
		if t := strings.TrimSpace(tokenFlag); t != "" {
			return t, false, regToken, nil
		}
		return envVal, false, regToken, nil
	}
	token, source, err := resolveServeToken(tokenFlag, envVal, true, generateServeToken)
	if err != nil {
		return "", false, "", err
	}
	return token, source == tokenSourceGenerated, regToken, nil
}

func serveHubSystemdEnvFileContent(token, regToken string) ([]byte, error) {
	var b strings.Builder
	b.WriteString("# Written by `term-llm serve hub systemd --install`.\n")
	for _, kv := range []struct{ key, value string }{
		{serveHubTokenEnv, token},
		{hubRegistrationTokenEnv, regToken},
	} {
		if kv.value == "" {
			continue
		}
		// The file is parsed by both systemd and contain.ReadEnvFile; keep
		// values to characters neither side needs quoting or escaping for.
		if strings.ContainsAny(kv.value, " \t\r\n\"'\\#") {
			return nil, fmt.Errorf("%s value must not contain whitespace, quotes, backslashes, or #", kv.key)
		}
		b.WriteString(kv.key + "=" + kv.value + "\n")
	}
	return []byte(b.String()), nil
}

func writeServeHubSystemdEnvFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create hub env file dir: %w", err)
	}
	// 0600: the file holds the Hub bearer and registration tokens. Chmod
	// first so an existing overly-permissive file is corrected before the
	// atomic rewrite preserves its mode, and new files still land at 0600.
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("secure hub env file permissions: %w", err)
	}
	if err := config.WriteFileAtomically(path, content, 0o600); err != nil {
		return fmt.Errorf("write hub env file: %w", err)
	}
	return nil
}

func runSystemctl(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w (output: %q)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// serveHubSystemdInstaller carries the paths and exec seams for --install so
// tests can drive it against a temp dir with a fake systemctl.
type serveHubSystemdInstaller struct {
	unitPath  string
	envPath   string
	goos      string
	settle    time.Duration
	systemctl func(ctx context.Context, args ...string) error
}

func (in serveHubSystemdInstaller) install(ctx context.Context, unit []byte, requireAuth bool, tokenFlag, regTokenFlag string, regTokenSet bool, out io.Writer) error {
	if in.goos != "linux" {
		return fmt.Errorf("--install requires systemd and is only supported on linux (running on %s); omit --install to print the unit instead", in.goos)
	}
	existing, err := contain.ReadEnvFile(in.envPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return serveHubSystemdSudoHint(fmt.Errorf("read %s: %w", in.envPath, err))
		}
		existing = nil
	}
	token, generated, regToken, err := resolveServeHubSystemdTokens(requireAuth, tokenFlag, os.Getenv(serveHubTokenEnv), regTokenFlag, regTokenSet, existing)
	if err != nil {
		return err
	}
	envContent, err := serveHubSystemdEnvFileContent(token, regToken)
	if err != nil {
		if len(existing) > 0 {
			return fmt.Errorf("%w (check the existing value in %s, or override with --token/--registration-token)", err, in.envPath)
		}
		return err
	}
	if err := writeServeHubSystemdEnvFile(in.envPath, envContent); err != nil {
		return serveHubSystemdSudoHint(err)
	}
	if err := os.MkdirAll(filepath.Dir(in.unitPath), 0o755); err != nil {
		return serveHubSystemdSudoHint(fmt.Errorf("create unit dir: %w", err))
	}
	if err := os.WriteFile(in.unitPath, unit, 0o644); err != nil {
		return serveHubSystemdSudoHint(fmt.Errorf("write %s: %w", in.unitPath, err))
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", serveHubSystemdUnitName}} {
		if err := in.systemctl(ctx, args...); err != nil {
			return serveHubSystemdSudoHint(err)
		}
	}
	// A crash-looping previous install can leave the unit start-rate-limited,
	// and manual restarts are subject to the same limit, so flush the counter
	// first. Best-effort: on a fresh install there is nothing to reset.
	_ = in.systemctl(ctx, "reset-failed", serveHubSystemdUnitName)
	if err := in.systemctl(ctx, "restart", serveHubSystemdUnitName); err != nil {
		return serveHubSystemdSudoHint(err)
	}
	// Type=exec makes restart fail on exec errors, but a service that starts
	// and exits moments later (bad config, port in use) still reports
	// success; give it a beat and confirm it is actually running.
	time.Sleep(in.settle)
	if err := in.systemctl(ctx, "is-active", "--quiet", serveHubSystemdUnitName); err != nil {
		return fmt.Errorf("%s was installed but is not running (check: journalctl -u %s -n 50): %w", serveHubSystemdUnitName, serveHubSystemdUnitName, err)
	}
	fmt.Fprintf(out, "Installed %s\n", in.unitPath)
	fmt.Fprintf(out, "  tokens: %s\n", in.envPath)
	fmt.Fprintf(out, "  service: enabled and started (%s)\n", serveHubSystemdUnitName)
	regStatus := "disabled"
	if regToken != "" {
		regStatus = "enabled"
	}
	fmt.Fprintf(out, "  registration: %s\n", regStatus)
	if generated {
		fmt.Fprintf(out, "  generated Hub bearer token: %s\n", token)
	}
	fmt.Fprintf(out, "  logs: journalctl -u %s -f\n", serveHubSystemdUnitName)
	return nil
}

// serveHubSystemdSudoHint appends a sudo hint to permission errors from the
// file writes under /etc. A denied systemctl exits non-zero (*exec.ExitError,
// never os.ErrPermission), so those failures surface with systemctl's own
// "Access denied" output instead.
func serveHubSystemdSudoHint(err error) error {
	if err != nil && errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w (try running with sudo)", err)
	}
	return err
}

func init() {
	serveHubCmd.AddCommand(serveHubSystemdCmd)
	addServeHubFlags(serveHubSystemdCmd)
	serveHubSystemdCmd.Flags().BoolVar(&serveHubSystemdInstall, "install", false, "Write the unit and token file and enable the service via systemctl (Linux, requires root)")
}
