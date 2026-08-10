package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func executeTrackedShell(t *testing.T, recorder *fakeFileRecorder, dir, command string, affectedPaths ...string) {
	t.Helper()
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.recorder = recorder
	args, err := json.Marshal(ShellArgs{
		Command:       command,
		WorkingDir:    dir,
		AffectedPaths: affectedPaths,
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(trackingContext(), args)
	if err != nil {
		t.Fatal(err)
	}
	if output.IsError {
		t.Fatalf("shell command failed: %s", output.Content)
	}
}

func requireTestGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	requireTestGit(t)
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func initCommittedTestRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	runTestGit(t, dir, "init", "-q")
	for path, content := range files {
		writeTestFile(t, filepath.Join(dir, path), content)
	}
	runTestGit(t, dir, "add", ".")
	runTestGit(t, dir, "commit", "-qm", "initial")
}

func TestShellToolReadOnlyCommandWithLargeSessionHistoryRecordsNothing(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, maxShellContentReads+25)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("historical-%03d.txt", i))
		writeTestFile(t, paths[i], "unchanged\n")
	}

	recorder := &fakeFileRecorder{sessionPaths: paths}
	executeTrackedShell(t, recorder, dir, "cat historical-000.txt >/dev/null")
	if records := recorder.recorded(); len(records) != 0 {
		t.Fatalf("read-only command recorded %d historical paths, want zero", len(records))
	}
}

func TestShellSnapshotChangesAroundContentReadCap(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, maxShellContentReads+2)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("path-%03d.txt", i))
		writeTestFile(t, paths[i], "before\n")
	}
	recorder := &fakeFileRecorder{sessionPaths: paths}
	ctx := trackingContext()
	snap := preShellSnapshot(ctx, recorder, dir, nil)

	captured := paths[maxShellContentReads-1]
	beyondCap := paths[maxShellContentReads]
	writeTestFile(t, captured, "captured changed\n")
	writeTestFile(t, beyondCap, "beyond cap changed\n")
	changes := postShellChanges(ctx, recorder, snap)
	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want two changed paths around cap", changes)
	}

	capturedRecord := recorder.findRecord(t, captured)
	if string(capturedRecord.Before) != "before\n" || string(capturedRecord.After) != "captured changed\n" {
		t.Fatalf("captured record = %q -> %q", capturedRecord.Before, capturedRecord.After)
	}
	beyondRecord := recorder.findRecord(t, beyondCap)
	if !beyondRecord.BeforeUnknown || string(beyondRecord.After) != "beyond cap changed\n" {
		t.Fatalf("beyond-cap record = %+v", beyondRecord)
	}
}

func TestShellGlobPrunesGitAdminAndPreservesSimilarNames(t *testing.T) {
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-q")
	for i := 0; i < maxShellGlobMatches+25; i++ {
		writeTestFile(t, filepath.Join(dir, ".git", "tracking-fixtures", fmt.Sprintf("entry-%04d", i)), "admin\n")
	}

	source := filepath.Join(dir, "zz-source.txt")
	github := filepath.Join(dir, ".github", "workflows", "ci.yml")
	suffix := filepath.Join(dir, "archive.git")
	marker := filepath.Join(dir, "submodule", ".git")
	writeTestFile(t, source, "before\n")
	writeTestFile(t, github, "before\n")
	writeTestFile(t, suffix, "before\n")
	writeTestFile(t, marker, "gitdir: elsewhere\n")

	recorder := &fakeFileRecorder{}
	executeTrackedShell(t, recorder, dir,
		"printf 'after source\\n' > zz-source.txt && printf 'after github\\n' > .github/workflows/ci.yml && printf 'after suffix\\n' > archive.git && printf 'gitdir: changed\\n' > submodule/.git",
		"**/*")

	if records := recorder.recorded(); len(records) != 3 {
		t.Fatalf("records = %+v, want source, .github, and archive.git only", records)
	}
	recorder.findRecord(t, source)
	recorder.findRecord(t, github)
	recorder.findRecord(t, suffix)
	for _, record := range recorder.recorded() {
		if hasGitAdminComponent(record.Path) {
			t.Fatalf("recorded Git administrative path: %+v", record)
		}
	}
}

func TestShellBroadScopeNewCleanRepoIsMaterializationBaseline(t *testing.T) {
	dir := t.TempDir()
	requireTestGit(t)
	recorder := &fakeFileRecorder{}
	executeTrackedShell(t, recorder, dir,
		"mkdir cloned && git -C cloned init -q && printf 'clean\\n' > cloned/source.txt && git -C cloned add . && git -C cloned -c user.name=t -c user.email=t@t commit -qm initial",
		"cloned/**/*")
	if records := recorder.recorded(); len(records) != 0 {
		t.Fatalf("new clean repository recorded materialized files: %+v", records)
	}
}

func setupBranchMaterializationRepo(t *testing.T) (dir, originalBranch string) {
	t.Helper()
	dir = t.TempDir()
	initCommittedTestRepo(t, dir, map[string]string{"tracked.txt": "base\n"})
	originalBranch = runTestGit(t, dir, "branch", "--show-current")
	runTestGit(t, dir, "checkout", "-qb", "other")
	writeTestFile(t, filepath.Join(dir, "tracked.txt"), "other\n")
	runTestGit(t, dir, "add", "tracked.txt")
	runTestGit(t, dir, "commit", "-qm", "other")
	runTestGit(t, dir, "checkout", "-q", originalBranch)
	return dir, originalBranch
}

