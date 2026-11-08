//go:build !windows

package shellcmd

import (
	"reflect"
	"testing"
)

func TestArgsUsesPosixCommandFlag(t *testing.T) {
	if got, want := Args("/bin/bash", "echo hi"), []string{"-c", "echo hi"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Args = %v, want %v", got, want)
	}
}

func TestPathHonorsExplicitShellAndDefaultsToBash(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	if got := Path(); got != "/bin/zsh" {
		t.Fatalf("Path = %q", got)
	}
	t.Setenv("SHELL", "")
	if got := Path(); got != "bash" {
		t.Fatalf("Path fallback = %q", got)
	}
}

func TestHookInvocationAlwaysUsesPOSIXSh(t *testing.T) {
	t.Setenv("SHELL", "/bin/fish")
	gotShell, gotArgs := HookInvocation("printf ok")
	if want := "sh"; gotShell != want {
		t.Fatalf("HookInvocation shell = %q, want %q", gotShell, want)
	}
	if want := []string{"-c", "printf ok"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("HookInvocation args = %v, want %v", gotArgs, want)
	}
}

func TestInteractivePathPreservesShFallbackAndTrimsShell(t *testing.T) {
	t.Setenv("SHELL", " /bin/fish ")
	if got := InteractivePath(); got != "/bin/fish" {
		t.Fatalf("InteractivePath = %q, want /bin/fish", got)
	}
	t.Setenv("SHELL", "")
	if got := InteractivePath(); got != "sh" {
		t.Fatalf("InteractivePath fallback = %q, want sh", got)
	}
}
