package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

func hubAttentionReviewTransport(slowPage time.Duration) hubJSONDeadlineTransport {
	return func(r *http.Request) (*http.Response, error) {
		delay := slowPage
		if r.URL.Host == "late.invalid" {
			delay = 700 * time.Millisecond
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		body := fmt.Sprintf(`{"protocol_version":2,"store_instance_id":%q,"snapshot_version":1,"items":[]}`, r.URL.Host)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func TestHubAttentionLateRoundDeadlineIsNotNodeFailure(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		name := "initially healthy"
		if recovered {
			name = "previously failed now healthy"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				nodes := make([]hub.Node, 0, 9)
				for i := 0; i < 8; i++ {
					nodes = append(nodes, hub.Node{ID: fmt.Sprintf("healthy%d", i), URL: fmt.Sprintf("http://healthy%d.invalid", i)})
				}
				late := hub.Node{ID: "late", URL: "http://late.invalid"}
				nodes = append(nodes, late)
				s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: nodes}), nil)
				s.attentionStore = openProjection(t)
				transport := hubAttentionReviewTransport(2300 * time.Millisecond)
				s.prober.Client.Transport = transport
				previousFailures := 0
				if recovered {
					s.attentionSchedule.record(late.ID, errors.New("past outage"), false)
					previousFailures = 1
					time.Sleep(3 * time.Minute) // fake time: past every retry eligibility delay
				}
				// Eight peers take 6.9s each; the ninth needs 2.1s in isolation but receives
				// only 1.1s when admitted late in the shared eight-second round.
				failures := s.collectAttention(t.Context())
				_, syncs, err := s.attentionStore.List(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				// Independent negative control exercises the identical three-page transport
				// with a full budget, without refreshing the main scheduler/projection.
				control := newHubServer(nil, nil)
				control.attentionStore = openProjection(t)
				control.prober.Client.Transport = transport
				ctx, cancel := context.WithTimeout(t.Context(), hubAttentionRoundBudget)
				defer cancel()
				if err := control.collectNodeAttention(ctx, late); err != nil {
					t.Fatalf("standalone healthy control: %v", err)
				}
				if failures != 0 || s.attentionDiagnostics.CollectorFailures.Load() != 0 {
					t.Fatalf("shared-round truncation attributed to healthy node: failures=%d", failures)
				}
				for _, state := range syncs {
					if state.NodeID == late.ID && !state.LastErrorAt.IsZero() {
						t.Fatalf("late round persisted a false node error: %+v", state)
					}
				}
				s.attentionSchedule.mu.Lock()
				retry := s.attentionSchedule.retries[late.ID]
				s.attentionSchedule.mu.Unlock()
				if retry.failures != previousFailures {
					t.Fatalf("late round advanced node failure streak: got %d want %d", retry.failures, previousFailures)
				}
				for round := 0; round < 3; round++ {
					time.Sleep(3 * time.Minute)
					s.collectAttention(t.Context())
				}
				state, err := s.attentionStore.GetSync(t.Context(), late.ID)
				if err != nil {
					t.Fatal(err)
				}
				if state.LastSuccessAt.IsZero() {
					t.Fatalf("deferred/recovered healthy node never received early admission: %+v", state)
				}
			})
		})
	}
}

func TestHubAttentionFullRoundTimeoutRemainsNodeFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := hub.Node{ID: "slow", URL: "http://slow.invalid"}
		s := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{node}}), nil)
		s.attentionStore = openProjection(t)
		// Every page fits the real three-second HTTP client timeout, but the whole
		// sync exceeds eight seconds. A full-budget timeout must retain its failure.
		s.prober.Client.Transport = hubAttentionReviewTransport(2900 * time.Millisecond)
		if failures := s.collectAttention(t.Context()); failures != 1 {
			t.Fatalf("full round timeout failures=%d want 1", failures)
		}
		state, err := s.attentionStore.GetSync(t.Context(), node.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state.LastErrorAt.IsZero() || state.LastError != "collection timed out" {
			t.Fatalf("full-budget timeout not persisted: %+v", state)
		}
		s.attentionSchedule.mu.Lock()
		retry := s.attentionSchedule.retries[node.ID]
		s.attentionSchedule.mu.Unlock()
		if retry.failures != 1 || !retry.next.After(time.Now()) {
			t.Fatalf("full-budget timeout did not back off: %+v", retry)
		}
	})
}
