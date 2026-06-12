package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/hub"
)

// Reverse node connections let a private node dial out to a public Hub. The
// Hub still exposes the same node abstraction: callers ask the Hub to request a
// node path, and the transport is either direct HTTP or this websocket tunnel.
// The tunnel is deliberately small: one request frame, one buffered response
// frame. That is enough for Hub UI proxying and jobs-v2 delegation without
// adding offline queues, arbitrary forwarding, or a second delegation API.

const (
	hubReversePingInterval = 20 * time.Second
	hubReversePongWait     = 60 * time.Second
	hubReverseWriteWait    = 10 * time.Second
)

type hubReverseRequest struct {
	ID     string      `json:"id"`
	Method string      `json:"method"`
	Path   string      `json:"path"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

type hubReverseResponse struct {
	ID     string      `json:"id"`
	Status int         `json:"status"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
	Error  string      `json:"error,omitempty"`
}

type hubReverseConnection struct {
	nodeID      string
	connectedAt time.Time
	lastSeenMu  sync.RWMutex
	lastSeen    time.Time
	conn        *websocket.Conn
	writeMu     sync.Mutex
	pendingMu   sync.Mutex
	pending     map[string]chan hubReverseResponse
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
		pending:     map[string]chan hubReverseResponse{},
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
		ch := c.pending[resp.ID]
		delete(c.pending, resp.ID)
		c.pendingMu.Unlock()
		if ch != nil {
			ch <- resp
			close(ch)
		}
	}
}

func (c *hubReverseConnection) failPending(msg string) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = map[string]chan hubReverseResponse{}
	c.pendingMu.Unlock()
	for id, ch := range pending {
		ch <- hubReverseResponse{ID: id, Status: http.StatusBadGateway, Error: msg}
		close(ch)
	}
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
	body, err := io.ReadAll(io.LimitReader(req.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	id := fmt.Sprintf("req_%d", time.Now().UnixNano())
	ch := make(chan hubReverseResponse, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	frame := hubReverseRequest{ID: id, Method: req.Method, Path: req.URL.RequestURI(), Header: req.Header.Clone(), Body: body}
	c.writeMu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(hubReverseWriteWait))
	err = c.conn.WriteJSON(frame)
	_ = c.conn.SetWriteDeadline(time.Time{})
	c.writeMu.Unlock()
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.Error != "" {
			return nil, errors.New(resp.Error)
		}
		return &http.Response{
			StatusCode:    resp.Status,
			Status:        fmt.Sprintf("%d %s", resp.Status, http.StatusText(resp.Status)),
			Header:        resp.Header,
			Body:          io.NopCloser(bytes.NewReader(resp.Body)),
			ContentLength: int64(len(resp.Body)),
			Request:       req,
		}, nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
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
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.reverse.attach(node, conn)
	log.Printf("hub: reverse node %q connected", node.ID)
}

func localHubConnectBase(host string, port int, _ string) string {
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s", netJoinHostPortForURL(host, port))
}

func netJoinHostPortForURL(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func runHubReverseConnector(ctx context.Context, hubURL, nodeID, token, localBase, allowedBasePath string, client *http.Client) {
	if client == nil {
		client = http.DefaultClient
	}
	for ctx.Err() == nil {
		if err := hubReverseConnectOnce(ctx, hubURL, nodeID, token, localBase, allowedBasePath, client); err != nil && ctx.Err() == nil {
			log.Printf("hub reverse connect: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func hubReverseConnectOnce(ctx context.Context, hubURL, nodeID, token, localBase, allowedBasePath string, client *http.Client) error {
	u, err := url.Parse(strings.TrimRight(hubURL, "/") + "/api/connect")
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return fmt.Errorf("unsupported hub url scheme %q", u.Scheme)
	}
	q := u.Query()
	q.Set("node_id", nodeID)
	u.RawQuery = q.Encode()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set(hubNodeIDHeader, nodeID)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), header)
	if err != nil {
		return err
	}
	defer conn.Close()
	var writeMu sync.Mutex
	donePing := make(chan struct{})
	defer close(donePing)
	if err := hubReverseSetHeartbeat(conn, nil); err != nil {
		return err
	}
	go hubReversePingLoop(conn, &writeMu, donePing)
	log.Printf("hub reverse connect: node %q connected to %s", nodeID, hubURL)
	for {
		var req hubReverseRequest
		if err := conn.ReadJSON(&req); err != nil {
			return err
		}
		resp := handleHubReverseRequest(ctx, req, token, localBase, allowedBasePath, client)
		writeMu.Lock()
		_ = conn.SetWriteDeadline(time.Now().Add(hubReverseWriteWait))
		err := conn.WriteJSON(resp)
		_ = conn.SetWriteDeadline(time.Time{})
		writeMu.Unlock()
		if err != nil {
			return err
		}
	}
}

func handleHubReverseRequest(ctx context.Context, frame hubReverseRequest, token, localBase, allowedBasePath string, client *http.Client) hubReverseResponse {
	if frame.ID == "" {
		return hubReverseResponse{Status: http.StatusBadRequest, Error: "missing request id"}
	}
	if !strings.HasPrefix(frame.Path, "/") || hubContainsEncodedSeparator(frame.Path) || hubHasDotDotSegment(frame.Path) {
		return hubReverseResponse{ID: frame.ID, Status: http.StatusBadRequest, Error: "invalid reverse request path"}
	}
	pathOnly := frame.Path
	if i := strings.IndexByte(pathOnly, '?'); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	allowedBasePath = strings.TrimRight(allowedBasePath, "/")
	if allowedBasePath != "" && pathOnly != allowedBasePath && !strings.HasPrefix(pathOnly, allowedBasePath+"/") {
		return hubReverseResponse{ID: frame.ID, Status: http.StatusForbidden, Error: "reverse request outside node base path"}
	}
	localURL := strings.TrimRight(localBase, "/") + frame.Path
	req, err := http.NewRequestWithContext(ctx, frame.Method, localURL, bytes.NewReader(frame.Body))
	if err != nil {
		return hubReverseResponse{ID: frame.ID, Status: http.StatusBadRequest, Error: err.Error()}
	}
	req.Header = frame.Header.Clone()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return hubReverseResponse{ID: frame.ID, Status: http.StatusBadGateway, Error: err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return hubReverseResponse{ID: frame.ID, Status: http.StatusBadGateway, Error: err.Error()}
	}
	return hubReverseResponse{ID: frame.ID, Status: resp.StatusCode, Header: resp.Header.Clone(), Body: body}
}
