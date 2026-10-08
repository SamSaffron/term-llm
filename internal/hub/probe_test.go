package hub

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	p := NewProber(http.DefaultTransport)
	st := p.Probe(context.Background(), Node{ID: "n", URL: deadURL, BasePath: ""})
	if st.Reachable || st.State != "unreachable" || st.Error == "" {
		t.Fatalf("status = %+v, want unreachable with error", st)
	}
}

func TestProbeDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer target.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/metadata", http.StatusFound)
	}))
	defer backend.Close()

	p := NewProber(http.DefaultTransport)
	st := p.Probe(context.Background(), Node{ID: "n", URL: backend.URL, BasePath: "/chat", Token: "tkn"})
	if redirected.Load() {
		t.Fatal("probe followed redirect to target server")
	}
	if st.Reachable || st.State != "error 302" {
		t.Fatalf("status = %+v, want error 302 without following redirect", st)
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

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	p := NewProber(http.DefaultTransport)
	statuses := p.ProbeAll(context.Background(), []Node{
		{ID: "a", URL: backend.URL},
		{ID: "b", URL: deadURL},
	})
	if !statuses["a"].Reachable {
		t.Errorf("a = %+v, want reachable", statuses["a"])
	}
	if statuses["b"].Reachable {
		t.Errorf("b = %+v, want unreachable", statuses["b"])
	}
}

// probeTestBody tracks consumption so the bound is checked without allocating
// or serving an arbitrarily large response over the network.
type probeTestBody struct {
	io.Reader
	read   int
	closed bool
}

func (b *probeTestBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *probeTestBody) Close() error {
	b.closed = true
	return nil
}

type probeTestTransport func(*http.Request) (*http.Response, error)

func (f probeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestProbeHealthBodyBoundAndBestEffort(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"malformed", `{"agent":"partial","capabilities":`},
		{"oversized identity", `{"agent":"` + strings.Repeat("a", HealthResponseMaxBytes) + `"}`},
		{"oversized unknown field", `{"agent":"partial","unknown":"` + strings.Repeat("a", HealthResponseMaxBytes) + `"}`},
		{"oversized whitespace", strings.Repeat(" ", HealthResponseMaxBytes) + `{"agent":"hidden"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &probeTestBody{Reader: strings.NewReader(tc.body)}
			p := NewProber(probeTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
			}))
			st := p.Probe(context.Background(), Node{ID: "n", URL: "http://node"})
			if !st.Reachable || st.State != "ok" || st.Error != "" {
				t.Fatalf("invalid health JSON changed best-effort reachability: %+v", st)
			}
			if st.Agent != "" || st.Version != "" || len(st.Capabilities) != 0 {
				t.Fatalf("invalid health JSON published partial identity: %+v", st)
			}
			if body.read > HealthResponseMaxBytes {
				t.Fatalf("health body consumed %d bytes, limit %d", body.read, HealthResponseMaxBytes)
			}
			if !body.closed {
				t.Fatal("health body was not closed")
			}
		})
	}
}
