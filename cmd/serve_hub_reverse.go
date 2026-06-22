package cmd

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
	"github.com/samsaffron/term-llm/internal/hub"
)

// Reverse node connections let a private node dial out to a public Hub. The
// Hub still exposes the same node abstraction: callers ask the Hub to request a
// node path, and the transport is either direct HTTP or this websocket tunnel.
// The tunnel uses a small JSON frame protocol: the Hub sends request frames; the
// node replies with response_start, response_body, and response_end frames. For
// non-streaming JSON-style calls, a single complete response frame is also
// accepted and exposed as an ordinary http.Response reader.

const (
	hubReversePingInterval        = 20 * time.Second
	hubReversePongWait            = 60 * time.Second
	hubReverseWriteWait           = 10 * time.Second
	hubReverseChunkSize           = 32 * 1024
	hubReversePendingBuffer       = 16
	hubReverseMaxRequestBodyBytes = 32 << 20

	hubReverseFrameRequest       = "request"
	hubReverseFrameCancel        = "cancel"
	hubReverseFrameResponseStart = "response_start"
	hubReverseFrameResponseBody  = "response_body"
	hubReverseFrameResponseEnd   = "response_end"
)

type hubReverseRequest struct {
	Type   string      `json:"type,omitempty"`
	ID     string      `json:"id"`
	Method string      `json:"method,omitempty"`
	Path   string      `json:"path,omitempty"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

type hubReverseResponse struct {
	Type   string      `json:"type,omitempty"`
	ID     string      `json:"id"`
	Status int         `json:"status,omitempty"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
	Error  string      `json:"error,omitempty"`
}

type hubReversePending struct {
	ch       chan hubReverseResponse
	done     chan struct{}
	doneOnce sync.Once
	mu       sync.RWMutex
	err      error
	pipe     *io.PipeWriter
}

type hubReverseConnection struct {
	nodeID      string
	connectedAt time.Time
	lastSeenMu  sync.RWMutex
	lastSeen    time.Time
	conn        *websocket.Conn
	writeMu     sync.Mutex
	requestSeq  atomic.Uint64
	pendingMu   sync.Mutex
	pending     map[string]*hubReversePending
}

