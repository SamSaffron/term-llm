package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/spf13/cobra"
)

func TestNotifyCommandsRegistered(t *testing.T) {
	for _, name := range []string{"app", "web", "telegram"} {
		t.Run(name, func(t *testing.T) {
			command, rest, err := rootCmd.Find([]string{"notify", name})
			if err != nil || command.Name() != name || command.Parent() != notifyCmd || len(rest) != 0 {
				t.Fatalf("notify %s command = %v, rest = %v, err = %v", name, command, rest, err)
			}
			if command.RunE == nil {
				t.Fatal("command has no application binding")
			}
			for _, args := range [][]string{nil, {"one", "two"}} {
				if err := command.Args(command, args); err == nil {
					t.Fatalf("accepted args %v, want exactly one message", args)
				}
			}
			if err := command.Args(command, []string{"build finished"}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if !strings.Contains(notifyCmd.Long, "notify app") || !strings.Contains(notifyCmd.Long, "browser-only") {
		t.Fatal("notify help must describe app and browser targeting")
	}
}

func TestNotifyPushCommandsReturnConfigErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := config.GetConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("serve: [invalid yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []*cobra.Command{notifyAppCmd, notifyWebCmd} {
		t.Run(command.Name(), func(t *testing.T) {
			// Execute a fresh Cobra shell with the real binding, without running
			// root startup hooks or changing the registered command's state.
			shell := &cobra.Command{Use: command.Use, Args: command.Args, RunE: command.RunE, SilenceUsage: true, SilenceErrors: true}
			shell.SetOut(io.Discard)
			shell.SetErr(io.Discard)
			shell.SetArgs([]string{"finished"})
			if err := shell.Execute(); err == nil || !strings.Contains(err.Error(), "failed to read config") {
				t.Fatalf("error = %v, want config parse error", err)
			}
		})
	}
}
