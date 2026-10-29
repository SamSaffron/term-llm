package serve

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/hub"
)

// Pace real socket writes below the old20MiB/10s threshold. Crucially, a giant
// Gorilla write would hold its control-frame mutex during this entire wait;
// bounded fragments instead yield that mutex between each paced network write.
type throttledUploadConn struct {
	net.Conn
	enabled         atomic.Bool
	maxWrite        atomic.Int64
	maxDeadlineWait atomic.Int64
	deadlineSets    atomic.Int64
	started         chan struct{}
	closed          chan struct{}
	startOnce       sync.Once
	closeOnce       sync.Once
}

func (c *throttledUploadConn) Write(p []byte) (int, error) {
	if c.enabled.Load() {
		for old := c.maxWrite.Load(); int64(len(p)) > old && !c.maxWrite.CompareAndSwap(old, int64(len(p))); old = c.maxWrite.Load() {
		}
		c.startOnce.Do(func() { close(c.started) })
		//27MiB encoded bytes at768KiB/s last~36s: enough to exercise the
		// DEFAULT20s ping and its10s control-write timeout, without test knobs.
		timer := time.NewTimer(time.Duration(len(p)) * time.Second / (768 << 10))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
	return c.Conn.Write(p)
}

func (c *throttledUploadConn) SetWriteDeadline(d time.Time) error {
	if c.enabled.Load() && !d.IsZero() {
		wait := int64(time.Until(d))
		for old := c.maxDeadlineWait.Load(); wait > old && !c.maxDeadlineWait.CompareAndSwap(old, wait); old = c.maxDeadlineWait.Load() {
		}
		c.deadlineSets.Add(1)
	}
	return c.Conn.SetWriteDeadline(d)
}

func (c *throttledUploadConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type throttledUploadListener struct{ net.Listener }

func (l throttledUploadListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &throttledUploadConn{Conn: c, started: make(chan struct{}), closed: make(chan struct{})}, nil
}

func TestHubReverseLargeCanceledUploadKeepsDefaultHeartbeatAndSibling(t *testing.T) {
	node := hub.Node{ID: "slow-upload", Connection: "reverse", BasePath: "/chat", Token: "token"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
	node, _ = s.registry.Lookup(node.ID)
	ts := httptest.NewUnstartedServer(s.handler())
	ts.Listener = throttledUploadListener{ts.Listener}
	ts.Start()
	defer ts.Close()
	peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/api/connect?node_id="+node.ID, http.Header{"Authorization": {"Bearer token"}, hubNodeIDHeader: {node.ID}})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	waitForReverseNode(t, s, node.ID)
	s.reverse.mu.RLock()
	c := s.reverse.conns[node.ID]
	s.reverse.mu.RUnlock()
	defer c.closeWithError(errors.New("throttled test complete"))
	socket := c.conn.UnderlyingConn().(*throttledUploadConn)
	beforePing := c.lastSeenValue()
	ping := make(chan struct{})
	var pingOnce sync.Once
	defaultPing := peer.PingHandler()
	peer.SetPingHandler(func(data string) error { err := defaultPing(data); pingOnce.Do(func() { close(ping) }); return err })
	type observedFrame struct {
		kind, id, path string
		size           int
	}
	frames := make(chan observedFrame, 8)
	peerFailure := make(chan error, 1)
	go func() {
		for {
			var frame hubReverseRequest
			if err := peer.ReadJSON(&frame); err != nil {
				peerFailure <- err
				return
			}
			frames <- observedFrame{frame.Type, frame.ID, frame.Path, len(frame.Body)}
			if frame.Path == "/sibling" {
				if err := peer.WriteJSON(hubReverseResponse{ID: frame.ID, Status: 200, Body: []byte("ok")}); err != nil {
					peerFailure <- err
					return
				}
			}
		}
	}()
	socket.enabled.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := containmentDo(s, ctx, node, "/upload", bytes.Repeat([]byte("x"), 20<<20))
	select {
	case <-socket.started:
	case <-time.After(10 * time.Second):
		t.Fatal("upload never entered socket")
	}
	cancel()
	sibling := containmentDo(s, t.Context(), node, "/sibling", nil)
	// Caller cancellation must not truncate the JSON message. The unchanged
	// production heartbeat must cross that IN-PROGRESS message and receive pong.
	select {
	case <-ping:
	case err := <-peerFailure:
		t.Fatal(err)
	case <-time.After(hubReversePingInterval + 10*time.Second):
		t.Fatal("default ping starved behind large JSON writer")
	}
	select {
	case err := <-first:
		t.Fatalf("upload finished before heartbeat interleaved: %v", err)
	default:
	}
	watchdog := time.After(3 * time.Second)
	for !c.lastSeenValue().After(beforePing) {
		select {
		case <-watchdog:
			t.Fatal("default pong did not refresh heartbeat")
		default:
			runtime.Gosched()
		}
	}
	if !s.reverse.isConnected(node.ID) {
		t.Fatal("canceled in-flight upload detached shared tunnel")
	}
	if err := containmentAwaitWithin(t, first, 70*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("upload error=%v", err)
	}
	if err := containmentAwaitWithin(t, sibling, 10*time.Second); err != nil {
		t.Fatalf("sibling failed after canceled upload: %v", err)
	}
	var uploadID string
	gotCancel, gotSibling := false, false
	watchdog = time.After(5 * time.Second)
	for !gotCancel || !gotSibling {
		select {
		case frame := <-frames:
			switch {
			case frame.path == "/upload":
				if frame.size != 20<<20 {
					t.Fatalf("partial original reached remote: %d", frame.size)
				}
				uploadID = frame.id
			case frame.kind == hubReverseFrameCancel:
				if uploadID == "" || frame.id != uploadID {
					t.Fatalf("remote cancel before complete original: %#v upload=%s", frame, uploadID)
				}
				gotCancel = true
			case frame.path == "/sibling":
				gotSibling = true
			}
		case err := <-peerFailure:
			t.Fatal(err)
		case <-watchdog:
			t.Fatalf("remote cancellation/sibling missing: cancel=%v sibling=%v", gotCancel, gotSibling)
		}
	}
	if socket.maxDeadlineWait.Load() > int64(hubReverseWriteWait) || socket.deadlineSets.Load() < 10 {
		t.Fatalf("fragment deadlines not refreshed/bounded: max=%v updates=%d", time.Duration(socket.maxDeadlineWait.Load()), socket.deadlineSets.Load())
	}

	if socket.maxWrite.Load() > (32<<10)+4096 {
		t.Fatalf("unbounded Gorilla socket write=%d", socket.maxWrite.Load())
	}
	if !s.reverse.isConnected(node.ID) {
		t.Fatal("healthy sibling required tunnel reconnect")
	}
	c.pendingMu.Lock()
	pending := len(c.pending)
	c.pendingMu.Unlock()
	if pending != 0 || len(c.active) != 0 {
		t.Fatalf("cancellation retained pending=%d active=%d", pending, len(c.active))
	}
}
