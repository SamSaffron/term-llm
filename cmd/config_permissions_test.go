//go:build unix

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestConfigCommandsPrivateWrites(t *testing.T) {
	if os.Getenv("TERM_LLM_CONFIG_COMMAND_PERMISSIONS_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestConfigCommandsPrivateWrites$", "-test.v")
		cmd.Env = append(os.Environ(), "TERM_LLM_CONFIG_COMMAND_PERMISSIONS_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("permissions subprocess: %v\n%s", err, out)
		}
		return
	}
	// A dedicated process avoids changing the umask of unrelated tests.
	syscall.Umask(0)
	for _, writer := range []struct {
		name  string
		run   func() error
		want  string
		reset bool
	}{
		{"set", func() error { return configSet(nil, []string{"providers.test.api_key", "literal-test-key"}) }, "api_key: literal-test-key", false},
		{"debug enable", func() error { return setDebugLogEnabled(true) }, "enabled: true", false},
		{"debug disable", func() error { return setDebugLogEnabled(false) }, "enabled: false", false},
		{"theme", func() error { return saveThemeToConfig(ui.ThemeConfig{Primary: "#123456"}) }, "#123456", false},
		{"reset", func() error { return configReset(nil, nil) }, "# term-llm configuration", true},
	} {
		for _, state := range []string{"new", "public", "symlink", "hardlink"} {
			t.Run(writer.name+"/"+state, func(t *testing.T) {
				path, target := seedPrivateCommandConfig(t, state)
				if err := writer.run(); err != nil {
					t.Fatal(err)
				}
				assertPrivateCommandConfig(t, path, target, state, writer.want, !writer.reset && state != "new")
			})
		}
	}
}

const privateCommandConfig = "# keep config comment\nproviders:\n  test:\n    api_key: literal-test-key # keep key comment\n"

func seedPrivateCommandConfig(t *testing.T, state string) (string, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	path, err := config.GetConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	target := path
	if state == "new" {
		return path, target
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if state == "symlink" {
		target = filepath.Join(root, "dotfile.yaml")
		if err := os.Symlink("../dotfile.yaml", path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(target, []byte(privateCommandConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if state == "hardlink" {
		if err := os.Link(target, filepath.Join(root, "unrelated.yaml")); err != nil {
			t.Fatal(err)
		}
	}
	return path, target
}

func assertPrivateCommandConfig(t *testing.T, path, target, state, want string, comments bool) {
	t.Helper()
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %04o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Errorf("saved config missing %q", want)
	}
	if comments {
		for _, text := range []string{"# keep config comment", "# keep key comment", "api_key: literal-test-key"} {
			if !strings.Contains(string(data), text) {
				t.Errorf("saved config missing %q", text)
			}
		}
	}
	if state == "symlink" {
		if link, err := os.Readlink(path); err != nil || link != "../dotfile.yaml" {
			t.Errorf("config symlink changed: %q, %v", link, err)
		}
	}
	if state == "hardlink" {
		unrelated := filepath.Join(filepath.Dir(filepath.Dir(path)), "unrelated.yaml")
		info, err := os.Stat(unrelated)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(unrelated)
		if err != nil || string(data) != privateCommandConfig || info.Mode().Perm() != 0o644 {
			t.Errorf("unrelated hardlink modified: mode %o, read error %v", info.Mode().Perm(), err)
		}
	}
}

func TestConfigEditPrivatePermissions(t *testing.T) {
	for _, state := range []string{"new", "public", "symlink", "hardlink"} {
		t.Run(state, func(t *testing.T) {
			path, target := seedPrivateCommandConfig(t, state)
			// The editor verifies entry hardening, then simulates an atomic save
			// that creates a public file. Exit hardening must repair it too.
			editor := filepath.Join(t.TempDir(), "editor")
			if err := os.WriteFile(editor, []byte("#!/bin/sh\nexec \"$CONFIG_EDITOR_TEST_BINARY\" -test.run=^TestConfigEditorHelper$ -- \"$1\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("EDITOR", editor)
			t.Setenv("CONFIG_EDITOR_TEST_BINARY", os.Args[0])
			t.Setenv("CONFIG_EDITOR_TARGET", target)
			t.Setenv("GO_WANT_CONFIG_EDITOR_HELPER", "1")
			if err := configEdit(nil, nil); err != nil {
				t.Fatal(err)
			}
			assertPrivateCommandConfig(t, path, target, state, "# editor saved", state != "new")
		})
	}
}

// The editor wrapper runs only this helper in a separate test process.
func TestConfigEditorHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CONFIG_EDITOR_HELPER") != "1" {
		return
	}
	path := os.Args[len(os.Args)-1]
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		os.Exit(21)
	}
	target := os.Getenv("CONFIG_EDITOR_TARGET")
	data, err := os.ReadFile(path)
	if err != nil {
		os.Exit(22)
	}
	if err := os.WriteFile(target+".saved", append(data, []byte("\n# editor saved\n")...), 0o644); err != nil {
		os.Exit(23)
	}
	if err := os.Chmod(target+".saved", 0o644); err != nil {
		os.Exit(24)
	}
	if err := os.Rename(target+".saved", target); err != nil {
		os.Exit(25)
	}
	os.Exit(0)
}

func TestConfigEditPreservesEditorFailure(t *testing.T) {
	_, target := seedPrivateCommandConfig(t, "public")
	editor := filepath.Join(t.TempDir(), "editor")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nchmod 644 \"$1\"\nexit 17\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", editor)
	err := configEdit(nil, nil)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 {
		t.Fatalf("editor result = %v, want exit 17", err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("failed editor left config public: %v", err)
	}
}
