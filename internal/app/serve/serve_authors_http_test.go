package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	"github.com/term-llm/term-llm/internal/session"
)

const authorHTTPSession = "wake-parent"

// Use the production HTTP mux/auth wrappers, a real file-backed SQLite store,
// and scripted providers. Each runtime factory also registers the test gate so
// first-party input refresh cannot accidentally replace it with a live model.
func newAuthorHTTPFixture(t *testing.T, p *mockprovider.MockProvider, gate *blockingModelSwitchTool) (*serveServer, *session.SQLiteStore) {
	t.Helper()
	t.Cleanup(SwapProcessSessionInputs(NewSessionInputCoordinator()))
	s, store, _ := newAgentWakeFixture(t, p)
	configureAuthorHTTPFixture(s, store, p, gate)
	return s, store
}

func configureAuthorHTTPFixture(s *serveServer, store *session.SQLiteStore, p *mockprovider.MockProvider, gate *blockingModelSwitchTool) {
	s.cfg.ui = false
	s.cfg.api = true
	s.cfg.identityHeader = "Remote-User"
	s.cfg.identityNameHeader = "Remote-Name"
	s.cfg.identityEmailHeader = "Remote-Email"
	s.cfgRef.DefaultProvider = "debug"
	s.shutdownCh = make(chan struct{})
	s.runtimeFactory = func(context.Context, serveRuntimeRequest) (*serveRuntime, error) {
		registry := engine.NewToolRegistry()
		if gate != nil {
			registry.Register(gate)
		}
		rt := &serveRuntime{provider: p, providerKey: "debug", engine: engine.NewEngine(p, registry), store: store, defaultModel: "fast", platform: "web", maxTurns: 5}
		rt.Touch()
		return rt, nil
	}
}

