package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/app/ask"
	appchat "github.com/samsaffron/term-llm/internal/app/chat"
	"github.com/spf13/cobra"
)

func TestCLIUserFlagsCaptureAndValidate(t *testing.T) {
	for _, command := range []*cobra.Command{askCmd, chatCmd} {
		t.Run(command.Name(), func(t *testing.T) {
			flag := command.Flags().Lookup("user")
			if flag == nil || flag.Shorthand != "" {
				t.Fatalf("--user flag=%v", flag)
			}
			old, changed := flag.Value.String(), flag.Changed
			t.Cleanup(func() { _ = flag.Value.Set(old); flag.Changed = changed })
			for _, name := range []string{" nick kroll ", "", "bad\nname", strings.Repeat("x", 129)} {
				if err := command.ParseFlags([]string{"--user", name}); err != nil {
					t.Fatal(err)
				}
				if command == askCmd {
					opts, _ := captureAskInvocation(command)
					if opts.User != name || !opts.Changed.User {
						t.Fatalf("snapshot=%+v", opts)
					}
					if name != " nick kroll " {
						if err := ask.Run(context.Background(), nil, opts, ask.Dependencies{}); err == nil || !strings.Contains(err.Error(), "--user") {
							t.Fatalf("invalid ask user: %v", err)
						}
					}
				} else {
					opts, _ := captureChatInvocation(command)
					if opts.User != name || !opts.Changed.User {
						t.Fatalf("snapshot=%+v", opts)
					}
					if name != " nick kroll " {
						if err := appchat.Run(context.Background(), nil, opts, appchat.Dependencies{}); err == nil || !strings.Contains(err.Error(), "--user") {
							t.Fatalf("invalid chat user: %v", err)
						}
					}
				}
			}
			if err := command.ParseFlags([]string{"--user"}); err == nil {
				t.Fatal("missing --user value accepted")
			}
		})
	}
}
