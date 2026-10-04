package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/session"
)

func openProjection(t *testing.T) *hub.AttentionProjectionStore {
	t.Helper()
	p, err := hub.OpenAttentionProjectionStore(filepath.Join(t.TempDir(), "attention.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}
func seedRows(t *testing.T, s *hub.AttentionProjectionStore, node, store string, a ...hub.SessionActivity) {
	t.Helper()
	if err := s.ReplaceNode(context.Background(), node, store, "", a); err != nil {
		t.Fatal(err)
	}
}
func requireKeys(t *testing.T, s *hub.AttentionProjectionStore, want ...string) {
	t.Helper()
	a, _, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(a))
	for _, v := range a {
		got = append(got, v.NodeID+"/"+v.SessionID+"/"+v.Kind+fmt.Sprintf("@%d", v.AttentionSeq))
	}
	sort.Strings(got)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}
func doClear(h http.Handler, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}
func requireClear(t *testing.T, r *httptest.ResponseRecorder, cleared, failed int64) {
	t.Helper()
	if r.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body=%s", r.Code, r.Body.String())
	}
	var v struct {
		Cleared int64 `json:"cleared"`
		Failed  int64 `json:"failed"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", r.Body.String(), err)
	}
	if v.Cleared != cleared || v.Failed != failed {
		t.Fatalf("clear = %d/%d, want %d/%d", v.Cleared, v.Failed, cleared, failed)
	}
}
func seenSession(path string) string {
	p := strings.TrimSuffix(path, "/attention/seen")
	return p[strings.LastIndex(p, "/")+1:]
}
func strictSeen(store string, seq map[string]int64, calls *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var q markAttentionSeenRequest
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			http.Error(w, "bad", 400)
			return
		}
		sid := seenSession(r.URL.Path)
		if q.StoreInstanceID != store || seq[sid] != q.ThroughSeq {
			writeOpenAIError(w, 409, "conflict_error", "changed")
			return
		}
		calls.Add(1)
		writeJSON(w, 200, session.AttentionState{StoreInstanceID: q.StoreInstanceID, SessionID: sid, SeenThroughSeq: q.ThroughSeq})
	}
}
func TestHubAttentionClearBulkRetryRetain(t *testing.T) {
	const n = 260
	seq := make(map[string]int64, n)
	acts := make([]hub.SessionActivity, 0, n+2)
	for i := range n {
		id := fmt.Sprintf("sess_%03d", i)
		seq[id] = int64(i + 1)
		acts = append(acts, hub.SessionActivity{SessionID: id, Kind: "terminal_unseen", AttentionSeq: int64(i + 1), TerminalAt: time.UnixMilli(1_800_000_000_000 + int64(i))})
	}
	acts = append(acts, hub.SessionActivity{SessionID: "live", Kind: "running", StartedAt: time.UnixMilli(1_799_000_000_000)},
		hub.SessionActivity{SessionID: "live", Kind: "input_required", PendingInteractionCount: 1, InteractionRequiredSince: time.UnixMilli(1_799_000_000_001)})
	var flaky atomic.Bool
	flaky.Store(true)
	var calls atomic.Int64
	srv := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		if seenSession(r.URL.Path) == "sess_005" && flaky.Load() {
			writeOpenAIError(w, 500, "server_error", "down")
			return
		}
		strictSeen("store-a", seq, &calls)(w, r)
	})
	store := openProjection(t)
	srv.attentionStore = store
	seedRows(t, store, "alpha", "store-a", acts...)
	seedRows(t, store, "ghost", "store-ghost", hub.SessionActivity{SessionID: "sess_ghost", Kind: "terminal_unseen", AttentionSeq: 3, TerminalAt: time.UnixMilli(3)})
	h := srv.handler()
	requireClear(t, doClear(h, "/api/attention/clear"), n-1, 2)
	requireKeys(t, store, "alpha/live/input_required@0", "alpha/live/running@0", "alpha/sess_005/terminal_unseen@6", "ghost/sess_ghost/terminal_unseen@3")
	flaky.Store(false)
	requireClear(t, doClear(h, "/api/attention/clear"), 1, 1)
	requireKeys(t, store, "alpha/live/input_required@0", "alpha/live/running@0", "ghost/sess_ghost/terminal_unseen@3")
	if calls.Load() != n {
		t.Fatalf("acks = %d, want %d", calls.Load(), n)
	}
}
func TestHubAttentionClearValidatesNodeAck(t *testing.T) {
	bad := func(sid, store string, seq int64) session.AttentionState {
		return session.AttentionState{StoreInstanceID: store, SessionID: sid, SeenThroughSeq: seq}
	}
	cases := []struct {
		name string
		ack  func(http.ResponseWriter, markAttentionSeenRequest)
		ok   bool
	}{
		{"exact success", nil, true},
		{"http error", func(w http.ResponseWriter, _ markAttentionSeenRequest) {
			writeOpenAIError(w, 500, "server_error", "failed")
		}, false},
		{"malformed", func(w http.ResponseWriter, _ markAttentionSeenRequest) {
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"seen_through_seq":`)
		}, false},
		{"wrong store", func(w http.ResponseWriter, q markAttentionSeenRequest) {
			writeJSON(w, 200, bad("sess_ack", "other", q.ThroughSeq))
		}, false},
		{"wrong session", func(w http.ResponseWriter, q markAttentionSeenRequest) {
			writeJSON(w, 200, bad("other", q.StoreInstanceID, q.ThroughSeq))
		}, false},
		{"seq below", func(w http.ResponseWriter, q markAttentionSeenRequest) {
			writeJSON(w, 200, bad("sess_ack", q.StoreInstanceID, q.ThroughSeq-1))
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var path, auth, ctype, body string
			srv := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
				var q markAttentionSeenRequest
				_ = json.Unmarshal(data, &q)
				if c.ack == nil {
					path, auth, ctype, body = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(data)
					writeJSON(w, 200, bad("sess_ack", q.StoreInstanceID, q.ThroughSeq))
					return
				}
				c.ack(w, q)
			})
			store := openProjection(t)
			srv.attentionStore = store
			seedRows(t, store, "alpha", "store-a", hub.SessionActivity{SessionID: "sess_ack", Kind: "terminal_unseen", AttentionSeq: 5, TerminalAt: time.UnixMilli(1)})
			var wantC, wantF int64 = 1, 0
			wantKeys := []string{}
			if !c.ok {
				wantC, wantF, wantKeys = 0, 1, []string{"alpha/sess_ack/terminal_unseen@5"}
			}
			requireClear(t, doClear(srv.handler(), "/api/attention/clear"), wantC, wantF)
			requireKeys(t, store, wantKeys...)
			if c.ok && (path != "/chat/v1/sessions/sess_ack/attention/seen" || auth != "Bearer tkn-123" || ctype != "application/json" || body != `{"store_instance_id":"store-a","through_seq":5}`) {
				t.Fatalf("seen = %q %q %q %q", path, auth, ctype, body)
			}
		})
	}
}
func TestHubAttentionClearHighestCachedSeqOneClick(t *testing.T) {
	nodeServe, sessions, sid, first := attentionHandlerFixture(t)
	ctx := context.Background()
	mkRun := func(resp string, epoch, rev int64) session.AttentionState {
		l, err := sessions.AdmitResponseRun(ctx, session.ResponseRunAdmission{ResponseID: resp, SessionID: sid, RunEpoch: epoch, OwnerInstanceID: "owner", StartedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		s, err := sessions.FinalizeResponseRun(ctx, session.ResponseRunTerminal{ResponseID: resp, OwnerInstanceID: "owner", FencingToken: l.FencingToken, Outcome: session.ResponseRunCompleted, FinalRev: rev})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	newer, newest := mkRun("resp_newer", 2, 4), mkRun("resp_newest", 3, 5)
	var ac, bc atomic.Int64
	dlg := func(c *atomic.Int64) *httptest.Server {
		b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.Add(1)
			nodeServe.handleSessionAttentionSeen(w, r, seenSession(r.URL.Path))
		}))
		t.Cleanup(b.Close)
		return b
	}
	ab, bb := dlg(&ac), dlg(&bc)
	srv := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{{ID: "alpha", Name: "Alpha", Source: hub.SourceConfig, URL: ab.URL, BasePath: "/chat", Token: "tkn-123"}, {ID: "beta", Name: "Beta", Source: hub.SourceConfig, URL: bb.URL, BasePath: "/chat", Token: "tkn-123"}}}), nil)
	store := openProjection(t)
	srv.attentionStore = store
	seedRows(t, store, "alpha", first.StoreInstanceID, hub.SessionActivity{SessionID: sid, Kind: "terminal_unseen", AttentionSeq: first.LatestAttentionSeq, TerminalAt: time.UnixMilli(1)})
	seedRows(t, store, "beta", first.StoreInstanceID, hub.SessionActivity{SessionID: sid, Kind: "terminal_unseen", AttentionSeq: newer.LatestAttentionSeq, TerminalAt: time.UnixMilli(4)})
	if err := store.MarkUnavailable(ctx, "beta", false); err != nil {
		t.Fatal(err)
	}
	// A removed registration must not hide the available route to this store.
	seedRows(t, store, "ghost", first.StoreInstanceID, hub.SessionActivity{SessionID: sid, Kind: "terminal_unseen", AttentionSeq: newer.LatestAttentionSeq})
	requireClear(t, doClear(srv.handler(), "/api/attention/clear"), 1, 0)
	if ac.Load() != 1 || bc.Load() != 0 {
		t.Fatalf("route alpha:%d beta:%d, want 1/0", ac.Load(), bc.Load())
	}
	requireKeys(t, store)
	st, err := sessions.GetAttention(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if st.SeenThroughSeq != newer.LatestAttentionSeq || !st.Unseen || st.LatestAttentionSeq != newest.LatestAttentionSeq {
		t.Fatalf("durable = %+v, want seen %d newest %d", st, newer.LatestAttentionSeq, newest.LatestAttentionSeq)
	}
	if _, err := sessions.Get(ctx, sid); err != nil {
		t.Fatalf("conversation was removed: %v", err)
	}
}

