package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

func TestHubHealthCacheFreshness(t *testing.T) {
	n := orderTestNodes()[0]
	now := time.Unix(1000, 0)
	var cache hubHealthCache
	assertUnknown := func(status hub.Status) {
		t.Helper()
		if status.State != "unknown" || status.Reachable {
			t.Fatalf("expected unreachable unknown status, got %+v", status)
		}
	}
	assertUnknown(cache.status(n, now))
	cache.retain([]hub.Node{n})
	assertUnknown(cache.status(n, now))
	cache.publish(context.Background(), n, now, hub.Status{State: "ok", Reachable: true})
	if status := cache.status(n, now.Add(hubHealthFreshness-time.Nanosecond)); !status.Reachable {
		t.Fatalf("fresh observation = %+v", status)
	}
	assertUnknown(cache.status(n, now.Add(hubHealthFreshness)))
}

func TestHubHealthCacheIdentityAndPruning(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*hub.Node)
	}{
		{"id", func(n *hub.Node) { n.ID = "replacement" }},
		{"url", func(n *hub.Node) { n.URL = "http://replacement" }},
		{"base path", func(n *hub.Node) { n.BasePath = "/replacement" }},
		{"connection", func(n *hub.Node) { n.Connection = "reverse" }},
		{"token", func(n *hub.Node) { n.Token = "replacement" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := orderTestNodes()[0]
			now := time.Now()
			var cache hubHealthCache
			cache.retain([]hub.Node{n})
			cache.publish(context.Background(), n, now, hub.Status{State: "ok", Reachable: true})
			replacement := n
			tc.change(&replacement)
			if status := cache.status(replacement, now); status.State != "unknown" || status.Reachable {
				t.Fatalf("replacement reused old observation: %+v", status)
			}
			cache.retain([]hub.Node{replacement})
			cache.publish(context.Background(), n, now.Add(time.Second), hub.Status{State: "ok", Reachable: true})
			if len(cache.entries) != 1 {
				t.Fatalf("late observation reinserted removed identity: %d entries", len(cache.entries))
			}
			cache.retain(nil)
			if len(cache.entries) != 0 {
				t.Fatal("removed entries were not pruned")
			}
		})
	}
}

