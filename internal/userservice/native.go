package userservice

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
)

// Native is injectable so tests never alter the user's service manager.
type Native struct {
	OS, Home, ConfigHome string
	UID                  int
	Run                  func(context.Context, string, ...string) ([]byte, error)
}

func Command(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
func (n Native) Label(kind string) string {
	if n.OS == "darwin" {
		return "com.term-llm." + kind
	}
	return "term-llm-" + kind + ".service"
}
func (n Native) Path(kind string) string {
	if n.OS == "darwin" {
		return filepath.Join(n.Home, "Library", "LaunchAgents", n.Label(kind)+".plist")
	}
	return filepath.Join(n.ConfigHome, "systemd", "user", n.Label(kind))
}
func (n Native) domain() string { return "gui/" + strconv.Itoa(n.UID) }
func (n Native) Check(ctx context.Context) error {
	if n.UID == 0 {
		return fmt.Errorf("service supports per-user installs only; do not run with sudo")
	}
	var err error
	switch n.OS {
	case "linux":
		_, err = n.run(ctx, "systemctl", "--user", "show-environment")
	case "darwin":
		_, err = n.run(ctx, "launchctl", "print", n.domain())
	default:
		return fmt.Errorf("native services support Linux/systemd and macOS/launchd only")
	}
	if err != nil {
		return fmt.Errorf("user service manager unavailable (on Linux a systemd user session is required; on macOS log in graphically): %w", err)
	}
	return nil
}
func (n Native) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	f := n.Run
	if f == nil {
		f = Command
	}
	out, err := f(ctx, name, args...)
	if err != nil {
		if detail := strings.TrimSpace(string(out)); detail != "" {
			return out, fmt.Errorf("%s %s failed: %w\n%s", name, strings.Join(args, " "), err, detail)
		}
		return out, fmt.Errorf("%s %s failed: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, "$", "$$")
	return strconv.Quote(s)
}
func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (n Native) Render(s Spec, specPath string) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	args := []string{s.Binary, "service", "run", s.Kind, "--spec", specPath}
	keys := make([]string, 0, len(s.Environment))
	for key := range s.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var nativeEnv strings.Builder
	for _, key := range keys {
		if n.OS == "linux" {
			fmt.Fprintf(&nativeEnv, "Environment=%s\n", strconv.Quote(strings.ReplaceAll(key+"="+s.Environment[key], "%", "%%")))
		} else {
			fmt.Fprintf(&nativeEnv, "<key>%s</key><string>%s</string>", xmlText(key), xmlText(s.Environment[key]))
		}
	}
	if n.OS == "linux" {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = systemdQuote(a)
		}
		return []byte(fmt.Sprintf(`# %s
[Unit]
Description=term-llm %s
StartLimitIntervalSec=60
StartLimitBurst=3

[Service]
Type=exec
WorkingDirectory=%s
ExecStart=%s
%sRestart=on-failure
RestartSec=5
TimeoutStopSec=30
UMask=0077

[Install]
WantedBy=default.target
`, Marker, s.Kind, strings.ReplaceAll(s.Directory, "%", "%%"), strings.Join(quoted, " "), nativeEnv.String())), nil
	}
	if n.OS != "darwin" {
		return nil, fmt.Errorf("unsupported service platform %s", n.OS)
	}
	var argv strings.Builder
	for _, a := range args {
		fmt.Fprintf(&argv, "<string>%s</string>", xmlText(a))
	}
	log := filepath.Join(filepath.Dir(specPath), "service.log")
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- %s -->
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array>%s</array>
<key>WorkingDirectory</key><string>%s</string>
<key>EnvironmentVariables</key><dict>%s</dict>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ThrottleInterval</key><integer>5</integer>
<key>ExitTimeOut</key><integer>30</integer>
<key>Umask</key><integer>63</integer>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, Marker, n.Label(s.Kind), argv.String(), xmlText(s.Directory), nativeEnv.String(), xmlText(log), xmlText(log))), nil
}

// CheckOwned refuses to replace arbitrary existing native units, including the
// older hand-installed examples. Native identity alone is not ownership.
func (n Native) CheckOwned(kind, specPath string) error {
	path := n.Path(kind)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular native service %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	quoted := systemdQuote(specPath)
	if n.OS == "darwin" {
		quoted = xmlText(specPath)
	}
	if !bytes.Contains(data, []byte(Marker)) || !bytes.Contains(data, []byte(quoted)) {
		return fmt.Errorf("%s is not owned by this installer; remove or migrate it explicitly", path)
	}
	return nil
}
func (n Native) Install(s Spec, specPath string) error {
	if err := n.CheckOwned(s.Kind, specPath); err != nil {
		return err
	}
	data, err := n.Render(s, specPath)
	if err != nil {
		return err
	}
	path := n.Path(s.Kind)
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return config.WriteFileAtomicallyNoFollow(path, data, 0600)
}

// Start starts or restarts the process without replacing its native registration.
func (n Native) Start(ctx context.Context, kind string, restart bool) error {
	return n.start(ctx, kind, restart, false)
}

// Reconcile starts an installed service, reloading its native definition when changed.
func (n Native) Reconcile(ctx context.Context, kind string, changed bool) error {
	return n.start(ctx, kind, changed, changed)
}

