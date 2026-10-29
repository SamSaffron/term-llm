package serve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	appserver "github.com/samsaffron/term-llm/internal/app/serve/server"
	"github.com/samsaffron/term-llm/internal/app/serve/wire"
	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/restart"
)

// Reverse node connections let a private node dial out to a public Hub. The
// Hub still exposes the same node abstraction: callers ask the Hub to request a
// node path, and the transport is either direct HTTP or this websocket tunnel.
// The tunnel uses a small JSON frame protocol: the Hub sends request frames; the
// node replies with response_start, response_body, and response_end frames. For
// non-streaming JSON-style calls, a single complete response frame is also
// accepted and exposed as an ordinary http.Response reader when the complete
// JSON/base64 frame remains below hubReverseMaxWireFrameBytes; larger responses
// must use the chunked frame sequence.

const (
	hubReversePingInterval = appserver.ReversePingInterval
	hubReversePongWait     = appserver.ReversePongWait
	hubReverseWriteWait    = wire.JSONChunkWriteWait
	hubReverseChunkSize    = appserver.ReverseChunkSize
	// Bound each request by both frames and decoded body bytes. This absorbs an
	// ~8 MiB burst at the connector's maximum chunk size without allowing one
	// downstream client to wedge the multiplexed websocket reader.
	hubReversePendingBuffer    = 256
	hubReversePendingBodyBytes = 8 << 20
	// JSON/base64 framing expands a 32 KiB body chunk. Leave ample room for
	// response metadata while rejecting unexpectedly large node frames.
	hubReverseMaxWireFrameBytes   = 256 << 10
	hubReverseMaxRequestBodyBytes = 32 << 20

	hubReverseFrameRequest       = "request"
	hubReverseFrameCancel        = "cancel"
	hubReverseFrameResponseStart = appserver.ReverseFrameResponseStart
	hubReverseFrameResponseBody  = appserver.ReverseFrameResponseBody
	hubReverseFrameResponseEnd   = appserver.ReverseFrameResponseEnd
)

var (
	errHubReverseSlowConsumer     = errors.New("reverse response consumer is too slow")
	errHubReverseResponseTooLarge = errors.New("reverse response frame body exceeds per-request limit")
)

type hubReverseRequest = appserver.ReverseRequest
type hubReverseResponse = appserver.ReverseResponse

type hubReversePending struct {
	ch            chan hubReverseResponse
	terminal      chan hubReverseResponse
	queuedBytes   atomic.Int64
	done          chan struct{}
	doneOnce      sync.Once
	mu            sync.RWMutex
	err           error
	pipe          *io.PipeWriter
	finishOnce    sync.Once
	releaseActive func()
}

type hubReverseConnection struct {
	nodeID      string
	node        hub.Node
	manager     *hubReverseManager
	connectedAt time.Time
	lastSeenMu  sync.RWMutex
	lastSeen    time.Time
	conn        *websocket.Conn
	ioOnce      sync.Once
	closeOnce   sync.Once
	writeGate   chan struct{}
	active      chan struct{}
	cancels     chan hubReverseRequest
	closed      chan struct{}
	detach      func()
	requestSeq  atomic.Uint64
	pendingMu   sync.Mutex
	pending     map[string]*hubReversePending
}

func newHubReversePending() *hubReversePending {
	return &hubReversePending{
		ch:       make(chan hubReverseResponse, hubReversePendingBuffer),
		terminal: make(chan hubReverseResponse, 1),
		done:     make(chan struct{}),
	}
}

func (p *hubReversePending) finish() {
	p.finishOnce.Do(func() {
		if p.releaseActive != nil {
			p.releaseActive()
		}
	})
}

func (p *hubReversePending) doneClosed() bool {
	if p == nil {
		return true
	}
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *hubReversePending) errValue() error {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.err
}

func (p *hubReversePending) setPipeWriter(pw *io.PipeWriter) {
	if p == nil || pw == nil {
		return
	}
	p.mu.Lock()
	p.pipe = pw
	err := p.err
	p.mu.Unlock()
	if err != nil {
		_ = pw.CloseWithError(err)
	}
}

func (p *hubReversePending) abort(err error) {
	if p == nil {
		return
	}
	if err == nil {
		err = context.Canceled
	}
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	pw := p.pipe
	p.mu.Unlock()
	p.doneOnce.Do(func() { close(p.done) })
	if pw != nil {
		_ = pw.CloseWithError(err)
	}
}

