package peer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type diagnosticTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *diagnosticTestClock) Next() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(10 * time.Millisecond)
	return c.now
}

type diagnosticEventCollector struct {
	mu     sync.Mutex
	events []DiagnosticEvent
	notify chan struct{}
}

func newDiagnosticEventCollector() *diagnosticEventCollector {
	return &diagnosticEventCollector{notify: make(chan struct{}, 32)}
}

func (c *diagnosticEventCollector) Observe(event DiagnosticEvent) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *diagnosticEventCollector) snapshot() []DiagnosticEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]DiagnosticEvent(nil), c.events...)
}

func TestDiagnosticMountedRoutesRemainFiniteAndUseful(t *testing.T) {
	for _, test := range []struct {
		target string
		want   DiagnosticRoute
	}{
		{"/chat/v1/events?token=secret", DiagnosticRouteEvents},
		{"/deep/mount/v1/sessions/private-id/messages?query=secret", DiagnosticRouteSession},
		{"/ui/healthz?key=secret", DiagnosticRouteHealth},
		{"/chat/private/secret?token=secret", DiagnosticRouteOther},
	} {
		if got := diagnosticRoute(test.target); got != test.want {
			t.Errorf("diagnosticRoute(%q) = %q, want %q", test.target, got, test.want)
		}
	}
}

func TestDiagnosticAdmissionFailureHasNoOrphanLifecycle(t *testing.T) {
	clock := &diagnosticTestClock{now: time.Unix(300, 0)}
	collector := newDiagnosticEventCollector()
	var admitted atomic.Bool
	p := newPair(t, pairConfig{
		handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }),
		gate: func(context.Context) error {
			if !admitted.Load() {
				return errors.New("secret admission error")
			}
			return nil
		},
		observer: collector.Observe, now: clock.Next,
	})
	response, err := p.httpClient().Get("http://peer/chat/v1/models?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	response.Body.Close()
	for _, event := range collector.snapshot() {
		if event.Stage == DiagnosticRequestStart || event.Stage == DiagnosticResponseHeaders ||
			event.Stage == DiagnosticRequestEnd || event.Stage == DiagnosticRequestCancel || event.Stage == DiagnosticRequestFailure {
			t.Fatalf("rejected admission emitted orphan request lifecycle: %+v", event)
		}
	}

	admitted.Store(true)
	response, err = p.httpClient().Get("http://peer/chat/v1/models?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	response.Body.Close()
	events := waitDiagnosticStages(t, collector, DiagnosticRequestStart, DiagnosticResponseHeaders, DiagnosticRequestEnd)
	start := eventForStage(events, DiagnosticRequestStart)
	if start.Route != DiagnosticRouteModels || countDiagnosticStage(events, DiagnosticRequestEnd) != 1 {
		t.Fatalf("admitted lifecycle = %+v", events)
	}
}

func TestDiagnosticRequestLifecycleUsesFiniteRouteAndActualCounters(t *testing.T) {
	clock := &diagnosticTestClock{now: time.Unix(100, 0)}
	collector := newDiagnosticEventCollector()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "hello")
	})
	p := newPair(t, pairConfig{handler: handler, observer: collector.Observe, now: clock.Next})
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://peer/v1/sessions/private-id?token=forbidden", nil)
	response, err := p.httpClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "hello" {
		t.Fatalf("response = %q, %v", body, err)
	}

	events := waitDiagnosticStages(t, collector, DiagnosticChannelOpen, DiagnosticRequestStart, DiagnosticResponseHeaders, DiagnosticRequestEnd)
	start, headers, end := eventForStage(events, DiagnosticRequestStart), eventForStage(events, DiagnosticResponseHeaders), eventForStage(events, DiagnosticRequestEnd)
	if start.Method != DiagnosticMethodGET || start.Route != DiagnosticRouteSession || start.Request == 0 {
		t.Fatalf("request start = %+v", start)
	}
	if headers.Request != start.Request || headers.Status != http.StatusCreated {
		t.Fatalf("response headers = %+v, start=%+v", headers, start)
	}
	if end.Request != start.Request || end.Status != http.StatusCreated || end.Bytes != 5 || end.Duration != 10*time.Millisecond || end.Outcome != DiagnosticOutcomeFinished {
		t.Fatalf("request end = %+v", end)
	}
	if countDiagnosticStage(events, DiagnosticRequestEnd)+countDiagnosticStage(events, DiagnosticRequestCancel)+countDiagnosticStage(events, DiagnosticRequestFailure) != 1 {
		t.Fatalf("request emitted duplicate terminal events: %+v", events)
	}
}

func TestDiagnosticCancellationEmitsOneTerminalEvent(t *testing.T) {
	clock := &diagnosticTestClock{now: time.Unix(200, 0)}
	collector := newDiagnosticEventCollector()
	started := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	p := newPair(t, pairConfig{handler: handler, observer: collector.Observe, now: clock.Next})
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://peer/v1/events?secret=query", nil)
	response, err := p.httpClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	waitFailureSignal(t, started, "diagnostic request start")
	cancel()
	_, _ = io.ReadAll(response.Body)
	response.Body.Close()

	events := waitDiagnosticStages(t, collector, DiagnosticRequestCancel)
	terminal := eventForStage(events, DiagnosticRequestCancel)
	if terminal.Route != DiagnosticRouteEvents || terminal.Outcome != DiagnosticOutcomeCanceled || terminal.Reason != DiagnosticReasonPeerCanceled {
		t.Fatalf("cancel event = %+v", terminal)
	}
	if countDiagnosticStage(events, DiagnosticRequestEnd)+countDiagnosticStage(events, DiagnosticRequestCancel)+countDiagnosticStage(events, DiagnosticRequestFailure) != 1 {
		t.Fatalf("request emitted duplicate terminal events: %+v", events)
	}
}

func waitDiagnosticStages(t *testing.T, collector *diagnosticEventCollector, stages ...DiagnosticStage) []DiagnosticEvent {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		events := collector.snapshot()
		ready := true
		for _, stage := range stages {
			if countDiagnosticStage(events, stage) == 0 {
				ready = false
				break
			}
		}
		if ready {
			return events
		}
		select {
		case <-collector.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for diagnostic stages %v; events=%+v", stages, events)
		}
	}
}

func eventForStage(events []DiagnosticEvent, stage DiagnosticStage) DiagnosticEvent {
	for _, event := range events {
		if event.Stage == stage {
			return event
		}
	}
	return DiagnosticEvent{}
}

func countDiagnosticStage(events []DiagnosticEvent, stage DiagnosticStage) int {
	count := 0
	for _, event := range events {
		if event.Stage == stage {
			count++
		}
	}
	return count
}
