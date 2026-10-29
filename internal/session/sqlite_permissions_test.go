//go:build unix

package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// Only the child changes umask: other tests may run concurrently in the parent.
func TestSQLitePrivatePermissions(t *testing.T) {
	if os.Getenv("TERM_LLM_SESSION_PERMISSIONS_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSQLitePrivatePermissions$", "-test.v")
		cmd.Env = append(os.Environ(), "TERM_LLM_SESSION_PERMISSIONS_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("permissions subprocess: %v\n%s", err, out)
		}
		return
	}
	syscall.Umask(0)

	t.Run("new default storage", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("XDG_DATA_HOME", root)
		path, err := GetDBPath()
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewSQLiteStore(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		assertStorageMode(t, filepath.Dir(path), 0o700)
		assertDatabaseModes(t, path)
	})

	t.Run("repair existing storage only", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("XDG_DATA_HOME", root)
		path, err := GetDBPath()
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewSQLiteStore(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		// Keep the first connection open so WAL/SHM survive until the next open.
		for _, name := range []string{path, path + "-wal", path + "-shm"} {
			if err := os.Chmod(name, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatal(err)
		}
		unrelated := filepath.Join(filepath.Dir(path), "unrelated.yaml")
		if err := os.WriteFile(unrelated, []byte("unrelated\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		reopened, err := NewSQLiteStore(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		assertDatabaseModes(t, path)
		assertStorageMode(t, filepath.Dir(path), 0o700)
		assertStorageMode(t, root, 0o755)
		assertStorageMode(t, unrelated, 0o644)
	})

	t.Run("custom existing parent stays unchanged", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "custom.db")
		store, err := NewSQLiteStore(Config{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		assertDatabaseModes(t, path)
		assertStorageMode(t, dir, 0o755)
		// SQLite removes WAL/SHM on the last close. Reopening must recreate
		// them privately without a per-connection or per-write chmod hook.
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := NewSQLiteStore(Config{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		if _, err := reopened.db.Exec("INSERT INTO metadata (key, value) VALUES ('permission-test', 'private')"); err != nil {
			t.Fatal(err)
		}
		assertDatabaseModes(t, path)
	})

	t.Run("new custom parent is private", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "new", "custom.db")
		store, err := NewSQLiteStore(Config{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		assertStorageMode(t, filepath.Dir(path), 0o700)
		assertDatabaseModes(t, path)
	})

	t.Run("reject symlinks without changing their targets", func(t *testing.T) {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			t.Run("database"+suffix, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "sessions.db")
				target := filepath.Join(dir, "unrelated.yaml")
				if err := os.WriteFile(target, []byte("unrelated\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path+suffix); err != nil {
					t.Fatal(err)
				}
				store, err := NewSQLiteStore(Config{Path: path})
				if err == nil {
					store.Close()
					t.Fatal("expected a non-regular session file to be rejected")
				}
				assertStorageMode(t, target, 0o644)
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "unrelated\n" {
					t.Fatalf("symlink target changed: %q, %v", data, err)
				}
			})
		}
	})

	t.Run("read only leaves permissions unchanged", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "readonly.db")
		store, err := NewSQLiteStore(Config{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		reader, err := NewSQLiteStore(Config{Path: path, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		assertStorageMode(t, path, 0o644)
	})
}

func assertDatabaseModes(t *testing.T, path string) {
	t.Helper()
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		assertStorageMode(t, name, 0o600)
	}
}

func assertStorageMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}

func TestSQLitePrivatePermissionsMemory(t *testing.T) {
	store, err := NewSQLiteStore(Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}
