package serve

import (
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/restart"
)

const hubAttentionRoundBudget = 8 * time.Second
const hubAttentionPersistenceBudget = 500 * time.Millisecond

// Only failures have a retry eligibility time. Healthy nodes are collected on
// every base tick, independently of the failure streak of any other node.
type hubAttentionRetry struct {
	failures int
	next     time.Time
	deferred bool
}
type hubAttentionSchedule struct {
	mu      sync.Mutex
	retries map[string]hubAttentionRetry
	offset  int
	now     func() time.Time
}

func (s *hubAttentionSchedule) timeNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
func (s *hubAttentionSchedule) eligible(nodes []hub.Node) []hub.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.timeNow()
	current := make(map[string]bool, len(nodes))
	due := make([]hub.Node, 0, len(nodes))
	for _, node := range nodes {
		current[node.ID] = true
		if !now.Before(s.retries[node.ID].next) {
			due = append(due, node)
		}
	}
	for id := range s.retries {
		if !current[id] {
			delete(s.retries, id)
		}
	}
	if len(due) > 0 {
		// Rotate unattempted nodes fairly when a large fleet fills a whole round.
		start := s.offset % len(due)
		due = append(append(make([]hub.Node, 0, len(due)), due[start:]...), due[:start]...)
		s.offset = (start + 8) % len(due)
	}
	// A truncated attempt needs an initial slot next time. Do not sort by
	// failure count: that would undo rotation and starve recovered peers.
	sort.SliceStable(due, func(i, j int) bool { return s.retries[due[i].ID].deferred && !s.retries[due[j].ID].deferred })
	return due
}
func (s *hubAttentionSchedule) deferNode(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retries == nil {
		s.retries = make(map[string]hubAttentionRetry)
	}
	state := s.retries[id]
	state.deferred = true
	s.retries[id] = state
}

func (s *hubAttentionSchedule) record(id string, err error, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.retries, id)
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, errHubAttentionAcknowledged) {
		return
	}
	if s.retries == nil {
		s.retries = make(map[string]hubAttentionRetry)
	}
	state := s.retries[id]
	state.deferred = false
	state.failures = min(state.failures+1, 3)
	state.next = s.timeNow().Add(hubAttentionCollectionDelay(active, state.failures))
	s.retries[id] = state
}

// Collection retains a bounded round barrier (at most eight seconds of node
// I/O, plus bounded local persistence). It no longer multiplies that budget by
// fleet size, launches a goroutine for each queued node, or backs off healthy
// nodes. Atomic node snapshots and acknowledgement generations remain owned by
// collectNodeAttention, not this scheduler.
func (s *hubServer) collectAttention(ctx context.Context) int {
	ctx, release, err := restart.Default.Activity(ctx)
	if err != nil {
		return 0
	}
	defer release()
	roundCtx, cancel := context.WithTimeout(ctx, hubAttentionRoundBudget)
	defer cancel()
	nodes, _ := s.registry.Nodes()
	due := s.attentionSchedule.eligible(nodes)
	active, _ := s.attentionStore.HasActivity(roundCtx)
	type collectionJob struct {
		node       hub.Node
		fullBudget bool
	}
	jobs := make(chan collectionJob)
	var wg sync.WaitGroup
	var failures atomic.Int64
	workers := min(8, len(due))
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				node := job.node
				if roundCtx.Err() != nil {
					return
				}
				started := time.Now()
				err := s.collectNodeAttention(roundCtx, node)
				s.attentionDiagnostics.CollectorSamples.Add(1)
				s.attentionDiagnostics.CollectorLatencyMS.Add(uint64(max(time.Since(started).Milliseconds(), 0)))
				if !job.fullBudget && roundCtx.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
					// Only the initial admission batch gets the full round window.
					// A later sync can use spare time, but exhausting that partial
					// window is scheduler deferral, not evidence of node failure.
					s.attentionSchedule.deferNode(node.ID)
					continue
				}
				s.attentionSchedule.record(node.ID, err, active)
				if err == nil {
					s.attentionDiagnostics.CollectorSuccesses.Add(1)
					continue
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, errHubAttentionAcknowledged) {
					continue
				}
				s.attentionDiagnostics.CollectorFailures.Add(1)
				failures.Add(1)
				summary := hubAttentionErrorSummary(err)
				// Persist a timeout after its network budget expires, but never wait on
				// storage forever or mask host shutdown with context.Background().
				persistCtx, persistCancel := context.WithTimeout(ctx, hubAttentionPersistenceBudget)
				_ = s.attentionStore.MarkError(persistCtx, node.ID, summary)
				persistCancel()
				log.Printf("[hub] attention sync failed for %.12s: %s", node.ID, summary)
			}
		}()
	}
outer:
	for index, node := range due {
		select {
		case <-roundCtx.Done():
			break outer
		case jobs <- collectionJob{node: node, fullBudget: index < workers}:
		}
	}
	close(jobs)
	wg.Wait()
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, hubAttentionPersistenceBudget)
	defer cleanupCancel()
	current := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		current[node.ID] = true
	}
	_, syncs, err := s.attentionStore.List(cleanupCtx)
	if err == nil {
		for _, state := range syncs {
			if !current[state.NodeID] {
				_ = s.attentionStore.RemoveNode(cleanupCtx, state.NodeID)
			}
		}
	}
	return int(failures.Load())
}
