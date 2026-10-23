package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/hub"
)

func TestHubDelegationListContainsSilentReverseNode(t *testing.T) {
	s, ts, bad, peer, connection := containmentHub(t)
	s.delegations = hub.NewDelegationStore(filepath.Join(t.TempDir(), "delegations.json"))
	// This peer continues reading (and thus answers WebSocket pings), but never
	// answers a jobs API request. It is live, not disconnected or write-blocked.
	requests := make(chan struct{}, 32)
	go func() {
		for {
			var frame hubReverseRequest
			if peer.ReadJSON(&frame) != nil {
				return
			}
			if frame.Type == hubReverseFrameRequest {
				requests <- struct{}{}
			}
		}
	}()
	backend := newFakeTargetJobs(t)
	backend.setRun("succeeded", "finished", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHubReverseConnector(ctx, ts.URL, "healthy", "good", backend.server.URL, "/chat", backend.server.Client())
	waitForReverseNode(t, s, "healthy")
	// More silent records than workers prove healthy refresh is not stuck behind
	// a FIFO backlog of many requests to the same unresponsive target.
	for i := 0; i < 24; i++ {
		if err := s.delegations.Add(hub.Delegation{ID: fmt.Sprintf("bad-%02d", i), OriginNode: "origin", TargetNode: bad.ID, JobID: "job", RunID: "run", Status: "running", CreatedAt: time.Now().Add(-time.Hour + time.Duration(i)*time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.delegations.Add(hub.Delegation{ID: "healthy-record", OriginNode: "origin", TargetNode: "healthy", JobID: "job", RunID: "run", Status: "running", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	reqCtx, stop := context.WithCancel(context.Background())
	defer stop()
	req := httptest.NewRequest(http.MethodGet, "/api/delegations", nil).WithContext(reqCtx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.handler().ServeHTTP(rec, req); close(done) }()
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("silent reverse node did not receive jobs request")
	}
	// Prove heartbeat liveness, not just a connected registry flag: the peer
	// must answer an actual ping while it withholds the jobs response.
	beforePong := connection.lastSeenValue()
	if err := connection.conn.WriteControl(websocket.PingMessage, []byte("containment"), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	pongWatchdog := time.After(2 * time.Second)
	for !connection.lastSeenValue().After(beforePong) {
		select {
		case <-pongWatchdog:
			t.Fatal("silent jobs peer did not answer heartbeat ping")
		default:
			runtime.Gosched()
		}
	}
	t.Log("silent jobs peer answered heartbeat ping without answering jobs request")
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("delegation dashboard read exceeded refresh budget with a heartbeat-live silent reverse node")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var result struct {
		Delegations []hub.Delegation `json:"delegations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, d := range result.Delegations {
		if d.ID == "healthy-record" {
			if d.Status != "succeeded" {
				t.Fatalf("healthy delegation not refreshed: %+v", d)
			}
			return
		}
	}
	t.Fatal("healthy delegation omitted")
}
