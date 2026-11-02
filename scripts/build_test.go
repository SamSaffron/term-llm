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
	text := func(s string) *string { return &s }
	for _, tc := range []struct {
		name, host, target                      string
		authority, override, envOverride        string
		config, homeConfig                      *string
		xdg, configKind                         string
		identity                                string
		inspect, fail, beforeBuild, defaultGoal bool
	}{
		{name: "macOS", identity: "-", inspect: true},
		{name: "plain make", identity: "-", inspect: true, defaultGoal: true},
		{name: "retain macOS signing authority", authority: "Apple Development: Example (TEAM)", identity: "Apple Development: Example (TEAM)", inspect: true},
		{name: "explicit signing authority", authority: "Old Certificate", override: "New Certificate", identity: "New Certificate"},
		{name: "explicit authority with apostrophe", override: "Apple Development: O'Connell (TEAM)", identity: "Apple Development: O'Connell (TEAM)"},
		{name: "explicit authority with shell metacharacters", override: "Apple Development: `exit 1` \"Example\" (TEAM)", identity: "Apple Development: `exit 1` \"Example\" (TEAM)"},
		{name: "explicit ad hoc", authority: "Old Certificate", config: text("Local Certificate"), override: "-", identity: "-"},
		{name: "fresh clone with local identity", config: text("Local Certificate\n"), identity: "Local Certificate"},
		{name: "plain make with local identity", config: text("Local Certificate"), identity: "Local Certificate", defaultGoal: true},
		{name: "local identity beats existing authority", authority: "Old Certificate", config: text("Local Certificate"), identity: "Local Certificate"},
		{name: "absolute XDG beats HOME", config: text("XDG Certificate"), homeConfig: text("Home Certificate"), identity: "XDG Certificate"},
		{name: "unset XDG uses HOME", xdg: "unset", homeConfig: text("Home Certificate"), identity: "Home Certificate"},
		{name: "empty XDG uses HOME", xdg: "empty", homeConfig: text("Home Certificate"), identity: "Home Certificate"},
		{name: "relative XDG ignored", xdg: "relative", config: text("Ignored Certificate"), homeConfig: text("Home Certificate"), identity: "Home Certificate"},
		{name: "absolute XDG does not fall back to HOME", homeConfig: text("Ignored Certificate"), identity: "-", inspect: true},
		{name: "local identity is literal", config: text("Apple Development: O'Connell \"$HOME\" $(exit 1) `exit 1` # \\ (TEAM)"), identity: "Apple Development: O'Connell \"$HOME\" $(exit 1) `exit 1` # \\ (TEAM)"},
		{name: "environment overrides local identity", envOverride: "Environment Certificate", config: text("Local Certificate"), authority: "Old Certificate", identity: "Environment Certificate"},
		{name: "command line overrides environment and local identity", override: "Command Certificate", envOverride: "Environment Certificate", config: text("Local Certificate"), identity: "Command Certificate"},
		{name: "empty override uses local identity", config: text("Local Certificate"), identity: "Local Certificate"},
		{name: "explicit identity ignores malformed local file", override: "Explicit Certificate", config: text("Bad\nCertificate"), identity: "Explicit Certificate"},
		{name: "environment identity ignores malformed local file", envOverride: "Environment Certificate", config: text(""), identity: "Environment Certificate"},
		{name: "explicit identity ignores directory", override: "Explicit Certificate", configKind: "directory", identity: "Explicit Certificate"},
		{name: "empty local identity fails without fallback", config: text(""), authority: "Old Certificate", fail: true, beforeBuild: true},
		{name: "newline only local identity fails", config: text("\n"), fail: true, beforeBuild: true},
		{name: "multiline local identity fails", config: text("First\nSecond\n"), authority: "Old Certificate", fail: true, beforeBuild: true},
		{name: "extra trailing line fails", config: text("Certificate\n\n"), fail: true, beforeBuild: true},
		{name: "carriage return local identity fails", config: text("Bad\rCertificate\n"), fail: true, beforeBuild: true},
		{name: "tab local identity fails", config: text("Bad\tCertificate"), fail: true, beforeBuild: true},
		{name: "NUL local identity fails", config: text("Bad\x00Certificate"), fail: true, beforeBuild: true},
		{name: "DEL local identity fails", config: text("Bad\x7fCertificate"), fail: true, beforeBuild: true},
		{name: "directory local identity fails", configKind: "directory", authority: "Old Certificate", fail: true, beforeBuild: true},
		{name: "broken symlink local identity fails", configKind: "broken symlink", authority: "Old Certificate", fail: true, beforeBuild: true},
		{name: "unreadable local identity fails", config: text("Local Certificate"), configKind: "unreadable", authority: "Old Certificate", fail: true, beforeBuild: true},
		{name: "Linux ignores malformed local identity", host: "linux", target: "linux", config: text("Bad\nCertificate")},
		{name: "macOS cross-build ignores malformed local identity", host: "darwin", target: "linux", config: text("Bad\nCertificate")},
		{name: "Linux cross-build ignores local directory", host: "linux", target: "darwin", configKind: "directory"},
		{name: "signing failure", identity: "-", inspect: true, fail: true},
		{name: "configured certificate failure never falls back", config: text("Missing Certificate"), authority: "Old Certificate", identity: "Missing Certificate", fail: true},
		{name: "local file can explicitly request ad hoc", config: text("-\n"), authority: "Old Certificate", identity: "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.configKind == "unreadable" && os.Geteuid() == 0 {
				t.Skip("root can read mode-000 files")
			}
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			xdg := filepath.Join(dir, "xdg config")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			if tc.xdg == "relative" {
				xdg = "relative-config"
			}
			configRoot := xdg
			if !filepath.IsAbs(configRoot) {
				configRoot = filepath.Join(dir, configRoot)
			}
			configPath := filepath.Join(configRoot, "term-llm", "codesign-identity")
			writeConfig := func(path string, contents *string) {
				t.Helper()
				if contents == nil {
					return
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(*contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			writeConfig(configPath, tc.config)
			writeConfig(filepath.Join(home, ".config", "term-llm", "codesign-identity"), tc.homeConfig)
			switch tc.configKind {
			case "broken symlink":
				if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, "missing-identity"), configPath); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.MkdirAll(configPath, 0700); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if err := os.Chmod(configPath, 0000); err != nil {
					t.Fatal(err)
				}
			}
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
[ "$#" -eq 6 ] || exit 2
printf 'codesign %s\n' "$*" >> "$COMMAND_LOG"
exit "$TEST_SIGN_EXIT"
`)
			exit := "0"
			if tc.fail && !tc.beforeBuild {
				exit = "1"
			}
			host, target := tc.host, tc.target
			if host == "" {
				host, target = "darwin", "darwin"
			}
			args := []string{"-o", "frontend", "VERSION=test", "COMMIT=test", "BUILD_DATE=test"}
			if !tc.defaultGoal {
				args = append(args, "build")
			}
			if tc.override != "" || tc.envOverride == "" {
				args = append(args, "CODESIGN_IDENTITY="+tc.override)
			}
			cmd := exec.Command(makePath, args...)
			cmd.Dir = dir
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				switch key {
				case "HOME", "XDG_CONFIG_HOME", "CODESIGN_IDENTITY", "MAKEFLAGS", "MFLAGS", "GNUMAKEFLAGS", "MAKEOVERRIDES", "MAKELEVEL":
					continue
				}
				cmd.Env = append(cmd.Env, entry)
			}
			cmd.Env = append(cmd.Env, "HOME="+home, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "COMMAND_LOG="+filepath.Join(dir, "commands.log"), "TEST_HOSTOS="+host, "TEST_TARGETOS="+target, "TEST_SIGN_EXIT="+exit, "TEST_SIGN_AUTHORITY="+tc.authority)
			switch tc.xdg {
			case "unset":
			case "empty":
				cmd.Env = append(cmd.Env, "XDG_CONFIG_HOME=")
			default:
				cmd.Env = append(cmd.Env, "XDG_CONFIG_HOME="+xdg)
			}
			if tc.envOverride != "" {
				cmd.Env = append(cmd.Env, "CODESIGN_IDENTITY="+tc.envOverride)
			}
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("build error = %v; want failure %v\n%s", err, tc.fail, out)
			}
			data, err := os.ReadFile(filepath.Join(dir, "commands.log"))
			if err != nil && !(tc.beforeBuild && os.IsNotExist(err)) {
				t.Fatal(err)
			}
			want := ""
			if tc.inspect {
				want += "inspect\n"
			}
			if !tc.beforeBuild {
				want += "build\n"
			}
			if tc.identity != "" {
				want += "codesign --force --sign " + tc.identity + " --identifier com.term-llm.cli ./term-llm\n"
			}
			if string(data) != want {
				t.Fatalf("command log = %q; want %q\n%s", data, want, out)
			}
		})
	}
}
