package authorship

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/linkeddevices"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/pairing"
	"github.com/samsaffron/term-llm/internal/session"
)

func authorContext(a session.Author, device bool) context.Context {
	return context.WithValue(context.Background(), identityContextKey{}, &Identity{Author: a, LinkedDevice: device})
}

func TestIdentityAmbiguousHeadersAndEmptyDeviceName(t *testing.T) {
	for _, field := range []string{"ID", "Name", "Email"} {
		t.Run(field, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("ID", "alice")
			r.Header.Set("Name", "Alice")
			r.Header.Set("Email", "a@example.com")
			r.Header.Add(field, "second")
			got := Resolve(r, "ID", "Name", "Email")
			if field == "ID" {
				if got != nil {
					t.Fatalf("ambiguous ID accepted: %+v", got)
				}
				return
			}
			if got == nil || got.Author.ID != "alice" {
				t.Fatal(got)
			}
			if field == "Name" && got.Author.Name != "alice" {
				t.Fatal(got)
			}
			if field == "Email" && got.Author.Email != "" {
				t.Fatal(got)
			}
		})
	}
	r := linkeddevices.WithDevicePrincipal(httptest.NewRequest(http.MethodGet, "/", nil), linkeddevices.DevicePrincipal{DeviceID: "device", Scope: linkeddevices.ScopeWebV1, ApprovedBy: &pairing.Person{ID: "alice"}})
	if got := Resolve(r, "ID", "Name", "Email"); got == nil || got.Author.Name != "alice" {
		t.Fatalf("empty approver name=%+v", got)
	}
}

func TestViewerPrivateCacheAndQuotedETag(t *testing.T) {
	for _, etag := range []string{"", `W/"base"`, `"strong"`} {
		t.Run(etag, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(authorContext(session.Author{ID: "alice", Name: "Alice"}, false))
			w := httptest.NewRecorder()
			w.Header().Set("ETag", etag)
			AddViewerCapability(w, r, map[string]any{})
			if w.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatal(w.Header())
			}
			if !regexp.MustCompile(`^(W/)?"[^"\r\n]+"$`).MatchString(w.Header().Get("ETag")) {
				t.Fatalf("invalid ETag: %q", w.Header().Get("ETag"))
			}
		})
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	payload := map[string]any{}
	AddViewerCapability(w, r, payload)
	if len(payload) != 0 || w.Header().Get("Cache-Control") != "" || w.Header().Get("ETag") != "" {
		t.Fatalf("anonymous response changed: %v %v", payload, w.Header())
	}
	r = r.WithContext(authorContext(session.Author{ID: "alice", Name: "Alice"}, false))
	r.URL.RawQuery = "created_by=me"
	if id, ok := RequireCreatedBy(w, r, func(http.ResponseWriter, int, string, string) { t.Fatal("filter rejected") }); !ok || id != "alice" || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal(w.Header())
	}
}

type observedAuthorStore struct {
	session.Store
	directory        session.AuthorStore
	mu               sync.Mutex
	writes, probes   int
	readError        bool
	blockedID        string
	entered, release chan struct{}
}

func (s *observedAuthorStore) UpsertAuthor(ctx context.Context, a session.Author, missing bool) error {
	s.mu.Lock()
	s.writes++
	blocked := a.ID == s.blockedID
	s.mu.Unlock()
	if blocked {
		close(s.entered)
		<-s.release
	}
	return s.directory.UpsertAuthor(ctx, a, missing)
}
func (s *observedAuthorStore) HasAuthors(ctx context.Context) (bool, error) {
	s.mu.Lock()
	s.probes++
	s.mu.Unlock()
	return s.directory.HasAuthors(ctx)
}
func (s *observedAuthorStore) GetAuthors(ctx context.Context, ids []string) (map[string]session.Author, error) {
	if s.readError {
		return nil, errors.New("synthetic read failure")
	}
	return s.directory.GetAuthors(ctx, ids)
}
func (s *observedAuthorStore) SessionParticipants(ctx context.Context, ids []string) (map[string]session.Participants, error) {
	return s.directory.SessionParticipants(ctx, ids)
}
func newObservedAuthors(t *testing.T) *observedAuthorStore {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &observedAuthorStore{Store: store, directory: store}
}

