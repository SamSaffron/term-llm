package service

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/samsaffron/term-llm/internal/passkeyauth"
	"github.com/samsaffron/term-llm/internal/userservice"
	"github.com/spf13/cobra"
)

const statusTestSecret = "must-not-appear-in-json"

func statusFixture(t *testing.T, platform string) (serviceEnvironment, userservice.Spec) {
	t.Helper()
	home := isolatedServiceHome(t)
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	e := serviceEnvironment{home: home, root: filepath.Join(home, "config", "term-llm", "services"), native: userservice.Native{OS: platform, Home: home, ConfigHome: filepath.Join(home, "config"), UID: 501}}
	e.native.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case joined == "--user show-environment", joined == "print gui/501":
			return nil, nil
		case strings.Contains(joined, "is-enabled"):
			return []byte("enabled\n"), nil
		case strings.HasPrefix(joined, "print-disabled"):
			return []byte(`"com.term-llm.web" => false`), nil
		case strings.HasPrefix(joined, "--user show"):
			return []byte("MainPID=0\nExecMainStatus=0\nEnvironment=" + statusTestSecret), nil
		case strings.HasPrefix(joined, "print gui/501/com.term-llm."):
			return []byte("state = not running\nlast exit code = 0\nenvironment = { TOKEN = " + statusTestSecret + " }"), nil
		default:
			t.Fatalf("unexpected manager command: %s %v", name, args)
			return nil, nil
		}
	}
	spec := userservice.Spec{Version: 1, Kind: "web", Binary: filepath.Join(home, "bin", "term-llm"), Directory: home, Auth: "bearer", Host: "127.0.0.1", Port: 8765, BasePath: "/ui", URL: "http://localhost:8765/ui/", Environment: map[string]string{"PATH": statusTestSecret}, Secrets: []string{"OPENAI_API_KEY"}, AuthFile: filepath.Join(home, "passkeys.json")}
	return e, spec
}

