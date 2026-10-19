package cmd

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/restart"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	// childLifecycleReserveTurns is the lifecycle-only allowance a delegated
	// child spends once its ordinary turn allowance is exhausted. It is
	// deliberately small: enough to collect finished children and report them,
	// never a second working budget. Turns spent under it cannot spawn or
	// continue children.
	childLifecycleReserveTurns = 4

	// childEngineDefaultMaxTurns mirrors the engine's unexported default for a
	// request that carries no MaxTurns. A barrier turn must be charged against
	// the same cumulative allowance as the child's own turns.
	childEngineDefaultMaxTurns = 50
)

// childLifecycleRetryBackoff paces retries of an undelivered lifecycle notice.
// Undelivered results have no owner once the child ends, so the last delay
// repeats instead of giving up: a transient outage must not strand them.
var childLifecycleRetryBackoff = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// childLifecycleTurn is exactly one inference stream on a delegated child's
// runtime: the single-turn operation shared by the CLI path (cmdRunner.Run) and
// the hosted path (childRunHandle.Execute), so both drive identical lifecycle
// turns. own is true only for the child's own first turn; later turns must not
// replay history replacement.
type childLifecycleTurn func(ctx context.Context, own bool, messages []core.Message, req core.Request, onEvent func(core.Event) error) (serveRunResult, error)

// childLifecycleSink is implemented by output sinks that accumulate one turn's
// output. Resetting at a turn boundary keeps a child's reported response the
// turn that produced it instead of every turn concatenated.
type childLifecycleSink interface {
	BeginChildTurn()
}

// childLifecycleBudget is the cumulative turn allowance of one delegated run.
// Barrier turns spend the same allowance as the child's own turns: a new stream
// must never silently restart it.
type childLifecycleBudget struct {
	ordinary int
	reserve  int
}

