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
	sidebandPingInterval    = 20 * time.Second
	sidebandWriteWait       = 10 * time.Second
	sidebandPongWait        = 60 * time.Second
	sidebandInitialBackoff  = 250 * time.Millisecond
	sidebandMaxBackoff      = 5 * time.Second
	sidebandMaxDialFailures = 8
	sidebandSendAttempts    = 3
	sidebandEventBuffer     = 64
)

// sidebandConfig configures the control-channel connection.
type sidebandConfig struct {
	BaseURL   string
	CallID    string
	SessionID string
	Auth      AuthFunc
	Dialer    *websocket.Dialer
}

// sideband is the provider control channel: it delivers events and carries
// outbound appends, reconnecting transparently while the call is alive.
type sideband struct {
	cfg    sidebandConfig
	events chan Event

	mu      sync.Mutex
	conn    *websocket.Conn
	ready   chan struct{}
	stopped bool

	writeMu   sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
	finished  chan struct{}

	voiceSerial  chan struct{}
	voiceMu      sync.Mutex
	voiceWaiter  *voiceUpdateWaiter
	currentVoice string
}

type voiceUpdateWaiter struct {
	voice  string
	result chan error
}

// dialSideband connects the control channel and starts its read loop. The
// first connection must succeed; later drops are retried in the background.
func dialSideband(ctx context.Context, cfg sidebandConfig) (*sideband, error) {
	endpoint, err := sidebandURL(cfg.BaseURL, cfg.CallID)
	if err != nil {
		return nil, err
	}
	s := &sideband{
		cfg:         cfg,
		events:      make(chan Event, sidebandEventBuffer),
		ready:       make(chan struct{}),
		done:        make(chan struct{}),
		finished:    make(chan struct{}),
		voiceSerial: make(chan struct{}, 1),
	}
	s.voiceSerial <- struct{}{}
	conn, err := s.dial(ctx, endpoint, false)
	if err != nil {
		return nil, err
	}
	s.setConn(conn)
	go s.run(endpoint, conn)
	return s, nil
}

// Events streams parsed control-channel events until the session ends.
func (s *sideband) Events() <-chan Event { return s.events }

func (s *sideband) run(endpoint string, conn *websocket.Conn) {
	defer close(s.finished)
	defer close(s.events)
	backoff := sidebandInitialBackoff
	failures := 0
	for {
		s.readPump(conn)
		s.clearConn(conn)
		if s.isStopped() {
			return
		}
		next, err := s.redial(endpoint, &backoff, &failures)
		if err != nil {
			if !errors.Is(err, errSidebandEnded) {
				s.emit(Event{Kind: EventError, Text: err.Error()})
			}
			return
		}
		conn = next
		s.setConn(conn)
	}
}

var errSidebandEnded = errors.New("live: session ended")

func (s *sideband) redial(endpoint string, backoff *time.Duration, failures *int) (*websocket.Conn, error) {
	for {
		if s.isStopped() {
			return nil, errSidebandEnded
		}
		select {
		case <-s.done:
			return nil, errSidebandEnded
		case <-time.After(*backoff):
		}
		ctx, cancel := context.WithTimeout(context.Background(), sidebandWriteWait)
		conn, err := s.dial(ctx, endpoint, true)
		cancel()
		if err == nil {
			*backoff = sidebandInitialBackoff
			*failures = 0
			return conn, nil
		}
		if errors.Is(err, errSidebandEnded) {
			return nil, errSidebandEnded
		}
		if errors.Is(err, ErrForbidden) {
			return nil, err
		}
		*failures++
		if *failures >= sidebandMaxDialFailures {
			return nil, fmt.Errorf("live control channel unavailable: %w", err)
		}
		*backoff *= 2
		if *backoff > sidebandMaxBackoff {
			*backoff = sidebandMaxBackoff
		}
	}
}

func (s *sideband) dial(ctx context.Context, endpoint string, refresh bool) (*websocket.Conn, error) {
	auth := Auth{}
	if s.cfg.Auth != nil {
		resolved, err := s.cfg.Auth(ctx, refresh)
		if err != nil {
			return nil, fmt.Errorf("live control channel auth: %w", err)
		}
		auth = resolved
	}
	header := http.Header{}
	auth.apply(header, s.cfg.SessionID)
	dialer := s.cfg.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, resp, err := dialer.DialContext(ctx, endpoint, header)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusNotFound, http.StatusGone:
				return nil, errSidebandEnded
			case http.StatusForbidden:
				// Refreshing cannot grant access, so stop instead of retrying.
				return nil, fmt.Errorf("%w: control channel", ErrForbidden)
			}
			return nil, fmt.Errorf("dial live control channel: http %d: %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("dial live control channel: %w", err)
	}
	return conn, nil
}

func (s *sideband) readPump(conn *websocket.Conn) {
	donePing := make(chan struct{})
	defer close(donePing)
	_ = conn.SetReadDeadline(time.Now().Add(sidebandPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(sidebandPongWait))
	})
	go s.pingLoop(conn, donePing)
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(sidebandPongWait))
		event, parseErr := ParseEvent(payload)
		if parseErr != nil {
			log.Printf("[live] dropped malformed control event: %v", parseErr)
			continue
		}
		if event.Kind == EventUnknown {
			continue
		}
		s.notifyVoiceUpdate(&event)
		s.emit(event)
		if event.Kind == EventEnded {
			s.markStopped()
			_ = conn.Close()
			return
		}
	}
}