func saveStatusFixture(t *testing.T, e serviceEnvironment, spec userservice.Spec, native bool) {
	t.Helper()
	if err := userservice.Save(e.specPath(spec.Kind), spec); err != nil {
		t.Fatal(err)
	}
	if native {
		if _, err := e.native.Install(spec, e.specPath(spec.Kind)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServiceStatusJSONStates(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		for _, name := range []string{"not installed", "unmanaged", "not registered", "stopped", "running healthy", "running unhealthy", "manager failure", "status failure", "autostart unknown", "autostart disabled", "pending", "enrolled", "bad spec", "unowned definition"} {
			t.Run(platform+"/"+name, func(t *testing.T) {
				e, spec := statusFixture(t, platform)
				want, installed, managed, registered := "stopped", true, true, true
				switch name {
				case "not installed":
					want, installed, managed, registered = "not_installed", false, false, false
				case "unmanaged":
					want, installed, managed = "unmanaged", false, false
					if err := os.MkdirAll(filepath.Dir(e.native.Path("web")), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(e.native.Path("web"), []byte(statusTestSecret), 0600); err != nil {
						t.Fatal(err)
					}
				case "not registered":
					want, registered = "not_registered", false
				case "running healthy", "running unhealthy":
					want = "ready"
					if name == "running unhealthy" {
						want = "unhealthy"
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/ui/healthz" || r.Header.Get("Authorization") != "" {
							t.Errorf("unexpected health request: %s", r.URL.Path)
						}
						if name == "running unhealthy" {
							w.WriteHeader(http.StatusServiceUnavailable)
							_, _ = w.Write([]byte(statusTestSecret))
							return
						}
						w.WriteHeader(http.StatusOK)
					}))
					defer server.Close()
					spec.Host, _, _ = net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
					_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
					spec.Port, _ = strconv.Atoi(port)
					oldRun := e.native.Run
					e.native.Run = func(ctx context.Context, command string, args ...string) ([]byte, error) {
						if strings.Contains(strings.Join(args, " "), "--property=ActiveState") {
							return []byte("MainPID=1234\nExecMainStatus=0\nEnvironment=" + statusTestSecret), nil
						}
						if strings.HasPrefix(strings.Join(args, " "), "print gui/501/com.term-llm.") {
							return []byte("pid = 1234\nlast exit code = 0\nenvironment = { TOKEN = " + statusTestSecret + " }"), nil
						}
						return oldRun(ctx, command, args...)
					}
				case "manager failure":
					want = "error"
					e.native.Run = func(context.Context, string, ...string) ([]byte, error) {
						return []byte(statusTestSecret), errors.New(statusTestSecret)
					}
				case "status failure":
					want = "error"
					oldRun := e.native.Run
					e.native.Run = func(ctx context.Context, command string, args ...string) ([]byte, error) {
						joined := strings.Join(args, " ")
						if strings.Contains(joined, "--property=ActiveState") || strings.HasPrefix(joined, "print gui/501/com.term-llm.") {
							return []byte(statusTestSecret), errors.New(statusTestSecret)
						}
						return oldRun(ctx, command, args...)
					}
				case "autostart unknown":
					oldRun := e.native.Run
					e.native.Run = func(ctx context.Context, command string, args ...string) ([]byte, error) {
						if strings.Contains(strings.Join(args, " "), "is-enabled") || args[0] == "print-disabled" {
							return []byte(statusTestSecret), errors.New(statusTestSecret)
						}
						return oldRun(ctx, command, args...)
					}
				case "autostart disabled":
					oldRun := e.native.Run
					e.native.Run = func(ctx context.Context, command string, args ...string) ([]byte, error) {
						if strings.Contains(strings.Join(args, " "), "is-enabled") {
							return []byte("disabled"), errors.New("exit status 1")
						}
						if args[0] == "print-disabled" {
							return []byte(`"com.term-llm.web" => true`), nil
						}
						return oldRun(ctx, command, args...)
					}
				case "pending", "enrolled":
					spec.Auth = "passkey"
					if name == "enrolled" {
						enrollStatusCredential(t, spec.AuthFile)
					}
				case "bad spec":
					want, installed, managed, registered = "error", false, false, false
				case "unowned definition":
					want, managed = "unmanaged", false
				}
				if installed {
					saveStatusFixture(t, e, spec, registered)
				}
				if name == "bad spec" {
					if err := userservice.WritePrivate(e.specPath("web"), []byte(`{"invalid":"`+statusTestSecret+`"}`)); err != nil {
						t.Fatal(err)
					}
				}
				if name == "unowned definition" {
					if err := os.WriteFile(e.native.Path("web"), []byte(statusTestSecret), 0600); err != nil {
						t.Fatal(err)
					}
				}
				now := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
				report := buildServiceStatus(context.Background(), e, []string{"web"}, now, nil)
				entry := report.Services[0]
				if entry.State != want || entry.Installed != installed || entry.Managed != managed || entry.Registered != registered {
					t.Fatalf("got %+v; want %s installed=%v managed=%v registered=%v", entry, want, installed, managed, registered)
				}
				running := strings.HasPrefix(name, "running ")
				if entry.Running != running || entry.Healthy != (want == "ready") {
					t.Fatalf("incorrect readiness: %+v", entry)
				}
				if running && (entry.PID != 1234 || entry.LastExitCode == nil || *entry.LastExitCode != 0) {
					t.Fatalf("incorrect native fields: %+v", entry)
				}
				if want == "unhealthy" && entry.HealthError == "" {
					t.Fatal("missing health error")
				}
				if want == "error" && entry.Error == "" {
					t.Fatal("missing status error")
				}
				if (name == "autostart unknown" || name == "autostart disabled") && entry.Autostart {
					t.Fatal("unknown autostart must be false")
				}
				if name == "stopped" && !entry.Autostart {
					t.Fatal("enabled autostart was lost")
				}
				enrollment := "not_required"
				if name == "pending" {
					enrollment = "pending"
				}
				if name == "enrolled" {
					enrollment = "enrolled"
				}
				if entry.Enrollment != enrollment {
					t.Fatalf("enrollment=%q want %q", entry.Enrollment, enrollment)
				}
				data, err := json.Marshal(report)
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{statusTestSecret, "OPENAI_API_KEY", "environment", "auth_file"} {
					if bytes.Contains(data, []byte(secret)) {
						t.Fatalf("status leaked %q", secret)
					}
				}
				if report.SchemaVersion != 1 || report.UID != 501 || report.Platform != platform || !report.GeneratedAt.Equal(now) {
					t.Fatalf("incorrect envelope: %+v", report)
				}
				var decoded map[string]any
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}
				fields := decoded["services"].([]any)[0].(map[string]any)
				for _, key := range []string{"installed", "managed", "registered", "running", "autostart", "healthy", "health_error", "error", "binary", "url", "host", "port", "base_path", "auth", "enrollment", "pid", "spec_path", "native_path"} {
					if _, ok := fields[key]; !ok {
						t.Fatalf("missing stable field %s", key)
					}
				}
			})
		}
	}
}

