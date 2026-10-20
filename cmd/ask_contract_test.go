package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/spf13/cobra"
)

func TestCaptureAskInvocationFreezesInputs(t *testing.T) {
	oldFiles, oldTools, oldSearch := askFiles, askTools, askSearch
	oldDB, oldNoSession, oldRaw, oldStats := sessionDBPath, noSession, debugRaw, showStats
	t.Cleanup(func() {
		askFiles, askTools, askSearch = oldFiles, oldTools, oldSearch
		sessionDBPath, noSession, debugRaw, showStats = oldDB, oldNoSession, oldRaw, oldStats
	})
	askFiles, askTools, askSearch = []string{"before"}, "", false
	sessionDBPath, noSession, debugRaw, showStats = "captured.db", true, true, true
	command := &cobra.Command{}
	for _, name := range []string{"resume", "tools", "system", "mcp"} {
		command.Flags().String(name, "", "")
		_ = command.Flags().Set(name, "")
	}
	command.Flags().Bool("search", true, "")
	_ = command.Flags().Set("search", "false")
	command.Flags().Int("max-turns", 50, "")
	_ = command.Flags().Set("max-turns", "0")
	var out, errOut bytes.Buffer
	input := strings.NewReader("input")
	command.SetIn(input)
	command.SetOut(&out)
	command.SetErr(&errOut)
	opts, deps := captureAskInvocation(command)
	askFiles[0], askTools, askSearch = "after", "shell", true
	sessionDBPath, noSession, debugRaw, showStats = "later.db", false, false, false
	if opts.Files[0] != "before" || opts.Tools != "" || opts.Search || !opts.DebugRaw || !opts.ShowStats {
		t.Fatalf("snapshot = %+v", opts)
	}
	if !opts.Changed.Resume || !opts.Changed.Tools || !opts.Changed.System || !opts.Changed.MCP || !opts.Changed.Search || !opts.Changed.MaxTurns {
		t.Fatalf("changed = %+v", opts.Changed)
	}
	if deps.Stdin != input || !deps.HasStdin || deps.Stdout != &out || deps.Stderr != &errOut || deps.SessionStore.DBPath != "captured.db" || !deps.SessionStore.NoSession {
		t.Fatalf("dependencies = %+v", deps)
	}
}

func TestAskStartupPrecedence(t *testing.T) {
	command := &cobra.Command{}
	command.Flags().Bool("auto", false, "")
	_ = command.Flags().Set("auto", "false")
	opts, deps := captureAskInvocation(command)
	configErr := errors.New("config wins")
	deps.LoadConfig = func() (*config.Config, error) { return nil, configErr }
	opts.Progressive, opts.StopWhen = true, "invalid"
	if err := runAskWith(context.Background(), nil, opts, deps); err == nil || errors.Is(err, configErr) {
		t.Fatalf("validation must precede config: %v", err)
	}
	opts.Progressive, opts.StopWhen = false, ""
	if err := runAskWith(context.Background(), nil, opts, deps); !errors.Is(err, configErr) {
		t.Fatalf("config must precede approval: %v", err)
	}
	deps.LoadConfig = func() (*config.Config, error) { return &config.Config{}, nil }
	if err := runAskWith(context.Background(), nil, opts, deps); err == nil || !strings.Contains(err.Error(), "--auto=false") {
		t.Fatalf("approval must precede question: %v", err)
	}
}
