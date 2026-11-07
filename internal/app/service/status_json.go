package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/userservice"
	"github.com/spf13/cobra"
)

// This is an explicit allowlist, not a serialization of Spec or native output.
// In particular, arguments, environment, credential names/values and auth-file
// contents must never become part of the status contract.
type serviceStatusReport struct {
	SchemaVersion int                  `json:"schema_version"`
	GeneratedAt   time.Time            `json:"generated_at"`
	UID           int                  `json:"uid"`
	Platform      string               `json:"platform"`
	Services      []serviceStatusEntry `json:"services"`
}

type serviceStatusEntry struct {
	Kind         string `json:"kind"`
	Installed    bool   `json:"installed"`
	Managed      bool   `json:"managed"`
	Registered   bool   `json:"registered"`
	State        string `json:"state"`
	Running      bool   `json:"running"`
	PID          int    `json:"pid"`
	Autostart    bool   `json:"autostart"`
	Binary       string `json:"binary"`
	URL          string `json:"url"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	BasePath     string `json:"base_path"`
	Auth         string `json:"auth"`
	Enrollment   string `json:"enrollment"`
	Healthy      bool   `json:"healthy"`
	HealthError  string `json:"health_error"`
	SpecPath     string `json:"spec_path"`
	NativePath   string `json:"native_path"`
	LastExitCode *int   `json:"last_exit_code,omitempty"`
	Error        string `json:"error"`
}

// StatusJSON emits one versioned object, including service errors as data.
// Unlike mutating management, inspection is also safe as root or on platforms
// without a native manager: these conditions are reported, not fatal exits.
func (a App) StatusJSON(cmd *cobra.Command, args []string) error {
	if len(args) > 1 || (len(args) == 1 && !userservice.ValidKind(args[0])) {
		return fmt.Errorf("use status [web|hub] --json")
	}
	e, err := statusServiceEnvironment()
	kinds := []string{"web", "hub"}
	if len(args) == 1 {
		kinds = args
	}
	report := buildServiceStatus(cmd.Context(), e, kinds, time.Now().UTC(), err)
	return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
}

func statusServiceEnvironment() (serviceEnvironment, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return serviceEnvironment{}, err
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(cfg) {
		return serviceEnvironment{}, fmt.Errorf("XDG_CONFIG_HOME must be absolute")
	}
	return serviceEnvironment{root: filepath.Join(cfg, "term-llm", "services"), home: home, native: userservice.Native{OS: runtime.GOOS, Home: home, ConfigHome: cfg, UID: os.Getuid()}}, nil
}

func buildServiceStatus(ctx context.Context, e serviceEnvironment, kinds []string, now time.Time, envErr error) serviceStatusReport {
	report := serviceStatusReport{SchemaVersion: 1, GeneratedAt: now.UTC(), UID: os.Getuid(), Platform: runtime.GOOS, Services: make([]serviceStatusEntry, 0, len(kinds))}
	if envErr == nil {
		report.UID, report.Platform = e.native.UID, e.native.OS
	}
	for _, kind := range kinds {
		if envErr != nil {
			report.Services = append(report.Services, serviceStatusEntry{Kind: kind, State: "error", Enrollment: "not_required", Error: "service environment unavailable; check HOME and XDG_CONFIG_HOME"})
			continue
		}
		report.Services = append(report.Services, inspectServiceStatus(ctx, e, kind))
	}
	return report
}

func inspectServiceStatus(ctx context.Context, e serviceEnvironment, kind string) serviceStatusEntry {
	entry := serviceStatusEntry{Kind: kind, State: "not_installed", Enrollment: "not_required", SpecPath: e.specPath(kind), NativePath: e.native.Path(kind)}
	_, nativeErr := os.Lstat(entry.NativePath)
	entry.Registered = nativeErr == nil
	if nativeErr != nil && !os.IsNotExist(nativeErr) {
		return entry.failure("cannot inspect native service definition")
	}
	spec, err := userservice.Load(entry.SpecPath)
	if err != nil {
		if entry.Registered {
			entry.State = "unmanaged"
			return entry
		}
		if !os.IsNotExist(err) {
			return entry.failure("cannot load managed service specification")
		}
		return entry
	}
	entry.Installed = true
	if spec.Kind != kind {
		return entry.failure("service specification kind mismatch")
	}
	if err := e.native.CheckOwned(kind, entry.SpecPath); err != nil {
		entry.State = "unmanaged"
		return entry
	}
	entry.Managed = true
	entry.Binary, entry.URL, entry.Host = spec.Binary, publicStatusURL(spec.URL), spec.Host
	entry.Port, entry.BasePath, entry.Auth = spec.Port, spec.BasePath, spec.Auth
	if spec.Auth == "passkey" {
		entry.Enrollment = "" // Unknown if the credential store cannot be inspected.
		count, err := serviceCredentialCount(spec)
		if err != nil {
			return entry.failure("cannot inspect passkey enrollment")
		}
		entry.Enrollment = "pending"
		if count > 0 {
			entry.Enrollment = "enrolled"
		}
	}
	if !entry.Registered {
		entry.State = "not_registered"
		return entry
	}
	return inspectServiceProcess(ctx, e.native, spec, entry)
}

func (s serviceStatusEntry) failure(message string) serviceStatusEntry {
	s.State, s.Error = "error", message
	return s
}

func inspectServiceProcess(ctx context.Context, native userservice.Native, spec userservice.Spec, entry serviceStatusEntry) serviceStatusEntry {
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := native.Check(queryCtx); err != nil {
		return entry.failure("user service manager unavailable")
	}
	// Errors from native.run may include a full launchctl environment dump.
	// Never expose their text, even when a command failed.
	enabled, enabledErr := native.Enabled(queryCtx, entry.Kind)
	entry.Autostart = enabledErr == nil && enabled
	status, err := native.Status(queryCtx, entry.Kind)
	if err != nil {
		// A booted-out LaunchAgent still has a registered plist. Missing jobs
		// are stopped, not manager failures (the domain was checked above).
		if native.OS == "darwin" && (strings.Contains(status, "Could not find service") || strings.Contains(status, "Could not find specified service")) {
			entry.State = "stopped"
			return entry
		}
		return entry.failure("native service status query failed")
	}
	entry.PID, entry.LastExitCode = serviceProcessFields(status)
	entry.Running = entry.PID > 0
	entry.State = "stopped"
	if !entry.Running {
		return entry
	}
	entry.Healthy, entry.HealthError = serviceHealth(ctx, spec)
	entry.State = "unhealthy"
	if entry.Healthy {
		entry.State = "ready"
	}
	return entry
}

func serviceProcessFields(status string) (pid int, exitCode *int) {
	for _, line := range strings.Split(status, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		number, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "MainPID", "pid":
			if number > 0 {
				pid = number
			}
		case "ExecMainStatus", "last exit code":
			exitCode = &number
		}
	}
	return
}

func publicStatusURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	// Public browser metadata is useful; URL credentials and query capabilities
	// are not. Managed installers normally produce neither.
	parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
	parsed.ForceQuery = false
	return parsed.String()
}

func serviceHealth(ctx context.Context, spec userservice.Spec) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	// Keep the same local bind/mount construction as waitUserService. Never
	// probe the public URL, attach credentials, or follow a redirect.
	target := "http://" + net.JoinHostPort(spec.Host, strconv.Itoa(spec.Port)) + spec.BasePath + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, "invalid local health endpoint"
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, "local health request failed or timed out"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("health endpoint returned HTTP %d", resp.StatusCode)
	}
	return true, ""
}
