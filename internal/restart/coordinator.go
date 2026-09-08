// Package restart coordinates safe-point process replacement. Admission and
// actual execution ownership outlive cancellation; eligible steps receive a
// grace-period interrupt while reload remains pending until settlement.
package restart

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

var ErrDraining = errors.New("process is draining for reload; retry shortly")
var ErrReleased = errors.New("reload operation has already finished")

// Status separates current availability from the last attempt's result. A failed
// attempt returns to ready without erasing its error before a waiter can see it.
type Status struct {
	Phase   string
	Attempt uint64
	Error   string
}

type binding struct {
	stopping bool
	ctx      context.Context
	replace  func(context.Context) error
}

// Coordinator is both the admission fence and the single replacement owner.
// The zero value is usable. Observe must return promptly and not call back into
// the coordinator. Configure Timeout and Observe before admitting work/binding.
type Coordinator struct {
	InterruptAfter time.Duration // Grace before cancelling opted-in task steps; default 30 seconds.
	tasks          map[*Task]struct{}
	interrupting   bool
	Timeout        time.Duration // Optional drain limit; zero waits until owned work finishes.
	Observe        func(Status)
	Diagnostic     func(string, []Blocker) // Optional slow-drain log; configure before Bind. Called outside mu.
	mu             sync.Mutex
	active         int
	operations     map[*operation]struct{}
	idle           chan struct{}
	owner          *binding
	pause          chan struct{}
	pending        bool
	draining       bool
	status         Status
	done           chan struct{}
	cancel         context.CancelFunc
	resources      map[*Resource]struct{}
	beforeExec     func() func() // installed by Listen; preserves SIGUSR2 across exec
}

var Default = &Coordinator{}

type operationKey struct{}
type operation struct {
	coordinator *Coordinator
	released    bool // guarded by coordinator.mu
	source      string
	started     time.Time
}

// Blocker summarizes outstanding ownership without retaining request contents,
// arguments, URLs or credentials. Source is the admission's source file/line.
type Blocker struct {
	Source string
	Count  int
	Oldest time.Duration
}

// Enter admits a root or a child of the live operation in ctx. A child must be
// acquired BEFORE launching its goroutine or releasing its parent. This makes
// detached work visible even after its caller/stream returns on cancellation.
// The returned release must outlive effects, persistence and cleanup. Cancellation
// of ctx never releases ownership. An expired parent cannot admit late children.
func (c *Coordinator) Enter(ctx context.Context) (context.Context, func(), error) {
	return c.admit(ctx, false, false)
}

// Root admits a new operation even when ctx belongs to a control request.
func (c *Coordinator) Root(ctx context.Context) (context.Context, func(), error) {
	return c.admit(ctx, false, true)
}

// Activity accounts for control and maintenance work during drain (lease renewals,
// approvals, final delivery). It cannot start after the quiescence commit. New
// user work must use Root, not Activity. No URL or tool-name eligibility lists.
func (c *Coordinator) Activity(ctx context.Context) (context.Context, func(), error) {
	return c.admit(ctx, true, false)
}

func (c *Coordinator) admit(ctx context.Context, duringDrain, root bool) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	parent, _ := ctx.Value(operationKey{}).(*operation)
	if !root && parent != nil && parent.coordinator == c {
		if parent.released {
			return ctx, nil, ErrReleased
		}
	} else if c.draining && (!duringDrain || c.status.Phase == "replacing") {
		return ctx, nil, ErrDraining
	}
	source := "unknown"
	if _, file, line, ok := runtime.Caller(2); ok {
		source = fmt.Sprintf("%s:%d", filepath.Base(file), line)
	}
	return c.trackLocked(ctx, source)
}

func (c *Coordinator) trackLocked(ctx context.Context, source string) (context.Context, func(), error) {
	if c.active == 0 {
		c.idle = make(chan struct{})
	}
	c.active++
	op := &operation{coordinator: c, source: source, started: time.Now()}
	if c.operations == nil {
		c.operations = make(map[*operation]struct{})
	}
	c.operations[op] = struct{}{}
	return context.WithValue(ctx, operationKey{}, op), func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.releaseLocked(op)
	}, nil
}

func (c *Coordinator) releaseLocked(op *operation) {
	if op.released {
		return
	}
	op.released = true
	delete(c.operations, op)
	c.active--
	if c.active == 0 {
		close(c.idle)
	}
}

