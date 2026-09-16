package widgets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/restart"
)

func TestWidgetWebSocketAllowsReloadAfterDisconnect(t *testing.T) {
	c := &restart.Coordinator{}
	previous := restart.Default
	restart.Default = c
	defer func() { restart.Default = previous }()

	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{entries: map[string]*widgetEntry{
		"socket": {
			manifest: &Manifest{ID: "socket", Mount: "socket"},
			state:    stateRunning,
			proxy:    httputil.NewSingleHostReverseProxy(target),
		},
	}}
	transport := &restart.HTTPTransport{Coordinator: c}
	handlerDone := make(chan struct{})
	handler := transport.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/widgets/socket")
		m.Proxy("socket", w, r)
	}))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		handler.ServeHTTP(w, r)
	}))
	server.Config.ConnContext = transport.ConnContext
	server.Config.ConnState = transport.ConnState
	server.Start()
	defer server.Close()
	conn, _, err := (&websocket.Dialer{HandshakeTimeout: time.Second}).Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/widgets/socket/", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	exec := make(chan struct{}, 1)
	stop, err := c.Bind(context.Background(), func(context.Context) error {
		exec <- struct{}{}
		return errors.New("fixture exec")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	select {
	case <-exec:
		t.Fatal("reload replaced an unfinished WebSocket proxy")
	case <-time.After(50 * time.Millisecond):
	}
	if _, release, err := c.Root(context.Background()); !errors.Is(err, restart.ErrDraining) {
		if release != nil {
			release()
		}
		t.Fatalf("new root during drain: %v", err)
	}
	_ = conn.Close()
	for _, done := range []<-chan struct{}{upstreamDone, handlerDone, exec} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("WebSocket disconnect did not settle proxy and allow reload")
		}
	}
}
