package contain

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeRunner struct {
	name    string
	args    []string
	dir     string
	runs    [][]string
	output  []byte
	outputs [][]byte
}

func (f *fakeRunner) Run(ctx context.Context, name string, args []string, opts RunOptions) error {
	f.name = name
	f.args = append([]string(nil), args...)
	f.runs = append(f.runs, append([]string{name}, args...))
	f.dir = opts.Dir
	return nil
}

func (f *fakeRunner) Output(ctx context.Context, name string, args []string, opts RunOptions) ([]byte, error) {
	f.name = name
	f.args = append([]string(nil), args...)
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
		{"start", func(r *fakeRunner) error { return Start(context.Background(), r, "box", io.Discard, io.Discard) }, append(append([]string{}, base...), "up", "-d", "--build")},
		{"stop", func(r *fakeRunner) error { return Stop(context.Background(), r, "box", io.Discard, io.Discard) }, append(append([]string{}, base...), "stop")},
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