func (p *hubReversePending) enqueue(resp hubReverseResponse) (queued, full bool) {
	if p == nil {
		return false, false
	}
	bodyBytes := int64(len(resp.Body))
	if bodyBytes > hubReversePendingBodyBytes {
		return false, true
	}
	if bodyBytes > 0 && p.queuedBytes.Add(bodyBytes) > hubReversePendingBodyBytes {
		p.queuedBytes.Add(-bodyBytes)
		return false, true
	}
	// Never block the sole websocket reader behind one request. The bounded
	// frame and byte windows absorb normal scheduling/network-backpressure
	// bursts; exhausting either isolates a consumer that has stopped draining.
	select {
	case p.ch <- resp:
		return true, false
	case <-p.done:
		p.queuedBytes.Add(-bodyBytes)
		return false, false
	default:
		p.queuedBytes.Add(-bodyBytes)
		return false, true
	}
}

func (p *hubReversePending) release(resp hubReverseResponse) {
	if p != nil && len(resp.Body) > 0 {
		p.queuedBytes.Add(-int64(len(resp.Body)))
	}
}

// deferTerminal preserves a terminal frame when body traffic has filled the
// regular queue. The body copier drains queued frames before observing this
// one-slot terminal lane, so node errors and clean completion are not replaced
// by a synthetic slow-consumer failure.
func (p *hubReversePending) deferTerminal(resp hubReverseResponse) bool {
	if p == nil || len(resp.Body) > hubReversePendingBodyBytes {
		return false
	}
	bodyBytes := int64(len(resp.Body))
	p.queuedBytes.Add(bodyBytes)
	select {
	case p.terminal <- resp:
		return true
	case <-p.done:
		p.queuedBytes.Add(-bodyBytes)
		return false
	default:
		p.queuedBytes.Add(-bodyBytes)
		return false
	}
}

type hubReverseManager struct {
	registry *hub.Registry
	mu       sync.RWMutex
	conns    map[string]*hubReverseConnection
}

func newHubReverseManager() *hubReverseManager {
	return &hubReverseManager{conns: map[string]*hubReverseConnection{}}
}

func (m *hubReverseManager) isConnected(nodeID string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	c := m.conns[nodeID]
	m.mu.RUnlock()
	return c != nil
}

func (m *hubReverseManager) status(nodeID string) (connected bool, connectedAt, lastSeen time.Time) {
	if m == nil {
		return false, time.Time{}, time.Time{}
	}
	m.mu.RLock()
	c := m.conns[nodeID]
	m.mu.RUnlock()
	if c == nil {
		return false, time.Time{}, time.Time{}
	}
	return true, c.connectedAt, c.lastSeenValue()
}

func (m *hubReverseManager) attach(node hub.Node, conn *websocket.Conn) error {
	c := &hubReverseConnection{
		nodeID:      node.ID,
		node:        node,
		manager:     m,
		connectedAt: time.Now().UTC(),
		lastSeen:    time.Now().UTC(),
		conn:        conn,
		pending:     map[string]*hubReversePending{},
	}
	c.initIO()
	c.detach = func() {
		m.mu.Lock()
		if m.conns[node.ID] == c {
			delete(m.conns, node.ID)
		}
		m.mu.Unlock()
	}
	err := m.registry.WithCurrent(node, func(hub.Node) error {
		m.mu.Lock()
		old := m.conns[node.ID]
		m.conns[node.ID] = c
		m.mu.Unlock()
		if old != nil {
			old.closeWithError(errors.New("reverse connection replaced"))
		}
		return nil
	})
	if err != nil {
		_ = conn.Close()
		return err
	}
	go c.cancelLoop()
	go c.readLoop()
	return nil
}

func (c *hubReverseConnection) touch() {
	c.lastSeenMu.Lock()
	c.lastSeen = time.Now().UTC()
	c.lastSeenMu.Unlock()
}

func (c *hubReverseConnection) lastSeenValue() time.Time {
	c.lastSeenMu.RLock()
	defer c.lastSeenMu.RUnlock()
	return c.lastSeen
}

func (c *hubReverseConnection) nextRequestID() string {
	return fmt.Sprintf("req_%d", c.requestSeq.Add(1))
}

