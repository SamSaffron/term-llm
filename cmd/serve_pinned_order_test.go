package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type pinnedOrderTestEntry struct {
	ID       string `json:"id"`
	Pinned   bool   `json:"pinned"`
	PinOrder int64  `json:"pin_order"`
}

func createPinnedOrderTestSession(t *testing.T, store *session.SQLiteStore, id, projectID, cwd string, activity time.Time) {
	t.Helper()
	ctx := context.Background()
	sess := &session.Session{ID: id, Provider: "mock", Model: "mock-model", Mode: session.ModeChat, Origin: session.OriginWeb, ProjectID: projectID, CWD: cwd, CreatedAt: activity, UpdatedAt: activity, Status: session.StatusComplete}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
	touchPinnedOrderTestSession(t, store, id, activity)
}

func touchPinnedOrderTestSession(t *testing.T, store *session.SQLiteStore, id string, at time.Time) {
	t.Helper()
	message := session.NewMessage(id, llm.UserText("hello "+id), -1)
	message.CreatedAt = at
	if err := store.AddMessage(context.Background(), id, message); err != nil {
		t.Fatalf("AddMessage(%s): %v", id, err)
	}
}

func patchSessionPinned(t *testing.T, srv *serveServer, id string, pinned bool) pinnedOrderTestEntry {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/"+id, strings.NewReader(fmt.Sprintf(`{"pinned":%t}`, pinned)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH pinned=%t %s status=%d body=%s", pinned, id, rr.Code, rr.Body.String())
	}
	var entry pinnedOrderTestEntry
	if err := json.Unmarshal(rr.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	return entry
}

func patchPinnedOrder(srv *serveServer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "/v1/sessions/pinned-order", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleSessionsPinnedOrder(rr, req)
	return rr
}

