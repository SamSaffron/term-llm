package cmd

import (
	"context"
	"testing"
	"time"

	servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"
)

type jobsV2HostRunnerFunc func(context.Context, jobsV2Job, progressWriter) (jobsV2RunResult, error)

func (fn jobsV2HostRunnerFunc) Run(ctx context.Context, job jobsV2Job, pw progressWriter) (jobsV2RunResult, error) {
	return fn(ctx, job, pw)
}

func newJobsV2HostManager(t testing.TB, workers int, runners map[jobsV2RunnerType]jobsV2Runner, notify jobsV2RunDoneNotifier) *jobsV2Manager {
	t.Helper()
	mgr, err := servejobs.NewManager(":memory:", workers, runners, notify)
	if err != nil {
		t.Fatalf("jobs.NewManager: %v", err)
	}
	t.Cleanup(func() {
		if err := mgr.Close(); err != nil {
			t.Errorf("close jobs manager: %v", err)
		}
	})
	return mgr
}

func waitJobsV2HostCompletion[T any](t testing.TB, completions <-chan T) T {
	t.Helper()
	select {
	case completion := <-completions:
		return completion
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for jobs completion")
		var zero T
		return zero
	}
}
