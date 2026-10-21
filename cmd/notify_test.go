package cmd

import (
	"bytes"
	"fmt"
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

func TestNotifyMessageErrors(t *testing.T) {
	for _, command := range []*cobra.Command{notifyCmd, notifyAppCmd, notifyWebCmd, notifyTelegramCmd} {
		t.Run(command.Name(), func(t *testing.T) {
			for _, tc := range []struct {
				args []string
				want string
			}{
				{nil, "notification message is required"},
				{[]string{"   "}, "notification message is required"},
				{[]string{"build", "finished"}, "wrap messages containing spaces in quotes"},
			} {
				err := command.Args(command, tc.args)
				if err == nil {
					t.Fatalf("accepted %q", tc.args)
				}
				example := command.CommandPath()
				if command == notifyTelegramCmd {
					example += " --chat-id 12345"
				}
				for _, want := range []string{tc.want, "Usage: " + command.UseLine(), "Example: " + example + ` "build finished"`} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want %q", err, want)
					}
				}
			}
			for _, message := range []string{"build finished", "app"} {
				if err := command.Args(command, []string{message}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestNotifyCompletionProtocol(t *testing.T) {
	for _, request := range []string{cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd} {
		for _, tc := range []struct {
			name       string
			args, want []string
		}{
			{"targets", []string{"notify", ""}, []string{"app", "telegram", "web"}},
			{"target prefix", []string{"notify", "ap"}, []string{"app"}},
			{"broadcast message", []string{"notify", "build"}, nil},
			{"app message", []string{"notify", "app", ""}, nil},
			{"web message", []string{"notify", "web", ""}, nil},
			{"telegram required flag", []string{"notify", "telegram", ""}, []string{"--chat-id"}},
			{"telegram message", []string{"notify", "telegram", "build"}, nil},
			{"broadcast chat ID", []string{"notify", "--chat-id", ""}, nil},
			{"telegram chat ID", []string{"notify", "telegram", "--chat-id", ""}, nil},
			{"message supplied", []string{"notify", "app", "finished", ""}, nil},
			{"broadcast parse mode", []string{"notify", "--parse-mode", ""}, []string{"Markdown", "HTML"}},
			{"telegram parse mode", []string{"notify", "telegram", "--parse-mode", ""}, []string{"Markdown", "HTML"}},
		} {
			t.Run(request+"/"+tc.name, func(t *testing.T) {
				// Copy the real bindings to avoid mutating the global command tree.
				clone := func(source *cobra.Command) *cobra.Command {
					command := &cobra.Command{Use: source.Use, Short: source.Short, Args: source.Args, ValidArgsFunction: source.ValidArgsFunction, RunE: source.RunE}
					// Reuse production flags so completion registrations and required-flag
					// annotations are tested, not recreated by the test. These requests
					// never supply flag values, so the shared flag state is unchanged.
					command.Flags().AddFlagSet(source.LocalNonPersistentFlags())
					return command
				}
				root := &cobra.Command{Use: "term-llm"}
				parent := clone(notifyCmd)
				for _, source := range []*cobra.Command{notifyAppCmd, notifyWebCmd, notifyTelegramCmd} {
					parent.AddCommand(clone(source))
				}
				root.AddCommand(parent)
				var output bytes.Buffer
				root.SetOut(&output)
				root.SetErr(io.Discard)
				root.SetArgs(append([]string{request}, tc.args...))
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(strings.TrimSpace(output.String()), "\n")
				if got := lines[len(lines)-1]; got != fmt.Sprintf(":%d", cobra.ShellCompDirectiveNoFileComp) {
					t.Fatalf("directive = %q; output = %q", got, output.String())
				}
				var got []string
				for _, line := range lines[:len(lines)-1] {
					value, _, _ := strings.Cut(line, "\t")
					got = append(got, value)
				}
				if strings.Join(got, ",") != strings.Join(tc.want, ",") {
					t.Fatalf("completions = %q, want %q", got, tc.want)
				}
			})
		}
	}
}
