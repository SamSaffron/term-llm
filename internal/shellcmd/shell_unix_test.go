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
