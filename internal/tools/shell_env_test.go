package tools

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/procutil"
)

func TestShellEnvironmentCommandIsLossless(t *testing.T) {
	overrides := EnvMap{
		"BASH_ENV": "/repo/payload.sh",
		"PATH":     "/repo/bin:$PATH",
		"QUOTED":   "a'b\"c\n\t$HOME; $(payload) && *",
		"EMPTY":    "",
	}
	command := "printf '%s' \"$QUOTED\""
	got := procutil.ShellEnvironmentCommand(command, "/bin/bash", overrides)
	words, err := splitShellWords(got)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"env", "--", "BASH_ENV=/repo/payload.sh", "EMPTY=",
		"PATH=/repo/bin:$PATH", "QUOTED=" + overrides["QUOTED"],
		"/bin/bash", "-c", command,
	}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("approval evidence decoded as %#v, want %#v", words, want)
	}
	if reordered := procutil.ShellEnvironmentCommand(command, "/bin/bash", EnvMap{
		"EMPTY": "", "QUOTED": overrides["QUOTED"], "PATH": overrides["PATH"], "BASH_ENV": overrides["BASH_ENV"],
	}); reordered != got {
		t.Fatalf("map order changed approval identity: %q != %q", reordered, got)
	}
	if plain := procutil.ShellEnvironmentCommand(command, "/bin/bash", nil); plain != command {
		t.Fatalf("no overrides changed ordinary approval: %q", plain)
	}
}

func TestResolveShellEnvironmentSnapshot(t *testing.T) {
	t.Setenv("TERM_LLM_ENV_KEEP", "inherited")
	t.Setenv("TERM_LLM_ENV_REPLACE", "old")
	overrides := EnvMap{"TERM_LLM_ENV_REPLACE": "new", "TERM_LLM_ENV_LITERAL": "$PATH"}
	env, err := procutil.ResolveShellEnvironment(overrides)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_LLM_ENV_KEEP", "changed after snapshot")
	counts := map[string]int{}
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		counts[key]++
		values[key] = value
	}
	for key, want := range map[string]string{
		"TERM_LLM_ENV_KEEP": "inherited", "TERM_LLM_ENV_REPLACE": "new", "TERM_LLM_ENV_LITERAL": "$PATH",
	} {
		if counts[key] != 1 || values[key] != want {
			t.Errorf("snapshot %s = %q (%d entries), want %q once", key, values[key], counts[key], want)
		}
	}
}

func TestBuildShellOptionsEnvironmentIsExactOnly(t *testing.T) {
	command := procutil.ShellEnvironmentCommand("git status", "/bin/bash", EnvMap{"BASH_ENV": "/repo/payload.sh"})
	options := BuildShellOptions(command, &GitRepoInfo{IsRepo: true, RepoName: "repo"})
	for _, option := range options {
		if option.Choice == ApprovalChoicePattern || option.SaveToRepo {
			t.Fatalf("environment approval offered a persistent/pattern grant: %+v", option)
		}
	}
}

func TestShellExportsSessionIDOverInheritedAndModelValues(t *testing.T) {
	t.Setenv(procutil.SessionEnv, "outer-session")
	run := func(ctx context.Context, env EnvMap) string {
		out, err := NewShellTool(nil, nil, DefaultOutputLimits()).Execute(ctx, mustToolArgs(t, ShellArgs{
			Command: `printf '[%s]' "$TERM_LLM_SESSION_ID"`, WorkingDir: t.TempDir(), Env: env,
		}))
		if err != nil {
			t.Fatal(err)
		}
		return out.Content
	}
	ctx := core.ContextWithSessionID(context.Background(), "chat-1")
	if got := run(ctx, nil); !strings.Contains(got, "[chat-1]") {
		t.Fatalf("shell output = %q, want the session's own ID", got)
	}
	if got := run(ctx, EnvMap{procutil.SessionEnv: "spoofed"}); !strings.Contains(got, "[chat-1]") {
		t.Fatalf("shell output = %q, want the session ID to win over a model override", got)
	}
	if got := run(context.Background(), nil); !strings.Contains(got, "[outer-session]") {
		t.Fatalf("shell output = %q, want the inherited value without a session", got)
	}
}

func TestWithSessionReplacesInheritedValue(t *testing.T) {
	env := []string{"A=1", procutil.SessionEnv + "=outer", "B=2"}
	got := procutil.WithSession(env, "inner")
	want := []string{"A=1", "B=2", procutil.SessionEnv + "=inner"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WithSession = %v, want %v", got, want)
	}
	if got := procutil.WithSession(env, ""); !reflect.DeepEqual(got, env) {
		t.Fatalf("WithSession with no session = %v, want unchanged", got)
	}
}
