package serve

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/tools"
)

func TestServeShellApprovalDefaultsToOnceAndDisclosesExecutionGrant(t *testing.T) {
	dir := t.TempDir()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	pending := newServePendingApproval("python build.py", false, true, false, dir)
	prompt := pending.snapshot()
	want := []string{"once", "command", "pattern", "deny"}
	if len(prompt.Options) != len(want) {
		t.Fatalf("options = %+v, want four choices", prompt.Options)
	}
	for i, choice := range want {
		if prompt.Options[i].Choice != choice || prompt.Options[i].Index != i {
			t.Errorf("option %d = %+v, want %s", i, prompt.Options[i], choice)
		}
	}
	advanced := prompt.Options[2]
	if !strings.Contains(advanced.Label, "Advanced:") || !strings.Contains(advanced.Label, "python *") || !strings.Contains(advanced.Label, "code execution") {
		t.Errorf("missing broad execution warning: %+v", advanced)
	}
	if !strings.Contains(advanced.Description, "OS user's access") {
		t.Errorf("missing OS authority warning: %+v", advanced)
	}
	if got := pending.Options[0]; got.Choice != tools.ApprovalChoiceOnce || got.SaveToRepo || got.Pattern != "" {
		t.Errorf("web default grants more than once: %+v", got)
	}
}

func TestServeSharedShellApprovalDefaultsToOnceWithoutPattern(t *testing.T) {
	pending := newServePendingApprovalScoped("python build.py", false, true, false, "", "shared_shell")
	prompt := pending.snapshot()
	if len(prompt.Options) != 3 || prompt.Options[0].Choice != "once" {
		t.Fatalf("shared-shell options = %+v, want once, exact, deny", prompt.Options)
	}
	for _, option := range pending.Options {
		if option.Choice == tools.ApprovalChoicePattern || option.SaveToRepo {
			t.Errorf("shared shell offered persistent grant: %+v", option)
		}
	}
}
