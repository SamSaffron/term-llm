package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/tui/chat"
)

const (
	threadWorkerLabelKey   = "term_llm_thread_worker"
	threadWorkerLabelValue = "durable"
	threadWorkerTimeout    = 60 * time.Minute
)

const threadWorkerSystemMessage = `You are a durable background worker attached to a coordinating chat.
Work only on the task supplied by the user. This MVP uses a flat topology: do not launch nested workers or attempt to steer the coordinator.
Use the report tool for useful progress or blockers. Before finishing, call report with kind=result and a concise, self-contained outcome. Mailbox reports remain outside the coordinator's context until the user explicitly imports one.
The coordinator may share this working directory. Avoid broad or unrelated edits and report any concurrency risk before proceeding.`

type chatWorkerRuntime struct {
	mu      sync.Mutex
	manager *jobsV2Manager
	cfg     *config.Config
}

func (r *chatWorkerRuntime) Ensure(cfg *config.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.manager != nil {
		return nil
	}
	if cfg == nil {
		return fmt.Errorf("worker runtime configuration is unavailable")
	}
	r.cfg = cloneConfigForServeJob(cfg)
	fallback := resolvedApprovalMode{Mode: tools.ModePrompt, Source: approvalModeSourceBuiltinDefault}
	executor := newServeJobsExecutorWithApprovalResolver(r.cfg, fallback, r.resolveApproval)
	manager, err := newThreadWorkerJobsV2Manager("", 2, executor, r.notifyDone)
	if err != nil {
		return err
	}
	r.manager = manager
	return nil
}

func (r *chatWorkerRuntime) Close() error {
	r.mu.Lock()
	manager := r.manager
	r.manager = nil
	r.mu.Unlock()
	if manager == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return manager.CloseContext(ctx)
}

func (r *chatWorkerRuntime) withWorkerStore(ctx context.Context, fn func(session.WorkerStore, session.Store) error) error {
	r.mu.Lock()
	cfg := r.cfg
	r.mu.Unlock()
	if cfg == nil {
		return fmt.Errorf("worker runtime is not initialized")
	}
	store, cleanup := InitSessionStore(cfg, io.Discard)
	defer cleanup()
	workerStore, ok := session.AsWorkerStore(store)
	if !ok {
		return session.ErrWorkersUnsupported
	}
	return fn(workerStore, store)
}

func (r *chatWorkerRuntime) resolveApproval(ctx context.Context, cfg jobsV2LLMConfig) (resolvedApprovalMode, error) {
	if !cfg.Worker || strings.TrimSpace(cfg.ParentSessionID) == "" || strings.TrimSpace(cfg.SessionID) == "" {
		return resolvedApprovalMode{}, fmt.Errorf("thread worker approval requires durable parent linkage")
	}
	resolved := resolvedApprovalMode{Mode: tools.ModePrompt, Source: approvalModeSourceSession}
	err := r.withWorkerStore(ctx, func(workerStore session.WorkerStore, store session.Store) error {
		edge, err := workerStore.GetWorker(ctx, cfg.SessionID)
		if err != nil {
			return fmt.Errorf("load worker edge: %w", err)
		}
		if edge.CoordinatorSessionID != cfg.ParentSessionID {
			return fmt.Errorf("worker parent does not match durable edge")
		}
		parent, err := store.Get(ctx, edge.CoordinatorSessionID)
		if err != nil {
			return fmt.Errorf("load worker coordinator: %w", err)
		}
		switch parent.ApprovalMode {
		case session.ApprovalModeAuto:
			resolved.Mode = tools.ModeAuto
		case session.ApprovalModeYolo:
			resolved.Mode = tools.ModeYolo
		default:
			resolved.Mode = tools.ModePrompt
		}
		if cfg.WorkerApprovalMode != "" {
			requested, err := parseCLIApprovalMode(cfg.WorkerApprovalMode)
			if err != nil {
				return err
			}
			if requested != resolved.Mode {
				return fmt.Errorf("worker approval %s exceeds or differs from durable coordinator approval %s", requested, resolved.Mode)
			}
			resolved.Mode = requested
		}
		return workerStore.UpdateWorkerStatus(ctx, edge.ChildSessionID, session.WorkerRunning)
	})
	return resolved, err
}

