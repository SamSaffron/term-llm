package serve

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestAuthorHTTPDurableSteeringAfterRuntimeRestart(t *testing.T) {
	s, store := newAuthorHTTPFixture(t, mockprovider.NewMockProvider("debug"), nil)
	// Freeze an active runtime between user admission and consumption. Unlike
	// graceful cancellation, a process restart cannot drain its in-memory queue.
	rt, ok := s.sessionMgr.Get(authorHTTPSession)
	if !ok {
		t.Fatal("missing source runtime")
	}
	run := newResponseRun("before-restart", authorHTTPSession, "", "fast", time.Now().Unix(), func() {})
	active := &runtimeInterruptState{responseRun: run, done: make(chan struct{}), cancel: func() {}}
	rt.configurePendingSteeringPersistence(active, authorHTTPSession)
	rt.setActiveInterrupt(active)
	defer rt.clearActiveInterrupt(active)
	prior := &session.Message{Role: "assistant", TextContent: "previous answer"}
	if err := store.AddMessage(context.Background(), authorHTTPSession, prior); err != nil {
		t.Fatal(err)
	}
	body := `{"message":"durable guidance","client_message_id":"durable","delivery":"steer"}`
	receipt, _ := authorHTTPJSON(t, s, http.MethodPost, "/v1/sessions/"+authorHTTPSession+"/interrupt", body, "bob", true)
	if receipt.Code != 200 {
		t.Fatalf("queue=%d %s", receipt.Code, receipt.Body.String())
	}
	assertSteeringEventAuthor(t, run, "response.steering.queued", "bob")
	rt.clearActiveInterrupt(active)
	pending, err := store.ListPendingSteering(context.Background(), authorHTTPSession)
	if err != nil || len(pending) != 1 || pending[0].Message.AuthorID != "bob" {
		t.Fatalf("pending before restart=%+v err=%v", pending, err)
	}
	assertStoredAuthors(t, store, nil, "")
	path, err := session.SessionInputStoreIdentity(store)
	if err != nil {
		t.Fatal(err)
	}
	s.responseRuns.Close()
	s.sessionMgr.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// Recreate both runtime ownership and the HTTP host, retaining only SQLite.
	t.Cleanup(SwapProcessSessionInputs(NewSessionInputCoordinator()))
	restarted := &serveServer{cfg: s.cfg, cfgRef: s.cfgRef, store: reopened, sessionMgr: newServeSessionManager(time.Minute, 10, nil), responseRuns: newServeResponseRunManager()}
	configureAuthorHTTPFixture(restarted, reopened, mockprovider.NewMockProvider("debug").AddTextResponse("resumed answer"), nil)
	restarted.authors.Initialize(context.Background(), reopened)
	defer func() { close(restarted.shutdownCh); restarted.responseRuns.Close(); restarted.sessionMgr.Close() }()
	state, projection := authorHTTPJSON(t, restarted, http.MethodGet, "/v1/sessions/"+authorHTTPSession+"/state", "", "", false)
	if state.Code != 200 {
		t.Fatalf("state=%d %s", state.Code, state.Body.String())
	}
	restored := projection["pending_steering"].([]any)
	if len(restored) != 1 || restored[0].(map[string]any)["author"].(map[string]any)["id"] != "bob" {
		t.Fatalf("restored projection=%v", projection)
	}
	// The browser resends pending client IDs after reconnect. The original author
	// must survive even when the resumer is anonymous (or a different viewer).
	anchor := latestDurableResponseID(authorRows(t, reopened))
	body = fmt.Sprintf(`{"input":"durable guidance","client_message_id":"durable","previous_response_id":%q,"stream":false}`, anchor)
	response, _ := authorHTTPJSON(t, restarted, http.MethodPost, "/v1/responses", body, "", true)
	if response.Code != 200 {
		t.Fatalf("resume=%d %s", response.Code, response.Body.String())
	}
	assertStoredAuthors(t, reopened, []string{"bob"}, "bob")
	assertMessageAuthorsAPI(t, restarted, []string{"bob"})
	if pending, err := reopened.ListPendingSteering(context.Background(), authorHTTPSession); err != nil || len(pending) != 0 {
		t.Fatalf("committed pending=%+v err=%v", pending, err)
	}
}