func authorHTTPRequest(method, path, body, author string, firstParty bool) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Term-LLM-Steering-Protocol", "1")
	r.Header.Set(requestSessionIDHeader, authorHTTPSession)
	if firstParty {
		r.Header.Set("X-Term-LLM-UI-Version", "test")
	}
	if author != "" {
		r.Header.Set("Remote-User", author)
		r.Header.Set("Remote-Name", strings.ToUpper(author[:1])+author[1:]+" Example")
		r.Header.Set("Remote-Email", author+"@example.com")
	}
	return r
}
func authorHTTPJSON(t *testing.T, s *serveServer, method, path, body, author string, firstParty bool) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	s.httpHandler().ServeHTTP(w, authorHTTPRequest(method, path, body, author, firstParty))
	var data map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatalf("HTTP %s %s: %d %s (%v)", method, path, w.Code, w.Body.String(), err)
	}
	return w, data
}
func authorRows(t *testing.T, store *session.SQLiteStore) []session.Message {
	t.Helper()
	rows, err := store.GetMessages(context.Background(), authorHTTPSession, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
func assertStoredAuthors(t *testing.T, store *session.SQLiteStore, want []string, starter string) {
	t.Helper()
	var got []string
	for _, row := range authorRows(t, store) {
		if row.Role == core.RoleUser {
			got = append(got, row.AuthorID)
		} else if row.AuthorID != "" {
			t.Fatalf("non-user author: %+v", row)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("stored user authors=%v want=%v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("stored user authors=%v want=%v", got, want)
		}
	}
	sess, err := store.Get(context.Background(), authorHTTPSession)
	if err != nil || sess.CreatedBy != starter {
		t.Fatalf("sessions.created_by=%+v want=%q err=%v", sess, starter, err)
	}
}
func assertMessageAuthorsAPI(t *testing.T, s *serveServer, want []string) {
	t.Helper()
	w, data := authorHTTPJSON(t, s, http.MethodGet, "/v1/sessions/"+authorHTTPSession+"/messages", "", "", false)
	if w.Code != 200 {
		t.Fatalf("transcript=%d %s", w.Code, w.Body.String())
	}
	var users []map[string]any
	for _, raw := range data["messages"].([]any) {
		row := raw.(map[string]any)
		if row["role"] == "user" {
			users = append(users, row)
		} else if _, exists := row["author"]; exists {
			t.Fatalf("non-user API author: %v", row)
		}
	}
	if len(users) != len(want) {
		t.Fatalf("user projection=%v want=%v", users, want)
	}
	for i, id := range want {
		value, exists := users[i]["author"]
		if id == "" {
			if exists {
				t.Fatalf("anonymous row has author: %v", users[i])
			}
		} else if !exists || value.(map[string]any)["id"] != id {
			t.Fatalf("missing author %q: %v", id, users[i])
		}
	}
}

func TestAuthorHTTPResponsesStampingAndProjection(t *testing.T) {
	cases := []struct {
		name, input, id, viewer, starter string
		want                             []string
		disabled, external               bool
	}{
		{name: "identified first-party", input: `"identified attribution"`, id: "send", viewer: "alice", starter: "alice", want: []string{"alice"}},
		{name: "anonymous first-party", input: `"anonymous attribution"`, id: "send", want: []string{""}},
		{name: "disabled headers ignored", input: `"disabled attribution"`, id: "send", viewer: "alice", want: []string{""}, disabled: true},
		{name: "external without client ID", input: `"external attribution"`, viewer: "alice", want: []string{""}, external: true},
		{name: "identified batch", input: `[{"type":"message","role":"user","client_message_id":"batch-a","content":"first attribution"},{"type":"message","role":"user","client_message_id":"batch-b","content":"second attribution"}]`, id: "batch-b", viewer: "alice", starter: "alice", want: []string{"alice", "alice"}},
		{name: "replayed history", input: `[{"type":"message","role":"user","client_message_id":"old","content":"old history"},{"type":"message","role":"assistant","content":"old answer"},{"type":"message","role":"user","client_message_id":"new","content":"new attribution"}]`, id: "new", viewer: "alice", want: []string{"", "alice"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := mockprovider.NewMockProvider("debug").AddTextResponse("mock answer")
			s, store := newAuthorHTTPFixture(t, provider, nil)
			if tc.disabled {
				s.cfg.identityHeader = ""
			}
			body := fmt.Sprintf(`{"input":%s,"client_message_id":%q,"stream":false}`, tc.input, tc.id)
			w, _ := authorHTTPJSON(t, s, http.MethodPost, "/v1/responses", body, tc.viewer, !tc.external)
			if w.Code != 200 {
				t.Fatalf("send=%d %s", w.Code, w.Body.String())
			}
			assertStoredAuthors(t, store, tc.want, tc.starter)
			assertMessageAuthorsAPI(t, s, tc.want)
			_, page := authorHTTPJSON(t, s, http.MethodGet, "/v1/sessions?scope=all", "", "", false)
			entries := page["sessions"].([]any)
			if len(entries) != 1 {
				t.Fatal(page)
			}
			entry := entries[0].(map[string]any)
			attributed := false
			for _, id := range tc.want {
				attributed = attributed || id != ""
			}
			if !attributed {
				for _, key := range []string{"created_by", "created_by_id", "participants", "participant_count"} {
					if _, exists := entry[key]; exists {
						t.Fatalf("flag-off/anonymous wire changed: %s=%v", key, entry)
					}
				}
			} else {
				if tc.starter != "" {
					if entry["created_by"].(map[string]any)["id"] != tc.starter {
						t.Fatal(entry)
					}
				} else if _, exists := entry["created_by"]; exists {
					t.Fatalf("anonymous starter attributed: %v", entry)
				}
				if entry["participant_count"] != float64(1) || entry["participants"].([]any)[0].(map[string]any)["id"] != "alice" {
					t.Fatal(entry)
				}
			}
			requests := provider.RecordedRequests()
			if len(requests) != 1 {
				t.Fatalf("inference requests=%d", len(requests))
			}
			for _, m := range requests[0].Messages {
				if m.AuthorID != "" {
					t.Fatal("provider received attribution")
				}
			}
		})
	}
}

func TestAuthorHTTPPersonalResponsesArePrivate(t *testing.T) {
	s, _ := newAuthorHTTPFixture(t, mockprovider.NewMockProvider("debug").AddTextResponse("answer"), nil)
	w, _ := authorHTTPJSON(t, s, http.MethodPost, "/v1/responses", `{"input":"attribution search","client_message_id":"personal","stream":false}`, "alice", true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	s.projectsEnabled = true
	for _, path := range []string{"/v1/capabilities", "/v1/sessions?created_by=me", "/v1/sidebar?created_by=me", "/v1/sessions/search?q=attribution&created_by=me"} {
		w, _ := authorHTTPJSON(t, s, http.MethodGet, path, "", "alice", false)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("personal response %s: %d %v %s", path, w.Code, w.Header(), w.Body.String())
		}
	}
	for _, path := range []string{"/v1/sessions?created_by=me", "/v1/sidebar?created_by=me", "/v1/sessions/search?q=attribution&created_by=me"} {
		w, _ := authorHTTPJSON(t, s, http.MethodGet, path, "", "", false)
		if w.Code != 400 {
			t.Fatalf("anonymous filter %s=%d", path, w.Code)
		}
	}
}

func waitAuthorSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: " + what)
	}
}
func assertSteeringEventAuthor(t *testing.T, run *responseRun, event, id string) {
	t.Helper()
	sub := run.subscribe(0)
	if sub.ch != nil {
		defer run.unsubscribe(sub.ch)
	}
	for _, ev := range sub.replay {
		if ev.Event != event {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatal(err)
		}
		author, ok := payload["author"].(map[string]any)
		if !ok || author["id"] != id || author["name"] != "Bob Example" || author["email"] != "bob@example.com" {
			t.Fatalf("%s author=%v", event, payload)
		}
		return
	}
	t.Fatalf("missing %s in %v", event, sub.replay)
}

func TestAuthorHTTPInterruptCommitAndRush(t *testing.T) {
	for _, rush := range []bool{false, true} {
		t.Run(fmt.Sprintf("rush=%t", rush), func(t *testing.T) {
			gate := &blockingModelSwitchTool{started: make(chan struct{}), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(gate.release) })
			defer release()
			provider := mockprovider.NewMockProvider("debug").AddTextResponse("bootstrap answer").AddToolCall("gate", gate.Spec().Name, map[string]any{}).AddTextResponse("after steering")
			s, store := newAuthorHTTPFixture(t, provider, gate)
			boot, bootstrap := authorHTTPJSON(t, s, http.MethodPost, "/v1/responses", `{"input":"bootstrap attribution","client_message_id":"bootstrap","stream":false}`, "alice", true)
			if boot.Code != 200 {
				t.Fatal(boot.Body.String())
			}
			handler := s.httpHandler()
			done := make(chan struct{})
			w := httptest.NewRecorder()
			go func() {
				defer close(done)
				handler.ServeHTTP(w, authorHTTPRequest(http.MethodPost, "/v1/responses", fmt.Sprintf(`{"input":"initial attribution","client_message_id":"initial","stream":true,"previous_response_id":%q}`, bootstrap["id"]), "alice", true))
			}()
			waitAuthorSignal(t, gate.started, "mock tool entered")
			runID := s.responseRuns.activeRunID(authorHTTPSession)
			run, ok := s.responseRuns.get(runID)
			if !ok {
				t.Fatal("missing active response")
			}
			t.Cleanup(func() { run.cancelRun(); release(); waitAuthorSignal(t, done, "source HTTP cleanup") })
			body := fmt.Sprintf(`{"message":"steering attribution","client_message_id":"guidance","delivery":"steer","expected_response_id":%q,"expected_run_epoch":%d}`, run.id, run.runEpoch)
			receipt, _ := authorHTTPJSON(t, s, http.MethodPost, "/v1/sessions/"+authorHTTPSession+"/steering", body, "bob", true)
			if receipt.Code != 200 {
				t.Fatalf("steering=%d %s", receipt.Code, receipt.Body.String())
			}
			pending, err := store.ListPendingSteering(context.Background(), authorHTTPSession)
			if err != nil || len(pending) != 1 || pending[0].Message.AuthorID != "bob" {
				t.Fatalf("durable steering=%+v %v", pending, err)
			}
			assertSteeringEventAuthor(t, run, "response.steering.queued", "bob")
			if rush {
				body := fmt.Sprintf(`{"request_id":"author-rush","expected_response_id":%q,"expected_run_epoch":%d}`, run.id, run.runEpoch)
				accepted, _ := authorHTTPJSON(t, s, http.MethodPost, "/v1/sessions/"+authorHTTPSession+"/steering/rush", body, "alice", true)
				if accepted.Code != 202 {
					t.Fatalf("rush=%d %s", accepted.Code, accepted.Body.String())
				}
				waitAuthorSignal(t, done, "source settled")
				waitForServeCondition(t, 5*time.Second, func() bool {
					op, err := store.GetRush(context.Background(), authorHTTPSession, "author-rush")
					return err == nil && op.Status == session.RushStarted
				}, "rush committed")
				op, err := store.GetRush(context.Background(), authorHTTPSession, "author-rush")
				if err != nil {
					t.Fatal(err)
				}
				waitNotificationNativeRun(t, s, op.ReplacementResponseID)
			} else {
				release()
				waitAuthorSignal(t, done, "steered HTTP complete")
				waitNotificationNativeRun(t, s, runID)
				assertSteeringEventAuthor(t, run, "response.steering", "bob")
			}
			assertStoredAuthors(t, store, []string{"alice", "alice", "bob"}, "alice")
			assertMessageAuthorsAPI(t, s, []string{"alice", "alice", "bob"})
			if pending, err := store.ListPendingSteering(context.Background(), authorHTTPSession); err != nil || len(pending) != 0 {
				t.Fatalf("committed pending row retained: %+v %v", pending, err)
			}
			for _, req := range provider.RecordedRequests() {
				for _, m := range req.Messages {
					if m.AuthorID != "" {
						t.Fatal("steering reached provider with author")
					}
				}
			}
		})
	}
}

