package restart

import "context"

// Child retains ownership inherited from a reload-aware caller. Unowned callers
// are unchanged: libraries do not install signals, bind modes, or reach for a
// process global. Acquire before launching asynchronous work and release only
// when that work really returns, not when its caller stops waiting.
func Child(ctx context.Context) (context.Context, func(), error) {
	op, _ := ctx.Value(operationKey{}).(*operation)
	if op == nil {
		return ctx, func() {}, nil
	}
	return op.coordinator.Enter(ctx)
}

// AdmissionContext cancels only an idle intake operation (for example a Telegram
// long poll) when drain begins. Never use it as the context of admitted user work.
func (c *Coordinator) AdmissionContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	if c.pause == nil {
		c.pause = make(chan struct{})
		if c.draining {
			close(c.pause)
		}
	}
	paused := c.pause
	c.mu.Unlock()
	go func() {
		select {
		case <-paused:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// Inherit transfers admission ownership without transferring cancellation. A
// detached operation can outlive a UI/request context while keeping its ticket.
func Inherit(ctx, owner context.Context) context.Context {
	if op, _ := owner.Value(operationKey{}).(*operation); op != nil {
		return context.WithValue(ctx, operationKey{}, op)
	}
	return ctx
}

// Detached admits work that outlives its caller but cannot be checkpointed in
// place, such as a background delegated agent. Like Child it retains reload
// ownership from the caller's live operation and must be acquired before the
// caller returns. Unlike plain ownership, it opts into the grace-period
// cancellation policy: a reload interrupts it with ErrInterrupt instead of
// waiting for it to finish naturally. Unowned callers are unchanged.
func Detached(ctx context.Context) (context.Context, func(), error) {
	op, _ := ctx.Value(operationKey{}).(*operation)
	if op == nil {
		return ctx, func() {}, nil
	}
	c := op.coordinator
	owned, release, err := c.Enter(ctx)
	if err != nil {
		return ctx, nil, err
	}
	cancellable, closeCancellable := c.Cancellable(owned)
	return cancellable, func() { closeCancellable(); release() }, nil
}
