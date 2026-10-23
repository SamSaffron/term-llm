package service

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/userservice"
	"github.com/spf13/cobra"
)

func TestServiceDebugFailure(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.Flags().Bool("debug", true, "")
			cmd.SetErr(&out)
			calls := 0
			e := serviceEnvironment{root: t.TempDir(), native: userservice.Native{OS: platform, UID: 1000, Trace: &out, Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
				calls++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("diagnostics need a deadline")
				}
				if name == "journalctl" || name == "/usr/bin/tail" {
					if !strings.Contains(strings.Join(args, " "), "60") {
						t.Fatal("logs must be bounded")
					}
					return []byte("server failed: address already in use"), nil
				}
				return []byte("state = exited\nlast exit code = 1\nMainPID=0\nEnvironment=DO_NOT_PRINT\nSECRET = DO_NOT_PRINT\n"), errors.New("not running")
			}}}
			reportServiceDebugFailure(cmd, e, "web")
			got := out.String()
			for _, want := range []string{"last exit code = 1", "MainPID=0", "address already in use", "sensitive data", "service logs web --follow"} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q: %s", want, got)
				}
			}
			if strings.Contains(got, "DO_NOT_PRINT") {
				t.Fatal("native environment leaked")
			}
			if calls != 2 {
				t.Fatalf("got %d diagnostic commands", calls)
			}
		})
	}
}

func TestServiceDebugOptIn(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&out)
	serviceDebug(cmd, "hidden")
	if out.Len() != 0 {
		t.Fatal(out.String())
	}
	cmd.Flags().Bool("debug", true, "")
	serviceDebug(cmd, "visible %s", "stage")
	if !strings.Contains(out.String(), "visible stage") {
		t.Fatal(out.String())
	}

}

func TestServiceReadinessTimeoutReportsCrashLoop(t *testing.T) {
	prev := serviceReadyTimeout
	serviceReadyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { serviceReadyTimeout = prev })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	spec := userservice.Spec{Kind: "web", Host: "127.0.0.1", Port: port, BasePath: "/ui"}
	native := userservice.Native{OS: "darwin", UID: 501, Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("\tstate = spawn scheduled\n\truns = 42\n\tlast exit code = 1\n\tenvironment = {\n\t\tTOKEN => DO_NOT_PRINT\n\t}\n\t\tstate = active\n"), nil
	}}
	err = waitUserService(context.Background(), spec, native)
	if err == nil {
		t.Fatal("unreachable backend reported ready")
	}
	got := err.Error()
	for _, want := range []string{"did not answer health checks", "not running (state = spawn scheduled, runs = 42, last exit code = 1)", "exiting during startup", "--debug"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "DO_NOT_PRINT") || strings.Count(got, "state =") != 1 {
		t.Fatalf("summary must contain only first-occurrence lifecycle fields: %s", got)
	}

	native.Run = func(context.Context, string, ...string) ([]byte, error) { return []byte("\tpid = 77\n"), nil }
	if err = waitUserService(context.Background(), spec, native); err == nil || strings.Contains(err.Error(), "not running") {
		t.Fatalf("running but unhealthy service must not be reported as exiting: %v", err)
	}
}