func (c *hubReverseConnection) readLoop() {
	defer c.closeWithError(errors.New("reverse reader stopped"))
	if err := appserver.SetReverseHeartbeat(c.conn, c.touch); err != nil {
		c.closeWithError(fmt.Errorf("reverse connection heartbeat setup failed: %w", err))
		return
	}
	go appserver.RunReverseControlPingLoop(c.conn, c.closed, c.closeWithError)
	for {
		var resp hubReverseResponse
		if err := c.conn.ReadJSON(&resp); err != nil {
			c.closeWithError(err)
			return
		}
		c.touch()
		c.dispatchResponse(resp)
	}
}

func (c *hubReverseConnection) dispatchResponse(resp hubReverseResponse) {
	c.pendingMu.Lock()
	pending := c.pending[resp.ID]
	terminal := hubReverseResponseFrameTerminal(resp)
	c.pendingMu.Unlock()
	if pending == nil {
		return
	}
	if len(resp.Body) > hubReversePendingBodyBytes {
		c.abortPending(resp.ID, pending, errHubReverseResponseTooLarge, true)
		return
	}
	queued, full := pending.enqueue(resp)
	if full {
		switch {
		case terminal && pending.deferTerminal(resp):
			c.removePending(resp.ID, pending)
		case resp.Error != "":
			c.abortPending(resp.ID, pending, errors.New(resp.Error), false)
		default:
			c.abortPending(resp.ID, pending, errHubReverseSlowConsumer, true)
		}
	}
	if !queued {
		return
	}
	if terminal {
		c.removePending(resp.ID, pending)
	}
}

func hubReverseResponseFrameTerminal(resp hubReverseResponse) bool {
	if resp.Type == "" {
		return true
	}
	return resp.Type == hubReverseFrameResponseEnd || resp.Error != ""
}