type staleGate struct {
	phase      atomic.Int64
	started    chan struct{}
	resume     chan struct{}
	startOnce  sync.Once
	resumeOnce sync.Once
}

func (g *staleGate) stale(r *http.Request) bool {
	if g.phase.Load() != 1 {
		return g.phase.Load() == 0
	}
	g.startOnce.Do(func() { close(g.started) })
	select {
	case <-g.resume:
	case <-r.Context().Done():
	}
	return true
}
func TestHubAttentionClearStaleFetchRetriesCurrent(t *testing.T) {
	g := &staleGate{started: make(chan struct{}), resume: make(chan struct{})}
	defer g.resumeOnce.Do(func() { close(g.resume) })
	var acks, lastSeq atomic.Int64
	srv := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			strictSeen("store-a", map[string]int64{"sess_old": 5}, &acks)(w, r)
			lastSeq.Store(5)
			return
		}
		if r.URL.Query().Get("kind") == "input_required" {
			http.Error(w, "unsupported", 400)
			return
		}
		page := hubAttentionPage{ProtocolVersion: 1, StoreInstanceID: "store-a", SnapshotVersion: 7}
		if r.URL.Query().Get("kind") == "unseen" {
			sess, resp, seq, at := "sess_old", "resp_old", int64(5), time.UnixMilli(1_800_000_000_000)
			if !g.stale(r) {
				sess, resp, seq, at = "sess_new", "resp_new", 9, time.UnixMilli(1_800_000_100_000)
			}
			page.Items = []hubAttentionPageItem{{SessionID: sess, ResponseID: resp, Kind: "unseen", LifecycleState: "completed", Outcome: "completed", AttentionSeq: seq, FinalRev: 3, TerminalAt: at}}
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	store := openProjection(t)
	srv.attentionStore = store
	ctx := context.Background()
	nodes, _ := srv.registry.Nodes()
	if err := srv.collectNodeAttention(ctx, nodes[0]); err != nil {
		t.Fatalf("prime: %v", err)
	}
	requireKeys(t, store, "alpha/sess_old/terminal_unseen@5")
	g.phase.Store(1)
	done := make(chan error, 1)
	go func() { done <- srv.collectNodeAttention(ctx, nodes[0]) }()
	<-g.started
	requireClear(t, doClear(srv.handler(), "/api/attention/clear"), 1, 0)
	requireKeys(t, store)
	g.resumeOnce.Do(func() {
		g.phase.Store(2)
		close(g.resume)
	})
	if err := <-done; err != nil {
		t.Fatalf("collect: %v", err)
	}
	requireKeys(t, store, "alpha/sess_new/terminal_unseen@9")
	if srv.attentionDiagnostics.SnapshotRetries.Load() != 1 {
		t.Fatalf("retries = %d, want 1", srv.attentionDiagnostics.SnapshotRetries.Load())
	}
	if acks.Load() != 1 || lastSeq.Load() != 5 {
		t.Fatalf("acks = %d seq = %d, want 1/5", acks.Load(), lastSeq.Load())
	}
}
func TestHubAttentionClearGuardsOriginMountAuth(t *testing.T) {
	var calls atomic.Int64
	srv := hubWithBackend(t, "/chat", strictSeen("store-a", map[string]int64{"sess_guard": 3}, &calls))
	store := openProjection(t)
	srv.attentionStore = store
	seedRows(t, store, "alpha", "store-a", hub.SessionActivity{SessionID: "sess_guard", Kind: "terminal_unseen", AttentionSeq: 3, TerminalAt: time.UnixMilli(1)})
	h := srv.handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/attention/clear", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", rec.Code)
	}
	for _, c := range []struct {
		name string
		mut  func(*http.Request)
	}{{"no JSON", func(r *http.Request) { r.Header.Del("Content-Type") }}, {"cross-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }}, {"same-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }}, {"cross-origin", func(r *http.Request) { r.Header.Set("Origin", "https://hub.example") }}} {
		t.Run(c.name, func(t *testing.T) {
			q := httptest.NewRequest(http.MethodPost, "/api/attention/clear", strings.NewReader("{}"))
			q.Header.Set("Content-Type", "application/json")
			c.mut(q)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, q)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d", rec.Code)
			}
			requireKeys(t, store, "alpha/sess_guard/terminal_unseen@3")
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("rejected reached node (%d)", calls.Load())
	}
	srv.requireAuth, srv.token, srv.basePath = true, "hub-secret", "/hub"
	h = srv.handler()
	if rec := doClear(h, "/hub/api/attention/clear"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth = %d", rec.Code)
	}
	if rec := doClear(h, "/api/attention/clear"); rec.Code != http.StatusNotFound {
		t.Fatalf("unmounted = %d", rec.Code)
	}
	q := httptest.NewRequest(http.MethodPost, "/hub/api/attention/clear", strings.NewReader("{}"))
	q.Header.Set("Content-Type", "application/json")
	q.Header.Set("Authorization", "Bearer hub-secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, q)
	requireClear(t, rec, 1, 0)
	requireKeys(t, store)
}
func TestHubAttentionClearNoStoreNoop(t *testing.T) {
	requireClear(t, doClear(newHubServer(nil, nil).handler(), "/api/attention/clear"), 0, 0)
}
func TestHubAttentionSupersededCollectNotFailure(t *testing.T) {
	var started, resume [2]chan struct{}
	var sOnce, rOnce [2]sync.Once
	for i := range 2 {
		started[i], resume[i] = make(chan struct{}), make(chan struct{})
	}
	var unseen, seenOK atomic.Int64
	srv := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			strictSeen("store-a", map[string]int64{"sess_old": 5, "sess_new": 9}, &seenOK)(w, r)
			return
		}
		if kind := r.URL.Query().Get("kind"); kind != "unseen" {
			if kind == "input_required" {
				http.Error(w, "unsupported", 400)
				return
			}
			_ = json.NewEncoder(w).Encode(hubAttentionPage{ProtocolVersion: 1, StoreInstanceID: "store-a", SnapshotVersion: 7})
			return
		}
		if n := int(unseen.Add(1)) - 1; n < 2 {
			sOnce[n].Do(func() { close(started[n]) })
			select {
			case <-resume[n]:
			case <-r.Context().Done():
			}
		}
		_ = json.NewEncoder(w).Encode(hubAttentionPage{ProtocolVersion: 1, StoreInstanceID: "store-a", SnapshotVersion: 7, Items: []hubAttentionPageItem{{SessionID: "sess_old", ResponseID: "resp_old", Kind: "unseen", LifecycleState: "completed", Outcome: "completed", AttentionSeq: 5, FinalRev: 3, TerminalAt: time.UnixMilli(1_800_000_000_000)}}})
	})
	store := openProjection(t)
	srv.attentionStore = store
	seedRows(t, store, "alpha", "store-a", hub.SessionActivity{SessionID: "sess_old", Kind: "terminal_unseen", AttentionSeq: 5, TerminalAt: time.UnixMilli(1_800_000_000_000)})
	release := func(n int) { rOnce[n].Do(func() { close(resume[n]) }) }
	defer release(0)
	defer release(1)
	done := make(chan int, 1)
	go func() { done <- srv.collectAttention(context.Background()) }()
	<-started[0]
	requireClear(t, doClear(srv.handler(), "/api/attention/clear"), 1, 0)
	seedRows(t, store, "alpha", "store-a", hub.SessionActivity{SessionID: "sess_new", Kind: "terminal_unseen", AttentionSeq: 9, TerminalAt: time.UnixMilli(1_800_000_100_000)})
	release(0)
	<-started[1]
	requireClear(t, doClear(srv.handler(), "/api/attention/clear"), 1, 0)
	release(1)
	if failures := <-done; failures != 0 {
		t.Fatalf("failures = %d, want 0", failures)
	}
	if got := srv.attentionDiagnostics.CollectorFailures.Load(); got != 0 {
		t.Fatalf("collector failures = %d, want 0", got)
	}
	st, err := store.GetSync(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if st.LastError != "" {
		t.Fatalf("node error = %q, want none", st.LastError)
	}
	if seenOK.Load() != 2 {
		t.Fatalf("seen = %d, want 2", seenOK.Load())
	}
}
func TestHubAttentionClearSkipsUnreachableNode(t *testing.T) {
	var badCalls, goodAcks atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badCalls.Add(1)
		if h, ok := w.(http.Hijacker); ok {
			if c, _, err := h.Hijack(); err == nil {
				_ = c.Close()
			}
		}
	}))
	t.Cleanup(bad.Close)
	good := httptest.NewServer(strictSeen("store-good", map[string]int64{"sess_good": 1}, &goodAcks))
	t.Cleanup(good.Close)
	srv := newHubServer(hub.NewRegistry(fakeHubResolver{nodes: []hub.Node{
		{ID: "bad", Name: "Bad", Source: hub.SourceConfig, URL: bad.URL, BasePath: "/chat", Token: "bad"},
		{ID: "good", Name: "Good", Source: hub.SourceConfig, URL: good.URL, BasePath: "/chat", Token: "tkn-123"},
	}}), nil)
	store := openProjection(t)
	srv.attentionStore = store
	badActs := make([]hub.SessionActivity, 0, 20)
	for i := range 20 {
		badActs = append(badActs, hub.SessionActivity{SessionID: fmt.Sprintf("bad_%02d", i), Kind: "terminal_unseen", AttentionSeq: int64(i + 1), TerminalAt: time.UnixMilli(1_800_000_000_000 + int64(i))})
	}
	seedRows(t, store, "bad", "store-bad", badActs...)
	seedRows(t, store, "good", "store-good", hub.SessionActivity{SessionID: "sess_good", Kind: "terminal_unseen", AttentionSeq: 1, TerminalAt: time.UnixMilli(1)})
	requireClear(t, doClear(srv.handler(), "/api/attention/clear"), 1, 20)
	if goodAcks.Load() != 1 {
		t.Fatalf("good = %d, want 1", goodAcks.Load())
	}
	if badCalls.Load() > 8 {
		t.Fatalf("bad calls = %d, want <=8", badCalls.Load())
	}
}

