package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/diff"
	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/llm"
)

// fakeFileRecorder captures ChangeRecords for assertions.
type fakeFileRecorder struct {
	mu           sync.Mutex
	records      []filetrack.ChangeRecord
	sessionPaths []string
	seq          int64
	maxFileBytes int // 0 = filetrack.DefaultMaxFileBytes
}

func (f *fakeFileRecorder) RecordChange(ctx context.Context, rec filetrack.ChangeRecord) *llm.FileChange {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
	f.seq++
	kind := filetrack.KindModify
	switch {
	case rec.BeforeMissing && rec.AfterMissing:
		return nil
	case rec.BeforeMissing:
		kind = filetrack.KindCreate
	case rec.AfterMissing:
		kind = filetrack.KindDelete
	}
	return &llm.FileChange{Path: rec.Path, Kind: kind, Seq: f.seq}
}

func (f *fakeFileRecorder) SessionPaths(ctx context.Context, sessionID string) []string {
	return f.sessionPaths
}

func (f *fakeFileRecorder) MaxFileBytes() int {
	if f.maxFileBytes > 0 {
		return f.maxFileBytes
	}
	return filetrack.DefaultMaxFileBytes
}

func (f *fakeFileRecorder) recorded() []filetrack.ChangeRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]filetrack.ChangeRecord(nil), f.records...)
}

func (f *fakeFileRecorder) findRecord(t *testing.T, path string) filetrack.ChangeRecord {
	t.Helper()
	wantPath := canonicalTestPath(path)
	for _, rec := range f.recorded() {
		if canonicalTestPath(rec.Path) == wantPath {
			return rec
		}
	}
	t.Fatalf("no record for %s in %+v", path, f.recorded())
	return filetrack.ChangeRecord{}
}

func canonicalTestPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func trackingContext() context.Context {
	ctx := llm.ContextWithSessionID(context.Background(), "test-session")
	return llm.ContextWithCallID(ctx, "call-1")
}

func TestWriteFileToolRecordsChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	absPath, _ := resolveToolPath(path, true)
	recorder := &fakeFileRecorder{}
	tool := NewWriteFileTool(nil)
	tool.recorder = recorder

	// Content larger than the LLM diff cap must still be fully recorded.
	content := strings.Repeat("line of text\n", diff.MaxDiffSize/13+1)
	args, _ := json.Marshal(WriteFileArgs{Path: path, Content: content})
	output, err := tool.Execute(trackingContext(), args)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Diffs) != 0 {
		t.Fatal("oversized content should skip LLM diffs")
	}
	if len(output.FileChanges) != 1 || output.FileChanges[0].Kind != filetrack.KindCreate {
		t.Fatalf("file changes = %+v, want one create", output.FileChanges)
	}

	rec := recorder.findRecord(t, absPath)
	if !rec.BeforeMissing || string(rec.After) != content {
		t.Fatalf("create record = missing:%v afterLen:%d, want full content (%d)", rec.BeforeMissing, len(rec.After), len(content))
	}
	if rec.SessionID != "test-session" || rec.ToolCallID != "call-1" || rec.ToolName != WriteFileToolName {
		t.Fatalf("record metadata = %+v", rec)
	}

	// Overwrite: before content must be captured.
	args, _ = json.Marshal(WriteFileArgs{Path: path, Content: "short\n"})
	if _, err := tool.Execute(trackingContext(), args); err != nil {
		t.Fatal(err)
	}
	records := recorder.recorded()
	last := records[len(records)-1]
	if last.BeforeMissing || string(last.Before) != content || string(last.After) != "short\n" {
		t.Fatalf("modify record = %+v", last)
	}
}

func TestWriteFileToolNoSessionSkipsRecording(t *testing.T) {
	dir := t.TempDir()
	recorder := &fakeFileRecorder{}
	tool := NewWriteFileTool(nil)
	tool.recorder = recorder

	args, _ := json.Marshal(WriteFileArgs{Path: filepath.Join(dir, "f.txt"), Content: "x\n"})
	output, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.FileChanges) != 0 || len(recorder.recorded()) != 0 {
		t.Fatal("recording must be skipped without a session ID")
	}
}

func TestEditFileToolRecordsFullFileContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	original := "header\nold line\nfooter\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	absPath, _ := resolveToolPath(path, true)

	recorder := &fakeFileRecorder{}
	tool := NewEditFileTool(nil)
	tool.recorder = recorder

	args, _ := json.Marshal(EditFileArgs{Path: path, OldText: "old line", NewText: "new line"})
	output, err := tool.Execute(trackingContext(), args)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.FileChanges) != 1 {
		t.Fatalf("file changes = %+v, want one", output.FileChanges)
	}

	rec := recorder.findRecord(t, absPath)
	if string(rec.Before) != original {
		t.Fatalf("before = %q, want full original file", rec.Before)
	}
	if string(rec.After) != "header\nnew line\nfooter\n" {
		t.Fatalf("after = %q, want full new file", rec.After)
	}
}

