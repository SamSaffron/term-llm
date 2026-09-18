package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The fuzz runner retries a target once when Go's fuzzing coordinator reports a
// bare shutdown deadline, because that failure names no input and is unrelated
// to the code under test. Findings that name an input or a seed entry must stop
// the run immediately.
func TestFuzzRunnerRetriesShutdownDeadlineButNotRealFindings(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test file")
	}
	scriptPath := filepath.Join(filepath.Dir(currentFile), "fuzz_owned_renderer.sh")

	// One invocation per fuzz target, plus any retry the runner decides to make.
	for _, testCase := range []struct {
		name      string
		mode      string
		wantError bool
		wantRetry bool
		wantCalls string
	}{
		{name: "shutdown deadline", mode: "flake", wantRetry: true, wantCalls: "11"},
		{name: "failing input", mode: "crasher", wantError: true, wantCalls: "1"},
		{name: "seed corpus entry", mode: "seed", wantError: true, wantCalls: "1"},
		{name: "all targets pass", mode: "pass", wantCalls: "10"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tempDir := t.TempDir()
			statePath := filepath.Join(tempDir, "go-call-count")
			for _, module := range []string{"runtime", "renderer"} {
				if err := os.MkdirAll(filepath.Join(tempDir, "internal", "terminal", module), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// $0 stays the harness, so place it where the script resolves the
			// owned terminal modules relative to itself.
			harnessPath := filepath.Join(tempDir, "scripts", "fuzz-harness.sh")
			if err := os.MkdirAll(filepath.Dir(harnessPath), 0o755); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, harnessPath, `#!/bin/bash
set -eu
fuzz_script=$1
go() {
  count=0
  if [ -f "$GO_STATE" ]; then read -r count < "$GO_STATE"; fi
  count=$((count + 1))
  printf '%s\n' "$count" > "$GO_STATE"
  printf 'fuzz: elapsed: 3s, execs: 1234 (411/sec), new interesting: 0 (total: 5)\n'
  case "$GO_MODE" in
    flake)
      if [ "$count" -eq 1 ]; then
        printf -- '--- FAIL: FuzzTarget (3.10s)\n    context deadline exceeded\nFAIL\n'
        return 1
      fi
      ;;
    crasher)
      printf -- '--- FAIL: FuzzTarget (0.04s)\n    Failing input written to testdata/fuzz/FuzzTarget/dbe14d95ca95b914\nFAIL\n'
      return 1
      ;;
    seed)
      printf 'failure while testing seed corpus entry: FuzzTarget/seed#0\n'
      printf -- '--- FAIL: FuzzTarget (0.01s)\n    key_test.go:9: boom\nFAIL\n'
      return 1
      ;;
  esac
  printf 'PASS\n'
}
export -f go
source "$fuzz_script"
`)

			cmd := exec.Command("bash", harnessPath, scriptPath)
			cmd.Env = append(os.Environ(), "GO_MODE="+testCase.mode, "GO_STATE="+statePath)
			output, err := cmd.CombinedOutput()
			if testCase.wantError && err == nil {
				t.Fatalf("fuzz runner succeeded, want failure:\n%s", output)
			}
			if !testCase.wantError && err != nil {
				t.Fatalf("fuzz runner failed: %v\n%s", err, output)
			}
			if retries := strings.Count(string(output), "retrying once"); (retries > 0) != testCase.wantRetry {
				t.Errorf("retry notices = %d, want retry = %v:\n%s", retries, testCase.wantRetry, output)
			}
			state, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(state)); got != testCase.wantCalls {
				t.Errorf("go invocations = %s, want %s:\n%s", got, testCase.wantCalls, output)
			}
		})
	}
}
