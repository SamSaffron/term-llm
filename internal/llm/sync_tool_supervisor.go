package llm

import (
	"context"
	"fmt"
	"sync"
)

// syncToolSupervisor owns synchronous bridge calls for one provider turn. A
// FIFO worker queue makes serial mode truly serial while parallel mode uses the
// same bounded worker count as API-provider batches. Dispatch only appends to
// the in-memory queue, so the provider event loop never waits for tool capacity.
type syncToolSupervisor struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	ready   *sync.Cond
	jobs    []syncToolJob
	entries []*syncToolSupervisorEntry
}

type syncToolJob struct {
	entry *syncToolSupervisorEntry
	run   func(context.Context) toolCallOutcome
}

type syncToolSupervisorEntry struct {
	call      ToolCall
	outcome   toolCallOutcome
	done      chan struct{}
	respond   func(toolCallOutcome)
	finalized bool
}

func newSyncToolSupervisor(parent context.Context, parallel bool) *syncToolSupervisor {
	workerCount := 1
	if parallel {
		workerCount = defaultMaxParallelToolCalls
	}
	ctx, cancel := context.WithCancel(parent)
	s := &syncToolSupervisor{ctx: ctx, cancel: cancel}
	s.ready = sync.NewCond(&s.mu)
	context.AfterFunc(ctx, func() {
		s.mu.Lock()
		s.ready.Broadcast()
		s.mu.Unlock()
	})
	for range workerCount {
		go s.worker()
	}
	return s
}

func (s *syncToolSupervisor) worker() {
	for {
		s.mu.Lock()
		for len(s.jobs) == 0 && s.ctx.Err() == nil {
			s.ready.Wait()
		}
		if s.ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		job := s.jobs[0]
		s.jobs[0] = syncToolJob{}
		s.jobs = s.jobs[1:]
		s.mu.Unlock()

		outcome := func() (outcome toolCallOutcome) {
			defer func() {
				if recovered := recover(); recovered != nil {
					outcome = toolCallOutcome{call: job.entry.call, err: fmt.Errorf("sync tool execution panicked: %v", recovered)}
				}
			}()
			return job.run(s.ctx)
		}()
		s.finish(job.entry, outcome)
	}
}

// finish chooses the single terminal outcome observed by both the engine and
// the bridge. A non-cooperative worker that returns after cancellation cannot
// overwrite or contradict an outcome already published by settle or abort.
func (s *syncToolSupervisor) finish(entry *syncToolSupervisorEntry, outcome toolCallOutcome) {
	s.mu.Lock()
	if entry.finalized {
		s.mu.Unlock()
		return
	}
	entry.outcome = outcome
	entry.finalized = true
	respond := entry.respond
	s.mu.Unlock()

	// Bridge delivery is part of settlement: stream teardown must not race ahead
	// of the terminal response selected for the caller. Production callbacks use
	// buffered channels and cancellation-aware sends.
	func() {
		defer func() { _ = recover() }()
		respond(outcome)
	}()

	s.mu.Lock()
	close(entry.done)
	s.mu.Unlock()
}

func (s *syncToolSupervisor) dispatch(call ToolCall, run func(context.Context) toolCallOutcome, respond func(toolCallOutcome)) {
	entry := &syncToolSupervisorEntry{call: call, done: make(chan struct{}), respond: respond}
	s.mu.Lock()
	s.entries = append(s.entries, entry)
	if err := s.ctx.Err(); err != nil {
		s.mu.Unlock()
		s.finish(entry, toolCallOutcome{call: call, err: err})
		return
	}
	s.jobs = append(s.jobs, syncToolJob{entry: entry, run: run})
	s.ready.Signal()
	s.mu.Unlock()
}

func (s *syncToolSupervisor) pendingCalls() []ToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := make([]ToolCall, len(s.entries))
	for i, entry := range s.entries {
		calls[i] = entry.call
	}
	return calls
}

// evidenceFor returns this call and its still-supervised predecessors, plus
// outcomes that completed before execution began. Serial bridge calls therefore
// retain the same causal approval evidence as the former inline path.
func (s *syncToolSupervisor) evidenceFor(callID string) ([]ToolCall, []toolCallOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var calls []ToolCall
	var outcomes []toolCallOutcome
	for _, entry := range s.entries {
		calls = append(calls, entry.call)
		if entry.call.ID == callID {
			break
		}
		if entry.finalized {
			outcomes = append(outcomes, entry.outcome)
		}
	}
	return calls, outcomes
}

// settleCompleted consumes only a finalized prefix so transcript results remain
// in model dispatch order even when later parallel calls finish first.
func (s *syncToolSupervisor) settleCompleted() []toolCallOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for count < len(s.entries) {
		select {
		case <-s.entries[count].done:
			count++
		default:
			return s.consumeLocked(count)
		}
	}
	return s.consumeLocked(count)
}

func (s *syncToolSupervisor) settle(ctx context.Context) []toolCallOutcome {
	s.mu.Lock()
	entries := append([]*syncToolSupervisorEntry(nil), s.entries...)
	s.mu.Unlock()
	cancelled := false
	for i, entry := range entries {
		select {
		case <-entry.done:
		case <-ctx.Done():
			s.cancel()
			for _, remaining := range entries[i:] {
				s.finish(remaining, toolCallOutcome{call: remaining.call, err: ctx.Err()})
			}
			cancelled = true
		}
		if cancelled {
			break
		}
	}
	s.cancel()
	outcomes := make([]toolCallOutcome, len(entries))
	s.mu.Lock()
	for i, entry := range entries {
		outcomes[i] = entry.outcome
	}
	s.entries = nil
	s.jobs = nil
	s.mu.Unlock()
	return outcomes
}

func (s *syncToolSupervisor) abort(cause error) []toolCallOutcome {
	s.cancel()
	s.mu.Lock()
	entries := append([]*syncToolSupervisorEntry(nil), s.entries...)
	s.mu.Unlock()
	for _, entry := range entries {
		s.finish(entry, toolCallOutcome{call: entry.call, err: cause})
	}
	for _, entry := range entries {
		<-entry.done
	}
	outcomes := make([]toolCallOutcome, len(entries))
	s.mu.Lock()
	for i, entry := range entries {
		outcomes[i] = entry.outcome
	}
	s.entries = nil
	s.jobs = nil
	s.mu.Unlock()
	return outcomes
}

func (s *syncToolSupervisor) consumeLocked(count int) []toolCallOutcome {
	if count == 0 {
		return nil
	}
	outcomes := make([]toolCallOutcome, count)
	for i := range count {
		outcomes[i] = s.entries[i].outcome
	}
	s.entries = append([]*syncToolSupervisorEntry(nil), s.entries[count:]...)
	return outcomes
}
