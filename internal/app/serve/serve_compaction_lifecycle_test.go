package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
)

func waitForServeCompaction(t *testing.T, s *serveServer, id string) *serveSessionActivity {
	t.Helper()
	broker := s.ensureEventBroker()
	sub, err := broker.Subscribe(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Unsubscribe(sub.ID)
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		if activity := s.sessionActivitySnapshot(id); activity != nil && activity.Status != "running" {
			return activity
		}
		select {
		case <-sub.Events:
		case <-timeout.C:
			t.Fatal("compaction did not settle")
		}
	}
}

type gatedCompactionProvider struct {
	core.Provider
	entered    chan context.Context
	release    chan struct{}
	err        error
	panicValue any
}

func (p *gatedCompactionProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	select {
	case p.entered <- ctx:
	default:
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if p.panicValue != nil {
		panic(p.panicValue)
	}
	if p.err != nil {
		return nil, p.err
	}
	return p.Provider.Stream(ctx, req)
}

func newCompactionLifecycleFixture(t *testing.T, failure error) (*serveServer, *serveRuntime, *gatedCompactionProvider, func()) {
	t.Helper()
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	meta := &session.Session{ID: "lifecycle", Provider: "mock", ProviderKey: "mock", Model: "mock-model", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := store.Create(ctx, meta); err != nil {
		t.Fatal(err)
	}
	history := []core.Message{core.UserText("question"), core.AssistantText("answer")}
	for _, msg := range history {
		if err := store.AddMessage(ctx, meta.ID, session.NewMessage(meta.ID, msg, -1)); err != nil {
			t.Fatal(err)
		}
	}
	provider := &gatedCompactionProvider{
		Provider: mockprovider.NewMockProvider("mock").AddTextResponse("Continue from the answer."),
		entered:  make(chan context.Context, 1), release: make(chan struct{}), err: failure,
	}
	rt := &serveRuntime{provider: provider, providerKey: "mock", defaultModel: "mock-model", store: store, sessionMeta: meta, history: history, historyPersisted: true, engine: engine.NewEngine(provider, nil)}
	manager := newServeSessionManager(time.Minute, 4, nil)
	putTestSession(manager, meta.ID, rt)
	t.Cleanup(manager.Close)
	s := &serveServer{sessionMgr: manager, store: store}
	var once sync.Once
	unblock := func() { once.Do(func() { close(provider.release) }) }
	t.Cleanup(func() {
		unblock()
		if rt.activitySnapshot() != nil {
			waitForServeCompaction(t, s, meta.ID)
		}
	})
	return s, rt, provider, unblock
}

func acceptTestCompaction(t *testing.T, s *serveServer, ctx context.Context) *serveSessionActivity {
	t.Helper()
	out := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleSessionRuntimeCompact(out, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx), "lifecycle")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("POST waited for compaction instead of accepting it")
	}
	if out.Code != http.StatusAccepted {
		t.Fatalf("accept status=%d body=%s", out.Code, out.Body.String())
	}
	var body struct {
		Activity *serveSessionActivity `json:"activity"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Activity == nil || body.Activity.Status != "running" || body.Activity.ID == "" {
		t.Fatalf("accepted body=%s", out.Body.String())
	}
	return body.Activity
}

func readTestCompactionState(t *testing.T, s *serveServer) *serveSessionActivity {
	t.Helper()
	out := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleSessionState(out, httptest.NewRequest(http.MethodGet, "/", nil), "lifecycle")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("state poll blocked on the runtime mutex")
	}
	var body struct {
		Activity  *serveSessionActivity `json:"activity"`
		ActiveRun bool                  `json:"active_run"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ActiveRun {
		t.Fatal("manual compaction changed active_run semantics")
	}
	return body.Activity
}

func TestServeCompactionAcceptedSurvivesDisconnectAndPolls(t *testing.T) {
	s, rt, provider, unblock := newCompactionLifecycleFixture(t, nil)
	if got := readTestCompactionState(t, s); got != nil {
		t.Fatalf("idle session has activity: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accepted := acceptTestCompaction(t, s, ctx)
	var workCtx context.Context
	select {
	case workCtx = <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted work did not start")
	}
	cancel()
	if workCtx.Err() != nil {
		t.Fatalf("request cancellation stopped accepted work: %v", workCtx.Err())
	}
	if got := readTestCompactionState(t, s); got == nil || got.ID != accepted.ID || got.Status != "running" {
		t.Fatalf("running state=%+v", got)
	}
	if err := s.ensureRuntimeBaseDirForSession(context.Background(), "lifecycle", rt); !errors.Is(err, errServeSessionBusy) {
		t.Fatalf("workspace restoration did not reject manual ownership: %v", err)
	}
	// Header metadata and status polls must not wait for the provider-owned
	// mutex either. Header reads can use durable session metadata instead.
	headerRead := make(chan bool, 1)
	go func() {
		_, created := rt.ensureSessionInStore(context.Background(), "lifecycle", nil)
		headerRead <- created
	}()
	select {
	case created := <-headerRead:
		if created {
			t.Fatal("header lookup unexpectedly created a session")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session header lookup waited on compaction mutex")
	}
	statusDone := make(chan struct{})
	statusOut := httptest.NewRecorder()
	go func() {
		defer close(statusDone)
		s.handleSessionsStatus(statusOut, httptest.NewRequest(http.MethodGet, "/v1/sessions/status", nil))
	}()
	select {
	case <-statusDone:
	case <-time.After(5 * time.Second):
		t.Fatal("status poll waited on compaction mutex")
	}
	if statusOut.Code != http.StatusOK || !strings.Contains(statusOut.Body.String(), `"status":"running"`) {
		t.Fatalf("running status=%d %s", statusOut.Code, statusOut.Body.String())
	}
	// Both synchronous and streamed admissions reject before touching input.
	if _, err := s.sessionMgr.admitSynchronousActivity("lifecycle", rt); !errors.Is(err, errServeSessionBusy) {
		t.Fatalf("synchronous send admission: %v", err)
	}
	if _, err := s.startResponseRun(rt, true, false, []core.Message{core.UserText("must not persist")}, core.Request{SessionID: "lifecycle"}, "lifecycle", startResponseRunOptions{}); !errors.Is(err, errServeSessionBusy) {
		t.Fatalf("response send admission: %v", err)
	}
	if _, err := rt.Run(context.Background(), true, false, []core.Message{core.UserText("must not persist")}, core.Request{SessionID: "lifecycle"}); !errors.Is(err, errServeSessionBusy) {
		t.Fatalf("direct send: %v", err)
	}
	for _, surface := range []string{"chat", "responses"} {
		body := `{"model":"mock-model","messages":[{"role":"user","content":"must not persist"}]}`
		if surface == "responses" {
			body = `{"model":"mock-model","stream":true,"input":"must not persist","client_message_id":"rejected-input"}`
		}
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(requestSessionIDHeader, "lifecycle")
		if surface == "responses" {
			req.Header.Set("X-Term-LLM-UI-Version", "test")
		}
		out := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			if surface == "chat" {
				s.handleChatCompletions(out, req)
			} else {
				s.handleResponses(out, req)
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s busy rejection blocked", surface)
		}
		if out.Code != http.StatusConflict {
			t.Fatalf("%s busy status=%d body=%s", surface, out.Code, out.Body.String())
		}
	}
	rows, err := s.store.GetMessages(context.Background(), "lifecycle", 0, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("busy rejection persisted input: rows=%d err=%v", len(rows), err)
	}
	unblock()
	terminal := waitForServeCompaction(t, s, "lifecycle")
	if terminal.Status != "completed" || terminal.ID != accepted.ID || terminal.Revision <= accepted.Revision {
		t.Fatalf("terminal=%+v accepted=%+v", terminal, accepted)
	}
	if rt.hasActiveActivity() {
		t.Fatal("terminal result retained busy ownership")
	}
	if got := readTestCompactionState(t, s); got == nil || got.Status != "completed" {
		t.Fatalf("terminal state=%+v", got)
	}
}

func TestServeCompactionFailureIsRetainedForMultiplePolls(t *testing.T) {
	s, rt, _, unblock := newCompactionLifecycleFixture(t, errors.New("summary provider failed"))
	accepted := acceptTestCompaction(t, s, context.Background())
	unblock()
	terminal := waitForServeCompaction(t, s, "lifecycle")
	for i := 0; i < 2; i++ {
		got := readTestCompactionState(t, s)
		if got == nil || got.ID != accepted.ID || got.Status != "failed" || !strings.Contains(got.Error, "summary provider failed") || got.Revision != terminal.Revision {
			t.Fatalf("failure poll %d=%+v", i, got)
		}
		out := httptest.NewRecorder()
		s.handleSessionsStatus(out, httptest.NewRequest(http.MethodGet, "/v1/sessions/status", nil))
		var status struct {
			Sessions []struct {
				Activity *serveSessionActivity `json:"activity"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal(out.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if len(status.Sessions) != 1 || status.Sessions[0].Activity == nil || status.Sessions[0].Activity.Error != got.Error {
			t.Fatalf("failure status poll %d=%s", i, out.Body.String())
		}
	}
	if rt.hasActiveActivity() {
		t.Fatal("failed compaction retained ownership")
	}
}

func TestServeCompactionRejectsResponseSetupOwnership(t *testing.T) {
	s, rt, _, _ := newCompactionLifecycleFixture(t, nil)
	run := newResponseRun("response-setup", "lifecycle", "", "mock-model", time.Now().Unix(), nil)
	if err := s.ensureResponseRuns().create(run); err != nil {
		t.Fatal(err)
	}
	s.ensureResponseRuns().setActiveRun("lifecycle", run.id)
	if _, _, err := s.prepareSessionCompaction(context.Background(), "lifecycle", rt); !errors.Is(err, errServeSessionBusy) {
		t.Fatalf("compaction admitted during response setup: %v", err)
	}
	if rt.compacting.Load() || rt.admittedActivity.Load() != 0 || rt.activitySnapshot() != nil {
		t.Fatal("rejected reservation leaked ownership or lifecycle")
	}
}

func TestServeCompactionAutomaticStatusProjection(t *testing.T) {
	rt := &serveRuntime{}
	s := &serveServer{}
	s.bindRuntimeActivity(rt, "automatic")
	sub, err := s.ensureEventBroker().Subscribe(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.ensureEventBroker().Unsubscribe(sub.ID)
	// Phases remain display text and never create activity attempts.
	rt.recordRuntimeEvent(core.Event{Type: core.EventPhase, Text: "compacting context"})
	if rt.activitySnapshot() != nil {
		t.Fatal("phase text created lifecycle state")
	}
	rt.recordRuntimeEvent(core.Event{Type: core.EventCompactionStatus, CompactionActive: true})
	started := rt.activitySnapshot()
	run := newResponseRun("automatic-run", "automatic", "", "mock", time.Now().Unix(), nil)
	if err := s.appendResponseRunEvent(rt, run, &responseRunStreamState{}, core.Event{Type: core.EventCompactionStatus, CompactionActive: true}); err != nil {
		t.Fatal(err)
	}
	if activity, ok := run.snapshot()["activity"].(*serveSessionActivity); !ok || activity.ID != started.ID {
		t.Fatalf("response snapshot lost activity: %#v", run.snapshot()["activity"])
	}
	rt.recordRuntimeEvent(core.Event{Type: core.EventCompactionStatus, Err: errors.New("automatic failure")})
	ended := rt.activitySnapshot()
	if started == nil || ended.ID != started.ID || ended.Status != "failed" || ended.Revision <= started.Revision || ended.Error != "automatic failure" {
		t.Fatalf("automatic lifecycle start=%+v end=%+v", started, ended)
	}
	for i := 0; i < 2; i++ {
		select {
		case event := <-sub.Events:
			if event.Type != serveEventSessionRuntimeChanged || event.SessionID != "automatic" {
				t.Fatalf("invalidation=%+v", event)
			}
		default:
			t.Fatal("missing lifecycle invalidation")
		}
	}
	rt.recordRuntimeEvent(core.Event{Type: core.EventCompactionStatus, CompactionActive: true})
	next := rt.activitySnapshot()
	if next.ID == ended.ID || next.Revision <= ended.Revision || next.Error != "" {
		t.Fatalf("next attempt did not replace terminal snapshot: %+v", next)
	}
	rt.finishCompactionActivity(nil)
}

type statusOnlyCompactionProvider struct {
	core.Provider
	events []core.Event
}

func (p *statusOnlyCompactionProvider) Stream(context.Context, core.Request) (core.Stream, error) {
	return &serveRuntimeTestStream{events: p.events}, nil
}

func TestServeCompactionConsumerCleanupAfterCancellation(t *testing.T) {
	for _, failure := range []string{"cancelled", "suspended", "consumer failed", "failed while cancelled"} {
		t.Run(failure, func(t *testing.T) {
			provider := &statusOnlyCompactionProvider{
				Provider: mockprovider.NewMockProvider("mock"),
				events:   []core.Event{{Type: core.EventCompactionStatus, CompactionActive: true}},
			}
			rt := &serveRuntime{provider: provider, engine: engine.NewEngine(provider, nil)}
			s := &serveServer{}
			s.bindRuntimeActivity(rt, "automatic")
			sub, err := s.ensureEventBroker().Subscribe(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.ensureEventBroker().Unsubscribe(sub.ID)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			consumerErr := errors.New("consumer failed")
			_, err = rt.RunWithEvents(ctx, false, false, []core.Message{core.UserText("hello")}, core.Request{Model: "mock"}, func(ev core.Event) error {
				if ev.Type != core.EventCompactionStatus {
					return nil
				}
				if got := rt.activitySnapshot(); got == nil || got.Status != "running" {
					t.Errorf("consumer observed status before snapshot: %+v", got)
				}
				if failure == "cancelled" {
					cancel()
					return context.Canceled
				}
				if failure == "suspended" {
					return fmt.Errorf("replacement: %w", &engine.SuspendedError{})
				}
				if failure == "failed while cancelled" {
					cancel()
				}
				return consumerErr
			})
			if err == nil {
				t.Fatal("consumer failure was lost")
			}
			got := rt.activitySnapshot()
			if failure == "cancelled" || failure == "suspended" {
				if got == nil || got.Status != "cancelled" || got.Error != "" {
					t.Fatalf("cancellation reported as failure: %+v", got)
				}
			} else if got == nil || got.Status != "failed" || got.Error != consumerErr.Error() {
				t.Fatalf("consumer failure lost during cleanup: %+v", got)
			}
			for i := 0; i < 2; i++ {
				select {
				case event := <-sub.Events:
					if event.Type != serveEventSessionRuntimeChanged {
						t.Fatalf("invalidation=%+v", event)
					}
				default:
					t.Fatal("consumer cleanup omitted lifecycle invalidation")
				}
			}
		})
	}
}
