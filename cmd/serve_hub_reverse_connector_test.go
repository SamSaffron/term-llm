package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type reverseJoinTransport struct {
	entered   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func (tr *reverseJoinTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	close(tr.entered)
	<-r.Context().Done()
	close(tr.cancelled)
	// Model a child transport which observes cancellation but has not yet joined.
	<-tr.release
	return nil, r.Context().Err()
}

func TestHubReverseConnectorStopJoinsRequests(t *testing.T) {
	tr := &reverseJoinTransport{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(tr.release) }) }
	defer release()
	connected := make(chan struct{})
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		close(connected)
		if err := conn.WriteJSON(hubReverseRequest{Type: hubReverseFrameRequest, ID: "one", Method: "GET", Path: "/chat/healthz"}); err != nil {
			return
		}
		// The client must actively wake its ReadJSON, not wait for a heartbeat.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer hub.Close()
	c := startHubReverseConnector(context.Background(), hub.URL, "sandbox", "", "http://local.invalid", "/chat", &http.Client{Transport: tr})
	defer func() {
		release()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Stop(cleanupCtx); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("connector lifecycle did not progress")
		}
	}
	wait(connected)
	wait(tr.entered)
	cancelledCtx, cancelStop := context.WithCancel(context.Background())
	cancelStop()
	if err := c.Stop(cancelledCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unjoined stop = %v", err)
	}
	wait(tr.cancelled)
	select {
	case <-c.done:
		t.Fatal("stop completed while a request was still owned")
	default:
	}
	release()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}
