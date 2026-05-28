package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachFileExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	path := filepath.Join(home, "tmp", "test.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	att, err := AttachFile("~/tmp/test.txt")
	if err != nil {
		t.Fatalf("AttachFile() error = %v", err)
	}
	if att.Path != path {
		t.Fatalf("attachment path = %q, want %q", att.Path, path)
	}
	if att.Content != "hello" {
		t.Fatalf("attachment content = %q, want hello", att.Content)
	}
}

func TestExpandGlobExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := filepath.Join(home, "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}

	paths, err := ExpandGlob("~/tmp/*.txt")
	if err != nil {
		t.Fatalf("ExpandGlob() error = %v", err)
	}
	want := []string{filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ExpandGlob() = %#v, want %#v", paths, want)
	}
}

func TestApprovedDirsExpandTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dirs := &ApprovedDirs{Directories: []string{filepath.Join(home, "tmp")}}
	if !dirs.IsPathApproved("~/tmp/test.txt") {
		t.Fatal("~/tmp/test.txt should be approved when the expanded directory is approved")
	}
}

// TestApprovedDirsResolvesSymlinks ensures that a file under an approved
// directory is recognized as approved even when the approved directory itself
// is reached via a symlink (e.g. macOS /var -> /private/var, or a user-created
// symlink on Linux). Before the fix, only the file path had its symlinks
// resolved, so the prefix check against an un-resolved approved dir failed.
func TestApprovedDirsResolvesSymlinks(t *testing.T) {
	realDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(realDir, "test.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Create a symlink pointing at realDir and approve the symlink path.
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	dirs := &ApprovedDirs{Directories: []string{linkDir}}
	if !dirs.IsPathApproved(filepath.Join(realDir, "test.txt")) {
		t.Fatal("file under symlinked approved dir should be approved")
	}
}

func TestCmdFileClearsComposerAfterAttach(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	m := newTestChatModel(false)
	m.approvedDirs = &ApprovedDirs{Directories: []string{dir}}
	m.setTextareaValue("/file " + path)

	result, _ := m.cmdFile([]string{path})
	rm := result.(*Model)
	if got := rm.textarea.Value(); got != "" {
		t.Fatalf("composer = %q, want cleared", got)
	}
	if len(rm.files) != 1 || rm.files[0].Name != "test.txt" {
		t.Fatalf("attached files = %#v, want test.txt", rm.files)
	}
}

func TestAttachFileAllowsTwentyMegabyteTextFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")
	content := strings.Repeat("a", 3*1024*1024)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	att, err := AttachFile(path)
	if err != nil {
		t.Fatalf("AttachFile() error = %v", err)
	}
	if att.Size != int64(len(content)) || att.Content != content {
		t.Fatalf("attachment size/content mismatch")
	}
}

func TestAttachFileRejectsOverTwentyMegabytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "too-large.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", maxAttachmentSize+1)), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := AttachFile(path)
	if err == nil {
		t.Fatal("AttachFile() error = nil, want size error")
	}
	if !strings.Contains(err.Error(), "20.0MB") {
		t.Fatalf("AttachFile() error = %v, want 20MB limit", err)
	}
}
