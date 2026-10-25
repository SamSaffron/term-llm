package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGoTestWrapperPreservesRunAndIsolation(t *testing.T) {
	root := t.TempDir()
	script, err := filepath.Abs("gotest.sh")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "args")
	writeExecutable(t, filepath.Join(root, "go"), `#!/bin/sh
set -eu
if [ "$1" = env ]; then
  printf '%s/cache/%s\n' "$TEST_ROOT" "$2"
  exit 0
fi
# Only the existing checker helper, never user-selected source, is bootstrapped.
[ "$1" = run ] && [ "$2" = ./scripts/checkchanged ] && [ "$3" = --go-test ]
shift 3
printf '%s\n' "$@" > "$TEST_LOG"
[ -z "${GOTEST_RUN:-}" ]
[ "$GOMODCACHE" = "$TEST_ROOT/cache/GOMODCACHE" ]
[ "$GOCACHE" = "$TEST_ROOT/cache/GOCACHE" ]
[ "$HOME" != "$TEST_ROOT" ]
for path in "$HOME" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$XDG_CACHE_HOME" "$TMPDIR"; do
  [ -d "$path" ]
done
printf '%s\n' "$HOME" > "$TEST_ROOT/home"
`)
	pattern := "^TestO'Brien(One|Two)$"
	cmd := exec.Command("bash", script, "-count=1", "./internal/a", "./internal/b")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TEST_ROOT="+root, "TEST_LOG="+log, "HOME="+root, "GOTEST_RUN="+pattern)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper: %v: %s", err, out)
	}
	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-run", pattern, "-count=1", "./internal/a", "./internal/b"}
	if got := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments %q; want %q", got, want)
	}
	home, err := os.ReadFile(filepath.Join(root, "home"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(strings.TrimSpace(string(home))); !os.IsNotExist(err) {
		t.Fatalf("isolated HOME was not cleaned up: %v", err)
	}
}
