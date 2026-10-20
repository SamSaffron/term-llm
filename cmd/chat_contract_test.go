package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
)

func TestCaptureChatInvocationFreezesInputsAndDefersApproval(t *testing.T) {
	oldSend, oldRead, oldProvider := chatAutoSend, chatReadDirs, chatProvider
	oldDB, oldNoSession := sessionDBPath, noSession
	t.Cleanup(func() {
		chatAutoSend, chatReadDirs, chatProvider = oldSend, oldRead, oldProvider
		sessionDBPath, noSession = oldDB, oldNoSession
	})
	chatAutoSend, chatReadDirs, chatProvider = []string{"send"}, []string{"read"}, "debug"
	sessionDBPath, noSession = "chat-captured.db", true
	command := &cobra.Command{}
	for _, name := range []string{"resume", "tools", "system"} {
		command.Flags().String(name, "", "")
		_ = command.Flags().Set(name, "")
	}
	command.Flags().Bool("search", true, "")
	_ = command.Flags().Set("search", "false")
	command.Flags().Int("max-turns", 200, "")
	_ = command.Flags().Set("max-turns", "0")
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	opts, deps := captureChatInvocation(command)
	chatAutoSend[0], chatReadDirs[0], chatProvider = "later", "later", "later"
	sessionDBPath, noSession = "later.db", false
	if opts.AutoSend[0] != "send" || opts.ReadDirs[0] != "read" || opts.Provider != "debug" || !opts.Changed.Resume || !opts.Changed.Tools || !opts.Changed.System || !opts.Changed.MaxTurns || !opts.Changed.Search {
		t.Fatalf("options=%+v", opts)
	}
	if deps.Stdout != &stdout || deps.Stderr != &stderr || deps.SessionStore.DBPath != "chat-captured.db" || !deps.SessionStore.NoSession || deps.ExecReload == nil || deps.OpenTTY == nil || deps.RestoreReloadState == nil {
		t.Fatalf("deps=%+v", deps)
	}
	carried := tools.ModeYolo
	persisted := session.ApprovalModePrompt
	resolved, err := deps.ResolveApproval(&config.Config{}, &persisted, &carried)
	if err != nil || resolved.Mode != carried {
		t.Fatalf("handover carry=%+v %v", resolved, err)
	}
	nilOpts, nilDeps := captureChatInvocation(nil)
	if nilOpts.Changed.MaxTurns || nilDeps.Stderr != io.Discard {
		t.Fatalf("nil helper semantics=%+v %+v", nilOpts, nilDeps)
	}
	if got, err := resolveChatHandoverSystemPromptWithConfig(nil, nil, nil, "", "", nilDeps.MemoryPaths); err != nil || got != "" {
		t.Fatalf("nil target=%q %v", got, err)
	}
}

func TestChatStartupPrecedence(t *testing.T) {
	command := &cobra.Command{}
	command.Flags().Bool("auto", false, "")
	_ = command.Flags().Set("auto", "false")
	opts, deps := captureChatInvocation(command)
	deps.Interactive = func() bool { return false }
	deps.RestoreReloadState = func(string, any) (bool, error) { return false, nil }
	configErr := errors.New("config wins")
	deps.LoadConfig = func() (*config.Config, error) { return nil, configErr }
	invocation := &chatInvocation{opts: opts, deps: deps}
	if err := invocation.run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("terminal before config: %v", err)
	}
	invocation.opts.AutoSend = []string{"test"}
	if err := invocation.run(context.Background(), nil); !errors.Is(err, configErr) {
		t.Fatalf("config before approval: %v", err)
	}
	invocation.deps.LoadConfig = func() (*config.Config, error) { return &config.Config{}, nil }
	invocation.deps.SessionStore.NoSession = true
	if _, err := invocation.buildChatSessionRuntime(context.Background(), chatSessionLaunch{resumeRequested: true}, nil); err == nil || !strings.Contains(err.Error(), "storage is disabled") {
		t.Fatalf("resume before approval: %v", err)
	}
	if _, err := invocation.buildChatSessionRuntime(context.Background(), chatSessionLaunch{}, nil); err == nil || !strings.Contains(err.Error(), "--auto=false") {
		t.Fatalf("approval after settings: %v", err)
	}
}
