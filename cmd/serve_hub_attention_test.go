package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
)

func TestHubAttentionCollectorInstallsBothKindsAtomically(t *testing.T) {
	const unseenETag = `"unseen-v1"`
	runningConditional := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ui/v1/attention" {
			http.NotFound(w, r)
			return
		}
		kind := r.URL.Query().Get("kind")
		if kind == "unseen" && r.Header.Get("If-None-Match") == unseenETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if kind == "running" && r.Header.Get("If-None-Match") != "" {
			runningConditional = true
		}
		page := hubAttentionPage{ProtocolVersion: 1, StoreInstanceID: "store-a", SnapshotVersion: 10}
		switch kind {
		case "unseen":
			w.Header().Set("ETag", unseenETag)
			page.Items = []hubAttentionPageItem{{SessionID: "done", SessionNumber: 41, ResponseID: "resp-done", Kind: "unseen", LifecycleState: "completed", Outcome: "completed", AttentionSeq: 9, FinalRev: 3, TerminalAt: time.Now().UTC()}}
		case "running":
			w.Header().Set("ETag", `"running-v1"`)
			page.Items = []hubAttentionPageItem{{SessionID: "live", SessionNumber: 42, ResponseID: "resp-live", Kind: "running", LifecycleState: "running", StartedAt: time.Now().UTC()}}
		default:
			http.Error(w, "bad kind", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()

	node := hub.Node{ID: "alpha", Name: "Alpha", URL: server.URL + "/ui"}
	if err := node.Normalize(); err != nil {
		t.Fatal(err)
	}
	projection, err := hub.OpenAttentionProjectionStore(filepath.Join(t.TempDir(), "attention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer projection.Close()
	srv := newHubServer(nil, nil)
	srv.attentionStore = projection
	if err := srv.collectNodeAttention(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	if runningConditional {
		t.Fatal("collector reused the unseen validator for the running representation")
	}
	activities, syncs, err := projection.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 2 || len(syncs) != 1 || syncs[0].ETag != unseenETag || activities[0].SessionNumber <= 0 || activities[1].SessionNumber <= 0 {
		t.Fatalf("projection = %+v, sync=%+v", activities, syncs)
	}
	// A global snapshot that has not changed may short-circuit on the first
	// (unseen) representation without clearing the prior complete projection.
	if err := srv.collectNodeAttention(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	activities, _, _ = projection.List(context.Background())
	if len(activities) != 2 {
		t.Fatalf("304 cleared cached projection: %+v", activities)
	}
}

func TestHubAttentionCollectorIncludesProtocolV2InputRequired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := hubAttentionPage{ProtocolVersion: 2, StoreInstanceID: "store-v2", SnapshotVersion: 12}
		switch r.URL.Query().Get("kind") {
		case "unseen", "running":
		case "input_required":
			page.Items = []hubAttentionPageItem{{
				SessionID: "blocked", ResponseID: "resp-blocked", Kind: "input_required", LifecycleState: "running",
				InteractionRequired: true, InteractionStateRev: 8, PendingInteractionCount: 2,
				PendingInteractionKinds: []string{"approval.shell", "ask_user"}, InteractionRequiredSince: time.Now().UTC(),
			}}
		default:
			http.Error(w, "bad kind", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()
	node := hub.Node{ID: "alpha", Name: "Alpha", URL: server.URL + "/chat"}
	if err := node.Normalize(); err != nil {
		t.Fatal(err)
	}
	projection, err := hub.OpenAttentionProjectionStore(filepath.Join(t.TempDir(), "attention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer projection.Close()
	srv := newHubServer(nil, nil)
	srv.attentionStore = projection
	if err := srv.collectNodeAttention(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	activities, _, err := projection.List(context.Background())
	if err != nil || len(activities) != 1 {
		t.Fatalf("activities = %+v, %v", activities, err)
	}
	activity := activities[0]
	if activity.Kind != "input_required" || activity.PendingInteractionCount != 2 || len(activity.PendingInteractionKinds) != 2 {
		t.Fatalf("input-required activity = %+v", activity)
	}
}

func TestHubAttentionCollectorRetriesSnapshotDriftWithoutInstallingPartialState(t *testing.T) {
	unseenRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("kind")
		if kind == "input_required" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		version := int64(10)
		if kind == "unseen" {
			unseenRequests++
			if unseenRequests > 1 {
				version = 11
			}
		} else if r.URL.Query().Get("snapshot_version") == "10" {
			w.WriteHeader(http.StatusConflict)
			return
		} else {
			version = 11
		}
		page := hubAttentionPage{ProtocolVersion: 1, StoreInstanceID: "store-a", SnapshotVersion: version}
		if kind == "unseen" {
			page.Items = []hubAttentionPageItem{{SessionID: "done", AttentionSeq: version, LifecycleState: "completed"}}
		} else {
			page.Items = []hubAttentionPageItem{{SessionID: "live", LifecycleState: "running"}}
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()

	node := hub.Node{ID: "alpha", Name: "Alpha", URL: server.URL + "/ui"}
	if err := node.Normalize(); err != nil {
		t.Fatal(err)
	}
	projection, err := hub.OpenAttentionProjectionStore(filepath.Join(t.TempDir(), "attention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer projection.Close()
	srv := newHubServer(nil, nil)
	srv.attentionStore = projection
	if err := srv.collectNodeAttention(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	activities, _, err := projection.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if unseenRequests != 2 || len(activities) != 2 {
		t.Fatalf("requests=%d activities=%+v", unseenRequests, activities)
	}
	for _, activity := range activities {
		if activity.Kind == "terminal_unseen" && activity.AttentionSeq != 11 {
			t.Fatalf("installed partial first snapshot: %+v", activity)
		}
	}
}

func TestHubAttentionDedupPrefersHealthyDeterministicRoute(t *testing.T) {
	now := time.Now().UTC()
	activities := []hub.SessionActivity{
		{NodeID: "alpha", StoreInstanceID: "store-a", SessionID: "session", Kind: "terminal_unseen"},
		{NodeID: "beta", StoreInstanceID: "store-a", SessionID: "session", Kind: "terminal_unseen"},
	}
	syncs := []hub.AttentionSync{
		{NodeID: "alpha", Capability: hub.AttentionSupported, LastSuccessAt: now, LastErrorAt: now.Add(time.Second)},
		{NodeID: "beta", Capability: hub.AttentionSupported, LastSuccessAt: now},
	}
	selected := deduplicateHubActivities(activities, syncs)
	if len(selected) != 1 || selected[0].NodeID != "beta" {
		t.Fatalf("deduplicated activities = %+v", selected)
	}
	// Equal health/freshness falls back to lexical node ID, independent of input order.
	syncs[0].LastErrorAt = time.Time{}
	selected = deduplicateHubActivities([]hub.SessionActivity{activities[1], activities[0]}, syncs)
	if len(selected) != 1 || selected[0].NodeID != "alpha" {
		t.Fatalf("deterministic activities = %+v", selected)
	}
}

func TestHubAttentionAPIKeepsExactCountsWhenInboxIsBounded(t *testing.T) {
	projection, err := hub.OpenAttentionProjectionStore(filepath.Join(t.TempDir(), "attention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer projection.Close()
	now := time.Now().UTC()
	activities := []hub.SessionActivity{
		{SessionID: "one", SessionNumber: 21, Kind: "terminal_unseen", AttentionSeq: 1, TerminalAt: now.Add(-time.Minute)},
		{SessionID: "two", SessionNumber: 22, Kind: "terminal_unseen", AttentionSeq: 2, TerminalAt: now},
		{SessionID: "live", SessionNumber: 23, Kind: "running", StartedAt: now},
		{SessionID: "blocked", SessionNumber: 24, Kind: "input_required", PendingInteractionCount: 1,
			PendingInteractionKinds: []string{"approval.workspace"}, InteractionRequiredSince: now},
	}
	if err := projection.ReplaceNode(context.Background(), "alpha", "store-a", "etag", activities); err != nil {
		t.Fatal(err)
	}
	srv := newHubServer(nil, nil)
	srv.attentionStore = projection
	recorder := httptest.NewRecorder()
	srv.handleHubAttention(recorder, httptest.NewRequest(http.MethodGet, "/api/attention?limit=1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		TotalRunning       int                     `json:"total_running"`
		TotalInputRequired int                     `json:"total_input_required"`
		TotalUnseen        int                     `json:"total_unseen"`
		HasMore            bool                    `json:"has_more"`
		InputRequired      []hubInputRequiredItem  `json:"input_required"`
		Inbox              []hubAttentionInboxItem `json:"inbox"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.TotalRunning != 1 || response.TotalInputRequired != 1 || response.TotalUnseen != 2 || !response.HasMore ||
		len(response.InputRequired) != 1 || response.InputRequired[0].SessionID != "blocked" ||
		response.InputRequired[0].ResumePath != "/node/alpha/24" || len(response.Inbox) != 1 ||
		response.Inbox[0].SessionID != "two" || response.Inbox[0].ResumePath != "/node/alpha/22" {
		t.Fatalf("bounded response = %+v", response)
	}
}

func TestHubAttentionErrorSummaryDoesNotDiscloseBackendURL(t *testing.T) {
	backend := "https://secret.internal.example:8443/ui/v1/attention"
	summary := hubAttentionErrorSummary(&url.Error{Op: "Get", URL: backend, Err: errors.New("dial failed")})
	if summary != "node unreachable" {
		t.Fatalf("summary = %q", summary)
	}
}