func newChildLifecycleBudget(limit, used int) childLifecycleBudget {
	if limit <= 0 {
		limit = childEngineDefaultMaxTurns
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return childLifecycleBudget{ordinary: remaining, reserve: childLifecycleReserveTurns}
}

// next reports the turn limit of the next barrier turn and whether that turn is
// a restricted lifecycle-only turn.
func (b *childLifecycleBudget) next() (int, bool, bool) {
	if b.ordinary > 0 {
		return b.ordinary, false, true
	}
	if b.reserve <= 0 {
		return 0, true, false
	}
	return b.reserve, true, true
}

// spend charges finished inference to the allowance.
func (b *childLifecycleBudget) spend(turns int) {
	if turns <= 0 {
		return
	}
	if b.ordinary > 0 {
		if b.ordinary -= turns; b.ordinary < 0 {
			b.ordinary = 0
		}
		return
	}
	if b.reserve -= turns; b.reserve < 0 {
		b.reserve = 0
	}
}

// childLifecycleCharge reports what an attempt spent. Real inference is charged
// in full; an attempt that exhausted its own limit is charged that limit; a
// failure that produced no usage is free, so a provider outage cannot drain the
// lifecycle reserve that the guarantee depends on.
func childLifecycleCharge(turns, limit int, err error) int {
	if turns > 0 {
		return turns
	}
	if err != nil && isChildTurnLimit(err) && limit > 0 {
		return limit
	}
	return 0
}

// childLifecycleTurnOutcome summarizes a delegated run so far: the latest
// meaningful response plus aggregate usage over every turn it spent.
type childLifecycleTurnOutcome struct {
	response string
	thinking string
	turns    int
	input    int
	output   int
}

func newChildLifecycleTurnOutcome(collector *runnerEventCollector, res serveRunResult) childLifecycleTurnOutcome {
	response := collector.Response()
	if response == "" {
		response = res.Text.String()
	}
	return childLifecycleTurnOutcome{
		response: response,
		thinking: collector.Thinking(),
		turns:    collector.turns,
		input:    collector.input,
		output:   collector.output,
	}
}

// adopt folds a later attempt into the totals and reports its own output. A
// failed attempt that produced no text must not erase the last meaningful
// report.
func (o *childLifecycleTurnOutcome) adopt(latest childLifecycleTurnOutcome) {
	if strings.TrimSpace(latest.response) == "" {
		latest.response = o.response
	}
	if strings.TrimSpace(latest.thinking) == "" {
		latest.thinking = o.thinking
	}
	latest.turns += o.turns
	latest.input += o.input
	latest.output += o.output
	*o = latest
}

// runChildLifecycleTurns runs a child's own turn and then the host-owned
// lifecycle turns that deliver the results of every finished direct child. The
// child stays live until its local children finish, so a model that forgets
// wait_agent can never strand them.
func (env *cmdRunEnvironment) runChildLifecycleTurns(ctx context.Context, sink runpkg.EventSink, turn childLifecycleTurn) (runpkg.Result, error) {
	boundary, _ := sink.(childLifecycleSink)

	first := env.runChildLifecycleTurn(ctx, true, sink, turn, env.inputMessages, env.llmReq)
	latest := first.outcome
	runErr := first.err

	if !env.childLifecycleEnabled() || !childLifecycleContinues(ctx, first.err) {
		return env.childRunResult(latest), runErr
	}

	limit := env.childTurnLimit()
	budget := newChildLifecycleBudget(limit, childLifecycleCharge(first.outcome.turns, limit, first.err))
	for {
		events, ok := env.awaitChildBarrierState(ctx)
		if !ok {
			// The child was stopped while it waited for its children: the stop is
			// the run's outcome and its results stay pending for the host.
			runErr = joinRunErrors(runErr, ctx.Err())
			break
		}
		if len(events) == 0 {
			break
		}
		outcome, delivered, cause := env.deliverChildLifecycleNotice(ctx, sink, turn, boundary, &budget, events)
		latest.adopt(outcome)
		runErr = joinRunErrors(runErr, cause)
		if !delivered {
			// Undelivered generations stay pending for the owning host. A stopped
			// run must not keep the child alive.
			break
		}
		// A delivered barrier turn may have delegated more work; the next pass
		// waits for those children.
	}
	return env.childRunResult(latest), runErr
}

// runChildLifecycleTurn executes one turn and records its outcome.
func (env *cmdRunEnvironment) runChildLifecycleTurn(ctx context.Context, own bool, sink runpkg.EventSink, turn childLifecycleTurn, messages []core.Message, req core.Request) childLifecycleTurnResult {
	collector := &runnerEventCollector{sink: sink}
	res, err := turn(ctx, own, messages, req, collector.Event)
	return childLifecycleTurnResult{outcome: newChildLifecycleTurnOutcome(collector, res), err: err}
}

type childLifecycleTurnResult struct {
	outcome childLifecycleTurnOutcome
	err     error
}

// deliverChildLifecycleNotice drives one pending set to delivery. Ordinary
// failures are retried while the child is live and uncancelled, recomputing the
// remaining allowance (and therefore the restriction) before every attempt, and
// the offered generations are acknowledged only once a turn actually delivered
// them.
func (env *cmdRunEnvironment) deliverChildLifecycleNotice(ctx context.Context, sink runpkg.EventSink, turn childLifecycleTurn, boundary childLifecycleSink, budget *childLifecycleBudget, events []session.AgentRun) (childLifecycleTurnOutcome, bool, error) {
	notice := []core.Message{tools.AgentLifecycleMessage(events, true)}
	var outcome childLifecycleTurnOutcome
	for attempt := 0; ; attempt++ {
		if attempt > 0 && !sleepChildLifecycleRetry(ctx, attempt-1) {
			return outcome, false, ctx.Err()
		}
		limit, restricted, ok := budget.next()
		if !ok {
			// The lifecycle reserve is spent: events stay pending for the host.
			return outcome, false, nil
		}
		if boundary != nil {
			boundary.BeginChildTurn()
		}
		env.resetChildOutputCapture()
		result := env.runChildLifecycleTurn(env.childLifecycleTurnContext(ctx, restricted), false, sink, turn, notice, env.childLifecycleRequest(limit))
		budget.spend(childLifecycleCharge(result.outcome.turns, limit, result.err))
		outcome.adopt(result.outcome)
		switch {
		case result.err == nil:
			// The results have been delivered to the child: only the durable
			// acknowledgement is retried, never another inference.
			if env.acknowledgeChildBarrier(ctx, events) {
				return outcome, true, nil
			}
			return outcome, false, ctx.Err()
		case isChildTurnLimit(result.err):
			// A spent allowance still delivered the notice: the provider inferred
			// on it and its tool calls and results are persisted. Acknowledge the
			// offered generations, keep the typed limit as the run's outcome, and
			// let the next pass wait for any child this turn started.
			if env.acknowledgeChildBarrier(ctx, events) {
				return outcome, true, result.err
			}
			return outcome, false, result.err
		case ctx.Err() != nil:
			return outcome, false, joinRunErrors(result.err, ctx.Err())
		case errors.Is(result.err, restart.ErrInterrupt):
			// The host is reloading; the child ends resumable instead of
			// holding the reload open.
			return outcome, false, result.err
		}
	}
}

// awaitChildBarrierState waits for every local child of this session to finish
// and reads the pending set. State failures are retried without inference: rows
// that cannot be read would otherwise be stranded when the child ends.
func (env *cmdRunEnvironment) awaitChildBarrierState(ctx context.Context) ([]session.AgentRun, bool) {
	for attempt := 0; ; attempt++ {
		if waitErr := env.waitForChildRuns(ctx); waitErr != nil {
			if ctx.Err() != nil {
				return nil, false
			}
			if !sleepChildLifecycleRetry(ctx, attempt) {
				return nil, false
			}
			continue
		}
		events, err := env.pendingChildBarrier(ctx)
		if err == nil {
			return events, true
		}
		runtimeoutput.Logf("[agents] child %s barrier query failed: %v", env.req.SessionID, err)
		if !sleepChildLifecycleRetry(ctx, attempt) {
			return nil, false
		}
	}
}

// resetChildOutputCapture forgets structured output the child captured before a
// lifecycle turn. A finishing output tool can end the child's own turn with a
// capture, and a lifecycle turn that reports in prose must not be answered with
// that stale value instead of its own report.
func (env *cmdRunEnvironment) resetChildOutputCapture() {
	if env == nil || env.engine == nil {
		return
	}
	registry := env.engine.Tools()
	if registry == nil {
		return
	}
	for _, spec := range registry.AllSpecsIncludingDeferred() {
		tool, ok := registry.Get(spec.Name)
		if !ok {
			continue
		}
		if output, ok := tool.(*tools.SetOutputTool); ok {
			output.Reset()
		}
	}
}

// childLifecycleTurnContext marks a barrier turn as host-authored recovery. An
// exhausted allowance additionally restricts the turn to collecting and
// reporting: it may not create or continue work.
func (env *cmdRunEnvironment) childLifecycleTurnContext(ctx context.Context, restricted bool) context.Context {
	ctx = tools.ContextWithAgentRecoveryNotice(ctx)
	if restricted {
		ctx = tools.ContextWithAgentLifecycleRestricted(ctx)
	}
	return ctx
}

// childTurnLimit is the child's ordinary allowance for this run, mirroring the
// engine's default when the request carries none.
func (env *cmdRunEnvironment) childTurnLimit() int {
	limit := env.llmReq.MaxTurns
	if limit <= 0 {
		limit = childEngineDefaultMaxTurns
	}
	return limit
}

// childLifecycleRequest derives the request of a barrier turn from the child's
// own request. Tools, approvals and ownership stay unchanged; the allowance is
// the remainder of the child's cumulative budget and no consumed provider
// continuation is replayed.
func (env *cmdRunEnvironment) childLifecycleRequest(limit int) core.Request {
	req := env.llmReq
	req.Messages = nil
	req.Resume = nil
	req.MaxTurns = limit
	return req
}

// childLifecycleEnabled reports whether this run owns a barrier at all. Only a
// delegated child with a live runtime can wait for and report its children.
func (env *cmdRunEnvironment) childLifecycleEnabled() bool {
	return env != nil && env.req.IsSubagent && env.runtime != nil && strings.TrimSpace(env.req.SessionID) != ""
}

// childLifecycleContinues reports whether barrier turns may run after the
// child's own turn. Stopped or unadmitted children deliver nothing; an admitted
// turn that failed otherwise still owes delivery to the descendants it created.
func childLifecycleContinues(ctx context.Context, err error) bool {
	var admission *tools.AgentRunAdmissionError
	if ctx.Err() != nil || errors.As(err, &admission) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, restart.ErrInterrupt) {
		return false
	}
	return true
}