func (n Native) start(ctx context.Context, kind string, restart, reload bool) error {
	if n.OS == "linux" {
		if _, err := n.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		_, _ = n.run(ctx, "systemctl", "--user", "reset-failed", n.Label(kind))
		if _, err := n.run(ctx, "systemctl", "--user", "enable", n.Path(kind)); err != nil {
			return err
		}
		action := "start"
		if restart {
			action = "restart"
		}
		_, err := n.run(ctx, "systemctl", "--user", action, n.Label(kind))
		return err
	}
	target := n.domain() + "/" + n.Label(kind)
	if _, err := n.run(ctx, "launchctl", "enable", target); err != nil {
		return err
	}
	_, loaded := n.run(ctx, "launchctl", "print", target)
	if loaded == nil && reload {
		if _, err := n.run(ctx, "launchctl", "bootout", target); err != nil {
			return err
		}
		loaded = fmt.Errorf("reload")
	}
	if loaded != nil {
		_, err := n.run(ctx, "launchctl", "bootstrap", n.domain(), n.Path(kind))
		return err
	}
	// Let launchd replace the process in-place. Bootout followed immediately by
	// bootstrap can race with teardown and leave an ordinary restart unloaded.
	args := []string{"kickstart"}
	if restart {
		args = append(args, "-k")
	}
	_, err := n.run(ctx, "launchctl", append(args, target)...)
	return err
}
func (n Native) Stop(ctx context.Context, kind string) error {
	if n.OS == "linux" {
		_, err := n.run(ctx, "systemctl", "--user", "disable", "--now", n.Label(kind))
		return err
	}
	target := n.domain() + "/" + n.Label(kind)
	if _, err := n.run(ctx, "launchctl", "disable", target); err != nil {
		return err
	}
	if _, err := n.run(ctx, "launchctl", "print", target); err != nil {
		return nil
	}
	_, err := n.run(ctx, "launchctl", "bootout", target)
	return err
}
func (n Native) Remove(ctx context.Context, kind, specPath string) error {
	if err := n.CheckOwned(kind, specPath); err != nil {
		return err
	}
	if err := n.Stop(ctx, kind); err != nil {
		return err
	}
	if err := os.Remove(n.Path(kind)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if n.OS == "linux" {
		_, err := n.run(ctx, "systemctl", "--user", "daemon-reload")
		return err
	}
	return nil
}
func (n Native) Status(ctx context.Context, kind string) (string, error) {
	if n.OS == "linux" {
		out, err := n.run(ctx, "systemctl", "--user", "show", n.Label(kind), "--property=ActiveState,SubState,UnitFileState,MainPID", "--no-pager")
		return string(out), err
	}
	out, err := n.run(ctx, "launchctl", "print", n.domain()+"/"+n.Label(kind))
	return string(out), err
}
func (n Native) Logs(ctx context.Context, kind, specPath string, follow bool, out, errOut io.Writer) error {
	var cmd *exec.Cmd
	if n.OS == "linux" {
		args := []string{"--user", "-u", n.Label(kind), "-n", "60", "--no-pager"}
		if follow {
			args = append(args, "-f")
		}
		cmd = exec.CommandContext(ctx, "journalctl", args...)
	} else {
		args := []string{"-n", "60"}
		if follow {
			args = append(args, "-F")
		}
		args = append(args, filepath.Join(filepath.Dir(specPath), "service.log"))
		cmd = exec.CommandContext(ctx, "/usr/bin/tail", args...)
	}
	cmd.Stdout = out
	cmd.Stderr = errOut
	return cmd.Run()
}

func (n Native) Enabled(ctx context.Context, kind string) (bool, error) {
	if n.OS == "linux" {
		out, err := n.run(ctx, "systemctl", "--user", "is-enabled", n.Label(kind))
		if strings.TrimSpace(string(out)) == "disabled" {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return strings.TrimSpace(string(out)) == "enabled", nil
	}
	out, err := n.run(ctx, "launchctl", "print-disabled", n.domain())
	if err != nil {
		return false, err
	}
	return !strings.Contains(string(out), `"`+n.Label(kind)+`" => true`), nil
}

// CheckLoaded also checks the supervisor's actual source, which may be outside
// this shell's XDG_CONFIG_HOME or overridden in a runtime unit directory.
func (n Native) CheckLoaded(ctx context.Context, kind, specPath string) error {
	var source string
	if n.OS == "linux" {
		out, err := n.run(ctx, "systemctl", "--user", "show", "--property=FragmentPath", "--value", n.Label(kind))
		if err != nil {
			return err
		}
		source = strings.TrimSpace(string(out))
	} else {
		out, err := n.run(ctx, "launchctl", "print", n.domain()+"/"+n.Label(kind))
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(out), "\n") {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "path = "); ok {
				source = value
				break
			}
		}
		if source == "" {
			return fmt.Errorf("existing launchd job %s has no verifiable managed definition", n.Label(kind))
		}
	}
	if source == "" {
		return nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("inspect loaded service definition: %w", err)
	}
	quoted := systemdQuote(specPath)
	if n.OS == "darwin" {
		quoted = xmlText(specPath)
	}
	if !bytes.Contains(data, []byte(Marker)) || !bytes.Contains(data, []byte(quoted)) {
		return fmt.Errorf("existing %s is loaded from an unmanaged definition at %s; it will not be replaced", n.Label(kind), source)
	}
	return nil
}

func (n Native) Running(ctx context.Context, kind string) bool {
	status, err := n.Status(ctx, kind)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(status, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) == "MainPID" || strings.TrimSpace(key) == "pid" {
			pid, err := strconv.Atoi(strings.TrimSpace(value))
			return err == nil && pid > 0
		}
	}
	return false
}
