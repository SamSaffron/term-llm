package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildAdHocSigning(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make unavailable")
	}
	makefile, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, host, target string
		sign, fail         bool
	}{
		{name: "macOS", host: "darwin", target: "darwin", sign: true},
		{name: "Linux", host: "linux", target: "linux"},
		{name: "macOS cross-build to Linux", host: "darwin", target: "linux"},
		{name: "Linux cross-build to macOS", host: "linux", target: "darwin"},
		{name: "signing failure", host: "darwin", target: "darwin", sign: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "Makefile"), makefile, 0600); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(dir, "go"), `#!/bin/sh
set -eu
case "$1" in
 env)
  case "$2" in
   GOHOSTOS) printf '%s\n' "$TEST_HOSTOS" ;;
   GOOS) printf '%s\n' "$TEST_TARGETOS" ;;
  esac ;;
 build) printf 'build\n' >> "$COMMAND_LOG" ;;
 *) exit 1 ;;
esac
`)
			writeExecutable(t, filepath.Join(dir, "codesign"), `#!/bin/sh
set -eu
printf 'codesign %s\n' "$*" >> "$COMMAND_LOG"
exit "$TEST_SIGN_EXIT"
`)
			exit := "0"
			if tc.fail {
				exit = "1"
			}
			cmd := exec.Command(makePath, "-o", "frontend", "build", "VERSION=test", "COMMIT=test", "BUILD_DATE=test")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "COMMAND_LOG="+filepath.Join(dir, "commands.log"), "TEST_HOSTOS="+tc.host, "TEST_TARGETOS="+tc.target, "TEST_SIGN_EXIT="+exit)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("build error = %v; want failure %v\n%s", err, tc.fail, out)
			}
			data, err := os.ReadFile(filepath.Join(dir, "commands.log"))
			if err != nil {
				t.Fatal(err)
			}
			want := "build\n"
			if tc.sign {
				want += "codesign --force --sign - --identifier com.term-llm.cli ./term-llm\n"
			}
			if string(data) != want {
				t.Fatalf("unexpected command order: %s", strings.TrimSpace(string(data)))
			}
		})
	}
}
