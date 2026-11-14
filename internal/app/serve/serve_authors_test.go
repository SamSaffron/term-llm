package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/term-llm/term-llm/internal/linkeddevices"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/pairing"
	"github.com/term-llm/term-llm/internal/session"
)

func TestAuthorStampProjectionAndFilter(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &serveServer{store: store, cfg: serveServerConfig{identityHeader: "Remote-User", identityNameHeader: "Remote-Name"}}
	sess := &session.Session{ID: "attribution", Provider: "mock", Model: "mock"}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions?created_by=me", nil)
	req.Header.Set("Remote-User", "alice")
	req.Header.Set("Remote-Name", "Alice Renamed")
	s.auth(func(w http.ResponseWriter, r *http.Request) {
		msgs := []core.Message{core.UserText("replayed"), core.AssistantText("answer"), core.UserText("new"), core.UserText("batch")}
		msgs[2].ClientMessageID = "new"
		msgs[3].ClientMessageID = "batch"
		s.authors.StampInputs(r, s.store, msgs)
		if msgs[0].AuthorID != "" || msgs[1].AuthorID != "" || msgs[2].AuthorID != "alice" || msgs[3].AuthorID != "alice" {
			t.Fatalf("stamping=%+v", msgs)
		}
		for _, m := range msgs[2:] {
			if err := store.AddMessage(r.Context(), sess.ID, session.NewMessage(sess.ID, m, -1)); err != nil {
				t.Fatal(err)
			}
		}
		id, valid := requestCreatedBy(w, r)
		if !valid || id != "alice" {
			t.Fatal("filter failed")
		}
	})(httptest.NewRecorder(), req)
	rows, _ := store.GetMessages(context.Background(), sess.ID, 0, 0)
	authors := s.authors.MessageAuthors(s.store, rows)
	if authors["alice"].Name != "Alice Renamed" {
		t.Fatal(authors)
	}
	summaries, _ := store.List(context.Background(), session.ListOptions{})
	entries := []webSessionEntry{s.webSessionEntryFromSummary(summaries[0])}
	s.enrichSessionAuthors(context.Background(), entries)
	if entries[0].CreatedBy == nil || entries[0].CreatedBy.ID != "alice" || entries[0].ParticipantCount == nil || *entries[0].ParticipantCount != 1 {
		t.Fatalf("session projection=%+v", entries[0])
	}
	deviceReq := linkeddevices.WithDevicePrincipal(httptest.NewRequest(http.MethodPost, "/", nil), linkeddevices.DevicePrincipal{DeviceID: "d", Scope: linkeddevices.ScopeWebV1, ApprovedBy: &pairing.Person{ID: "alice", Name: "Stale"}})
	s.auth(func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{}
		addViewerCapability(w, r, payload, s.store)
		if viewer, ok := payload["viewer"].(session.Author); !ok || viewer.Name != "Alice Renamed" {
			t.Fatalf("device viewer used stale pairing name: %+v", payload)
		}
		m := core.UserText("device")
		s.authors.Stamp(r.Context(), s.store, &m)
		if m.AuthorID != "alice" {
			t.Fatal(m)
		}
	})(httptest.NewRecorder(), deviceReq)
	authors = s.authors.MessageAuthors(s.store, rows)
	if authors["alice"].Name != "Alice Renamed" {
		t.Fatal("device overwrote directory")
	}
	if err := store.ResetMessages(context.Background(), sess.ID, nil); err != nil {
		t.Fatal(err)
	}
	s.enrichSessionAuthors(context.Background(), entries)
	if entries[0].ParticipantCount == nil || *entries[0].ParticipantCount != 0 || len(entries[0].Participants) != 0 {
		t.Fatalf("empty participants must be explicit: %+v", entries[0])
	}
	for _, path := range []string{"/v1/sessions?created_by=me", "/v1/sessions?created_by=alice"} {
		w := httptest.NewRecorder()
		_, valid := requestCreatedBy(w, httptest.NewRequest(http.MethodGet, path, nil))
		if valid || w.Code != 400 {
			t.Fatalf("anonymous filter: %d", w.Code)
		}
	}
}
