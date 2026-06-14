package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeReadsIdentityFields(t *testing.T) {
	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/chat/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","agent":"jarvis","version":"1.2.3","capabilities":["web","jobs"]}`))
	}))
	defer backend.Close()

	p := NewProber(http.DefaultTransport)
	st := p.Probe(context.Background(), Node{ID: "n", URL: backend.URL, BasePath: "/chat", Token: "tkn"})

	if gotAuth != "Bearer tkn" {
		t.Errorf("probe Authorization = %q, want Bearer tkn", gotAuth)
	}
	if !st.Reachable || st.State != "ok" {
		t.Fatalf("status = %+v, want reachable ok", st)
	}
	if st.Agent != "jarvis" || st.Version != "1.2.3" || len(st.Capabilities) != 2 {
		t.Errorf("identity = %+v", st)
	}
	if st.LatencyMS < 0 {
		t.Errorf("latency = %d", st.LatencyMS)
	}
}

func TestProbeUnreachable(t *testing.T) {
	p := NewProber(http.DefaultTransport)
	// Reserved TEST-NET-1 address: connection should fail fast via context.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	st := p.Probe(ctx, Node{ID: "n", URL: "http://192.0.2.1:9", BasePath: ""})
	if st.Reachable || st.State != "unreachable" || st.Error == "" {
		t.Fatalf("status = %+v, want unreachable with error", st)
	}
}

func TestProbeNon200(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer backend.Close()

	p := NewProber(http.DefaultTransport)
	st := p.Probe(context.Background(), Node{ID: "n", URL: backend.URL})
	if st.Reachable || st.State != "error 401" {
		t.Fatalf("status = %+v, want error 401", st)
	}
}

func TestProbeAllKeysByID(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer backend.Close()

	p := NewProber(http.DefaultTransport)
	statuses := p.ProbeAll(context.Background(), []Node{
		{ID: "a", URL: backend.URL},
		{ID: "b", URL: "http://192.0.2.1:9"},
	})
	if !statuses["a"].Reachable {
		t.Errorf("a = %+v, want reachable", statuses["a"])
	}
	if statuses["b"].Reachable {
		t.Errorf("b = %+v, want unreachable", statuses["b"])
	}
}
