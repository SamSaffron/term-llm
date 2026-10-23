package peerbroker

import (
	"bufio"
	"errors"
	"net"
	"net/http"
)

// wsMaxFrameHeader is the largest client frame header (2 + 8 length + 4 mask).
const wsMaxFrameHeader = 14

// newRawBudget returns the per-connection budget for raw bytes read from the
// upgraded socket: the payload byte budget plus a maximal header per allowed
// frame. Every wire byte, including headers of empty continuation or control
// frames, is charged, so a peer cannot make the parser loop without limit.
func (h *Handler) newRawBudget() *tokenBucket {
	rate := h.cfg.WSBytesPerSecond + h.cfg.WSFramesPerSecond*wsMaxFrameHeader
	burst := h.cfg.WSByteBurst + h.cfg.WSFrameBurst*wsMaxFrameHeader
	return newTokenBucket(rate, burst, h.cfg.Now)
}

// rawLimitedWriter wraps the ResponseWriter so gorilla's Hijack yields a
// rawLimitedConn. It exposes only http.Hijacker, which is all Upgrade needs.
type rawLimitedWriter struct {
	http.ResponseWriter
	budget *tokenBucket
}

func (w *rawLimitedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("peerbroker: response does not support hijacking")
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &rawLimitedConn{Conn: c, budget: w.budget}, brw, nil
}

// rawLimitedConn charges every byte read against budget. Once the budget is
// exhausted further reads fail with errWSRateLimited (sticky in gorilla).
type rawLimitedConn struct {
	net.Conn
	budget *tokenBucket
}

func (c *rawLimitedConn) Read(p []byte) (int, error) {
	// Never read more than one budget burst at a time, and refuse to read at
	// all once the bucket is empty, so overdraw is bounded by len(p).
	if !c.budget.allowN(1) {
		return 0, errWSRateLimited
	}
	n, err := c.Conn.Read(p)
	if n > 1 && !c.budget.allowN(float64(n-1)) {
		c.budget.drain()
		return n, errWSRateLimited
	}
	return n, err
}
