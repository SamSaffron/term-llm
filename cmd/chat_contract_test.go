package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	appchat "github.com/samsaffron/term-llm/internal/app/chat"
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
	if err := appchat.Run(context.Background(), nil, opts, deps); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("terminal before config: %v", err)
	}
	opts.AutoSend = []string{"test"}
	if err := appchat.Run(context.Background(), nil, opts, deps); !errors.Is(err, configErr) {
		t.Fatalf("config before approval: %v", err)
	}
	deps.LoadConfig = func() (*config.Config, error) { return &config.Config{}, nil }
	deps.SessionStore.NoSession = true
	if err := appchat.Run(context.Background(), nil, func() appchat.Options { o := opts; o.Changed.Resume = true; return o }(), deps); err == nil || !strings.Contains(err.Error(), "storage is disabled") {
		t.Fatalf("resume before approval: %v", err)
	}
	if err := appchat.Run(context.Background(), nil, opts, deps); err == nil || !strings.Contains(err.Error(), "--auto=false") {
		t.Fatalf("approval after settings: %v", err)
	}
}
