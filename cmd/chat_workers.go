package cmd

import (
	"context"
	"database/sql"
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
	threadWorkerLabelKey            = "term_llm_thread_worker"
	threadWorkerLabelValue          = "durable"
	threadWorkerOwnerLabelKey       = "thread_owner_id"
	threadWorkerCoordinatorLabelKey = "coordinator_session_id"
	threadWorkerChildLabelKey       = "child_session_id"
	threadWorkerTimeout             = 60 * time.Minute
)

var threadWorkerUnstartedGracePeriod = 2 * time.Second

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

func (r *chatWorkerRuntime) OwnerID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.manager == nil {
		return ""
	}
	return r.manager.workerID
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
	labels, _ := json.Marshal(map[string]string{
		threadWorkerLabelKey:            threadWorkerLabelValue,
		threadWorkerOwnerLabelKey:       manager.workerID,
		threadWorkerCoordinatorLabelKey: req.CoordinatorSessionID,
		threadWorkerChildLabelKey:       req.ChildSessionID,
	})
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

func decodeThreadWorkerLabels(job jobsV2Job) map[string]string {
	labels := map[string]string{}
	_ = json.Unmarshal(job.Labels, &labels)
	return labels
}

func (r *chatWorkerRuntime) finishWorkerEdge(ctx context.Context, workerStore session.WorkerStore, edge session.WorkerEdge, status session.WorkerStatus, title, body string, metadata json.RawMessage) error {
	return workerStore.FinishWorker(ctx, edge.ChildSessionID, status, session.WorkerReport{
		ChildSessionID: edge.ChildSessionID, SourceSessionID: edge.ChildSessionID,
		DestinationSessionID: edge.CoordinatorSessionID, Kind: session.WorkerReportResult,
		Title: title, Body: body, Metadata: metadata, Origin: "terminal_synthesis",
	})
}

func (r *chatWorkerRuntime) adoptWorkerOwner(ctx context.Context, manager *jobsV2Manager, workerStore session.WorkerStore, edge *session.WorkerEdge) (bool, error) {
	if edge.OwnerID == manager.workerID {
		return true, nil
	}
	live, err := manager.threadOwnerLive(edge.OwnerID)
	if err != nil {
		return false, err
	}
	if live {
		return false, nil
	}
	changed, err := workerStore.SetWorkerOwner(ctx, edge.ChildSessionID, edge.OwnerID, manager.workerID)
	if err != nil {
		return false, err
	}
	if !changed {
		refreshed, err := workerStore.GetWorker(ctx, edge.ChildSessionID)
		if err != nil {
			return false, err
		}
		*edge = refreshed
		return edge.OwnerID == manager.workerID, nil
	}
	edge.OwnerID = manager.workerID
	return true, nil
}

func (r *chatWorkerRuntime) adoptThreadJob(ctx context.Context, manager *jobsV2Manager, edge session.WorkerEdge, job *jobsV2Job) (bool, error) {
	labels := decodeThreadWorkerLabels(*job)
	if labels[threadWorkerLabelKey] != threadWorkerLabelValue || labels[threadWorkerChildLabelKey] != edge.ChildSessionID || labels[threadWorkerCoordinatorLabelKey] != edge.CoordinatorSessionID {
		return false, fmt.Errorf("worker job linkage does not match durable edge")
	}
	ownerID := labels[threadWorkerOwnerLabelKey]
	if ownerID == manager.workerID {
		return true, nil
	}
	live, err := manager.threadOwnerLive(ownerID)
	if err != nil {
		return false, err
	}
	if live {
		return false, nil
	}
	labels[threadWorkerOwnerLabelKey] = manager.workerID
	updated, err := json.Marshal(labels)
	if err != nil {
		return false, err
	}
	result, err := manager.db.ExecContext(ctx, `UPDATE jobs_v2 SET labels = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND COALESCE(labels, '') = ?`, string(updated), job.ID, string(job.Labels))
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		refreshed, err := manager.GetJob(job.ID)
		if err != nil {
			return false, err
		}
		*job = refreshed
		return decodeThreadWorkerLabels(refreshed)[threadWorkerOwnerLabelKey] == manager.workerID, nil
	}
	job.Labels = updated
	manager.notifyWorkers(1)
	return true, nil
}

func jobsV2RunIsTerminal(status jobsV2RunStatus) bool {
	switch status {
	case jobsV2RunSucceeded, jobsV2RunFailed, jobsV2RunCancelled, jobsV2RunTimedOut, jobsV2RunSkipped:
		return true
	default:
		return false
	}
}

