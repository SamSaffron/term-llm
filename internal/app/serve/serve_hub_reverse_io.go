package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	appserver "github.com/term-llm/term-llm/internal/app/serve/server"
	"github.com/term-llm/term-llm/internal/app/serve/wire"
)

const (
	hubReverseMaxActive     = appserver.ReverseMaxActive
	hubReverseMaxWaiting    = appserver.ReverseMaxWaiting
	hubReverseAdmissionWait = appserver.ReverseAdmissionWait
)

func (c *hubReverseConnection) initIO() {
	c.ioOnce.Do(func() {
		c.writeGate = make(chan struct{}, 1)
		c.active = make(chan struct{}, hubReverseMaxActive)
		c.waiting = make(chan struct{}, hubReverseMaxWaiting)
		c.streams = make(chan struct{}, appserver.ReverseMaxStreams)
		c.cancels = appserver.NewReverseCancels(hubReverseMaxActive + appserver.ReverseMaxStreams)
		c.closed = make(chan struct{})
	})
}

func (c *hubReverseConnection) closeWithError(err error) {
	c.initIO()
	c.closeOnce.Do(func() {
		// Detach before waking callers; a completed failed request must not leave
		// the dashboard advertising a connected but permanently unwritable tunnel.
		if c.detach != nil {
			c.detach()
		}
		// Atomically stop registration and empty the pending map before waking
		// queued writers. A failed tunnel cannot retain newly registered work.
		c.pendingMu.Lock()
		close(c.closed)
		pending := c.pending
		c.pending = map[string]*hubReversePending{}
		c.pendingMu.Unlock()
		if c.conn != nil {
			_ = c.conn.Close()
		}
		failure := fmt.Errorf("reverse connection closed: %w", err)
		for _, p := range pending {
			p.abort(failure)
			p.finish()
		}
	})
}

func (c *hubReverseConnection) admit(ctx context.Context) error {
	c.initIO()
	return appserver.AdmitReverseRequest(ctx, c.active, c.waiting, c.closed)
}

// One fixed worker owns cancellation traffic. A queued cancel keeps its
// operation's slot until written (see appserver.ReverseCancels). If the bounded
// queue still fills, drop this tunnel rather than leak remote operations or
// spawn unlimited waiters.
func (c *hubReverseConnection) queueCancel(id string, release func()) {
	cancel := appserver.ReverseCancel{ID: id, Release: release}
	if c.conn == nil {
		cancel.Done()
		return
	}
	c.initIO()
	if !c.cancels.Push(cancel) {
		c.closeWithError(errors.New("reverse cancellation queue exhausted"))
		cancel.Done()
	}
}
func (c *hubReverseConnection) cancelLoop() {
	defer c.cancels.Close()
	for {
		select {
		case <-c.closed:
			return
		case cancel := <-c.cancels.Queue():
			err := c.writeFrame(context.Background(), hubReverseRequest{Type: hubReverseFrameCancel, ID: cancel.ID})
			cancel.Done()
			if err != nil {
				return
			}
		}
	}
}

func (c *hubReverseConnection) writeFrame(ctx context.Context, frame hubReverseRequest) error {
	if c == nil || c.conn == nil {
		return errors.New("reverse connection is unavailable")
	}
	c.initIO()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return errors.New("reverse connection is closed")
	case c.writeGate <- struct{}{}:
	}
	defer func() { <-c.writeGate }()
	// Select can choose the gate even when cancellation is also ready. Never
	// put an already-expired queued request onto the wire.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.closed:
		return errors.New("reverse connection is closed")
	default:
	}
	if frame.Type == hubReverseFrameRequest {
		if err := c.validate(); err != nil {
			return err
		}
	}
	// Once a JSON message begins, finish it under a byte-scaled hard deadline.
	// A caller cancellation must not corrupt the shared tunnel; do() sends its
	// remote cancel after this bounded write completes. Actual IO failures still
	// poison Gorilla's writer and must fail closed for every pending operation.
	started, err := wire.WriteFragmentedJSON(ctx, c.conn, frame)
	if err != nil && started {
		c.closeWithError(err)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}
	// Successfully sent frames stay owned by response/cancellation handling.
	return err
}

type hubReverseBody struct {
	io.ReadCloser
	once      sync.Once
	closeBody func()
}

func (b *hubReverseBody) Close() error {
	b.once.Do(b.closeBody)
	return b.ReadCloser.Close()
}