func (s *sideband) pingLoop(conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(sidebandPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-s.done:
			return
		case <-ticker.C:
			s.writeMu.Lock()
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(sidebandWriteWait))
			s.writeMu.Unlock()
			if err != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

func (s *sideband) emit(event Event) {
	select {
	case s.events <- event:
	case <-s.done:
	}
}

// Send writes one outbound message, waiting for a reconnect when the socket
// dropped mid-session.
func (s *sideband) Send(ctx context.Context, message Outbound) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode live message: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < sidebandSendAttempts; attempt++ {
		conn, err := s.waitConn(ctx)
		if err != nil {
			return err
		}
		s.writeMu.Lock()
		_ = conn.SetWriteDeadline(time.Now().Add(sidebandWriteWait))
		err = conn.WriteMessage(websocket.TextMessage, payload)
		_ = conn.SetWriteDeadline(time.Time{})
		s.writeMu.Unlock()
		if err == nil {
			return nil
		}
		lastErr = err
		// Force the read pump to notice the dead socket so the retry lands on
		// a fresh connection.
		_ = conn.Close()
	}
	return fmt.Errorf("send live message: %w", lastErr)
}

// updateVoice serializes a partial session update and waits until the provider
// echoes the requested voice in session.updated. A canceled call leaves the
// update pending until its late acknowledgement or an error arrives, preventing
// that acknowledgement from being mistaken for a later request.
func (s *sideband) updateVoice(ctx context.Context, voice string, message Outbound) error {
	select {
	case <-s.voiceSerial:
	case <-s.done:
		return errSidebandEnded
	case <-ctx.Done():
		return ctx.Err()
	}

	waiter := &voiceUpdateWaiter{voice: voice, result: make(chan error, 1)}
	s.voiceMu.Lock()
	s.voiceWaiter = waiter
	s.voiceMu.Unlock()

	if err := s.Send(ctx, message); err != nil {
		s.finishVoiceUpdate(waiter, err)
		return err
	}

	select {
	case err := <-waiter.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errSidebandEnded
	}
}

func (s *sideband) notifyVoiceUpdate(event *Event) {
	s.voiceMu.Lock()
	if event.Kind == EventSessionUpdated && event.Voice != "" {
		s.currentVoice = event.Voice
	}
	waiter := s.voiceWaiter
	s.voiceMu.Unlock()
	if waiter == nil {
		return
	}

	switch event.Kind {
	case EventSessionUpdated:
		if event.Voice != waiter.voice {
			return
		}
		s.finishVoiceUpdate(waiter, nil)
	case EventError:
		event.ErrorHandled = true
		s.finishVoiceUpdate(waiter, fmt.Errorf("live voice update failed: %s", event.Text))
	case EventEnded:
		s.finishVoiceUpdate(waiter, errSidebandEnded)
	}
}

func (s *sideband) finishVoiceUpdate(waiter *voiceUpdateWaiter, err error) {
	s.voiceMu.Lock()
	if s.voiceWaiter != waiter {
		s.voiceMu.Unlock()
		return
	}
	s.voiceWaiter = nil
	s.voiceMu.Unlock()

	waiter.result <- err
	s.voiceSerial <- struct{}{}
}

func (s *sideband) failVoiceUpdate(err error) {
	s.voiceMu.Lock()
	waiter := s.voiceWaiter
	s.voiceMu.Unlock()
	if waiter != nil {
		s.finishVoiceUpdate(waiter, err)
	}
}

func (s *sideband) waitConn(ctx context.Context) (*websocket.Conn, error) {
	for {
		s.mu.Lock()
		conn, ready, stopped := s.conn, s.ready, s.stopped
		s.mu.Unlock()
		if stopped {
			return nil, errSidebandEnded
		}
		if conn != nil {
			return conn, nil
		}
		select {
		case <-ready:
		case <-s.done:
			return nil, errSidebandEnded
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *sideband) setConn(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = conn
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
}

func (s *sideband) clearConn(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == conn {
		s.conn = nil
		s.ready = make(chan struct{})
	}
	_ = conn.Close()
}

func (s *sideband) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *sideband) markStopped() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.failVoiceUpdate(errSidebandEnded)
}

// Close stops the control channel. It is safe to call more than once.
func (s *sideband) Close() {
	s.markStopped()
	s.closeOnce.Do(func() { close(s.done) })
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	select {
	case <-s.finished:
	case <-time.After(sidebandWriteWait):
	}
}

// sidebandURL builds wss://host/v1/live/{callID} from the configured base.
func sidebandURL(baseURL, callID string) (string, error) {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return "", errors.New("live: sideband base url is empty")
	}
	call := strings.TrimSpace(callID)
	if call == "" {
		return "", errors.New("live: sideband call id is empty")
	}
	parsed, err := url.Parse(strings.TrimRight(trimmed, "/"))
	if err != nil {
		return "", fmt.Errorf("live: invalid sideband base url %q: %w", baseURL, err)
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("live: unsupported sideband scheme %q", parsed.Scheme)
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, "/live") {
		path += "/live"
	}
	parsed.Path = path + "/" + call
	return parsed.String(), nil
}
