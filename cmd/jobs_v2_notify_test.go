package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/providerhttp"
)

type jobsV2NotifyTransientTestError struct{}

func (jobsV2NotifyTransientTestError) Error() string {
	return "transient notify failure"
}

func (jobsV2NotifyTransientTestError) RetryAfterDelay() (time.Duration, bool) {
	return time.Millisecond, true
}

func TestJobsV2NotifyRetriesTransientFailures(t *testing.T) {
	attempts := 0
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 0, nil, func(context.Context, jobsV2Run, jobsV2Job, jobsV2RunStatus, jobsV2RunResult, string, bool, string) error {
		attempts++
		if attempts < jobsV2NotifyMaxAttempts {
			return jobsV2NotifyTransientTestError{}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithNotifier: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "notify-retry-success",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob: %v", err)
	}

	mgr.notifyRunDone(run, jobsV2RunSucceeded, jobsV2RunResult{Stdout: "ok"}, exitReasonNatural, false, "")
	if attempts != jobsV2NotifyMaxAttempts {
		t.Fatalf("notification attempts = %d, want %d", attempts, jobsV2NotifyMaxAttempts)
	}
	events, _, err := mgr.ListRunEvents(run.ID, 0, 20, 0)
	if err != nil {
		t.Fatalf("ListRunEvents: %v", err)
	}
	for _, ev := range events {
		if ev.EventType == "notify_failed" {
			t.Fatalf("unexpected notify_failed event after successful retry: %#v", events)
		}
	}
}

func TestJobsV2NotifyPermanentHTTPFailureDoesNotRetry(t *testing.T) {
	attempts := 0
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 0, nil, func(context.Context, jobsV2Run, jobsV2Job, jobsV2RunStatus, jobsV2RunResult, string, bool, string) error {
		attempts++
		return providerhttp.NewStatusErrorString("Telegram", http.StatusBadRequest, "400 Bad Request", nil, "bad chat")
	})
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithNotifier: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "notify-permanent-failure",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob: %v", err)
	}

	mgr.notifyRunDone(run, jobsV2RunSucceeded, jobsV2RunResult{Stdout: "ok"}, exitReasonNatural, false, "")
	if attempts != 1 {
		t.Fatalf("notification attempts = %d, want 1 for permanent HTTP failure", attempts)
	}
	events, _, err := mgr.ListRunEvents(run.ID, 0, 20, 0)
	if err != nil {
		t.Fatalf("ListRunEvents: %v", err)
	}
	notifyFailures := 0
	for _, ev := range events {
		if ev.EventType == "notify_failed" {
			notifyFailures++
		}
	}
	if notifyFailures != 1 {
		t.Fatalf("notify_failed events = %d, want 1: %#v", notifyFailures, events)
	}
}

func TestJobsV2NotifyFailureDoesNotChangeRunStatus(t *testing.T) {
	var attempts atomic.Int32
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 0, nil, func(context.Context, jobsV2Run, jobsV2Job, jobsV2RunStatus, jobsV2RunResult, string, bool, string) error {
		attempts.Add(1)
		return jobsV2NotifyTransientTestError{}
	})
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithNotifier: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "notify-failure",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob: %v", err)
	}

	if err := mgr.finishRun(run.ID, jobsV2RunSucceeded, jobsV2RunResult{Stdout: "ok"}, nil, run.Attempt); err != nil {
		t.Fatalf("finishRun: %v", err)
	}
	updated, err := mgr.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if updated.Status != jobsV2RunSucceeded {
		t.Fatalf("status = %s, want succeeded", updated.Status)
	}
	var events []jobsV2RunEvent
	waitForServeCondition(t, 2*time.Second, func() bool {
		var err error
		events, _, err = mgr.ListRunEvents(run.ID, 0, 20, 0)
		if err != nil {
			return false
		}
		for _, ev := range events {
			if ev.EventType == "notify_failed" {
				return true
			}
		}
		return false
	}, "async notify_failed event")
	notifyFailures := 0
	for _, ev := range events {
		if ev.EventType == "notify_failed" {
			notifyFailures++
		}
	}
	if got := attempts.Load(); got != jobsV2NotifyMaxAttempts {
		t.Fatalf("notification attempts = %d, want %d", got, jobsV2NotifyMaxAttempts)
	}
	if notifyFailures != 1 {
		t.Fatalf("notify_failed events = %d, want 1: %#v", notifyFailures, events)
	}
}

func TestJobsV2FinishRunDoesNotBlockOnCompletionNotification(t *testing.T) {
	notifyStarted := make(chan struct{})
	releaseNotify := make(chan struct{})
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 0, nil, func(ctx context.Context, run jobsV2Run, job jobsV2Job, status jobsV2RunStatus, result jobsV2RunResult, exitReason string, truncated bool, errText string) error {
		close(notifyStarted)
		select {
		case <-releaseNotify:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithNotifier: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	defer close(releaseNotify)

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "notify-nonblocking",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob: %v", err)
	}

	started := time.Now()
	if err := mgr.finishRun(run.ID, jobsV2RunSucceeded, jobsV2RunResult{Stdout: "ok"}, nil, run.Attempt); err != nil {
		t.Fatalf("finishRun: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("finishRun blocked on notification for %v", elapsed)
	}

	select {
	case <-notifyStarted:
	case <-time.After(time.Second):
		t.Fatal("completion notification did not start asynchronously")
	}
}

func TestJobsV2CloseCancelsCompletionNotification(t *testing.T) {
	notifyStarted := make(chan struct{}, 1)
	var attempts atomic.Int32
	mgr, err := newJobsV2ManagerWithNotifier(":memory:", 0, nil, func(ctx context.Context, _ jobsV2Run, _ jobsV2Job, _ jobsV2RunStatus, _ jobsV2RunResult, _ string, _ bool, _ string) error {
		attempts.Add(1)
		notifyStarted <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatalf("newJobsV2ManagerWithNotifier: %v", err)
	}

	job, err := mgr.CreateJob(jobsV2Job{
		Name:          "notify-shutdown",
		Enabled:       true,
		RunnerType:    jobsV2RunnerProgram,
		RunnerConfig:  json.RawMessage(`{}`),
		TriggerType:   jobsV2TriggerManual,
		TriggerConfig: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	run, err := mgr.TriggerJob(job.ID)
	if err != nil {
		t.Fatalf("TriggerJob: %v", err)
	}
	if err := mgr.finishRun(run.ID, jobsV2RunSucceeded, jobsV2RunResult{Stdout: "ok"}, nil, run.Attempt); err != nil {
		t.Fatalf("finishRun: %v", err)
	}

	select {
	case <-notifyStarted:
	case <-time.After(time.Second):
		t.Fatal("completion notification did not start")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := mgr.CloseContext(closeCtx); err != nil {
		t.Fatalf("CloseContext: %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("notification attempts after shutdown = %d, want 1", got)
	}
}
