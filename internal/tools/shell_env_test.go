package tools

import (
	"github.com/samsaffron/term-llm/internal/procutil"
	"reflect"
	"strings"
	"testing"
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