func TestUnifiedDiffToolRecordsPerFile(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(pathA, []byte("alpha\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte("beta\n"), 0644); err != nil {
		t.Fatal(err)
	}

	recorder := &fakeFileRecorder{}
	tool := NewUnifiedDiffTool(nil)
	tool.recorder = recorder

	diffText := fmt.Sprintf(`--- a/%s
+++ b/%s
@@ -1 +1 @@
-alpha
+ALPHA
--- a/%s
+++ b/%s
@@ -1 +1 @@
-beta
+BETA
`, pathA, pathA, pathB, pathB)

	args, _ := json.Marshal(UnifiedDiffArgs{Diff: diffText})
	output, err := tool.Execute(trackingContext(), args)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.FileChanges) != 2 {
		t.Fatalf("file changes = %+v, want two", output.FileChanges)
	}
	if len(recorder.recorded()) != 2 {
		t.Fatalf("records = %+v, want two", recorder.recorded())
	}
}

func TestShellToolRecordsAffectedPaths(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(existing, []byte("before\n"), 0644); err != nil {
		t.Fatal(err)
	}
	doomed := filepath.Join(dir, "doomed.txt")
	if err := os.WriteFile(doomed, []byte("delete me\n"), 0644); err != nil {
		t.Fatal(err)
	}

	recorder := &fakeFileRecorder{}
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.recorder = recorder

	args, _ := json.Marshal(ShellArgs{
		Command:       "echo after > existing.txt && echo new > created.txt && rm doomed.txt",
		WorkingDir:    dir,
		AffectedPaths: []string{"*.txt"},
	})
	output, err := tool.Execute(trackingContext(), args)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.FileChanges) != 3 {
		t.Fatalf("file changes = %+v, want three", output.FileChanges)
	}

	mod := recorder.findRecord(t, existing)
	if string(mod.Before) != "before\n" || string(mod.After) != "after\n" {
		t.Fatalf("modify record = %q → %q", mod.Before, mod.After)
	}

	created := recorder.findRecord(t, filepath.Join(dir, "created.txt"))
	if !created.BeforeMissing || string(created.After) != "new\n" {
		t.Fatalf("create record = %+v", created)
	}

	deleted := recorder.findRecord(t, doomed)
	if !deleted.AfterMissing || string(deleted.Before) != "delete me\n" {
		t.Fatalf("delete record = %+v", deleted)
	}
}

