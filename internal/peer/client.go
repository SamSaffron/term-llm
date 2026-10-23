package peer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/pion/webrtc/v4"
)

// ClientConfig configures a Client.
type ClientConfig struct {
	Limits Limits
}

// Client is an http.RoundTripper that sends requests across a bridge
// DataChannel. The URL scheme and host are ignored; only the origin-relative
// request target is sent. Response bodies stream. Cancelling the request
// context, or closing the body before it ends, sends a cancel frame.
//
// Incoming body frames are delivered in order on Pion's single read loop.
// A consumer that stops reading stalls the channel (head-of-line), which in
// turn pushes SCTP flow control back to the sender instead of buffering
// without bound.
type Client struct {
	conn   *conn
	limits Limits
	sem    chan struct{}
	open   chan struct{}
	nextID atomic.Uint64

	mu      sync.Mutex
	streams map[string]*clientStream
}

var _ http.RoundTripper = (*Client)(nil)

// NewClient serves the requesting side of dc, which must be the bridge channel.
func NewClient(dc *webrtc.DataChannel, cfg ClientConfig) (*Client, error) {
	if err := validChannel(dc); err != nil {
		return nil, err
	}
	limits := cfg.Limits.withDefaults()
	c := &Client{
		conn:    newConn(dc, limits),
		limits:  limits,
		sem:     make(chan struct{}, limits.MaxConcurrentStreams),
		open:    make(chan struct{}),
		streams: map[string]*clientStream{},
	}
	var openOnce sync.Once
	dc.OnOpen(func() { openOnce.Do(func() { close(c.open) }) })
	dc.OnClose(c.closeAll)
	dc.OnMessage(c.onMessage)
	return c, nil
}

