package cmd

import (
	"context"
	"sync"
	"time"
)

const asyncCallbackQueueTimeout = 5 * time.Second

type asyncCallbackQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	jobs   []func()
	closed bool
	wg     sync.WaitGroup
	once   sync.Once
}

func newAsyncCallbackQueue() *asyncCallbackQueue {
	q := &asyncCallbackQueue{}
	q.cond = sync.NewCond(&q.mu)
	go q.run()
	return q
}

func (q *asyncCallbackQueue) run() {
	for {
		q.mu.Lock()
		for len(q.jobs) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.jobs) == 0 && q.closed {
			q.mu.Unlock()
			return
		}
		job := q.jobs[0]
		q.jobs[0] = nil
		q.jobs = q.jobs[1:]
		q.mu.Unlock()

		job()
		q.wg.Done()
	}
}

func (q *asyncCallbackQueue) Enqueue(job func()) bool {
	if q == nil || job == nil {
		return true
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	q.jobs = append(q.jobs, job)
	q.wg.Add(1)
	q.cond.Signal()
	return true
}

func (q *asyncCallbackQueue) Drain() {
	if q == nil {
		return
	}
	q.once.Do(func() {
		q.mu.Lock()
		q.closed = true
		q.cond.Broadcast()
		q.mu.Unlock()
		q.wg.Wait()
	})
}

func asyncCallbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), asyncCallbackQueueTimeout)
}
