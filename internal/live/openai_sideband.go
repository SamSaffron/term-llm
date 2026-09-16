package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	openAISidebandPingInterval    = 20 * time.Second
	openAISidebandWriteWait       = 10 * time.Second
	openAISidebandPongWait        = 60 * time.Second
	openAISidebandInitialBackoff  = 250 * time.Millisecond
	openAISidebandMaxBackoff      = 5 * time.Second
	openAISidebandMaxDialFailures = 8
	openAISidebandSendAttempts    = 3
	openAISidebandEventBuffer     = 64
)

var errOpenAISidebandEnded = errors.New("live: OpenAI session ended")

type openAISidebandConfig struct {
	BaseURL     string
	CallID      string
	APIKey      string
	Dialer      *websocket.Dialer
	EmitStarted bool
	Endpoint    func(baseURL, callID string) (string, error)
	ParseEvent  func([]byte) (Event, error)
	Protocol    string
}

type openAISideband struct {
	cfg          openAISidebandConfig
	events       chan Event
	emitMu       sync.RWMutex
	eventsClosed bool

	mu           sync.Mutex
	conn         *websocket.Conn
	ready        chan struct{}
	stopped      bool
	sessionEnded bool

	writeMu   sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
	finished  chan struct{}
}

func dialOpenAISideband(ctx context.Context, cfg openAISidebandConfig) (*openAISideband, error) {
	endpointBuilder := cfg.Endpoint
	if endpointBuilder == nil {
		endpointBuilder = openAISidebandURL
	}
	endpoint, err := endpointBuilder(cfg.BaseURL, cfg.CallID)
	if err != nil {
		return nil, err
	}
	s := &openAISideband{
		cfg:      cfg,
		events:   make(chan Event, openAISidebandEventBuffer),
		ready:    make(chan struct{}),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
	conn, err := s.dial(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	s.setConn(conn)
	if cfg.EmitStarted {
		s.events <- Event{Kind: EventSessionStarted, RawType: "session.created"}
	}
	go s.run(endpoint, conn)
	return s, nil
}

func openAISidebandURL(baseURL, callID string) (string, error) {
	endpoint, err := openAIWebSocketURL(baseURL, "/realtime")
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("live: invalid OpenAI sideband url: %w", err)
	}
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return "", errors.New("live: OpenAI sideband call id is empty")
	}
	query := parsed.Query()
	query.Set("call_id", callID)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func openAILiveSidebandURL(baseURL, sessionID string) (string, error) {
	endpoint, err := openAIWebSocketURL(baseURL, "/live/sessions")
	if err != nil {
		return "", err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", errors.New("live: OpenAI GPT-Live session id is empty")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("live: invalid OpenAI GPT-Live sideband url: %w", err)
	}
	rawPath := strings.TrimRight(parsed.EscapedPath(), "/") + "/" + url.PathEscape(sessionID) + "/attach"
	path, err := url.PathUnescape(rawPath)
	if err != nil {
		return "", fmt.Errorf("live: encode OpenAI GPT-Live session id: %w", err)
	}
	parsed.Path = path
	parsed.RawPath = rawPath
	return parsed.String(), nil
}

func openAIWebSocketURL(baseURL, suffix string) (string, error) {
	endpoint, err := openAIURL(baseURL, suffix)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("live: invalid OpenAI sideband url: %w", err)
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("live: invalid OpenAI sideband scheme %q", parsed.Scheme)
	}
	return parsed.String(), nil
}

func (s *openAISideband) Events() <-chan Event { return s.events }

func (s *openAISideband) Done() <-chan struct{} { return s.done }

func (s *openAISideband) protocol() string {
	if protocol := strings.TrimSpace(s.cfg.Protocol); protocol != "" {
		return protocol
	}
	return "Realtime"
}

func (s *openAISideband) run(endpoint string, conn *websocket.Conn) {
	defer close(s.finished)
	defer s.closeEvents()
	defer s.markStopped()
	backoff := openAISidebandInitialBackoff
	failures := 0
	for {
		s.readPump(conn)
		_ = conn.Close()
		s.clearConn(conn)
		if s.isStopped() {
			return
		}
		next, err := s.redial(endpoint, &backoff, &failures)
		if err != nil {
			if !errors.Is(err, errOpenAISidebandEnded) {
				s.emit(Event{Kind: EventError, RawType: "sideband", Text: err.Error()})
			}
			return
		}
		conn = next
		s.setConn(conn)
	}
}

