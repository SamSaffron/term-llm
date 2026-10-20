package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestChatRequiresApprovalResolverAfterConfigSessionAndSettings(t *testing.T) {
	r := testInvocation()
	r.deps.Interactive = func() bool { return false }
	r.deps.RestoreReloadState = func(string, any) (bool, error) { return false, nil }
	configErr := errors.New("invalid config")
	r.deps.LoadConfig = func() (*config.Config, error) { return nil, configErr }
	if err := Run(context.Background(), nil, r.opts, r.deps); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("terminal must win: %v", err)
	}
	r.opts.AutoSend = []string{"question"}
	if err := Run(context.Background(), nil, r.opts, r.deps); !errors.Is(err, configErr) {
		t.Fatalf("config must win: %v", err)
	}
	r.deps.LoadConfig = func() (*config.Config, error) { return &config.Config{}, nil }
	r.deps.SessionStore.NoSession = true
	r.opts.Changed.Resume = true
	if err := Run(context.Background(), nil, r.opts, r.deps); err == nil || !strings.Contains(err.Error(), "storage is disabled") {
		t.Fatalf("resume must win: %v", err)
	}
	r.opts.Changed.Resume = false
	cycle := filepath.Join(t.TempDir(), "cycle.md")
	if err := os.WriteFile(cycle, []byte("{{file:cycle.md}}"), 0600); err != nil {
		t.Fatal(err)
	}
	r.opts.SystemMessage = "{{file:" + cycle + "}}"
	if err := Run(context.Background(), nil, r.opts, r.deps); err == nil || strings.Contains(err.Error(), "resolver") {
		t.Fatalf("settings must win: %v", err)
	}
	r.opts.SystemMessage = ""
	if err := Run(context.Background(), nil, r.opts, r.deps); err == nil || err.Error() != "chat approval resolver is required" {
		t.Fatalf("missing resolver: %v", err)
	}
}
