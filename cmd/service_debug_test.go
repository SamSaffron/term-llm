package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

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
	for _, name := range []string{"start", "restart"} {
		child, _, err := serviceCmd.Find([]string{name})
		if err != nil || child.Flags().Lookup("debug") == nil {
			t.Fatalf("%s missing debug flag: %v", name, err)
		}
	}
}
