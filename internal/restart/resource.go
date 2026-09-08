package restart

import (
	"context"
	"errors"
	"sync"
)

// Resource is idle infrastructure, not an operation: an MCP server, widget,
// terminal, or a state-only handoff. Prepare runs with operation admission sealed.
// It must join what it stops, respect ctx, and return an undo even on partial
// failure. Undo must restore availability, never re-execute user work.
type Resource struct {
	Check   func(context.Context) error
	Prepare func(context.Context) (undo func(context.Context), err error)
}

func (c *Coordinator) Register(r *Resource) func() {
	c.mu.Lock()
	if c.resources == nil {
		c.resources = make(map[*Resource]struct{})
	}
	c.resources[r] = struct{}{}
	c.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { c.mu.Lock(); delete(c.resources, r); c.mu.Unlock() }) }
}

func (c *Coordinator) prepareResources(ctx context.Context) (func(), error) {
	c.mu.Lock()
	resources := make([]*Resource, 0, len(c.resources))
	for r := range c.resources {
		resources = append(resources, r)
	}
	c.mu.Unlock()
	for _, r := range resources {
		if r != nil && r.Check != nil {
			if err := r.Check(ctx); err != nil {
				return nil, err
			}
		}
	}
	var undos []func(context.Context)
	rollback := func() {
		for i := len(undos) - 1; i >= 0; i-- {
			// Rollback may need to restart idle infrastructure asynchronously. Retain
			// that recovery as owned work so a subsequent reload must join it too.
			c.mu.Lock()
			recovery, release, _ := c.trackLocked(context.Background(), "reload rollback")
			c.mu.Unlock()
			undos[i](recovery)
			release()
		}
	}
	for _, r := range resources {
		if err := ctx.Err(); err != nil {
			return rollback, err
		}
		if r == nil {
			return rollback, errors.New("invalid reload resource")
		}
		if r.Prepare == nil {
			continue
		}
		undo, err := r.Prepare(ctx)
		if undo != nil {
			undos = append(undos, undo)
		}
		if err != nil {
			return rollback, err
		}
	}
	return rollback, ctx.Err()
}

// Go retains an admitted operation before launching its descendant. Without an
// inherited operation, it accounts for maintenance until the exec commit.
func (c *Coordinator) Go(ctx context.Context, fn func(context.Context)) error {
	ctx, release, err := c.Activity(ctx)
	if err != nil {
		return err
	}
	go func() { defer release(); fn(ctx) }()
	return nil
}

// Resume waits for rollback and reacquires a fresh ownership ticket for parked
// work. Readiness and acquisition are checked under the same lock, so a second
// reload cannot seal admission between them. Unlike Enter, the old (released)
// parent ticket is intentionally replaced; cancellation still does not settle it.
func (c *Coordinator) Resume(ctx context.Context) (context.Context, func(), error) {
	for {
		c.mu.Lock()
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return ctx, nil, err
		}
		if !c.draining {
			source := "resumed work"
			if op, _ := ctx.Value(operationKey{}).(*operation); op != nil {
				source = op.source
			}
			active, release, err := c.trackLocked(ctx, source)
			c.mu.Unlock()
			return active, release, err
		}
		done := c.done
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx, nil, ctx.Err()
		case <-done:
		}
	}
}

// WaitReady lets admission loops sleep during drain without spinning.
func (c *Coordinator) WaitReady(ctx context.Context) error {
	for {
		c.mu.Lock()
		if !c.draining {
			c.mu.Unlock()
			return ctx.Err()
		}
		done := c.done
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
	}
}
