package peerbroker

import (
	"bufio"
	"errors"
	"net"
	"net/http"
)

const wsMaxFrameHeader = 14

func (h *Handler) newRawBudget() *tokenBucket {
	return newTokenBucket(
		h.cfg.SignalBytesPerSecond+h.cfg.SignalRate*wsMaxFrameHeader,
		h.cfg.SignalByteBurst+h.cfg.SignalBurst*wsMaxFrameHeader,
		h.cfg.Now,
	)
}

type rawLimitedWriter struct {
	http.ResponseWriter
	budget *tokenBucket
}

func (w *rawLimitedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("peerbroker: response does not support hijacking")
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &rawLimitedConn{Conn: conn, budget: w.budget}, buffered, nil
}

type rawLimitedConn struct {
	net.Conn
	budget *tokenBucket
}

func (c *rawLimitedConn) Read(p []byte) (int, error) {
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