func (c *hubReverseConnection) removePending(id string, pending *hubReversePending) bool {
	c.pendingMu.Lock()
	removed := c.pending[id] == pending
	if removed {
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
	return removed
}

func (c *hubReverseConnection) abortPending(id string, pending *hubReversePending, err error, sendCancel bool) {
	removed := c.removePending(id, pending)
	pending.abort(err)
	if sendCancel && removed {
		c.queueCancel(id)
	}
}

func (m *hubReverseManager) do(ctx context.Context, node hub.Node, req *http.Request) (*http.Response, error) {
	if m == nil {
		return nil, fmt.Errorf("node %q is configured for reverse connection but reverse transport is disabled", node.ID)
	}
	c, err := m.connection(node)
	if err != nil {
		return nil, err
	}
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	pending := newHubReversePending()
	pending.releaseActive = func() { <-c.active }
	success := false
	defer func() {
		if !success {
			pending.finish()
		}
	}()
	var body []byte
	if req.Body != nil {
		var readErr error
		stop := context.AfterFunc(ctx, func() { _ = req.Body.Close() })
		body, readErr = io.ReadAll(io.LimitReader(req.Body, hubReverseMaxRequestBodyBytes+1))
		stop()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if readErr != nil {
			return nil, readErr
		}
		if len(body) > hubReverseMaxRequestBodyBytes {
			return nil, fmt.Errorf("reverse request body exceeds %d bytes: %w", hubReverseMaxRequestBodyBytes, &http.MaxBytesError{Limit: hubReverseMaxRequestBodyBytes})
		}
	}
	id := c.nextRequestID()
	c.pendingMu.Lock()
	select {
	case <-c.closed:
		c.pendingMu.Unlock()
		return nil, errors.New("reverse connection is closed")
	default:
	}
	c.pending[id] = pending
	c.pendingMu.Unlock()

	frame := hubReverseRequest{Type: hubReverseFrameRequest, ID: id, Method: req.Method, Path: req.URL.RequestURI(), Header: req.Header.Clone(), Body: body}
	if err := c.writeFrame(ctx, frame); err != nil {
		c.removePending(id, pending)
		pending.abort(err)
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		c.abortPending(id, pending, err, true)
		return nil, err
	}
	select {
	case resp := <-pending.ch:
		pending.release(resp)
		// Cancellation must not lose to a ready response_start: unlike a
		// terminal frame, it still owns a remote operation.
		if err := ctx.Err(); err != nil {
			c.abortPending(id, pending, err, true)
			return nil, err
		}
		response, err := c.buildHTTPResponseFromReverseFrame(ctx, req, id, pending, resp)
		success = err == nil
		return response, err
	case <-pending.done:
		if err := pending.errValue(); err != nil {
			return nil, err
		}
		return nil, context.Canceled
	case <-ctx.Done():
		c.abortPending(id, pending, ctx.Err(), true)
		return nil, ctx.Err()
	}
}

func (c *hubReverseConnection) buildHTTPResponseFromReverseFrame(ctx context.Context, req *http.Request, id string, pending *hubReversePending, resp hubReverseResponse) (*http.Response, error) {
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	if resp.Type == "" {
		pending.finish()
		return &http.Response{
			StatusCode:    resp.Status,
			Status:        fmt.Sprintf("%d %s", resp.Status, http.StatusText(resp.Status)),
			Header:        resp.Header,
			Body:          io.NopCloser(bytes.NewReader(resp.Body)),
			ContentLength: int64(len(resp.Body)),
			Request:       req,
		}, nil
	}
	if resp.Type != hubReverseFrameResponseStart {
		c.cancelPending(id, fmt.Errorf("unexpected reverse response frame %q", resp.Type))
		return nil, fmt.Errorf("unexpected reverse response frame %q", resp.Type)
	}
	pr, pw := io.Pipe()
	pending.setPipeWriter(pw)
	// Cancellation must close the pipe independently of the copying goroutine:
	// that goroutine may be blocked inside pw.Write on an undrained client.
	stop := context.AfterFunc(ctx, func() { c.abortPending(id, pending, ctx.Err(), true) })
	go func() { defer stop(); c.copyReverseResponseBody(ctx, id, pending, pw) }()
	return &http.Response{
		StatusCode: resp.Status,
		Status:     fmt.Sprintf("%d %s", resp.Status, http.StatusText(resp.Status)),
		Header:     resp.Header,
		Body: &hubReverseBody{ReadCloser: pr, closeBody: func() {
			c.abortPending(id, pending, context.Canceled, true)
		}},
		Request: req,
	}, nil
}

func (c *hubReverseConnection) copyReverseResponseBody(ctx context.Context, id string, pending *hubReversePending, pw *io.PipeWriter) {
	defer pw.Close()
	defer pending.finish()
	for {
		var resp hubReverseResponse
		// Drain ordinary frames before a deferred terminal frame. This preserves
		// wire order when response_end arrived while the body queue was full.
		select {
		case resp = <-pending.ch:
		default:
			select {
			case resp = <-pending.ch:
			case resp = <-pending.terminal:
			case <-pending.done:
				err := pending.errValue()
				if err == nil {
					err = context.Canceled
				}
				_ = pw.CloseWithError(err)
				return
			case <-ctx.Done():
				_ = pw.CloseWithError(ctx.Err())
				c.cancelPending(id, ctx.Err())
				return
			}
		}
		pending.release(resp)

		if resp.Error != "" {
			_ = pw.CloseWithError(errors.New(resp.Error))
			return
		}
		switch resp.Type {
		case hubReverseFrameResponseBody:
			if len(resp.Body) > 0 {
				if _, err := pw.Write(resp.Body); err != nil {
					c.cancelPending(id, err)
					return
				}
			}
		case hubReverseFrameResponseEnd:
			return
		default:
			err := fmt.Errorf("unexpected reverse response frame %q", resp.Type)
			_ = pw.CloseWithError(err)
			c.cancelPending(id, err)
			return
		}
	}
}

func (c *hubReverseConnection) cancelPending(id string, err error) {
	c.pendingMu.Lock()
	pending := c.pending[id]
	delete(c.pending, id)
	c.pendingMu.Unlock()
	if pending != nil {
		pending.abort(err)
		c.queueCancel(id)
	}
}

func (s *hubServer) handleReverseConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	node, err := s.authenticateNode(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if !node.UsesReverseConnection() {
		http.Error(w, fmt.Sprintf("node %q is not configured for reverse connection", node.ID), http.StatusForbidden)
		return
	}
	restart.Passive(r.Context()) // registration socket is passive; forwarded operations have their own owner
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(hubReverseMaxWireFrameBytes)
	if err := s.reverse.attach(node, conn); err != nil {
		return
	}
	log.Printf("hub: reverse node %q connected", node.ID)
}
