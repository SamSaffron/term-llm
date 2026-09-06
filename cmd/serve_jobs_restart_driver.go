package cmd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type jobsRestartDriver struct {
	mu               sync.Mutex
	setup            jobsRestartSetup
	active, rollback bool
	failure          error
}

func (m *jobsV2Manager) beginLLMRestart(ctx context.Context, setup jobsRestartSetup) error {
	d := &m.restartDriver
	d.mu.Lock()
	if d.setup.RestartID != "" && d.setup.RestartID != setup.RestartID {
		var pending int
		err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM job_restart_checkpoints c JOIN job_runs_v2 r ON r.id=c.run_id WHERE c.restart_id=? AND c.phase IN ('prepared','sealed') AND r.status='running'`, d.setup.RestartID).Scan(&pending)
		if err != nil || pending > 0 {
			d.mu.Unlock()
			return errors.New("previous LLM restart cleanup remains unresolved")
		}
	}
	d.setup = setup
	d.active = true
	d.rollback = false
	d.failure = nil
	d.mu.Unlock()
	err := m.interruptLLMJobs(ctx, setup.RestartID, setup.Service)
	if err != nil {
		d.mu.Lock()
		d.failure = err
		d.mu.Unlock()
	}
	return err
}

func (m *jobsV2Manager) prepareLateLLMSource(x *jobsLLMExecution) {
	d := &m.restartDriver
	d.mu.Lock()
	setup, active := d.setup, d.active
	d.mu.Unlock()
	if !active {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := x.prepare(ctx, setup.RestartID, setup.Service); err != nil {
		x.mu.Lock()
		runID := x.checkpoint.RunID
		x.mu.Unlock()
		m.noteLLMRestartFailure(runID, err)
	}
}

func (m *jobsV2Manager) noteLLMRestartFailure(runID string, err error) {
	if err == nil {
		return
	}
	run, getErr := m.GetRun(runID)
	if getErr == nil && (run.Status == jobsV2RunCancelled || run.Status == jobsV2RunCancelRequested) {
		return
	}
	d := &m.restartDriver
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failure = fmt.Errorf("LLM job %s checkpoint: %w", runID, err)
}

func (m *jobsV2Manager) llmRestartFailure() error {
	d := &m.restartDriver
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.failure
}

func (m *jobsV2Manager) rollbackLLMRestart() error {
	d := &m.restartDriver
	d.mu.Lock()
	d.active = false
	d.rollback = true
	d.mu.Unlock()
	return m.recoverRolledBackLLMJobs()
}

// Called after worker source/cancel-map cleanup, before its gate is released.
// This also recovers sources which only settle after the coordinator aborts.
func (m *jobsV2Manager) recoverRolledBackLLMJobs() error {
	d := &m.restartDriver
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.rollback || d.setup.RestartID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.recoverLLMRestarts(ctx, d.setup, true); err != nil {
		d.failure = err
		return err
	}
	m.notifyWorkers(m.workers)
	return nil
}
