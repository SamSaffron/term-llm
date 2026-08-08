package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseAutoWaitsForGitHubWorkflow(t *testing.T) {
	tempDir := t.TempDir()
	logPath := filepath.Join(tempDir, "commands.log")
	statePath := filepath.Join(tempDir, "gh-list-count")

	harnessPath := filepath.Join(tempDir, "release-harness.sh")
	writeExecutable(t, harnessPath, `#!/bin/bash
set -eu
release_script=$1
git() {
  printf 'git %s\n' "$*" >> "$COMMAND_LOG"
  case "${1:-}" in
    remote) printf 'origin\n' ;;
    fetch) ;;
    tag)
      if [ "${2:-}" = "-l" ]; then printf 'v1.2.3\n'; fi
      ;;
    branch) printf 'main\n' ;;
    status) ;;
    push) ;;
  esac
}
gh() {
  printf 'gh %s\n' "$*" >> "$COMMAND_LOG"
  if [ "${1:-}" = "run" ] && [ "${2:-}" = "list" ]; then
    count=0
    if [ -f "$GH_STATE" ]; then read -r count < "$GH_STATE"; fi
    count=$((count + 1))
    printf '%s\n' "$count" > "$GH_STATE"
    if [ "$count" -ge 2 ]; then printf '12345\n'; fi
  fi
}
sleep() { :; }
export -f git gh sleep
source "$release_script" --auto --wait
`)

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test file")
	}
	scriptPath := filepath.Join(filepath.Dir(currentFile), "release.sh")
	cmd := exec.Command("bash", harnessPath, scriptPath)
	cmd.Env = append(os.Environ(),
		"COMMAND_LOG="+logPath,
		"GH_STATE="+statePath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("release script failed: %v\n%s", err, output)
	}

	if !strings.Contains(string(output), "Auto-detected next version: v1.2.4") {
		t.Errorf("output did not report auto-detected version:\n%s", output)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logData)
	if count := strings.Count(log, "gh run list "); count != 2 {
		t.Errorf("gh run list call count = %d, want 2\n%s", count, log)
	}
	if !strings.Contains(log, "gh run list --workflow release.yml --branch v1.2.4 --event push --limit 1 --json databaseId --jq .[0].databaseId") {
		t.Errorf("workflow lookup did not target the release tag:\n%s", log)
	}
	if !strings.Contains(log, "gh run watch 12345 --exit-status") {
		t.Errorf("release workflow was not watched with exit status propagation:\n%s", log)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}
