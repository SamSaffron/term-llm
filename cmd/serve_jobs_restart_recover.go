package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/session"
)

type jobsRestartSetup struct {
	Store                        session.Store
	RestartID, Service, Instance string
}

// recoverLLMRestarts runs before worker-loss recovery and before any workers
// start. A sealed jobs row alone is not authorization: the session grant must
// pass its atomic revision, service, instance, expiry and one-shot checks.
func (m *jobsV2Manager) recoverLLMRestarts(ctx context.Context, setup jobsRestartSetup, reclaim bool) error {
	if setup.RestartID == "" {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `SELECT phase,payload FROM job_restart_checkpoints WHERE restart_id=? AND phase IN ('prepared','sealed')`, setup.RestartID)
	if err != nil {
		return err
	}
	type entry struct {
		phase string
		c     jobsRestartCheckpoint
	}
	var entries []entry
	for rows.Next() {
		var e entry
		var raw string
		if err = rows.Scan(&e.phase, &raw); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal([]byte(raw), &e.c); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	handoffs, ok := session.AsCommandHandoffStore(setup.Store)
	if !ok {
		return fmt.Errorf("LLM restart adoption requires durable session storage")
	}
	// Validate the batch's identities/phases before consuming any grants.
	for _, e := range entries {
		if reclaim && e.phase == "prepared" {
			continue
		}
		run, err := m.GetRun(e.c.RunID)
		if err != nil {
			return err
		}
		if run.Status != jobsV2RunRunning {
			continue
		}
		if e.phase != "sealed" || e.c.Service != setup.Service || e.c.RestartID != setup.RestartID || e.c.SessionID != run.SessionID {
			return errJobsRestartConflict
		}
		if setup.Instance == "" || (e.c.SourceInstance == setup.Instance) != reclaim {
			return errJobsRestartConflict
		}
	}
	for _, e := range entries {
		if reclaim && e.phase == "prepared" {
			continue
		}
		run, err := m.GetRun(e.c.RunID)
		if err != nil {
			return err
		}
		if reclaim && run.Status == jobsV2RunCancelRequested {
			result := jobsV2RunResult{SessionID: run.SessionID, Stdout: run.Stdout, Stderr: run.Stderr, Thinking: run.Thinking, Response: run.Response, TurnCount: run.TurnCount, InputTokens: run.InputTokens, OutputTokens: run.OutputTokens}
			if err := m.finishRun(run.ID, jobsV2RunCancelled, result, context.Canceled, run.Attempt); err != nil {
				return err
			}
			continue
		}
		if run.Status != jobsV2RunRunning {
			continue
		} // User Stop is authoritative.
		var grant session.CommandHandoff
		if reclaim {
			grant, err = handoffs.ReclaimCommandHandoff(ctx, e.c.HandoffID, setup.Service, setup.Instance)
		} else {
			grant, err = handoffs.ConsumeCommandHandoff(ctx, e.c.HandoffID, setup.Service, setup.Instance)
		}
		if err != nil {
			return fmt.Errorf("adopt LLM job %s: %w", e.c.RunID, err)
		}
		if err = m.activateLLMRestart(ctx, e.c.RunID, e.c.RestartID, grant); err != nil {
			// Stop can win after consuming the grant but before the jobs transaction.
			current, getErr := m.GetRun(e.c.RunID)
			if getErr == nil && (current.Status == jobsV2RunCancelled || current.Status == jobsV2RunCancelRequested) {
				continue
			}
			return err
		}
	}
	return nil
}