func listPinnedOrderEntries(t *testing.T, srv *serveServer, target string) ([]pinnedOrderTestEntry, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handleSessions(rr, httptest.NewRequest(http.MethodGet, target, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", target, rr.Code, rr.Body.String())
	}
	var payload struct {
		Sessions   []pinnedOrderTestEntry `json:"sessions"`
		NextCursor string                 `json:"next_cursor"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Sessions, payload.NextCursor
}

func pinnedOrderEntryIDs(entries []pinnedOrderTestEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	return ids
}

func TestSessionMetadataPatchAppendsNewPinsAndKeepsExistingRanks(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"a", "b", "c"} {
		createPinnedOrderTestSession(t, store, id, "", "", base.Add(time.Duration(i)*time.Minute))
	}

	if got := patchSessionPinned(t, srv, "a", true); !got.Pinned || got.PinOrder != 1 {
		t.Fatalf("pin a = %+v, want rank 1", got)
	}
	if got := patchSessionPinned(t, srv, "c", true); got.PinOrder != 2 {
		t.Fatalf("pin c = %+v, want appended rank 2", got)
	}
	if got := patchSessionPinned(t, srv, "a", true); got.PinOrder != 1 {
		t.Fatalf("re-pin a = %+v, want unchanged rank 1", got)
	}
	if got := patchSessionPinned(t, srv, "a", false); got.Pinned || got.PinOrder != 0 {
		t.Fatalf("unpin a = %+v", got)
	}
	if got := patchSessionPinned(t, srv, "a", true); got.PinOrder != 3 {
		t.Fatalf("re-pin a after unpin = %+v, want appended rank 3", got)
	}

	// New activity on the older pin does not move it.
	touchPinnedOrderTestSession(t, store, "c", time.Now())
	entries, _ := listPinnedOrderEntries(t, srv, "/v1/sessions")
	if got, want := pinnedOrderEntryIDs(entries), []string{"c", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("listing = %v, want %v", got, want)
	}
	if entries[0].PinOrder != 2 || entries[1].PinOrder != 3 || entries[2].PinOrder != 0 {
		t.Fatalf("listed ranks = %+v", entries)
	}
}

func TestSessionsPinnedOrderReordersAtomically(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"a", "b", "c", "unpinned"} {
		createPinnedOrderTestSession(t, store, id, "", "", base.Add(time.Duration(i)*time.Minute))
	}
	for _, id := range []string{"a", "b", "c"} {
		patchSessionPinned(t, srv, id, true)
	}
	subscription, err := srv.ensureEventBroker().Subscribe(nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := patchPinnedOrder(srv, `{"session_ids":["c","a","b"]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("reorder status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Pinned []session.PinnedPosition `json:"pinned"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := []session.PinnedPosition{{ID: "c", PinOrder: 1}, {ID: "a", PinOrder: 2}, {ID: "b", PinOrder: 3}}
	if !reflect.DeepEqual(body.Pinned, want) {
		t.Fatalf("reorder response = %+v, want %+v", body.Pinned, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	published := map[string]bool{}
	for len(published) < 3 {
		event, err := waitForServeEvent(ctx, subscription.Events)
		if err != nil {
			t.Fatalf("waiting for reorder events: %v (saw %v)", err, published)
		}
		if event.Type == serveEventSessionMetadataChanged {
			published[event.SessionID] = true
		}
	}
	entries, _ := listPinnedOrderEntries(t, srv, "/v1/sessions")
	if got := pinnedOrderEntryIDs(entries); !reflect.DeepEqual(got[:3], []string{"c", "a", "b"}) {
		t.Fatalf("listing after reorder = %v", got)
	}

	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{name: "unknown", body: `{"session_ids":["b","missing"]}`, status: http.StatusNotFound},
		{name: "unpinned", body: `{"session_ids":["b","unpinned"]}`, status: http.StatusConflict},
		{name: "duplicate", body: `{"session_ids":["b","b"]}`, status: http.StatusBadRequest},
		{name: "empty", body: `{"session_ids":[]}`, status: http.StatusBadRequest},
		{name: "unknown field", body: `{"ids":["b"]}`, status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rr := patchPinnedOrder(srv, tc.body); rr.Code != tc.status {
				t.Fatalf("status=%d body=%s, want %d", rr.Code, rr.Body.String(), tc.status)
			}
			entries, _ := listPinnedOrderEntries(t, srv, "/v1/sessions")
			if got := pinnedOrderEntryIDs(entries); !reflect.DeepEqual(got[:3], []string{"c", "a", "b"}) {
				t.Fatalf("rejected request changed order to %v", got)
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/pinned-order", strings.NewReader(`{"session_ids":["a"]}`))
	req.Header.Set("Content-Type", "application/json")
	methodRR := httptest.NewRecorder()
	srv.handleSessionsPinnedOrder(methodRR, req)
	if methodRR.Code != http.StatusMethodNotAllowed || methodRR.Header().Get("Allow") != http.MethodPatch {
		t.Fatalf("POST status=%d allow=%q", methodRR.Code, methodRR.Header().Get("Allow"))
	}
	typeReq := httptest.NewRequest(http.MethodPatch, "/v1/sessions/pinned-order", strings.NewReader(`{"session_ids":["a"]}`))
	typeRR := httptest.NewRecorder()
	srv.handleSessionsPinnedOrder(typeRR, typeReq)
	if typeRR.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("missing content type status=%d", typeRR.Code)
	}
}

// The reorder route must win over the /v1/sessions/ prefix (which would treat
// "pinned-order" as a session ID) and keep the sessions API auth wrapper.
func TestSessionsPinnedOrderRouteThroughServeMux(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	srv.cfg = serveServerConfig{basePath: "/ui", requireAuth: true, token: "route-secret"}
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"a", "b"} {
		createPinnedOrderTestSession(t, store, id, "", "", base.Add(time.Duration(i)*time.Minute))
		if _, err := store.SetSessionPinned(context.Background(), id, true); err != nil {
			t.Fatal(err)
		}
	}
	handler := srv.httpHandler()
	send := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/ui/v1/sessions/pinned-order", strings.NewReader(`{"session_ids":["b","a"]}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	if rr := send(""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated reorder status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr := send("route-secret")
	if rr.Code != http.StatusOK {
		t.Fatalf("reorder through mux status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Pinned []session.PinnedPosition `json:"pinned"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if want := []session.PinnedPosition{{ID: "b", PinOrder: 1}, {ID: "a", PinOrder: 2}}; !reflect.DeepEqual(body.Pinned, want) {
		t.Fatalf("reorder through mux = %+v, want %+v", body.Pinned, want)
	}
}

func TestSessionsPinnedOrderUnsupportedStore(t *testing.T) {
	srv := &serveServer{store: &session.NoopStore{}}
	if rr := patchPinnedOrder(srv, `{"session_ids":["a"]}`); rr.Code != http.StatusNotImplemented {
		t.Fatalf("unsupported store status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSidebarRecentAndProjectsSharePinnedOrderAcrossPages(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	srv.cfgRef = &config.Config{Providers: map[string]config.ProviderConfig{}}
	ctx := context.Background()
	alpha := &session.Project{Name: "Alpha", CanonicalDir: t.TempDir()}
	if err := store.CreateProject(ctx, alpha); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	createPinnedOrderTestSession(t, store, "alpha-old", alpha.ID, alpha.CanonicalDir, base)
	createPinnedOrderTestSession(t, store, "loose", "", "", base.Add(time.Minute))
	createPinnedOrderTestSession(t, store, "alpha-new", alpha.ID, alpha.CanonicalDir, base.Add(2*time.Minute))
	createPinnedOrderTestSession(t, store, "alpha-regular", alpha.ID, alpha.CanonicalDir, base.Add(3*time.Minute))
	for _, id := range []string{"alpha-new", "loose", "alpha-old"} {
		patchSessionPinned(t, srv, id, true)
	}
	if rr := patchPinnedOrder(srv, `{"session_ids":["alpha-old","alpha-new","loose"]}`); rr.Code != http.StatusOK {
		t.Fatalf("reorder status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Activity after the reorder must not move any pin.
	touchPinnedOrderTestSession(t, store, "loose", time.Now())
	wantPins := []string{"alpha-old", "alpha-new", "loose"}

	rr := httptest.NewRecorder()
	srv.handleSidebar(rr, httptest.NewRequest(http.MethodGet, "/v1/sidebar?per_project=12&include_archived_projects=1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("sidebar status=%d body=%s", rr.Code, rr.Body.String())
	}
	var sidebar struct {
		Groups         []session.SidebarGroup `json:"groups"`
		RecentSessions []pinnedOrderTestEntry `json:"recent_sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &sidebar); err != nil {
		t.Fatal(err)
	}
	var recentPins []string
	for _, entry := range sidebar.RecentSessions {
		if entry.Pinned {
			recentPins = append(recentPins, entry.ID)
		}
	}
	if !reflect.DeepEqual(recentPins, wantPins) {
		t.Fatalf("recent pins = %v, want %v", recentPins, wantPins)
	}
	rank := map[string]int64{}
	for _, group := range sidebar.Groups {
		for _, summary := range group.Sessions {
			if summary.Pinned {
				rank[summary.ID] = summary.PinOrder
			}
		}
	}
	if want := map[string]int64{"alpha-old": 1, "alpha-new": 2, "loose": 3}; !reflect.DeepEqual(rank, want) {
		t.Fatalf("grouped pin ranks = %v, want %v", rank, want)
	}

	// The Recent feed pages through pins in rank order before any other row.
	var paged []string
	cursor := ""
	for page := 0; page < 5; page++ {
		target := "/v1/sessions?scope=all&limit=2"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		entries, next := listPinnedOrderEntries(t, srv, target)
		paged = append(paged, pinnedOrderEntryIDs(entries)...)
		if next == "" {
			break
		}
		cursor = next
	}
	if want := append(append([]string(nil), wantPins...), "alpha-regular"); !reflect.DeepEqual(paged, want) {
		t.Fatalf("paged recent feed = %v, want %v", paged, want)
	}
}
