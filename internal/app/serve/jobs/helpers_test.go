package jobs

import (
	"testing"
	"time"
)

func newJobsV2Manager(dbPath string, workers int, _ any) (*Manager, error) {
	return NewManager(dbPath, workers, map[RunnerType]Runner{
		RunnerProgram: &ProgramRunner{},
	}, nil)
}

func newJobsV2ManagerWithNotifier(dbPath string, workers int, _ any, notify RunDoneNotifier) (*Manager, error) {
	return NewManager(dbPath, workers, map[RunnerType]Runner{
		RunnerProgram: &ProgramRunner{},
	}, notify)
}

func waitForServeCondition(t *testing.T, timeout time.Duration, fn func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
