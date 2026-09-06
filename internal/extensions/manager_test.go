package extensions

import (
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, dir, id, css string) {
	t.Helper()
	p := filepath.Join(dir, id)
	if err := os.MkdirAll(p, 0755); err != nil {
		t.Fatal(err)
	}
	for n, b := range map[string]string{"extension.yaml": "title: " + id + "\nformat_version: 1\ncss: style.css\n", "style.css": css} {
		if err := os.WriteFile(filepath.Join(p, n), []byte(b), 0644); err != nil {
			t.Fatal(err)
		}
	}
}
func TestSnapshotsOrderAndReload(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "dracula", ":root { --bg: #282a36; }")
	fixture(t, dir, "clock", "body { color: pink; }")
	m := NewManager()
	reload := func(ids []string) {
		t.Helper()
		if err := m.Reload(dir, ids, "config", false, "r", "config.yaml"); err != nil {
			t.Fatal(err)
		}
	}
	reload([]string{"clock", "dracula"})
	s := m.Status()
	if s.Enabled[0] != "clock" || len(s.Entries) != 2 {
		t.Fatalf("%+v", s)
	}
	b, ok := m.Asset(s.Generation, "dracula/style.css")
	if !ok {
		t.Fatal("asset missing")
	}
	fixture(t, dir, "dracula", "body {color: green;}")
	old, _ := m.Asset(s.Generation, "dracula/style.css")
	if string(b) != string(old) {
		t.Fatal("mutable snapshot")
	}
	reload([]string{"dracula"})
	if m.Status().Generation == s.Generation {
		t.Fatal("generation did not change")
	}
	if _, ok = m.Asset(s.Generation, "dracula/style.css"); ok {
		t.Fatal("old generation served")
	}
	if _, ok = m.Asset(m.Status().Generation, "clock/style.css"); ok {
		t.Fatal("disabled asset served")
	}
}
func TestInvalidEntriesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "good", "x")
	fixture(t, dir, "bad", "x")
	if err := os.WriteFile(filepath.Join(dir, "bad", "extension.yaml"), []byte("title: Bad\nformat_version: 1\ncss: ../secret.css\n"), 0644); err != nil {
		t.Fatal(err)
	}
	fixture(t, dir, "link", "x")
	if err := os.Symlink(filepath.Join(dir, "good", "style.css"), filepath.Join(dir, "link", "escape.css")); err != nil {
		t.Fatal(err)
	}
	m := NewManager()
	if err := m.Reload(dir, []string{"good", "bad", "link", "missing"}, "config", false, "", ""); err != nil {
		t.Fatal(err)
	}
	s := m.Status()
	if len(s.Errors) != 3 {
		t.Fatalf("%+v", s)
	}
	if _, ok := m.Asset(s.Generation, "bad/style.css"); ok {
		t.Fatal("invalid extension served")
	}
	for _, ids := range [][]string{{"../escape"}, {"good", "good"}} {
		if err := m.Reload(dir, ids, "config", false, "", ""); err == nil {
			t.Fatal("invalid IDs accepted")
		}
	}
}
func TestDisabledAndMissingDirectory(t *testing.T) {
	m := NewManager()
	if err := m.Reload(filepath.Join(t.TempDir(), "missing"), []string{"x"}, "command-line", true, "", ""); err != nil {
		t.Fatal(err)
	}
	if !m.Status().Disabled || len(m.Status().Enabled) != 0 {
		t.Fatal("kill switch failed")
	}
}

func TestEntryPointChangesGeneration(t *testing.T) {
	dir := t.TempDir()
	fixture(t, dir, "theme", "body {color:red}")
	if err := os.WriteFile(filepath.Join(dir, "theme", "other.css"), []byte("body {color:blue}"), 0644); err != nil {
		t.Fatal(err)
	}
	m := NewManager()
	if err := m.Reload(dir, []string{"theme"}, "config", false, "", ""); err != nil {
		t.Fatal(err)
	}
	before := m.Status().Generation
	if err := os.WriteFile(filepath.Join(dir, "theme", "extension.yaml"), []byte("title: theme\nformat_version: 1\ncss: other.css\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(dir, []string{"theme"}, "config", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if before == m.Status().Generation {
		t.Fatal("entrypoint change reused generation")
	}
}
func TestKillSwitchSkipsBrokenDirectory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager()
	if err := m.Reload(p, []string{"theme"}, "command-line", true, "", ""); err != nil {
		t.Fatal(err)
	}
	if len(m.Status().Enabled) != 0 {
		t.Fatal("kill switch ignored")
	}
}
