package service

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/userservice"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func isolatedServiceHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	return home
}
func TestServiceInstallDryRunAndReinstall(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getuid() == 0 {
		t.Skip("Linux user-unit fixture")
	}
	home := isolatedServiceHome(t)
	a := testApp()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := a.installUserService(cmd, "web", nil, InstallOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "config")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote configuration")
	}
	if !strings.Contains(out.String(), "ExecStart=") {
		t.Fatal(out.String())
	}
	opts := InstallOptions{NoStart: true, Yes: true, Custom: true}
	if err := a.installUserService(cmd, "web", []string{"--auth", "bearer", "--port", "18181"}, opts); err != nil {
		t.Fatal(err)
	}
	e, err := newServiceEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	first, err := userservice.Load(e.specPath("web"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := e.credentials("web").Get(cmd.Context(), serviceTokenName("web"))
	if err != nil {
		t.Fatal(err)
	}
	opts.Custom = false
	if err := a.installUserService(cmd, "web", nil, opts); err != nil {
		t.Fatal(err)
	}
	second, err := userservice.Load(e.specPath("web"))
	if err != nil {
		t.Fatal(err)
	}
	nextToken, err := e.credentials("web").Get(cmd.Context(), serviceTokenName("web"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Port != second.Port || second.Auth != "bearer" || token != nextToken {
		t.Fatal("reinstall reset custom setup or rotated token")
	}
	if strings.Contains(out.String(), token) {
		t.Fatal("installation output leaked credential")
	}
	native, _ := os.ReadFile(e.native.Path("web"))
	spec, _ := os.ReadFile(e.specPath("web"))
	if bytes.Contains(native, []byte(token)) || bytes.Contains(spec, []byte(token)) {
		t.Fatal("secret in native unit or specification")
	}
}
func TestServiceKindsAreIndependent(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getuid() == 0 {
		t.Skip("Linux user-unit fixture")
	}
	isolatedServiceHome(t)
	a := testApp()
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	for _, kind := range []string{"web", "hub"} {
		if err := a.installUserService(cmd, kind, nil, InstallOptions{NoStart: true}); err != nil {
			t.Fatal(err)
		}
	}
	e, _ := newServiceEnvironment()
	web, _ := userservice.Load(e.specPath("web"))
	hub, _ := userservice.Load(e.specPath("hub"))
	if web.Port == hub.Port || web.AuthFile == hub.AuthFile {
		t.Fatal("service identities overlap")
	}
	if _, err := e.selectKind(nil); err == nil {
		t.Fatal("ambiguous service selection")
	}
}

func TestServiceReadinessRequiresNativeProcess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer server.Close()
	host, portText, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	spec := userservice.Spec{Kind: "hub", Host: host, Port: port, BasePath: "/hub"}
	if err := checkUserServicePort(spec); err == nil {
		t.Fatal("accepted an occupied port")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	native := userservice.Native{OS: "linux", Run: func(context.Context, string, ...string) ([]byte, error) { cancel(); return []byte("MainPID=0\n"), nil }}
	if err := waitUserService(ctx, spec, native); err == nil {
		t.Fatal("foreign HTTP responder was mistaken for a running managed service")
	}
	native.Run = func(context.Context, string, ...string) ([]byte, error) { return []byte("MainPID=123\n"), nil }
	if err := waitUserService(context.Background(), spec, native); err != nil {
		t.Fatal(err)
	}
}

func TestServiceSelection(t *testing.T) {
	for _, kind := range []string{"web", "hub"} {
		t.Run(kind, func(t *testing.T) {
			e := serviceEnvironment{native: userservice.Native{OS: "darwin", Home: t.TempDir()}}
			path := e.native.Path(kind)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{nil, {kind}} {
				got, err := e.selectKind(args)
				if err != nil || got != kind {
					t.Fatalf("selectKind(%v) = %q, %v", args, got, err)
				}
			}
		})
	}
}

func TestServiceInstallReloadsReplacedDefinition(t *testing.T) {
	spec := userservice.Spec{Kind: "web", Port: 8080}
	for _, tc := range []struct {
		name     string
		oldErr   error
		replaced bool
		want     bool
	}{
		{"fresh install", os.ErrNotExist, false, false},
		{"unchanged reinstall", nil, false, false},
		// Upgrades can change the rendered launchd/systemd template (for example
		// ProcessType) without changing the saved spec; the loaded job must reload.
		{"template changed", nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceInstallNeedsRestart(tc.oldErr, spec, spec, nil, "", tc.replaced); got != tc.want {
				t.Fatalf("serviceInstallNeedsRestart = %v, want %v", got, tc.want)
			}
		})
	}
}

// testApp builds an App over a small stand-in for serve's flags so install
// flows can run without importing cmd.
func testApp() App {
	flags := func(kind string) []*pflag.FlagSet {
		fs := pflag.NewFlagSet(kind, pflag.ContinueOnError)
		port := 8080
		if kind == "hub" {
			port = 8090
		}
		fs.String("auth", "", "")
		fs.Bool("no-auth", false, "")
		fs.Bool("allow-no-auth", false, "")
		fs.String("host", "localhost", "")
		fs.Int("port", port, "")
		fs.String("base-path", "", "")
		fs.String("public-url", "", "")
		fs.String("hub-connect", "direct", "")
		fs.Bool("hub-register", false, "")
		fs.String("hub-url", "", "")
		fs.String("hub-node-id", "", "")
		fs.String("passkey-auth-file", "", "")
		fs.StringArray("cors-origin", nil, "")
		return []*pflag.FlagSet{fs}
	}
	return App{Hooks{
		LaunchFlags:                 flags,
		ResolveWebAuthMode:          func(_ bool, mode string, _ bool, _ bool) (string, error) { return mode, nil },
		ResolveHubAuthMode:          func(mode string) (string, error) { return mode, nil },
		ValidateBind:                func(string, int, bool) error { return nil },
		IsLoopbackHost:              func(string) bool { return true },
		ValidateWebPasskeyTransport: func(string, bool, string, []string) error { return nil },
		NormalizeBasePath:           func(p string) (string, error) { return p, nil },
		NormalizeHubBasePath:        func(p string) (string, error) { return p, nil },
		GenerateToken:               func() (string, error) { return "generated-token", nil },
		OpenBrowser:                 func(string) error { return nil },
		ReplaceProcess:              func(string, []string, []string) error { return nil },
		OutputIsTerminal:            func(any) bool { return false },
		RegistrationTokenEnv:        "TERM_LLM_HUB_REGISTRATION_TOKEN",
	}}
}

func TestServiceReinstallKeepsRecordedBinary(t *testing.T) {
	home := isolatedServiceHome(t)
	recorded := filepath.Join(home, "stable", "term-llm")
	if err := os.MkdirAll(filepath.Dir(recorded), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recorded, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e := serviceEnvironment{root: filepath.Join(home, "config", "term-llm", "services"), home: home}
	old := &userservice.Spec{Kind: "web", Binary: recorded, Directory: home}
	a := testApp()

	spec, err := a.buildServiceInstallSpec(e, "web", nil, InstallOptions{}, old)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Binary != recorded {
		t.Fatalf("bare reinstall replaced recorded binary %s with %s", recorded, spec.Binary)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec, err = a.buildServiceInstallSpec(e, "web", nil, InstallOptions{Binary: self}, old)
	if err != nil || spec.Binary != self {
		t.Fatalf("explicit --binary = (%q, %v), want %q", spec.Binary, err, self)
	}

	spec, err = a.buildServiceInstallSpec(e, "web", nil, InstallOptions{}, nil)
	if err != nil || spec.Binary != self {
		t.Fatalf("first install binary = (%q, %v), want running executable %q", spec.Binary, err, self)
	}

	if err := os.Remove(recorded); err != nil {
		t.Fatal(err)
	}
	if _, err = a.buildServiceInstallSpec(e, "web", nil, InstallOptions{}, old); err == nil || !strings.Contains(err.Error(), "--binary") {
		t.Fatalf("missing recorded binary must ask for --binary: %v", err)
	}
}
