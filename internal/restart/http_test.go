package restart

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPDrainWaitsForTransportFlush(t *testing.T) {
	c := &Coordinator{}
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	state := c.HTTPConnections()
	state(conn, http.StateActive)
	c.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("result")) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil))
	called := make(chan struct{}, 1)
	stop, err := c.Bind(context.Background(), func(context.Context) error { called <- struct{}{}; return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	select {
	case <-called:
		t.Fatal("handler return was mistaken for response flush")
	default:
	}
	rejected := httptest.NewRecorder()
	c.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("admitted while draining") })).ServeHTTP(rejected, httptest.NewRequest("POST", "/", nil))
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatal(rejected.Code)
	}
	state(conn, http.StateIdle)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("idle connection held drain open")
	}
	waitAttempt(t, c)
	state(conn, http.StateActive)
	state(conn, http.StateClosed)
	// Failed exec reopened both operation and connection admission.
	response := httptest.NewRecorder()
	c.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(response, httptest.NewRequest("POST", "/", nil))
	if response.Code != http.StatusNoContent {
		t.Fatal(response.Code)
	}
}

func TestHTTPConnectionsRefuseNewWorkWhileDraining(t *testing.T) {
	c := &Coordinator{}
	_, release, _ := c.Enter(context.Background())
	defer release()
	stop, err := c.Bind(context.Background(), func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	c.HTTPConnections()(conn, http.StateActive)
	if _, err := peer.Write([]byte("new work")); err == nil {
		t.Fatal("draining accepted connection")
	}
}
