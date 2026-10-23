package serve

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

func TestHubAttentionFailureBackoffIsNodeLocal(t *testing.T) {
	var badCalls, healthyCalls atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badCalls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyCalls.Add(1)
		writeAttentionBudgetPage(w, 0, 1, false, "")
	}))
	defer healthy.Close()
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{{ID: "bad", URL: bad.URL}, {ID: "healthy", URL: healthy.URL}}}), nil)
	s.attentionStore = openProjection(t)
	if failures := s.collectAttention(context.Background()); failures != 1 {
		t.Fatalf("initial failures=%d", failures)
	}
	// A second scheduler tick is allowed to collect healthy nodes immediately;
	// failed-node eligibility, not a global failure streak, holds back retries.
	s.collectAttention(context.Background())
	if got := badCalls.Load(); got != 1 {
		t.Fatalf("failed node ignored its own backoff: requests=%d, want 1", got)
	}
	if got := healthyCalls.Load(); got != 6 {
		t.Fatalf("healthy node cadence slowed by another node: page requests=%d, want 6", got)
	}
}

func TestHubAttentionHealthyCadenceWithFakeClock(t *testing.T) {
	var healthyCalls atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "offline", http.StatusServiceUnavailable) }))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyCalls.Add(1)
		writeAttentionBudgetPage(w, 0, 1, false, "")
	}))
	defer good.Close()
	s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{{ID: "bad", URL: bad.URL}, {ID: "healthy", URL: good.URL}}}), nil)
	s.attentionStore = openProjection(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.attentionSchedule.now = func() time.Time { return now }
	for tick := 0; tick < 10; tick++ {
		s.collectAttention(context.Background())
		if got := healthyCalls.Load(); got != int64(3*(tick+1)) {
			t.Fatalf("tick=%d healthy page requests=%d", tick, got)
		}
		now = now.Add(45 * time.Second)
	}
	if got := s.attentionDiagnostics.CollectorFailures.Load(); got >= 10 || got == 0 {
		t.Fatalf("failed peer retries=%d, want fewer than healthy base ticks", got)
	}
}

func TestHubAttentionRoundRotatesUnattemptedNodes(t *testing.T) {
	var schedule hubAttentionSchedule
	nodes := make([]hub.Node, 32)
	for i := range nodes {
		nodes[i].ID = fmt.Sprintf("node-%d", i)
	}
	seen := make(map[string]bool)
	for round := 0; round < 4; round++ {
		due := schedule.eligible(nodes)
		for _, node := range due[:8] {
			seen[node.ID] = true
		}
	}
	if len(seen) != 32 {
		t.Fatalf("round scheduling starved nodes: attempted=%d/32", len(seen))
	}
}
