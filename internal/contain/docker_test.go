package contain

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeRunner struct {
	name       string
	args       []string
	dir        string
	runs       [][]string
	outputsRun [][]string
	output     []byte
	outputs    [][]byte
	stdins     [][]byte
	runErrs    []error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args []string, opts RunOptions) error {
	f.name = name
	f.args = append([]string(nil), args...)
	f.runs = append(f.runs, append([]string{name}, args...))
	f.dir = opts.Dir
	if opts.Stdin != nil {
		data, err := io.ReadAll(opts.Stdin)
		if err != nil {
			return err
		}
		f.stdins = append(f.stdins, data)
	} else {
		f.stdins = append(f.stdins, nil)
	}
	if len(f.runErrs) > 0 {
		err := f.runErrs[0]
		f.runErrs = f.runErrs[1:]
		return err
	}
	return nil
}

func (f *fakeRunner) Output(ctx context.Context, name string, args []string, opts RunOptions) ([]byte, error) {
	f.name = name
	f.args = append([]string(nil), args...)
	f.outputsRun = append(f.outputsRun, append([]string{name}, args...))
	f.dir = opts.Dir
	if len(f.outputs) > 0 {
		out := f.outputs[0]
		f.outputs = f.outputs[1:]
		return out, nil
	}
	return f.output, nil
}

func writeComposeForDockerTest(t *testing.T, name string, body string) string {
	t.Helper()
	dir, err := ContainerDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "compose.yaml")
	if body == "" {
		body = "services:\n  app:\n    image: alpine\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func clearConsoleEnvForContainTest(t *testing.T) {
	t.Helper()
	for _, name := range consoleEnvNames {
		t.Setenv(name, "")
	}
}

func TestDockerCommands(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clearConsoleEnvForContainTest(t)
	dir := writeComposeForDockerTest(t, "box", `x-term-llm:
  default_service: web
  shell: /bin/bash
services:
  web:
    image: alpine
`)
	compose := filepath.Join(dir, "compose.yaml")
	base := []string{"compose", "-f", compose, "--project-directory", dir, "-p", "term-llm-contain-box"}

	tests := []struct {
		name string
		run  func(*fakeRunner) error
		want []string
	}{
		{"start", func(r *fakeRunner) error { return Start(context.Background(), r, "box", io.Discard, io.Discard) }, append(append([]string{}, base...), "up", "-d")},
		{"stop", func(r *fakeRunner) error { return Stop(context.Background(), r, "box", io.Discard, io.Discard) }, append(append([]string{}, base...), "stop")},
		{"restart", func(r *fakeRunner) error { return Restart(context.Background(), r, "box", io.Discard, io.Discard) }, append(append([]string{}, base...), "restart")},
		{"exec", func(r *fakeRunner) error {
			return Exec(context.Background(), r, "box", []string{"echo", "hi"}, nil, io.Discard, io.Discard)
		}, append(append([]string{}, base...), "exec", "web", "echo", "hi")},
		{"shell", func(r *fakeRunner) error { return Shell(context.Background(), r, "box", nil, io.Discard, io.Discard) }, append(append([]string{}, base...), "exec", "web", "/bin/bash")},
		{"shell-user", func(r *fakeRunner) error {
			return ShellWithOptions(context.Background(), r, "box", ShellOptions{User: "appuser"}, nil, io.Discard, io.Discard)
		}, append(append([]string{}, base...), "exec", "--user", "appuser", "web", "/bin/bash")},
		{"exec-no-command", func(r *fakeRunner) error {
			return Exec(context.Background(), r, "box", nil, nil, io.Discard, io.Discard)
		}, append(append([]string{}, base...), "exec", "web", "/bin/bash")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{}
			if err := tc.run(r); err != nil {
				t.Fatal(err)
			}
			if r.name != "docker" {
				t.Fatalf("name = %q", r.name)
			}
			if !reflect.DeepEqual(r.args, tc.want) {
				t.Fatalf("args = %#v\nwant %#v", r.args, tc.want)
			}
			if r.dir != dir && tc.name != "start" && tc.name != "stop" {
				t.Fatalf("dir = %q, want %q", r.dir, dir)
			}
		})
	}
}

