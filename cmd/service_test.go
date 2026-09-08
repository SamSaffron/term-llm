package cmd

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/userservice"
	"github.com/spf13/cobra"
)

func TestServiceLaunchParsing(t *testing.T) {
	originalHost, originalPort, originalCORS := serveHost, servePort, append([]string(nil), serveCORSOrigins...)
	for _, tc := range []struct {
		name, kind string
		args       []string
		auth, url  string
		bad        bool
	}{
		{name: "web default", kind: "web", auth: "passkey", url: "http://localhost:8080/ui/"},
		{name: "hub default", kind: "hub", auth: "passkey", url: "http://localhost:8090/hub/"},
		{name: "custom port", kind: "web", args: []string{"--auth", "bearer", "--port", "8181"}, auth: "bearer", url: "http://localhost:8181/ui/"},
		{name: "public Hub", kind: "hub", args: []string{"--public-url", "https://hub.example.com/hub/"}, auth: "passkey", url: "https://hub.example.com/hub/"},
		{name: "reverse bearer", kind: "web", args: []string{"--auth", "bearer", "--hub-connect", "reverse", "--hub-register", "--hub-url", "https://hub.example.com/hub/", "--hub-node-id", "laptop"}, auth: "bearer", url: "http://localhost:8080/ui/"},
		{name: "existing no-auth alias", kind: "web", args: []string{"--no-auth"}, auth: "none", url: "http://localhost:8080/ui/"},
		{name: "unsafe secret argv", kind: "web", args: []string{"--token", "do-not-print"}, bad: true},
		{name: "unsafe Hub secret argv", kind: "hub", args: []string{"--registration-token", "do-not-print"}, bad: true},
		{name: "passkey reverse rejected", kind: "web", args: []string{"--hub-connect", " Reverse "}, bad: true},
		{name: "unknown native server flag", kind: "hub", args: []string{"--made-up"}, bad: true},
		{name: "additional platform", kind: "web", args: []string{"jobs"}, bad: true},
		{name: "public unauthenticated", kind: "web", args: []string{"--no-auth", "--host", "0.0.0.0"}, bad: true},
		{name: "mismatched passkey paths", kind: "web", args: []string{"--public-url", "http://localhost:8080/chat/", "--base-path", "/ui"}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := parseServiceLaunch(tc.kind, tc.args)
			if tc.bad {
				if err == nil {
					t.Fatalf("accepted %+v", s)
				}
				if strings.Contains(err.Error(), "do-not-print") {
					t.Fatal("error leaked secret")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.Auth != tc.auth || s.URL != tc.url {
				t.Fatalf("got %+v", s)
			}
			again, err := parseServiceLaunch(tc.kind, s.Args)
			if err != nil || again.URL != s.URL || again.Auth != s.Auth {
				t.Fatalf("not idempotent %+v %v", again, err)
			}
		})
	}
	if serveHost != originalHost || servePort != originalPort || !slices.Equal(serveCORSOrigins, originalCORS) {
		t.Fatal("installer changed serve globals")
	}
}
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
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := installUserService(cmd, "web", nil, serviceInstallOptions{dryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "config")); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote configuration")
	}
	if !strings.Contains(out.String(), "ExecStart=") {
		t.Fatal(out.String())
	}
	opts := serviceInstallOptions{noStart: true, yes: true, custom: true}
	if err := installUserService(cmd, "web", []string{"--auth", "bearer", "--port", "18181"}, opts); err != nil {
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
	opts.custom = false
	if err := installUserService(cmd, "web", nil, opts); err != nil {
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
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	for _, kind := range []string{"web", "hub"} {
		if err := installUserService(cmd, kind, nil, serviceInstallOptions{noStart: true}); err != nil {
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