func newHubReversePending() *hubReversePending {
	return &hubReversePending{
		ch:   make(chan hubReverseResponse, hubReversePendingBuffer),
		done: make(chan struct{}),
	}
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

func (p *hubReversePending) enqueue(resp hubReverseResponse) bool {
	if p == nil {
		return false
	}
	// Apply per-request backpressure instead of dropping body frames for a
	// slower downstream consumer. Because one websocket reader multiplexes all
	// requests for a node, a full per-request queue can temporarily stall sibling
	// requests on the same reverse connection until this request drains or is
	// canceled.
	select {
	case p.ch <- resp:
		return true
	case <-p.done:
		return false
	}
}

type hubReverseManager struct {
	mu    sync.RWMutex
	conns map[string]*hubReverseConnection
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

func (m *hubReverseManager) attach(node hub.Node, conn *websocket.Conn) {
	c := &hubReverseConnection{
		nodeID:      node.ID,
		connectedAt: time.Now().UTC(),
		lastSeen:    time.Now().UTC(),
		conn:        conn,
		pending:     map[string]*hubReversePending{},
	}
	m.mu.Lock()
	old := m.conns[node.ID]
	m.conns[node.ID] = c
	m.mu.Unlock()
	if old != nil {
		_ = old.conn.Close()
		old.failPending("reverse connection replaced")
	}
	go c.readLoop(func() {
		m.mu.Lock()
		if m.conns[node.ID] == c {
			delete(m.conns, node.ID)
		}
		m.mu.Unlock()
	})
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

func hubReverseSetHeartbeat(conn *websocket.Conn, touch func()) error {
	if touch != nil {
		touch()
	}
	if err := conn.SetReadDeadline(time.Now().Add(hubReversePongWait)); err != nil {
		return err
	}
	conn.SetPongHandler(func(string) error {
		if touch != nil {
			touch()
		}
		return conn.SetReadDeadline(time.Now().Add(hubReversePongWait))
	})
	return nil
}

func hubReversePingLoop(conn *websocket.Conn, writeMu *sync.Mutex, done <-chan struct{}) {
	ticker := time.NewTicker(hubReversePingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			writeMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(hubReverseWriteWait))
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(hubReverseWriteWait))
			_ = conn.SetWriteDeadline(time.Time{})
			writeMu.Unlock()
			if err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

func (c *hubReverseConnection) readLoop(done func()) {
	donePing := make(chan struct{})
	defer close(donePing)
	defer done()
	defer c.conn.Close()
	if err := hubReverseSetHeartbeat(c.conn, c.touch); err != nil {
		c.failPending(fmt.Sprintf("reverse connection heartbeat setup failed: %v", err))
		return
	}
	go hubReversePingLoop(c.conn, &c.writeMu, donePing)
	for {
		var resp hubReverseResponse
		if err := c.conn.ReadJSON(&resp); err != nil {
			c.failPending(fmt.Sprintf("reverse connection closed: %v", err))
			return
		}
		c.touch()
		c.pendingMu.Lock()
		pending := c.pending[resp.ID]
		terminal := hubReverseResponseFrameTerminal(resp)
		c.pendingMu.Unlock()
		if pending != nil {
			if !pending.enqueue(resp) {
				continue
			}
			if terminal {
				c.removePending(resp.ID, pending)
			}
		}
	}
}

func hubReverseResponseFrameTerminal(resp hubReverseResponse) bool {
	if resp.Type == "" {
		return true
	}
	return resp.Type == hubReverseFrameResponseEnd || resp.Error != ""
}

func (c *hubReverseConnection) removePending(id string, pending *hubReversePending) {
	c.pendingMu.Lock()
	if c.pending[id] == pending {
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
}

func (c *hubReverseConnection) abortPending(id string, pending *hubReversePending, err error, sendCancel bool) {
	c.removePending(id, pending)
	pending.abort(err)
	if sendCancel {
		go func() { _ = c.writeRequest(hubReverseRequest{Type: hubReverseFrameCancel, ID: id}) }()
	}
}

func (c *hubReverseConnection) failPending(msg string) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = map[string]*hubReversePending{}
	c.pendingMu.Unlock()
	err := errors.New(msg)
	for _, pending := range pending {
		pending.abort(err)
	}
}

func (c *hubReverseConnection) writeRequest(frame hubReverseRequest) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(hubReverseWriteWait))
	err := c.conn.WriteJSON(frame)
	_ = c.conn.SetWriteDeadline(time.Time{})
	return err
}

func (m *hubReverseManager) do(ctx context.Context, node hub.Node, req *http.Request) (*http.Response, error) {
	if m == nil {
		return nil, fmt.Errorf("node %q is configured for reverse connection but reverse transport is disabled", node.ID)
	}
	m.mu.RLock()
	c := m.conns[node.ID]
	m.mu.RUnlock()
	if c == nil {
		return nil, fmt.Errorf("node %q is not connected", node.ID)
	}
	var body []byte
	if req.Body != nil {
		var readErr error
		body, readErr = io.ReadAll(io.LimitReader(req.Body, hubReverseMaxRequestBodyBytes+1))
		if readErr != nil {
			return nil, readErr
		}
		if len(body) > hubReverseMaxRequestBodyBytes {
			return nil, fmt.Errorf("reverse request body exceeds %d bytes: %w", hubReverseMaxRequestBodyBytes, &http.MaxBytesError{Limit: hubReverseMaxRequestBodyBytes})
		}
	}
	id := c.nextRequestID()
	pending := newHubReversePending()
	c.pendingMu.Lock()
	c.pending[id] = pending
	c.pendingMu.Unlock()

	frame := hubReverseRequest{Type: hubReverseFrameRequest, ID: id, Method: req.Method, Path: req.URL.RequestURI(), Header: req.Header.Clone(), Body: body}
	if err := c.writeRequest(frame); err != nil {
		c.removePending(id, pending)
		pending.abort(err)
		return nil, err
	}

	select {
	case resp := <-pending.ch:
		return c.buildHTTPResponseFromReverseFrame(ctx, req, id, pending, resp)
	case <-pending.done:
		if err := pending.errValue(); err != nil {
			return nil, err
		}
		return nil, context.Canceled
	case <-ctx.Done():
		c.cancelPending(id, ctx.Err())
		return nil, ctx.Err()
	}
}

func (c *hubReverseConnection) buildHTTPResponseFromReverseFrame(ctx context.Context, req *http.Request, id string, pending *hubReversePending, resp hubReverseResponse) (*http.Response, error) {
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	if resp.Type == "" {
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
	go c.copyReverseResponseBody(ctx, id, pending, pw)
	return &http.Response{
		StatusCode: resp.Status,
		Status:     fmt.Sprintf("%d %s", resp.Status, http.StatusText(resp.Status)),
		Header:     resp.Header,
		Body:       pr,
		Request:    req,
	}, nil
}

func (c *hubReverseConnection) copyReverseResponseBody(ctx context.Context, id string, pending *hubReversePending, pw *io.PipeWriter) {
	defer pw.Close()
	for {
		select {
		case resp := <-pending.ch:
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
}

func (c *hubReverseConnection) cancelPending(id string, err error) {
	c.pendingMu.Lock()
	pending := c.pending[id]
	delete(c.pending, id)
	c.pendingMu.Unlock()
	if pending != nil {
		pending.abort(err)
	}
	go func() { _ = c.writeRequest(hubReverseRequest{Type: hubReverseFrameCancel, ID: id}) }()
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
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.reverse.attach(node, conn)
	log.Printf("hub: reverse node %q connected", node.ID)
}
