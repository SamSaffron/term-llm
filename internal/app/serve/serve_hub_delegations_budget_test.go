package serve

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

type hubJSONDeadlineTransport func(*http.Request) (*http.Response, error)

func (f hubJSONDeadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestHubNodeJSONWholeResponseDeadline(t *testing.T) {
	for _, shorter := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "shorter caller"}[shorter], func(t *testing.T) {
			s := newHubServer(nil, nil)
			ctx := t.Context()
			if shorter {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			}
			s.nodeAPIClient.Transport = hubJSONDeadlineTransport(func(req *http.Request) (*http.Response, error) {
				deadline, ok := req.Context().Deadline()
				if !ok {
					t.Fatal("JSON request has no whole-response deadline")
				}
				remaining := time.Until(deadline)
				max := 30 * time.Second
				if shorter {
					max = time.Second
				}
				if remaining <= 0 || remaining > max {
					t.Fatalf("request deadline remaining=%v, want <=%v", remaining, max)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
			})
			if err := s.doNodeJSON(ctx, hub.Node{ID: "direct", URL: "http://node.invalid"}, http.MethodGet, "/jobs", nil, &map[string]any{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHubNodeJSONReverseBodyReadDeadline(t *testing.T) {
	original := hubNodeJSONTimeout
	hubNodeJSONTimeout = 50 * time.Millisecond
	defer func() { hubNodeJSONTimeout = original }()
	s, _, node, peer, _ := containmentHub(t)
	go func() {
		var frame hubReverseRequest
		if peer.ReadJSON(&frame) == nil {
			_ = peer.WriteJSON(hubReverseResponse{Type: hubReverseFrameResponseStart, ID: frame.ID, Status: 200})
		}
	}()
	done := make(chan error, 1)
	go func() {
		var result map[string]any
		done <- s.doNodeJSON(context.Background(), node, http.MethodGet, "/silent-body", nil, &result)
	}()
	if err := containmentAwait(t, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled reverse JSON body error=%v", err)
	}
	if !s.reverse.isConnected(node.ID) {
		t.Fatal("a read timeout unnecessarily disconnected the multiplexed tunnel")
	}
}

func TestFairDashboardDelegationTargets(t *testing.T) {
	active := []hub.Delegation{{ID: "a1", TargetNode: "a"}, {ID: "a2", TargetNode: "a"}, {ID: "b1", TargetNode: "b"}, {ID: "c1", TargetNode: "c"}}
	for offset, first := range []string{"a", "b", "c"} {
		fair := fairDashboardDelegations(active, uint64(offset))
		if fair[0].TargetNode != first {
			t.Fatalf("offset=%d first=%s", offset, fair[0].TargetNode)
		}
		seen := make(map[string]bool)
		for _, d := range fair[:3] {
			seen[d.TargetNode] = true
		}
		if len(seen) != 3 {
			t.Fatalf("one target monopolized initial workers: %+v", fair)
		}
	}
}

func TestHubDelegationDashboardPreservesRefreshErrorStatus(t *testing.T) {
	s, _ := newDelegationHub(t)
	d := hub.Delegation{ID: "orphan", TargetNode: "unregistered", JobID: "job", Status: "running", UpdatedAt: time.Now().Add(-48 * time.Hour)}
	if err := s.delegations.Add(d); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/delegations", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	stored, _, err := s.delegations.Get(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "running" {
		t.Fatalf("dashboard read terminalized a refresh error: %+v", stored)
	}
}
