package restart

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrInterrupt is the cause of a reload's bounded, cooperative cancellation.
// It is distinct from a user's cancellation and never releases execution tickets.
var ErrInterrupt = errors.New("operation interrupted to reach a restart safe point")

type taskKey struct{}

// Task marks an invocation whose adapter can save and restore continuation state.
// Ordinary admitted work is not cancelled merely because a Task exists elsewhere.
// Every Step must be joined before its adapter relinquishes the invocation.
type Task struct {
	coordinator    *Coordinator
	steps          map[*taskStep]struct{}
	closed         bool
	owner          any
	suspendAllowed func() bool
	onRequest      func()
}
type taskStep struct{ cancel context.CancelCauseFunc }

func (c *Coordinator) NewTask(ctx context.Context) (context.Context, func()) {
	task := &Task{coordinator: c, steps: make(map[*taskStep]struct{})}
	c.mu.Lock()
	if c.tasks == nil {
		c.tasks = make(map[*Task]struct{})
	}
	c.tasks[task] = struct{}{}
	c.mu.Unlock()
	var once sync.Once
	return context.WithValue(ctx, taskKey{}, task), func() {
		once.Do(func() {
			c.mu.Lock()
			task.closed = true
			delete(c.tasks, task)
			c.mu.Unlock()
		})
	}
}

func CurrentTask(ctx context.Context) *Task { task, _ := ctx.Value(taskKey{}).(*Task); return task }
func (t *Task) Pending() bool {
	if t == nil {
		return false
	}
	t.coordinator.mu.Lock()
	pending := !t.closed && t.coordinator.draining
	allowed := t.suspendAllowed
	t.coordinator.mu.Unlock()
	return pending && (allowed == nil || allowed())
}

// Claim limits checkpoints to one execution owner. Nested helper engines may
// inherit cancellation, but must not return checkpoints their caller cannot save.
// owner must be a comparable identity (normally an engine pointer).
func (t *Task) Claim(owner any) bool {
	if t == nil {
		return false
	}
	t.coordinator.mu.Lock()
	defer t.coordinator.mu.Unlock()
	if t.owner == nil {
		t.owner = owner
	}
	return !t.closed && t.owner == owner
}

func (t *Task) SetSuspendAllowed(allowed func() bool) {
	t.coordinator.mu.Lock()
	t.suspendAllowed = allowed
	t.coordinator.mu.Unlock()
}

// Step gives a provider/tool phase its own cancellation lifetime. Persisting a
// completed checkpoint uses the original invocation context, not this context.
func (t *Task) Step(ctx context.Context) (context.Context, func()) {
	if t == nil {
		return ctx, func() {}
	}
	stepCtx, cancel := context.WithCancelCause(ctx)
	step := &taskStep{cancel: cancel}
	c := t.coordinator
	c.mu.Lock()
	if t.closed {
		cancel(context.Canceled)
	} else {
		t.steps[step] = struct{}{}
		if c.interrupting {
			cancel(ErrInterrupt)
		}
	}
	c.mu.Unlock()
	var once sync.Once
	return stepCtx, func() {
		once.Do(func() { c.mu.Lock(); delete(t.steps, step); c.mu.Unlock(); cancel(context.Canceled) })
	}
}

func (c *Coordinator) interruptTasks(done <-chan struct{}) {
	c.mu.Lock()
	if c.done != done || !c.draining || c.status.Phase == "replacing" {
		c.mu.Unlock()
		return
	}
	c.interrupting = true
	var cancel []context.CancelCauseFunc
	for task := range c.tasks {
		for step := range task.steps {
			cancel = append(cancel, step.cancel)
		}
	}
	if len(cancel) > 0 {
		c.publish("cancelling")
	}
	c.mu.Unlock()
	for _, fn := range cancel {
		fn(ErrInterrupt)
	}
	// Diagnostics must not delay sending the cancellation requests.
	c.reportBlockers(done)
}

func (c *Coordinator) interruptionDelay() time.Duration {
	if c.InterruptAfter > 0 {
		return c.InterruptAfter
	}
	return 30 * time.Second
}

// Cancellable opts a non-resumable operation into the grace-period cancellation
// policy. Its descendants keep ordinary ownership accounting, but do not inherit
// a claim that their execution can be checkpointed and resumed.
func (c *Coordinator) Cancellable(ctx context.Context) (context.Context, func()) {
	taskCtx, closeTask := c.NewTask(ctx)
	stepCtx, closeStep := CurrentTask(taskCtx).Step(ctx)
	return stepCtx, func() { closeStep(); closeTask() }
}

// OnRequest installs a nonblocking request notification (for example, asking an
// inline provider to yield at its next tool result, without cancelling it).
func (t *Task) OnRequest(notify func()) {
	t.coordinator.mu.Lock()
	t.onRequest = notify
	pending := !t.closed && t.coordinator.draining
	t.coordinator.mu.Unlock()
	if pending && notify != nil {
		notify()
	}
}

func (c *Coordinator) requestTaskBoundaries(done <-chan struct{}) {
	c.mu.Lock()
	var notify []func()
	if c.done == done {
		for task := range c.tasks {
			if task.onRequest != nil {
				notify = append(notify, task.onRequest)
			}
		}
	}
	c.mu.Unlock()
	for _, fn := range notify {
		fn()
	}
}

func (t *Task) Release(owner any) {
	if t == nil {
		return
	}
	t.coordinator.mu.Lock()
	if t.owner == owner {
		t.owner = nil
	}
	t.coordinator.mu.Unlock()
}