func TestHubHealthCacheRejectsOlderAndCanceledObservations(t *testing.T) {
	n := orderTestNodes()[0]
	now := time.Now()
	var cache hubHealthCache
	cache.retain([]hub.Node{n})
	cache.publish(context.Background(), n, now, hub.Status{State: "ok", Reachable: true})
	cache.publish(context.Background(), n, now.Add(-time.Second), hub.Status{State: "unreachable"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cache.publish(ctx, n, now.Add(time.Second), hub.Status{State: "unreachable"})
	if status := cache.status(n, now); !status.Reachable || status.State != "ok" {
		t.Fatalf("older/canceled observation poisoned cache: %+v", status)
	}
}

type hubHealthTestTransport func(*http.Request) (*http.Response, error)

func (f hubHealthTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func hubHealthTestResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"status":"ok","agent":"test"}`))}
}

func waitHubHealthSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("health monitor did not reach expected boundary")
	}
}

func TestHubHealthPublishesHealthyBeforeBlockedNodes(t *testing.T) {
	// Eight workers receive one healthy and seven blocked nodes. A ninth
	// request can start only after the healthy worker published its result.
	// This gives a channel-based publication barrier, without timing sleeps.
	nodes := make([]hub.Node, hubHealthConcurrency+1)
	for i := range nodes {
		id := fmt.Sprintf("node-%02d", i)
		nodes[i] = hub.Node{ID: id, Name: id, URL: "http://" + id, BasePath: "/chat"}
	}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: nodes}), nil)
	s.nodeAPIClient = nil
	witness := make(chan struct{})
	s.prober = hub.NewProber(hubHealthTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/chat/healthz" {
			t.Errorf("unexpected live request: %s", r.URL)
		}
		switch r.URL.Host {
		case nodes[0].ID:
			return hubHealthTestResponse(), nil
		case nodes[len(nodes)-1].ID:
			close(witness)
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		s.collectNodeHealth(ctx)
	}()
	defer func() { cancel(); waitHubHealthSignal(t, finished) }()
	waitHubHealthSignal(t, witness)
	views, err := s.collectSidebarNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !views[0].Status.Reachable || views[0].Status.Agent != "test" {
		t.Fatalf("healthy node was held behind blocked nodes: %+v", views[0].Status)
	}
	for _, view := range views[1:] {
		if view.Status.State != "unknown" || view.Status.Reachable {
			t.Fatalf("blocked node unexpectedly published: %+v", view.Status)
		}
	}
	select {
	case <-finished:
		t.Fatal("cycle finished before blocked probes")
	default:
	}
}

func TestHubHealthMonitorPublishesOfflineResults(t *testing.T) {
	n := orderTestNodes()[0]
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{n}}), nil)
	s.nodeAPIClient = nil
	s.prober = hub.NewProber(hubHealthTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/chat/healthz" || r.Header.Get("Authorization") != "Bearer "+n.Token {
			t.Errorf("unexpected health request: path=%s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable", Body: io.NopCloser(strings.NewReader("offline"))}, nil
	}))
	s.collectNodeHealth(context.Background())
	status := s.healthCache.status(n, time.Now())
	if status.State != "error 503" || status.Reachable {
		t.Fatalf("offline health result = %+v", status)
	}
	// Once collected, even an offline sidebar read never needs a prober.
	s.prober = nil
	views, err := s.collectSidebarNodes(context.Background())
	if err != nil || len(views) != 1 || views[0].Status.State != "error 503" || views[0].Status.Reachable {
		t.Fatalf("offline sidebar = %+v, err = %v", views, err)
	}
}

func TestHubHealthMonitorStartsImmediatelyAndJoinsOnCancellation(t *testing.T) {
	n := orderTestNodes()[0]
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{n}}), nil)
	s.healthCache.retain([]hub.Node{n})
	s.healthCache.publish(context.Background(), n, time.Now(), hub.Status{State: "ok", Reachable: true})
	started := make(chan struct{})
	s.prober = hub.NewProber(hubHealthTestTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	s.startHealthMonitor()
	finished := make(chan struct{})
	defer func() { s.healthCancel(); s.healthWG.Wait() }()
	waitHubHealthSignal(t, started)
	s.healthCancel()
	go func() { s.healthWG.Wait(); close(finished) }()
	waitHubHealthSignal(t, finished)
	if status := s.healthCache.status(n, time.Now()); !status.Reachable {
		t.Fatalf("monitor cancellation poisoned previous health: %+v", status)
	}
}

func TestHubSidebarHealthUsesLiveReverseConnection(t *testing.T) {
	n := hub.Node{ID: "reverse", Connection: "reverse", BasePath: "/chat"}
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{n}}), nil)
	s.prober, s.nodeAPIClient = nil, nil
	s.healthCache.retain([]hub.Node{n})
	s.healthCache.publish(context.Background(), n, time.Now(), hub.Status{Reachable: true, State: "connected", Version: "old-socket"})
	assertStatus := func(state string, reachable bool) hub.Status {
		t.Helper()
		views, err := s.collectSidebarNodes(context.Background())
		if err != nil || len(views) != 1 {
			t.Fatalf("sidebar views = %+v, err = %v", views, err)
		}
		status := views[0].Status
		if status.State != state || status.Reachable != reachable || status.Version != "" {
			t.Fatalf("reverse status = %+v, want %s reachable=%v without old metadata", status, state, reachable)
		}
		return status
	}
	assertStatus("disconnected", false)
	connectedAt := time.Unix(1000, 0)
	s.reverse.conns[n.ID] = &hubReverseConnection{connectedAt: connectedAt, lastSeen: connectedAt}
	status := assertStatus("connected", true)
	if status.Details["connected_at"] != connectedAt.Format(time.RFC3339) {
		t.Fatalf("wrong live socket details: %+v", status.Details)
	}
	delete(s.reverse.conns, n.ID)
	assertStatus("disconnected", false)
	// Neither the monitor nor sidebar needs to send reverse health requests.
	s.collectNodeHealth(context.Background())
}
