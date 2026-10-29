//go:build unix

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestPrivateConfigSavePreservesSymlink(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	path, err := GetConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "dotfile.yaml")
	if err := os.WriteFile(target, []byte("default_provider: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../dotfile.yaml", path); err != nil {
		t.Fatal(err)
	}
	if err := Save(&Config{DefaultProvider: "new"}); err != nil {
		t.Fatal(err)
	}
	if link, err := os.Readlink(path); err != nil || link != "../dotfile.yaml" {
		t.Fatalf("config link changed: %q, %v", link, err)
	}
	assertConfigMode(t, target, 0o600)
	data, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(data), "default_provider: new") {
		t.Fatalf("target not updated: %s, %v", data, err)
	}
}

func TestWritePrivateConfigFileFailurePreservesConfig(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "dotfile.yaml")
	path := filepath.Join(root, "config.yaml")
	const original = "# keep comment\nvalue: original\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dotfile.yaml", path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	err := WritePrivateConfigFile(path, []byte("replacement\n"))
	if err == nil {
		t.Skip("config directory remained writable; cannot exercise atomic write failure")
	}
	if !strings.Contains(err.Error(), "create temp file") {
		t.Fatalf("unexpected write failure: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != original {
		t.Fatalf("target modified after failed write: %q, %v", data, err)
	}
	assertConfigMode(t, target, 0o644)
	if link, err := os.Readlink(path); err != nil || link != "dotfile.yaml" {
		t.Fatalf("config link changed: %q, %v", link, err)
	}
}

func TestEditPrivateConfigFilePathScope(t *testing.T) {
	for _, action := range []string{"retarget symlink", "replace with symlink", "replace with hardlink", "remove", "replace with directory"} {
		for _, editorFails := range []bool{false, true} {
			name := action + "/success"
			if editorFails {
				name = action + "/failure"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "config.yaml")
				target := path
				if action == "retarget symlink" {
					target = filepath.Join(root, "dotfile.yaml")
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(target, []byte("# original\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				unrelated := filepath.Join(root, "unrelated.yaml")
				if err := os.WriteFile(unrelated, []byte("# unrelated\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(unrelated, 0o644); err != nil {
					t.Fatal(err)
				}
				sentinel := errors.New("editor failed")
				err := EditPrivateConfigFile(path, nil, func(got string) error {
					if got != path {
						t.Fatalf("editor path = %s, want logical path %s", got, path)
					}
					assertConfigMode(t, target, 0o600)
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					switch action {
					case "retarget symlink", "replace with symlink":
						if err := os.Symlink(unrelated, path); err != nil {
							t.Fatal(err)
						}
					case "replace with hardlink":
						if err := os.Link(unrelated, path); err != nil {
							t.Fatal(err)
						}
					case "replace with directory":
						if err := os.Mkdir(path, 0o755); err != nil {
							t.Fatal(err)
						}
					}
					if editorFails {
						return sentinel
					}
					return nil
				})
				if editorFails && !errors.Is(err, sentinel) {
					t.Fatalf("lost editor failure: %v", err)
				}
				switch action {
				case "retarget symlink", "replace with symlink", "replace with directory":
					if err == nil || (err == sentinel) {
						t.Fatalf("expected exit hardening error, got %v", err)
					}
				case "remove":
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("editor deletion was not respected: %v", err)
					}
					if (!editorFails && err != nil) || (editorFails && err != sentinel) {
						t.Fatalf("editor result changed: %v", err)
					}
				case "replace with hardlink":
					assertConfigMode(t, path, 0o600)
					if (!editorFails && err != nil) || (editorFails && err != sentinel) {
						t.Fatalf("editor result changed: %v", err)
					}
				}
				assertConfigMode(t, unrelated, 0o644)
				data, readErr := os.ReadFile(unrelated)
				if readErr != nil || string(data) != "# unrelated\n" {
					t.Fatalf("unrelated file modified: %q, %v", data, readErr)
				}
			})
		}
	}
}

func TestPrivateConfigRejectsUnsupportedTargets(t *testing.T) {
	for _, state := range []string{"directory", "fifo", "dangling symlink"} {
		t.Run(state, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			switch state {
			case "directory":
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				// A non-regular target must be rejected before attempting a read.
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			case "dangling symlink":
				if err := os.Symlink("missing.yaml", path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := WritePrivateConfigFile(path, nil); err == nil {
				t.Fatal("write accepted unsupported config target")
			}
			if err := EditPrivateConfigFile(path, nil, func(string) error {
				t.Fatal("editor must not run for unsupported config targets")
				return nil
			}); err == nil {
				t.Fatal("edit accepted unsupported config target")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("unsupported config target changed: %v", err)
			}
		})
	}
}
