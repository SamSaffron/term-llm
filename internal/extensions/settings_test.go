package extensions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsValidationAndComments(t *testing.T) {
	for _, data := range []string{"", "unknown: []", "enabled: [bad/id]", "enabled: [one, one]", "enabled: []\n---\nenabled: []"} {
		if _, err := ParseSettings([]byte(data)); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
	b, err := SettingsBytes([]byte("# my themes\nenabled: [one] # order\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "# my themes") || !strings.Contains(string(b), "# order") {
		t.Fatalf("lost comments: %s", b)
	}
	s, err := ParseSettings(b)
	if err != nil || len(s.Enabled) != 0 {
		t.Fatalf("empty list: %+v %v", s, err)
	}
	dir := t.TempDir()
	if err := WriteSettings(dir, b); err != nil {
		t.Fatal(err)
	}
	got, exists, err := ReadSettings(dir)
	if err != nil || !exists || string(got) != string(b) {
		t.Fatal("settings roundtrip failed")
	}
}
func TestSettingsCannotFollowMainConfigSymlink(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("sensitive: unchanged\n")
	if err := os.WriteFile(main, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(main, filepath.Join(dir, SettingsFile)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadSettings(dir); err == nil {
		t.Fatal("read followed symlink")
	}
	if err := WriteSettings(dir, []byte("enabled: []")); err == nil {
		t.Fatal("write followed symlink")
	}
	got, _ := os.ReadFile(main)
	if string(got) != string(original) {
		t.Fatal("main config changed")
	}
}