func TestDirectoryDoesNotLockReadersOrOtherAuthorsAcrossWrite(t *testing.T) {
	store := newObservedAuthors(t)
	store.blockedID = "alice"
	store.entered = make(chan struct{})
	store.release = make(chan struct{})
	var directory Directory
	done := make(chan struct{})
	go func() {
		defer close(done)
		msg := core.UserText("blocked")
		directory.Stamp(authorContext(session.Author{ID: "alice", Name: "Alice"}, false), store, &msg)
	}()
	<-store.entered
	// Always release the writer before SQLite test cleanup, including on failure.
	defer func() { close(store.release); <-done }()
	read := make(chan struct{})
	go func() {
		defer close(read)
		msg := core.UserText("independent")
		directory.Stamp(authorContext(session.Author{ID: "bob", Name: "Bob"}, false), store, &msg)
		directory.Lookup(context.Background(), store, []string{"bob"})
	}()
	select {
	case <-read:
	case <-time.After(2 * time.Second):
		t.Fatal("directory mutex held across SQLite write")
	}
}

func TestDirectoryDeviceCacheDoesNotShadowHeaderNames(t *testing.T) {
	store := newObservedAuthors(t)
	var directory Directory
	old := session.Author{ID: "alice", Name: "Pairing snapshot"}
	current := session.Author{ID: "alice", Name: "Renamed", Email: "new@example.com"}
	for range 3 {
		msg := core.UserText("device")
		directory.Stamp(authorContext(old, true), store, &msg)
	}
	if store.writes != 1 {
		t.Fatalf("device performed %d inserts", store.writes)
	}
	msg := core.UserText("header")
	directory.Stamp(authorContext(current, false), store, &msg)
	for range 3 {
		msg := core.UserText("device")
		directory.Stamp(authorContext(old, true), store, &msg)
	}
	if store.writes != 2 {
		t.Fatalf("cached device/header wrote %d times", store.writes)
	}
	if got := directory.Lookup(context.Background(), store, []string{"alice"})["alice"]; got != current {
		t.Fatal(got)
	}
	store.readError = true
	if got := directory.Lookup(context.Background(), store, []string{"alice"})["alice"]; got != current {
		t.Fatalf("stale device shadowed header cache: %+v", got)
	}
}

func TestDirectoryZeroMarkerOnlyAfterAnAuthorExists(t *testing.T) {
	store := newObservedAuthors(t)
	var directory Directory
	directory.Initialize(context.Background(), store)
	for range 2 {
		fields := directory.Sessions(context.Background(), store, map[string]string{"empty": ""})["empty"]
		data, err := json.Marshal(fields)
		if err != nil || string(data) != "{}" {
			t.Fatalf("never-attributed wire changed: %s %v", data, err)
		}
	}
	if store.probes != 1 {
		t.Fatalf("EXISTS queried %d times", store.probes)
	}
	msg := core.UserText("stamp")
	directory.Stamp(authorContext(session.Author{ID: "alice", Name: "Alice"}, false), store, &msg)
	fields := directory.Sessions(context.Background(), store, map[string]string{"empty": ""})["empty"]
	if fields.ParticipantCount == nil || *fields.ParticipantCount != 0 {
		t.Fatalf("missing known-zero marker: %+v", fields)
	}
	var restarted Directory
	restarted.Initialize(context.Background(), store)
	if fields := restarted.Sessions(context.Background(), store, map[string]string{"empty": ""})["empty"]; fields.ParticipantCount == nil {
		t.Fatal("startup missed stored authors")
	}
}
