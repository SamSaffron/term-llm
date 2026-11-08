package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceWritesNewContent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(dir, ".config.yaml.tmp")
	if err := os.WriteFile(temp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(temp, target); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("content = %q", data)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("temp file still exists: %v", err)
	}
}

func TestReplaceCreatesMissingTarget(t *testing.T) {
	dir := t.TempDir()
	temp := filepath.Join(dir, "temp")
	if err := os.WriteFile(temp, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "new-file")
	if err := Replace(temp, target); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "data" {
		t.Fatalf("read = %q, %v", data, err)
	}
}

func TestReplaceReportsMissingTemp(t *testing.T) {
	dir := t.TempDir()
	if err := Replace(filepath.Join(dir, "missing"), filepath.Join(dir, "target")); err == nil {
		t.Fatal("expected error for missing temp file")
	}
}
