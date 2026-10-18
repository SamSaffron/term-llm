package main

import (
	"os/exec"
	"testing"
)

func TestDefaultBasePrefersUpstreamThenOriginThenMain(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, args := range [][]string{
		{"init", "-q", "-b", "trunk"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if got := defaultBase(); got != "upstream/main" {
		t.Fatalf("no refs: got %s", got)
	}
	for _, c := range []struct{ ref, want string }{
		{"refs/heads/main", "main"},
		{"refs/remotes/origin/main", "origin/main"},
		{"refs/remotes/upstream/main", "upstream/main"},
	} {
		if out, err := exec.Command("git", "update-ref", c.ref, "HEAD").CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if got := defaultBase(); got != c.want {
			t.Fatalf("got %s want %s", got, c.want)
		}
	}
}