func TestShellBroadScopeCleanBranchCheckoutRecordsNothing(t *testing.T) {
	dir, _ := setupBranchMaterializationRepo(t)
	recorder := &fakeFileRecorder{}
	executeTrackedShell(t, recorder, dir, "git checkout -q other", "**/*.txt")
	if records := recorder.recorded(); len(records) != 0 {
		t.Fatalf("clean checkout recorded materialized files: %+v", records)
	}
}

func TestShellBroadScopeRetainsDirtyTrackedFileAfterMaterialization(t *testing.T) {
	dir, _ := setupBranchMaterializationRepo(t)
	recorder := &fakeFileRecorder{}
	executeTrackedShell(t, recorder, dir, "git checkout -q other && printf 'dirty after checkout\\n' > tracked.txt", "**/*.txt")
	record := recorder.findRecord(t, filepath.Join(dir, "tracked.txt"))
	if string(record.Before) != "base\n" || string(record.After) != "dirty after checkout\n" {
		t.Fatalf("dirty materialization record = %q -> %q", record.Before, record.After)
	}
}

func TestShellGitFallbackRetainsNewStagedFile(t *testing.T) {
	dir := t.TempDir()
	initCommittedTestRepo(t, dir, map[string]string{"tracked.txt": "base\n"})
	recorder := &fakeFileRecorder{}
	executeTrackedShell(t, recorder, dir, "printf 'staged\\n' > new.txt && git add new.txt")
	record := recorder.findRecord(t, filepath.Join(dir, "new.txt"))
	if !record.BeforeMissing || string(record.After) != "staged\n" {
		t.Fatalf("new staged record = %+v", record)
	}
}

func TestShellExactLiteralTracksPythonStyleEdit(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 unavailable: %v", err)
	}
	dir := t.TempDir()
	initCommittedTestRepo(t, dir, map[string]string{"tracked.txt": "before\n"})
	recorder := &fakeFileRecorder{}
	command := fmt.Sprintf("%s -c 'open(\"tracked.txt\", \"w\").write(\"after\\n\")'", python)
	executeTrackedShell(t, recorder, dir, command, "tracked.txt")
	record := recorder.findRecord(t, filepath.Join(dir, "tracked.txt"))
	if string(record.Before) != "before\n" || string(record.After) != "after\n" {
		t.Fatalf("literal Python edit = %q -> %q", record.Before, record.After)
	}
}

func TestShellExactLiteralTracksEditCommittedBeforeReturn(t *testing.T) {
	dir := t.TempDir()
	initCommittedTestRepo(t, dir, map[string]string{"tracked.txt": "before\n"})
	runTestGit(t, dir, "config", "user.name", "t")
	runTestGit(t, dir, "config", "user.email", "t@t")
	recorder := &fakeFileRecorder{}
	executeTrackedShell(t, recorder, dir,
		"printf 'committed\\n' > tracked.txt && git add tracked.txt && git commit -qm edit",
		"tracked.txt")
	record := recorder.findRecord(t, filepath.Join(dir, "tracked.txt"))
	if string(record.Before) != "before\n" || string(record.After) != "committed\n" {
		t.Fatalf("literal edit+commit = %q -> %q", record.Before, record.After)
	}
}

func TestShellBroadScopeExcludesIgnoredBuildOutput(t *testing.T) {
	dir := t.TempDir()
	initCommittedTestRepo(t, dir, map[string]string{".gitignore": "build/\n"})
	recorder := &fakeFileRecorder{}
	executeTrackedShell(t, recorder, dir, "mkdir -p build && printf 'artifact\\n' > build/output.txt", "**/*")
	if records := recorder.recorded(); len(records) != 0 {
		t.Fatalf("ignored build output was recorded: %+v", records)
	}
}

func TestShellSessionTrackedRevertToCleanIsRecorded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tracked.txt")
	initCommittedTestRepo(t, dir, map[string]string{"tracked.txt": "base\n"})
	writeTestFile(t, path, "session edit\n")
	recorder := &fakeFileRecorder{sessionPaths: []string{path}}
	executeTrackedShell(t, recorder, dir, "git checkout -- tracked.txt")
	record := recorder.findRecord(t, path)
	if string(record.Before) != "session edit\n" || string(record.After) != "base\n" {
		t.Fatalf("session revert record = %q -> %q", record.Before, record.After)
	}
}

func TestShellBroadScopeUsesDeepestNestedRepoOwnership(t *testing.T) {
	outer := t.TempDir()
	initCommittedTestRepo(t, outer, map[string]string{"outer.txt": "outer\n"})
	nested := filepath.Join(outer, "nested")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	initCommittedTestRepo(t, nested, map[string]string{"clean.txt": "clean\n", "dirty.txt": "base\n"})

	recorder := &fakeFileRecorder{}
	executeTrackedShell(t, recorder, outer,
		"printf 'temporary\\n' > nested/clean.txt && git -C nested checkout -- clean.txt && printf 'dirty\\n' > nested/dirty.txt",
		"nested/**/*.txt")
	if records := recorder.recorded(); len(records) != 1 {
		t.Fatalf("nested repo records = %+v, want only dirty.txt", records)
	}
	record := recorder.findRecord(t, filepath.Join(nested, "dirty.txt"))
	if string(record.Before) != "base\n" || string(record.After) != "dirty\n" {
		t.Fatalf("nested dirty record = %q -> %q", record.Before, record.After)
	}
}