func (r *chatWorkerRuntime) StartWorker(ctx context.Context, req chat.WorkerStartRequest) (chat.WorkerStartResult, error) {
	r.mu.Lock()
	manager := r.manager
	r.mu.Unlock()
	if manager == nil {
		return chat.WorkerStartResult{}, fmt.Errorf("worker runtime is not initialized")
	}
	if strings.TrimSpace(req.ChildSessionID) == "" || strings.TrimSpace(req.CoordinatorSessionID) == "" {
		return chat.WorkerStartResult{}, fmt.Errorf("worker session linkage is required")
	}
	persist := true
	cfg := jobsV2LLMConfig{
		AgentName: "developer", Instructions: req.Task, PersistSession: &persist,
		SessionID: req.ChildSessionID, SessionName: "Worker: " + session.TruncateSummary(req.Task),
		ParentSessionID: req.CoordinatorSessionID, Worker: true, WorkerApprovalMode: req.ApprovalMode.String(),
		Cwd: req.Cwd, Provider: req.Provider, Model: req.Model, Tools: req.Tools,
		Search: req.Search, SystemMessage: threadWorkerSystemMessage,
	}
	runnerConfig, err := json.Marshal(cfg)
	if err != nil {
		return chat.WorkerStartResult{}, err
	}
	labels, _ := json.Marshal(map[string]string{threadWorkerLabelKey: threadWorkerLabelValue, "child_session_id": req.ChildSessionID})
	job, err := manager.CreateJob(jobsV2Job{
		Name: "thread-" + session.ShortID(req.ChildSessionID), Enabled: true,
		RunnerType: jobsV2RunnerLLM, RunnerConfig: runnerConfig,
		TriggerType: jobsV2TriggerManual, TriggerConfig: json.RawMessage(`{}`),
		ConcurrencyPolicy: "forbid", MaxConcurrentRuns: 1,
		RetryPolicy:    json.RawMessage(`{"max_attempts":1}`),
		TimeoutSeconds: int(threadWorkerTimeout / time.Second), MisfirePolicy: jobsV2MisfireSkip,
		Labels: labels,
	})
	if err != nil {
		return chat.WorkerStartResult{}, err
	}
	run, err := manager.TriggerJob(job.ID)
	if err != nil {
		_ = manager.DeleteJob(job.ID, true)
		return chat.WorkerStartResult{}, err
	}
	return chat.WorkerStartResult{JobID: job.ID, RunID: run.ID}, nil
}

func (r *chatWorkerRuntime) CancelWorker(_ context.Context, runID string) error {
	r.mu.Lock()
	manager := r.manager
	r.mu.Unlock()
	if manager == nil {
		return fmt.Errorf("worker runtime is not initialized")
	}
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("worker run id is empty")
	}
	_, err := manager.CancelRun(runID)
	return err
}

func terminalWorkerStatus(status jobsV2RunStatus) session.WorkerStatus {
	switch status {
	case jobsV2RunSucceeded:
		return session.WorkerDone
	case jobsV2RunCancelled:
		return session.WorkerCancelled
	default:
		return session.WorkerFailed
	}
}

func synthesizedWorkerBody(status jobsV2RunStatus, result jobsV2RunResult, exitReason, errText string) string {
	body := strings.TrimSpace(result.Response)
	if body == "" {
		body = strings.TrimSpace(result.Stdout)
	}
	if body == "" {
		body = strings.TrimSpace(errText)
	}
	if body == "" {
		body = fmt.Sprintf("Worker finished with status %s (%s).", status, exitReason)
	}
	return body
}

func (r *chatWorkerRuntime) notifyDone(ctx context.Context, run jobsV2Run, job jobsV2Job, status jobsV2RunStatus, result jobsV2RunResult, exitReason string, truncated bool, errText string) error {
	var cfg jobsV2LLMConfig
	if err := json.Unmarshal(job.RunnerConfig, &cfg); err != nil || !cfg.Worker {
		return nil
	}
	return r.withWorkerStore(ctx, func(workerStore session.WorkerStore, _ session.Store) error {
		edge, err := workerStore.GetWorker(ctx, cfg.SessionID)
		if err != nil {
			return err
		}
		hasResult, err := workerStore.HasWorkerReportKind(ctx, edge.ChildSessionID, session.WorkerReportResult)
		if err != nil {
			return err
		}
		if !hasResult {
			metadata, _ := json.Marshal(map[string]any{
				"status": status, "exit_reason": exitReason, "truncated": truncated,
				"run_id": run.ID, "job_id": job.ID,
			})
			title := "Worker completed"
			if status != jobsV2RunSucceeded {
				title = "Worker ended: " + string(status)
			}
			if _, err := workerStore.AddWorkerReport(ctx, session.WorkerReport{
				ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
				DestinationSessionID: edge.CoordinatorSessionID, Kind: session.WorkerReportResult,
				Title: title, Body: synthesizedWorkerBody(status, result, exitReason, errText),
				Metadata: metadata, Origin: "terminal_synthesis",
			}); err != nil {
				return err
			}
		}
		if err := workerStore.UpdateWorkerStatus(ctx, edge.ChildSessionID, terminalWorkerStatus(status)); err != nil && !errors.Is(err, session.ErrNotFound) {
			return err
		}
		return nil
	})
}