func TestShellToolRecordsSessionTrackedPaths(t *testing.T) {
	dir := t.TempDir()
	tracked := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("v1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	recorder := &fakeFileRecorder{sessionPaths: []string{tracked}}
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.recorder = recorder

	// No affected_paths hint: layer 3 (already-tracked paths) must catch this.
	args, _ := json.Marshal(ShellArgs{
		Command:    "echo v2 > tracked.txt",
		WorkingDir: dir,
	})
	if _, err := tool.Execute(trackingContext(), args); err != nil {
		t.Fatal(err)
	}

	rec := recorder.findRecord(t, tracked)
	if string(rec.Before) != "v1\n" || string(rec.After) != "v2\n" {
		t.Fatalf("record = %q → %q", rec.Before, rec.After)
	}
}

func TestShellToolGitFallback(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable, skipping: %v (%s)", err, out)
		}
	}
	runGit("init")
	committed := filepath.Join(dir, "committed.txt")
	dirty := filepath.Join(dir, "dirty.txt")
	if err := os.WriteFile(committed, []byte("clean\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dirty, []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "init")
	if err := os.WriteFile(dirty, []byte("dirty-before\n"), 0644); err != nil {
		t.Fatal(err)
	}
	preExistingUntracked := filepath.Join(dir, "preexisting-untracked.txt")
	if err := os.WriteFile(preExistingUntracked, []byte("untracked-before\n"), 0644); err != nil {
		t.Fatal(err)
	}

	recorder := &fakeFileRecorder{}
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.recorder = recorder

	// No hints at all: the git layer must detect modifications of clean tracked
	// files, already-dirty tracked files whose porcelain status stays " M", new
	// untracked files, and pre-existing untracked files whose status stays "??".
	args, _ := json.Marshal(ShellArgs{
		Command:    "echo dirty > committed.txt && echo dirtier > dirty.txt && echo fresh > untracked.txt && echo changed > preexisting-untracked.txt",
		WorkingDir: dir,
	})
	if _, err := tool.Execute(trackingContext(), args); err != nil {
		t.Fatal(err)
	}

	mod := recorder.findRecord(t, committed)
	if string(mod.Before) != "clean\n" || string(mod.After) != "dirty\n" {
		t.Fatalf("git-recovered record = %q → %q (unknown=%v)", mod.Before, mod.After, mod.BeforeUnknown)
	}

	dirtyRec := recorder.findRecord(t, dirty)
	if string(dirtyRec.Before) != "dirty-before\n" || string(dirtyRec.After) != "dirtier\n" {
		t.Fatalf("pre-dirty record = %q → %q (unknown=%v)", dirtyRec.Before, dirtyRec.After, dirtyRec.BeforeUnknown)
	}

	created := recorder.findRecord(t, filepath.Join(dir, "untracked.txt"))
	if string(created.After) != "fresh\n" {
		t.Fatalf("untracked create record = %+v", created)
	}
	if !created.BeforeMissing && !created.BeforeUnknown {
		t.Fatalf("untracked create should have missing/unknown before, got %+v", created)
	}

	untrackedMod := recorder.findRecord(t, preExistingUntracked)
	if string(untrackedMod.Before) != "untracked-before\n" || string(untrackedMod.After) != "changed\n" {
		t.Fatalf("pre-existing untracked record = %q → %q", untrackedMod.Before, untrackedMod.After)
	}
}

func TestShellSnapshotBudgets(t *testing.T) {
	snap := &shellSnapshot{maxFileBytes: 1024}

	if !snap.canReadContent(512) {
		t.Fatal("small file within all budgets must be readable")
	}
	if snap.canReadContent(2048) {
		t.Fatal("file above the per-file cap must be refused")
	}

	snap.contentBytes = maxShellSnapshotBytes - 100
	if snap.canReadContent(512) {
		t.Fatal("read exceeding the total-bytes budget must be refused")
	}
	if !snap.canReadContent(100) {
		t.Fatal("read exactly filling the total-bytes budget is allowed")
	}

	snap.contentBytes = 0
	snap.contentReads = maxShellContentReads
	if snap.canReadContent(10) {
		t.Fatal("read beyond the read-count cap must be refused")
	}
}

func TestShellToolOversizedFilesDegradeToMetadata(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.txt")
	// Larger than the recorder's per-file cap below.
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 200)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	recorder := &fakeFileRecorder{maxFileBytes: 64}
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.recorder = recorder

	args, _ := json.Marshal(ShellArgs{
		Command:       "echo more >> big.txt",
		WorkingDir:    dir,
		AffectedPaths: []string{"big.txt"},
	})
	if _, err := tool.Execute(trackingContext(), args); err != nil {
		t.Fatal(err)
	}

	rec := recorder.findRecord(t, big)
	if !rec.BeforeUnknown || !rec.AfterUnknown {
		t.Fatalf("oversized file should record unknown content on both sides, got %+v", rec)
	}
	if rec.Before != nil || rec.After != nil {
		t.Fatal("oversized file content must not be captured")
	}
	if rec.BeforeSizeHint != 201 || rec.AfterSizeHint <= rec.BeforeSizeHint {
		t.Fatalf("size hints = %d → %d, want 201 → larger", rec.BeforeSizeHint, rec.AfterSizeHint)
	}
}

func TestRegistrySetFileChangeRecorder(t *testing.T) {
	toolConfig := &ToolConfig{Enabled: []string{WriteFileToolName, EditFileToolName, UnifiedDiffToolName, ShellToolName}}
	registry, err := NewLocalToolRegistry(toolConfig, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	recorder := &fakeFileRecorder{}
	registry.SetFileChangeRecorder(recorder)

	assertWired := func(stage string) {
		t.Helper()
		wt, _ := registry.Get(WriteFileToolName)
		if wt.(*WriteFileTool).recorder == nil {
			t.Fatalf("%s: write_file recorder not wired", stage)
		}
		et, _ := registry.Get(EditFileToolName)
		if et.(*EditFileTool).recorder == nil {
			t.Fatalf("%s: edit_file recorder not wired", stage)
		}
		ut, _ := registry.Get(UnifiedDiffToolName)
		if ut.(*UnifiedDiffTool).recorder == nil {
			t.Fatalf("%s: unified_diff recorder not wired", stage)
		}
		st, _ := registry.Get(ShellToolName)
		if st.(*ShellTool).recorder == nil {
			t.Fatalf("%s: shell recorder not wired", stage)
		}
	}

	assertWired("after SetFileChangeRecorder")

	// SetLimits re-creates the shell tool; the recorder must survive.
	registry.SetLimits(DefaultOutputLimits())
	assertWired("after SetLimits")
}
