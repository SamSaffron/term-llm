package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
)

func TestBuildChatHandoverApprovalManager_SeedsShellPolicy(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Enabled = []string{"read_file"}
	cfg.Tools.ShellAllow = []string{"git *"}

	mgr, err := buildChatHandoverApprovalManager(cfg, SessionSettings{
		ShellAllow: []string{"go test *"},
		Scripts:    []string{"./handover.sh"},
	})
	if err != nil {
		t.Fatalf("buildChatHandoverApprovalManager() error = %v", err)
	}

	cases := []struct {
		command string
		want    tools.ConfirmOutcome
	}{
		{command: "git status", want: tools.ProceedOnce},
		{command: "go test ./...", want: tools.ProceedOnce},
		{command: "./handover.sh", want: tools.ProceedOnce},
	}

	for _, tc := range cases {
		got, err := mgr.CheckShellApproval(tc.command, "")
		if err != nil {
			t.Fatalf("CheckShellApproval(%q) error = %v", tc.command, err)
		}
		if got != tc.want {
			t.Fatalf("CheckShellApproval(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

func TestResolveChatHandoverSystemPromptWithConfig_UsesStartupPipelineIncludingSkills(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".skills", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".skills", "demo", "SKILL.md"), []byte(`---
name: demo
description: Demo skill
---

Use this demo skill when appropriate.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	oldChatSkills := chatSkills
	chatSkills = ""
	t.Cleanup(func() { chatSkills = oldChatSkills })

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"openai": {Model: "gpt-4o"}}}
	cfg.Skills = config.SkillsConfig{
		Enabled:               true,
		AutoInvoke:            true,
		MetadataBudgetTokens:  8000,
		MaxVisibleSkills:      8,
		IncludeProjectSkills:  true,
		IncludeEcosystemPaths: false,
	}
	targetAgent := &agents.Agent{Name: "target", SystemPrompt: "Target prompt"}

	got, err := resolveChatHandoverSystemPromptWithConfig(&cobra.Command{Use: "test"}, cfg, targetAgent, "openai", "gpt-4o")
	if err != nil {
		t.Fatalf("resolveChatHandoverSystemPromptWithConfig() error = %v", err)
	}
	if !strings.Contains(got, "Target prompt") {
		t.Fatalf("resolved prompt missing target prompt: %q", got)
	}
	if !strings.Contains(got, "<available_skills>") || !strings.Contains(got, "demo") {
		t.Fatalf("resolved prompt missing skills metadata: %q", got)
	}
}

func TestBuildChatProgramInput_AutoSendDisablesInput(t *testing.T) {
	got, err := buildChatProgramInput(true)
	if err != nil {
		t.Fatalf("buildChatProgramInput(true) error = %v", err)
	}
	if !got.disableInput {
		t.Fatal("buildChatProgramInput(true) should disable input")
	}
	if got.reader != nil {
		t.Fatalf("buildChatProgramInput(true) reader = %v, want nil", got.reader)
	}
	if got.cleanup == nil {
		t.Fatal("buildChatProgramInput(true) cleanup should not be nil")
	}
}

func TestBuildChatProgramInput_InteractiveUsesTTYInput(t *testing.T) {
	origOpenTTY := chatOpenTTY
	defer func() {
		chatOpenTTY = origOpenTTY
	}()

	ttyIn, ttyInWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() input error = %v", err)
	}
	defer ttyInWriter.Close()

	ttyOutReader, ttyOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() output error = %v", err)
	}
	defer ttyOutReader.Close()

	chatOpenTTY = func() (*os.File, *os.File, error) {
		return ttyIn, ttyOut, nil
	}

	got, err := buildChatProgramInput(false)
	if err != nil {
		t.Fatalf("buildChatProgramInput(false) error = %v", err)
	}
	if got.disableInput {
		t.Fatal("buildChatProgramInput(false) should keep input enabled")
	}
	if got.reader != ttyIn {
		t.Fatalf("buildChatProgramInput(false) reader = %v, want %v", got.reader, ttyIn)
	}
	if got.cleanup == nil {
		t.Fatal("buildChatProgramInput(false) cleanup should not be nil")
	}

	got.cleanup()

	if err := ttyIn.Close(); err == nil {
		t.Fatal("expected tty input to be closed by cleanup")
	}
	if err := ttyOut.Close(); err == nil {
		t.Fatal("expected tty output to be closed by cleanup")
	}
}

func TestGetModelName(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{
			name: "provider has explicit model",
			cfg: &config.Config{
				DefaultProvider: "claude-bin",
				Providers: map[string]config.ProviderConfig{
					"claude-bin": {Model: "opus-max"},
				},
			},
			want: "opus-max",
		},
		{
			name: "provider config present but model empty",
			cfg: &config.Config{
				DefaultProvider: "claude-bin",
				Providers: map[string]config.ProviderConfig{
					"claude-bin": {Model: ""},
				},
			},
			want: "",
		},
		{
			name: "default provider missing from providers map",
			cfg: &config.Config{
				DefaultProvider: "claude-bin",
				Providers: map[string]config.ProviderConfig{
					"claude-bin1": {Model: "opus-max"},
				},
			},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := getModelName(tc.cfg)
			if got == "unknown" {
				t.Fatalf("getModelName must never return literal \"unknown\"")
			}
			if got != tc.want {
				t.Fatalf("getModelName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractModelFromProviderName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Claude CLI (sonnet)", "sonnet"},
		{"Claude CLI (sonnet, effort=high)", "sonnet"},
		{"Grok CLI (grok-4.5, effort=xhigh)", "grok-4.5"},
		{"OpenAI (gpt-5)", "gpt-5"},
		{"OpenAI (gpt-5, effort=high)", "gpt-5"},
		{"Anthropic (claude-sonnet-4, thinking=8k)", "claude-sonnet-4"},
		{"Bedrock (claude-sonnet-4, adaptive, us-west-2)", "claude-sonnet-4"},
		{"Gemini (gemini-2.5-pro, thinking=high)", "gemini-2.5-pro"},
		{"xAI (grok-4-1-fast)", "grok-4-1-fast"},
		{"debug", "debug"},
		{"debug:slow", "debug:slow"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := extractModelFromProviderName(tc.in); got != tc.want {
				t.Fatalf("extractModelFromProviderName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildChatProgramInput_InteractivePropagatesTTYError(t *testing.T) {
	origOpenTTY := chatOpenTTY
	defer func() {
		chatOpenTTY = origOpenTTY
	}()

	chatOpenTTY = func() (*os.File, *os.File, error) {
		return nil, nil, errors.New("boom")
	}

	_, err := buildChatProgramInput(false)
	if err == nil {
		t.Fatal("expected error when opening chat TTY fails")
	}
}
