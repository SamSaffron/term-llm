package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/process"
	"github.com/samsaffron/term-llm/internal/session"
)

type jobsExecutionContextKey struct{}

// jobsLLMExecution is the source-side lifetime owner. The worker retains its
// existing restart gate until runner cleanup and journal sealing both finish.
type jobsLLMExecution struct {
	native             bool
	ready              bool
	progressive        bool
	passStartTurns     int64
	passCancel         context.CancelFunc
	passGeneration     uint64
	mu                 sync.Mutex
	manager            *jobsV2Manager
	ctx                context.Context
	cancel             context.CancelFunc
	checkpoint         jobsRestartCheckpoint
	engine             *llm.Engine
	handoffs           session.CommandHandoffStore
	owner              llm.SteeringTransition
	prepared, finished bool
	savedID            string
	saveErr            error
	initialTurns       int64
}

func (x *jobsLLMExecution) bind(env *cmdRunEnvironment) {
	x.mu.Lock()
	x.engine = env.engine
	x.native = env.provider != nil && env.provider.Capabilities().InlineToolLoop
	x.initialTurns = env.engine.ModelTurns()
	x.handoffs, _ = session.AsCommandHandoffStore(env.store)
	x.checkpoint.RemainingTurns = env.llmReq.TurnLimit()
	x.progressive = env.req.Progressive != nil
	if x.progressive {
		x.checkpoint.PassTurnLimit = env.llmReq.TurnLimit()
		if prior, ok := x.ctx.Value(jobsRestartContextKey{}).(*jobsRestartCheckpoint); ok && prior.PassTurnLimit > 0 {
			x.checkpoint.PassTurnLimit = prior.PassTurnLimit
			x.checkpoint.RemainingTurns = prior.RemainingTurns
			x.checkpoint.PassHadWork = prior.PassHadWork
			x.checkpoint.PassHadCommit = prior.PassHadCommit
		}
	}
	if x.handoffs != nil && !x.native {
		prior := env.llmReq.ModelBoundary
		env.llmReq.ModelBoundary = func(ctx context.Context) error {
			if prior != nil {
				if err := prior(ctx); err != nil {
					return err
				}
			}
			x.markReady()
			return ctx.Err()
		}
	} else {
		x.ready = true
	}
	x.mu.Unlock()
	if x.handoffs == nil || x.native {
		x.manager.prepareLateLLMSource(x)
	}
}

// The existing model boundary runs after initial input persistence, not merely
// after engine construction. Freezing earlier could lose an uncommitted prompt.
func (x *jobsLLMExecution) markReady() {
	x.mu.Lock()
	x.ready = true
	x.mu.Unlock()
	x.manager.prepareLateLLMSource(x)
}

func (x *jobsLLMExecution) prepare(ctx context.Context, restartID, service string) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.finished {
		return nil
	}
	if x.prepared {
		if x.checkpoint.RestartID == restartID && x.checkpoint.Service == service {
			return nil
		}
		return errors.New("job already owns restart interruption")
	}
	if x.engine == nil || !x.ready {
		return nil
	} // bind will prepare this already-admitted source.
	if x.native {
		return errors.New("native job execution ownership is not yet checkpointable")
	}
	if x.handoffs == nil {
		return errors.New("job runtime has no durable restart checkpoint")
	}
	if x.ctx.Err() != nil {
		// An already timed-out/stopped job has no continuation to authorize,
		// but its detached finalization may still need to be cancelled.
		if x.passCancel != nil {
			x.passCancel()
		}
		return nil
	}
	owner := llm.SteeringTransition{OperationID: restartID + ":" + x.checkpoint.RunID, Fence: 1}
	pending, err := x.engine.FreezeExecutionSnapshot(owner)
	if err != nil {
		return err
	}
	c := x.checkpoint
	c.RestartID = restartID
	c.Service = service
	if c.SourceInstance == "" {
		c.SourceInstance = process.Instance()
	}
	c.Pending = pending
	if err = x.manager.prepareLLMRestart(ctx, c); err != nil {
		x.engine.ReleaseSteeringFreeze(owner, false)
		return err
	}
	x.checkpoint = c
	x.owner = owner
	x.prepared = true
	// Only a persisted internal intent authorizes this cancellation. CancelRun
	// remains separate and its durable status revokes later sealing/adoption.
	x.cancel()
	if x.passCancel != nil {
		x.passCancel()
	}
	return nil
}