func enrollStatusCredential(t *testing.T, path string) {
	t.Helper()
	store, err := passkeyauth.OpenStore(passkeyauth.StoreOptions{Path: path, RPID: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: key.X.FillBytes(make([]byte, 32)), -3: key.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CommitFirstCredential(webauthn.Credential{ID: []byte(statusTestSecret), PublicKey: publicKey, Flags: webauthn.CredentialFlags{UserPresent: true, UserVerified: true}}, statusTestSecret)
	if err != nil {
		t.Fatal(err)
	}
}

func TestServiceStatusJSONCommand(t *testing.T) {
	isolatedServiceHome(t)
	for _, args := range [][]string{nil, {"web"}, {"hub"}} {
		cmd := &cobra.Command{}
		var output bytes.Buffer
		cmd.SetOut(&output)
		if err := (App{}).StatusJSON(cmd, args); err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(&output)
		var report serviceStatusReport
		if err := dec.Decode(&report); err != nil {
			t.Fatal(err)
		}
		want := []string{"web", "hub"}
		if len(args) == 1 {
			want = args
		}
		if len(report.Services) != len(want) {
			t.Fatalf("wrong services: %+v", report)
		}
		for i, kind := range want {
			if report.Services[i].Kind != kind || report.Services[i].State != "not_installed" {
				t.Fatalf("wrong service: %+v", report.Services[i])
			}
		}
		if output.Len() != 0 {
			t.Fatalf("extra output: %s", output.String())
		}
	}
	for _, args := range [][]string{{"bad"}, {"web", "hub"}} {
		if err := (App{}).StatusJSON(&cobra.Command{}, args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	// Environment failure is data too, not an operational exit failure.
	t.Setenv("XDG_CONFIG_HOME", "relative")
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := (App{}).StatusJSON(cmd, nil); err != nil {
		t.Fatal(err)
	}
	var report serviceStatusReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Services) != 2 || report.Services[0].State != "error" {
		t.Fatalf("%s", output.String())
	}
}

func TestServiceStatusJSONURLRedaction(t *testing.T) {
	got := publicStatusURL("https://user:" + statusTestSecret + "@localhost:8765/ui/?token=" + statusTestSecret + "#" + statusTestSecret)
	if got != "https://localhost:8765/ui/" {
		t.Fatalf("unsafe URL: %s", got)
	}
}

func TestServiceStatusDarwinUnloaded(t *testing.T) {
	e, spec := statusFixture(t, "darwin")
	saveStatusFixture(t, e, spec, true)
	oldRun := e.native.Run
	e.native.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if strings.HasPrefix(strings.Join(args, " "), "print gui/501/com.term-llm.") {
			return []byte(`Could not find service "com.term-llm.web" in domain for user gui: 501`), errors.New("missing")
		}
		return oldRun(ctx, name, args...)
	}
	got := inspectServiceStatus(context.Background(), e, "web")
	if got.State != "stopped" || !got.Registered || !got.Autostart {
		t.Fatalf("%+v", got)
	}
}

func TestServiceHealthDoesNotFollowRedirectOrProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			http.Redirect(w, r, "/secret", http.StatusFound)
			return
		}
		t.Error("followed health redirect")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	number, _ := strconv.Atoi(port)
	healthy, message := serviceHealth(context.Background(), userservice.Spec{Host: host, Port: number})
	if healthy || message != "health endpoint returned HTTP 302" {
		t.Fatalf("healthy=%v error=%q", healthy, message)
	}
}

