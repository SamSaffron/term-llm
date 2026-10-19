package serve

import (
	"fmt"

	"os"
	"os/exec"
	"path/filepath"

	"testing"

	"github.com/samsaffron/term-llm/internal/testutil/procenv"

	"github.com/samsaffron/term-llm/internal/testutil/mcptest"
)

var bindingTestRepoTemplate string

func TestMain(m *testing.M) {
	mcptest.RunIfChild()
	cleanup, err := procenv.Setup()
	if err != nil {
		os.Exit(1)
	}
	defer cleanup()
	dataHome := os.Getenv("XDG_DATA_HOME")
	bindingTestRepoTemplate = filepath.Join(dataHome, "binding-repo-template")
	if err := os.MkdirAll(bindingTestRepoTemplate, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create git template: %v\n", err)
		os.Exit(1)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "file.txt"}, {"commit", "-q", "-m", "init"}} {
		if args[0] == "add" {
			if err := os.WriteFile(filepath.Join(bindingTestRepoTemplate, "file.txt"), []byte("base\n"), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "write git template file: %v\n", err)
				os.Exit(1)
			}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = bindingTestRepoTemplate
		cmd.Env = bindingTestGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "initialize git template with git %v: %v\n%s", args, err, out)
			os.Exit(1)
		}
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