// capture executes after the producer and actual tools have settled but while
// the session store is still open. Journal sealing waits for Run to return too,
// so provider/MCP cleanup still prevents replacement.
func (x *jobsLLMExecution) capture() {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.finished = true
	if !x.prepared {
		return
	}
	defer x.engine.ReleaseSteeringFreeze(x.owner, false)
	if x.saveErr != nil {
		return
	}
	if x.engine.ActiveToolExecutions() != 0 {
		x.saveErr = errors.New("job execution has not settled")
		return
	}
	raw, err := json.Marshal(x.checkpoint.Pending)
	if err != nil {
		x.saveErr = err
		return
	}
	id := uuid.NewString()
	x.saveErr = x.handoffs.SaveCommandHandoff(context.Background(), session.CommandHandoff{ID: id, Service: x.checkpoint.Service, SourceInstance: x.checkpoint.SourceInstance, SessionID: x.checkpoint.SessionID, Payload: raw})
	if x.saveErr == nil {
		x.savedID = id
	}
}

func (x *jobsLLMExecution) seal(result *jobsV2RunResult) (bool, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.engine != nil {
		result.TurnCount = max(result.TurnCount, int(x.engine.ModelTurns()-x.initialTurns))
	}
	if x.progressive && x.engine != nil {
		used := int(x.engine.ModelTurns() - x.passStartTurns)
		result.restartBudgetTurns = &used
		snapshot := progressivePassResult{hadNonProgressTool: x.checkpoint.PassHadWork}
		if x.checkpoint.PassHadCommit {
			snapshot.newCommitCount = 1
		}
		result.restartPass = &snapshot
	}
	if !x.prepared {
		return false, nil
	}
	if x.saveErr != nil {
		return false, x.saveErr
	}
	if !x.finished || x.savedID == "" {
		return false, errors.New("job source checkpoint was not captured")
	}
	if err := x.manager.sealLLMRestart(context.Background(), x.checkpoint.RunID, x.checkpoint.RestartID, x.savedID, *result); err != nil {
		return false, err
	}
	return true, nil
}

func (m *jobsV2Manager) interruptLLMJobs(ctx context.Context, restartID, service string) error {
	var result error
	m.llmExecutions.Range(func(_, value any) bool {
		if err := value.(*jobsLLMExecution).prepare(ctx, restartID, service); err != nil {
			result = fmt.Errorf("prepare LLM job interruption: %w", err)
			return false
		}
		return true
	})
	return result
}

// Each progressive pass has its own model-turn allowance. Its owned context
// also makes detached finalization cancellable by the process/job owner.
func (x *jobsLLMExecution) beginProgressivePass(parent context.Context, req llm.Request) (context.Context, func(), error) {
	x.mu.Lock()
	if x.prepared {
		x.mu.Unlock()
		return nil, nil, context.Canceled
	}
	ctx, cancel := context.WithCancel(parent)
	// Preserve normal deadline grace, but never let explicit user Stop keep
	// running a detached finalization pass.
	stopParent := context.AfterFunc(x.ctx, func() {
		if errors.Is(x.ctx.Err(), context.Canceled) {
			cancel()
		}
	})
	if errors.Is(x.ctx.Err(), context.Canceled) {
		cancel()
	}
	x.passGeneration++
	if x.passGeneration > 1 {
		x.checkpoint.PassHadWork = false
		x.checkpoint.PassHadCommit = false
	}
	generation := x.passGeneration
	x.passCancel = cancel
	x.passStartTurns = x.engine.ModelTurns()
	x.checkpoint.RemainingTurns = req.TurnLimit()
	x.mu.Unlock()
	return ctx, func() {
		stopParent()
		cancel()
		x.mu.Lock()
		if x.passGeneration == generation {
			x.passCancel = nil
		}
		x.mu.Unlock()
	}, nil
}

func (x *jobsLLMExecution) beginFinalization(reason string, req llm.Request) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.prepared {
		return context.Canceled
	}
	x.checkpoint.FinalizationReason = reason
	x.checkpoint.RemainingTurns = req.TurnLimit()
	x.passStartTurns = x.engine.ModelTurns()
	return nil
}

func (x *jobsLLMExecution) endProgressivePass(result progressivePassResult) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.checkpoint.PassHadWork = x.checkpoint.PassHadWork || result.hadNonProgressTool
	x.checkpoint.PassHadCommit = x.checkpoint.PassHadCommit || result.newCommitCount > 0
}

func (x *jobsLLMExecution) failCheckpoint(err error) {
	if err == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.saveErr == nil {
		x.saveErr = fmt.Errorf("incomplete source persistence: %w", err)
	}
}
