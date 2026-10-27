package peer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// ServerConfig configures a Server. Handler, SessionGate and Authorize are
// required; the server fails closed without them.
type ServerConfig struct {
	// Handler is the fixed handler every bridged request is served by.
	Handler http.Handler
	// SessionGate is consulted before each request and must return nil only
	// while the remote peer belongs to an authenticated session.
	SessionGate func(ctx context.Context) error
	// Authorize is consulted for each sanitised request after the session
	// gate passes; a non-nil error rejects the route with 403.
	Authorize func(r *http.Request) error
	// Host is the Host presented to Handler; defaults to "localhost".
	Host string
	// RemoteAddr is presented to Handler; defaults to "webrtc-peer".
	RemoteAddr string
	Limits     Limits
	// MaxChannels bounds bridge channels served at once by this Server
	// across all callers of ServeChannel; defaults to DefaultMaxChannels.
	// Endpoint.Serve additionally limits each peer to one channel.
	MaxChannels int
	// OnClose runs after a bridge channel closes and all of its request handlers drain.
	OnClose func()
	// Observer receives closed, privacy-safe lifecycle events. Nil disables all
	// diagnostic classification and emission.
	Observer DiagnosticObserver
	// Now supplies the monotonic clock used for request durations.
	Now func() time.Time
	// Logf receives operational diagnostics; nil discards them.
	Logf func(format string, args ...any)

	testOnBackpressure func()
}

// Server serves bridge DataChannels with a fixed http.Handler. Concurrency
// limits apply across all channels served by the same Server.
type Server struct {
	cfg         ServerConfig
	sem         chan struct{}
	channels    chan struct{}
	nextRequest atomic.Uint64
}

// DefaultMaxChannels is the default ServerConfig.MaxChannels.
const DefaultMaxChannels = 16

// ErrTooManyChannels reports that a Server is already serving MaxChannels.
var ErrTooManyChannels = errors.New("peer: too many bridge channels")

// NewServer validates cfg and returns a Server.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Handler == nil {
		return nil, errors.New("peer: server handler is required")
	}
	if cfg.SessionGate == nil {
		return nil, errors.New("peer: server session gate is required")
	}
	if cfg.Authorize == nil {
		return nil, errors.New("peer: server route authorizer is required")
	}
	if cfg.Host == "" {
		cfg.Host = "localhost"
	}
	if cfg.RemoteAddr == "" {
		cfg.RemoteAddr = "webrtc-peer"
	}
	if cfg.Observer != nil && cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.MaxChannels <= 0 {
		cfg.MaxChannels = DefaultMaxChannels
	}
	cfg.Limits = cfg.Limits.withDefaults()
	return &Server{
		cfg:      cfg,
		sem:      make(chan struct{}, cfg.Limits.MaxConcurrentStreams),
		channels: make(chan struct{}, cfg.MaxChannels),
	}, nil
}

// ServeChannel starts serving requests arriving on dc and returns at once.
// Serving stops, and in-flight handler contexts are cancelled, when dc
// closes or ctx is done (which also closes dc). A channel that is not the
// reliable, ordered bridge channel is closed and rejected, as is any channel
// beyond MaxChannels concurrently served ones (ErrTooManyChannels).
func (s *Server) ServeChannel(ctx context.Context, dc *webrtc.DataChannel) error {
	if err := validChannel(dc); err != nil {
		_ = dc.Close()
		return err
	}
	select {
	case s.channels <- struct{}{}:
	default:
		_ = dc.Close()
		return ErrTooManyChannels
	}
	ctx, cancel := context.WithCancel(ctx)
	sc := &serverConn{
		srv:     s,
		conn:    newConn(dc, s.cfg.Limits),
		ctx:     ctx,
		cancel:  cancel,
		streams: map[string]*serverStream{},
		replies: make(chan struct{}, s.cfg.Limits.MaxConcurrentStreams),
	}
	sc.conn.testOnBackpressure = s.cfg.testOnBackpressure
	if s.cfg.Observer != nil {
		dc.OnOpen(func() {
			s.cfg.Observer(DiagnosticEvent{Stage: DiagnosticChannelOpen, Outcome: DiagnosticOutcomeConnected})
		})
	}
	dc.OnClose(func() {
		sc.conn.markClosed()
		cancel()
	})
	dc.OnMessage(sc.onMessage)
	go func() {
		<-ctx.Done()
		sc.mu.Lock()
		sc.closing = true
		sc.mu.Unlock()
		sc.conn.close()
		sc.wg.Wait()
		<-s.channels
		if s.cfg.OnClose != nil {
			s.cfg.OnClose()
		}
	}()
	return nil
}

type serverConn struct {
	srv    *Server
	conn   *conn
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	wg      sync.WaitGroup
	closing bool
	streams map[string]*serverStream
	// replies bounds pending rejection replies so a peer that floods
	// requests without reading cannot pile up goroutines.
	replies chan struct{}
}

type serverStream struct {
	cancel context.CancelFunc
	trace  *requestDiagnostics
}

func (sc *serverConn) onMessage(msg webrtc.DataChannelMessage) {
	f, err := decodeFrame(msg.Data, sc.srv.cfg.Limits.MaxFrameBytes)
	if err != nil {
		if f != nil && f.ID != "" && len(f.ID) <= maxIDLength && f.Type == TypeRequest {
			sc.reply(f.ID, http.StatusBadRequest, err.Error())
			return
		}
		sc.srv.cfg.Logf("peer: closing bridge channel on protocol violation: %v", err)
		sc.cancel()
		return
	}
	switch f.Type {
	case TypeRequest:
		sc.startRequest(f)
	case TypeCancel:
		sc.mu.Lock()
		stream := sc.streams[f.ID]
		if stream != nil && stream.trace != nil {
			stream.trace.peerCanceled.Store(true)
		}
		sc.mu.Unlock()
		if stream != nil {
			stream.cancel()
		}
	}
}

