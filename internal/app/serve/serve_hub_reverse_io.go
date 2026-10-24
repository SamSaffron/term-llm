package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/app/serve/wire"
)

// A connection admits a finite number of operations, including streams whose
// terminal frame has arrived but whose body is still undrained. Admission is
// nonblocking: callers cannot build an unbounded queue behind a stopped peer.
const hubReverseMaxActive = 16

var errHubReverseBusy = errors.New("reverse connection has too many active requests")

func (c *hubReverseConnection) initIO() {
	c.ioOnce.Do(func() {
		c.writeGate = make(chan struct{}, 1)
		c.active = make(chan struct{}, hubReverseMaxActive)
		c.cancels = make(chan hubReverseRequest, hubReverseMaxActive)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.closed:
		return errors.New("reverse connection is closed")
	default:
	}
	select {
	case c.active <- struct{}{}:
		return nil
	default:
		return errHubReverseBusy
	}
}

// One fixed worker owns cancellation traffic. If its bounded queue fills, drop
// this tunnel rather than leak remote operations or spawn unlimited waiters.
func (c *hubReverseConnection) queueCancel(id string) {
	if c.conn == nil {
		return
	}
	c.initIO()
	select {
	case <-c.closed:
		return
	case c.cancels <- hubReverseRequest{Type: hubReverseFrameCancel, ID: id}:
	default:
		c.closeWithError(errors.New("reverse cancellation queue exhausted"))
	}
}
func (c *hubReverseConnection) cancelLoop() {
	for {
		select {
		case <-c.closed:
			return
		case frame := <-c.cancels:
			if c.writeFrame(context.Background(), frame) != nil {
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

func (c *hubReverseConnection) pingLoop() {
	ticker := time.NewTicker(hubReversePingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-ticker.C:
			// WriteControl is Gorilla's concurrency-safe control API and has its own
			// deadline, including waiting for a data writer. Do not race SetWriteDeadline.
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(hubReverseWriteWait)); err != nil {
				c.closeWithError(err)
				return
			}
		}
	}
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