func TestHubAttentionClearUsesStoreAliasWithoutSessionRow(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale alias registered=%t", registered), func(t *testing.T) {
			var calls atomic.Int64
			srv := hubWithBackend(t, "/chat", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer tkn-123" {
					t.Error("clear used the stale route instead of the healthy alias")
				}
				strictSeen("store-a", map[string]int64{"done": 7}, &calls)(w, r)
			})
			nodes, _ := srv.registry.Nodes()
			if registered {
				stale := nodes[0]
				stale.ID, stale.Token = "stale", "stale-route"
				srv.registry = hub.NewRegistry(fakeHubResolver{nodes: append(nodes, stale)})
			}
			srv.attentionStore = openProjection(t)
			seedRows(t, srv.attentionStore, "stale", "store-a", hub.SessionActivity{SessionID: "done", Kind: "terminal_unseen", AttentionSeq: 7})
			if err := srv.attentionStore.MarkUnavailable(context.Background(), "stale", true); err != nil {
				t.Fatal(err)
			}
			seedRows(t, srv.attentionStore, "alpha", "store-a")
			requireClear(t, doClear(srv.handler(), "/api/attention/clear"), 1, 0)
			requireKeys(t, srv.attentionStore)
			if calls.Load() != 1 {
				t.Fatalf("acknowledgements = %d, want 1", calls.Load())
			}
		})
	}
}