func TestAuthorHTTPMainSkillStamping(t *testing.T) {
	setup, root := serveSkillTestSetup(t)
	s, store := newAuthorHTTPFixture(t, mockprovider.NewMockProvider("debug").AddTextResponse("skill answer"), nil)
	s.skillsSetup = setup
	s.cfgRef.Skills.Enabled = true
	s.cfgRef.Skills.IncludeProjectSkills = true
	sess, err := store.Get(context.Background(), authorHTTPSession)
	if err != nil {
		t.Fatal(err)
	}
	sess.CWD = root
	if err := store.Update(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	w, payload := authorHTTPJSON(t, s, http.MethodPost, "/v1/sessions/"+authorHTTPSession+"/skills/invoke", `{"name":"default","arguments":"review attribution","client_message_id":"skill-intent"}`, "alice", true)
	if w.Code != 202 {
		t.Fatalf("skill=%d %s", w.Code, w.Body.String())
	}
	waitNotificationNativeRun(t, s, payload["response_id"].(string))
	assertStoredAuthors(t, store, []string{"alice"}, "alice")
	assertMessageAuthorsAPI(t, s, []string{"alice"})
}

func TestAuthorPendingSteeringNilState(t *testing.T) {
	(&serveRuntime{}).configurePendingSteeringPersistence(nil, "")
}