func TestExecAndShellUseServiceDefaultUserLabel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clearConsoleEnvForContainTest(t)
	dir := writeComposeForDockerTest(t, "labelbox", `x-term-llm:
  default_service: web
  shell: /bin/bash
services:
  web:
    image: alpine
    labels:
      org.term-llm.contain.user: appuser
`)
	compose := filepath.Join(dir, "compose.yaml")
	base := []string{"compose", "-f", compose, "--project-directory", dir, "-p", "term-llm-contain-labelbox"}

	r := &fakeRunner{}
	if err := Exec(context.Background(), r, "labelbox", []string{"id", "-un"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := append(append([]string{}, base...), "exec", "--user", "appuser", "web", "id", "-un")
	if !reflect.DeepEqual(r.args, want) {
		t.Fatalf("exec args = %#v\nwant %#v", r.args, want)
	}

	r = &fakeRunner{}
	if err := Shell(context.Background(), r, "labelbox", nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want = append(append([]string{}, base...), "exec", "--user", "appuser", "web", "/bin/bash")
	if !reflect.DeepEqual(r.args, want) {
		t.Fatalf("args = %#v\nwant %#v", r.args, want)
	}

	r = &fakeRunner{}
	if err := ShellWithOptions(context.Background(), r, "labelbox", ShellOptions{User: "root"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want = append(append([]string{}, base...), "exec", "--user", "root", "web", "/bin/bash")
	if !reflect.DeepEqual(r.args, want) {
		t.Fatalf("override args = %#v\nwant %#v", r.args, want)
	}
}

func TestExecAndShellUseWorkspaceForDefaultUserHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clearConsoleEnvForContainTest(t)
	dir := writeComposeForDockerTest(t, "homebox", `x-term-llm:
  default_service: web
  shell: /bin/zsh
  workspace: /home/agent
services:
  web:
    image: alpine
    labels:
      org.term-llm.contain.user: agent
`)
	compose := filepath.Join(dir, "compose.yaml")
	base := []string{"compose", "-f", compose, "--project-directory", dir, "-p", "term-llm-contain-homebox"}

	r := &fakeRunner{}
	if err := Shell(context.Background(), r, "homebox", nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := append(append([]string{}, base...), "exec", "--user", "agent", "--workdir", "/home/agent", "-e", "HOME=/home/agent", "-e", "USER=agent", "-e", "LOGNAME=agent", "web", "/bin/zsh")
	if !reflect.DeepEqual(r.args, want) {
		t.Fatalf("shell args = %#v\nwant %#v", r.args, want)
	}

	r = &fakeRunner{}
	if err := Exec(context.Background(), r, "homebox", []string{"pwd"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want = append(append([]string{}, base...), "exec", "--user", "agent", "--workdir", "/home/agent", "-e", "HOME=/home/agent", "-e", "USER=agent", "-e", "LOGNAME=agent", "web", "pwd")
	if !reflect.DeepEqual(r.args, want) {
		t.Fatalf("exec args = %#v\nwant %#v", r.args, want)
	}
}

func TestExecAndShellForwardConsoleColorEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clearConsoleEnvForContainTest(t)
	t.Setenv("TERM", "xterm-kitty")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("FORCE_COLOR", "3")
	dir := writeComposeForDockerTest(t, "box", `x-term-llm:
  default_service: web
  shell: /bin/zsh
services:
  web:
    image: alpine
`)
	compose := filepath.Join(dir, "compose.yaml")
	base := []string{"compose", "-f", compose, "--project-directory", dir, "-p", "term-llm-contain-box", "exec", "-e", "TERM=xterm-kitty", "-e", "COLORTERM=truecolor", "-e", "FORCE_COLOR=3", "web"}

	r := &fakeRunner{}
	if err := Exec(context.Background(), r, "box", []string{"printf", "hi"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := append(append([]string{}, base...), "printf", "hi")
	if !reflect.DeepEqual(r.args, want) {
		t.Fatalf("exec args = %#v\nwant %#v", r.args, want)
	}

	r = &fakeRunner{}
	if err := Shell(context.Background(), r, "box", nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want = append(append([]string{}, base...), "/bin/zsh")
	if !reflect.DeepEqual(r.args, want) {
		t.Fatalf("shell args = %#v\nwant %#v", r.args, want)
	}
}

func TestPrimarySelectionProxyOptIn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clearConsoleEnvForContainTest(t)
	dir := writeComposeForDockerTest(t, "box", `x-term-llm:
  default_service: web
  shell: /bin/bash
services:
  web:
    image: alpine
`)
	compose := filepath.Join(dir, "compose.yaml")
	base := []string{"compose", "-f", compose, "--project-directory", dir, "-p", "term-llm-contain-box"}

	r := &fakeRunner{outputs: [][]byte{[]byte("container123\n"), []byte("127.0.0.1\n")}}
	if err := Exec(context.Background(), r, "box", []string{"true"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := append(append([]string{}, base...), "exec", "web", "true")
	if !reflect.DeepEqual(r.args, want) {
		t.Fatalf("args without opt-in = %#v\nwant %#v", r.args, want)
	}

	t.Setenv(containPrimarySelectionProxyEnableEnv, "1")
	r = &fakeRunner{outputs: [][]byte{[]byte("container123\n"), []byte("127.0.0.1\n")}}
	if err := Exec(context.Background(), r, "box", []string{"true"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(r.args, "\x00")
	if !strings.Contains(args, containPrimarySelectionURLEnv+"=http://127.0.0.1:") {
		t.Fatalf("proxy URL env not injected when opted in: %#v", r.args)
	}
	if strings.Contains(args, "TERM_LLM_PRIMARY_SELECTION_TOKEN") {
		t.Fatalf("unexpected separate proxy token env in args: %#v", r.args)
	}
}

func TestStartUsesComposeUpRegardlessOfExistingContainers(t *testing.T) {
	for _, output := range []string{"abc123\n", "\n"} {
		t.Run(strings.TrimSpace(output), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			dir := writeComposeForDockerTest(t, "box", "")
			compose := filepath.Join(dir, "compose.yaml")
			base := []string{"docker", "compose", "-f", compose, "--project-directory", dir, "-p", "term-llm-contain-box"}
			runner := &fakeRunner{output: []byte(output)}
			if err := Start(context.Background(), runner, "box", io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			wantOutputs := [][]string{append(append([]string{}, base...), "ps", "--all", "-q")}
			if !reflect.DeepEqual(runner.outputsRun, wantOutputs) {
				t.Fatalf("outputs = %#v\nwant %#v", runner.outputsRun, wantOutputs)
			}
			wantRuns := [][]string{append(append([]string{}, base...), "up", "-d")}
			if !reflect.DeepEqual(runner.runs, wantRuns) {
				t.Fatalf("runs = %#v\nwant %#v", runner.runs, wantRuns)
			}
		})
	}
}

func TestRemoveCommandDownsVolumesAndDeletesWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := writeComposeForDockerTest(t, "box", "")
	compose := filepath.Join(dir, "compose.yaml")
	base := []string{"docker", "compose", "-f", compose, "--project-directory", dir, "-p", "term-llm-contain-box"}
	r := &fakeRunner{}
	if err := Remove(context.Background(), r, "box", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		append(append([]string{}, base...), "down", "--volumes", "--remove-orphans"),
	}
	if !reflect.DeepEqual(r.runs, want) {
		t.Fatalf("runs = %#v\nwant %#v", r.runs, want)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("workspace dir still exists or unexpected stat error: %v", err)
	}
}

func TestRebuildSyncsManagedAgentImageBeforeDocker(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	agentDir, err := ImageDir("agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(agentDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := writeComposeForDockerTest(t, "agentbox", `services:
  app:
    build:
      context: `+agentDir+`
      dockerfile: Dockerfile
`)
	compose := filepath.Join(dir, "compose.yaml")
	base := []string{"docker", "compose", "-f", compose, "--project-directory", dir, "-p", "term-llm-contain-agentbox"}
	r := &fakeRunner{}
	if err := Rebuild(context.Background(), r, "agentbox", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), managedImageMarker) {
		t.Fatalf("managed image was not synced before rebuild; Dockerfile = %s", data)
	}
	want := [][]string{
		append(append([]string{}, base...), "build", "--pull", "--no-cache"),
		append(append([]string{}, base...), "up", "-d", "--force-recreate"),
	}
	if !reflect.DeepEqual(r.runs, want) {
		t.Fatalf("runs = %#v\nwant %#v", r.runs, want)
	}
}

func TestRebuildCommand(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := writeComposeForDockerTest(t, "box", "")
	compose := filepath.Join(dir, "compose.yaml")
	base := []string{"docker", "compose", "-f", compose, "--project-directory", dir, "-p", "term-llm-contain-box"}
	r := &fakeRunner{}
	if err := Rebuild(context.Background(), r, "box", io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		append(append([]string{}, base...), "build", "--pull", "--no-cache"),
		append(append([]string{}, base...), "up", "-d", "--force-recreate"),
	}
	if !reflect.DeepEqual(r.runs, want) {
		t.Fatalf("runs = %#v\nwant %#v", r.runs, want)
	}
}

func TestExecRecipeCopiesFilesBeforeCommand(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tempHome)
	clearConsoleEnvForContainTest(t)

	configDir := filepath.Join(tempHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oauthPayload := []byte(`{"access_token":"abc","refresh_token":"xyz"}`)
	if err := os.WriteFile(filepath.Join(configDir, "chatgpt_oauth.json"), oauthPayload, 0o600); err != nil {
		t.Fatal(err)
	}

	dir := writeComposeForDockerTest(t, "copybox", `x-term-llm:
  default_service: app
  workspace: /home/agent
  exec_recipes:
    seed-chatgpt-auth:
      description: Seed ChatGPT auth
      copy_files:
        - from: "{{config_dir}}/chatgpt_oauth.json"
          to: "{{workspace}}/.config/term-llm/chatgpt_oauth.json"
          mode: "0600"
          missing_hint: Run login first
      post_run_hint: "Restart term-llm contain restart {{name}}"
      command: [true]
services:
  app:
    image: alpine
    labels:
      org.term-llm.contain.user: agent
`)
	compose := filepath.Join(dir, "compose.yaml")
	base := []string{"compose", "-f", compose, "--project-directory", dir, "-p", "term-llm-contain-copybox"}

	r := &fakeRunner{}
	var stderr strings.Builder
	if err := Exec(context.Background(), r, "copybox", []string{"seed-chatgpt-auth"}, strings.NewReader("final stdin"), io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}

	if len(r.runs) != 2 {
		t.Fatalf("runs = %#v, want copy then final exec", r.runs)
	}
	copyRun := r.runs[0]
	wantCopyHead := append(append([]string{"docker"}, base...), "exec", "-T", "--user", "agent", "--workdir", "/home/agent", "-e", "HOME=/home/agent", "-e", "USER=agent", "-e", "LOGNAME=agent", "app", "sh", "-c")
	if len(copyRun) < len(wantCopyHead)+4 {
		t.Fatalf("copy run too short: %#v", copyRun)
	}
	for i, want := range wantCopyHead {
		if copyRun[i] != want {
			t.Fatalf("copyRun[%d] = %q, want %q (full: %#v)", i, copyRun[i], want, copyRun)
		}
	}
	copyTail := copyRun[len(wantCopyHead)+1:]
	if script := copyRun[len(wantCopyHead)]; !strings.Contains(script, "mktemp") || !strings.Contains(script, "mv -f \"$tmp\" \"$target\"") || strings.Contains(script, "cat > \"$target\"") {
		t.Fatalf("copy script should write atomically via temp file, got:\n%s", script)
	}
	wantTail := []string{"term-llm-copy-files", "/home/agent/.config/term-llm/chatgpt_oauth.json", "0600"}
	if !reflect.DeepEqual(copyTail, wantTail) {
		t.Fatalf("copy tail = %#v, want %#v (full: %#v)", copyTail, wantTail, copyRun)
	}
	finalRun := r.runs[1]
	wantFinal := append(append([]string{"docker"}, base...), "exec", "--user", "agent", "--workdir", "/home/agent", "-e", "HOME=/home/agent", "-e", "USER=agent", "-e", "LOGNAME=agent", "app", "true")
	if !reflect.DeepEqual(finalRun, wantFinal) {
		t.Fatalf("final run = %#v\nwant %#v", finalRun, wantFinal)
	}
	if len(r.stdins) != 2 || string(r.stdins[0]) != string(oauthPayload) || string(r.stdins[1]) != "final stdin" {
		t.Fatalf("stdins = %#v", r.stdins)
	}
	if !strings.Contains(stderr.String(), "Restart term-llm contain restart copybox") {
		t.Fatalf("stderr missing post-run hint: %q", stderr.String())
	}
}

func TestExecRecipeCopyFilesMissingSourceIncludesHint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clearConsoleEnvForContainTest(t)
	writeComposeForDockerTest(t, "missingcopy", `x-term-llm:
  default_service: app
  workspace: /home/agent
  exec_recipes:
    seed-chatgpt-auth:
      copy_files:
        - from: "{{config_dir}}/chatgpt_oauth.json"
          to: "{{workspace}}/.config/term-llm/chatgpt_oauth.json"
          missing_hint: Run term-llm auth login chatgpt first
      command: [true]
services:
  app:
    image: alpine
`)

	r := &fakeRunner{}
	err := Exec(context.Background(), r, "missingcopy", []string{"seed-chatgpt-auth"}, nil, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected missing source error")
	}
	if !strings.Contains(err.Error(), "chatgpt_oauth.json") || !strings.Contains(err.Error(), "Run term-llm auth login chatgpt first") {
		t.Fatalf("error = %v", err)
	}
	if len(r.runs) != 0 {
		t.Fatalf("expected no docker runs, got %#v", r.runs)
	}
}

func TestExecRecipeCopyOnlyNoCommandUsesTrue(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tempHome)
	clearConsoleEnvForContainTest(t)
	configDir := filepath.Join(tempHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "copilot_oauth.json"), []byte(`{"access_token":"abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := writeComposeForDockerTest(t, "copyonly", `x-term-llm:
  default_service: app
  workspace: /home/agent
  exec_recipes:
    seed-copilot-auth:
      copy_files:
        - from: "{{config_dir}}/copilot_oauth.json"
          to: "{{workspace}}/.config/term-llm/copilot_oauth.json"
services:
  app:
    image: alpine
`)
	compose := filepath.Join(dir, "compose.yaml")
	base := []string{"docker", "compose", "-f", compose, "--project-directory", dir, "-p", "term-llm-contain-copyonly"}
	r := &fakeRunner{}
	if err := Exec(context.Background(), r, "copyonly", []string{"seed-copilot-auth"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(r.runs) != 2 {
		t.Fatalf("runs = %#v, want copy plus true command", r.runs)
	}
	wantFinal := append(append([]string{}, base...), "exec", "app", "true")
	if !reflect.DeepEqual(r.runs[1], wantFinal) {
		t.Fatalf("final run = %#v\nwant %#v", r.runs[1], wantFinal)
	}
}

func TestExecRecipeCopyFilesRejectsInvalidModeAndRelativeDestination(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tempHome)
	clearConsoleEnvForContainTest(t)
	configDir := filepath.Join(tempHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "chatgpt_oauth.json"), []byte(`{"access_token":"abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		copyYML string
		want    string
	}{
		{
			name: "badmode",
			copyYML: `        - from: "{{config_dir}}/chatgpt_oauth.json"
          to: "{{workspace}}/.config/term-llm/chatgpt_oauth.json"
          mode: "abc"
`,
			want: "invalid mode",
		},
		{
			name: "relative",
			copyYML: `        - from: "{{config_dir}}/chatgpt_oauth.json"
          to: ".config/term-llm/chatgpt_oauth.json"
`,
			want: "absolute container path",
		},
		{
			name: "missingto",
			copyYML: `        - from: "{{config_dir}}/chatgpt_oauth.json"
`,
			want: "missing to path",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			writeComposeForDockerTest(t, tc.name, `x-term-llm:
  default_service: app
  workspace: /home/agent
  exec_recipes:
    seed-chatgpt-auth:
      copy_files:
`+tc.copyYML+`      command: [true]
services:
  app:
    image: alpine
`)
			r := &fakeRunner{}
			err := Exec(context.Background(), r, tc.name, []string{"seed-chatgpt-auth"}, nil, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
			if len(r.runs) != 0 {
				t.Fatalf("expected no docker runs, got %#v", r.runs)
			}
		})
	}
}

func TestExecRecipePostRunHintNotPrintedWhenFinalCommandFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	clearConsoleEnvForContainTest(t)
	writeComposeForDockerTest(t, "failhint", `x-term-llm:
  default_service: app
  workspace: /home/agent
  exec_recipes:
    fail:
      post_run_hint: Do not print me
      command: [false]
services:
  app:
    image: alpine
`)
	r := &fakeRunner{runErrs: []error{errors.New("boom")}}
	var stderr strings.Builder
	err := Exec(context.Background(), r, "failhint", []string{"fail"}, nil, io.Discard, &stderr)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(stderr.String(), "Do not print me") {
		t.Fatalf("post-run hint printed after failure: %q", stderr.String())
	}
}