// reply sends a complete short response without blocking the read loop.
func (sc *serverConn) reply(id string, status int, msg string) {
	sc.background(func() { writeError(newResponseWriter(sc.ctx, sc.conn, id, false, nil), status, msg) })
}

// background runs a bounded control send off the read loop; when the bound
// is exhausted the peer is not draining replies and the channel is closed.
func (sc *serverConn) background(fn func()) {
	sc.mu.Lock()
	if sc.closing {
		sc.mu.Unlock()
		return
	}
	select {
	case sc.replies <- struct{}{}:
	default:
		sc.mu.Unlock()
		sc.srv.cfg.Logf("peer: closing bridge channel: too many pending rejections")
		sc.cancel()
		return
	}
	sc.wg.Add(1)
	sc.mu.Unlock()
	go func() {
		defer sc.wg.Done()
		defer func() { <-sc.replies }()
		fn()
	}()
}

func (sc *serverConn) startRequest(f *Frame) {
	sc.mu.Lock()
	if sc.closing || sc.ctx.Err() != nil {
		sc.mu.Unlock()
		return
	}
	if _, dup := sc.streams[f.ID]; dup {
		sc.mu.Unlock()
		sc.background(func() {
			_ = sc.conn.send(sc.ctx, &Frame{Type: TypeResponseEnd, ID: f.ID, Error: "duplicate request id"})
		})
		return
	}
	select {
	case sc.srv.sem <- struct{}{}:
	default:
		sc.mu.Unlock()
		sc.reply(f.ID, http.StatusServiceUnavailable, "too many concurrent requests")
		return
	}
	ctx, cancel := context.WithCancel(sc.ctx)
	trace := sc.srv.newRequestDiagnostics(f)
	sc.streams[f.ID] = &serverStream{cancel: cancel, trace: trace}
	sc.wg.Add(1)
	sc.mu.Unlock()

	go func() {
		defer sc.wg.Done()
		defer func() {
			sc.mu.Lock()
			delete(sc.streams, f.ID)
			sc.mu.Unlock()
			cancel()
			<-sc.srv.sem
		}()
		sc.serve(ctx, f, trace)
	}()
}

func (sc *serverConn) serve(ctx context.Context, f *Frame, trace *requestDiagnostics) {
	cfg := &sc.srv.cfg
	rw := newResponseWriter(ctx, sc.conn, f.ID, f.Method == http.MethodHead, trace)
	req, status, err := sc.buildRequest(ctx, f)
	if err != nil {
		writeError(rw, status, err.Error())
		return
	}
	if trace != nil {
		trace.start()
	}
	defer func() {
		if p := recover(); p != nil {
			if p != http.ErrAbortHandler {
				cfg.Logf("peer: handler panic serving %s %s: %v", f.Method, req.URL.Path, p)
			}
			rw.abort("internal error")
		}
	}()
	cfg.Handler.ServeHTTP(rw, req)
	rw.finish("")
}

// buildRequest applies the session gate, request validation and route
// authorization, in that order, returning the status to send on failure.
func (sc *serverConn) buildRequest(ctx context.Context, f *Frame) (*http.Request, int, error) {
	cfg := &sc.srv.cfg
	if err := cfg.SessionGate(ctx); err != nil {
		return nil, http.StatusUnauthorized, errors.New("unauthenticated")
	}
	if requestsWebSocket(f.Header) {
		return nil, http.StatusNotImplemented, errors.New("websocket upgrade is not supported over the peer bridge")
	}
	if !allowedMethods[f.Method] {
		return nil, http.StatusMethodNotAllowed, fmt.Errorf("method %q not allowed", f.Method)
	}
	u, err := cleanRequestPath(f.Path)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	header, err := sanitizeRequestHeader(f.Header)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if len(f.Body) > cfg.Limits.MaxRequestBodyBytes {
		return nil, http.StatusRequestEntityTooLarge, errors.New("request body too large")
	}
	var body io.ReadCloser = http.NoBody
	if len(f.Body) > 0 {
		body = io.NopCloser(bytes.NewReader(f.Body))
	}
	req := (&http.Request{
		Method:        f.Method,
		URL:           u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          body,
		ContentLength: int64(len(f.Body)),
		Host:          cfg.Host,
		RemoteAddr:    cfg.RemoteAddr,
		RequestURI:    f.Path,
	}).WithContext(ctx)
	if err := cfg.Authorize(req); err != nil {
		return nil, http.StatusForbidden, errors.New("forbidden")
	}
	return req, 0, nil
}

func requestsWebSocket(header http.Header) bool {
	if strings.EqualFold(strings.TrimSpace(header.Get("Upgrade")), "websocket") || header.Get("Sec-Websocket-Key") != "" {
		return true
	}
	for _, value := range header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

func writeError(rw *responseWriter, status int, msg string) {
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	switch status {
	case http.StatusUnauthorized:
		rw.Header().Set(PeerErrorHeader, PeerErrorAuthorizationRevoked)
	case http.StatusForbidden:
		rw.Header().Set(PeerErrorHeader, PeerErrorScopeDenied)
	}
	rw.WriteHeader(status)
	_, _ = io.WriteString(rw, msg+"\n")
	rw.finish("")
}
