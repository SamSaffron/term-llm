package service

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestServiceAutostartManagedAndIndependent(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		for _, action := range []string{"on", "off", "status"} {
			t.Run(platform+"/"+action, func(t *testing.T) {
				e, spec := statusFixture(t, platform)
				saveStatusFixture(t, e, spec, true)
				oldRun := e.native.Run
				var mutations []string
				e.native.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
					joined := strings.Join(args, " ")
					if strings.HasPrefix(joined, "print gui/501/com.term-llm.") {
						return []byte("path = " + e.native.Path("web")), nil
					}
					if strings.HasPrefix(joined, "--user show --property=FragmentPath") {
						return nil, nil
					}
					if strings.Contains(joined, " enable ") || strings.Contains(joined, " disable ") || args[0] == "enable" || args[0] == "disable" {
						mutations = append(mutations, joined)
						return nil, nil
					}
					return oldRun(ctx, name, args...)
				}
				cmd := &cobra.Command{}
				cmd.SetContext(context.Background())
				var output bytes.Buffer
				cmd.SetOut(&output)
				if err := autostartUserService(cmd, e, []string{"web", action}); err != nil {
					t.Fatal(err)
				}
				if action == "status" {
					if len(mutations) != 0 || output.String() != "enabled\n" {
						t.Fatalf("mutations=%v output=%q", mutations, output.String())
					}
				} else if len(mutations) != 1 || !strings.Contains(mutations[0], map[string]string{"on": "enable", "off": "disable"}[action]) {
					t.Fatalf("mutations=%v", mutations)
				}
			})
		}
	}
}

func TestServiceAutostartRefusesUnsafeDefinitionsAndLock(t *testing.T) {
	for _, name := range []string{"not installed", "not registered", "unmanaged", "locked"} {
		t.Run(name, func(t *testing.T) {
			e, spec := statusFixture(t, "linux")
			if name != "not installed" {
				saveStatusFixture(t, e, spec, name != "not registered")
			}
			if name == "unmanaged" {
				if err := os.WriteFile(e.native.Path("web"), []byte("not ours"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "locked" {
				unlock, err := e.lock("web")
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			e.native.Run = func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("unsafe operation contacted manager")
				return nil, nil
			}
			if err := autostartUserService(&cobra.Command{}, e, []string{"web", "on"}); err == nil {
				t.Fatal("unsafe autostart succeeded")
			}
		})
	}
}

func TestServiceLifecycleControlFlagDispatch(t *testing.T) {
	for _, tc := range []struct{ action, flag, want string }{
		{"start", "", "enable"}, {"start", "no-enable", "start"}, {"restart", "", "restart"}, {"restart", "no-enable", "restart"}, {"stop", "", "disable --now"}, {"stop", "keep-enabled", "stop"},
	} {
		t.Run(tc.action+"/"+tc.flag, func(t *testing.T) {
			e, _ := statusFixture(t, "linux")
			var calls []string
			e.native.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, strings.Join(args, " "))
				return nil, nil
			}
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			if tc.flag != "" {
				cmd.Flags().Bool(tc.flag, true, "")
			}
			var err error
			if tc.action == "stop" {
				err = stopUserService(cmd, e, "web")
			} else {
				err = startNativeUserService(cmd, e, "web", tc.action)
			}
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(calls, "\n")
			if !strings.Contains(joined, "--user "+tc.want+" ") {
				t.Fatalf("wrong dispatch: %s", joined)
			}
			if tc.action != "start" || tc.flag != "" {
				if strings.Contains(joined, "--user enable ") {
					t.Fatalf("unexpected enable: %s", joined)
				}
			}
			if tc.flag == "keep-enabled" && strings.Contains(joined, "disable") {
				t.Fatalf("unexpected disable: %s", joined)
			}
		})
	}
}

func TestServiceRestartDisabledRequiresNoEnable(t *testing.T) {
	for _, noEnable := range []bool{false, true} {
		e, spec := statusFixture(t, "linux")
		var checkedEnabled bool
		e.native.Run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), "is-enabled") {
				checkedEnabled = true
				return []byte("disabled"), nil
			}
			return []byte("MainPID=123"), nil
		}
		// Cancel readiness immediately: the test concerns the enabled guard, not
		// successful startup. The credentials list is empty to avoid secret access.
		spec.Secrets = nil
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetOut(&bytes.Buffer{})
		cmd.Flags().Bool("no-enable", noEnable, "")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cmd.SetContext(ctx)
		err := startUserService(cmd, e, spec, "web", "restart")
		if noEnable {
			if checkedEnabled || (err != nil && strings.Contains(err.Error(), "autostart is disabled")) {
				t.Fatalf("--no-enable required autostart: %v", err)
			}
		} else if !checkedEnabled || err == nil || !strings.Contains(err.Error(), "autostart is disabled") {
			t.Fatalf("default restart lost guard: %v", err)
		}
	}
}