func (c *Coordinator) reportBlockers(done <-chan struct{}) {
	c.mu.Lock()
	if c.Diagnostic == nil || c.done != done || !c.draining || c.active == 0 || c.status.Phase == "replacing" {
		c.mu.Unlock()
		return
	}
	bySource := make(map[string]Blocker)
	for op := range c.operations {
		blocker := bySource[op.source]
		blocker.Source = op.source
		blocker.Count++
		blocker.Oldest = max(blocker.Oldest, time.Since(op.started))
		bySource[op.source] = blocker
	}
	diagnostic, phase := c.Diagnostic, c.status.Phase
	c.mu.Unlock()
	blockers := make([]Blocker, 0, len(bySource))
	for _, blocker := range bySource {
		blockers = append(blockers, blocker)
	}
	sort.Slice(blockers, func(i, j int) bool { return blockers[i].Source < blockers[j].Source })
	diagnostic(phase, blockers)
}

func (c *Coordinator) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *Coordinator) Draining() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.draining
}

func (c *Coordinator) publish(phase string) {
	c.status.Phase = phase
	if c.Observe != nil {
		c.Observe(c.status)
	}
}

// Bind declares this invocation ready to replace. A combined mode binds once,
// after ALL its components have installed their admission/ownership adapters.
// replace runs only with closed admission and zero owned work. It must preserve
// the old process on error (including undoing any preparation). On successful
// kernel exec it does not return. Stop cancels/join the attempt, never its work.
func (c *Coordinator) Bind(ctx context.Context, replace func(context.Context) error) (func(), error) {
	if replace == nil {
		return nil, errors.New("reload requires a replacement function")
	}
	c.mu.Lock()
	if c.owner != nil {
		c.mu.Unlock()
		return nil, errors.New("reload already has a process owner")
	}
	b := &binding{ctx: ctx, replace: replace}
	c.owner = b
	c.publish("ready")
	if c.pending {
		c.startLocked()
	}
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			b.stopping = true
			done := c.done
			if c.cancel != nil {
				c.cancel()
			}
			// Keep the binding installed until its attempt has joined.
			c.mu.Unlock()
			if done != nil {
				<-done
			}
			c.mu.Lock()
			if c.owner == b {
				c.owner = nil
				c.pending = false
				c.publish("deferred")
			}
			c.mu.Unlock()
		})
	}, nil
}

// Request coalesces requests, including signals during startup. A non-resumable
// invocation never binds and simply finishes its original action without replay.
func (c *Coordinator) Request() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.draining || (c.pending && c.owner == nil) || (c.owner != nil && c.owner.stopping) {
		return
	}
	c.pending = true
	if c.owner == nil {
		c.publish("deferred")
		return
	}
	c.startLocked()
}

func (c *Coordinator) startLocked() {
	c.pending = false
	c.draining = true
	if c.pause != nil {
		close(c.pause)
	}
	c.status.Attempt++
	c.status.Error = ""
	ctx, cancel := context.WithCancel(c.owner.ctx)
	if c.Timeout > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(c.owner.ctx, c.Timeout)
	}
	c.cancel = cancel
	done := make(chan struct{})
	c.done = done
	idle := c.idle
	if c.active == 0 {
		idle = make(chan struct{})
		close(idle)
	}
	owner := c.owner
	c.publish("draining")
	go func() {
		defer close(done)
		defer cancel()
		interrupt := time.AfterFunc(c.interruptionDelay(), func() { c.interruptTasks(done) })
		defer interrupt.Stop()
		c.requestTaskBoundaries(done)
		slow := time.NewTimer(2 * time.Second)
		defer slow.Stop()
		var err error
		for {
			select {
			case <-slow.C:
				c.reportBlockers(done)
				continue
			case <-ctx.Done():
				err = fmt.Errorf("reload drain: %w", ctx.Err())
			case <-idle:
				c.mu.Lock()
				if c.active != 0 {
					idle = c.idle
					c.mu.Unlock()
					continue
				}
				err = ctx.Err()
				if err == nil {
					c.publish("replacing")
				}
				c.mu.Unlock()
				if err == nil {
					var rollback func()
					rollback, err = c.prepareResources(ctx)
					if err == nil {
						err = owner.replace(ctx)
					}
					if rollback != nil {
						rollback()
					}
					if err == nil {
						err = errors.New("replacement returned without exec")
					}
				}
			}
			break
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.status.Error = err.Error()
		c.draining = false
		c.interrupting = false
		c.pause = nil
		c.cancel = nil
		c.publish("ready")
	}()
}

// Exec protects the replacement boot window. Unix caught-signal dispositions
// reset on exec, but SIG_IGN survives. Listen temporarily ignores/coalesces USR2
// until the successor installs its listener; failed exec restores this listener.
// The callback must perform the actual kernel exec, not asynchronous preparation.
func (c *Coordinator) Exec(exec func() error) error {
	c.mu.Lock()
	prepare := c.beforeExec
	c.mu.Unlock()
	if prepare != nil {
		restore := prepare()
		defer restore()
	}
	return exec()
}