// sleepChildLifecycleRetry waits out the retry schedule. It reports false only
// when the child was stopped while waiting.
func sleepChildLifecycleRetry(ctx context.Context, attempt int) bool {
	delay := childLifecycleRetryBackoff[len(childLifecycleRetryBackoff)-1]
	if attempt < len(childLifecycleRetryBackoff) {
		delay = childLifecycleRetryBackoff[attempt]
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// waitForChildRuns blocks until every local child of this session has finished.
// The runtime's own spawn tool owns those entries, whatever wiring path
// installed its runner, so its non-draining wait is authoritative.
func (env *cmdRunEnvironment) waitForChildRuns(ctx context.Context) error {
	if env == nil || env.runtime == nil {
		return nil
	}
	parent := strings.TrimSpace(env.req.SessionID)
	if parent == "" {
		return nil
	}
	if env.runtime.toolMgr != nil {
		if tool := env.runtime.toolMgr.GetSpawnAgentTool(); tool != nil {
			return tool.WaitForChildren(ctx, parent)
		}
	}
	if env.runtime.spawnRunner != nil {
		return env.runtime.spawnRunner.WaitForChildren(ctx, parent)
	}
	return nil
}

func (env *cmdRunEnvironment) pendingChildBarrier(ctx context.Context) ([]session.AgentRun, error) {
	parent := strings.TrimSpace(env.req.SessionID)
	if parent == "" {
		return nil, nil
	}
	return tools.PendingAgentBarrier(ctx, env.agentRunDelivery(), parent)
}

// acknowledgeChildBarrier acknowledges the offered generations. The turn already
// ran, so a cancelled run context must not turn a delivered result back into a
// pending wake; failures are retried until the child is stopped.
func (env *cmdRunEnvironment) acknowledgeChildBarrier(ctx context.Context, events []session.AgentRun) bool {
	for attempt := 0; ; attempt++ {
		ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := tools.AcknowledgeAgentEvents(ackCtx, env.agentRunDelivery(), events)
		cancel()
		if err == nil {
			return true
		}
		runtimeoutput.Logf("[agents] child %s barrier acknowledge failed: %v", env.req.SessionID, err)
		if !sleepChildLifecycleRetry(ctx, attempt) {
			return false
		}
	}
}

// agentRunDelivery is the durable delivery view of the runner's store. It is nil
// when the run has no store, and the shared barrier helpers then work from local
// process entries only.
func (env *cmdRunEnvironment) agentRunDelivery() session.AgentRunDeliveryStore {
	if env == nil {
		return nil
	}
	return session.AsAgentRunDeliveryStore(env.store)
}

// childRunResult summarizes the delegated run. Only the latest meaningful
// response is reported; usage covers every turn the run spent.
func (env *cmdRunEnvironment) childRunResult(latest childLifecycleTurnOutcome) runpkg.Result {
	result := runpkg.Result{
		SessionID:        env.req.SessionID,
		Response:         latest.response,
		Thinking:         latest.thinking,
		Turns:            latest.turns,
		InputTokens:      latest.input,
		OutputTokens:     latest.output,
		Engine:           env.engine,
		Model:            env.modelName,
		ProviderInstance: env.provider,
	}
	if env.provider != nil {
		result.Provider = env.provider.Name()
	}
	return result
}

// joinRunErrors keeps every terminal cause visible without losing the typed
// classification the caller uses for status (turn limit, cancellation).
func joinRunErrors(existing, additional error) error {
	switch {
	case existing == nil:
		return additional
	case additional == nil:
		return existing
	default:
		return errors.Join(existing, additional)
	}
}
