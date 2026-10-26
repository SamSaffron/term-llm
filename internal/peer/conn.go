package peer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"
)

// ErrClosed reports that the bridge DataChannel closed.
var ErrClosed = errors.New("peer: data channel closed")

// conn serialises frame writes onto one DataChannel and applies send-side
// backpressure: a writer blocks while the channel's buffered amount is above
// the high-water mark, until Pion reports the buffer has drained.
type conn struct {
	dc     *webrtc.DataChannel
	limits Limits

	sendGate  chan struct{}
	low       chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func newConn(dc *webrtc.DataChannel, limits Limits) *conn {
	c := &conn{
		dc:       dc,
		limits:   limits,
		sendGate: make(chan struct{}, 1),
		low:      make(chan struct{}, 1),
		closed:   make(chan struct{}),
	}
	c.sendGate <- struct{}{}
	dc.SetBufferedAmountLowThreshold(uint64(limits.MaxBufferedBytes / 2))
	dc.OnBufferedAmountLow(func() {
		select {
		case c.low <- struct{}{}:
		default:
		}
	})
	return c
}

// validChannel enforces the bridge's channel contract: the agreed label,
// reliable and ordered delivery.
func validChannel(dc *webrtc.DataChannel) error {
	if dc.Label() != ChannelLabel {
		return fmt.Errorf("unexpected data channel label %q", dc.Label())
	}
	if !dc.Ordered() || dc.MaxRetransmits() != nil || dc.MaxPacketLifeTime() != nil {
		return errors.New("bridge data channel must be reliable and ordered")
	}
	return nil
}

func (c *conn) markClosed() {
	c.closeOnce.Do(func() { close(c.closed) })
}

func (c *conn) close() {
	c.markClosed()
	_ = c.dc.Close()
}

func (c *conn) send(ctx context.Context, f *Frame) error {
	b, err := encodeFrame(f, c.limits.MaxFrameBytes)
	if err != nil {
		return err
	}
	select {
	case <-c.sendGate:
		defer func() { c.sendGate <- struct{}{} }()
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	high := uint64(c.limits.MaxBufferedBytes)
	for c.dc.BufferedAmount() > high {
		select {
		case <-c.low:
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return ErrClosed
		}
	}
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	if err := c.dc.SendText(string(b)); err != nil {
		return fmt.Errorf("send %s frame: %w", f.Type, err)
	}
	return nil
}
