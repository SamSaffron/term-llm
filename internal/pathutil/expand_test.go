package pathutil

import (
	"os"
	"os/user"
	"strings"
	"testing"
)

func TestExpand_TildeSlash(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	got, err := Expand("~/foo/bar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := home + "/foo/bar"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExpand_BareTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	got, err := Expand("~")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != home {
		t.Errorf("got %q, want %q", got, home)
	}
}

func TestExpand_AbsPath(t *testing.T) {
	got, err := Expand("/absolute/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/absolute/path" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestExpand_RelativePath(t *testing.T) {
	got, err := Expand("relative/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "relative/path" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestExpand_Empty(t *testing.T) {
	got, err := Expand("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestExpand_TildeInMiddle(t *testing.T) {
	got, err := Expand("/some/~/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/some/~/path" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestExpand_TildeUsername(t *testing.T) {
	// Look up the current user by name and verify ~username expands to their home.
	current, err := user.Current()
	if err != nil {
		t.Skip("cannot determine current user")
	}
	got, err := Expand("~" + current.Username)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != current.HomeDir {
		t.Errorf("got %q, want %q", got, current.HomeDir)
	}
}

func TestExpand_TildeUsernameSubpath(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skip("cannot determine current user")
	}
	got, err := Expand("~" + current.Username + "/docs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := current.HomeDir + "/docs"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExpand_TildeUnknownUser(t *testing.T) {
	_, err := Expand("~thisuserdoesnotexist_xyz/path")
	if err == nil {
		t.Fatal("expected error for unknown user, got nil")
	}
	if !strings.Contains(err.Error(), "user not found") {
		t.Errorf("error %q does not mention 'user not found'", err)
	}
}

func TestMustExpand_ReturnsOriginalOnError(t *testing.T) {
	// Unknown user → MustExpand should return the original path unchanged.
	path := "~thisuserdoesnotexist_xyz/path"
	got := MustExpand(path)
	if got != path {
		t.Errorf("MustExpand on error: got %q, want original %q", got, path)
	}
}

func TestMustExpand_TildeSlash(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	got := MustExpand("~/foo")
	want := home + "/foo"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