// WaitOpen blocks until the channel is open, closed, or ctx is done.
func (c *Client) WaitOpen(ctx context.Context) error {
	select {
	case <-c.open:
		return nil
	case <-c.conn.closed:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close closes the bridge channel and fails in-flight requests.
func (c *Client) Close() error {
	c.conn.close()
	c.closeAll()
	return nil
}

func (c *Client) closeAll() {
	c.conn.markClosed()
	c.mu.Lock()
	streams := c.streams
	c.streams = map[string]*clientStream{}
	c.mu.Unlock()
	for _, s := range streams {
		s.finish(ErrClosed)
		<-c.sem
	}
}

// RoundTrip implements http.RoundTripper.
func (c *Client) RoundTrip(req *http.Request) (*http.Response, error) {
	f, err := c.requestFrame(req)
	if err != nil {
		return nil, err
	}
	ctx := req.Context()
	if err := c.WaitOpen(ctx); err != nil {
		return nil, err
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.conn.closed:
		return nil, ErrClosed
	}
	s := &clientStream{
		client: c,
		id:     f.ID,
		start:  make(chan *Frame, 1),
		chunks: make(chan []byte, 8),
		done:   make(chan struct{}),
		end:    make(chan struct{}),
	}
	c.mu.Lock()
	c.streams[s.id] = s
	c.mu.Unlock()
	if err := c.conn.send(ctx, f); err != nil {
		c.remove(s.id)
		return nil, err
	}
	go s.watch(ctx)

	select {
	case start := <-s.start:
		return s.response(req, start), nil
	case <-s.end:
		select {
		case start := <-s.start: // started and ended back to back
			return s.response(req, start), nil
		default:
			return nil, s.endErr
		}
	}
}

func (c *Client) requestFrame(req *http.Request) (*Frame, error) {
	if req.Body != nil {
		defer req.Body.Close()
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	if req.URL == nil {
		return nil, errors.New("peer: request has no URL")
	}
	target := req.URL.RequestURI()
	if _, err := cleanRequestPath(target); err != nil {
		return nil, fmt.Errorf("peer: %w", err)
	}
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(io.LimitReader(req.Body, int64(c.limits.MaxRequestBodyBytes)+1))
		if err != nil {
			return nil, fmt.Errorf("peer: read request body: %w", err)
		}
		if len(b) > c.limits.MaxRequestBodyBytes {
			return nil, fmt.Errorf("peer: request body exceeds %d bytes", c.limits.MaxRequestBodyBytes)
		}
		body = b
	}
	return &Frame{
		Type:   TypeRequest,
		ID:     strconv.FormatUint(c.nextID.Add(1), 10),
		Method: method,
		Path:   target,
		Header: req.Header.Clone(),
		Body:   body,
	}, nil
}

// remove unregisters a stream, releasing its concurrency slot once.
func (c *Client) remove(id string) bool {
	c.mu.Lock()
	_, ok := c.streams[id]
	delete(c.streams, id)
	c.mu.Unlock()
	if ok {
		<-c.sem
	}
	return ok
}

func (c *Client) onMessage(msg webrtc.DataChannelMessage) {
	f, err := decodeFrame(msg.Data, c.limits.MaxFrameBytes)
	if err != nil {
		if f != nil && f.ID != "" {
			if s := c.lookup(f.ID); s != nil && c.remove(f.ID) {
				s.finish(fmt.Errorf("peer: %w", err))
			}
			return
		}
		c.conn.close()
		return
	}
	s := c.lookup(f.ID)
	if s == nil {
		return // late frame for a finished or cancelled request
	}
	switch f.Type {
	case TypeResponseStart:
		if s.started {
			c.protocolError(s, "duplicate response_start")
			return
		}
		s.started = true
		s.start <- f
	case TypeResponseBody:
		if !s.started {
			c.protocolError(s, "response_body before response_start")
			return
		}
		if len(f.Body) == 0 {
			return
		}
		select {
		case s.chunks <- f.Body:
		case <-s.done:
		case <-s.end:
		}
	case TypeResponseEnd:
		var endErr error
		if f.Error != "" {
			endErr = fmt.Errorf("peer: response failed: %s", f.Error)
		} else if !s.started {
			endErr = errors.New("peer: response_end before response_start")
		}
		if c.remove(s.id) {
			s.finish(endErr)
		}
	}
}

func (c *Client) lookup(id string) *clientStream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[id]
}

func (c *Client) protocolError(s *clientStream, msg string) {
	if c.remove(s.id) {
		s.finish(errors.New("peer: protocol error: " + msg))
		go func() { _ = c.conn.send(context.Background(), &Frame{Type: TypeCancel, ID: s.id}) }()
	}
}

// clientStream is one in-flight request. started is touched only by the
// read loop; endErr is written before end is closed.
type clientStream struct {
	client  *Client
	id      string
	started bool
	start   chan *Frame
	chunks  chan []byte
	done    chan struct{} // closed when the consumer closes the body
	end     chan struct{} // closed when the response ends or fails

	endOnce   sync.Once
	endErr    error
	closeOnce sync.Once
}

func (s *clientStream) finish(err error) {
	s.endOnce.Do(func() {
		s.endErr = err
		close(s.end)
	})
}

// cancel abandons the stream locally and tells the server to stop.
func (s *clientStream) cancel(err error) {
	if s.client.remove(s.id) {
		s.finish(err)
		go func() { _ = s.client.conn.send(context.Background(), &Frame{Type: TypeCancel, ID: s.id}) }()
	}
}

func (s *clientStream) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		s.cancel(ctx.Err())
	case <-s.end:
	case <-s.done:
	}
}

func (s *clientStream) response(req *http.Request, start *Frame) *http.Response {
	header := start.Header
	if header == nil {
		header = http.Header{}
	}
	contentLength := int64(-1)
	if v, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64); err == nil && v >= 0 {
		contentLength = v
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", start.Status, http.StatusText(start.Status)),
		StatusCode:    start.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          &clientBody{s: s},
		ContentLength: contentLength,
		Request:       req,
	}
}

var errBodyClosed = errors.New("peer: read on closed response body")

type clientBody struct {
	s       *clientStream
	pending []byte
}

func (b *clientBody) Read(p []byte) (int, error) {
	for len(b.pending) == 0 {
		select {
		case chunk := <-b.s.chunks:
			b.pending = chunk
		case <-b.s.done:
			return 0, errBodyClosed
		case <-b.s.end:
			select {
			case chunk := <-b.s.chunks:
				b.pending = chunk
			default:
				if b.s.endErr != nil {
					return 0, b.s.endErr
				}
				return 0, io.EOF
			}
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *clientBody) Close() error {
	b.s.closeOnce.Do(func() {
		close(b.s.done)
		b.s.cancel(context.Canceled)
	})
	return nil
}
