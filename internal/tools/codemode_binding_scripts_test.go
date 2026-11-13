package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/skills"
)

func writeProbeScript(t *testing.T, dir, output string) {
	t.Helper()
	script := "#!/bin/sh\necho " + output + "\npwd\n"
	if err := os.WriteFile(filepath.Join(dir, "probe.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCodeModeScriptAuthorityFreezesSourceAndWorkingDir(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writeProbeScript(t, dirA, "source-a")
	writeProbeScript(t, dirB, "source-b")
	cfg := DefaultToolConfig()
	cfg.BaseDir = dirA
	cfg.ReadDirs = []string{dirA, dirB}
	cfg.WriteDirs = []string{dirA, dirB}
	cfg.Workspace = "none"
	manager, err := NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := manager.Registry
	def := agents.CustomToolDef{Name: "probe_script", Description: "probe", Script: "probe.sh", Call: "json"}
	if err := registry.RegisterCustomTools([]agents.CustomToolDef{def}, dirA); err != nil {
		t.Fatal(err)
	}
	authority := captureBindingAuthority(t, registry, "script-session", []string{"probe_script"})
	defer authority.Close()
	bound, ok := authority.Tool("probe_script")
	if !ok {
		t.Fatal("custom script was not bound")
	}
	live, _ := registry.Get("probe_script")
	if bound == live {
		t.Fatal("authority reused the mutable live custom script")
	}

	// Replace the live registration with a different source directory and
	// retarget the registry working directory after admission.
	if err := registry.RegisterCustomTools([]agents.CustomToolDef{def}, dirB); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetBaseDir(dirB); err != nil {
		t.Fatal(err)
	}
	ctx := core.ContextWithSessionID(context.Background(), "script-session")
	out := lifecycleCall(t, bound, ctx, `{}`)
	if out.IsError || !strings.Contains(out.Content, "source-a") {
		t.Fatalf("bound script output = %+v", out)
	}
	if strings.Contains(out.Content, "source-b") || strings.Contains(out.Content, dirB) {
		t.Fatalf("bound script followed the replacement source: %+v", out)
	}
	if !strings.Contains(out.Content, dirA) {
		t.Fatalf("bound script lost the admitted working directory: %+v", out)
	}
}

func TestCodeModeGatedSkillScriptKeepsTrustGateAndExposure(t *testing.T) {
	dir := t.TempDir()
	writeProbeScript(t, dir, "gated")
	cfg := DefaultToolConfig()
	cfg.BaseDir = dir
	cfg.ReadDirs = []string{dir}
	cfg.Workspace = "none"
	manager, err := NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := manager.Registry
	root := manager.ApprovalMgr
	root.SetApprovalMode(ModeYolo)
	def := agents.CustomToolDef{Name: "gated_probe", Description: "probe", Script: "probe.sh", Call: "json"}
	gated := skills.GateProjectScriptTool(newCustomScriptTool(def, dir, registry.limits, &cfg), root.YoloEnabled)
	registry.mu.Lock()
	registry.tools["gated_probe"] = gated
	registry.mu.Unlock()

	// The live wrapper is the registered instance; embedding core.Tool does not
	// promote optional exposure, so the wrapper must declare it explicitly.
	exposure, ok := gated.(core.CodeModeExposureProvider)
	if !ok || !exposure.CodeModeExposure().Allowed {
		t.Fatalf("gated skill script is not script-eligible: %#v", exposure)
	}

	authority := captureBindingAuthority(t, registry, "gated-session", []string{"gated_probe"})
	defer authority.Close()
	bound, ok := authority.Tool("gated_probe")
	if !ok {
		t.Fatal("gated script was not bound")
	}
	ctx := core.ContextWithSessionID(context.Background(), "gated-session")
	out := lifecycleCall(t, bound, ctx, `{}`)
	if out.IsError || !strings.Contains(out.Content, "gated") {
		t.Fatalf("gated script admitted under yolo = %+v", out)
	}

	// Narrowing the live root closes the captured gate; a later widening cannot
	// reopen an authority admitted under prompt.
	root.SetApprovalMode(ModePrompt)
	out = lifecycleCall(t, bound, ctx, `{}`)
	if !out.IsError || !strings.Contains(out.Content, "disabled") {
		t.Fatalf("narrowed gate did not apply: %+v", out)
	}
}

func TestCodeModeScriptBindingRejectsUnknownLocalAdapter(t *testing.T) {
	cfg := DefaultToolConfig()
	manager, err := NewToolManager(&cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := manager.Registry
	registry.mu.Lock()
	registry.tools["future_script"] = &futureScriptAdapter{name: "future_script"}
	registry.mu.Unlock()
	authority := captureBindingAuthority(t, registry, "future-session", []string{"future_script"})
	defer authority.Close()
	bound, ok := authority.Tool("future_script")
	if !ok {
		t.Fatal("unknown adapter was dropped instead of failing closed")
	}
	if _, ok := bound.(*codeModeUnavailableTool); !ok {
		t.Fatalf("unknown adapter type = %T", bound)
	}
	out := lifecycleCall(t, bound, context.Background(), `{}`)
	if !out.IsError || !strings.Contains(out.Content, codeModeBindingUnavailableCode) {
		t.Fatalf("unknown adapter did not fail closed: %#v", out)
	}
}

type futureScriptAdapter struct{ name string }

func (t *futureScriptAdapter) Spec() core.ToolSpec {
	return core.ToolSpec{Name: t.name, Description: "future local script adapter"}
}

func (t *futureScriptAdapter) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("live fallback executed"), nil
}

func (t *futureScriptAdapter) Preview(json.RawMessage) string { return t.name }
