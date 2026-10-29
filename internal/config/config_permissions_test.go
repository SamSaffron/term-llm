//go:build unix

package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestConfigSavePrivatePermissions(t *testing.T) {
	if os.Getenv("TERM_LLM_CONFIG_PERMISSIONS_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestConfigSavePrivatePermissions$", "-test.v")
		cmd.Env = append(os.Environ(), "TERM_LLM_CONFIG_PERMISSIONS_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("permissions subprocess: %v\n%s", err, out)
		}
		return
	}
	// This process runs only this test, so the permissive umask cannot race
	// unrelated tests or affect the developer's shell.
	syscall.Umask(0)

	for _, mode := range []os.FileMode{0o644, 0o666} {
		for _, symlink := range []bool{false, true} {
			name := mode.String()
			if symlink {
				name += "/symlink"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("XDG_CONFIG_HOME", root)
				path, err := GetConfigPath()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				target := path
				if symlink {
					target = filepath.Join(root, "dotfile.yaml")
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
				const original = "# keep this comment\nproviders:\n  test:\n    api_key: literal-test-key\n"
				if err := os.WriteFile(target, []byte(original), mode); err != nil {
					t.Fatal(err)
				}
				unrelated := filepath.Join(root, "unrelated.yaml")
				if err := os.WriteFile(unrelated, []byte("unrelated\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := SetAgentPreference("test", "model", "test-model"); err != nil {
					t.Fatal(err)
				}
				assertConfigMode(t, target, 0o600)
				assertConfigMode(t, filepath.Dir(path), 0o755)
				assertConfigMode(t, unrelated, 0o644)
				data, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"# keep this comment", "api_key: literal-test-key", "model: test-model"} {
					if !strings.Contains(string(data), want) {
						t.Errorf("saved config missing %q", want)
					}
				}
				if symlink {
					info, err := os.Lstat(path)
					if err != nil || info.Mode()&os.ModeSymlink == 0 {
						t.Fatalf("config symlink was replaced: %v", err)
					}
				}
			})
		}
	}

	t.Run("new config", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		if _, err := SetAgentPreference("test", "model", "test-model"); err != nil {
			t.Fatal(err)
		}
		path, err := GetConfigPath()
		if err != nil {
			t.Fatal(err)
		}
		assertConfigMode(t, path, 0o600)
	})

	t.Run("full save repairs existing mode", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		cfg := &Config{}
		if err := Save(cfg); err != nil {
			t.Fatal(err)
		}
		path, err := GetConfigPath()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Save(cfg); err != nil {
			t.Fatal(err)
		}
		assertConfigMode(t, path, 0o600)
	})

	t.Run("generic atomic writer still preserves mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "not-config.yaml")
		if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := WriteFileAtomically(path, []byte("new\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertConfigMode(t, path, 0o644)
	})
}

func assertConfigMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}
