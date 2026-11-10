package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/skills"
)

func probeActivation(t *testing.T) *skills.Activation {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.mjs"), []byte("export const answer = 42;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &skills.Activation{
		Skill: &skills.Skill{
			Name:          "probe",
			UserInstalled: true,
			CodeMode:      &skills.CodeModeManifest{Module: "index.mjs", Files: []string{"index.mjs"}},
		},
		BaseDir: dir,
	}
}

func controlResultText(result core.CodeModeCallResult) string {
	var b strings.Builder
	for _, part := range result.Parts {
		if part.Type == core.ToolContentPartText {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func awaitControlTerminal(t *testing.T, eng *engine.Engine, result core.CodeModeCallResult) core.CodeModeCallResult {
	t.Helper()
	for attempts := 0; attempts < 10; attempts++ {
		if result.Status == "completed" || result.Status == "failed" || result.Status == "cancelled" {
			return result
		}
		next, err := eng.WaitCodeMode(context.Background(), result.CellID, 2*time.Second, 0)
		if err != nil {
			t.Fatal(err)
		}
		next.Parts = append(result.Parts, next.Parts...)
		result = next
	}
	t.Fatalf("cell did not finish: %+v", result)
	return result
}

func TestAdmitSkillWithExecOnlyAndImportModule(t *testing.T) {
	activation := probeActivation(t)
	eng := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	eng.RegisterTool(NewExec())
	if got := OriginTrust(eng, activation, nil, nil); got == nil || !got() {
		t.Fatal("spawn_script-only trust rejected an installed skill")
	}
	if err := AdmitSkill(eng, activation, func() bool { return true }); err != nil {
		t.Fatalf("spawn_script-only admission: %v", err)
	}
	result, err := eng.ExecCodeMode(context.Background(), `const m=await import('skill:probe'); text(m.answer);`, 2*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	result = awaitControlTerminal(t, eng, result)
	if result.Status != "completed" || !result.StateCommitted {
		t.Fatalf("module cell: %+v", result)
	}
	if got := controlResultText(result); got != "42" {
		t.Fatalf("module output %q", got)
	}
}

func TestAdmitSkillLegacyCompatibilityPathStillAdmitted(t *testing.T) {
	activation := probeActivation(t)
	eng := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	eng.RegisterTool(New())
	if got := OriginTrust(eng, activation, nil, nil); got == nil || !got() {
		t.Fatal("legacy trust rejected an installed skill")
	}
	if err := AdmitSkill(eng, activation, func() bool { return true }); err != nil {
		t.Fatalf("legacy admission: %v", err)
	}
}

func TestAdmitSkillWithoutExecOrLegacyFailsClosed(t *testing.T) {
	activation := probeActivation(t)
	eng := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	if err := AdmitSkill(eng, activation, func() bool { return true }); err == nil || !strings.Contains(err.Error(), "spawn_script") {
		t.Fatalf("missing code mode tool admitted: %v", err)
	}
	if trust := OriginTrust(eng, activation, nil, nil); trust == nil || trust() {
		t.Fatal("trust enabled without a code mode tool")
	}
}

func TestAdmitSkillRejectsDeniedExec(t *testing.T) {
	activation := probeActivation(t)
	eng := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	eng.RegisterTool(NewExec())
	eng.SetAllowedToolsFilter([]string{"read_file"})
	if err := AdmitSkill(eng, activation, func() bool { return true }); err == nil {
		t.Fatal("denied spawn_script admitted a module")
	}
}

func TestActivationInstructionsDistinguishExecAndLegacyState(t *testing.T) {
	activation := probeActivation(t)
	instructions := ActivationInstructions(activation, "")
	if !strings.Contains(instructions, "spawn_script") || !strings.Contains(instructions, "legacy code_mode") {
		t.Fatalf("activation instructions do not distinguish lifecycle: %s", instructions)
	}
	if !strings.Contains(instructions, "committed automatically") || !strings.Contains(instructions, "explicitly") {
		t.Fatalf("activation instructions do not distinguish state semantics: %s", instructions)
	}
}
