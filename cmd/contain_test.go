package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/contain"
	"github.com/spf13/cobra"
)

type containFakeRunner struct {
	output []byte
	calls  []string
}

func (f *containFakeRunner) Run(ctx context.Context, name string, args []string, opts contain.RunOptions) error {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return nil
}

func (f *containFakeRunner) Output(ctx context.Context, name string, args []string, opts contain.RunOptions) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return f.output, nil
}

func executeRootForContainTest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	return executeRootForContainTestWithInput(t, "", args...)
}

func executeRootForContainTestWithInput(t *testing.T, input string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetIn(strings.NewReader(input))
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestContainCommandExists(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"contain"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd == nil || cmd.Name() != "contain" {
		t.Fatalf("contain command not found: %#v", cmd)
	}
}

func TestContainNewMissingNameError(t *testing.T) {
	_, _, err := executeRootForContainTest(t, "contain", "new")
	if err == nil {
		t.Fatal("contain new without a name succeeded")
	}
	if !strings.Contains(err.Error(), "expecting workspace name") || !strings.Contains(err.Error(), "term-llm contain new <name>") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestContainNewCreatesFiles(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	containNewTemplate = "basic"
	if err := containNewCmd.Flags().Set("template", "basic"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeRootForContainTest(t, "contain", "new", "cmdbox")
	if err != nil {
		t.Fatalf("contain new error = %v stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "Created contain workspace") {
		t.Fatalf("stdout = %q", stdout)
	}
	path, err := contain.ComposePath("cmdbox")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestContainNewDefaultTemplateIsAgent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	containNewTemplate = "agent"
	if err := containNewCmd.Flags().Set("template", "agent"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeRootForContainTest(t, "contain", "new", "defaultagent", "--no-input", "--set", "provider=skip", "--set", "web_port=8282")
	if err != nil {
		t.Fatalf("contain new default agent error = %v stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "Web UI: http://localhost:8282/chat") || !strings.Contains(stdout, "Web UI bearer token:") {
		t.Fatalf("stdout missing default agent web UI info/token: %q", stdout)
	}
	composePath, err := contain.ComposePath("defaultagent")
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := contain.AgentImageHash()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "term-llm-agent:${AGENT_DISTRO:-arch}-defaultagent-"+hash) {
		t.Fatalf("default template did not render agent compose with image hash %q: %s", hash, compose)
	}
}

func TestContainNewStartPromptDefaultYesStartsWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := contain.CreateWorkspace("promptbox", contain.CreateOptions{Template: "basic", CWD: t.TempDir()}); err != nil {
		t.Fatal(err)
	}

	oldRunner := containRunner
	oldIsTerminal := containInputIsTerminal
	oldNoInput := containNewNoInput
	r := &containFakeRunner{}
	containRunner = r
	containInputIsTerminal = func(input any) bool { return true }
	containNewNoInput = false
	t.Cleanup(func() {
		containRunner = oldRunner
		containInputIsTerminal = oldIsTerminal
		containNewNoInput = oldNoInput
	})

	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetIn(strings.NewReader("\n"))
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	started, err := promptAndMaybeStartContainWorkspace(cmd, "promptbox")
	if err != nil {
		t.Fatalf("promptAndMaybeStartContainWorkspace error = %v stderr=%s", err, stderr.String())
	}
	if !started {
		t.Fatal("expected default empty answer to start workspace")
	}
	if len(r.calls) != 1 || !strings.Contains(r.calls[0], "up -d --build") {
		t.Fatalf("runner calls = %#v", r.calls)
	}
	if !strings.Contains(stdout.String(), "Start container now? [Y/n]") || !strings.Contains(stdout.String(), "Starting contain workspace") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestContainNewStartPromptNoSkipsStart(t *testing.T) {
	oldRunner := containRunner
	oldIsTerminal := containInputIsTerminal
	oldNoInput := containNewNoInput
	r := &containFakeRunner{}
	containRunner = r
	containInputIsTerminal = func(input any) bool { return true }
	containNewNoInput = false
	t.Cleanup(func() {
		containRunner = oldRunner
		containInputIsTerminal = oldIsTerminal
		containNewNoInput = oldNoInput
	})

	cmd := &cobra.Command{}
	var stdout bytes.Buffer
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetOut(&stdout)

	started, err := promptAndMaybeStartContainWorkspace(cmd, "promptbox")
	if err != nil {
		t.Fatalf("promptAndMaybeStartContainWorkspace error = %v", err)
	}
	if started {
		t.Fatal("expected n answer to skip start")
	}
	if len(r.calls) != 0 {
		t.Fatalf("runner should not be called: %#v", r.calls)
	}
	if !strings.Contains(stdout.String(), "Start container now? [Y/n]") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestContainWorkspaceNameCompletion(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := contain.CreateWorkspace("alpha", contain.CreateOptions{Template: "basic", CWD: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, err := contain.CreateWorkspace("beta", contain.CreateOptions{Template: "basic", CWD: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	comps, directive := containWorkspaceNameCompletion(containStartCmd, nil, "a")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("directive = %v", directive)
	}
	joined := strings.Join(comps, "\n")
	if !strings.Contains(joined, "alpha") || strings.Contains(joined, "beta") {
		t.Fatalf("completions = %#v", comps)
	}
	if strings.Contains(joined, "missing") {
		t.Fatalf("completion descriptions should not expose Docker reconciliation status: %#v", comps)
	}
	if !strings.Contains(joined, "service: app") {
		t.Fatalf("completion descriptions should describe the workspace service: %#v", comps)
	}
}

func TestContainTemplateFlagCompletion(t *testing.T) {
	comps, directive := containTemplateFlagCompletion(containNewCmd, nil, "a")
	if directive != cobra.ShellCompDirectiveDefault {
		t.Fatalf("directive = %v", directive)
	}
	joined := strings.Join(comps, "\n")
	if !strings.Contains(joined, "agent") || strings.Contains(joined, "basic") {
		t.Fatalf("completions = %#v", comps)
	}
}

func TestContainShellUserFlag(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clearContainShellUserFlagForTest(t)
	if _, err := contain.CreateWorkspace("userbox", contain.CreateOptions{Template: "basic", CWD: t.TempDir()}); err != nil {
		t.Fatal(err)
	}

	oldRunner := containRunner
	r := &containFakeRunner{}
	containRunner = r
	t.Cleanup(func() { containRunner = oldRunner })

	_, stderr, err := executeRootForContainTest(t, "contain", "shell", "--user", "appuser", "userbox")
	if err != nil {
		t.Fatalf("contain shell --user error = %v stderr=%s", err, stderr)
	}
	if len(r.calls) != 1 {
		t.Fatalf("runner calls = %#v", r.calls)
	}
	call := r.calls[0]
	if !strings.Contains(call, " exec --user appuser ") || !strings.Contains(call, " app /bin/sh") {
		t.Fatalf("runner call missing --user before service: %#v", r.calls)
	}
}

func clearContainShellUserFlagForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		containShellUser = ""
		if err := containShellCmd.Flags().Set("user", ""); err != nil {
			t.Fatal(err)
		}
	}
	reset()
	t.Cleanup(reset)
}

func TestContainTemplatesCommand(t *testing.T) {
	stdout, stderr, err := executeRootForContainTest(t, "contain", "templates")
	if err != nil {
		t.Fatalf("contain templates error = %v stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "agent") || !strings.Contains(stdout, "basic") || strings.Contains(stdout, "term-llm\t") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestContainNewAgentTemplate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	containNewTemplate = "basic"
	if err := containNewCmd.Flags().Set("template", "basic"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeRootForContainTest(t, "contain", "new", "agentbox", "--template", "agent", "--no-input", "--set", "provider=skip", "--set", "web_port=8181")
	if err != nil {
		t.Fatalf("contain new --template agent error = %v stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "Web UI: http://localhost:8181/chat") || !strings.Contains(stdout, "Web UI bearer token:") {
		t.Fatalf("stdout missing web UI info/token: %q", stdout)
	}
	composePath, err := contain.ComposePath("agentbox")
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := contain.AgentImageHash()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "term-llm-agent:${AGENT_DISTRO:-arch}-agentbox-"+hash) {
		t.Fatalf("compose missing agent image tag with hash %q:\n%s", hash, compose)
	}
	envData, err := os.ReadFile(filepath.Join(filepath.Dir(composePath), ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envData), "WEB_PORT=8181") || strings.Contains(string(envData), "{{") {
		t.Fatalf("env = %s", envData)
	}
	imageDockerfile, err := contain.AgentImageDockerfilePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(imageDockerfile); err != nil {
		t.Fatalf("agent image was not synced: %v", err)
	}
}

func TestContainNewExternalFileAndDir(t *testing.T) {
	for _, tc := range []struct {
		name      string
		makeTmpl  func(t *testing.T) string
		workspace string
	}{
		{
			name: "file",
			makeTmpl: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "compose-template.yaml")
				if err := os.WriteFile(path, []byte("services:\n  app:\n    image: alpine\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
			workspace: "filebox",
		},
		{
			name: "dir",
			makeTmpl: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  app:\n    image: alpine\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			workspace: "dirbox",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			tmpl := tc.makeTmpl(t)
			containNewTemplate = "basic"
			if err := containNewCmd.Flags().Set("template", "basic"); err != nil {
				t.Fatal(err)
			}
			_, stderr, err := executeRootForContainTest(t, "contain", "new", tc.workspace, "--template", tmpl)
			if err != nil {
				t.Fatalf("contain new error = %v stderr=%s", err, stderr)
			}
			path, err := contain.ComposePath(tc.workspace)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContainExecResolvesRecipeAndAllowsRawOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := contain.ContainerDir("recipebox")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(dir, "compose.yaml")
	compose := `x-term-llm:
  default_service: app
  workspace: /home/agent
  exec_recipes:
    agent:
      description: Chat with agent
      command:
        - term-llm
        - chat
        - "@recipebox"
        - --yolo
services:
  app:
    image: alpine
    labels:
      org.term-llm.contain.user: agent
`
	if err := os.WriteFile(composePath, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}

	old := containRunner
	r := &containFakeRunner{output: []byte("\n")}
	containRunner = r
	t.Cleanup(func() { containRunner = old })

	if _, stderr, err := executeRootForContainTest(t, "contain", "exec", "recipebox", "agent", "hello"); err != nil {
		t.Fatalf("exec recipe error = %v stderr=%s", err, stderr)
	}
	if _, stderr, err := executeRootForContainTest(t, "contain", "exec", "recipebox", "--", "agent", "hello"); err != nil {
		t.Fatalf("exec raw override error = %v stderr=%s", err, stderr)
	}
	if len(r.calls) != 2 {
		t.Fatalf("runner calls = %#v", r.calls)
	}
	if !strings.Contains(r.calls[0], " app term-llm chat @recipebox --yolo hello") {
		t.Fatalf("recipe call = %q", r.calls[0])
	}
	if !strings.Contains(r.calls[1], " app agent hello") || strings.Contains(r.calls[1], "term-llm chat") {
		t.Fatalf("raw override call = %q", r.calls[1])
	}
}

func TestContainNewAgentPrintsNextStepsLastWithWebUI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	containNewTemplate = "agent"
	if err := containNewCmd.Flags().Set("template", "agent"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeRootForContainTest(t, "contain", "new", "tipbox", "--no-input", "--set", "provider=skip", "--set", "web_port=8383")
	if err != nil {
		t.Fatalf("contain new error = %v stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "Chat with agent: term-llm contain exec tipbox agent") {
		t.Fatalf("stdout missing recipe tip: %q", stdout)
	}
	if !strings.Contains(stdout, "Force raw command if a recipe collides: term-llm contain exec tipbox -- <cmd...>") {
		t.Fatalf("stdout missing raw override tip: %q", stdout)
	}
	if !strings.Contains(stdout, "Web UI: http://localhost:8383/chat") || !strings.Contains(stdout, "Web UI bearer token:") {
		t.Fatalf("stdout missing web UI tip: %q", stdout)
	}
	last := strings.TrimSpace(stdout)
	if !strings.Contains(last[strings.LastIndex(last, "Next steps:"):], "Web UI bearer token:") {
		t.Fatalf("next-step tip should include final web UI token, got: %q", stdout)
	}
}

func TestContainImageSyncCommand(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	containImageSyncForce = false
	if err := containImageSyncCmd.Flags().Set("force", "false"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeRootForContainTest(t, "contain", "image", "sync")
	if err != nil {
		t.Fatalf("contain image sync error = %v stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "Synced contain image \"agent\"") {
		t.Fatalf("stdout = %q", stdout)
	}
	path, err := contain.AgentImageDockerfilePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestContainRmRequiresExactYES(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := contain.ContainerDir("box")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  app:\n    image: alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := containRunner
	r := &containFakeRunner{output: []byte("\n")}
	containRunner = r
	t.Cleanup(func() { containRunner = old })

	_, stderr, err := executeRootForContainTestWithInput(t, "yes\n", "contain", "rm", "box")
	if err == nil {
		t.Fatal("contain rm accepted non-exact confirmation")
	}
	if !strings.Contains(err.Error(), "confirmation did not match YES") {
		t.Fatalf("error = %q", err.Error())
	}
	if !strings.Contains(stderr, "WARNING: destructive contain removal") || !strings.Contains(stderr, "Type YES") {
		t.Fatalf("stderr = %q", stderr)
	}
	if len(r.calls) != 0 {
		t.Fatalf("runner was called despite failed confirmation: %#v", r.calls)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("workspace dir should remain after failed confirmation: %v", err)
	}
}

func TestContainRmRunsComposeDownAndDeletesWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := contain.ContainerDir("box")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(composePath, []byte("services:\n  app:\n    image: alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := containRunner
	r := &containFakeRunner{output: []byte("\n")}
	containRunner = r
	t.Cleanup(func() { containRunner = old })

	_, stderr, err := executeRootForContainTestWithInput(t, "YES\n", "contain", "rm", "box")
	if err != nil {
		t.Fatalf("contain rm error = %v stderr=%s", err, stderr)
	}
	if len(r.calls) != 1 {
		t.Fatalf("runner calls = %#v", r.calls)
	}
	call := r.calls[0]
	for _, want := range []string{"docker compose", "down --volumes --remove-orphans", "-f " + composePath, "-p term-llm-contain-box"} {
		if !strings.Contains(call, want) {
			t.Fatalf("runner call = %q, want contains %q", call, want)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("workspace dir still exists or unexpected stat error: %v", err)
	}
}

func TestContainDockerCommandsUseFakeRunner(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := contain.ContainerDir("box")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  app:\n    image: alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := containRunner
	r := &containFakeRunner{output: []byte("\n")}
	containRunner = r
	t.Cleanup(func() { containRunner = old })

	if _, stderr, err := executeRootForContainTest(t, "contain", "start", "box"); err != nil {
		t.Fatalf("start error = %v stderr=%s", err, stderr)
	}
	if _, stderr, err := executeRootForContainTest(t, "contain", "rebuild", "box"); err != nil {
		t.Fatalf("rebuild error = %v stderr=%s", err, stderr)
	}
	if _, stderr, err := executeRootForContainTest(t, "contain", "exec", "box", "echo", "hi"); err != nil {
		t.Fatalf("exec error = %v stderr=%s", err, stderr)
	}
	if _, stderr, err := executeRootForContainTest(t, "contain", "shell", "box"); err != nil {
		t.Fatalf("shell error = %v stderr=%s", err, stderr)
	}
	stdout, stderr, err := executeRootForContainTest(t, "contain", "ls")
	if err != nil {
		t.Fatalf("ls error = %v stderr=%s", err, stderr)
	}
	if !strings.Contains(stdout, "NAME") || !strings.Contains(stdout, "box") {
		t.Fatalf("ls stdout = %q", stdout)
	}
	if len(r.calls) < 4 {
		t.Fatalf("fake runner calls = %#v", r.calls)
	}
}
