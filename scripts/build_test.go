package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSigning(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make unavailable")
	}
	makefile, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, host, target  string
		authority, override string
		sign, fail          bool
	}{
		{name: "macOS", host: "darwin", target: "darwin", sign: true},
		{name: "retain macOS signing authority", host: "darwin", target: "darwin", sign: true, authority: "Apple Development: Example (TEAM)"},
		{name: "explicit signing authority", host: "darwin", target: "darwin", sign: true, authority: "Old Certificate", override: "New Certificate"},
		{name: "explicit authority with apostrophe", host: "darwin", target: "darwin", sign: true, override: "Apple Development: O'Connell (TEAM)"},
		{name: "explicit authority with shell metacharacters", host: "darwin", target: "darwin", sign: true, override: "Apple Development: `exit 1` \"Example\" (TEAM)"},
		{name: "explicit ad hoc", host: "darwin", target: "darwin", sign: true, authority: "Old Certificate", override: "-"},
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
if [ "$1" = "-dvv" ]; then
 printf 'inspect\n' >> "$COMMAND_LOG"
 if [ -n "$TEST_SIGN_AUTHORITY" ]; then
  printf 'Authority=%s\nAuthority=Root Certificate\n' "$TEST_SIGN_AUTHORITY" >&2
  exit 0
 fi
 exit 1
fi
printf 'codesign %s\n' "$*" >> "$COMMAND_LOG"
exit "$TEST_SIGN_EXIT"
`)
			exit := "0"
			if tc.fail {
				exit = "1"
			}
			cmd := exec.Command(makePath, "-o", "frontend", "build", "VERSION=test", "COMMIT=test", "BUILD_DATE=test", "CODESIGN_IDENTITY="+tc.override)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "COMMAND_LOG="+filepath.Join(dir, "commands.log"), "TEST_HOSTOS="+tc.host, "TEST_TARGETOS="+tc.target, "TEST_SIGN_EXIT="+exit, "TEST_SIGN_AUTHORITY="+tc.authority)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("build error = %v; want failure %v\n%s", err, tc.fail, out)
			}
			data, err := os.ReadFile(filepath.Join(dir, "commands.log"))
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.sign && tc.override == "" {
				want += "inspect\n"
			}
			want += "build\n"
			if tc.sign {
				identity := tc.override
				if identity == "" {
					identity = tc.authority
				}
				if identity == "" {
					identity = "-"
				}
				want += "codesign --force --sign " + identity + " --identifier com.term-llm.cli ./term-llm\n"
			}
			if string(data) != want {
				t.Fatalf("unexpected command order: %s", strings.TrimSpace(string(data)))
			}
		})
	}
}
