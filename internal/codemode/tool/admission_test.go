package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	"github.com/term-llm/term-llm/internal/skills"
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
	err := AdmitSkill(eng, activation, func() bool { return true })
	if err == nil || !strings.Contains(err.Error(), "spawn_script") || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing code mode tool admitted or misreported: %v", err)
	}
	if admitted, rest := Admitted(err); admitted || rest != nil {
		t.Fatalf("Admitted(unavailable) = %v, %v; want false, nil so activation goes on", admitted, rest)
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
	err := AdmitSkill(eng, activation, func() bool { return true })
	if err == nil {
		t.Fatal("denied spawn_script admitted a module")
	}
	// A filtered-out spawn_script is no code mode: the skill's prose may still activate.
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("denied spawn_script reported as %v, want ErrUnavailable", err)
	}
}

func TestActivationInstructionsDistinguishExecAndLegacyState(t *testing.T) {
	activation := probeActivation(t)
	if note := ActivationInstructions(activation, "", false); strings.Contains(note, "admitted") || !strings.Contains(note, "not available here") {
		t.Fatalf("unadmitted module described as admitted: %s", note)
	}
	instructions := ActivationInstructions(activation, "", true)
	if !strings.Contains(instructions, "spawn_script") || !strings.Contains(instructions, "legacy code_mode") {
		t.Fatalf("activation instructions do not distinguish lifecycle: %s", instructions)
	}
	if !strings.Contains(instructions, "committed automatically") || !strings.Contains(instructions, "explicitly") {
		t.Fatalf("activation instructions do not distinguish state semantics: %s", instructions)
	}
}

func TestAdmittedKeepsTrustFailures(t *testing.T) {
	activation := probeActivation(t)
	eng := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	eng.RegisterTool(NewExec())
	err := AdmitSkill(eng, activation, func() bool { return false })
	if admitted, rest := Admitted(err); admitted || rest == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("Admitted(untrusted) = %v, %v; want the trust failure to reject activation", admitted, rest)
	}
}