func TestServiceStatusJSONSerializationExample(t *testing.T) {
	exitCode := 0
	report := serviceStatusReport{
		SchemaVersion: 1, GeneratedAt: time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC), UID: 501, Platform: "darwin",
		Services: []serviceStatusEntry{{
			Kind: "web", Installed: true, Managed: true, Registered: true, State: "ready", Running: true, PID: 1234,
			Binary: "/opt/term-llm/bin/term-llm", URL: "http://127.0.0.1:8765/", Host: "127.0.0.1", Port: 8765,
			Auth: "bearer", Enrollment: "not_required", Healthy: true,
			SpecPath: "/Users/example/.config/term-llm/services/web/service.json", NativePath: "/Users/example/Library/LaunchAgents/com.term-llm.web.plist", LastExitCode: &exitCode,
		}},
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"schema_version":1,"generated_at":"2026-11-07T09:00:00Z","uid":501,"platform":"darwin","services":[{"kind":"web","installed":true,"managed":true,"registered":true,"state":"ready","running":true,"pid":1234,"autostart":false,"binary":"/opt/term-llm/bin/term-llm","url":"http://127.0.0.1:8765/","host":"127.0.0.1","port":8765,"base_path":"","auth":"bearer","enrollment":"not_required","healthy":true,"health_error":"","spec_path":"/Users/example/.config/term-llm/services/web/service.json","native_path":"/Users/example/Library/LaunchAgents/com.term-llm.web.plist","last_exit_code":0,"error":""}]}`
	if string(data) != want {
		t.Fatalf("JSON contract changed:\ngot  %s\nwant %s", data, want)
	}
	t.Log(string(data))
}

func TestServiceStatusEnrollmentErrorIsSanitized(t *testing.T) {
	e, spec := statusFixture(t, "linux")
	spec.Auth = "passkey"
	saveStatusFixture(t, e, spec, true)
	if err := os.WriteFile(spec.AuthFile, []byte(statusTestSecret), 0600); err != nil {
		t.Fatal(err)
	}
	entry := inspectServiceStatus(context.Background(), e, "web")
	if entry.State != "error" || entry.Enrollment != "" || entry.Error == "" {
		t.Fatalf("%+v", entry)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(statusTestSecret)) {
		t.Fatal("leaked corrupt credential contents")
	}
}

func TestServiceStatusLeavesUnmanagedFilesUntouched(t *testing.T) {
	e, _ := statusFixture(t, "darwin")
	if err := os.MkdirAll(filepath.Dir(e.native.Path("web")), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.native.Path("web"), []byte(statusTestSecret), 0600); err != nil {
		t.Fatal(err)
	}
	e.native.Run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("unmanaged status contacted native manager")
		return nil, nil
	}
	entry := inspectServiceStatus(context.Background(), e, "web")
	if entry.State != "unmanaged" {
		t.Fatalf("%+v", entry)
	}
	data, err := os.ReadFile(e.native.Path("web"))
	if err != nil || string(data) != statusTestSecret {
		t.Fatalf("unmanaged definition was altered: %q %v", data, err)
	}
	if _, err := os.Stat(e.root); !os.IsNotExist(err) {
		t.Fatalf("status created managed files: %v", err)
	}
}