func (r *chatWorkerRuntime) reconcileWorker(ctx context.Context, manager *jobsV2Manager, workerStore session.WorkerStore, edge session.WorkerEdge) error {
	if !edge.Status.Active() {
		return nil
	}
	owned, err := r.adoptWorkerOwner(ctx, manager, workerStore, &edge)
	if err != nil || !owned {
		return err
	}
	if strings.TrimSpace(edge.RunID) == "" || strings.TrimSpace(edge.JobID) == "" {
		if time.Since(edge.UpdatedAt) < threadWorkerUnstartedGracePeriod {
			return nil
		}
		return r.finishWorkerEdge(ctx, workerStore, edge, session.WorkerFailed,
			"Worker execution was not started", "The chat exited before a durable worker run was recorded; no work was retried.", nil)
	}
	job, err := manager.GetJob(edge.JobID)
	if errors.Is(err, sql.ErrNoRows) {
		return r.finishWorkerEdge(ctx, workerStore, edge, session.WorkerFailed,
			"Worker execution record is missing", "The durable worker job no longer exists; no work was retried.", nil)
	}
	if err != nil {
		return err
	}
	owned, err = r.adoptThreadJob(ctx, manager, edge, &job)
	if err != nil || !owned {
		return err
	}
	run, err := manager.GetRun(edge.RunID)
	if errors.Is(err, sql.ErrNoRows) {
		return r.finishWorkerEdge(ctx, workerStore, edge, session.WorkerFailed,
			"Worker execution record is missing", "The durable worker run no longer exists; no work was retried.", nil)
	}
	if err != nil {
		return err
	}
	if jobsV2RunIsTerminal(run.Status) {
		result := jobsV2RunResult{
			Stdout: run.Stdout, Stderr: run.Stderr, Thinking: run.Thinking, Response: run.Response,
			SessionID: run.SessionID, ExitReason: run.ExitReason, Truncated: run.Truncated,
			TurnCount: run.TurnCount, InputTokens: run.InputTokens, OutputTokens: run.OutputTokens,
		}
		if run.ExitCode != nil {
			result.ExitCode = *run.ExitCode
		}
		return r.notifyDone(ctx, run, job, run.Status, result, run.ExitReason, run.Truncated, run.Error)
	}
	if err := manager.recoverRuns(); err != nil {
		return err
	}
	manager.notifyWorkers(1)
	return nil
}

func (r *chatWorkerRuntime) ReconcileWorkers(ctx context.Context, coordinatorSessionID string) error {
	r.mu.Lock()
	manager := r.manager
	r.mu.Unlock()
	if manager == nil {
		return fmt.Errorf("worker runtime is not initialized")
	}
	return r.withWorkerStore(ctx, func(workerStore session.WorkerStore, _ session.Store) error {
		edges, err := workerStore.ListWorkers(ctx, strings.TrimSpace(coordinatorSessionID))
		if err != nil {
			return err
		}
		for _, edge := range edges {
			if err := r.reconcileWorker(ctx, manager, workerStore, edge); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *chatWorkerRuntime) CancelWorker(ctx context.Context, req chat.WorkerCancelRequest) error {
	r.mu.Lock()
	manager := r.manager
	r.mu.Unlock()
	if manager == nil {
		return fmt.Errorf("worker runtime is not initialized")
	}
	return r.withWorkerStore(ctx, func(workerStore session.WorkerStore, _ session.Store) error {
		edge, err := workerStore.GetWorker(ctx, req.ChildSessionID)
		if err != nil {
			return err
		}
		if edge.CoordinatorSessionID != strings.TrimSpace(req.CoordinatorSessionID) {
			return fmt.Errorf("worker coordinator does not match cancellation request")
		}
		owned, err := r.adoptWorkerOwner(ctx, manager, workerStore, &edge)
		if err != nil {
			return err
		}
		if !owned {
			return fmt.Errorf("worker is owned by another live chat")
		}
		runID := strings.TrimSpace(edge.RunID)
		if runID == "" {
			runID = strings.TrimSpace(req.RunID)
		}
		if runID == "" {
			return r.finishWorkerEdge(ctx, workerStore, edge, session.WorkerCancelled,
				"Stale worker cleared", "No durable execution was recorded; the stale active edge was safely cleared.", nil)
		}
		run, err := manager.GetRun(runID)
		if errors.Is(err, sql.ErrNoRows) {
			return r.finishWorkerEdge(ctx, workerStore, edge, session.WorkerCancelled,
				"Stale worker cleared", "The durable execution run was missing; the stale active edge was safely cleared.", nil)
		}
		if err != nil {
			return err
		}
		job, err := manager.GetJob(run.JobID)
		if errors.Is(err, sql.ErrNoRows) {
			return r.finishWorkerEdge(ctx, workerStore, edge, session.WorkerCancelled,
				"Stale worker cleared", "The durable execution record was missing; the stale active edge was safely cleared.", nil)
		}
		if err != nil {
			return err
		}
		owned, err = r.adoptThreadJob(ctx, manager, edge, &job)
		if err != nil {
			return err
		}
		if !owned {
			return fmt.Errorf("worker run is owned by another live chat")
		}
		run, err = manager.CancelRun(runID)
		if errors.Is(err, sql.ErrNoRows) {
			return r.finishWorkerEdge(ctx, workerStore, edge, session.WorkerCancelled,
				"Stale worker cleared", "The durable execution run was missing; the stale active edge was safely cleared.", nil)
		}
		if err != nil {
			return err
		}
		if jobsV2RunIsTerminal(run.Status) {
			return r.reconcileWorker(ctx, manager, workerStore, edge)
		}
		return nil
	})
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
		metadata, _ := json.Marshal(map[string]any{
			"status": status, "exit_reason": exitReason, "truncated": truncated,
			"run_id": run.ID, "job_id": job.ID,
		})
		title := "Worker completed"
		if status != jobsV2RunSucceeded {
			title = "Worker ended: " + string(status)
		}
		return r.finishWorkerEdge(ctx, workerStore, edge, terminalWorkerStatus(status), title,
			synthesizedWorkerBody(status, result, exitReason, errText), metadata)
	})
}
