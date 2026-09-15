package live

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func wsURL(t *testing.T, server *httptest.Server) string {
	t.Helper()
	return "http" + strings.TrimPrefix(server.URL, "http")
}

// sidebandServer is a control-channel fake: each accepted connection is handed
// to the supplied handler.
func sidebandServer(t *testing.T, handle func(conn *websocket.Conn, attempt int, handshake http.Header), reject func(attempt int) int) *httptest.Server {
	t.Helper()
	var attempts atomic.Int64
	upgrader := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := int(attempts.Add(1))
		if reject != nil {
			if status := reject(attempt); status != 0 {
				w.WriteHeader(status)
				return
			}
		}
		header := r.Header.Clone()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		handle(conn, attempt, header)
	}))
}

func TestSidebandDeliversParsedEvents(t *testing.T) {
	handshake := make(chan http.Header, 1)
	server := sidebandServer(t, func(conn *websocket.Conn, _ int, header http.Header) {
		defer conn.Close()
		handshake <- header
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.started"}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"output_transcript.added","item":{"text":"hi"}}`))
		time.Sleep(200 * time.Millisecond)
	}, nil)
	defer server.Close()

	channel, err := dialSideband(context.Background(), sidebandConfig{
		BaseURL: wsURL(t, server), CallID: "rtc_1", SessionID: "sess_1",
		Auth: func(context.Context, bool) (Auth, error) { return testAuth(), nil },
	})
	if err != nil {
		t.Fatalf("dialSideband: %v", err)
	}
	defer channel.Close()

	first := receiveEvent(t, channel.Events())
	if first.Kind != EventSessionStarted {
		t.Fatalf("first event = %q", first.Kind)
	}
	second := receiveEvent(t, channel.Events())
	if second.Kind != EventAssistantTranscript || second.Text != "hi" {
		t.Fatalf("second event = %+v", second)
	}

	// The control channel joins with the same identity and protocol headers the
	// call was created with.
	header := <-handshake
	for name, want := range map[string]string{
		"OpenAI-Alpha":       "quicksilver=v2",
		"Authorization":      "Bearer token-123",
		"ChatGPT-Account-ID": "acct-9",
		"X-Session-Id":       "sess_1",
	} {
		if got := header.Get(name); got != want {
			t.Fatalf("handshake header %s = %q, want %q", name, got, want)
		}
	}
}

func TestSidebandReconnectsAndDeliversPendingSend(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
		refresh  atomic.Bool
	)
	delivered := make(chan struct{})
	reconnected := make(chan struct{})
	server := sidebandServer(t, func(conn *websocket.Conn, attempt int, _ http.Header) {
		if attempt == 1 {
			// Drop the first connection so the client must reconnect.
			conn.Close()
			return
		}
		defer conn.Close()
		select {
		case reconnected <- struct{}{}:
		default:
		}
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			mu.Lock()
			received = append(received, string(payload))
			mu.Unlock()
			select {
			case delivered <- struct{}{}:
			default:
			}
		}
	}, nil)
	defer server.Close()

	channel, err := dialSideband(context.Background(), sidebandConfig{
		BaseURL: wsURL(t, server), CallID: "rtc_1",
		Auth: func(_ context.Context, forceRefresh bool) (Auth, error) {
			if forceRefresh {
				refresh.Store(true)
			}
			return testAuth(), nil
		},
	})
	if err != nil {
		t.Fatalf("dialSideband: %v", err)
	}
	defer channel.Close()

	select {
	case <-reconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("the control channel never reconnected")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := channel.Send(ctx, DelegationContextAppend("item_1", ChannelSpeakable, "done")); err != nil {
		t.Fatalf("Send after reconnect: %v", err)
	}
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("message was never delivered after the reconnect")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || !strings.Contains(received[0], `"delegation.context.append"`) {
		t.Fatalf("received = %v", received)
	}
	if !refresh.Load() {
		t.Fatal("reconnect did not re-resolve credentials with a refresh")
	}
}

func TestSidebandStopsWhenCallIsGone(t *testing.T) {
	server := sidebandServer(t, func(conn *websocket.Conn, _ int, _ http.Header) {
		conn.Close()
	}, func(attempt int) int {
		if attempt > 1 {
			return http.StatusGone
		}
		return 0
	})
	defer server.Close()

	channel, err := dialSideband(context.Background(), sidebandConfig{
		BaseURL: wsURL(t, server), CallID: "rtc_1",
		Auth: func(context.Context, bool) (Auth, error) { return testAuth(), nil },
	})
	if err != nil {
		t.Fatalf("dialSideband: %v", err)
	}
	defer channel.Close()

	for {
		select {
		case event, ok := <-channel.Events():
			if !ok {
				return
			}
			if event.Kind == EventError {
				t.Fatalf("410 should end the session quietly, got %q", event.Text)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the control channel never stopped after 410")
		}
	}
}

func TestDialSidebandFailsWhenCallIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	if _, err := dialSideband(context.Background(), sidebandConfig{
		BaseURL: wsURL(t, server), CallID: "rtc_1",
		Auth: func(context.Context, bool) (Auth, error) { return testAuth(), nil },
	}); err == nil {
		t.Fatal("expected the initial dial to fail")
	}
}

func receiveEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("event channel closed early")
		}
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}
	}
}
