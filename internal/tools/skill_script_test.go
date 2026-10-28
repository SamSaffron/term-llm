package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/skills"
)

func TestSkillScriptExecutionPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires /bin/sh")
	}
	for _, tc := range []struct {
		name          string
		userInstalled bool
		optIn         bool
		mode          ApprovalMode
		noManager     bool
		nilConfig     bool
		allowed       bool
	}{
		{name: "project prompt"},
		{name: "project auto", mode: ModeAuto},
		{name: "project no config", nilConfig: true},
		{name: "project no manager", noManager: true},
		{name: "project config opt-in", optIn: true, allowed: true},
		{name: "project yolo", mode: ModeYolo, allowed: true},
		{name: "user prompt", userInstalled: true, allowed: true},
		{name: "user auto", userInstalled: true, mode: ModeAuto, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeScript(t, dir, "check.sh", "#!/bin/sh\nprintf ok > \"$TERM_LLM_AGENT_DIR/ran\"\necho ok\n")
			approval := NewApprovalManager(NewToolPermissions())
			approval.SetApprovalMode(tc.mode)
			approval.PromptFunc = func(*ApprovalRequest) (ConfirmOutcome, string) {
				t.Fatal("skill gate must not prompt")
				return Cancel, ""
			}
			approval.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
				t.Fatal("skill gate must not send code to Guardian")
				return PolicyDecision{}, nil
			}, nil)
			cfg := &config.Config{Skills: config.SkillsConfig{AllowProjectScriptTools: tc.optIn}}
			if tc.nilConfig {
				cfg = nil
			}
			if tc.noManager {
				approval = nil
			}
			registry := &LocalToolRegistry{tools: make(map[string]core.Tool), limits: DefaultOutputLimits(), appConfig: cfg, approval: approval}
			if err := registry.RegisterSkillTools([]skills.SkillToolDef{{
				Name: "check_build", Description: "Check build", Script: "check.sh", UserInstalled: tc.userInstalled,
			}}, dir); err != nil {
				t.Fatal(err)
			}
			tool, ok := registry.Get("check_build")
			if !ok {
				t.Fatal("skill tool was not registered")
			}
			output, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(filepath.Join(dir, "ran"))
			if tc.allowed {
				if output.IsError || statErr != nil {
					t.Fatalf("allowed script did not run: output=%+v stat=%v", output, statErr)
				}
			} else if !output.IsError || !os.IsNotExist(statErr) || !strings.Contains(output.Content, "--allow-project-skill-tools") || !strings.Contains(output.Content, "skills.allow_project_script_tools") {
				t.Fatalf("expected denial before execution: output=%+v stat=%v", output, statErr)
			}
		})
	}
}

func TestProjectSkillToolUsesCurrentYoloMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires /bin/sh")
	}
	dir := t.TempDir()
	writeScript(t, dir, "check.sh", "#!/bin/sh\necho ok\n")
	approval := NewApprovalManager(NewToolPermissions())
	registry := &LocalToolRegistry{tools: make(map[string]core.Tool), limits: DefaultOutputLimits(), approval: approval}
	if err := registry.RegisterSkillTools([]skills.SkillToolDef{{Name: "check_build", Description: "Check build", Script: "check.sh"}}, dir); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Get("check_build")
	for _, mode := range []ApprovalMode{ModePrompt, ModeYolo, ModeAuto, ModeYolo, ModePrompt} {
		approval.SetApprovalMode(mode)
		output, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil || output.IsError != (mode != ModeYolo) {
			t.Fatalf("mode=%v: output=%+v err=%v", mode, output, err)
		}
	}
}

func TestSkillGateLeavesAgentCustomToolsUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires /bin/sh")
	}
	dir := t.TempDir()
	writeScript(t, dir, "check.sh", "#!/bin/sh\necho ok\n")
	registry := &LocalToolRegistry{tools: make(map[string]core.Tool), limits: DefaultOutputLimits()}
	if err := registry.RegisterCustomTools([]agents.CustomToolDef{{Name: "agent_check", Script: "check.sh"}}, dir); err != nil {
		t.Fatal(err)
	}
	tool, _ := registry.Get("agent_check")
	output, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || output.IsError {
		t.Fatalf("agent tool changed: output=%+v err=%v", output, err)
	}
}

func TestRegisterSkillToolsValidatesRequiredFields(t *testing.T) {
	for _, tc := range []struct {
		def  skills.SkillToolDef
		want string
	}{
		{skills.SkillToolDef{}, "name is required"},
		{skills.SkillToolDef{Name: "check"}, "description is required"},
		{skills.SkillToolDef{Name: "check", Description: "Check"}, "script is required"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			registry := &LocalToolRegistry{tools: make(map[string]core.Tool)}
			if err := registry.RegisterSkillTools([]skills.SkillToolDef{tc.def}, t.TempDir()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("registration error = %v, want %q", err, tc.want)
			}
		})
	}
}
