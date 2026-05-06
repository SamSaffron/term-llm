package widgets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlaceholderMode(t *testing.T) {
	cases := []struct {
		name    string
		command []string
		want    string
		wantErr bool
	}{
		{"socket", []string{"cmd", "--socket", "$SOCKET"}, "socket", false},
		{"port", []string{"cmd", "--port", "$PORT"}, "port", false},
		{"port inline", []string{"cmd", "--port=$PORT"}, "port", false},
		{"both", []string{"cmd", "$SOCKET", "$PORT"}, "", true},
		{"neither", []string{"cmd", "--arg"}, "", true},
		{"socket in argv0", []string{"$SOCKET"}, "socket", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{Command: tc.command}
			got, err := m.PlaceholderMode()
			if (err != nil) != tc.wantErr {
				t.Fatalf("PlaceholderMode() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("PlaceholderMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSubstArgs(t *testing.T) {
	argv := []string{"cmd", "--socket=$SOCKET", "extra"}
	got := SubstArgs(argv, "$SOCKET", "/tmp/test.sock")
	if got[1] != "--socket=/tmp/test.sock" {
		t.Errorf("SubstArgs() = %v, want --socket=/tmp/test.sock", got[1])
	}
	if got[2] != "extra" {
		t.Errorf("SubstArgs() mutated unrelated arg: %v", got[2])
	}
	// original unchanged
	if argv[1] != "--socket=$SOCKET" {
		t.Errorf("SubstArgs() mutated original argv")
	}
}

func TestScanDir_Missing(t *testing.T) {
	manifests, errs := ScanDir("/nonexistent-dir-xyz")
	if len(manifests) != 0 || len(errs) != 0 {
		t.Errorf("missing dir should return empty results, got %v %v", manifests, errs)
	}
}

func TestScanDir_Valid(t *testing.T) {
	dir := t.TempDir()
	writeWidget(t, dir, "my-widget", `
title: "My Widget"
command: ["python", "server.py", "--socket", "$SOCKET"]
description: "A test widget"
`)
	manifests, errs := ScanDir(dir)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(manifests))
	}
	m := manifests[0]
	if m.ID != "my-widget" {
		t.Errorf("ID = %q, want my-widget", m.ID)
	}
	if m.Mount != "my-widget" {
		t.Errorf("Mount = %q, want my-widget (defaulted from ID)", m.Mount)
	}
	if m.Title != "My Widget" {
		t.Errorf("Title = %q, want 'My Widget'", m.Title)
	}
}

func TestScanDir_ExplicitMount(t *testing.T) {
	dir := t.TempDir()
	writeWidget(t, dir, "my-widget", `
title: "My Widget"
mount: hebrew
command: ["uv", "run", "python", "server.py", "--socket", "$SOCKET"]
`)
	manifests, errs := ScanDir(dir)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if manifests[0].Mount != "hebrew" {
		t.Errorf("Mount = %q, want hebrew", manifests[0].Mount)
	}
}

func TestScanDir_InvalidMount(t *testing.T) {
	cases := []struct{ name, mount string }{
		{"slash", "foo/bar"},
		{"uppercase", "FooBar"},
		{"space", "foo bar"},
		{"leading-dash", "-foo"},
		{"too-long", "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz01234"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			yaml := "title: T\ncommand: [\"cmd\", \"$SOCKET\"]\nmount: " + tc.mount + "\n"
			writeWidget(t, dir, "w", yaml)
			manifests, errs := ScanDir(dir)
			if len(errs) == 0 {
				t.Errorf("expected error for mount %q, got none; manifests=%v", tc.mount, manifests)
			}
		})
	}
}

func TestScanDir_MissingTitle(t *testing.T) {
	dir := t.TempDir()
	writeWidget(t, dir, "w", `command: ["cmd", "$PORT"]`)
	_, errs := ScanDir(dir)
	if len(errs) == 0 {
		t.Error("expected error for missing title")
	}
}

func TestScanDir_MissingCommand(t *testing.T) {
	dir := t.TempDir()
	writeWidget(t, dir, "w", `title: T`)
	_, errs := ScanDir(dir)
	if len(errs) == 0 {
		t.Error("expected error for missing command")
	}
}

func TestScanDir_BothPlaceholders(t *testing.T) {
	dir := t.TempDir()
	writeWidget(t, dir, "w", `title: T
command: ["cmd", "$SOCKET", "$PORT"]`)
	_, errs := ScanDir(dir)
	if len(errs) == 0 {
		t.Error("expected error for both $SOCKET and $PORT")
	}
}

func TestScanDir_DuplicateMount(t *testing.T) {
	dir := t.TempDir()
	// Both widgets default their mount to "aaa" and "bbb" but explicit mount "foo" collides
	writeWidget(t, dir, "aaa", `title: A
mount: foo
command: ["cmd", "$PORT"]`)
	writeWidget(t, dir, "bbb", `title: B
mount: foo
command: ["cmd", "$PORT"]`)
	manifests, errs := ScanDir(dir)
	if len(manifests) != 1 {
		t.Errorf("expected 1 manifest (first wins), got %d", len(manifests))
	}
	if len(errs) != 1 {
		t.Errorf("expected 1 error (duplicate), got %d", len(errs))
	}
	if manifests[0].ID != "aaa" {
		t.Errorf("expected first (aaa) to win, got %s", manifests[0].ID)
	}
}

func TestScanDir_SortedDeterministic(t *testing.T) {
	dir := t.TempDir()
	writeWidget(t, dir, "zz", `title: Z
command: ["cmd", "$PORT"]`)
	writeWidget(t, dir, "aa", `title: A
command: ["cmd", "$PORT"]`)
	writeWidget(t, dir, "mm", `title: M
command: ["cmd", "$PORT"]`)
	manifests, errs := ScanDir(dir)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	want := []string{"aa", "mm", "zz"}
	for i, m := range manifests {
		if m.ID != want[i] {
			t.Errorf("manifests[%d].ID = %q, want %q", i, m.ID, want[i])
		}
	}
}

func TestScanDir_SkipsNonDirs(t *testing.T) {
	dir := t.TempDir()
	// Create a regular file (not a directory) - should be skipped
	if err := os.WriteFile(filepath.Join(dir, "notadir"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	writeWidget(t, dir, "real", `title: R
command: ["cmd", "$PORT"]`)
	manifests, errs := ScanDir(dir)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(manifests) != 1 {
		t.Errorf("expected 1 manifest, got %d", len(manifests))
	}
}

func writeWidget(t *testing.T, base, id, yaml string) {
	t.Helper()
	d := filepath.Join(base, id)
	if err := os.MkdirAll(d, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "widget.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
}