func (s *openAISideband) redial(endpoint string, backoff *time.Duration, failures *int) (*websocket.Conn, error) {
	for {
		if s.isStopped() {
			return nil, errOpenAISidebandEnded
		}
		timer := time.NewTimer(*backoff)
		select {
		case <-timer.C:
		case <-s.done:
			timer.Stop()
			return nil, errOpenAISidebandEnded
		}
		ctx, cancel := context.WithTimeout(context.Background(), openAISidebandWriteWait)
		conn, err := s.dial(ctx, endpoint)
		cancel()
		if err == nil {
			*backoff = openAISidebandInitialBackoff
			*failures = 0
			return conn, nil
		}
		if errors.Is(err, errOpenAISidebandEnded) || errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrForbidden) {
			return nil, err
		}
		*failures++
		if *failures >= openAISidebandMaxDialFailures {
			return nil, fmt.Errorf("OpenAI %s control channel unavailable: %w", s.protocol(), err)
		}
		*backoff *= 2
		if *backoff > openAISidebandMaxBackoff {
			*backoff = openAISidebandMaxBackoff
		}
	}
}

func (s *openAISideband) dial(ctx context.Context, endpoint string) (*websocket.Conn, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	dialer := s.cfg.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, resp, err := dialer.DialContext(ctx, endpoint, header)
	if err == nil {
		return conn, nil
	}
	if resp == nil {
		return nil, fmt.Errorf("dial OpenAI %s control channel: %w", s.protocol(), err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: OpenAI %s control channel", ErrUnauthorized, s.protocol())
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: OpenAI %s control channel", ErrForbidden, s.protocol())
	case http.StatusNotFound, http.StatusGone:
		return nil, errOpenAISidebandEnded
	default:
		return nil, fmt.Errorf("dial OpenAI %s control channel: http %d: %w", s.protocol(), resp.StatusCode, err)
	}
}

func (s *openAISideband) readPump(conn *websocket.Conn) {
	donePing := make(chan struct{})
	defer close(donePing)
	_ = conn.SetReadDeadline(time.Now().Add(openAISidebandPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(openAISidebandPongWait))
	})
	go s.pingLoop(conn, donePing)
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(openAISidebandPongWait))
		parser := s.cfg.ParseEvent
		if parser == nil {
			parser = parseOpenAIEvent
		}
		event, err := parser(payload)
		if err != nil {
			log.Printf("[live] dropped malformed OpenAI %s event: %v", s.protocol(), err)
			continue
		}
		if event.Kind == EventUnknown {
			continue
		}
		if event.Kind == EventEnded {
			s.mu.Lock()
			s.sessionEnded = true
			s.mu.Unlock()
		}
		s.emit(event)
		if event.Kind == EventEnded {
			s.markStopped()
			_ = conn.Close()
			return
		}
	}
}

func (s *openAISideband) pingLoop(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(openAISidebandPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-s.done:
			return
		case <-ticker.C:
			s.writeMu.Lock()
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(openAISidebandWriteWait))
			s.writeMu.Unlock()
			if err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

func (s *openAISideband) Send(ctx context.Context, message any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode OpenAI %s message: %w", s.protocol(), err)
	}
	var lastErr error
	for attempt := 0; attempt < openAISidebandSendAttempts; attempt++ {
		conn, err := s.waitConn(ctx)
		if err != nil {
			return err
		}
		s.writeMu.Lock()
		_ = conn.SetWriteDeadline(time.Now().Add(openAISidebandWriteWait))
		err = conn.WriteMessage(websocket.TextMessage, payload)
		_ = conn.SetWriteDeadline(time.Time{})
		s.writeMu.Unlock()
		if err == nil {
			return nil
		}
		lastErr = err
		_ = conn.Close()
	}
	return fmt.Errorf("send OpenAI %s message: %w", s.protocol(), lastErr)
}

func (s *openAISideband) waitConn(ctx context.Context) (*websocket.Conn, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		conn, ready, stopped := s.conn, s.ready, s.stopped
		s.mu.Unlock()
		if stopped {
			return nil, errOpenAISidebandEnded
		}
		if conn != nil {
			return conn, nil
		}
		select {
		case <-ready:
		case <-s.done:
			return nil, errOpenAISidebandEnded
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *openAISideband) setConn(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = conn
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
}

func (s *openAISideband) clearConn(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != conn {
		return
	}
	s.conn = nil
	s.ready = make(chan struct{})
}

func (s *openAISideband) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *openAISideband) markStopped() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		conn := s.conn
		s.conn = nil
		select {
		case <-s.ready:
		default:
			close(s.ready)
		}
		s.mu.Unlock()
		close(s.done)
		if conn != nil {
			_ = conn.Close()
		}
	})
}

func (s *openAISideband) emit(event Event) {
	_ = s.emitContext(context.Background(), event)
}

func (s *openAISideband) emitContext(ctx context.Context, event Event) error {
	s.emitMu.RLock()
	defer s.emitMu.RUnlock()
	if s.eventsClosed {
		return errOpenAISidebandEnded
	}
	select {
	case s.events <- event:
		return nil
	case <-s.done:
		return errOpenAISidebandEnded
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *openAISideband) closeEvents() {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.eventsClosed {
		return
	}
	s.eventsClosed = true
	close(s.events)
}

func (s *openAISideband) Close() {
	s.markStopped()
	<-s.finished
}
