package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/lifecycle"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/termhost"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/cobra"
)

type forcedCleanupReporter struct {
	steps *[]string
}

func (r *forcedCleanupReporter) Report(lifecycle.Snapshot) termhost.Control {
	return termhost.Control{}
}
func (r *forcedCleanupReporter) RestoreOSC() string {
	*r.steps = append(*r.steps, "restore")
	return "clear"
}
func (r *forcedCleanupReporter) Close() {
	*r.steps = append(*r.steps, "close")
}

func TestChatTerminalHostAuthorityRequiresInteractiveInvokingStreams(t *testing.T) {
	original := chatLifecycleInteractive
	t.Cleanup(func() { chatLifecycleInteractive = original })
	for _, test := range []struct {
		name        string
		interactive bool
		want        bool
	}{
		{name: "interactive chat", interactive: true, want: true},
		{name: "direct terminal auto-send", interactive: true, want: true},
		{name: "tool-spawned noninteractive auto-send", interactive: false, want: false},
		{name: "captured nested child", interactive: false, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			chatLifecycleInteractive = func(*os.File, *os.File) bool { return test.interactive }
			if got := chatOwnsTerminalHost(); got != test.want {
				t.Fatalf("chatOwnsTerminalHost() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestForcedLifecycleCleanupRestoresOSCBeforeClosingAdapters(t *testing.T) {
	var steps []string
	reporter := &forcedCleanupReporter{steps: &steps}
	forcedLifecycleCleanup(reporter, func(sequence string) (int, error) {
		steps = append(steps, "write:"+sequence)
		return len(sequence), nil
	})
	if got := strings.Join(steps, ","); got != "restore,write:clear,close" {
		t.Fatalf("forced cleanup order = %q", got)
	}
}

func TestDeprecatedTerminalProgressRetainsSmartTitleScope(t *testing.T) {
	for _, test := range []struct {
		name  string
		title string
		want  bool
	}{
		{name: "default smart", title: "", want: true},
		{name: "smart", title: "smart", want: true},
		{name: "basic", title: "basic", want: false},
		{name: "off", title: "off", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Chat.TerminalProgress = true
			cfg.Chat.TerminalTitle = test.title
			if got := legacyTerminalProgressEnabled(cfg); got != test.want {
				t.Fatalf("legacyTerminalProgressEnabled() = %v, want %v", got, test.want)
			}
		})
	}
	if legacyTerminalProgressEnabled(&config.Config{}) {
		t.Fatal("deprecated progress disabled setting enabled compatibility OSC")
	}
}

func TestToolManagerHasPathCapableTools(t *testing.T) {
	tests := []struct {
		name    string
		enabled []string
		want    bool
	}{
		{name: "read file", enabled: []string{tools.ReadFileToolName}, want: true},
		{name: "view image", enabled: []string{tools.ViewImageToolName}, want: true},
		{name: "shell only", enabled: []string{tools.ShellToolName}, want: false},
		{name: "ask user only", enabled: []string{tools.AskUserToolName}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, err := tools.NewToolManager(&tools.ToolConfig{Enabled: tt.enabled}, &config.Config{})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.ApprovalMgr.Close()
			if got := toolManagerHasPathCapableTools(manager); got != tt.want {
				t.Fatalf("toolManagerHasPathCapableTools() = %v, want %v", got, tt.want)
			}
		})
	}
	if toolManagerHasPathCapableTools(nil) {
		t.Fatal("nil tool manager reported path capability")
	}
}

func TestSessionIsConversationBranchUsesDurableLineage(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	source := &session.Session{ID: "title-branch-source", Provider: "mock", Model: "mock"}
	if err := store.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	anchor := session.NewMessage(source.ID, llm.AssistantText("branch here"), -1)
	if err := store.AddMessage(ctx, source.ID, anchor); err != nil {
		t.Fatal(err)
	}
	branchStore := store.(session.ConversationBranchStore)
	result, err := branchStore.CreateBranch(ctx, source.ID, session.CreateBranchOptions{
		AnchorMessageID: anchor.ID,
		IdempotencyKey:  "title-branch-child",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sessionIsConversationBranch(ctx, store, source.ID) {
		t.Fatal("root session was identified as a branch")
	}
	if result.Session == nil || !sessionIsConversationBranch(ctx, store, result.Session.ID) {
		t.Fatalf("child session was not identified from durable branch lineage: %#v", result.Session)
	}
}

func TestConfigureChatMCPServersInstallsSamplingBeforeEnable(t *testing.T) {
	manager := &recordingChatMCPManager{}
	var warnings strings.Builder
	configureChatMCPServers(context.Background(), manager, llm.NewMockProvider("mock"), "model", true, " first, second ", &warnings)
	want := []string{"sampling:model:true", "enable:first", "enable:second"}
	if len(manager.events) != len(want) {
		t.Fatalf("events = %#v, want %#v", manager.events, want)
	}
	for i := range want {
		if manager.events[i] != want[i] {
			t.Fatalf("events = %#v, want %#v", manager.events, want)
		}
	}
	if warnings.Len() != 0 {
		t.Fatalf("unexpected warnings: %s", warnings.String())
	}
}

type recordingChatMCPManager struct {
	events []string
}

func (m *recordingChatMCPManager) SetSamplingProvider(_ llm.Provider, model string, yolo bool) {
	m.events = append(m.events, "sampling:"+model+":"+map[bool]string{true: "true", false: "false"}[yolo])
}

func (m *recordingChatMCPManager) Enable(_ context.Context, name string) error {
	m.events = append(m.events, "enable:"+name)
	return nil
}

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

func TestResolveChatRuntimeSystemContextUsesExplicitDirectoryWithoutChdir(t *testing.T) {
	processCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "AGENTS.md"), []byte("worktree project instructions"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(projectDir, ".skills", "worktree-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: worktree-skill\ndescription: Worktree skill\n---\nUse it."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Skills = config.SkillsConfig{Enabled: true, AutoInvoke: true, MetadataBudgetTokens: 8000, MaxVisibleSkills: 8, IncludeProjectSkills: true, IncludeEcosystemPaths: false}
	agent := &agents.Agent{Name: "target", SystemPrompt: "cwd={{cwd}}\n{{agents}}"}
	resolved, err := resolveChatRuntimeSystemContextWithConfig(&cobra.Command{Use: "test"}, cfg, agent, "openai", "model", projectDir, "", "")
	if err != nil {
		t.Fatalf("resolve runtime context: %v", err)
	}
	for _, want := range []string{projectDir, "worktree project instructions", "worktree-skill"} {
		if !strings.Contains(resolved.SystemPrompt, want) {
			t.Fatalf("resolved prompt missing %q: %s", want, resolved.SystemPrompt)
		}
	}
	engine := llm.NewEngine(llm.NewMockProvider("test"), nil)
	resolved.ApplySkills(engine, nil)
	if _, ok := engine.Tools().Get(tools.ActivateSkillToolName); !ok {
		t.Fatal("runtime context did not install activate_skill registry")
	}
	if got, _ := os.Getwd(); got != processCWD {
		t.Fatalf("process CWD changed from %q to %q", processCWD, got)
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
		{"agy CLI", ""},
		{"agy CLI (gemini-3.6-flash-high)", "gemini-3.6-flash-high"},
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
